// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// saveVaultScopeCandidate is supplied by an SaveVault client after it has
// uploaded an immutable archive. The server verifies that the archive bytes
// match ContentDigest before it may become a cloud head.
type saveVaultScopeCandidate struct {
	ID            string    `json:"id"`
	Archive       string    `json:"archive"`
	Parent        string    `json:"parent"`
	ContentDigest string    `json:"contentDigest"`
	CreatedAt     time.Time `json:"createdAt"`
}

// saveVaultScopeSnapshot is the persisted, server-audited form of a candidate.
// Device fields come from authenticated request headers and are informational;
// the bearer token remains the authorization boundary.
type saveVaultScopeSnapshot struct {
	saveVaultScopeCandidate
	Device     string    `json:"device,omitempty"`
	DeviceName string    `json:"deviceName,omitempty"`
	Platform   string    `json:"platform,omitempty"`
	ReceivedAt time.Time `json:"receivedAt"`
	Conflict   bool      `json:"conflict,omitempty"`
	ConflictOf string    `json:"conflictOf,omitempty"`
	PromotedAt time.Time `json:"promotedAt,omitzero"`
	// Locked and LockNote are response-only. They are never assigned on the
	// persisted state and therefore never appear in savevault-native-state.json.
	Locked   bool   `json:"locked,omitempty"`
	LockNote string `json:"lockNote,omitempty"`
}

type saveVaultScopeState struct {
	Head      string                           `json:"head"`
	Snapshots map[string]saveVaultScopeSnapshot `json:"snapshots"`
	// Identity is what a client reported about the game behind this scope;
	// GameLink is a game key set by hand on the dashboard. Both only group
	// scopes into games (savevault_games.go) and never move a head.
	Identity *saveVaultScopeIdentity `json:"identity,omitempty"`
	GameLink string                 `json:"gameLink,omitempty"`
}

// saveVaultNativeState is deliberately independent from Eden's encrypted scope
// state. SaveVault archives are opaque ZIPs and are only related by the common
// authenticated service and device inventory.
type saveVaultNativeState struct {
	Scopes map[string]saveVaultScopeState `json:"scopes"`
}

type saveVaultScopeResponse struct {
	Type      string                  `json:"type"`
	GameID    string                  `json:"gameId"`
	Head      *saveVaultScopeSnapshot  `json:"head"`
	Snapshots []saveVaultScopeSnapshot `json:"snapshots"`
	Conflicts []saveVaultScopeSnapshot `json:"conflicts"`
}

// saveVaultScopeAudit is the administrator-facing representation of one
// native SaveVault namespace. It deliberately exposes lineage and source
// fields, rather than presenting a timestamp as an authority decision.
type saveVaultScopeAudit struct {
	Type           string                  `json:"type"`
	GameID         string                  `json:"gameId"`
	Head           *saveVaultScopeSnapshot  `json:"head"`
	Snapshots      []saveVaultScopeSnapshot `json:"snapshots"`
	Conflicts      []saveVaultScopeSnapshot `json:"conflicts"`
	SnapshotCount  int                     `json:"snapshotCount"`
	ConflictCount  int                     `json:"conflictCount"`
	ArchiveBytes   int64                   `json:"archiveBytes"`
	LastReceivedAt time.Time               `json:"lastReceivedAt,omitzero"`
	HeadDevice     string                  `json:"headDevice,omitempty"`
	HeadDeviceName string                  `json:"headDeviceName,omitempty"`
	HeadPlatform   string                  `json:"headPlatform,omitempty"`
}

func (state saveVaultNativeState) clone() saveVaultNativeState {
	next := saveVaultNativeState{Scopes: make(map[string]saveVaultScopeState, len(state.Scopes)+1)}
	for key, value := range state.Scopes {
		next.Scopes[key] = value
	}
	return next
}

func (state saveVaultScopeState) clone() saveVaultScopeState {
	next := saveVaultScopeState{
		Head:      state.Head,
		Snapshots: make(map[string]saveVaultScopeSnapshot, len(state.Snapshots)+1),
		Identity:  state.Identity,
		GameLink:  state.GameLink,
	}
	for key, value := range state.Snapshots {
		next.Snapshots[key] = value
	}
	return next
}

func validSaveVaultGameID(value string) bool {
	return validIdentifier(value, 128)
}

func validSaveVaultContentDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func saveVaultScopeKey(kind, gameID string) string {
	return kind + "\x00" + gameID
}

// saveVaultEventScope identifies a typed SaveVault scope in the shared activity
// stream. Eden scope identifiers cannot contain ':' or '/', so this format is
// unambiguous and lets the dashboard link to the matching SaveVault view.
func saveVaultEventScope(kind, gameID string) string {
	return "savevault:" + kind + "/" + gameID
}

func saveVaultEventScopeName(value string) (string, bool) {
	var rest string
	switch {
	case strings.HasPrefix(value, "savevault:"):
		rest = strings.TrimPrefix(value, "savevault:")
	case strings.HasPrefix(value, "opensave:"):
		rest = strings.TrimPrefix(value, "opensave:")
	default:
		return "", false
	}
	kind, gameID, ok := strings.Cut(rest, "/")
	if !ok || !validSaveVaultType(kind) || !validSaveVaultGameID(gameID) {
		return "", false
	}
	return "SaveVault " + kind + " / " + gameID, true
}

// recordSaveVaultScopeEvent runs after a native scope write has committed and
// released saveVaultMu. Keeping the locks separate prevents activity and Bark
// persistence from becoming part of the archive/state critical section.
func (s *server) recordSaveVaultScopeEvent(eventKind, kind, gameID string, snapshot saveVaultScopeSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordEventLocked(event{
		Kind:     eventKind,
		Scope:    saveVaultEventScope(kind, gameID),
		Snapshot: snapshot.ID,
		Device:   snapshot.Device,
		Detail:   snapshot.Archive,
	})
}

// saveVaultArchiveMatchesScope keeps a client from attaching one game's ZIP
// to another game's head. game-save still treats ZIP contents as opaque; this
// only verifies the stable archive-name contract shared by SaveVault clients.
func saveVaultArchiveMatchesScope(gameID, archive string) bool {
	base := strings.TrimSuffix(archive, ".zip")
	parts := strings.SplitN(base, "__", 3)
	return len(parts) == 3 && parts[0] == gameID && parts[1] != "" && parts[2] != ""
}

func (s *server) saveVaultStatePath() string {
	return filepath.Join(s.dataDir, "savevault-native-state.json")
}

func (s *server) loadSaveVaultState() error {
	if err := readJSONFile(s.saveVaultStatePath(), &s.saveVaultState); err != nil {
		return err
	}
	if s.saveVaultState.Scopes == nil {
		s.saveVaultState.Scopes = make(map[string]saveVaultScopeState)
	}
	for key, scope := range s.saveVaultState.Scopes {
		if scope.Snapshots == nil {
			scope.Snapshots = make(map[string]saveVaultScopeSnapshot)
			s.saveVaultState.Scopes[key] = scope
		}
	}
	return nil
}

func (s *server) persistSaveVaultState(state saveVaultNativeState) error {
	return writeJSONFile(s.saveVaultStatePath(), state)
}

func saveVaultScopeFromRequest(w http.ResponseWriter, r *http.Request) (string, string, string, bool) {
	kind, gameID := r.PathValue("type"), r.PathValue("game")
	if !validSaveVaultType(kind) || !validSaveVaultGameID(gameID) {
		writeError(w, http.StatusBadRequest, "invalid SaveVault type or game id")
		return "", "", "", false
	}
	return kind, gameID, saveVaultScopeKey(kind, gameID), true
}

func decodeSaveVaultScopeCandidate(w http.ResponseWriter, r *http.Request, snapshotID string) (saveVaultScopeCandidate, bool) {
	var candidate saveVaultScopeCandidate
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMetadataBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&candidate); err != nil {
		writeError(w, http.StatusBadRequest, "invalid SaveVault snapshot metadata")
		return saveVaultScopeCandidate{}, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid SaveVault snapshot metadata")
		return saveVaultScopeCandidate{}, false
	}
	if candidate.ID != snapshotID || !validSnapshotID(candidate.ID) || !validSaveVaultName(candidate.Archive) ||
		(candidate.Parent != "" && !validSnapshotID(candidate.Parent)) ||
		!validSaveVaultContentDigest(candidate.ContentDigest) || candidate.CreatedAt.IsZero() {
		writeError(w, http.StatusBadRequest, "invalid SaveVault snapshot fields")
		return saveVaultScopeCandidate{}, false
	}
	candidate.CreatedAt = candidate.CreatedAt.UTC()
	return candidate, true
}

func saveVaultCandidateMatchesSnapshot(candidate saveVaultScopeCandidate, stored saveVaultScopeSnapshot) bool {
	return candidate.ID == stored.ID && candidate.Archive == stored.Archive &&
		candidate.Parent == stored.Parent && candidate.ContentDigest == stored.ContentDigest &&
		candidate.CreatedAt.Equal(stored.CreatedAt)
}

// expectedSaveVaultHead parses a strong ETag in the same form returned by the
// head endpoint. An empty quoted tag ("") is the only valid precondition for
// creating a scope's first head.
func expectedSaveVaultHead(r *http.Request) (string, bool) {
	value := strings.TrimSpace(r.Header.Get("If-Match"))
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return "", false
	}
	value = value[1 : len(value)-1]
	return value, value == "" || validSnapshotID(value)
}

func (s *server) saveVaultCandidateArchiveMatchesLocked(kind string, candidate saveVaultScopeCandidate) (int, string, bool) {
	digest, err := s.saveVaultArchiveDigestLocked(kind, candidate.Archive)
	if errors.Is(err, os.ErrNotExist) {
		return http.StatusFailedDependency, "SaveVault archive is missing", false
	}
	if err != nil {
		return http.StatusInternalServerError, "cannot inspect SaveVault archive", false
	}
	if digest != candidate.ContentDigest {
		return http.StatusUnprocessableEntity, "SaveVault archive digest does not match snapshot", false
	}
	return 0, "", true
}

func (s *server) saveVaultCandidateArchiveMatchesPrehashedLocked(
	kind string,
	candidate saveVaultScopeCandidate,
	preDigest string,
	preInfo os.FileInfo,
	preErr error,
) (int, string, bool) {
	if preErr == nil && preInfo != nil {
		if path, ok := s.saveVaultPath(kind, candidate.Archive); ok {
			if currentInfo, err := os.Stat(path); err == nil && os.SameFile(preInfo, currentInfo) {
				if preDigest != candidate.ContentDigest {
					return http.StatusUnprocessableEntity, "SaveVault archive digest does not match snapshot", false
				}
				return 0, "", true
			}
		}
	}
	return s.saveVaultCandidateArchiveMatchesLocked(kind, candidate)
}

func saveVaultSnapshotFromRequest(candidate saveVaultScopeCandidate, r *http.Request, now time.Time) saveVaultScopeSnapshot {
	return saveVaultScopeSnapshot{
		saveVaultScopeCandidate: candidate,
		Device:                 requestDeviceID(r),
		DeviceName:             trimDeviceField(requestClientHeader(r, "X-Eden-Device-Name", "X-SaveVault-Device-Name"), 80),
		Platform:               trimDeviceField(requestClientHeader(r, "X-Eden-Platform", "X-SaveVault-Platform"), 32),
		ReceivedAt:             now.UTC(),
	}
}

func writeSaveVaultHeadConflict(w http.ResponseWriter, message, head string) {
	writeJSON(w, http.StatusConflict, map[string]string{"error": message, "head": head})
}

func (s *server) handleSaveVaultScope(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := saveVaultScopeFromRequest(w, r)
	if !ok {
		return
	}
	if tombstone, blocked := s.saveVaultScopeBlocked(kind, gameID); blocked {
		writeSaveVaultTombstone(w, tombstone)
		return
	}

	response := saveVaultScopeResponse{
		Type:      kind,
		GameID:    gameID,
		Snapshots: []saveVaultScopeSnapshot{},
		Conflicts: []saveVaultScopeSnapshot{},
	}
	s.saveVaultMu.Lock()
	if tombstone, blocked := s.saveVaultScopeBlockedLocked(kind, gameID); blocked {
		s.saveVaultMu.Unlock()
		writeSaveVaultTombstone(w, tombstone)
		return
	}
	locks := s.saveVaultLocks(kind, gameID)
	stored, exists := s.saveVaultState.Scopes[key]
	if exists {
		for _, item := range stored.Snapshots {
			applySaveVaultLock(&item, locks)
			response.Snapshots = append(response.Snapshots, item)
			if item.Conflict {
				response.Conflicts = append(response.Conflicts, item)
			}
		}
		if stored.Head != "" {
			if item, found := stored.Snapshots[stored.Head]; found {
				itemCopy := item
				applySaveVaultLock(&itemCopy, locks)
				response.Head = &itemCopy
			}
		}
	}
	s.saveVaultMu.Unlock()
	sort.Slice(response.Snapshots, func(i, j int) bool {
		if !response.Snapshots[i].ReceivedAt.Equal(response.Snapshots[j].ReceivedAt) {
			return response.Snapshots[i].ReceivedAt.After(response.Snapshots[j].ReceivedAt)
		}
		return response.Snapshots[i].ID > response.Snapshots[j].ID
	})
	sort.Slice(response.Conflicts, func(i, j int) bool {
		if !response.Conflicts[i].ReceivedAt.Equal(response.Conflicts[j].ReceivedAt) {
			return response.Conflicts[i].ReceivedAt.After(response.Conflicts[j].ReceivedAt)
		}
		return response.Conflicts[i].ID > response.Conflicts[j].ID
	})
	writeJSON(w, http.StatusOK, response)
}

func (s *server) listSaveVaultScopeAudits() []saveVaultScopeAudit {
	s.saveVaultMu.Lock()
	defer s.saveVaultMu.Unlock()
	audits := make([]saveVaultScopeAudit, 0, len(s.saveVaultState.Scopes))
	for key, stored := range s.saveVaultState.Scopes {
		kind, gameID, ok := strings.Cut(key, "\x00")
		if !ok || !validSaveVaultType(kind) || !validSaveVaultGameID(gameID) {
			continue // an invalid persisted key is not a safe dashboard entry
		}
		audits = append(audits, s.saveVaultScopeAuditLocked(kind, gameID, stored))
	}
	sort.Slice(audits, func(i, j int) bool {
		if !audits[i].LastReceivedAt.Equal(audits[j].LastReceivedAt) {
			return audits[i].LastReceivedAt.After(audits[j].LastReceivedAt)
		}
		if audits[i].Type != audits[j].Type {
			return audits[i].Type < audits[j].Type
		}
		return audits[i].GameID < audits[j].GameID
	})
	return audits
}

// saveVaultScopeAuditLocked must be called while saveVaultMu is held.
func (s *server) saveVaultScopeAuditLocked(kind, gameID string, stored saveVaultScopeState) saveVaultScopeAudit {
	audit := saveVaultScopeAudit{
		Type:      kind,
		GameID:    gameID,
		Snapshots: []saveVaultScopeSnapshot{},
		Conflicts: []saveVaultScopeSnapshot{},
	}
	locks := s.saveVaultLocks(kind, gameID)
	archives := make(map[string]struct{}, len(stored.Snapshots))
	for _, item := range stored.Snapshots {
		applySaveVaultLock(&item, locks)
		audit.SnapshotCount++
		audit.Snapshots = append(audit.Snapshots, item)
		if item.Conflict {
			audit.ConflictCount++
			audit.Conflicts = append(audit.Conflicts, item)
		}
		// Snapshots may share an immutable archive; count it once.
		if _, seen := archives[item.Archive]; !seen {
			archives[item.Archive] = struct{}{}
			if path, ok := s.saveVaultPath(kind, item.Archive); ok {
				if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
					audit.ArchiveBytes += info.Size()
				}
			}
		}
		if item.ReceivedAt.After(audit.LastReceivedAt) {
			audit.LastReceivedAt = item.ReceivedAt
		}
	}
	if stored.Head != "" {
		if item, found := stored.Snapshots[stored.Head]; found {
			itemCopy := item
			applySaveVaultLock(&itemCopy, locks)
			audit.Head = &itemCopy
			audit.HeadDevice = item.Device
			audit.HeadDeviceName = item.DeviceName
			audit.HeadPlatform = item.Platform
		}
	}
	sort.Slice(audit.Snapshots, func(i, j int) bool {
		if !audit.Snapshots[i].ReceivedAt.Equal(audit.Snapshots[j].ReceivedAt) {
			return audit.Snapshots[i].ReceivedAt.After(audit.Snapshots[j].ReceivedAt)
		}
		return audit.Snapshots[i].ID > audit.Snapshots[j].ID
	})
	sort.Slice(audit.Conflicts, func(i, j int) bool {
		if !audit.Conflicts[i].ReceivedAt.Equal(audit.Conflicts[j].ReceivedAt) {
			return audit.Conflicts[i].ReceivedAt.After(audit.Conflicts[j].ReceivedAt)
		}
		return audit.Conflicts[i].ID > audit.Conflicts[j].ID
	})
	return audit
}

func (s *server) handleSaveVaultScopeHead(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := saveVaultScopeFromRequest(w, r)
	if !ok {
		return
	}
	if tombstone, blocked := s.saveVaultScopeBlocked(kind, gameID); blocked {
		writeSaveVaultTombstone(w, tombstone)
		return
	}
	s.saveVaultMu.Lock()
	if tombstone, blocked := s.saveVaultScopeBlockedLocked(kind, gameID); blocked {
		s.saveVaultMu.Unlock()
		writeSaveVaultTombstone(w, tombstone)
		return
	}
	stored, exists := s.saveVaultState.Scopes[key]
	if !exists || stored.Head == "" {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusNotFound, "SaveVault scope has no current snapshot")
		return
	}
	head, exists := stored.Snapshots[stored.Head]
	if exists {
		applySaveVaultLock(&head, s.saveVaultLocks(kind, gameID))
	}
	s.saveVaultMu.Unlock()
	if !exists {
		writeError(w, http.StatusInternalServerError, "SaveVault scope state is invalid")
		return
	}
	w.Header().Set("ETag", `"`+head.ID+`"`)
	writeJSON(w, http.StatusOK, head)
}

func (s *server) handlePutSaveVaultScopeSnapshot(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := saveVaultScopeFromRequest(w, r)
	if !ok {
		return
	}
	if tombstone, blocked := s.saveVaultScopeBlocked(kind, gameID); blocked {
		writeSaveVaultTombstone(w, tombstone)
		return
	}
	snapshotID := r.PathValue("snapshot")
	if !validSnapshotID(snapshotID) {
		writeError(w, http.StatusBadRequest, "invalid SaveVault snapshot id")
		return
	}
	candidate, ok := decodeSaveVaultScopeCandidate(w, r, snapshotID)
	if !ok {
		return
	}
	if !saveVaultArchiveMatchesScope(gameID, candidate.Archive) {
		writeError(w, http.StatusBadRequest, "SaveVault archive does not belong to this game scope")
		return
	}
	expectedHead, ok := expectedSaveVaultHead(r)
	if !ok {
		writeError(w, http.StatusPreconditionRequired, "If-Match must contain the confirmed SaveVault head")
		return
	}

	// Hash the archive before acquiring saveVaultMu so large files do not stall
	// unrelated scope reads/writes; under the lock we confirm the same file is still present.
	preDigest, preInfo, preErr := s.saveVaultArchiveDigestWithInfo(kind, candidate.Archive)

	s.saveVaultMu.Lock()
	if tombstone, blocked := s.saveVaultScopeBlockedLocked(kind, gameID); blocked {
		s.saveVaultMu.Unlock()
		writeSaveVaultTombstone(w, tombstone)
		return
	}
	if status, message, matches := s.saveVaultCandidateArchiveMatchesPrehashedLocked(kind, candidate, preDigest, preInfo, preErr); !matches {
		s.saveVaultMu.Unlock()
		writeError(w, status, message)
		return
	}
	stored, existed := s.saveVaultState.Scopes[key]
	if existing, exists := stored.Snapshots[snapshotID]; exists {
		s.saveVaultMu.Unlock()
		if saveVaultCandidateMatchesSnapshot(candidate, existing) && !existing.Conflict && stored.Head == snapshotID {
			w.Header().Set("ETag", `"`+existing.ID+`"`)
			writeJSON(w, http.StatusOK, existing)
			return
		}
		writeSaveVaultHeadConflict(w, "SaveVault snapshot id already exists", stored.Head)
		return
	}
	if stored.Head != expectedHead || candidate.Parent != stored.Head {
		s.saveVaultMu.Unlock()
		writeSaveVaultHeadConflict(w, "SaveVault scope head changed", stored.Head)
		return
	}

	nextScope := stored.clone()
	recorded := saveVaultSnapshotFromRequest(candidate, r, time.Now())
	nextScope.Head = snapshotID
	nextScope.Snapshots[snapshotID] = recorded
	nextState := s.saveVaultState.clone()
	nextState.Scopes[key] = nextScope
	if err := s.persistSaveVaultState(nextState); err != nil {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist SaveVault scope state")
		return
	}
	s.saveVaultState = nextState
	gameKey, _ := saveVaultGameKey(kind, gameID, nextScope)
	s.saveVaultMu.Unlock()
	if !existed {
		s.queueSaveVaultCoverFetch(gameKey)
	}
	s.recordSaveVaultScopeEvent(eventUpload, kind, gameID, recorded)
	w.Header().Set("ETag", `"`+recorded.ID+`"`)
	writeJSON(w, http.StatusCreated, recorded)
}

// handlePromoteSaveVaultScopeSnapshot moves a scope head to an existing
// snapshot under compare-and-swap. The administrator must present the head
// they saw so a concurrent client upload is never silently overwritten.
func (s *server) handlePromoteSaveVaultScopeSnapshot(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := saveVaultScopeFromRequest(w, r)
	if !ok {
		return
	}
	var request struct {
		Snapshot     string `json:"snapshot"`
		ExpectedHead string `json:"expectedHead"`
	}
	if !decodeAdminJSON(w, r, &request) {
		return
	}
	if !validSnapshotID(request.Snapshot) {
		writeError(w, http.StatusBadRequest, "invalid SaveVault snapshot id")
		return
	}

	s.saveVaultMu.Lock()
	if tombstone, blocked := s.saveVaultScopeBlockedLocked(kind, gameID); blocked {
		s.saveVaultMu.Unlock()
		if tombstone.State == saveVaultTombstoneRetired {
			writeError(w, http.StatusConflict, "retired SaveVault scope is read-only; unretire it first")
		} else {
			writeError(w, http.StatusConflict, "deleted SaveVault scope cannot be promoted")
		}
		return
	}
	stored, exists := s.saveVaultState.Scopes[key]
	if !exists {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusNotFound, "SaveVault scope not found")
		return
	}
	snapshot, exists := stored.Snapshots[request.Snapshot]
	if !exists {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusNotFound, "SaveVault snapshot not found")
		return
	}
	if stored.Head != request.ExpectedHead {
		s.saveVaultMu.Unlock()
		writeSaveVaultHeadConflict(w, "SaveVault scope head changed", stored.Head)
		return
	}
	archivePath, validPath := s.saveVaultPath(kind, snapshot.Archive)
	if !validPath {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusInternalServerError, "SaveVault scope state is invalid")
		return
	}
	archiveInfo, err := os.Stat(archivePath)
	if errors.Is(err, os.ErrNotExist) || (err == nil && !archiveInfo.Mode().IsRegular()) {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusConflict, "SaveVault archive is missing for this snapshot")
		return
	}
	if err != nil {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot inspect SaveVault archive")
		return
	}
	if stored.Head == snapshot.ID {
		s.saveVaultMu.Unlock()
		w.Header().Set("ETag", `"`+snapshot.ID+`"`)
		writeJSON(w, http.StatusOK, snapshot)
		return
	}

	nextScope := stored.clone()
	promoted := nextScope.Snapshots[snapshot.ID]
	promoted.Conflict = false
	promoted.PromotedAt = time.Now().UTC()
	nextScope.Head = promoted.ID
	nextScope.Snapshots[promoted.ID] = promoted
	nextState := s.saveVaultState.clone()
	nextState.Scopes[key] = nextScope
	if err := s.persistSaveVaultState(nextState); err != nil {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist SaveVault scope state")
		return
	}
	s.saveVaultState = nextState
	s.saveVaultMu.Unlock()
	s.recordSaveVaultScopeEvent(eventPromote, kind, gameID, promoted)
	w.Header().Set("ETag", `"`+promoted.ID+`"`)
	writeJSON(w, http.StatusOK, promoted)
}

// handlePutSaveVaultScopeConflict retains a divergent archive after a CAS
// failure. Conflict snapshots never advance Head and are never auto-pruned.
func (s *server) handlePutSaveVaultScopeConflict(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := saveVaultScopeFromRequest(w, r)
	if !ok {
		return
	}
	if tombstone, blocked := s.saveVaultScopeBlocked(kind, gameID); blocked {
		writeSaveVaultTombstone(w, tombstone)
		return
	}
	snapshotID := r.PathValue("snapshot")
	if !validSnapshotID(snapshotID) {
		writeError(w, http.StatusBadRequest, "invalid SaveVault snapshot id")
		return
	}
	candidate, ok := decodeSaveVaultScopeCandidate(w, r, snapshotID)
	if !ok {
		return
	}
	if !saveVaultArchiveMatchesScope(gameID, candidate.Archive) {
		writeError(w, http.StatusBadRequest, "SaveVault archive does not belong to this game scope")
		return
	}

	// Hash the archive before acquiring saveVaultMu so large files do not stall
	// unrelated scope reads/writes; under the lock we confirm the same file is still present.
	preDigest, preInfo, preErr := s.saveVaultArchiveDigestWithInfo(kind, candidate.Archive)

	s.saveVaultMu.Lock()
	if tombstone, blocked := s.saveVaultScopeBlockedLocked(kind, gameID); blocked {
		s.saveVaultMu.Unlock()
		writeSaveVaultTombstone(w, tombstone)
		return
	}
	if status, message, matches := s.saveVaultCandidateArchiveMatchesPrehashedLocked(kind, candidate, preDigest, preInfo, preErr); !matches {
		s.saveVaultMu.Unlock()
		writeError(w, status, message)
		return
	}
	stored, exists := s.saveVaultState.Scopes[key]
	if !exists || stored.Head == "" {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusNotFound, "SaveVault scope has no current snapshot")
		return
	}
	if existing, exists := stored.Snapshots[snapshotID]; exists {
		s.saveVaultMu.Unlock()
		if saveVaultCandidateMatchesSnapshot(candidate, existing) && existing.Conflict {
			writeJSON(w, http.StatusOK, existing)
			return
		}
		writeSaveVaultHeadConflict(w, "SaveVault snapshot id already exists", stored.Head)
		return
	}
	if candidate.Parent == stored.Head {
		s.saveVaultMu.Unlock()
		writeSaveVaultHeadConflict(w, "SaveVault snapshot still matches the current head", stored.Head)
		return
	}

	nextScope := stored.clone()
	recorded := saveVaultSnapshotFromRequest(candidate, r, time.Now())
	recorded.Conflict = true
	recorded.ConflictOf = stored.Head
	nextScope.Snapshots[snapshotID] = recorded
	nextState := s.saveVaultState.clone()
	nextState.Scopes[key] = nextScope
	if err := s.persistSaveVaultState(nextState); err != nil {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist SaveVault conflict snapshot")
		return
	}
	s.saveVaultState = nextState
	s.saveVaultMu.Unlock()
	s.recordSaveVaultScopeEvent(eventConflict, kind, gameID, recorded)
	writeJSON(w, http.StatusCreated, recorded)
}

// saveVaultArchiveReferencedLocked prevents deleting a head, conflict, or
// historical recovery archive that is still named by persisted scope state.
func (s *server) saveVaultArchiveReferencedLocked(kind, name string) bool {
	return saveVaultArchiveReferencedInState(s.saveVaultState, kind, name)
}
