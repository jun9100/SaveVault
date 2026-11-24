// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Vaults are independent tenants: one directory, one set of tokens, one store
// for Eden scopes and one for SaveVault archives. A vault never sees another
// vault's bytes, heads or tombstones.
//
// Phase 1 keeps a single "default" vault so an existing deployment behaves
// exactly as before; the layout and the routing are what later steps (users,
// login, quotas) build on. Everything a tenant owns lives under
// <data-dir>/vaults/<vault id>/, so moving a customer to their own box is
// "copy one directory".
const defaultVaultID = "default"

// legacyStateNames are the files and directories a pre-vault deployment kept
// at the top of the data directory. They move into vaults/default/ once.
var legacyStateNames = []string{
	"state.json",
	"dashboard.json",
	"device-tokens.json",
	"events.json",
	"settings.json",
	"catalog.json",
	"savevault-native-state.json",
	"savevault-native.json",
	"savevault-manage.json",
	"savevault-meta.json",
	"savevault-native",
	"savevault-meta",
	"savevault-trash",
	"opensave-native-state.json",
	"opensave-native.json",
	"opensave-manage.json",
	"opensave-meta.json",
	"opensave-native",
	"opensave-meta",
	"opensave-trash",
	"scope.json",
	"objects",
	"savevault",
	"opensave",
}

type vaultRegistry struct {
	root           string
	token          string
	maxObjectBytes int64
	options        serverOptions

	mu        sync.Mutex
	vaults    map[string]*server
	handlerMu sync.Mutex
	handlers  map[*server]http.Handler

	usersMu sync.Mutex
	users   userState
	// userLogins rate-limits /v1/login per client IP; loginVerifySem bounds
	// concurrent PBKDF2 work so unauthenticated requests cannot saturate CPU.
	userLogins     loginLimiter
	loginVerifySem chan struct{}
	// claimLogins and claimFailures are shared across all vaults so pairing
	// rate limits cannot be bypassed by probing non-default tenant vaults.
	claimLogins   *loginLimiter
	claimFailures *globalClaimFailures
	// byToken routes a client credential to its vault. The shared token of the
	// default vault is deliberately absent: an unknown credential falls back to
	// the default vault, which is what a single-tenant deployment expects.
	byToken map[string]string
}

func newVaultRegistry(root, token string, maxObjectBytes int64, options serverOptions) (*vaultRegistry, error) {
	if options.adminToken != "" && options.adminToken == token {
		slog.Error("administrator dashboard disabled: GAME_SAVE_ADMIN_TOKEN must differ from GAME_SAVE_TOKEN")
		options.adminToken = ""
	}
	if err := migrateLegacyVaultLayout(root); err != nil {
		return nil, err
	}
	registry := &vaultRegistry{
		root:           root,
		token:          token,
		maxObjectBytes: maxObjectBytes,
		options:        options,
		vaults:         map[string]*server{},
		handlers:       map[*server]http.Handler{},
		userLogins:     loginLimiter{failures: make(map[string]loginFailures)},
		loginVerifySem: make(chan struct{}, 4),
		claimLogins:    &loginLimiter{failures: make(map[string]loginFailures)},
		claimFailures:  &globalClaimFailures{},
		byToken:        map[string]string{},
	}
	if _, err := registry.Vault(defaultVaultID); err != nil {
		return nil, err
	}
	if err := registry.restoreUsers(); err != nil {
		return nil, err
	}
	return registry, nil
}

// migrateLegacyVaultLayout moves a pre-vault data directory into
// vaults/default/. It runs once: as soon as vaults/ exists the function is a
// no-op, so a half-finished migration is never retried on top of new data.
func migrateLegacyVaultLayout(root string) error {
	vaultsDir := filepath.Join(root, "vaults")
	if _, err := os.Stat(vaultsDir); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect vault directory: %w", err)
	}

	present := make([]string, 0, len(legacyStateNames))
	for _, name := range legacyStateNames {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			present = append(present, name)
		}
	}
	if len(present) == 0 {
		// A fresh install: nothing to move, the vault creates its own files.
		return os.MkdirAll(vaultsDir, 0o700)
	}

	target := filepath.Join(vaultsDir, defaultVaultID)
	if err := os.MkdirAll(target, 0o700); err != nil {
		return fmt.Errorf("create vault directory: %w", err)
	}
	sort.Strings(present)
	for _, name := range present {
		from := filepath.Join(root, name)
		to := filepath.Join(target, name)
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("move %s into the default vault: %w", name, err)
		}
	}
	slog.Info("migrated data directory into the default vault",
		"from", root, "to", target, "entries", strings.Join(present, ","))
	return nil
}

// Vault returns the vault with this id, creating it on first use.
func (r *vaultRegistry) Vault(id string) (*server, error) {
	return r.vaultWithToken(id, r.token)
}

// vaultWithToken creates a vault that authenticates with its own credential.
// Each vault gets a separate server, so its state, locks, tokens and object
// store are private by construction.
func (r *vaultRegistry) vaultWithToken(id, token string) (*server, error) {
	return r.vaultWithTokenAndQuota(id, token, 0)
}

func (r *vaultRegistry) vaultWithTokenAndQuota(id, token string, quotaBytes int64) (*server, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return nil, fmt.Errorf("invalid vault id %q", id)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.vaults[id]; ok {
		return existing, nil
	}
	options := r.options
	options.quotaBytes = quotaBytes
	options.claimLogins = r.claimLogins
	options.claimFailures = r.claimFailures
	vault, err := newServerWithOptions(filepath.Join(r.root, "vaults", id), token, r.maxObjectBytes, options)
	if err != nil {
		return nil, fmt.Errorf("create vault %q: %w", id, err)
	}
	r.vaults[id] = vault
	if id != defaultVaultID {
		r.byToken[token] = id
	}
	return vault, nil
}

// vaultForRequest resolves which vault a request belongs to. The credential is
// the tenant boundary: a token registered to another vault never reaches the
// default one, and lookups never recreate a deleted vault.
func (r *vaultRegistry) vaultForRequest(request *http.Request) (string, *server, bool) {
	defaultVault, err := r.Vault(defaultVaultID)
	if err != nil {
		return "", nil, false
	}
	// Administrative and metrics requests carry no tenant token, so the operator
	// names an existing vault explicitly — but only after authenticating as
	// admin, so an unauthenticated header can never probe or create vaults.
	// Never fall through to bearer-token routing for /admin/ or /metrics.
	if strings.HasPrefix(request.URL.Path, "/admin/") || request.URL.Path == "/metrics" {
		if id := strings.TrimSpace(request.Header.Get("X-Game-Save-Vault")); id != "" && id != defaultVaultID {
			if defaultVault.adminToken != "" && defaultVault.adminAuthenticatedPeek(request) {
				r.mu.Lock()
				vault, ok := r.vaults[id]
				r.mu.Unlock()
				if ok {
					return id, vault, true
				}
				return id, nil, true
			}
			return defaultVaultID, defaultVault, true
		}
		return defaultVaultID, defaultVault, true
	}
	if token := bearerToken(request); token != "" {
		if strings.HasPrefix(token, deviceTokenPrefix) {
			if tokenID, _, valid := parseDeviceToken(token); valid {
				r.mu.Lock()
				type entry struct {
					id    string
					vault *server
				}
				snapshot := make([]entry, 0, len(r.vaults))
				for id, v := range r.vaults {
					snapshot = append(snapshot, entry{id: id, vault: v})
				}
				r.mu.Unlock()
				for _, item := range snapshot {
					if item.vault.hasDeviceTokenID(tokenID) {
						return item.id, item.vault, false
					}
				}
			}
			return defaultVaultID, defaultVault, false
		}
		r.mu.Lock()
		id, ok := r.byToken[token]
		var vault *server
		if ok {
			vault = r.vaults[id]
		}
		r.mu.Unlock()
		if ok && vault != nil {
			return id, vault, false
		}
	}
	return defaultVaultID, defaultVault, false
}

func (r *vaultRegistry) handlerFor(vault *server) http.Handler {
	r.handlerMu.Lock()
	defer r.handlerMu.Unlock()
	if r.handlers == nil {
		r.handlers = map[*server]http.Handler{}
	}
	handler, ok := r.handlers[vault]
	if !ok {
		handler = vault.handler()
		r.handlers[vault] = handler
	}
	return handler
}

func (r *vaultRegistry) evictHandler(vault *server) {
	if vault == nil {
		return
	}
	r.handlerMu.Lock()
	delete(r.handlers, vault)
	r.handlerMu.Unlock()
}

func (r *vaultRegistry) vaultForPairingClaim(request *http.Request) (string, *server) {
	defaultVault, err := r.Vault(defaultVaultID)
	if err != nil {
		return "", nil
	}
	if request.Body == nil {
		return defaultVaultID, defaultVault
	}
	raw, err := io.ReadAll(io.LimitReader(request.Body, 4*1024+1))
	_ = request.Body.Close()
	request.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil || len(raw) > 4*1024 {
		return defaultVaultID, defaultVault
	}
	var peek struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil {
		return defaultVaultID, defaultVault
	}
	normalized, valid := normalizePairingCode(peek.Code)
	if !valid {
		return defaultVaultID, defaultVault
	}
	digest := sha256.Sum256([]byte(normalized))
	now := time.Now()
	r.mu.Lock()
	type entry struct {
		id    string
		vault *server
	}
	snapshot := make([]entry, 0, len(r.vaults))
	for id, v := range r.vaults {
		snapshot = append(snapshot, entry{id: id, vault: v})
	}
	r.mu.Unlock()
	for _, item := range snapshot {
		if item.vault.hasPairingCodeHash(digest, now) {
			return item.id, item.vault
		}
	}
	return defaultVaultID, defaultVault
}

func (r *vaultRegistry) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && request.URL.Path == "/v1/pair/claim" {
			defaultVault, err := r.Vault(defaultVaultID)
			if err != nil || defaultVault == nil {
				http.Error(w, "vault unavailable", http.StatusInternalServerError)
				return
			}
			// Check the shared pairing rate limiter BEFORE inspecting the code
			// or scanning tenant vaults so blocked clients cannot probe tenants.
			if defaultVault.adminToken != "" {
				now := defaultVault.now()
				if r.claimLogins.blocked(remoteHost(request), now) || r.claimFailures.blocked(now) {
					defaultVault.withCommonHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						writeErrorCode(w, http.StatusTooManyRequests, "rate_limited", "too many failed pairing attempts")
					})).ServeHTTP(w, request)
					return
				}
			}
			id, vault := r.vaultForPairingClaim(request)
			if vault == nil {
				http.Error(w, "vault unavailable", http.StatusInternalServerError)
				return
			}
			if id != defaultVaultID {
				if reason := r.vaultAccessError(id, time.Now().UTC()); reason != "" {
					vault.withCommonHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						writeErrorCode(w, http.StatusUnauthorized, "unauthorized", reason)
					})).ServeHTTP(w, request)
					return
				}
			}
			r.handlerFor(vault).ServeHTTP(w, request)
			return
		}
		id, vault, isAdminPath := r.vaultForRequest(request)
		if vault == nil {
			if isAdminPath && id != "" {
				writeError(w, http.StatusNotFound, "vault not found")
				return
			}
			http.Error(w, "vault unavailable", http.StatusInternalServerError)
			return
		}
		if r.serveTenantRoutes(w, request, vault) {
			return
		}
		if !isAdminPath && id != defaultVaultID {
			if reason := r.vaultAccessError(id, time.Now().UTC()); reason != "" {
				vault.withCommonHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					writeErrorCode(w, http.StatusUnauthorized, "unauthorized", reason)
				})).ServeHTTP(w, request)
				return
			}
		}
		r.handlerFor(vault).ServeHTTP(w, request)
	})
}

// maintenanceLoop runs scheduled retention and garbage collection across every
// currently registered vault. Iterating dynamically ensures newly created
// tenants are maintained and deleted tenants are never touched again.
func (r *vaultRegistry) maintenanceLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, vault := range r.all() {
				if _, err := vault.runMaintenance(false); err != nil {
					slog.Warn("scheduled maintenance", "error", err)
				}
			}
		}
	}
}

// serveTenantRoutes handles the two endpoints that are not scoped to a single
// vault: the login that turns a password into a vault token, and the admin
// user list that manages tenants. It reports whether it wrote the response.
func (r *vaultRegistry) serveTenantRoutes(w http.ResponseWriter, request *http.Request, vault *server) bool {
	if request.Method == http.MethodPost && request.URL.Path == "/v1/login" {
		vault.withCommonHeaders(http.HandlerFunc(r.handleLogin)).ServeHTTP(w, request)
		return true
	}
	if !strings.HasPrefix(request.URL.Path, "/admin/api/users") {
		return false
	}
	users := http.NewServeMux()
	users.HandleFunc("GET /admin/api/users", r.handleUserList)
	users.HandleFunc("POST /admin/api/users", r.handleUserCreate)
	users.HandleFunc("DELETE /admin/api/users/{username}", r.handleUserDelete)
	users.HandleFunc("POST /admin/api/users/{username}/password", r.handleUserPassword)

	write := request.Method != http.MethodGet
	var h http.HandlerFunc
	if write {
		h = vault.requireAdminWrite(users.ServeHTTP)
	} else {
		h = vault.requireAdmin(users.ServeHTTP)
	}
	vault.withCommonHeaders(h).ServeHTTP(w, request)
	return true
}

func (r *vaultRegistry) Close() {
	r.mu.Lock()
	vaults := make([]*server, 0, len(r.vaults))
	for _, vault := range r.vaults {
		vaults = append(vaults, vault)
	}
	r.mu.Unlock()
	for _, vault := range vaults {
		vault.Close()
	}
}

// bearerToken reads the credential without authenticating it; the vault's own
// handler still verifies it afterwards.
func bearerToken(request *http.Request) string {
	header := strings.TrimSpace(request.Header.Get("Authorization"))
	if len(header) > 7 && strings.EqualFold(header[:7], "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	return ""
}

// all returns every vault created so far, default first.
func (r *vaultRegistry) all() []*server {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.vaults))
	for id := range r.vaults {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*server, 0, len(ids))
	for _, id := range ids {
		out = append(out, r.vaults[id])
	}
	return out
}
