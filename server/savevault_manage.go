// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

// C6 SaveVault management state lives in savevault-manage.json. Existing state
// files never gain C6 fields, so a v1.8.0 binary can ignore this file and keep
// reading the data directory.
//
// Lock order: saveVaultMu -> manageMu and s.mu -> manageMu. manageMu is a leaf
// lock: while it is held no other lock may be acquired. All state updates are
// copy-on-write and only replace the in-memory value after an atomic write.

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	saveVaultManageFileName     = "savevault-manage.json"
	saveVaultTrashDirectoryName = "savevault-trash"

	saveVaultTombstoneDeleted = "deleted"
	saveVaultTombstoneRetired = "retired"

	saveVaultTombstoneRetention = 7 * 24 * time.Hour
	saveVaultOrphanGrace        = 24 * time.Hour
	saveVaultMaxLockNoteRunes   = 120
)

type saveVaultManageTombstone struct {
	State         string    `json:"state"`
	At            time.Time `json:"at,omitzero"`
	By            string    `json:"by,omitempty"`
	Head          string    `json:"head,omitempty"`
	Snapshots     int       `json:"snapshots,omitempty"`
	Conflicts     int       `json:"conflicts,omitempty"`
	Bytes         int64     `json:"bytes,omitempty"`
	PurgeAfter    time.Time `json:"purgeAfter,omitzero"`
	Successor     string    `json:"successor,omitempty"`
	SuccessorGame string    `json:"successorGame,omitempty"`
}

// tombstone is an alias used by tests and internal callers that only need the
// tombstone record, not its management-state container.
type tombstone = saveVaultManageTombstone

type saveVaultManageLock struct {
	Note string    `json:"note,omitempty"`
	At   time.Time `json:"at,omitzero"`
}

type saveVaultRetentionPolicy struct {
	Enabled      bool `json:"enabled"`
	KeepVersions int  `json:"keepVersions,omitempty"`
	KeepDays     int  `json:"keepDays,omitempty"`
}

type saveVaultRetentionSettings struct {
	Default       saveVaultRetentionPolicy            `json:"default"`
	OrphanCleanup bool                               `json:"orphanCleanup"`
	Games         map[string]saveVaultRetentionPolicy `json:"games,omitempty"`
}

type saveVaultManageState struct {
	Version   int                                      `json:"version"`
	Scopes    map[string]saveVaultManageTombstone       `json:"scopes,omitempty"`
	Locks     map[string]map[string]saveVaultManageLock `json:"locks,omitempty"`
	Retention saveVaultRetentionSettings                `json:"retention"`
}

// saveVaultManageFileOps allows tests to inject filesystem failures without
// changing the production call sites. A nil hook uses the os package.
type saveVaultManageFileOps struct {
	MkdirAll  func(path string, perm os.FileMode) error
	Rename    func(oldpath, newpath string) error
	Remove    func(path string) error
	RemoveAll func(path string) error
	WriteJSON func(path string, value any) error
}

func defaultSaveVaultManageState() saveVaultManageState {
	return saveVaultManageState{
		Version: 1,
		Scopes:  make(map[string]saveVaultManageTombstone),
		Locks:   make(map[string]map[string]saveVaultManageLock),
		Retention: saveVaultRetentionSettings{
			Default: saveVaultRetentionPolicy{KeepVersions: 20},
			Games:   make(map[string]saveVaultRetentionPolicy),
		},
	}
}

func (policy saveVaultRetentionPolicy) valid() bool {
	if policy.KeepVersions < 0 || policy.KeepVersions > 1000 || policy.KeepDays < 0 || policy.KeepDays > 3650 {
		return false
	}
	return !policy.Enabled || policy.KeepVersions >= 1
}

func (settings saveVaultRetentionSettings) valid() bool {
	if !settings.Default.valid() {
		return false
	}
	for key, policy := range settings.Games {
		if !validSaveVaultGameKey(key) || !policy.valid() {
			return false
		}
	}
	return true
}

func (settings saveVaultRetentionSettings) clone() saveVaultRetentionSettings {
	next := settings
	next.Games = make(map[string]saveVaultRetentionPolicy, len(settings.Games)+1)
	for key, policy := range settings.Games {
		next.Games[key] = policy
	}
	return next
}

func (state saveVaultManageState) clone() saveVaultManageState {
	next := saveVaultManageState{
		Version:   state.Version,
		Scopes:    make(map[string]saveVaultManageTombstone, len(state.Scopes)+1),
		Locks:     make(map[string]map[string]saveVaultManageLock, len(state.Locks)+1),
		Retention: state.Retention.clone(),
	}
	for key, tombstone := range state.Scopes {
		next.Scopes[key] = tombstone
	}
	for key, locks := range state.Locks {
		copied := make(map[string]saveVaultManageLock, len(locks)+1)
		for id, lock := range locks {
			copied[id] = lock
		}
		next.Locks[key] = copied
	}
	return next
}

func (s *server) saveVaultManagePath() string {
	return filepath.Join(s.dataDir, saveVaultManageFileName)
}

// loadSaveVaultManage never creates the file. A deployment that has never used a
// C6 feature keeps no management state at all.
func (s *server) loadSaveVaultManage() error {
	state := saveVaultManageState{}
	if err := readJSONFile(s.saveVaultManagePath(), &state); err != nil {
		return err
	}
	if state.Version != 0 && state.Version != 1 {
		return fmt.Errorf("%s contains an unsupported version", saveVaultManageFileName)
	}
	if state.Scopes == nil {
		state.Scopes = make(map[string]saveVaultManageTombstone)
	}
	if state.Locks == nil {
		state.Locks = make(map[string]map[string]saveVaultManageLock)
	}
	if state.Retention.Games == nil {
		state.Retention.Games = make(map[string]saveVaultRetentionPolicy)
	}
	if state.Retention.Default.KeepVersions == 0 && state.Retention.Default.KeepDays == 0 {
		state.Retention.Default.KeepVersions = 20
	}
	if !state.Retention.valid() {
		return fmt.Errorf("%s contains invalid retention settings", saveVaultManageFileName)
	}
	state.Version = 1
	s.saveVaultManage = state
	return nil
}

func (s *server) persistSaveVaultManage(state saveVaultManageState) error {
	state.Version = 1
	return writeJSONFile(s.saveVaultManagePath(), state)
}

func saveVaultManageScopeKey(kind, gameID string) string {
	return kind + "/" + gameID
}

func parseSaveVaultManageScopeKey(key string) (string, string, bool) {
	kind, gameID, ok := strings.Cut(key, "/")
	if !ok || !validSaveVaultType(kind) || !validSaveVaultGameID(gameID) {
		return "", "", false
	}
	return kind, gameID, true
}

func parseSaveVaultStateKey(key string) (string, string, bool) {
	kind, gameID, ok := strings.Cut(key, "\x00")
	if !ok || !validSaveVaultType(kind) || !validSaveVaultGameID(gameID) {
		return "", "", false
	}
	return kind, gameID, true
}

// saveVaultTombstone reads one tombstone. The caller may hold saveVaultMu; this
// function acquires only manageMu.
func (s *server) saveVaultTombstone(kind, gameID string) (saveVaultManageTombstone, bool) {
	s.manageMu.Lock()
	defer s.manageMu.Unlock()
	tombstone, ok := s.saveVaultManage.Scopes[saveVaultManageScopeKey(kind, gameID)]
	return tombstone, ok
}

// saveVaultTombstoneApplies implements the rollback exception: a deleted scope
// that exists again with different content was recreated after a rollback, so
// its tombstone must not be executed. If the live scope still matches the
// tombstone's head and snapshot count, a delete was interrupted and clients
// must stay blocked until the administrator retries. A retired scope normally
// remains in state, so it always applies.
func saveVaultTombstoneApplies(tombstone saveVaultManageTombstone, scope saveVaultScopeState, scopeExists bool) bool {
	switch tombstone.State {
	case saveVaultTombstoneDeleted:
		if !scopeExists {
			return true
		}
		return scope.Head == tombstone.Head && len(scope.Snapshots) == tombstone.Snapshots
	case saveVaultTombstoneRetired:
		return true
	default:
		return false
	}
}

func (s *server) saveVaultScopeBlocked(kind, gameID string) (saveVaultManageTombstone, bool) {
	s.saveVaultMu.Lock()
	defer s.saveVaultMu.Unlock()
	return s.saveVaultScopeBlockedLocked(kind, gameID)
}

// saveVaultScopeBlockedLocked must be called while saveVaultMu is held.
func (s *server) saveVaultScopeBlockedLocked(kind, gameID string) (saveVaultManageTombstone, bool) {
	scope, exists := s.saveVaultState.Scopes[saveVaultScopeKey(kind, gameID)]
	tombstone, ok := s.saveVaultTombstone(kind, gameID)
	if !ok || !saveVaultTombstoneApplies(tombstone, scope, exists) {
		return saveVaultManageTombstone{}, false
	}
	return tombstone, true
}

// saveVaultLocks returns a copy of the locks of one scope. The caller may hold
// saveVaultMu; this function acquires only manageMu.
func (s *server) saveVaultLocks(kind, gameID string) map[string]saveVaultManageLock {
	s.manageMu.Lock()
	defer s.manageMu.Unlock()
	stored := s.saveVaultManage.Locks[saveVaultManageScopeKey(kind, gameID)]
	result := make(map[string]saveVaultManageLock, len(stored))
	for id, lock := range stored {
		result[id] = lock
	}
	return result
}

func applySaveVaultLock(snapshot *saveVaultScopeSnapshot, locks map[string]saveVaultManageLock) {
	snapshot.Locked = false
	snapshot.LockNote = ""
	if lock, ok := locks[snapshot.ID]; ok {
		snapshot.Locked = true
		snapshot.LockNote = lock.Note
	}
}

func saveVaultArchiveGameID(name string) (string, bool) {
	gameID, _, found := strings.Cut(name, "__")
	if !found || !validSaveVaultGameID(gameID) {
		return "", false
	}
	return gameID, true
}

type saveVaultTombstoneSuccessor struct {
	Type   string `json:"type"`
	GameID string `json:"gameId"`
}

type saveVaultTombstoneResponse struct {
	Code      string                      `json:"code"`
	Error     string                      `json:"error"`
	DeletedAt time.Time                   `json:"deletedAt,omitzero"`
	Successor *saveVaultTombstoneSuccessor `json:"successor,omitempty"`
}

// saveVaultSuccessorScope turns a C1 game key back into the type/game ID shown
// to a client. A scope-only key loses its "scope:" marker; identity keys stay
// readable as the game identifier.
func saveVaultSuccessorScope(successor string) (string, string, bool) {
	kind, gameID, ok := strings.Cut(successor, "/")
	if !ok || !validSaveVaultType(kind) || !validSaveVaultGameID(gameID) {
		return "", "", false
	}
	return kind, gameID, true
}

func writeSaveVaultTombstone(w http.ResponseWriter, tombstone saveVaultManageTombstone) {
	response := saveVaultTombstoneResponse{}
	if tombstone.State == saveVaultTombstoneRetired {
		response.Code = "scope_retired"
		response.Error = "this SaveVault scope was merged into another scope"
		if kind, gameID, ok := saveVaultSuccessorScope(tombstone.Successor); ok {
			response.Successor = &saveVaultTombstoneSuccessor{Type: kind, GameID: gameID}
		}
	} else {
		response.Code = "scope_deleted"
		response.Error = "this SaveVault scope was deleted on the server"
		response.DeletedAt = tombstone.At
	}
	writeJSON(w, http.StatusGone, response)
}

func writeSaveVaultPreconditionFailed(w http.ResponseWriter, head string) {
	writeJSON(w, http.StatusPreconditionFailed, map[string]string{
		"error": "SaveVault scope head changed",
		"head":  head,
	})
}

func (s *server) saveVaultTrashRoot() string {
	return filepath.Join(s.dataDir, saveVaultTrashDirectoryName)
}

func (s *server) saveVaultTrashScopeDirectory(kind, gameID string) string {
	return filepath.Join(s.saveVaultTrashRoot(), kind, gameID)
}

func saveVaultTrashTimestamp(at time.Time) string {
	if at.IsZero() {
		return "unknown"
	}
	return strings.ReplaceAll(at.UTC().Format(time.RFC3339), ":", "")
}

func (s *server) saveVaultTrashDirectory(kind, gameID string, at time.Time) string {
	return filepath.Join(s.saveVaultTrashScopeDirectory(kind, gameID), saveVaultTrashTimestamp(at))
}

// saveVaultPruneEmptyTrash removes now-empty scope, type, and root directories.
// It never fails an operation: a non-empty parent is simply left in place.
func (s *server) saveVaultPruneEmptyTrash(kind, gameID string) {
	_ = os.Remove(s.saveVaultTrashScopeDirectory(kind, gameID))
	_ = os.Remove(filepath.Join(s.saveVaultTrashRoot(), kind))
	_ = os.Remove(s.saveVaultTrashRoot())
}

// saveVaultTrashDirectoryForTombstone returns the existing trash directory. A
// tombstone written before timestamps were recorded falls back to the only
// timestamped child directory.
func (s *server) saveVaultTrashDirectoryForTombstone(kind, gameID string,
	tombstone saveVaultManageTombstone) (string, bool) {
	if !tombstone.At.IsZero() {
		path := s.saveVaultTrashDirectory(kind, gameID, tombstone.At)
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return path, true
		}
		return path, false
	}
	entries, err := os.ReadDir(s.saveVaultTrashScopeDirectory(kind, gameID))
	if err != nil {
		return "", false
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	if len(names) == 0 {
		return "", false
	}
	sort.Strings(names)
	path := filepath.Join(s.saveVaultTrashScopeDirectory(kind, gameID), names[len(names)-1])
	return path, true
}

func (s *server) manageMkdirAll(path string, perm os.FileMode) error {
	if s.manageFileOps.MkdirAll != nil {
		return s.manageFileOps.MkdirAll(path, perm)
	}
	return os.MkdirAll(path, perm)
}

func (s *server) manageRename(oldpath, newpath string) error {
	if s.manageFileOps.Rename != nil {
		return s.manageFileOps.Rename(oldpath, newpath)
	}
	return os.Rename(oldpath, newpath)
}

func (s *server) manageRemove(path string) error {
	var size int64
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		size = info.Size()
	}
	var err error
	if s.manageFileOps.Remove != nil {
		err = s.manageFileOps.Remove(path)
	} else {
		err = os.Remove(path)
	}
	if err == nil && size > 0 {
		s.adjustUsedBytes(-size)
	}
	return err
}

func (s *server) manageRemoveAll(path string) error {
	var err error
	if s.manageFileOps.RemoveAll != nil {
		err = s.manageFileOps.RemoveAll(path)
	} else {
		err = os.RemoveAll(path)
	}
	if err == nil {
		s.reconcileUsedBytes()
	}
	return err
}

func (s *server) manageWriteJSON(path string, value any) error {
	if s.manageFileOps.WriteJSON != nil {
		return s.manageFileOps.WriteJSON(path, value)
	}
	return writeJSONFile(path, value)
}

// saveVaultScopeArchiveFilesLocked lists every archive file whose name starts
// with gameID+"__", including archives that no snapshot references. The caller
// holds saveVaultMu.
func (s *server) saveVaultScopeArchiveFilesLocked(kind, gameID string) ([]string, int64, error) {
	dir := filepath.Join(s.saveVaultNativeDirectory(), kind)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	prefix := gameID + "__"
	names := make([]string, 0)
	var bytes int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, 0, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		names = append(names, entry.Name())
		bytes += info.Size()
	}
	sort.Strings(names)
	return names, bytes, nil
}

// rollbackSaveVaultDelete restores already moved archives and removes the
// tombstone only when every rollback step succeeded. A failed rollback leaves
// the tombstone in place so clients stay blocked and a retry can finish.
func (s *server) rollbackSaveVaultDelete(kind, gameID, key, trashDir string, moved []string) bool {
	ok := true
	for _, name := range moved {
		destination, valid := s.saveVaultPath(kind, name)
		if !valid {
			ok = false
			continue
		}
		if err := s.manageRename(filepath.Join(trashDir, name), destination); err != nil {
			slog.Warn("rollback SaveVault delete archive", "type", kind, "name", name, "error", err)
			ok = false
		}
	}
	if ok {
		hasRemainingArchives := false
		if entries, err := os.ReadDir(trashDir); err == nil {
			for _, entry := range entries {
				if !entry.IsDir() && strings.HasSuffix(strings.ToLower(entry.Name()), ".zip") {
					hasRemainingArchives = true
					break
				}
			}
		}
		if hasRemainingArchives {
			// Earlier attempts of a continued delete still have archives in
			// trashDir; keep trashDir and the tombstone intact so a retry can
			// finish without permanently destroying already-trashed archives.
			return false
		}
		if err := s.manageRemoveAll(trashDir); err != nil && !errors.Is(err, os.ErrNotExist) {
			// All moved archives are back in the live directory; a leftover
			// trash directory is harmless and must not keep clients blocked.
			slog.Warn("rollback SaveVault delete trash", "dir", trashDir, "error", err)
		} else {
			s.saveVaultPruneEmptyTrash(kind, gameID)
		}
	}
	if ok {
		s.manageMu.Lock()
		next := s.saveVaultManage.clone()
		delete(next.Scopes, key)
		if err := s.persistSaveVaultManage(next); err != nil {
			slog.Warn("rollback SaveVault tombstone", "scope", key, "error", err)
			ok = false
		} else {
			s.saveVaultManage = next
		}
		s.manageMu.Unlock()
	}
	return ok
}

func (s *server) handleDeleteSaveVaultScope(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := saveVaultScopeFromRequest(w, r)
	if !ok {
		return
	}
	expected, ok := expectedSaveVaultHead(r)
	if !ok {
		writeError(w, http.StatusPreconditionRequired, "If-Match must contain the current SaveVault head")
		return
	}
	now := s.now().UTC()
	manageKey := saveVaultManageScopeKey(kind, gameID)

	s.saveVaultMu.Lock()
	stored, exists := s.saveVaultState.Scopes[key]
	tombstone, hasTombstone := s.saveVaultTombstone(kind, gameID)
	if !exists {
		s.saveVaultMu.Unlock()
		if hasTombstone && tombstone.State == saveVaultTombstoneDeleted {
			writeJSON(w, http.StatusOK, tombstone)
			return
		}
		writeError(w, http.StatusNotFound, "SaveVault scope not found")
		return
	}
	if stored.Head != expected {
		s.saveVaultMu.Unlock()
		writeSaveVaultPreconditionFailed(w, stored.Head)
		return
	}
	if hasTombstone && tombstone.State == saveVaultTombstoneRetired {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusConflict, "SaveVault scope is merged; unretire it first")
		return
	}
	// Scopes merged into this one name it as their successor. Deleting it would
	// point their clients at a scope that no longer exists, so they must be
	// unretired (or deleted) first.
	s.manageMu.Lock()
	hasMergedMembers := false
	for _, other := range s.saveVaultManage.Scopes {
		if other.State == saveVaultTombstoneRetired && other.Successor == manageKey {
			hasMergedMembers = true
			break
		}
	}
	s.manageMu.Unlock()
	if hasMergedMembers {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusConflict, "SaveVault scope has merged members; unretire them first")
		return
	}
	files, bytes, err := s.saveVaultScopeArchiveFilesLocked(kind, gameID)
	if err != nil {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot inspect SaveVault archives")
		return
	}

	// A retry after a partial failure reuses the original deletion time so it
	// continues the same trash directory and purge deadline. A recreated scope
	// has a different head and starts a fresh deletion.
	at := now
	continuation := hasTombstone && tombstone.State == saveVaultTombstoneDeleted && !tombstone.At.IsZero() &&
		tombstone.Head == stored.Head
	if continuation {
		at = tombstone.At
	} else if hasTombstone && tombstone.State == saveVaultTombstoneDeleted {
		// A recreated scope is being deleted again. The old tombstone is
		// replaced, so its trash would otherwise never be purged.
		if oldDir, found := s.saveVaultTrashDirectoryForTombstone(kind, gameID, tombstone); found {
			if err := s.manageRemoveAll(oldDir); err != nil {
				slog.Warn("remove stale SaveVault trash", "scope", manageKey, "error", err)
			}
			s.saveVaultPruneEmptyTrash(kind, gameID)
		}
	}
	nextTombstone := saveVaultManageTombstone{
		State:      saveVaultTombstoneDeleted,
		At:         at,
		By:         "admin",
		Head:       stored.Head,
		Snapshots:  len(stored.Snapshots),
		Bytes:      bytes,
		PurgeAfter: at.Add(saveVaultTombstoneRetention),
	}
	for _, snapshot := range stored.Snapshots {
		if snapshot.Conflict {
			nextTombstone.Conflicts++
		}
	}

	s.manageMu.Lock()
	nextManage := s.saveVaultManage.clone()
	nextManage.Scopes[manageKey] = nextTombstone
	if err := s.persistSaveVaultManage(nextManage); err != nil {
		s.manageMu.Unlock()
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist SaveVault tombstone")
		return
	}
	s.saveVaultManage = nextManage
	s.manageMu.Unlock()

	trashDir := s.saveVaultTrashDirectory(kind, gameID, at)
	rollback := func(moved []string) {
		if !s.rollbackSaveVaultDelete(kind, gameID, manageKey, trashDir, moved) {
			slog.Warn("SaveVault delete rollback incomplete; tombstone retained", "scope", manageKey)
		}
	}
	if err := s.manageMkdirAll(trashDir, 0o700); err != nil {
		rollback(nil)
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot create SaveVault trash directory")
		return
	}
	if err := s.manageWriteJSON(filepath.Join(trashDir, "scope.json"), stored); err != nil {
		rollback(nil)
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot write SaveVault trash state")
		return
	}
	catalogEntries := make(map[string]saveVaultFileMetadata)
	if continuation {
		var existingCatalog map[string]saveVaultFileMetadata
		if err := readJSONFile(filepath.Join(trashDir, "catalog.json"), &existingCatalog); err == nil {
			for name, metadata := range existingCatalog {
				catalogEntries[name] = metadata
			}
		}
	}
	for _, name := range files {
		if metadata, ok := s.saveVaultCatalog.Files[saveVaultCatalogKey(kind, name)]; ok {
			catalogEntries[name] = metadata
		}
	}
	if len(catalogEntries) > 0 {
		if err := s.manageWriteJSON(filepath.Join(trashDir, "catalog.json"), catalogEntries); err != nil {
			rollback(nil)
			s.saveVaultMu.Unlock()
			writeError(w, http.StatusInternalServerError, "cannot write SaveVault trash catalog")
			return
		}
	}

	moved := make([]string, 0, len(files))
	for _, name := range files {
		source, valid := s.saveVaultPath(kind, name)
		if !valid {
			continue
		}
		if err := s.manageRename(source, filepath.Join(trashDir, name)); err != nil {
			rollback(moved)
			s.saveVaultMu.Unlock()
			writeError(w, http.StatusInternalServerError, "cannot move SaveVault archives to trash")
			return
		}
		moved = append(moved, name)
	}

	nextState := s.saveVaultState.clone()
	delete(nextState.Scopes, key)
	if err := s.persistSaveVaultState(nextState); err != nil {
		rollback(moved)
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist SaveVault scope state")
		return
	}
	s.saveVaultState = nextState

	// Catalog cleanup is advisory: the files are already safely in trash.
	nextCatalog := s.saveVaultCatalog.clone()
	catalogChanged := false
	for _, name := range moved {
		catalogKey := saveVaultCatalogKey(kind, name)
		if _, ok := nextCatalog.Files[catalogKey]; ok {
			delete(nextCatalog.Files, catalogKey)
			catalogChanged = true
		}
	}
	if catalogChanged {
		if err := s.persistSaveVaultCatalog(nextCatalog); err != nil {
			slog.Warn("persist SaveVault trash catalog cleanup", "type", kind, "error", err)
		} else {
			s.saveVaultCatalog = nextCatalog
		}
	}
	s.saveVaultMu.Unlock()

	s.recordSaveVaultManageEvent(eventDeleteScope,
		fmt.Sprintf("%s/%s %d versions, %d bytes", kind, gameID, nextTombstone.Snapshots, nextTombstone.Bytes),
		kind, gameID, "")
	writeJSON(w, http.StatusOK, nextTombstone)
}

func (s *server) handleRestoreSaveVaultScope(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := saveVaultScopeFromRequest(w, r)
	if !ok {
		return
	}
	now := s.now().UTC()
	manageKey := saveVaultManageScopeKey(kind, gameID)

	s.saveVaultMu.Lock()
	tombstone, hasTombstone := s.saveVaultTombstone(kind, gameID)
	if !hasTombstone {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusNotFound, "SaveVault tombstone not found")
		return
	}
	if tombstone.State != saveVaultTombstoneDeleted {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusConflict, "only a deleted SaveVault scope can be restored")
		return
	}
	if _, exists := s.saveVaultState.Scopes[key]; exists {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusConflict, "SaveVault scope already exists")
		return
	}
	if !tombstone.PurgeAfter.IsZero() && !now.Before(tombstone.PurgeAfter) {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusConflict, "SaveVault restore window has expired")
		return
	}
	trashDir, found := s.saveVaultTrashDirectoryForTombstone(kind, gameID, tombstone)
	if !found {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusNotFound, "SaveVault trash directory not found")
		return
	}
	restored := saveVaultScopeState{}
	if err := readJSONFile(filepath.Join(trashDir, "scope.json"), &restored); err != nil {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot read SaveVault trash state")
		return
	}
	if restored.Snapshots == nil {
		restored.Snapshots = make(map[string]saveVaultScopeSnapshot)
	}

	names, err := saveVaultTrashFiles(trashDir)
	if err != nil {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot inspect SaveVault trash directory")
		return
	}
	destinationDir := filepath.Join(s.saveVaultNativeDirectory(), kind)
	if err := s.manageMkdirAll(destinationDir, 0o700); err != nil {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot create SaveVault storage")
		return
	}
	moved := make([]string, 0, len(names))
	moveBack := func() {
		for _, name := range moved {
			destination := filepath.Join(destinationDir, name)
			if err := s.manageRename(destination, filepath.Join(trashDir, name)); err != nil {
				slog.Warn("rollback SaveVault restore archive", "type", kind, "name", name, "error", err)
			}
		}
	}
	for _, name := range names {
		destination := filepath.Join(destinationDir, name)
		if _, err := os.Lstat(destination); err == nil {
			moveBack()
			s.saveVaultMu.Unlock()
			writeError(w, http.StatusConflict, "SaveVault archive already exists in the live directory")
			return
		}
		if err := s.manageRename(filepath.Join(trashDir, name), destination); err != nil {
			moveBack()
			s.saveVaultMu.Unlock()
			writeError(w, http.StatusInternalServerError, "cannot restore SaveVault archive")
			return
		}
		moved = append(moved, name)
	}

	nextState := s.saveVaultState.clone()
	nextState.Scopes[key] = restored
	if err := s.persistSaveVaultState(nextState); err != nil {
		moveBack()
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist SaveVault scope state")
		return
	}
	s.saveVaultState = nextState

	catalogEntries := make(map[string]saveVaultFileMetadata)
	_ = readJSONFile(filepath.Join(trashDir, "catalog.json"), &catalogEntries)
	for _, snapshot := range restored.Snapshots {
		if _, ok := catalogEntries[snapshot.Archive]; !ok {
			catalogEntries[snapshot.Archive] = saveVaultFileMetadata{
				SHA256:     snapshot.ContentDigest,
				Device:     snapshot.Device,
				DeviceName: snapshot.DeviceName,
				UploadedAt: snapshot.ReceivedAt,
			}
		}
	}
	if len(catalogEntries) > 0 {
		nextCatalog := s.saveVaultCatalog.clone()
		for name, metadata := range catalogEntries {
			nextCatalog.Files[saveVaultCatalogKey(kind, name)] = metadata
		}
		if err := s.persistSaveVaultCatalog(nextCatalog); err != nil {
			slog.Warn("persist SaveVault restored catalog", "type", kind, "error", err)
		} else {
			s.saveVaultCatalog = nextCatalog
		}
	}

	s.manageMu.Lock()
	nextManage := s.saveVaultManage.clone()
	delete(nextManage.Scopes, manageKey)
	if err := s.persistSaveVaultManage(nextManage); err != nil {
		s.manageMu.Unlock()
		oldState := s.saveVaultState.clone()
		delete(oldState.Scopes, key)
		if persistErr := s.persistSaveVaultState(oldState); persistErr == nil {
			s.saveVaultState = oldState
		} else {
			slog.Warn("rollback SaveVault restore state", "error", persistErr)
		}
		moveBack()
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist SaveVault tombstone removal")
		return
	}
	s.saveVaultManage = nextManage
	s.manageMu.Unlock()

	audit := s.saveVaultScopeAuditLocked(kind, gameID, restored)
	if err := s.manageRemoveAll(trashDir); err == nil || errors.Is(err, os.ErrNotExist) {
		s.saveVaultPruneEmptyTrash(kind, gameID)
	}
	s.saveVaultMu.Unlock()

	s.recordSaveVaultManageEvent(eventRestoreScope,
		fmt.Sprintf("%s/%s %d versions", kind, gameID, len(restored.Snapshots)), kind, gameID, "")
	writeJSON(w, http.StatusOK, audit)
}

func saveVaultTrashFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		switch entry.Name() {
		case "scope.json", "catalog.json":
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

func (s *server) handleRetireSaveVaultScope(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := saveVaultScopeFromRequest(w, r)
	if !ok {
		return
	}
	var request struct {
		Into string `json:"into"`
	}
	if !decodeAdminJSON(w, r, &request) {
		return
	}
	targetID := strings.TrimSpace(request.Into)
	if !validSaveVaultGameID(targetID) {
		writeError(w, http.StatusBadRequest, "into must be a valid SaveVault game id")
		return
	}
	if targetID == gameID {
		writeError(w, http.StatusBadRequest, "a scope cannot be merged into itself")
		return
	}
	now := s.now().UTC()
	manageKey := saveVaultManageScopeKey(kind, gameID)

	s.saveVaultMu.Lock()
	source, exists := s.saveVaultState.Scopes[key]
	if !exists {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusNotFound, "SaveVault scope not found")
		return
	}
	if _, hasTombstone := s.saveVaultTombstone(kind, gameID); hasTombstone {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusConflict, "SaveVault scope already has a tombstone")
		return
	}
	target, targetExists := s.saveVaultState.Scopes[saveVaultScopeKey(kind, targetID)]
	_, targetHasTombstone := s.saveVaultTombstone(kind, targetID)
	if !targetExists {
		crossType := false
		for stateKey := range s.saveVaultState.Scopes {
			otherKind, otherGameID, ok := parseSaveVaultStateKey(stateKey)
			if ok && otherKind != kind && otherGameID == targetID {
				crossType = true
				break
			}
		}
		s.saveVaultMu.Unlock()
		switch {
		case targetHasTombstone:
			writeError(w, http.StatusBadRequest, "target SaveVault scope was deleted")
		case crossType:
			writeError(w, http.StatusBadRequest, "a scope can only be merged into the same type")
		default:
			writeError(w, http.StatusNotFound, "target SaveVault scope not found")
		}
		return
	}
	if targetHasTombstone {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusConflict, "target SaveVault scope has a tombstone")
		return
	}
	sourceGameKey, _ := saveVaultGameKey(kind, gameID, source)
	s.manageMu.Lock()
	chained := false
	sourceScopeKey := saveVaultManageScopeKey(kind, gameID)
	for _, other := range s.saveVaultManage.Scopes {
		if other.State == saveVaultTombstoneRetired &&
			(other.SuccessorGame == sourceGameKey || other.Successor == sourceScopeKey) {
			chained = true
			break
		}
	}
	s.manageMu.Unlock()
	if chained {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusConflict, "SaveVault scope already has merged members")
		return
	}
	targetGameKey, _ := saveVaultGameKey(kind, targetID, target)
	nextTombstone := saveVaultManageTombstone{
		State:         saveVaultTombstoneRetired,
		At:            now,
		By:            "admin",
		Successor:     saveVaultManageScopeKey(kind, targetID),
		SuccessorGame: targetGameKey,
	}

	s.manageMu.Lock()
	nextManage := s.saveVaultManage.clone()
	nextManage.Scopes[manageKey] = nextTombstone
	if err := s.persistSaveVaultManage(nextManage); err != nil {
		s.manageMu.Unlock()
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist SaveVault tombstone")
		return
	}
	s.saveVaultManage = nextManage
	s.manageMu.Unlock()

	nextSource := source.clone()
	nextSource.GameLink = targetGameKey
	nextState := s.saveVaultState.clone()
	nextState.Scopes[key] = nextSource
	if err := s.persistSaveVaultState(nextState); err != nil {
		s.manageMu.Lock()
		rollback := s.saveVaultManage.clone()
		delete(rollback.Scopes, manageKey)
		if rollbackErr := s.persistSaveVaultManage(rollback); rollbackErr != nil {
			slog.Warn("rollback SaveVault retire tombstone", "scope", manageKey, "error", rollbackErr)
		} else {
			s.saveVaultManage = rollback
		}
		s.manageMu.Unlock()
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist SaveVault scope state")
		return
	}
	s.saveVaultState = nextState
	s.saveVaultMu.Unlock()

	s.recordSaveVaultManageEvent(eventRetireScope,
		fmt.Sprintf("%s/%s into %s", kind, gameID, targetGameKey), kind, gameID, "")
	writeJSON(w, http.StatusOK, nextTombstone)
}

// saveVaultTombstoneSuccessorGame returns the C1 game key recorded at merge
// time, falling back to the target scope's current key for older tombstones.
// The caller holds saveVaultMu.
func (s *server) saveVaultTombstoneSuccessorGame(kind string, tombstone saveVaultManageTombstone) string {
	if tombstone.SuccessorGame != "" {
		return tombstone.SuccessorGame
	}
	successorKind, targetID, ok := parseSaveVaultManageScopeKey(tombstone.Successor)
	if !ok || successorKind != kind {
		return ""
	}
	target, ok := s.saveVaultState.Scopes[saveVaultScopeKey(kind, targetID)]
	if !ok {
		return ""
	}
	gameKey, _ := saveVaultGameKey(kind, targetID, target)
	return gameKey
}

func (s *server) handleUnretireSaveVaultScope(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := saveVaultScopeFromRequest(w, r)
	if !ok {
		return
	}

	manageKey := saveVaultManageScopeKey(kind, gameID)
	s.saveVaultMu.Lock()
	source, exists := s.saveVaultState.Scopes[key]
	if !exists {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusNotFound, "SaveVault scope not found")
		return
	}
	tombstone, hasTombstone := s.saveVaultTombstone(kind, gameID)
	if !hasTombstone || tombstone.State != saveVaultTombstoneRetired {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusNotFound, "SaveVault retired tombstone not found")
		return
	}

	nextSource := source.clone()
	if successorGame := s.saveVaultTombstoneSuccessorGame(kind, tombstone); successorGame != "" &&
		nextSource.GameLink == successorGame {
		nextSource.GameLink = ""
	}
	oldState := s.saveVaultState
	nextState := s.saveVaultState.clone()
	nextState.Scopes[key] = nextSource
	if err := s.persistSaveVaultState(nextState); err != nil {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist SaveVault scope state")
		return
	}
	s.saveVaultState = nextState

	s.manageMu.Lock()
	nextManage := s.saveVaultManage.clone()
	delete(nextManage.Scopes, manageKey)
	if err := s.persistSaveVaultManage(nextManage); err != nil {
		s.manageMu.Unlock()
		if rollbackErr := s.persistSaveVaultState(oldState); rollbackErr == nil {
			s.saveVaultState = oldState
		} else {
			slog.Warn("rollback SaveVault unretire state", "error", rollbackErr)
		}
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist SaveVault tombstone removal")
		return
	}
	s.saveVaultManage = nextManage
	s.manageMu.Unlock()

	audit := s.saveVaultScopeAuditLocked(kind, gameID, nextSource)
	s.saveVaultMu.Unlock()

	s.recordSaveVaultManageEvent(eventUnretireScope, fmt.Sprintf("%s/%s", kind, gameID), kind, gameID, "")
	writeJSON(w, http.StatusOK, audit)
}

type saveVaultTombstoneView struct {
	Type          string    `json:"type"`
	GameID        string    `json:"gameId"`
	State         string    `json:"state"`
	At            time.Time `json:"at,omitzero"`
	By            string    `json:"by,omitempty"`
	Head          string    `json:"head,omitempty"`
	Snapshots     int       `json:"snapshots,omitempty"`
	Conflicts     int       `json:"conflicts,omitempty"`
	Bytes         int64     `json:"bytes,omitempty"`
	PurgeAfter    time.Time `json:"purgeAfter,omitzero"`
	Successor     string    `json:"successor,omitempty"`
	SuccessorGame string    `json:"successorGame,omitempty"`
	Restorable    bool      `json:"restorable"`
	ConflictState string    `json:"conflictState,omitempty"`
}

func (s *server) handleListSaveVaultTombstones(w http.ResponseWriter, _ *http.Request) {
	now := s.now().UTC()
	s.saveVaultMu.Lock()
	s.manageMu.Lock()
	views := make([]saveVaultTombstoneView, 0, len(s.saveVaultManage.Scopes))
	for key, tombstone := range s.saveVaultManage.Scopes {
		kind, gameID, ok := parseSaveVaultManageScopeKey(key)
		if !ok {
			continue
		}
		view := saveVaultTombstoneView{
			Type:          kind,
			GameID:        gameID,
			State:         tombstone.State,
			At:            tombstone.At,
			By:            tombstone.By,
			Head:          tombstone.Head,
			Snapshots:     tombstone.Snapshots,
			Conflicts:     tombstone.Conflicts,
			Bytes:         tombstone.Bytes,
			PurgeAfter:    tombstone.PurgeAfter,
			Successor:     tombstone.Successor,
			SuccessorGame: tombstone.SuccessorGame,
		}
		_, exists := s.saveVaultState.Scopes[saveVaultScopeKey(kind, gameID)]
		if tombstone.State == saveVaultTombstoneDeleted && exists {
			view.ConflictState = "recreated"
		}
		if tombstone.State == saveVaultTombstoneDeleted {
			if _, found := s.saveVaultTrashDirectoryForTombstone(kind, gameID, tombstone); found {
				view.Restorable = !exists && (tombstone.PurgeAfter.IsZero() || now.Before(tombstone.PurgeAfter))
			}
		}
		views = append(views, view)
	}
	s.manageMu.Unlock()
	s.saveVaultMu.Unlock()
	sort.Slice(views, func(i, j int) bool {
		if views[i].Type != views[j].Type {
			return views[i].Type < views[j].Type
		}
		return views[i].GameID < views[j].GameID
	})
	writeJSON(w, http.StatusOK, map[string]any{"tombstones": views})
}

func (s *server) handleClearSaveVaultTombstone(w http.ResponseWriter, r *http.Request) {
	kind, gameID, _, ok := saveVaultScopeFromRequest(w, r)
	if !ok {
		return
	}
	manageKey := saveVaultManageScopeKey(kind, gameID)
	s.saveVaultMu.Lock()
	tombstone, hasTombstone := s.saveVaultTombstone(kind, gameID)
	if !hasTombstone {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusNotFound, "SaveVault tombstone not found")
		return
	}
	if tombstone.State == saveVaultTombstoneRetired {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusBadRequest, "a retired scope must be unretired")
		return
	}
	// Persist the tombstone removal before deleting the trash directory so a
	// persistence failure never destroys the recoverable archives irreversibly.
	s.manageMu.Lock()
	nextManage := s.saveVaultManage.clone()
	delete(nextManage.Scopes, manageKey)
	if err := s.persistSaveVaultManage(nextManage); err != nil {
		s.manageMu.Unlock()
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist SaveVault tombstone removal")
		return
	}
	s.saveVaultManage = nextManage
	s.manageMu.Unlock()
	if trashDir, found := s.saveVaultTrashDirectoryForTombstone(kind, gameID, tombstone); found {
		if err := s.manageRemoveAll(trashDir); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("remove SaveVault trash directory after clearing tombstone", "scope", manageKey, "error", err)
		} else {
			s.saveVaultPruneEmptyTrash(kind, gameID)
		}
	}
	s.saveVaultMu.Unlock()

	s.recordSaveVaultManageEvent(eventTombstoneCleared, fmt.Sprintf("%s/%s", kind, gameID), kind, gameID, "")
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) recordSaveVaultManageEvent(kind, detail, scopeKind, gameID, snapshot string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordEventLocked(event{
		Kind:     kind,
		Scope:    saveVaultEventScope(scopeKind, gameID),
		Snapshot: snapshot,
		Detail:   detail,
	})
}

func validSaveVaultLockNote(note string) bool {
	if !utf8.ValidString(note) || utf8.RuneCountInString(note) > saveVaultMaxLockNoteRunes {
		return false
	}
	for _, character := range note {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func (s *server) handlePutSaveVaultSnapshotLock(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := saveVaultScopeFromRequest(w, r)
	if !ok {
		return
	}
	snapshotID := r.PathValue("snapshot")
	if !validSnapshotID(snapshotID) {
		writeError(w, http.StatusBadRequest, "invalid SaveVault snapshot id")
		return
	}
	var request struct {
		Note string `json:"note"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if !decodeAdminJSON(w, r, &request) {
			return
		}
	}
	note := strings.TrimSpace(request.Note)
	if !validSaveVaultLockNote(note) {
		writeError(w, http.StatusBadRequest, "lock note must be at most 120 characters without control characters")
		return
	}
	manageKey := saveVaultManageScopeKey(kind, gameID)

	s.saveVaultMu.Lock()
	stored, exists := s.saveVaultState.Scopes[key]
	if !exists {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusNotFound, "SaveVault scope not found")
		return
	}
	if _, exists := stored.Snapshots[snapshotID]; !exists {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusNotFound, "SaveVault snapshot not found")
		return
	}
	lock := saveVaultManageLock{Note: note, At: s.now().UTC()}
	s.manageMu.Lock()
	nextManage := s.saveVaultManage.clone()
	if nextManage.Locks[manageKey] == nil {
		nextManage.Locks[manageKey] = make(map[string]saveVaultManageLock)
	}
	nextManage.Locks[manageKey][snapshotID] = lock
	if err := s.persistSaveVaultManage(nextManage); err != nil {
		s.manageMu.Unlock()
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist SaveVault lock")
		return
	}
	s.saveVaultManage = nextManage
	s.manageMu.Unlock()
	s.saveVaultMu.Unlock()

	s.recordSaveVaultManageEvent(eventSnapshotLock, note, kind, gameID, snapshotID)
	writeJSON(w, http.StatusOK, map[string]any{"locked": true, "note": note})
}

func (s *server) handleDeleteSaveVaultSnapshotLock(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := saveVaultScopeFromRequest(w, r)
	if !ok {
		return
	}
	snapshotID := r.PathValue("snapshot")
	if !validSnapshotID(snapshotID) {
		writeError(w, http.StatusBadRequest, "invalid SaveVault snapshot id")
		return
	}
	manageKey := saveVaultManageScopeKey(kind, gameID)

	s.saveVaultMu.Lock()
	stored, exists := s.saveVaultState.Scopes[key]
	if !exists {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusNotFound, "SaveVault scope not found")
		return
	}
	if _, exists := stored.Snapshots[snapshotID]; !exists {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusNotFound, "SaveVault snapshot not found")
		return
	}
	s.manageMu.Lock()
	nextManage := s.saveVaultManage.clone()
	removed := false
	if locks := nextManage.Locks[manageKey]; locks != nil {
		if _, exists := locks[snapshotID]; exists {
			delete(locks, snapshotID)
			removed = true
		}
		if len(locks) == 0 {
			delete(nextManage.Locks, manageKey)
		}
	}
	if removed {
		if err := s.persistSaveVaultManage(nextManage); err != nil {
			s.manageMu.Unlock()
			s.saveVaultMu.Unlock()
			writeError(w, http.StatusInternalServerError, "cannot persist SaveVault lock removal")
			return
		}
		s.saveVaultManage = nextManage
	}
	s.manageMu.Unlock()
	s.saveVaultMu.Unlock()

	if removed {
		s.recordSaveVaultManageEvent(eventSnapshotUnlock, "", kind, gameID, snapshotID)
	}
	w.WriteHeader(http.StatusNoContent)
}

type saveVaultExportManifest struct {
	Type       string                  `json:"type"`
	GameID     string                  `json:"gameId"`
	Head       string                  `json:"head,omitempty"`
	ExportedAt time.Time               `json:"exportedAt"`
	Versions   []saveVaultExportVersion `json:"versions"`
}

type saveVaultExportVersion struct {
	ID            string    `json:"id"`
	Archive       string    `json:"archive"`
	Parent        string    `json:"parent,omitempty"`
	CreatedAt     time.Time `json:"createdAt,omitzero"`
	Device        string    `json:"device,omitempty"`
	DeviceName    string    `json:"deviceName,omitempty"`
	ReceivedAt    time.Time `json:"receivedAt,omitzero"`
	ContentDigest string    `json:"contentDigest"`
	Conflict      bool      `json:"conflict,omitempty"`
	ConflictOf    string    `json:"conflictOf,omitempty"`
	Locked        bool      `json:"locked,omitempty"`
	LockNote      string    `json:"lockNote,omitempty"`
	Missing       bool      `json:"missing,omitempty"`
}

func (s *server) handleExportSaveVaultScope(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := saveVaultScopeFromRequest(w, r)
	if !ok {
		return
	}
	s.saveVaultMu.Lock()
	stored, exists := s.saveVaultState.Scopes[key]
	if !exists {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusNotFound, "SaveVault scope not found")
		return
	}
	snapshots := make([]saveVaultScopeSnapshot, 0, len(stored.Snapshots))
	for _, snapshot := range stored.Snapshots {
		snapshots = append(snapshots, snapshot)
	}
	head := stored.Head
	locks := s.saveVaultLocks(kind, gameID)
	s.saveVaultMu.Unlock()

	sort.Slice(snapshots, func(i, j int) bool {
		if !snapshots[i].ReceivedAt.Equal(snapshots[j].ReceivedAt) {
			return snapshots[i].ReceivedAt.Before(snapshots[j].ReceivedAt)
		}
		return snapshots[i].ID < snapshots[j].ID
	})

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", kind+"-"+gameID+".zip"))
	w.Header().Set("Cache-Control", "no-store")
	writer := zip.NewWriter(w)
	written := make(map[string]bool)
	manifest := saveVaultExportManifest{
		Type:       kind,
		GameID:     gameID,
		Head:       head,
		ExportedAt: time.Now().UTC(),
		Versions:   make([]saveVaultExportVersion, 0, len(snapshots)),
	}
	for _, snapshot := range snapshots {
		entry := saveVaultExportVersion{
			ID:            snapshot.ID,
			Archive:       snapshot.Archive,
			Parent:        snapshot.Parent,
			CreatedAt:     snapshot.CreatedAt,
			Device:        snapshot.Device,
			DeviceName:    snapshot.DeviceName,
			ReceivedAt:    snapshot.ReceivedAt,
			ContentDigest: snapshot.ContentDigest,
			Conflict:      snapshot.Conflict,
			ConflictOf:    snapshot.ConflictOf,
		}
		if lock, ok := locks[snapshot.ID]; ok {
			entry.Locked = true
			entry.LockNote = lock.Note
		}
		if !written[snapshot.Archive] {
			path, valid := s.saveVaultPath(kind, snapshot.Archive)
			var file *os.File
			var err error
			if valid {
				file, err = os.Open(path)
			} else {
				err = os.ErrInvalid
			}
			if err != nil {
				entry.Missing = true
			} else {
				info, _ := file.Stat()
				header := &zip.FileHeader{Name: "versions/" + snapshot.Archive, Method: zip.Store}
				if info != nil {
					header.Modified = info.ModTime()
				}
				part, createErr := writer.CreateHeader(header)
				if createErr != nil {
					_ = file.Close()
					slog.Warn("write SaveVault export archive", "type", kind, "name", snapshot.Archive, "error", createErr)
					return
				}
				if _, copyErr := io.Copy(part, file); copyErr != nil {
					_ = file.Close()
					slog.Warn("copy SaveVault export archive", "type", kind, "name", snapshot.Archive, "error", copyErr)
					return
				}
				_ = file.Close()
				written[snapshot.Archive] = true
			}
		}
		manifest.Versions = append(manifest.Versions, entry)
	}
	part, err := writer.Create("manifest.json")
	if err != nil {
		slog.Warn("write SaveVault export manifest", "error", err)
		return
	}
	if err := json.NewEncoder(part).Encode(manifest); err != nil {
		slog.Warn("encode SaveVault export manifest", "error", err)
		return
	}
	if err := writer.Close(); err != nil {
		slog.Warn("close SaveVault export", "error", err)
	}
}

func (s *server) saveVaultRetentionSettings() saveVaultRetentionSettings {
	s.manageMu.Lock()
	defer s.manageMu.Unlock()
	return s.saveVaultManage.Retention.clone()
}

// saveVaultRetentionResponse exposes the settings both directly and under a
// "retention" key. The direct form is convenient for the endpoint, while the
// nested form matches the fragment stored in savevault-manage.json.
type saveVaultRetentionResponse struct {
	saveVaultRetentionSettings
	Retention saveVaultRetentionSettings `json:"retention"`
}

func writeSaveVaultRetention(w http.ResponseWriter, status int, settings saveVaultRetentionSettings) {
	writeJSON(w, status, saveVaultRetentionResponse{
		saveVaultRetentionSettings: settings,
		Retention:                 settings,
	})
}

func (s *server) handleGetSaveVaultRetention(w http.ResponseWriter, _ *http.Request) {
	writeSaveVaultRetention(w, http.StatusOK, s.saveVaultRetentionSettings())
}

func decodeSaveVaultRetentionRequest(w http.ResponseWriter, r *http.Request) (saveVaultRetentionSettings, bool) {
	var raw json.RawMessage
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMetadataBytes))
	if err := decoder.Decode(&raw); err != nil {
		writeError(w, http.StatusBadRequest, "invalid SaveVault retention settings")
		return saveVaultRetentionSettings{}, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid SaveVault retention settings")
		return saveVaultRetentionSettings{}, false
	}
	var wrapper struct {
		Retention *saveVaultRetentionSettings `json:"retention"`
	}
	if err := json.Unmarshal(raw, &wrapper); err == nil && wrapper.Retention != nil {
		return *wrapper.Retention, true
	}
	var settings saveVaultRetentionSettings
	if err := json.Unmarshal(raw, &settings); err != nil {
		writeError(w, http.StatusBadRequest, "invalid SaveVault retention settings")
		return saveVaultRetentionSettings{}, false
	}
	return settings, true
}

func (s *server) handlePutSaveVaultRetention(w http.ResponseWriter, r *http.Request) {
	settings, ok := decodeSaveVaultRetentionRequest(w, r)
	if !ok {
		return
	}
	if settings.Games == nil {
		settings.Games = make(map[string]saveVaultRetentionPolicy)
	}
	if settings.Default.KeepVersions == 0 && settings.Default.KeepDays == 0 {
		settings.Default.KeepVersions = 20
	}
	if !settings.valid() {
		writeError(w, http.StatusBadRequest, "invalid SaveVault retention settings")
		return
	}

	s.manageMu.Lock()
	if reflect.DeepEqual(settings, s.saveVaultManage.Retention) {
		current := s.saveVaultManage.Retention.clone()
		s.manageMu.Unlock()
		writeSaveVaultRetention(w, http.StatusOK, current)
		return
	}
	nextManage := s.saveVaultManage.clone()
	nextManage.Retention = settings.clone()
	if err := s.persistSaveVaultManage(nextManage); err != nil {
		s.manageMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist SaveVault retention settings")
		return
	}
	s.saveVaultManage = nextManage
	s.manageMu.Unlock()

	detail := "savevault_retention="
	if encoded, err := json.Marshal(settings); err == nil {
		detail += string(encoded)
	}
	s.mu.Lock()
	s.recordEventLocked(event{Kind: eventSettings, Detail: detail})
	s.mu.Unlock()
	writeSaveVaultRetention(w, http.StatusOK, settings)
}

type saveVaultRetentionPreviewScope struct {
	Type    string `json:"type"`
	GameID  string `json:"gameId"`
	GameKey string `json:"gameKey"`
	Enabled bool   `json:"enabled"`
	Removed int    `json:"removed"`
	Bytes   int64  `json:"bytes"`
}

type saveVaultOrphanArchive struct {
	Type       string    `json:"type"`
	Name       string    `json:"name"`
	GameID     string    `json:"gameId,omitempty"`
	ModifiedAt time.Time `json:"modifiedAt"`
	Size       int64     `json:"size"`
}

type saveVaultRetentionPreview struct {
	Scopes      []saveVaultRetentionPreviewScope `json:"scopes"`
	Orphans     int                             `json:"orphans"`
	OrphanBytes int64                           `json:"orphanBytes"`
	OrphanFiles []saveVaultOrphanArchive         `json:"orphanFiles"`
}

func (s *server) saveVaultTypeDirectories() []string {
	types := []string{"pc", "switch"}
	root := s.saveVaultNativeDirectory()
	if entries, err := os.ReadDir(root); err == nil {
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), "emulator-") && validSaveVaultType(entry.Name()) {
				types = append(types, entry.Name())
			}
		}
	}
	sort.Strings(types)
	return types
}

// saveVaultOrphanArchivesLocked lists regular files that no version of the same
// type references. Dot files are in-progress uploads and are never orphans.
// The caller holds saveVaultMu.
func (s *server) saveVaultOrphanArchivesLocked(state saveVaultNativeState) []saveVaultOrphanArchive {
	referenced := make(map[string]struct{})
	for key, stored := range state.Scopes {
		kind, _, ok := parseSaveVaultStateKey(key)
		if !ok {
			continue
		}
		for _, snapshot := range stored.Snapshots {
			referenced[saveVaultCatalogKey(kind, snapshot.Archive)] = struct{}{}
		}
	}
	orphans := make([]saveVaultOrphanArchive, 0)
	for _, kind := range s.saveVaultTypeDirectories() {
		dir := filepath.Join(s.saveVaultNativeDirectory(), kind)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			if _, ok := referenced[saveVaultCatalogKey(kind, entry.Name())]; ok {
				continue
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			orphan := saveVaultOrphanArchive{
				Type:       kind,
				Name:       entry.Name(),
				ModifiedAt: info.ModTime().UTC(),
				Size:       info.Size(),
			}
			if gameID, ok := saveVaultArchiveGameID(entry.Name()); ok {
				orphan.GameID = gameID
			}
			orphans = append(orphans, orphan)
		}
	}
	sort.Slice(orphans, func(i, j int) bool {
		if orphans[i].Type != orphans[j].Type {
			return orphans[i].Type < orphans[j].Type
		}
		return orphans[i].Name < orphans[j].Name
	})
	return orphans
}

func (s *server) handlePreviewSaveVaultRetention(w http.ResponseWriter, _ *http.Request) {
	now := s.now().UTC()
	retention := s.saveVaultRetentionSettings()

	s.saveVaultMu.Lock()
	nextState := s.saveVaultState.clone()
	type removal struct {
		kind     string
		gameID   string
		archives []string
	}
	removals := make([]removal, 0)
	preview := saveVaultRetentionPreview{
		Scopes:      make([]saveVaultRetentionPreviewScope, 0, len(s.saveVaultState.Scopes)),
		OrphanFiles: []saveVaultOrphanArchive{},
	}
	for key, stored := range s.saveVaultState.Scopes {
		kind, gameID, ok := parseSaveVaultStateKey(key)
		if !ok {
			continue
		}
		gameKey, _ := saveVaultGameKey(kind, gameID, stored)
		policy, ok := retention.Games[gameKey]
		if !ok {
			policy = retention.Default
		}
		entry := saveVaultRetentionPreviewScope{
			Type:    kind,
			GameID:  gameID,
			GameKey: gameKey,
			Enabled: policy.Enabled,
		}
		if policy.KeepVersions > 0 || policy.KeepDays > 0 {
			nextScope, removedIDs := saveVaultPruneScopeWithRetention(stored, policy.KeepVersions,
				policy.KeepDays, now, s.saveVaultLockedSet(kind, gameID))
			if len(removedIDs) > 0 {
				nextState.Scopes[key] = nextScope
				entry.Removed = len(removedIDs)
				candidate := removal{kind: kind, gameID: gameID}
				for _, id := range removedIDs {
					if snapshot, ok := stored.Snapshots[id]; ok && snapshot.Archive != "" {
						candidate.archives = append(candidate.archives, snapshot.Archive)
					}
				}
				removals = append(removals, candidate)
			}
		}
		preview.Scopes = append(preview.Scopes, entry)
	}

	// Fill per-scope bytes using the hypothetical post-removal state.
	for index := range preview.Scopes {
		entry := &preview.Scopes[index]
		if entry.Removed == 0 {
			continue
		}
		seen := make(map[string]struct{})
		for _, candidate := range removals {
			if candidate.kind != entry.Type || candidate.gameID != entry.GameID {
				continue
			}
			for _, archive := range candidate.archives {
				if _, ok := seen[archive]; ok {
					continue
				}
				seen[archive] = struct{}{}
				if saveVaultArchiveReferencedInState(nextState, candidate.kind, archive) {
					continue
				}
				path, ok := s.saveVaultPath(candidate.kind, archive)
				if !ok {
					continue
				}
				if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
					entry.Bytes += info.Size()
				}
			}
		}
	}

	// Orphans are counted against the current state: archives that the
	// retention above would delete are reported there, not as orphans.
	orphans := s.saveVaultOrphanArchivesLocked(s.saveVaultState)
	preview.Orphans = len(orphans)
	for _, orphan := range orphans {
		preview.OrphanBytes += orphan.Size
	}
	preview.OrphanFiles = orphans
	s.saveVaultMu.Unlock()

	sort.Slice(preview.Scopes, func(i, j int) bool {
		if preview.Scopes[i].Type != preview.Scopes[j].Type {
			return preview.Scopes[i].Type < preview.Scopes[j].Type
		}
		return preview.Scopes[i].GameID < preview.Scopes[j].GameID
	})
	writeJSON(w, http.StatusOK, preview)
}

type saveVaultMaintenanceReport struct {
	Versions         int
	Archives         int
	Failed           int
	FreedBytes       int64
	OrphanArchives   int
	OrphanBytes      int64
	OrphanRemoved    int
	TombstonesPurged int
}

// runSaveVaultMaintenance purges expired trash, applies automatic retention and
// optionally removes old orphan archives. It is a no-op when no C6 feature has
// been enabled, and dryRun never writes or removes anything.
func (s *server) runSaveVaultMaintenance(dryRun bool, now time.Time) (saveVaultMaintenanceReport, error) {
	report := saveVaultMaintenanceReport{}
	s.saveVaultMu.Lock()

	// Purge trash whose retention window has passed. The tombstone stays.
	s.manageMu.Lock()
	purgeNext := s.saveVaultManage.clone()
	for key, tombstone := range purgeNext.Scopes {
		if tombstone.State != saveVaultTombstoneDeleted || tombstone.PurgeAfter.IsZero() ||
			now.Before(tombstone.PurgeAfter) {
			continue
		}
		kind, gameID, ok := parseSaveVaultManageScopeKey(key)
		if !ok {
			continue
		}
		if live, liveExists := s.saveVaultState.Scopes[saveVaultScopeKey(kind, gameID)]; liveExists &&
			tombstone.Head != "" && live.Head == tombstone.Head && len(live.Snapshots) == tombstone.Snapshots {
			slog.Warn("skipping auto-purge of interrupted SaveVault delete", "scope", key)
			continue
		}
		if dryRun {
			report.TombstonesPurged++
			continue
		}
		if trashDir, found := s.saveVaultTrashDirectoryForTombstone(kind, gameID, tombstone); found {
			if err := s.manageRemoveAll(trashDir); err != nil && !errors.Is(err, os.ErrNotExist) {
				slog.Warn("purge SaveVault trash", "scope", key, "error", err)
				continue
			}
			s.saveVaultPruneEmptyTrash(kind, gameID)
		}
		tombstone.Head = ""
		tombstone.PurgeAfter = time.Time{}
		purgeNext.Scopes[key] = tombstone
		report.TombstonesPurged++
	}
	if !dryRun && report.TombstonesPurged > 0 {
		if err := s.persistSaveVaultManage(purgeNext); err != nil {
			s.manageMu.Unlock()
			s.saveVaultMu.Unlock()
			return report, err
		}
		s.saveVaultManage = purgeNext
	}
	retention := s.saveVaultManage.Retention.clone()
	s.manageMu.Unlock()

	// Automatic retention. State is committed before archives are unlinked.
	nextState := s.saveVaultState.clone()
	type removal struct {
		kind     string
		archives []string
	}
	removals := make([]removal, 0)
	for key, stored := range s.saveVaultState.Scopes {
		kind, gameID, ok := parseSaveVaultStateKey(key)
		if !ok {
			continue
		}
		gameKey, _ := saveVaultGameKey(kind, gameID, stored)
		policy, ok := retention.Games[gameKey]
		if !ok {
			policy = retention.Default
		}
		if !policy.Enabled || (policy.KeepVersions == 0 && policy.KeepDays == 0) {
			continue
		}
		nextScope, removedIDs := saveVaultPruneScopeWithRetention(stored, policy.KeepVersions,
			policy.KeepDays, now, s.saveVaultLockedSet(kind, gameID))
		if len(removedIDs) == 0 {
			continue
		}
		report.Versions += len(removedIDs)
		nextState.Scopes[key] = nextScope
		candidate := removal{kind: kind}
		for _, id := range removedIDs {
			if snapshot, ok := stored.Snapshots[id]; ok && snapshot.Archive != "" {
				candidate.archives = append(candidate.archives, snapshot.Archive)
			}
		}
		removals = append(removals, candidate)
	}
	if !dryRun && report.Versions > 0 {
		if err := s.persistSaveVaultState(nextState); err != nil {
			s.saveVaultMu.Unlock()
			return report, err
		}
		s.saveVaultState = nextState
	}

	// Delete archives that the post-removal state no longer references.
	nextCatalog := s.saveVaultCatalog.clone()
	catalogChanged := false
	seenArchives := make(map[string]struct{})
	for _, candidate := range removals {
		for _, archive := range candidate.archives {
			key := saveVaultCatalogKey(candidate.kind, archive)
			if _, ok := seenArchives[key]; ok {
				continue
			}
			seenArchives[key] = struct{}{}
			if saveVaultArchiveReferencedInState(nextState, candidate.kind, archive) {
				continue
			}
			path, ok := s.saveVaultPath(candidate.kind, archive)
			if !ok {
				continue
			}
			size := int64(0)
			if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
				size = info.Size()
			}
			if dryRun {
				report.FreedBytes += size
				continue
			}
			err := s.manageRemove(path)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				slog.Warn("delete SaveVault archive during retention", "type", candidate.kind, "name", archive, "error", err)
				report.Failed++
				continue
			}
			report.Archives++
			if err == nil {
				report.FreedBytes += size
			}
			if _, ok := nextCatalog.Files[key]; ok {
				delete(nextCatalog.Files, key)
				catalogChanged = true
			}
		}
	}

	// Orphan cleanup is independent from automatic retention. A dry run has not
	// deleted the retention candidates, so it counts against the current state.
	orphanBase := nextState
	if dryRun {
		orphanBase = s.saveVaultState
	}
	orphans := s.saveVaultOrphanArchivesLocked(orphanBase)
	report.OrphanArchives = len(orphans)
	for _, orphan := range orphans {
		report.OrphanBytes += orphan.Size
	}
	if retention.OrphanCleanup && !dryRun {
		for _, orphan := range orphans {
			if now.Sub(orphan.ModifiedAt) <= saveVaultOrphanGrace {
				continue
			}
			path, ok := s.saveVaultPath(orphan.Type, orphan.Name)
			if !ok {
				continue
			}
			if err := s.manageRemove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				slog.Warn("delete orphan SaveVault archive", "type", orphan.Type, "name", orphan.Name, "error", err)
				continue
			}
			report.OrphanRemoved++
			report.FreedBytes += orphan.Size
			catalogKey := saveVaultCatalogKey(orphan.Type, orphan.Name)
			if _, ok := nextCatalog.Files[catalogKey]; ok {
				delete(nextCatalog.Files, catalogKey)
				catalogChanged = true
			}
		}
	}
	if catalogChanged {
		if err := s.persistSaveVaultCatalog(nextCatalog); err != nil {
			slog.Warn("persist SaveVault retention catalog cleanup", "error", err)
		} else {
			s.saveVaultCatalog = nextCatalog
		}
	}
	s.saveVaultMu.Unlock()

	if !dryRun && (report.Versions > 0 || report.Archives > 0 || report.OrphanRemoved > 0) {
		s.retentionRemoved.Add(int64(report.Versions))
		detail := fmt.Sprintf("removed %d versions, %d archives, %d orphan archives, freed %d bytes",
			report.Versions, report.Archives, report.OrphanRemoved, report.FreedBytes)
		s.mu.Lock()
		s.recordEventLocked(event{Kind: eventRetention, Detail: detail})
		s.mu.Unlock()
	}
	return report, nil
}

// saveVaultManageCounts returns effective C6 gauge values. Locks that reference
// a missing scope or snapshot are retained on disk but are not counted.
func (s *server) saveVaultManageCounts() (deleted, retired, locked int) {
	s.saveVaultMu.Lock()
	defer s.saveVaultMu.Unlock()
	s.manageMu.Lock()
	defer s.manageMu.Unlock()
	for _, tombstone := range s.saveVaultManage.Scopes {
		switch tombstone.State {
		case saveVaultTombstoneDeleted:
			deleted++
		case saveVaultTombstoneRetired:
			retired++
		}
	}
	for key, locks := range s.saveVaultManage.Locks {
		kind, gameID, ok := parseSaveVaultManageScopeKey(key)
		if !ok {
			continue
		}
		stored, exists := s.saveVaultState.Scopes[saveVaultScopeKey(kind, gameID)]
		if !exists {
			continue
		}
		for id := range locks {
			if _, ok := stored.Snapshots[id]; ok {
				locked++
			}
		}
	}
	return deleted, retired, locked
}

func (s *server) saveVaultOrphanStats() (int, int64) {
	s.saveVaultMu.Lock()
	defer s.saveVaultMu.Unlock()
	orphans := s.saveVaultOrphanArchivesLocked(s.saveVaultState)
	var bytes int64
	for _, orphan := range orphans {
		bytes += orphan.Size
	}
	return len(orphans), bytes
}

func (s *server) saveVaultLockedSet(kind, gameID string) map[string]struct{} {
	locks := s.saveVaultLocks(kind, gameID)
	result := make(map[string]struct{}, len(locks))
	for id := range locks {
		result[id] = struct{}{}
	}
	return result
}
