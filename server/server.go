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
	openSaveUsername string
	openSavePassword string
	maxObjectBytes   int64
	startedAt        time.Time
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
	claimLogins   loginLimiter
	claimFailures globalClaimFailures

	openSaveMu      sync.Mutex
	openSaveCatalog openSaveCatalog
	openSaveState   openSaveNativeState

	// manageMu guards C6 SaveVault management state (tombstones, locks, and
	// retention). It is a leaf lock: openSaveMu -> manageMu and s.mu ->
	// manageMu are the only allowed orders. See opensave_manage.go.
	manageMu       sync.Mutex
	openSaveManage openSaveManageState
	// manageFileOps lets tests inject filesystem failures for delete rollback
	// paths. Nil hooks use the os package directly.
	manageFileOps openSaveManageFileOps

	// metaMu guards OpenSave metadata and cover files. See opensave_meta.go for
	// the fixed lock order.
	metaMu       sync.Mutex
	openSaveMeta openSaveMetaState

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
	adminToken       string
	openSaveUsername string
	openSavePassword string
	settings         serverSettings
	bark             barkConfig
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
	if (options.openSaveUsername == "") != (options.openSavePassword == "") {
		return nil, errors.New("GAME_SAVE_OPENSAVE_USERNAME and GAME_SAVE_OPENSAVE_PASSWORD must be set together")
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
	if options.openSaveUsername != "" {
		if err := os.MkdirAll(filepath.Join(dataDir, "opensave"), 0o700); err != nil {
			if bark != nil {
				bark.Close()
			}
			return nil, fmt.Errorf("create OpenSave data directory: %w", err)
		}
	}

	coverFetchClient, coverFetchTransport := newCoverFetchClient()
	coverFetchCtx, coverFetchCancel := context.WithCancel(context.Background())
	s := &server{
		dataDir:          dataDir,
		token:            token,
		adminToken:       options.adminToken,
		openSaveUsername: options.openSaveUsername,
		openSavePassword: options.openSavePassword,
		maxObjectBytes:   maxObjectBytes,
		startedAt:        time.Now().UTC(),
		now:              time.Now,
		bark:             bark,
		state:            serverState{Scopes: make(map[string]scope)},
		dashboard: dashboardState{
			Scopes:  make(map[string]scopeMetadata),
			Devices: make(map[string]deviceInfo),
		},
		deviceTokens:    defaultDeviceTokenState(),
		pairings:        make(map[string]pairing),
		claimLogins:     loginLimiter{failures: make(map[string]loginFailures)},
		openSaveCatalog: openSaveCatalog{Files: make(map[string]openSaveFileMetadata)},
		openSaveState:   openSaveNativeState{Scopes: make(map[string]openSaveScopeState)},
		openSaveManage:  defaultOpenSaveManageState(),
		openSaveMeta:    defaultOpenSaveMetaState(),
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
	if err := s.loadOpenSaveCatalog(); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.loadOpenSaveState(); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.loadOpenSaveManage(); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.loadOpenSaveMeta(); err != nil {
		s.Close()
		return nil, err
	}
	if repaired, err := s.repairOpenSaveHeads(); err != nil {
		slog.Warn("repair OpenSave heads at startup", "error", err)
	} else if repaired > 0 {
		slog.Warn("repaired OpenSave heads with missing archives", "count", repaired)
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

	// OpenSave is a separate client family. It uses the same private sync
	// credential but a wholly separate on-disk namespace from Eden snapshots.
	// Reads are intentionally independent from device heartbeats. Opening a
	// status screen, listing archives, or inspecting a head must not make an
	// inactive device appear to have synchronized successfully.
	mux.HandleFunc("GET /v1/opensave/files", s.requireTokenReadOnly(s.handleOpenSaveList))
	mux.HandleFunc("PUT /v1/opensave/files/{type}/{name}", s.requireToken(s.handleOpenSaveNativePut))
	mux.HandleFunc("GET /v1/opensave/files/{type}/{name}", s.requireTokenReadOnly(s.handleOpenSaveNativeGet))
	mux.HandleFunc("DELETE /v1/opensave/files/{type}/{name}", s.requireToken(s.handleOpenSaveNativeDelete))
	mux.HandleFunc("GET /v1/opensave/scopes/{type}/{game}", s.requireTokenReadOnly(s.handleOpenSaveScope))
	mux.HandleFunc("GET /v1/opensave/scopes/{type}/{game}/head", s.requireTokenReadOnly(s.handleOpenSaveScopeHead))
	mux.HandleFunc("PUT /v1/opensave/scopes/{type}/{game}/snapshots/{snapshot}", s.requireToken(s.handlePutOpenSaveScopeSnapshot))
	mux.HandleFunc("PUT /v1/opensave/scopes/{type}/{game}/conflicts/{snapshot}", s.requireToken(s.handlePutOpenSaveScopeConflict))
	mux.HandleFunc("PUT /v1/opensave/scopes/{type}/{game}/identity", s.requireToken(s.handlePutOpenSaveScopeIdentity))
	mux.HandleFunc("GET /v1/opensave/games", s.requireTokenReadOnly(s.handleListOpenSaveGames))
	mux.HandleFunc("GET /v1/opensave/games/{key...}", s.requireTokenReadOnly(s.handleGetOpenSaveGame))
	mux.HandleFunc("PUT /v1/opensave/scopes/{type}/{game}/custom-name", s.requireToken(s.handlePutOpenSaveCustomName))
	mux.HandleFunc("PUT /v1/opensave/scopes/{type}/{game}/custom-cover", s.requireToken(s.handlePutOpenSaveCustomCover))
	mux.HandleFunc("DELETE /v1/opensave/scopes/{type}/{game}/custom-cover", s.requireToken(s.handleDeleteOpenSaveCustomCover))
	mux.HandleFunc("GET /v1/opensave/covers/{sha256}", s.requireTokenReadOnly(s.handleOpenSaveCover))
	mux.HandleFunc("GET /v1/opensave/titles/switch", s.requireTokenReadOnly(s.handleSwitchTitleTable))

	// OpenSave uses ordinary WebDAV snapshots, isolated from the encrypted Eden
	// object store and guarded by a credential that is independent of both Eden
	// client and dashboard credentials.
	if s.openSaveEnabled() {
		mux.HandleFunc("/opensave", s.requireOpenSaveAuth(s.handleOpenSaveWebDAV))
		mux.HandleFunc("/opensave/", s.requireOpenSaveAuth(s.handleOpenSaveWebDAV))
	}

	s.registerAdmin(mux)
	return s.withCommonHeaders(mux)
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

func validIdentifier(value string, maximum int) bool {
	if value == "" || len(value) > maximum {
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
