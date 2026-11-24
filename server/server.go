// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultMaxObjectBytes int64 = 512 * 1024 * 1024
	maxMetadataBytes            = 64 * 1024
	maxDescriptorBytes          = 4096
	maxUserNameRunes            = 64
	onlineWindow                = 90 * time.Second
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "v1.4.1-dev"

type server struct {
	dataDir          string
	token            string
	adminToken       string
	saveVaultUsername string
	saveVaultPassword string
	maxObjectBytes    int64
	quotaBytes        int64
	startedAt         time.Time
	now              func() time.Time
	bark             *barkNotifier

	mu        sync.Mutex
	state     serverState
	dashboard dashboardState
	settings  serverSettings
	events    []event

	tokensMu      sync.Mutex
	deviceTokens  deviceTokenState
	pairings      map[string]pairing
	claimLogins   *loginLimiter
	claimFailures *globalClaimFailures

	quotaMu sync.Mutex

	saveVaultMu      sync.Mutex
	saveVaultCatalog saveVaultCatalog
	saveVaultState   saveVaultNativeState

	// manageMu guards C6 SaveVault management state (tombstones, locks, and
	// retention). It is a leaf lock: saveVaultMu -> manageMu and s.mu ->
	// manageMu are the only allowed orders. See savevault_manage.go.
	manageMu       sync.Mutex
	saveVaultManage saveVaultManageState
	// manageFileOps lets tests inject filesystem failures for delete rollback
	// paths. Nil hooks use the os package directly.
	manageFileOps saveVaultManageFileOps

	// metaMu guards SaveVault metadata and cover files. See savevault_meta.go for
	// the fixed lock order.
	metaMu       sync.Mutex
	saveVaultMeta saveVaultMetaState

	// Upstream cover fetching is default-off. The queue starts its two workers
	// lazily on the first enabled enqueue and Close cancels and joins them.
	coverFetchEnv                *bool
	coverFetchClient             *http.Client
	coverFetchTransport          http.RoundTripper
	coverFetchSteamTemplates     []string
	coverFetchSwitchIconTemplate string
	coverFetchSwitchTemplates    []string
	coverFetchMetrics            coverFetchMetric

	coverFetchMu      sync.Mutex
	coverFetchQueue   chan string
	coverFetchQueued  map[string]struct{}
	coverFetchCtx     context.Context
	coverFetchCancel  context.CancelFunc
	coverFetchWG      sync.WaitGroup
	coverFetchStarted bool
	coverFetchClosed  bool
	closeOnce         sync.Once

	retentionRemoved atomic.Int64

	maintaining          sync.Mutex
	verifying            sync.Mutex
	logins               loginLimiter
	webdavLogins         loginLimiter
	requests             atomic.Int64
	authDeviceRequests   atomic.Int64
	authSharedRequests   atomic.Int64
	authRejectedRequests atomic.Int64
}

type serverOptions struct {
	adminToken        string
	saveVaultUsername string
	saveVaultPassword string
	quotaBytes        int64
	settings          serverSettings
	bark              barkConfig
	claimLogins       *loginLimiter
	claimFailures     *globalClaimFailures
}

func newServer(dataDir, token string, maxObjectBytes int64, adminTokens ...string) (*server, error) {
	options := serverOptions{}
	if len(adminTokens) > 0 {
		options.adminToken = adminTokens[0]
	}
	return newServerWithOptions(dataDir, token, maxObjectBytes, options)
}

func newServerWithOptions(dataDir, token string, maxObjectBytes int64, options serverOptions) (*server, error) {
	if token == "" {
		return nil, errors.New("GAME_SAVE_TOKEN (or legacy EDEN_SYNCD_TOKEN) or --token is required")
	}
	if strings.HasPrefix(token, deviceTokenPrefix) {
		return nil, errors.New(`GAME_SAVE_TOKEN must not start with "gsdt_" (reserved for device tokens)`)
	}
	if options.adminToken != "" && options.adminToken == token {
		slog.Error("administrator dashboard disabled: GAME_SAVE_ADMIN_TOKEN must differ from GAME_SAVE_TOKEN")
		options.adminToken = ""
	}
	if (options.saveVaultUsername == "") != (options.saveVaultPassword == "") {
		return nil, errors.New("GAME_SAVE_SAVEVAULT_USERNAME and GAME_SAVE_SAVEVAULT_PASSWORD must be set together")
	}
	if maxObjectBytes <= 0 {
		return nil, errors.New("max object size must be positive")
	}
	if !options.settings.Retention.valid() {
		return nil, errors.New("invalid retention policy")
	}
	bark, err := newBarkNotifier(options.bark)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "objects"), 0o700); err != nil {
		if bark != nil {
			bark.Close()
		}
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	if err := migrateLegacySaveVaultData(dataDir); err != nil {
		if bark != nil {
			bark.Close()
		}
		return nil, fmt.Errorf("migrate legacy OpenSave data: %w", err)
	}
	if options.saveVaultUsername != "" {
		if err := os.MkdirAll(filepath.Join(dataDir, "savevault"), 0o700); err != nil {
			if bark != nil {
				bark.Close()
			}
			return nil, fmt.Errorf("create SaveVault data directory: %w", err)
		}
	}

	coverFetchClient, coverFetchTransport := newCoverFetchClient()
	coverFetchCtx, coverFetchCancel := context.WithCancel(context.Background())
	claimLogins := options.claimLogins
	if claimLogins == nil {
		claimLogins = &loginLimiter{failures: make(map[string]loginFailures)}
	}
	claimFailures := options.claimFailures
	if claimFailures == nil {
		claimFailures = &globalClaimFailures{}
	}
	s := &server{
		dataDir:          dataDir,
		token:            token,
		adminToken:       options.adminToken,
		saveVaultUsername: options.saveVaultUsername,
		saveVaultPassword: options.saveVaultPassword,
		maxObjectBytes:    maxObjectBytes,
		quotaBytes:        options.quotaBytes,
		startedAt:         time.Now().UTC(),
		now:              time.Now,
		bark:             bark,
		state:            serverState{Scopes: make(map[string]scope)},
		dashboard: dashboardState{
			Scopes:  make(map[string]scopeMetadata),
			Devices: make(map[string]deviceInfo),
		},
		deviceTokens:    defaultDeviceTokenState(),
		pairings:        make(map[string]pairing),
		claimLogins:     claimLogins,
		claimFailures:   claimFailures,
		saveVaultCatalog: saveVaultCatalog{Files: make(map[string]saveVaultFileMetadata)},
		saveVaultState:   saveVaultNativeState{Scopes: make(map[string]saveVaultScopeState)},
		saveVaultManage:  defaultSaveVaultManageState(),
		saveVaultMeta:    defaultSaveVaultMetaState(),
		logins:          loginLimiter{failures: make(map[string]loginFailures)},
		webdavLogins:    loginLimiter{failures: make(map[string]loginFailures)},

		coverFetchEnv:                coverFetchEnvSetting(),
		coverFetchClient:             coverFetchClient,
		coverFetchTransport:          coverFetchTransport,
		coverFetchSteamTemplates:     append([]string(nil), defaultCoverFetchSteamTemplates...),
		coverFetchSwitchIconTemplate: defaultCoverFetchSwitchIconTemplate,
		coverFetchSwitchTemplates:    append([]string(nil), defaultCoverFetchSwitchTemplates...),
		coverFetchQueue:              make(chan string, coverFetchQueueSize),
		coverFetchQueued:             make(map[string]struct{}),
		coverFetchCtx:                coverFetchCtx,
		coverFetchCancel:             coverFetchCancel,
	}
	if err := s.loadState(); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.loadDashboard(); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.loadSettings(options.settings); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.loadEvents(); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.loadDeviceTokens(); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.loadSaveVaultCatalog(); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.loadSaveVaultState(); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.loadSaveVaultManage(); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.loadSaveVaultMeta(); err != nil {
		s.Close()
		return nil, err
	}
	if repaired, err := s.repairSaveVaultHeads(); err != nil {
		slog.Warn("repair SaveVault heads at startup", "error", err)
	} else if repaired > 0 {
		slog.Warn("repaired SaveVault heads with missing archives", "count", repaired)
	}
	return s, nil
}

// Close releases resources that are not tied to the HTTP server itself. Cover
// workers are cancelled and joined so tests and graceful shutdown leave no
// goroutines behind.
func (s *server) Close() {
	s.closeOnce.Do(func() {
		if s.coverFetchCancel != nil {
			s.coverFetchMu.Lock()
			s.coverFetchClosed = true
			s.coverFetchMu.Unlock()
			s.coverFetchCancel()
			s.coverFetchWG.Wait()
		}
		if s.bark != nil {
			s.bark.Close()
		}
	})
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)

	// Pairing is unauthenticated by design: the one-time code is the credential.
	// It ignores any Authorization header because it is not wrapped by requireToken.
	mux.HandleFunc("POST /v1/pair/claim", s.handlePairClaim)
	mux.HandleFunc("GET /v1/auth/whoami", s.requireTokenReadOnly(s.handleWhoAmI))

	mux.HandleFunc("PUT /v1/blobs/{id}", s.requireToken(s.handlePutBlob))
	mux.HandleFunc("GET /v1/blobs/{id}", s.requireToken(s.handleGetBlob))
	mux.HandleFunc("GET /v1/scopes", s.requireToken(s.handleListScopes))
	mux.HandleFunc("GET /v1/scopes/{id}/status", s.requireTokenReadOnly(s.handleGetScopeStatus))
	// The legacy head endpoint remains a status fallback for older Eden
	// services. It is a read, not a synchronization heartbeat.
	mux.HandleFunc("GET /v1/scopes/{id}/head", s.requireTokenReadOnly(s.handleGetHead))
	mux.HandleFunc("PUT /v1/scopes/{id}/snapshots/{snapshot}", s.requireToken(s.handlePutSnapshot))
	mux.HandleFunc("PUT /v1/scopes/{id}/conflicts/{snapshot}", s.requireToken(s.handlePutConflictSnapshot))
	mux.HandleFunc("PUT /v1/scopes/{id}/metadata", s.requireToken(s.handlePutScopeMetadata))
	mux.HandleFunc("POST /v1/devices/heartbeat", s.requireToken(s.handleDeviceHeartbeat))

	// SaveVault is a separate client family. It uses the same private sync
	// credential but a wholly separate on-disk namespace from Eden snapshots.
	// Reads are intentionally independent from device heartbeats. Opening a
	// status screen, listing archives, or inspecting a head must not make an
	// inactive device appear to have synchronized successfully.
	// Register both /v1/savevault and the legacy /v1/opensave alias so older
	// clients keep working seamlessly across upgrades.
	for _, prefix := range []string{"/v1/savevault", "/v1/opensave"} {
		mux.HandleFunc("GET "+prefix+"/files", s.requireTokenReadOnly(s.handleSaveVaultList))
		mux.HandleFunc("PUT "+prefix+"/files/{type}/{name}", s.requireToken(s.handleSaveVaultNativePut))
		mux.HandleFunc("GET "+prefix+"/files/{type}/{name}", s.requireTokenReadOnly(s.handleSaveVaultNativeGet))
		mux.HandleFunc("DELETE "+prefix+"/files/{type}/{name}", s.requireToken(s.handleSaveVaultNativeDelete))
		mux.HandleFunc("GET "+prefix+"/scopes/{type}/{game}", s.requireTokenReadOnly(s.handleSaveVaultScope))
		mux.HandleFunc("GET "+prefix+"/scopes/{type}/{game}/head", s.requireTokenReadOnly(s.handleSaveVaultScopeHead))
		mux.HandleFunc("PUT "+prefix+"/scopes/{type}/{game}/snapshots/{snapshot}", s.requireToken(s.handlePutSaveVaultScopeSnapshot))
		mux.HandleFunc("PUT "+prefix+"/scopes/{type}/{game}/conflicts/{snapshot}", s.requireToken(s.handlePutSaveVaultScopeConflict))
		mux.HandleFunc("PUT "+prefix+"/scopes/{type}/{game}/identity", s.requireToken(s.handlePutSaveVaultScopeIdentity))
		mux.HandleFunc("GET "+prefix+"/games", s.requireTokenReadOnly(s.handleListSaveVaultGames))
		mux.HandleFunc("GET "+prefix+"/games/{key...}", s.requireTokenReadOnly(s.handleGetSaveVaultGame))
		mux.HandleFunc("PUT "+prefix+"/scopes/{type}/{game}/custom-name", s.requireToken(s.handlePutSaveVaultCustomName))
		mux.HandleFunc("PUT "+prefix+"/scopes/{type}/{game}/custom-cover", s.requireToken(s.handlePutSaveVaultCustomCover))
		mux.HandleFunc("DELETE "+prefix+"/scopes/{type}/{game}/custom-cover", s.requireToken(s.handleDeleteSaveVaultCustomCover))
		mux.HandleFunc("GET "+prefix+"/covers/{sha256}", s.requireTokenReadOnly(s.handleSaveVaultCover))
		mux.HandleFunc("GET "+prefix+"/titles/switch", s.requireTokenReadOnly(s.handleSwitchTitleTable))
	}

	// SaveVault uses ordinary WebDAV snapshots, isolated from the encrypted Eden
	// object store and guarded by a credential that is independent of both Eden
	// client and dashboard credentials. Keep /opensave as a compatibility alias.
	if s.saveVaultEnabled() {
		mux.HandleFunc("/savevault", s.requireSaveVaultAuth(s.handleSaveVaultWebDAV))
		mux.HandleFunc("/savevault/", s.requireSaveVaultAuth(s.handleSaveVaultWebDAV))
		mux.HandleFunc("/opensave", s.requireSaveVaultAuth(s.handleSaveVaultWebDAV))
		mux.HandleFunc("/opensave/", s.requireSaveVaultAuth(s.handleSaveVaultWebDAV))
	}

	s.registerAdmin(mux)
	return s.withCommonHeaders(mux)
}

var legacySaveVaultDataRenames = []struct {
	legacy  string
	current string
}{
	{"opensave-native", "savevault-native"},
	{"opensave-native.json", "savevault-native.json"},
	{"opensave-native-state.json", "savevault-native-state.json"},
	{"opensave-manage.json", "savevault-manage.json"},
	{"opensave-meta.json", "savevault-meta.json"},
	{"opensave-meta", "savevault-meta"},
	{"opensave-trash", "savevault-trash"},
	{"opensave", "savevault"},
}

// migrateLegacySaveVaultData renames pre-rebrand on-disk opensave* files and
// directories inside a vault directory to savevault* on first startup. If a
// destination already exists, the legacy entry is left untouched.
func migrateLegacySaveVaultData(dataDir string) error {
	for _, pair := range legacySaveVaultDataRenames {
		from := filepath.Join(dataDir, pair.legacy)
		if _, err := os.Lstat(from); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("inspect %s: %w", pair.legacy, err)
		}
		to := filepath.Join(dataDir, pair.current)
		if _, err := os.Lstat(to); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect %s: %w", pair.current, err)
		}
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("rename %s to %s: %w", pair.legacy, pair.current, err)
		}
		slog.Info("migrated legacy OpenSave data entry", "from", pair.legacy, "to", pair.current)
	}
	return nil
}

func (s *server) withCommonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		header := w.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Server", "game-save")
		if strings.HasPrefix(r.URL.Path, "/admin") {
			header.Set("X-Frame-Options", "DENY")
			header.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: https: http:; "+
				"style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; "+
				"base-uri 'none'; form-action 'self'")
		}
		next.ServeHTTP(w, r)
	})
}

func validLogoURL(value string) bool {
	if value == "" {
		return true
	}
	parsed, err := url.ParseRequestURI(value)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != ""
}

func validLogoData(value string) bool {
	if value == "" {
		return true
	}
	_, decoded, ok := decodeLogoData(value)
	return ok && len(decoded) <= 1024*1024
}

func decodeLogoData(value string) (string, []byte, bool) {
	header, encoded, ok := strings.Cut(value, ",")
	if !ok || !strings.HasPrefix(header, "data:image/") || !strings.HasSuffix(header, ";base64") {
		return "", nil, false
	}
	mediaType := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64"))
	switch mediaType {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
	default:
		// Reject SVG and other active or unrecognized image types to prevent stored XSS.
		return "", nil, false
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(decoded) == 0 || len(decoded) > 1024*1024 {
		return "", nil, false
	}
	return mediaType, decoded, true
}

func formatBytes(value int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	size := float64(value)
	unit := 0
	for size >= 1024 && unit < len(units)-1 {
		size /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%d %s", value, units[unit])
	}
	return fmt.Sprintf("%.1f %s", size, units[unit])
}

func validObjectID(id string) bool {
	if len(id) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && strings.ToLower(id) == id
}

func validScopeID(id string) bool {
	return validIdentifier(id, 128)
}

func validSnapshotID(id string) bool {
	return validIdentifier(id, 128)
}

func (s *server) checkQuota(additional int64) bool {
	if s.quotaBytes <= 0 {
		return true
	}
	if additional < 0 {
		additional = 0
	}
	var used int64
	for _, sub := range []string{"objects", "savevault-native", "savevault", "savevault-meta"} {
		_ = filepath.Walk(filepath.Join(s.dataDir, sub), func(_ string, info os.FileInfo, err error) error {
			if err != nil || info == nil || !info.Mode().IsRegular() {
				return nil
			}
			// Ignore staged temporary upload files (.upload-*, .part, etc.) so
			// in-flight writes are not double-counted against additional.
			if strings.HasPrefix(info.Name(), ".") {
				return nil
			}
			used += info.Size()
			return nil
		})
	}
	if info, err := os.Stat(filepath.Join(s.dataDir, "dashboard.json")); err == nil && info.Mode().IsRegular() {
		used += info.Size()
	}
	return used+additional <= s.quotaBytes
}

func validIdentifier(value string, maximum int) bool {
	if value == "" || len(value) > maximum || value == "." || value == ".." || strings.HasPrefix(value, ".") {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') &&
			character != '-' && character != '_' && character != '.' {
			return false
		}
	}
	return true
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// writeErrorCode keeps the legacy error field and adds the machine-readable
// reason introduced by the device-token protocol.
func writeErrorCode(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": message, "code": code})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Warn("encode response", "error", err)
	}
}
