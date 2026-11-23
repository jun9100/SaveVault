// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	deviceTokensFileName = "device-tokens.json"

	deviceTokenPrefix          = "gsdt_"
	deviceTokenIDBytes         = 8
	deviceTokenSecretBytes     = 32
	deviceTokenIDHexLength     = deviceTokenIDBytes * 2
	deviceTokenSecretHexLength = deviceTokenSecretBytes * 2
	deviceTokenLength          = len(deviceTokenPrefix) + deviceTokenIDHexLength + 1 + deviceTokenSecretHexLength
	deviceTokenTouchInterval   = 5 * time.Minute
	maxDeviceTokenRecords      = 256

	sharedTokenModeAllow = "allow"
	sharedTokenModeWarn  = "warn"
	sharedTokenModeDeny  = "deny"

	sharedTokenWarningHeader = "X-Game-Save-Auth-Warning"
	sharedTokenWarningValue  = "shared-token-deprecated"

	pairingCodeLength        = 8
	pairingAlphabet          = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	pairingDefaultTTLMinutes = 10
	pairingMinTTLMinutes     = 1
	pairingMaxTTLMinutes     = 60
	maxActivePairings        = 5
	globalClaimFailureLimit  = 30
)

// deviceTokenRecord is the persisted form of one device token. Only the SHA-256
// digest of the secret is stored; the plaintext token is returned once when it
// is created.
type deviceTokenRecord struct {
	ID           string    `json:"id"`
	Name         string    `json:"name,omitempty"`
	SecretSHA256 string    `json:"secret_sha256"`
	DeviceID     string    `json:"device_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	CreatedVia   string    `json:"created_via"`
	LastUsedAt   time.Time `json:"last_used_at,omitzero"`
	RevokedAt    time.Time `json:"revoked_at,omitzero"`
}

type deviceTokenState struct {
	Version         int                          `json:"version"`
	SharedTokenMode string                       `json:"shared_token_mode"`
	Tokens          map[string]deviceTokenRecord `json:"tokens"`
}

// pairing lives only in memory. Restarting the service invalidates every
// unclaimed code.
type pairing struct {
	ID         string    `json:"id"`
	Name       string    `json:"name,omitempty"`
	CodeSHA256 string    `json:"code_sha256"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func defaultDeviceTokenState() deviceTokenState {
	return deviceTokenState{
		Version:         1,
		SharedTokenMode: sharedTokenModeAllow,
		Tokens:          make(map[string]deviceTokenRecord),
	}
}

func (s deviceTokenState) clone() deviceTokenState {
	next := deviceTokenState{
		Version:         s.Version,
		SharedTokenMode: s.SharedTokenMode,
		Tokens:          make(map[string]deviceTokenRecord, len(s.Tokens)+1),
	}
	for id, record := range s.Tokens {
		next.Tokens[id] = record
	}
	return next
}

func validSharedTokenMode(mode string) bool {
	return mode == sharedTokenModeAllow || mode == sharedTokenModeWarn || mode == sharedTokenModeDeny
}

func (s *server) deviceTokenPath() string {
	return filepath.Join(s.dataDir, deviceTokensFileName)
}

// loadDeviceTokens never creates the file. A deployment that has not generated
// a token keeps working with an in-memory default state.
func (s *server) loadDeviceTokens() error {
	state := deviceTokenState{}
	if err := readJSONFile(s.deviceTokenPath(), &state); err != nil {
		return err
	}
	if state.Version != 0 && state.Version != 1 {
		return fmt.Errorf("%s contains an unsupported version", deviceTokensFileName)
	}
	if state.SharedTokenMode == "" {
		state.SharedTokenMode = sharedTokenModeAllow
	}
	if !validSharedTokenMode(state.SharedTokenMode) {
		return fmt.Errorf("%s contains an invalid shared_token_mode", deviceTokensFileName)
	}
	if state.Tokens == nil {
		state.Tokens = make(map[string]deviceTokenRecord)
	}
	state.Version = 1
	s.deviceTokens = state
	return nil
}

func (s *server) persistDeviceTokens(state deviceTokenState) error {
	state.Version = 1
	return writeJSONFile(s.deviceTokenPath(), state)
}

func randomHex(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func newDeviceTokenRecord(now time.Time, name, deviceID, createdVia string) (string, deviceTokenRecord, error) {
	id, err := randomHex(deviceTokenIDBytes)
	if err != nil {
		return "", deviceTokenRecord{}, err
	}
	secret, err := randomHex(deviceTokenSecretBytes)
	if err != nil {
		return "", deviceTokenRecord{}, err
	}
	digest := sha256.Sum256([]byte(secret))
	record := deviceTokenRecord{
		ID:           id,
		Name:         name,
		SecretSHA256: hex.EncodeToString(digest[:]),
		DeviceID:     deviceID,
		CreatedAt:    now.UTC(),
		CreatedVia:   createdVia,
	}
	return deviceTokenPrefix + id + "_" + secret, record, nil
}

func validLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func parseDeviceToken(value string) (string, string, bool) {
	if len(value) != deviceTokenLength || !strings.HasPrefix(value, deviceTokenPrefix) ||
		value[len(deviceTokenPrefix)+deviceTokenIDHexLength] != '_' {
		return "", "", false
	}
	id := value[len(deviceTokenPrefix) : len(deviceTokenPrefix)+deviceTokenIDHexLength]
	secret := value[len(deviceTokenPrefix)+deviceTokenIDHexLength+1:]
	if !validLowerHex(id, deviceTokenIDHexLength) || !validLowerHex(secret, deviceTokenSecretHexLength) {
		return "", "", false
	}
	return id, secret, true
}

func (s *server) hasDeviceTokenID(id string) bool {
	s.tokensMu.Lock()
	_, exists := s.deviceTokens.Tokens[id]
	s.tokensMu.Unlock()
	return exists
}

func (s *server) hasPairingCodeHash(digest [sha256.Size]byte, now time.Time) bool {
	s.tokensMu.Lock()
	defer s.tokensMu.Unlock()
	s.cleanupExpiredPairingsLocked(now)
	matched := false
	for _, candidate := range s.pairings {
		stored, err := hex.DecodeString(candidate.CodeSHA256)
		if err != nil || len(stored) != sha256.Size {
			continue
		}
		if subtle.ConstantTimeCompare(digest[:], stored) == 1 {
			matched = true
		}
	}
	return matched
}

// validateDeviceToken distinguishes only a correct secret on a revoked record.
// Every malformed, unknown, or wrong-secret token collapses to unauthorized so
// the response cannot be used to probe for valid token IDs.
func (s *server) validateDeviceToken(value string) (deviceTokenRecord, string) {
	id, secret, ok := parseDeviceToken(value)
	if !ok {
		return deviceTokenRecord{}, "unauthorized"
	}
	digest := sha256.Sum256([]byte(secret))
	s.tokensMu.Lock()
	record, exists := s.deviceTokens.Tokens[id]
	s.tokensMu.Unlock()
	if !exists {
		return deviceTokenRecord{}, "unauthorized"
	}
	stored, err := hex.DecodeString(record.SecretSHA256)
	if err != nil || len(stored) != sha256.Size || subtle.ConstantTimeCompare(digest[:], stored) != 1 {
		return deviceTokenRecord{}, "unauthorized"
	}
	if !record.RevokedAt.IsZero() {
		return record, "token_revoked"
	}
	return record, ""
}

// effectiveSharedTokenMode applies the documented no-admin-token rule: pairing
// and settings are unavailable, so the compatibility phase behaves as allow.
func (s *server) effectiveSharedTokenMode() string {
	if s.adminToken == "" {
		return sharedTokenModeAllow
	}
	s.tokensMu.Lock()
	defer s.tokensMu.Unlock()
	if s.deviceTokens.SharedTokenMode == "" {
		return sharedTokenModeAllow
	}
	return s.deviceTokens.SharedTokenMode
}

// touchDeviceToken binds an unbound token on its first write request and
// refreshes last_used_at at most once per five minutes. Both changes share one
// atomic write.
func (s *server) touchDeviceToken(auth authResult, headerDeviceID string, now time.Time) authResult {
	s.tokensMu.Lock()
	defer s.tokensMu.Unlock()
	record, exists := s.deviceTokens.Tokens[auth.tokenID]
	if !exists || !record.RevokedAt.IsZero() {
		return auth
	}
	boundID := record.DeviceID
	changed := false
	if boundID == "" && validIdentifier(headerDeviceID, 128) {
		boundID = headerDeviceID
		changed = true
	}
	if record.LastUsedAt.IsZero() || now.Sub(record.LastUsedAt) >= deviceTokenTouchInterval {
		record.LastUsedAt = now.UTC()
		changed = true
	}
	if !changed {
		auth.deviceID = boundID
		return auth
	}
	record.DeviceID = boundID
	next := s.deviceTokens.clone()
	next.Tokens[record.ID] = record
	if err := s.persistDeviceTokens(next); err != nil {
		slog.Warn("update device token", "token", record.ID, "error", err)
		return auth
	}
	s.deviceTokens = next
	auth.deviceID = boundID
	return auth
}

// globalClaimFailures pauses every pairing claim once too many codes have been
// guessed in the rolling window.
type globalClaimFailures struct {
	mu       sync.Mutex
	failures int
	start    time.Time
}

func (g *globalClaimFailures) blocked(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.start.IsZero() || now.Sub(g.start) > loginFailureWindow {
		g.failures = 0
		g.start = time.Time{}
		return false
	}
	return g.failures >= globalClaimFailureLimit
}

func (g *globalClaimFailures) fail(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.start.IsZero() || now.Sub(g.start) > loginFailureWindow {
		g.failures = 0
		g.start = now
	}
	g.failures++
}

func normalizePairingCode(value string) (string, bool) {
	value = strings.ToUpper(value)
	var builder strings.Builder
	builder.Grow(pairingCodeLength)
	for _, character := range value {
		switch {
		case unicode.IsSpace(character) || character == '-':
			continue
		case character == 'O':
			character = '0'
		case character == 'I' || character == 'L':
			character = '1'
		}
		if character > unicode.MaxASCII {
			return "", false
		}
		builder.WriteByte(byte(character))
	}
	normalized := builder.String()
	if len(normalized) != pairingCodeLength {
		return "", false
	}
	for index := 0; index < len(normalized); index++ {
		if !strings.ContainsRune(pairingAlphabet, rune(normalized[index])) {
			return "", false
		}
	}
	return normalized, true
}

func randomPairingCode() (string, error) {
	buffer := make([]byte, pairingCodeLength)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	// The alphabet has 32 symbols and 256 is an exact multiple of 32, so
	// reducing each random byte is unbiased.
	for index, value := range buffer {
		buffer[index] = pairingAlphabet[int(value)%len(pairingAlphabet)]
	}
	return string(buffer), nil
}

func formatPairingCode(code string) string {
	if len(code) != pairingCodeLength {
		return code
	}
	return code[:4] + "-" + code[4:]
}

// cleanupExpiredPairingsLocked removes codes that can no longer be claimed. The
// caller must hold tokensMu.
func (s *server) cleanupExpiredPairingsLocked(now time.Time) {
	for id, candidate := range s.pairings {
		if !now.Before(candidate.ExpiresAt) {
			delete(s.pairings, id)
		}
	}
}

type deviceTokenView struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	DeviceID    string    `json:"device_id"`
	DisplayName string    `json:"display_name"`
	Online      bool      `json:"online"`
	CreatedAt   time.Time `json:"created_at"`
	CreatedVia  string    `json:"created_via"`
	LastUsedAt  time.Time `json:"last_used_at,omitzero"`
	RevokedAt   time.Time `json:"revoked_at,omitzero"`
}

type sharedTokenDeviceView struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Platform string    `json:"platform,omitempty"`
	LastSeen time.Time `json:"last_seen"`
}

type deviceTokenListResponse struct {
	Tokens             []deviceTokenView       `json:"tokens"`
	SharedTokenMode    string                  `json:"shared_token_mode"`
	SharedTokenDevices []sharedTokenDeviceView `json:"shared_token_devices"`
}

type pairingView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *server) deviceTokenViewLocked(record deviceTokenRecord, now time.Time) deviceTokenView {
	device := s.dashboard.Devices[record.DeviceID]
	displayName := device.Alias
	if displayName == "" {
		displayName = record.Name
	}
	if displayName == "" {
		displayName = device.Name
	}
	return deviceTokenView{
		ID:          record.ID,
		Name:        record.Name,
		DeviceID:    record.DeviceID,
		DisplayName: displayName,
		Online:      !device.LastSeen.IsZero() && now.Sub(device.LastSeen) <= onlineWindow,
		CreatedAt:   record.CreatedAt,
		CreatedVia:  record.CreatedVia,
		LastUsedAt:  record.LastUsedAt,
		RevokedAt:   record.RevokedAt,
	}
}

func deviceTokenEventDetail(record deviceTokenRecord) string {
	detail := record.ID
	if name := strings.TrimSpace(record.Name); name != "" {
		detail += " " + name
	}
	return detail
}

func (s *server) handleDeviceTokenList(w http.ResponseWriter, _ *http.Request) {
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
	sharedDevices := make([]sharedTokenDeviceView, 0)
	for _, device := range s.dashboard.Devices {
		if device.Auth != "" {
			continue
		}
		name := device.Alias
		if name == "" {
			name = device.Name
		}
		sharedDevices = append(sharedDevices, sharedTokenDeviceView{
			ID: device.ID, Name: name, Platform: device.Platform, LastSeen: device.LastSeen,
		})
	}
	s.tokensMu.Unlock()
	s.mu.Unlock()

	sort.Slice(tokens, func(left, right int) bool {
		if !tokens[left].CreatedAt.Equal(tokens[right].CreatedAt) {
			return tokens[left].CreatedAt.After(tokens[right].CreatedAt)
		}
		return tokens[left].ID > tokens[right].ID
	})
	sort.Slice(sharedDevices, func(left, right int) bool {
		if !sharedDevices[left].LastSeen.Equal(sharedDevices[right].LastSeen) {
			return sharedDevices[left].LastSeen.After(sharedDevices[right].LastSeen)
		}
		return sharedDevices[left].ID < sharedDevices[right].ID
	})
	writeJSON(w, http.StatusOK, deviceTokenListResponse{
		Tokens: tokens, SharedTokenMode: mode, SharedTokenDevices: sharedDevices,
	})
}

func (s *server) handleCreateDeviceToken(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name string `json:"name"`
	}
	if !decodeAdminJSON(w, r, &request) {
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if len([]rune(request.Name)) > 80 {
		writeError(w, http.StatusBadRequest, "name is too long")
		return
	}
	now := s.now()
	s.mu.Lock()
	s.tokensMu.Lock()
	next := s.deviceTokens.clone()
	if len(next.Tokens) >= maxDeviceTokenRecords && !evictOldestRevokedDeviceToken(next.Tokens) {
		s.tokensMu.Unlock()
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "device token limit reached")
		return
	}
	token, record, err := newDeviceTokenRecord(now, request.Name, "", "admin")
	if err != nil {
		s.tokensMu.Unlock()
		s.mu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot generate device token")
		return
	}
	next.Tokens[record.ID] = record
	if err := s.persistDeviceTokens(next); err != nil {
		s.tokensMu.Unlock()
		s.mu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist device token")
		return
	}
	s.deviceTokens = next
	view := s.deviceTokenViewLocked(record, now)
	s.tokensMu.Unlock()
	s.recordEventLocked(event{Kind: eventDeviceTokenCreated, Device: record.DeviceID,
		Detail: deviceTokenEventDetail(record)})
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "record": view})
}

func (s *server) handlePatchDeviceToken(w http.ResponseWriter, r *http.Request) {
	tokenID := r.PathValue("id")
	var request struct {
		Name string `json:"name"`
	}
	if !decodeAdminJSON(w, r, &request) {
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if len([]rune(request.Name)) > 80 {
		writeError(w, http.StatusBadRequest, "name is too long")
		return
	}
	s.tokensMu.Lock()
	defer s.tokensMu.Unlock()
	record, exists := s.deviceTokens.Tokens[tokenID]
	if !exists {
		writeError(w, http.StatusNotFound, "device token not found")
		return
	}
	record.Name = request.Name
	next := s.deviceTokens.clone()
	next.Tokens[tokenID] = record
	if err := s.persistDeviceTokens(next); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot persist device token")
		return
	}
	s.deviceTokens = next
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleRevokeDeviceToken(w http.ResponseWriter, r *http.Request) {
	tokenID := r.PathValue("id")
	now := s.now()
	s.mu.Lock()
	s.tokensMu.Lock()
	record, exists := s.deviceTokens.Tokens[tokenID]
	if !exists {
		s.tokensMu.Unlock()
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "device token not found")
		return
	}
	if !record.RevokedAt.IsZero() {
		s.tokensMu.Unlock()
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	record.RevokedAt = now.UTC()
	next := s.deviceTokens.clone()
	next.Tokens[tokenID] = record
	if err := s.persistDeviceTokens(next); err != nil {
		s.tokensMu.Unlock()
		s.mu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist device token")
		return
	}
	s.deviceTokens = next
	s.tokensMu.Unlock()
	s.recordEventLocked(event{Kind: eventDeviceTokenRevoked, Device: record.DeviceID,
		Detail: deviceTokenEventDetail(record)})
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleDeleteDeviceToken(w http.ResponseWriter, r *http.Request) {
	tokenID := r.PathValue("id")
	s.tokensMu.Lock()
	defer s.tokensMu.Unlock()
	record, exists := s.deviceTokens.Tokens[tokenID]
	if !exists {
		writeError(w, http.StatusNotFound, "device token not found")
		return
	}
	if record.RevokedAt.IsZero() {
		writeError(w, http.StatusConflict, "only revoked device tokens can be deleted")
		return
	}
	next := s.deviceTokens.clone()
	delete(next.Tokens, tokenID)
	if err := s.persistDeviceTokens(next); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot persist device token")
		return
	}
	s.deviceTokens = next
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleListPairings(w http.ResponseWriter, _ *http.Request) {
	now := s.now()
	s.tokensMu.Lock()
	s.cleanupExpiredPairingsLocked(now)
	pairings := make([]pairingView, 0, len(s.pairings))
	for _, candidate := range s.pairings {
		pairings = append(pairings, pairingView{
			ID: candidate.ID, Name: candidate.Name, CreatedAt: candidate.CreatedAt, ExpiresAt: candidate.ExpiresAt,
		})
	}
	s.tokensMu.Unlock()
	sort.Slice(pairings, func(left, right int) bool {
		if !pairings[left].CreatedAt.Equal(pairings[right].CreatedAt) {
			return pairings[left].CreatedAt.After(pairings[right].CreatedAt)
		}
		return pairings[left].ID < pairings[right].ID
	})
	writeJSON(w, http.StatusOK, map[string]any{"pairings": pairings})
}

func (s *server) handleCreatePairing(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name       string `json:"name"`
		TTLMinutes *int   `json:"ttl_minutes"`
	}
	if !decodeAdminJSON(w, r, &request) {
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if len([]rune(request.Name)) > 80 {
		writeError(w, http.StatusBadRequest, "name is too long")
		return
	}
	ttlMinutes := pairingDefaultTTLMinutes
	if request.TTLMinutes != nil {
		ttlMinutes = *request.TTLMinutes
	}
	if ttlMinutes < pairingMinTTLMinutes || ttlMinutes > pairingMaxTTLMinutes {
		writeError(w, http.StatusBadRequest, "ttl_minutes must be between 1 and 60")
		return
	}
	now := s.now()
	s.tokensMu.Lock()
	s.cleanupExpiredPairingsLocked(now)
	if len(s.pairings) >= maxActivePairings {
		s.tokensMu.Unlock()
		writeError(w, http.StatusConflict, "too many active pairing codes")
		return
	}
	code, err := randomPairingCode()
	if err != nil {
		s.tokensMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot generate pairing code")
		return
	}
	id, err := randomHex(deviceTokenIDBytes)
	if err != nil {
		s.tokensMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot generate pairing code")
		return
	}
	digest := sha256.Sum256([]byte(code))
	candidate := pairing{
		ID:         id,
		Name:       request.Name,
		CodeSHA256: hex.EncodeToString(digest[:]),
		CreatedAt:  now.UTC(),
		ExpiresAt:  now.Add(time.Duration(ttlMinutes) * time.Minute).UTC(),
	}
	s.pairings[id] = candidate
	s.tokensMu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         candidate.ID,
		"code":       formatPairingCode(code),
		"expires_at": candidate.ExpiresAt,
	})
}

func (s *server) handleDeletePairing(w http.ResponseWriter, r *http.Request) {
	pairingID := r.PathValue("id")
	now := s.now()
	s.tokensMu.Lock()
	defer s.tokensMu.Unlock()
	s.cleanupExpiredPairingsLocked(now)
	if _, exists := s.pairings[pairingID]; !exists {
		writeError(w, http.StatusNotFound, "pairing not found")
		return
	}
	delete(s.pairings, pairingID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handlePutAuthSettings(w http.ResponseWriter, r *http.Request) {
	var request struct {
		SharedTokenMode string `json:"shared_token_mode"`
	}
	if !decodeAdminJSON(w, r, &request) {
		return
	}
	mode := strings.TrimSpace(request.SharedTokenMode)
	if !validSharedTokenMode(mode) {
		writeError(w, http.StatusBadRequest, "invalid shared_token_mode")
		return
	}
	s.mu.Lock()
	s.tokensMu.Lock()
	next := s.deviceTokens.clone()
	next.SharedTokenMode = mode
	if err := s.persistDeviceTokens(next); err != nil {
		s.tokensMu.Unlock()
		s.mu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist authentication settings")
		return
	}
	s.deviceTokens = next
	s.tokensMu.Unlock()
	s.recordEventLocked(event{Kind: eventSettings, Detail: "shared_token_mode=" + mode})
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// handlePairClaim exchanges a one-time pairing code for a device token. The
// check for a global or per-IP pause deliberately happens before the code is
// inspected, so a blocked client cannot distinguish a correct code.
func (s *server) handlePairClaim(w http.ResponseWriter, r *http.Request) {
	if s.adminToken == "" {
		http.NotFound(w, r)
		return
	}
	address := remoteHost(r)
	now := s.now()
	if s.claimLogins.blocked(address, now) || s.claimFailures.blocked(now) {
		writeErrorCode(w, http.StatusTooManyRequests, "rate_limited", "too many failed pairing attempts")
		return
	}

	var request struct {
		Code       string `json:"code"`
		DeviceID   string `json:"device_id"`
		DeviceName string `json:"device_name"`
		Platform   string `json:"platform"`
		Version    string `json:"version"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	request.DeviceID = strings.TrimSpace(request.DeviceID)
	if strings.TrimSpace(request.Code) == "" || !validIdentifier(request.DeviceID, 128) {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "code and device_id are required")
		return
	}
	deviceName := trimDeviceField(request.DeviceName, 80)
	platform := trimDeviceField(request.Platform, 32)
	tokenVersion := trimDeviceField(request.Version, 64)
	normalized, valid := normalizePairingCode(request.Code)
	if !valid {
		s.claimLogins.fail(address, now)
		s.claimFailures.fail(now)
		writeErrorCode(w, http.StatusUnauthorized, "pairing_invalid", "pairing code is invalid or expired")
		return
	}

	digest := sha256.Sum256([]byte(normalized))
	s.mu.Lock()
	s.tokensMu.Lock()
	s.cleanupExpiredPairingsLocked(now)
	var claimed pairing
	claimedID := ""
	for id, candidate := range s.pairings {
		stored, err := hex.DecodeString(candidate.CodeSHA256)
		if err != nil || len(stored) != sha256.Size {
			continue
		}
		// Every candidate is compared even after a match so lookup work does
		// not reveal the position of the matching entry.
		if subtle.ConstantTimeCompare(digest[:], stored) == 1 && claimedID == "" {
			claimedID, claimed = id, candidate
		}
	}
	if claimedID == "" {
		s.tokensMu.Unlock()
		s.mu.Unlock()
		s.claimLogins.fail(address, now)
		s.claimFailures.fail(now)
		writeErrorCode(w, http.StatusUnauthorized, "pairing_invalid", "pairing code is invalid or expired")
		return
	}
	nextTokens := s.deviceTokens.clone()
	if len(nextTokens.Tokens) >= maxDeviceTokenRecords && !evictOldestRevokedDeviceToken(nextTokens.Tokens) {
		s.tokensMu.Unlock()
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "device token limit reached")
		return
	}
	name := deviceName
	if name == "" {
		name = strings.TrimSpace(claimed.Name)
	}
	token, record, err := newDeviceTokenRecord(now, name, request.DeviceID, "pairing")
	if err != nil {
		s.tokensMu.Unlock()
		s.mu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot generate device token")
		return
	}
	nextTokens.Tokens[record.ID] = record
	if err := s.persistDeviceTokens(nextTokens); err != nil {
		s.tokensMu.Unlock()
		s.mu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist device token")
		return
	}
	s.deviceTokens = nextTokens
	delete(s.pairings, claimedID)

	device := s.dashboard.Devices[request.DeviceID]
	device.ID = request.DeviceID
	if name != "" {
		device.Name = name
	}
	device.Platform = platform
	device.Version = tokenVersion
	device.Address = address
	if device.FirstSeen.IsZero() {
		device.FirstSeen = now.UTC()
	}
	device.LastSeen = now.UTC()
	device.Auth = "device"
	device.TokenID = record.ID
	nextDashboard := s.dashboard.clone()
	nextDashboard.Devices[request.DeviceID] = device
	if err := s.persistDashboard(nextDashboard); err != nil {
		slog.Warn("record paired device", "device", request.DeviceID, "error", err)
	} else {
		s.dashboard = nextDashboard
	}
	// Events are recorded under s.mu only; tokensMu is never held while calling out.
	s.tokensMu.Unlock()
	s.recordEventLocked(event{Kind: eventDevicePaired, Device: request.DeviceID,
		Detail: deviceTokenEventDetail(record)})
	s.mu.Unlock()

	s.claimLogins.reset(address)
	writeJSON(w, http.StatusOK, map[string]any{
		"token":          token,
		"token_id":       record.ID,
		"device_id":      request.DeviceID,
		"name":           name,
		"server_version": version,
	})
}

func (s *server) handleWhoAmI(w http.ResponseWriter, r *http.Request) {
	auth, ok := authResultFromRequest(r)
	if !ok {
		writeErrorCode(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	sharedTokenMode := s.effectiveSharedTokenMode()
	if auth.kind != "device" {
		writeJSON(w, http.StatusOK, struct {
			Auth            string `json:"auth"`
			SharedTokenMode string `json:"shared_token_mode"`
		}{Auth: "shared", SharedTokenMode: sharedTokenMode})
		return
	}
	response := struct {
		Auth            string `json:"auth"`
		TokenID         string `json:"token_id"`
		DeviceID        string `json:"device_id"`
		Name            string `json:"name"`
		SharedTokenMode string `json:"shared_token_mode"`
	}{
		Auth: "device", TokenID: auth.tokenID, DeviceID: auth.deviceID, SharedTokenMode: sharedTokenMode,
	}
	s.tokensMu.Lock()
	if record, exists := s.deviceTokens.Tokens[auth.tokenID]; exists {
		response.Name = record.Name
		if response.DeviceID == "" {
			response.DeviceID = record.DeviceID
		}
	}
	s.tokensMu.Unlock()
	writeJSON(w, http.StatusOK, response)
}

// evictOldestRevokedDeviceToken deletes the oldest revoked record so repeated
// revoke/re-pair cycles do not permanently exhaust maxDeviceTokenRecords.
func evictOldestRevokedDeviceToken(tokens map[string]deviceTokenRecord) bool {
	var oldestID string
	var oldestRevoked time.Time
	for id, record := range tokens {
		if record.RevokedAt.IsZero() {
			continue
		}
		if oldestID == "" || record.RevokedAt.Before(oldestRevoked) {
			oldestID = id
			oldestRevoked = record.RevokedAt
		}
	}
	if oldestID == "" {
		return false
	}
	delete(tokens, oldestID)
	return true
}

// deviceTokenCounts is used by the Prometheus endpoint.
func (s *server) deviceTokenCounts() (active, revoked int) {
	s.tokensMu.Lock()
	defer s.tokensMu.Unlock()
	for _, record := range s.deviceTokens.Tokens {
		if record.RevokedAt.IsZero() {
			active++
		} else {
			revoked++
		}
	}
	return active, revoked
}
