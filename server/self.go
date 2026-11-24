// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// handleClientDeviceList lets an authenticated sync client (shared or device
// token) inspect paired devices and storage quota for its own vault.
func (s *server) handleClientDeviceList(w http.ResponseWriter, r *http.Request) {
	auth, ok := authResultFromRequest(r)
	if !ok {
		writeErrorCode(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	now := s.now()
	s.mu.Lock()
	s.tokensMu.Lock()
	tokens := make([]deviceTokenView, 0, len(s.deviceTokens.Tokens))
	for _, record := range s.deviceTokens.Tokens {
		tokens = append(tokens, s.deviceTokenViewLocked(record, now))
	}
	mode := s.deviceTokens.SharedTokenMode
	if mode == "" {
		mode = sharedTokenModeAllow
	}
	s.tokensMu.Unlock()
	s.mu.Unlock()

	used := s.usedBytes()
	quota := s.getQuotaBytes()
	writeJSON(w, http.StatusOK, map[string]any{
		"tokens":            tokens,
		"current_token_id":  auth.tokenID,
		"current_device_id": auth.deviceID,
		"shared_token_mode": mode,
		"used_bytes":        used,
		"quota_bytes":       quota,
	})
}

// handleClientRevokeDevice lets an authenticated sync client revoke a device
// token inside its own vault (for example, kicking a lost handheld from another
// device).
func (s *server) handleClientRevokeDevice(w http.ResponseWriter, r *http.Request) {
	tokenID := strings.TrimSpace(r.PathValue("id"))
	if !validLowerHex(tokenID, deviceTokenIDHexLength) {
		writeError(w, http.StatusBadRequest, "invalid device token id")
		return
	}
	now := s.now().UTC()
	s.mu.Lock()
	s.tokensMu.Lock()
	record, exists := s.deviceTokens.Tokens[tokenID]
	if !exists {
		s.tokensMu.Unlock()
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "device token not found")
		return
	}
	if record.RevokedAt.IsZero() {
		next := s.deviceTokens.clone()
		record.RevokedAt = now
		next.Tokens[tokenID] = record
		if err := s.persistDeviceTokens(next); err != nil {
			s.tokensMu.Unlock()
			s.mu.Unlock()
			writeError(w, http.StatusInternalServerError, "cannot revoke device token")
			return
		}
		s.deviceTokens = next
		s.tokensMu.Unlock()
		s.recordEventLocked(event{Kind: eventDeviceTokenRevoked, Device: record.DeviceID, Detail: deviceTokenEventDetail(record)})
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.tokensMu.Unlock()
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteSaveVaultScopeSnapshot deletes a single historical snapshot from
// a SaveVault native scope. The current head and locked snapshots are refused
// with 409 Conflict so live progress and pinned checkpoints cannot be removed.
func (s *server) handleDeleteSaveVaultScopeSnapshot(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := saveVaultScopeFromRequest(w, r)
	if !ok {
		return
	}
	snapshotID := r.PathValue("snapshot")
	if !validSnapshotID(snapshotID) {
		writeError(w, http.StatusBadRequest, "invalid SaveVault snapshot id")
		return
	}

	s.saveVaultMu.Lock()
	if tombstone, blocked := s.saveVaultScopeBlockedLocked(kind, gameID); blocked {
		s.saveVaultMu.Unlock()
		if tombstone.State == saveVaultTombstoneRetired {
			writeError(w, http.StatusConflict, "retired SaveVault scope is read-only")
		} else {
			writeError(w, http.StatusConflict, "deleted SaveVault scope cannot be modified")
		}
		return
	}
	stored, exists := s.saveVaultState.Scopes[key]
	if !exists {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusNotFound, "SaveVault scope not found")
		return
	}
	snap, exists := stored.Snapshots[snapshotID]
	if !exists {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusNotFound, "SaveVault snapshot not found")
		return
	}
	if stored.Head == snapshotID {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusConflict, "cannot delete the current head snapshot")
		return
	}
	locks := s.saveVaultLocks(kind, gameID)
	if _, locked := locks[snapshotID]; locked {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusConflict, "cannot delete a locked snapshot; unlock it first")
		return
	}

	nextState := s.saveVaultState.clone()
	nextScope := nextState.Scopes[key]
	delete(nextScope.Snapshots, snapshotID)
	nextState.Scopes[key] = nextScope
	if err := s.persistSaveVaultState(nextState); err != nil {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist SaveVault state")
		return
	}
	s.saveVaultState = nextState

	archiveReferenced := false
	for otherKey, otherScope := range s.saveVaultState.Scopes {
		otherKind, _, validKey := parseSaveVaultStateKey(otherKey)
		if !validKey || otherKind != kind {
			continue
		}
		for _, otherSnap := range otherScope.Snapshots {
			if otherSnap.Archive == snap.Archive {
				archiveReferenced = true
				break
			}
		}
		if archiveReferenced {
			break
		}
	}
	var freedBytes int64
	if !archiveReferenced && snap.Archive != "" {
		if archivePath, valid := s.saveVaultPath(kind, snap.Archive); valid {
			if info, err := os.Stat(archivePath); err == nil {
				freedBytes = info.Size()
			}
			if err := s.manageRemove(archivePath); err != nil && !errors.Is(err, os.ErrNotExist) {
				slog.Warn("delete SaveVault snapshot archive", "type", kind, "archive", snap.Archive, "error", err)
			}
		}
		catalogKey := saveVaultCatalogKey(kind, snap.Archive)
		if _, exists := s.saveVaultCatalog.Files[catalogKey]; exists {
			nextCatalog := s.saveVaultCatalog.clone()
			delete(nextCatalog.Files, catalogKey)
			if err := s.persistSaveVaultCatalog(nextCatalog); err == nil {
				s.saveVaultCatalog = nextCatalog
			}
		}
	}
	s.saveVaultMu.Unlock()

	s.recordSaveVaultManageEvent(eventRetention, "deleted snapshot "+snapshotID, kind, gameID, snapshotID)
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted":     true,
		"snapshot":    snapshotID,
		"freed_bytes": freedBytes,
	})
}

// -----------------------------------------------------------------------------
// Admin multi-tenant vault switcher & session revocation helpers
// -----------------------------------------------------------------------------

type vaultSummaryView struct {
	ID         string    `json:"id"`
	Username   string    `json:"username,omitempty"`
	Status     string    `json:"status"`
	Note       string    `json:"note,omitempty"`
	UsedBytes  int64     `json:"used_bytes"`
	QuotaBytes int64     `json:"quota_bytes"`
	ExpiresAt  time.Time `json:"expires_at,omitzero"`
	IsDefault  bool      `json:"is_default"`
}

func (r *vaultRegistry) handleVaultList(w http.ResponseWriter, _ *http.Request) {
	items := make([]vaultSummaryView, 0)
	if def, err := r.Vault(defaultVaultID); err == nil && def != nil {
		items = append(items, vaultSummaryView{
			ID:         defaultVaultID,
			Status:     userStatusActive,
			UsedBytes:  def.usedBytes(),
			QuotaBytes: def.getQuotaBytes(),
			IsDefault:  true,
		})
	}
	for _, user := range r.Users() {
		used := r.vaultUsedBytes(user.VaultID)
		items = append(items, vaultSummaryView{
			ID:         user.VaultID,
			Username:   user.Username,
			Status:     user.Status,
			Note:       user.Note,
			UsedBytes:  used,
			QuotaBytes: user.QuotaBytes,
			ExpiresAt:  user.ExpiresAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"vaults": items})
}

func (r *vaultRegistry) handleRevokeAllSessions(w http.ResponseWriter, request *http.Request) {
	var body struct {
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if request.Body != nil && request.ContentLength != 0 {
		if !decodeAdminJSON(w, request, &body) {
			return
		}
	}
	removed := 0
	switch {
	case strings.TrimSpace(body.Username) != "":
		removed = r.sessions.revokeUser(strings.TrimSpace(body.Username), "")
	case body.Role == sessionRoleTenant:
		removed = r.sessions.revokeRole(sessionRoleTenant)
	default:
		removed = r.sessions.revokeRole(sessionRoleAdmin)
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": removed})
}

// -----------------------------------------------------------------------------
// Tenant Self-Service Portal (/v1/self/*)
// -----------------------------------------------------------------------------

func (r *vaultRegistry) tenantSessionFromRequest(request *http.Request) (sessionRecord, userRecord, *server, bool) {
	if r.sessions == nil {
		return sessionRecord{}, userRecord{}, nil, false
	}
	cookie, err := request.Cookie(tenantSessionCookie)
	if err != nil {
		return sessionRecord{}, userRecord{}, nil, false
	}
	now := time.Now().UTC()
	sess, ok := r.sessions.validate(cookie.Value, sessionRoleTenant, now)
	if !ok {
		return sessionRecord{}, userRecord{}, nil, false
	}
	r.usersMu.Lock()
	_, rec := r.userLocked(sess.Username)
	var user userRecord
	found := rec != nil
	if found {
		user = *rec
	}
	r.usersMu.Unlock()
	if !found || user.VaultID != sess.VaultID || user.Status != userStatusActive || (!user.ExpiresAt.IsZero() && now.After(user.ExpiresAt)) {
		r.sessions.revokeCookie(cookie.Value)
		return sessionRecord{}, userRecord{}, nil, false
	}
	r.mu.Lock()
	vault := r.vaults[user.VaultID]
	r.mu.Unlock()
	if vault == nil {
		var loadErr error
		vault, loadErr = r.vaultWithTokenAndQuota(user.VaultID, user.Token, user.QuotaBytes)
		if loadErr != nil {
			return sessionRecord{}, userRecord{}, nil, false
		}
	}
	return sess, user, vault, true
}

func (r *vaultRegistry) serveSelfRoutes(w http.ResponseWriter, request *http.Request, defaultVault *server) bool {
	if !strings.HasPrefix(request.URL.Path, "/v1/self/") && request.URL.Path != "/v1/self" {
		return false
	}
	defaultVault.withCommonHeaders(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/v1/self/session":
			r.handleSelfSession(w, req, defaultVault)
			return
		case req.Method == http.MethodPost && req.URL.Path == "/v1/self/login":
			r.handleSelfLogin(w, req)
			return
		case req.Method == http.MethodPost && req.URL.Path == "/v1/self/logout":
			r.handleSelfLogout(w, req)
			return
		}

		sess, user, vault, ok := r.tenantSessionFromRequest(req)
		if !ok {
			writeError(w, http.StatusUnauthorized, "tenant authentication required")
			return
		}
		if req.Method != http.MethodGet && req.Method != http.MethodHead && !hasSelfWriteHeader(req) {
			writeError(w, http.StatusForbidden, "missing "+selfCSRFHeader+" header")
			return
		}

		mux := http.NewServeMux()
		mux.HandleFunc("GET /v1/self/overview", func(w http.ResponseWriter, req *http.Request) {
			r.handleSelfOverview(w, req, vault, user)
		})
		mux.HandleFunc("POST /v1/self/password", func(w http.ResponseWriter, req *http.Request) {
			r.handleSelfPassword(w, req, sess, user)
		})
		mux.HandleFunc("GET /v1/self/devices", vault.handleDeviceTokenList)
		mux.HandleFunc("POST /v1/self/devices/{id}/revoke", vault.handleRevokeDeviceToken)
		mux.HandleFunc("DELETE /v1/self/devices/{id}", vault.handleDeleteDeviceToken)
		mux.HandleFunc("GET /v1/self/pairings", vault.handleListPairings)
		mux.HandleFunc("POST /v1/self/pairings", vault.handleCreatePairing)
		mux.HandleFunc("DELETE /v1/self/pairings/{id}", vault.handleDeletePairing)

		mux.HandleFunc("GET /v1/self/savevault/games", vault.handleAdminListSaveVaultGames)
		mux.HandleFunc("GET /v1/self/savevault/scopes/{type}/{game}", vault.handleSaveVaultScopeDetail)
		mux.HandleFunc("GET /v1/self/savevault/covers/{sha256}", vault.handleSaveVaultCover)
		mux.HandleFunc("GET /v1/self/savevault/files/{type}/{name}/download", vault.handleDownloadSaveVaultArchive)
		mux.HandleFunc("PUT /v1/self/savevault/scopes/{type}/{game}/snapshots/{snapshot}/lock", vault.handlePutSaveVaultSnapshotLock)
		mux.HandleFunc("DELETE /v1/self/savevault/scopes/{type}/{game}/snapshots/{snapshot}/lock", vault.handleDeleteSaveVaultSnapshotLock)
		mux.HandleFunc("DELETE /v1/self/savevault/scopes/{type}/{game}/snapshots/{snapshot}", vault.handleDeleteSaveVaultScopeSnapshot)
		mux.HandleFunc("POST /v1/self/savevault/scopes/{type}/{game}/promote", vault.handlePromoteSaveVaultScopeSnapshot)
		mux.ServeHTTP(w, req)
	})).ServeHTTP(w, request)
	return true
}

func (r *vaultRegistry) handleSelfSession(w http.ResponseWriter, request *http.Request, defaultVault *server) {
	sess, user, _, ok := r.tenantSessionFromRequest(request)
	hasUsers := len(r.Users()) > 0
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": false,
			"version":       version,
			"multi_tenant":  hasUsers,
			"admin_enabled": defaultVault.adminToken != "",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"role":          sessionRoleTenant,
		"username":      user.Username,
		"vault_id":      user.VaultID,
		"expires_at":    sess.ExpiresAt,
		"version":       version,
		"multi_tenant":  hasUsers,
		"admin_enabled": defaultVault.adminToken != "",
	})
}

func (r *vaultRegistry) handleSelfLogin(w http.ResponseWriter, request *http.Request) {
	if !hasSelfWriteHeader(request) {
		writeError(w, http.StatusForbidden, "missing "+selfCSRFHeader+" header")
		return
	}
	now := time.Now()
	address := r.clientIP(request)
	if r.userLogins.blocked(address, now) {
		writeError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeAdminJSON(w, request, &body) {
		return
	}
	record, err := r.Authenticate(body.Username, body.Password)
	if err != nil {
		if errors.Is(err, errLoginBusy) {
			writeError(w, http.StatusTooManyRequests, err.Error())
			return
		}
		r.userLogins.fail(address, now)
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	r.userLogins.reset(address)
	value, sess, err := r.sessions.create(sessionRoleTenant, record.Username, record.VaultID, now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot create session")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     tenantSessionCookie,
		Value:    value,
		Path:     "/",
		Expires:  sess.ExpiresAt,
		HttpOnly: true,
		Secure:   r.options.trustedProxies.isSecure(request),
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"role":          sessionRoleTenant,
		"username":      record.Username,
		"vault_id":      record.VaultID,
		"expires_at":    sess.ExpiresAt,
	})
}

func (r *vaultRegistry) handleSelfLogout(w http.ResponseWriter, request *http.Request) {
	if !hasSelfWriteHeader(request) {
		writeError(w, http.StatusForbidden, "missing "+selfCSRFHeader+" header")
		return
	}
	if cookie, err := request.Cookie(tenantSessionCookie); err == nil {
		r.sessions.revokeCookie(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     tenantSessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   r.options.trustedProxies.isSecure(request),
		SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (r *vaultRegistry) handleSelfOverview(w http.ResponseWriter, _ *http.Request, vault *server, user userRecord) {
	used := vault.usedBytes()
	quota := vault.getQuotaBytes()
	activeDevices, revokedDevices := vault.deviceTokenCounts()
	vault.saveVaultMu.Lock()
	scopeCount := len(vault.saveVaultState.Scopes)
	snapshotCount := 0
	for _, sc := range vault.saveVaultState.Scopes {
		snapshotCount += len(sc.Snapshots)
	}
	vault.saveVaultMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"username":         user.Username,
		"vault_id":         user.VaultID,
		"note":             user.Note,
		"expires_at":       user.ExpiresAt,
		"used_bytes":       used,
		"quota_bytes":      quota,
		"scopes":           scopeCount,
		"snapshots":        snapshotCount,
		"active_devices":   activeDevices,
		"revoked_devices":  revokedDevices,
		"server_version":   version,
		"pairing_enabled":  vault.adminToken != "",
	})
}

func (r *vaultRegistry) handleSelfPassword(w http.ResponseWriter, request *http.Request, sess sessionRecord, user userRecord) {
	var body struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if !decodeAdminJSON(w, request, &body) {
		return
	}
	if len(body.NewPassword) < 8 {
		writeError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	if _, err := r.Authenticate(user.Username, body.CurrentPassword); err != nil {
		writeError(w, http.StatusUnauthorized, "current password is incorrect")
		return
	}
	if err := r.SetPassword(user.Username, body.NewPassword); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	r.sessions.revokeUser(user.Username, sess.ID)
	w.WriteHeader(http.StatusNoContent)
}
