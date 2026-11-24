// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	tenantSessionCookie = "game_save_tenant_session"
	selfCSRFHeader      = "X-Game-Save-Self"

	sessionRoleAdmin  = "admin"
	sessionRoleTenant = "tenant"

	maxActiveSessions = 4096
)

type sessionRecord struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"`
	Username  string    `json:"username,omitempty"`
	VaultID   string    `json:"vault_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// sessionStore tracks active admin and tenant web sessions so logout, password
// rotation, account disable/delete, and admin revoke-all immediately invalidate
// existing browser cookies.
type sessionStore struct {
	secret   []byte
	mu       sync.Mutex
	sessions map[string]sessionRecord
}

func newSessionStore() *sessionStore {
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	return &sessionStore{
		secret:   secret,
		sessions: make(map[string]sessionRecord),
	}
}

func (st *sessionStore) sign(id, role string, expires int64) string {
	mac := hmac.New(sha256.New, st.secret)
	mac.Write([]byte("savevault-session-v2:" + id + ":" + role + ":" + strconv.FormatInt(expires, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

func (st *sessionStore) create(role, username, vaultID string, now time.Time) (string, sessionRecord, error) {
	id, err := randomHex(16)
	if err != nil {
		return "", sessionRecord{}, err
	}
	expires := now.Add(sessionLifetime).UTC()
	record := sessionRecord{
		ID:        id,
		Role:      role,
		Username:  username,
		VaultID:   vaultID,
		CreatedAt: now.UTC(),
		ExpiresAt: expires,
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for sid, existing := range st.sessions {
		if !now.Before(existing.ExpiresAt) {
			delete(st.sessions, sid)
		}
	}
	if len(st.sessions) >= maxActiveSessions {
		var oldestID string
		var oldestAt time.Time
		for sid, existing := range st.sessions {
			if oldestID == "" || existing.CreatedAt.Before(oldestAt) {
				oldestID = sid
				oldestAt = existing.CreatedAt
			}
		}
		if oldestID != "" {
			delete(st.sessions, oldestID)
		}
	}
	st.sessions[id] = record
	unix := expires.Unix()
	value := "v2." + id + "." + strconv.FormatInt(unix, 10) + "." + st.sign(id, role, unix)
	return value, record, nil
}

func parseSessionCookieValue(value string) (id string, expires int64, sig string, ok bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 4 || parts[0] != "v2" {
		return "", 0, "", false
	}
	if !validLowerHex(parts[1], 32) {
		return "", 0, "", false
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || exp <= 0 {
		return "", 0, "", false
	}
	if !validLowerHex(parts[3], 64) {
		return "", 0, "", false
	}
	return parts[1], exp, parts[3], true
}

func (st *sessionStore) validate(value, expectedRole string, now time.Time) (sessionRecord, bool) {
	id, expires, sig, ok := parseSessionCookieValue(value)
	if !ok || now.Unix() >= expires {
		return sessionRecord{}, false
	}
	expectedSig := st.sign(id, expectedRole, expires)
	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return sessionRecord{}, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	record, exists := st.sessions[id]
	if !exists || record.Role != expectedRole || !now.Before(record.ExpiresAt) {
		delete(st.sessions, id)
		return sessionRecord{}, false
	}
	return record, true
}

func (st *sessionStore) revokeCookie(value string) {
	id, _, _, ok := parseSessionCookieValue(value)
	if !ok {
		return
	}
	st.mu.Lock()
	delete(st.sessions, id)
	st.mu.Unlock()
}

func (st *sessionStore) revokeRole(role string) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	removed := 0
	for id, record := range st.sessions {
		if record.Role == role {
			delete(st.sessions, id)
			removed++
		}
	}
	return removed
}

func (st *sessionStore) revokeUser(username, exceptID string) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	removed := 0
	for id, record := range st.sessions {
		if record.Role == sessionRoleTenant && strings.EqualFold(record.Username, username) && id != exceptID {
			delete(st.sessions, id)
			removed++
		}
	}
	return removed
}

func (st *sessionStore) revokeVault(vaultID string) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	removed := 0
	for id, record := range st.sessions {
		if record.Role == sessionRoleTenant && record.VaultID == vaultID {
			delete(st.sessions, id)
			removed++
		}
	}
	return removed
}

func hasSelfWriteHeader(r *http.Request) bool {
	return r.Header.Get(selfCSRFHeader) == "1" || hasAdminWriteHeader(r)
}
