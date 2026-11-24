// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A user is one tenant's login: a name and password that map to exactly one
// vault. The password never authenticates a request by itself — it only mints
// (or returns) the vault's client token, so every existing endpoint keeps its
// bearer-token contract.
type userRecord struct {
	Username     string    `json:"username"`
	VaultID      string    `json:"vault_id"`
	Token        string    `json:"token"`
	PasswordHash string    `json:"password_hash"`
	Status       string    `json:"status"`
	Note         string    `json:"note,omitempty"`
	QuotaBytes   int64     `json:"quota_bytes,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at,omitzero"`
	LastLoginAt  time.Time `json:"last_login_at,omitzero"`
}

type userState struct {
	Version int          `json:"version"`
	Users   []userRecord `json:"users"`
}

const userStatusActive = "active"
const userStatusDisabled = "disabled"

// passwordIterations is deliberately slow enough to matter and fast enough for
// a login: 210k PBKDF2-HMAC-SHA256 rounds is roughly 100 ms on a small NAS.
const passwordIterations = 210_000

func (r *vaultRegistry) usersPath() string { return filepath.Join(r.root, "users.json") }

func (r *vaultRegistry) loadUsers() error {
	r.usersMu.Lock()
	defer r.usersMu.Unlock()
	var state userState
	raw, err := os.ReadFile(r.usersPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			r.users = userState{Version: 1}
			return nil
		}
		return fmt.Errorf("read users: %w", err)
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return fmt.Errorf("decode users: %w", err)
	}
	if state.Version == 0 {
		state.Version = 1
	}
	r.users = state
	return nil
}

func (r *vaultRegistry) persistUsersLocked(next userState) error {
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(r.usersPath(), raw)
}

func (r *vaultRegistry) vaultAccessError(vaultID string, now time.Time) string {
	r.usersMu.Lock()
	defer r.usersMu.Unlock()
	for _, record := range r.users.Users {
		if record.VaultID != vaultID {
			continue
		}
		if record.Status != userStatusActive {
			return "account disabled"
		}
		if !record.ExpiresAt.IsZero() && now.After(record.ExpiresAt) {
			return "account expired"
		}
		return ""
	}
	return ""
}

func (r *vaultRegistry) userLocked(username string) (int, *userRecord) {
	for index := range r.users.Users {
		if strings.EqualFold(r.users.Users[index].Username, username) {
			return index, &r.users.Users[index]
		}
	}
	return -1, nil
}

// CreateUser registers a tenant: a vault of its own plus the credential that
// reaches it. The vault directory is created immediately so the token routes
// from the first request on.
func (r *vaultRegistry) CreateUser(username, password, note string, quotaBytes int64, expiresAt time.Time) (userRecord, error) {
	username = strings.TrimSpace(username)
	if len([]rune(username)) < 2 || len([]rune(username)) > 64 {
		return userRecord{}, errors.New("username must be 2-64 characters")
	}
	if len(password) < 8 {
		return userRecord{}, errors.New("password must be at least 8 characters")
	}
	if quotaBytes < 0 {
		return userRecord{}, errors.New("quota must not be negative")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return userRecord{}, err
	}
	token, err := newUserToken()
	if err != nil {
		return userRecord{}, err
	}
	record := userRecord{
		Username:     username,
		VaultID:      vaultIDForUsername(username),
		Token:        token,
		PasswordHash: hash,
		Status:       userStatusActive,
		Note:         strings.TrimSpace(note),
		QuotaBytes:   quotaBytes,
		CreatedAt:    time.Now().UTC(),
		ExpiresAt:    expiresAt.UTC(),
	}

	r.usersMu.Lock()
	defer r.usersMu.Unlock()
	if _, existing := r.userLocked(username); existing != nil {
		return userRecord{}, errors.New("username already exists")
	}
	if record.VaultID == defaultVaultID {
		return userRecord{}, errors.New("username maps to reserved default vault")
	}
	for _, existing := range r.users.Users {
		if existing.VaultID == record.VaultID {
			return userRecord{}, errors.New("username maps to an existing vault")
		}
	}
	// If a previous deleted user left a vault directory behind on disk and this
	// username only collides via sanitization (e.g. "alice!" -> "alice"),
	// allocate a fresh isolated vault ID instead of inheriting another user's dir.
	if _, err := os.Stat(filepath.Join(r.root, "vaults", record.VaultID)); err == nil &&
		strings.ToLower(strings.TrimSpace(username)) != record.VaultID {
		base := record.VaultID
		for attempt := 0; attempt < 8; attempt++ {
			candidate := base + "-" + randomSuffix()
			if _, statErr := os.Stat(filepath.Join(r.root, "vaults", candidate)); errors.Is(statErr, os.ErrNotExist) {
				record.VaultID = candidate
				break
			}
		}
		if record.VaultID == base {
			return userRecord{}, errors.New("cannot allocate isolated vault directory")
		}
	}
	previous := r.users
	next := r.users
	next.Version = 1
	next.Users = append(append([]userRecord{}, r.users.Users...), record)
	if err := r.persistUsersLocked(next); err != nil {
		return userRecord{}, err
	}
	if _, err := r.vaultWithMeta(record.VaultID, record.Token, record.Username, record.QuotaBytes); err != nil {
		_ = r.persistUsersLocked(previous)
		return userRecord{}, err
	}
	r.users = next
	return record, nil
}

var errLoginBusy = errors.New("too many concurrent login attempts, try again later")

func (r *vaultRegistry) acquireLoginSlot() (func(), error) {
	if r.loginVerifySem == nil {
		return func() {}, nil
	}
	select {
	case r.loginVerifySem <- struct{}{}:
		return func() { <-r.loginVerifySem }, nil
	default:
		return nil, errLoginBusy
	}
}

// Authenticate checks a password and returns the tenant's token. Disabled and
// expired accounts fail without issuing a token.
func (r *vaultRegistry) Authenticate(username, password string) (userRecord, error) {
	release, err := r.acquireLoginSlot()
	if err != nil {
		return userRecord{}, err
	}
	defer release()

	r.usersMu.Lock()
	_, record := r.userLocked(username)
	var snapshot userRecord
	if record != nil {
		snapshot = *record
	}
	r.usersMu.Unlock()
	if record == nil {
		// Still spend the hashing time so a missing user is not faster than a
		// wrong password.
		_, _ = hashPassword(password)
		return userRecord{}, errors.New("invalid credentials")
	}
	if !verifyPassword(snapshot.PasswordHash, password) {
		return userRecord{}, errors.New("invalid credentials")
	}
	if snapshot.Status != userStatusActive {
		return userRecord{}, errors.New("account disabled")
	}
	if !snapshot.ExpiresAt.IsZero() && time.Now().UTC().After(snapshot.ExpiresAt) {
		return userRecord{}, errors.New("account expired")
	}

	r.usersMu.Lock()
	i, current := r.userLocked(username)
	if current == nil || i < 0 {
		r.usersMu.Unlock()
		return userRecord{}, errors.New("invalid credentials")
	}
	if current.Status != userStatusActive {
		r.usersMu.Unlock()
		return userRecord{}, errors.New("account disabled")
	}
	if !current.ExpiresAt.IsZero() && time.Now().UTC().After(current.ExpiresAt) {
		r.usersMu.Unlock()
		return userRecord{}, errors.New("account expired")
	}
	if current.PasswordHash != snapshot.PasswordHash {
		r.usersMu.Unlock()
		return userRecord{}, errors.New("invalid credentials")
	}
	next := r.users
	next.Users = append([]userRecord{}, r.users.Users...)
	next.Users[i].LastLoginAt = time.Now().UTC()
	if err := r.persistUsersLocked(next); err == nil {
		r.users = next
	}
	snapshot = next.Users[i]
	r.usersMu.Unlock()
	return snapshot, nil
}

func (r *vaultRegistry) Users() []userRecord {
	r.usersMu.Lock()
	defer r.usersMu.Unlock()
	out := append([]userRecord{}, r.users.Users...)
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out
}

// DeleteUser revokes the credential and drops the vault from memory. The data
// stays on disk: deleting a customer's history is a separate, explicit action.
func (r *vaultRegistry) DeleteUser(username string) error {
	return r.DeleteUserWithPurge(username, false)
}

// DeleteUserWithPurge revokes the user's credentials and web sessions, closes
// their vault, and when purgeData is true removes the isolated tenant directory.
func (r *vaultRegistry) DeleteUserWithPurge(username string, purgeData bool) error {
	r.usersMu.Lock()
	index, record := r.userLocked(username)
	if record == nil {
		r.usersMu.Unlock()
		return errors.New("user not found")
	}
	vaultID, token, actualUsername := record.VaultID, record.Token, record.Username
	next := r.users
	next.Users = append([]userRecord{}, r.users.Users...)
	next.Users = append(next.Users[:index], next.Users[index+1:]...)
	if err := r.persistUsersLocked(next); err != nil {
		r.usersMu.Unlock()
		return err
	}
	r.users = next
	r.mu.Lock()
	delete(r.byToken, token)
	vault := r.vaults[vaultID]
	delete(r.vaults, vaultID)
	r.mu.Unlock()
	r.usersMu.Unlock()

	r.sessions.revokeVault(vaultID)
	r.sessions.revokeUser(actualUsername, "")
	r.evictHandler(vault)
	if vault != nil {
		vault.revokeAllDeviceTokens()
		vault.Close()
	}
	if purgeData && vaultID != "" && vaultID != defaultVaultID && !strings.ContainsAny(vaultID, `/\`) && vaultID != "." && vaultID != ".." {
		vaultsRoot := filepath.Clean(filepath.Join(r.root, "vaults"))
		target := filepath.Clean(filepath.Join(vaultsRoot, vaultID))
		if strings.HasPrefix(target, vaultsRoot+string(filepath.Separator)) {
			_ = os.RemoveAll(target)
		}
	}
	return nil
}

type userPatch struct {
	Note        *string `json:"note"`
	QuotaBytes  *int64  `json:"quotaBytes"`
	ExpiresAt   *string `json:"expiresAt"`
	Status      *string `json:"status"`
	Password    *string `json:"password"`
	RotateToken bool    `json:"rotateToken"`
}

// UpdateUser applies partial lifecycle edits to a tenant user: note, quota,
// expiry, active/disabled status, password reset, and shared token rotation.
func (r *vaultRegistry) UpdateUser(username string, patch userPatch) (userRecord, error) {
	if patch.QuotaBytes != nil && *patch.QuotaBytes < 0 {
		return userRecord{}, errors.New("quota must not be negative")
	}
	var newStatus string
	if patch.Status != nil {
		newStatus = strings.TrimSpace(*patch.Status)
		if newStatus != userStatusActive && newStatus != userStatusDisabled {
			return userRecord{}, errors.New("status must be active or disabled")
		}
	}
	var newExpires time.Time
	hasExpires := false
	if patch.ExpiresAt != nil {
		hasExpires = true
		trimmed := strings.TrimSpace(*patch.ExpiresAt)
		if trimmed != "" {
			parsed, err := time.Parse(time.RFC3339, trimmed)
			if err != nil {
				return userRecord{}, errors.New("expiresAt must be RFC3339")
			}
			newExpires = parsed.UTC()
		}
	}
	var newHash string
	if patch.Password != nil && *patch.Password != "" {
		if len(*patch.Password) < 8 {
			return userRecord{}, errors.New("password must be at least 8 characters")
		}
		var err error
		newHash, err = hashPassword(*patch.Password)
		if err != nil {
			return userRecord{}, err
		}
	}
	var rotatedToken string
	if patch.RotateToken {
		var err error
		rotatedToken, err = newUserToken()
		if err != nil {
			return userRecord{}, err
		}
	}

	r.usersMu.Lock()
	index, record := r.userLocked(username)
	if record == nil {
		r.usersMu.Unlock()
		return userRecord{}, errors.New("user not found")
	}
	oldToken := record.Token
	updated := *record
	if patch.Note != nil {
		updated.Note = strings.TrimSpace(*patch.Note)
	}
	if patch.QuotaBytes != nil {
		updated.QuotaBytes = *patch.QuotaBytes
	}
	if hasExpires {
		updated.ExpiresAt = newExpires
	}
	if newStatus != "" {
		updated.Status = newStatus
	}
	if newHash != "" {
		updated.PasswordHash = newHash
	}
	if rotatedToken != "" {
		updated.Token = rotatedToken
	}

	next := r.users
	next.Users = append([]userRecord{}, r.users.Users...)
	next.Users[index] = updated
	if err := r.persistUsersLocked(next); err != nil {
		r.usersMu.Unlock()
		return userRecord{}, err
	}
	r.users = next

	r.mu.Lock()
	vault := r.vaults[updated.VaultID]
	if rotatedToken != "" {
		delete(r.byToken, oldToken)
		r.byToken[rotatedToken] = updated.VaultID
	}
	r.mu.Unlock()
	r.usersMu.Unlock()

	if vault != nil {
		if patch.QuotaBytes != nil {
			vault.setQuotaBytes(updated.QuotaBytes)
		}
		if rotatedToken != "" {
			vault.setSharedToken(rotatedToken)
		}
	} else if updated.Status == userStatusActive {
		_, _ = r.vaultWithMeta(updated.VaultID, updated.Token, updated.Username, updated.QuotaBytes)
	}
	if newHash != "" || updated.Status == userStatusDisabled {
		r.sessions.revokeUser(updated.Username, "")
	}
	return updated, nil
}

func (r *vaultRegistry) SetPassword(username, password string) error {
	if len(password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	r.usersMu.Lock()
	index, record := r.userLocked(username)
	if record == nil {
		r.usersMu.Unlock()
		return errors.New("user not found")
	}
	actualUsername := record.Username
	next := r.users
	next.Users = append([]userRecord{}, r.users.Users...)
	next.Users[index].PasswordHash = hash
	if err := r.persistUsersLocked(next); err != nil {
		r.usersMu.Unlock()
		return err
	}
	r.users = next
	r.usersMu.Unlock()
	r.sessions.revokeUser(actualUsername, "")
	return nil
}

// restoreUsers recreates the vault routing table after a restart.
func (r *vaultRegistry) restoreUsers() error {
	if err := r.loadUsers(); err != nil {
		return err
	}
	for _, record := range r.Users() {
		if record.Status != userStatusActive {
			continue
		}
		if _, err := r.vaultWithMeta(record.VaultID, record.Token, record.Username, record.QuotaBytes); err != nil {
			return err
		}
	}
	return nil
}

func vaultIDForUsername(username string) string {
	lower := strings.ToLower(strings.TrimSpace(username))
	var b strings.Builder
	for _, character := range lower {
		switch {
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9', character == '-', character == '_':
			b.WriteRune(character)
		default:
			b.WriteByte('-')
		}
	}
	id := strings.Trim(b.String(), "-")
	if id == "" {
		id = "user"
	}
	return id
}

func newUserToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", passwordIterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations <= 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

func sanitizeUserRecord(record userRecord) userRecord {
	record.PasswordHash = ""
	return record
}

// handleLogin is public: the password is the credential. It returns the vault
// token so every other endpoint keeps using ordinary bearer auth.
func (r *vaultRegistry) handleLogin(w http.ResponseWriter, request *http.Request) {
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
	decoder := json.NewDecoder(http.MaxBytesReader(w, request.Body, maxMetadataBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
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
	writeJSON(w, http.StatusOK, map[string]any{
		"token":    record.Token,
		"vault":    record.VaultID,
		"username": record.Username,
	})
}

type userListItem struct {
	userRecord
	UsedBytes int64 `json:"used_bytes"`
}

func (r *vaultRegistry) handleUserList(w http.ResponseWriter, _ *http.Request) {
	users := r.Users()
	items := make([]userListItem, 0, len(users))
	for _, user := range users {
		items = append(items, userListItem{
			userRecord: sanitizeUserRecord(user),
			UsedBytes:  r.vaultUsedBytes(user.VaultID),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": items})
}

func (r *vaultRegistry) handleUserCreate(w http.ResponseWriter, request *http.Request) {
	var body struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		Note       string `json:"note"`
		QuotaBytes int64  `json:"quotaBytes"`
		ExpiresAt  string `json:"expiresAt"`
	}
	if !decodeAdminJSON(w, request, &body) {
		return
	}
	var expires time.Time
	if strings.TrimSpace(body.ExpiresAt) != "" {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(body.ExpiresAt))
		if err != nil {
			writeError(w, http.StatusBadRequest, "expiresAt must be RFC3339")
			return
		}
		expires = parsed
	}
	record, err := r.CreateUser(body.Username, body.Password, body.Note, body.QuotaBytes, expires)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sanitizeUserRecord(record))
}

func (r *vaultRegistry) handleUserPatch(w http.ResponseWriter, request *http.Request) {
	var patch userPatch
	if !decodeAdminJSON(w, request, &patch) {
		return
	}
	record, err := r.UpdateUser(request.PathValue("username"), patch)
	if err != nil {
		status := http.StatusBadRequest
		if err.Error() == "user not found" {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, userListItem{
		userRecord: sanitizeUserRecord(record),
		UsedBytes:  r.vaultUsedBytes(record.VaultID),
	})
}

func (r *vaultRegistry) handleUserDelete(w http.ResponseWriter, request *http.Request) {
	purge := strings.EqualFold(request.URL.Query().Get("purgeData"), "true") || request.URL.Query().Get("purgeData") == "1"
	if err := r.DeleteUserWithPurge(request.PathValue("username"), purge); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (r *vaultRegistry) handleUserPassword(w http.ResponseWriter, request *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if !decodeAdminJSON(w, request, &body) {
		return
	}
	if err := r.SetPassword(request.PathValue("username"), body.Password); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
