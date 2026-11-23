// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

// C6 SaveVault management state lives in opensave-manage.json. Existing state
// files never gain C6 fields, so a v1.8.0 binary can ignore this file and keep
// reading the data directory.
//
// Lock order: openSaveMu -> manageMu and s.mu -> manageMu. manageMu is a leaf
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
	openSaveManageFileName     = "opensave-manage.json"
	openSaveTrashDirectoryName = "opensave-trash"

	openSaveTombstoneDeleted = "deleted"
	openSaveTombstoneRetired = "retired"

	openSaveTombstoneRetention = 7 * 24 * time.Hour
	openSaveOrphanGrace        = 24 * time.Hour
	openSaveMaxLockNoteRunes   = 120
)

type openSaveManageTombstone struct {
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
type tombstone = openSaveManageTombstone

type openSaveManageLock struct {
	Note string    `json:"note,omitempty"`
	At   time.Time `json:"at,omitzero"`
}

type openSaveRetentionPolicy struct {
	Enabled      bool `json:"enabled"`
	KeepVersions int  `json:"keepVersions,omitempty"`
	KeepDays     int  `json:"keepDays,omitempty"`
}

type openSaveRetentionSettings struct {
	Default       openSaveRetentionPolicy            `json:"default"`
	OrphanCleanup bool                               `json:"orphanCleanup"`
	Games         map[string]openSaveRetentionPolicy `json:"games,omitempty"`
}

type openSaveManageState struct {
	Version   int                                      `json:"version"`
	Scopes    map[string]openSaveManageTombstone       `json:"scopes,omitempty"`
	Locks     map[string]map[string]openSaveManageLock `json:"locks,omitempty"`
	Retention openSaveRetentionSettings                `json:"retention"`
}

// openSaveManageFileOps allows tests to inject filesystem failures without
// changing the production call sites. A nil hook uses the os package.
type openSaveManageFileOps struct {
	MkdirAll  func(path string, perm os.FileMode) error
	Rename    func(oldpath, newpath string) error
	Remove    func(path string) error
	RemoveAll func(path string) error
	WriteJSON func(path string, value any) error
}

func defaultOpenSaveManageState() openSaveManageState {
	return openSaveManageState{
		Version: 1,
		Scopes:  make(map[string]openSaveManageTombstone),
		Locks:   make(map[string]map[string]openSaveManageLock),
		Retention: openSaveRetentionSettings{
			Default: openSaveRetentionPolicy{KeepVersions: 20},
			Games:   make(map[string]openSaveRetentionPolicy),
		},
	}
}

func (policy openSaveRetentionPolicy) valid() bool {
	if policy.KeepVersions < 0 || policy.KeepVersions > 1000 || policy.KeepDays < 0 || policy.KeepDays > 3650 {
		return false
	}
	return !policy.Enabled || policy.KeepVersions >= 1
}

func (settings openSaveRetentionSettings) valid() bool {
	if !settings.Default.valid() {
		return false
	}
	for key, policy := range settings.Games {
		if !validOpenSaveGameKey(key) || !policy.valid() {
			return false
		}
	}
	return true
}

func (settings openSaveRetentionSettings) clone() openSaveRetentionSettings {
	next := settings
	next.Games = make(map[string]openSaveRetentionPolicy, len(settings.Games)+1)
	for key, policy := range settings.Games {
		next.Games[key] = policy
	}
	return next
}

func (state openSaveManageState) clone() openSaveManageState {
	next := openSaveManageState{
		Version:   state.Version,
		Scopes:    make(map[string]openSaveManageTombstone, len(state.Scopes)+1),
		Locks:     make(map[string]map[string]openSaveManageLock, len(state.Locks)+1),
		Retention: state.Retention.clone(),
	}
	for key, tombstone := range state.Scopes {
		next.Scopes[key] = tombstone
	}
	for key, locks := range state.Locks {
		copied := make(map[string]openSaveManageLock, len(locks)+1)
		for id, lock := range locks {
			copied[id] = lock
		}
		next.Locks[key] = copied
	}
	return next
}

func (s *server) openSaveManagePath() string {
	return filepath.Join(s.dataDir, openSaveManageFileName)
}

// loadOpenSaveManage never creates the file. A deployment that has never used a
// C6 feature keeps no management state at all.
func (s *server) loadOpenSaveManage() error {
	state := openSaveManageState{}
	if err := readJSONFile(s.openSaveManagePath(), &state); err != nil {
		return err
	}
	if state.Version != 0 && state.Version != 1 {
		return fmt.Errorf("%s contains an unsupported version", openSaveManageFileName)
	}
	if state.Scopes == nil {
		state.Scopes = make(map[string]openSaveManageTombstone)
	}
	if state.Locks == nil {
		state.Locks = make(map[string]map[string]openSaveManageLock)
	}
	if state.Retention.Games == nil {
		state.Retention.Games = make(map[string]openSaveRetentionPolicy)
	}
	if state.Retention.Default.KeepVersions == 0 && state.Retention.Default.KeepDays == 0 {
		state.Retention.Default.KeepVersions = 20
	}
	if !state.Retention.valid() {
		return fmt.Errorf("%s contains invalid retention settings", openSaveManageFileName)
	}
	state.Version = 1
	s.openSaveManage = state
	return nil
}

func (s *server) persistOpenSaveManage(state openSaveManageState) error {
	state.Version = 1
	return writeJSONFile(s.openSaveManagePath(), state)
}

func openSaveManageScopeKey(kind, gameID string) string {
	return kind + "/" + gameID
}

func parseOpenSaveManageScopeKey(key string) (string, string, bool) {
	kind, gameID, ok := strings.Cut(key, "/")
	if !ok || !validOpenSaveType(kind) || !validOpenSaveGameID(gameID) {
		return "", "", false
	}
	return kind, gameID, true
}

func parseOpenSaveStateKey(key string) (string, string, bool) {
	kind, gameID, ok := strings.Cut(key, "\x00")
	if !ok || !validOpenSaveType(kind) || !validOpenSaveGameID(gameID) {
		return "", "", false
	}
	return kind, gameID, true
}

// openSaveTombstone reads one tombstone. The caller may hold openSaveMu; this
// function acquires only manageMu.
func (s *server) openSaveTombstone(kind, gameID string) (openSaveManageTombstone, bool) {
	s.manageMu.Lock()
	defer s.manageMu.Unlock()
	tombstone, ok := s.openSaveManage.Scopes[openSaveManageScopeKey(kind, gameID)]
	return tombstone, ok
}

// openSaveTombstoneApplies implements the rollback exception: a deleted scope
// that exists again with different content was recreated after a rollback, so
// its tombstone must not be executed. If the live scope still matches the
// tombstone's head and snapshot count, a delete was interrupted and clients
// must stay blocked until the administrator retries. A retired scope normally
// remains in state, so it always applies.
func openSaveTombstoneApplies(tombstone openSaveManageTombstone, scope openSaveScopeState, scopeExists bool) bool {
	switch tombstone.State {
	case openSaveTombstoneDeleted:
		if !scopeExists {
			return true
		}
		return scope.Head == tombstone.Head && len(scope.Snapshots) == tombstone.Snapshots
	case openSaveTombstoneRetired:
		return true
	default:
		return false
	}
}

func (s *server) openSaveScopeBlocked(kind, gameID string) (openSaveManageTombstone, bool) {
	s.openSaveMu.Lock()
	defer s.openSaveMu.Unlock()
	return s.openSaveScopeBlockedLocked(kind, gameID)
}

// openSaveScopeBlockedLocked must be called while openSaveMu is held.
func (s *server) openSaveScopeBlockedLocked(kind, gameID string) (openSaveManageTombstone, bool) {
	scope, exists := s.openSaveState.Scopes[openSaveScopeKey(kind, gameID)]
	tombstone, ok := s.openSaveTombstone(kind, gameID)
	if !ok || !openSaveTombstoneApplies(tombstone, scope, exists) {
		return openSaveManageTombstone{}, false
	}
	return tombstone, true
}

// openSaveLocks returns a copy of the locks of one scope. The caller may hold
// openSaveMu; this function acquires only manageMu.
func (s *server) openSaveLocks(kind, gameID string) map[string]openSaveManageLock {
	s.manageMu.Lock()
	defer s.manageMu.Unlock()
	stored := s.openSaveManage.Locks[openSaveManageScopeKey(kind, gameID)]
	result := make(map[string]openSaveManageLock, len(stored))
	for id, lock := range stored {
		result[id] = lock
	}
	return result
}

func applyOpenSaveLock(snapshot *openSaveScopeSnapshot, locks map[string]openSaveManageLock) {
	snapshot.Locked = false
	snapshot.LockNote = ""
	if lock, ok := locks[snapshot.ID]; ok {
		snapshot.Locked = true
		snapshot.LockNote = lock.Note
	}
}

func openSaveArchiveGameID(name string) (string, bool) {
	gameID, _, found := strings.Cut(name, "__")
	if !found || !validOpenSaveGameID(gameID) {
		return "", false
	}
	return gameID, true
}

type openSaveTombstoneSuccessor struct {
	Type   string `json:"type"`
	GameID string `json:"gameId"`
}

type openSaveTombstoneResponse struct {
	Code      string                      `json:"code"`
	Error     string                      `json:"error"`
	DeletedAt time.Time                   `json:"deletedAt,omitzero"`
	Successor *openSaveTombstoneSuccessor `json:"successor,omitempty"`
}

// openSaveSuccessorScope turns a C1 game key back into the type/game ID shown
// to a client. A scope-only key loses its "scope:" marker; identity keys stay
// readable as the game identifier.
func openSaveSuccessorScope(successor string) (string, string, bool) {
	kind, gameID, ok := strings.Cut(successor, "/")
	if !ok || !validOpenSaveType(kind) || !validOpenSaveGameID(gameID) {
		return "", "", false
	}
	return kind, gameID, true
}

func writeOpenSaveTombstone(w http.ResponseWriter, tombstone openSaveManageTombstone) {
	response := openSaveTombstoneResponse{}
	if tombstone.State == openSaveTombstoneRetired {
		response.Code = "scope_retired"
		response.Error = "this SaveVault scope was merged into another scope"
		if kind, gameID, ok := openSaveSuccessorScope(tombstone.Successor); ok {
			response.Successor = &openSaveTombstoneSuccessor{Type: kind, GameID: gameID}
		}
	} else {
		response.Code = "scope_deleted"
		response.Error = "this SaveVault scope was deleted on the server"
		response.DeletedAt = tombstone.At
	}
	writeJSON(w, http.StatusGone, response)
}

func writeOpenSavePreconditionFailed(w http.ResponseWriter, head string) {
	writeJSON(w, http.StatusPreconditionFailed, map[string]string{
		"error": "OpenSave scope head changed",
		"head":  head,
	})
}

func (s *server) openSaveTrashRoot() string {
	return filepath.Join(s.dataDir, openSaveTrashDirectoryName)
}

func (s *server) openSaveTrashScopeDirectory(kind, gameID string) string {
	return filepath.Join(s.openSaveTrashRoot(), kind, gameID)
}

func openSaveTrashTimestamp(at time.Time) string {
	if at.IsZero() {
		return "unknown"
	}
	return strings.ReplaceAll(at.UTC().Format(time.RFC3339), ":", "")
}

func (s *server) openSaveTrashDirectory(kind, gameID string, at time.Time) string {
	return filepath.Join(s.openSaveTrashScopeDirectory(kind, gameID), openSaveTrashTimestamp(at))
}

// openSavePruneEmptyTrash removes now-empty scope, type, and root directories.
// It never fails an operation: a non-empty parent is simply left in place.
func (s *server) openSavePruneEmptyTrash(kind, gameID string) {
	_ = os.Remove(s.openSaveTrashScopeDirectory(kind, gameID))
	_ = os.Remove(filepath.Join(s.openSaveTrashRoot(), kind))
	_ = os.Remove(s.openSaveTrashRoot())
}

// openSaveTrashDirectoryForTombstone returns the existing trash directory. A
// tombstone written before timestamps were recorded falls back to the only
// timestamped child directory.
func (s *server) openSaveTrashDirectoryForTombstone(kind, gameID string,
	tombstone openSaveManageTombstone) (string, bool) {
	if !tombstone.At.IsZero() {
		path := s.openSaveTrashDirectory(kind, gameID, tombstone.At)
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return path, true
		}
		return path, false
	}
	entries, err := os.ReadDir(s.openSaveTrashScopeDirectory(kind, gameID))
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
	path := filepath.Join(s.openSaveTrashScopeDirectory(kind, gameID), names[len(names)-1])
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
	if s.manageFileOps.Remove != nil {
		return s.manageFileOps.Remove(path)
	}
	return os.Remove(path)
}

func (s *server) manageRemoveAll(path string) error {
	if s.manageFileOps.RemoveAll != nil {
		return s.manageFileOps.RemoveAll(path)
	}
	return os.RemoveAll(path)
}

func (s *server) manageWriteJSON(path string, value any) error {
	if s.manageFileOps.WriteJSON != nil {
		return s.manageFileOps.WriteJSON(path, value)
	}
	return writeJSONFile(path, value)
}

// openSaveScopeArchiveFilesLocked lists every archive file whose name starts
// with gameID+"__", including archives that no snapshot references. The caller
// holds openSaveMu.
func (s *server) openSaveScopeArchiveFilesLocked(kind, gameID string) ([]string, int64, error) {
	dir := filepath.Join(s.openSaveNativeDirectory(), kind)
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

// rollbackOpenSaveDelete restores already moved archives and removes the
// tombstone only when every rollback step succeeded. A failed rollback leaves
// the tombstone in place so clients stay blocked and a retry can finish.
func (s *server) rollbackOpenSaveDelete(kind, gameID, key, trashDir string, moved []string) bool {
	ok := true
	for _, name := range moved {
		destination, valid := s.openSavePath(kind, name)
		if !valid {
			ok = false
			continue
		}
		if err := s.manageRename(filepath.Join(trashDir, name), destination); err != nil {
			slog.Warn("rollback OpenSave delete archive", "type", kind, "name", name, "error", err)
			ok = false
		}
	}
	if ok {
		if err := s.manageRemoveAll(trashDir); err != nil && !errors.Is(err, os.ErrNotExist) {
			// All moved archives are back in the live directory; a leftover
			// trash directory is harmless and must not keep clients blocked.
			slog.Warn("rollback OpenSave delete trash", "dir", trashDir, "error", err)
		} else {
			s.openSavePruneEmptyTrash(kind, gameID)
		}
	}
	if ok {
		s.manageMu.Lock()
		next := s.openSaveManage.clone()
		delete(next.Scopes, key)
		if err := s.persistOpenSaveManage(next); err != nil {
			slog.Warn("rollback OpenSave tombstone", "scope", key, "error", err)
			ok = false
		} else {
			s.openSaveManage = next
		}
		s.manageMu.Unlock()
	}
	return ok
}

func (s *server) handleDeleteOpenSaveScope(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := openSaveScopeFromRequest(w, r)
	if !ok {
		return
	}
	expected, ok := expectedOpenSaveHead(r)
	if !ok {
		writeError(w, http.StatusPreconditionRequired, "If-Match must contain the current OpenSave head")
		return
	}
	now := s.now().UTC()
	manageKey := openSaveManageScopeKey(kind, gameID)

	s.openSaveMu.Lock()
	stored, exists := s.openSaveState.Scopes[key]
	tombstone, hasTombstone := s.openSaveTombstone(kind, gameID)
	if !exists {
		s.openSaveMu.Unlock()
		if hasTombstone && tombstone.State == openSaveTombstoneDeleted {
			writeJSON(w, http.StatusOK, tombstone)
			return
		}
		writeError(w, http.StatusNotFound, "OpenSave scope not found")
		return
	}
	if stored.Head != expected {
		s.openSaveMu.Unlock()
		writeOpenSavePreconditionFailed(w, stored.Head)
		return
	}
	// Scopes merged into this one name it as their successor. Deleting it would
	// point their clients at a scope that no longer exists, so they must be
	// unretired (or deleted) first.
	s.manageMu.Lock()
	hasMergedMembers := false
	for _, other := range s.openSaveManage.Scopes {
		if other.State == openSaveTombstoneRetired && other.Successor == manageKey {
			hasMergedMembers = true
			break
		}
	}
	s.manageMu.Unlock()
	if hasMergedMembers {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusConflict, "OpenSave scope has merged members; unretire them first")
		return
	}
	files, bytes, err := s.openSaveScopeArchiveFilesLocked(kind, gameID)
	if err != nil {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot inspect OpenSave archives")
		return
	}

	// A retry after a partial failure reuses the original deletion time so it
	// continues the same trash directory and purge deadline. A recreated scope
	// has a different head and starts a fresh deletion.
	at := now
	continuation := hasTombstone && tombstone.State == openSaveTombstoneDeleted && !tombstone.At.IsZero() &&
		tombstone.Head == stored.Head
	if continuation {
		at = tombstone.At
	} else if hasTombstone && tombstone.State == openSaveTombstoneDeleted {
		// A recreated scope is being deleted again. The old tombstone is
		// replaced, so its trash would otherwise never be purged.
		if oldDir, found := s.openSaveTrashDirectoryForTombstone(kind, gameID, tombstone); found {
			if err := s.manageRemoveAll(oldDir); err != nil {
				slog.Warn("remove stale OpenSave trash", "scope", manageKey, "error", err)
			}
			s.openSavePruneEmptyTrash(kind, gameID)
		}
	}
	nextTombstone := openSaveManageTombstone{
		State:      openSaveTombstoneDeleted,
		At:         at,
		By:         "admin",
		Head:       stored.Head,
		Snapshots:  len(stored.Snapshots),
		Bytes:      bytes,
		PurgeAfter: at.Add(openSaveTombstoneRetention),
	}
	for _, snapshot := range stored.Snapshots {
		if snapshot.Conflict {
			nextTombstone.Conflicts++
		}
	}

	s.manageMu.Lock()
	nextManage := s.openSaveManage.clone()
	nextManage.Scopes[manageKey] = nextTombstone
	if err := s.persistOpenSaveManage(nextManage); err != nil {
		s.manageMu.Unlock()
		s.openSaveMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist OpenSave tombstone")
		return
	}
	s.openSaveManage = nextManage
	s.manageMu.Unlock()

	trashDir := s.openSaveTrashDirectory(kind, gameID, at)
	rollback := func(moved []string) {
		if !s.rollbackOpenSaveDelete(kind, gameID, manageKey, trashDir, moved) {
			slog.Warn("OpenSave delete rollback incomplete; tombstone retained", "scope", manageKey)
		}
	}
	if err := s.manageMkdirAll(trashDir, 0o700); err != nil {
		rollback(nil)
		s.openSaveMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot create OpenSave trash directory")
		return
	}
	if err := s.manageWriteJSON(filepath.Join(trashDir, "scope.json"), stored); err != nil {
		rollback(nil)
		s.openSaveMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot write OpenSave trash state")
		return
	}
	catalogEntries := make(map[string]openSaveFileMetadata)
	for _, name := range files {
		if metadata, ok := s.openSaveCatalog.Files[openSaveCatalogKey(kind, name)]; ok {
			catalogEntries[name] = metadata
		}
	}
	if len(catalogEntries) > 0 {
		if err := s.manageWriteJSON(filepath.Join(trashDir, "catalog.json"), catalogEntries); err != nil {
			rollback(nil)
			s.openSaveMu.Unlock()
			writeError(w, http.StatusInternalServerError, "cannot write OpenSave trash catalog")
			return
		}
	}

	moved := make([]string, 0, len(files))
	for _, name := range files {
		source, valid := s.openSavePath(kind, name)
		if !valid {
			continue
		}
		if err := s.manageRename(source, filepath.Join(trashDir, name)); err != nil {
			rollback(moved)
			s.openSaveMu.Unlock()
			writeError(w, http.StatusInternalServerError, "cannot move OpenSave archives to trash")
			return
		}
		moved = append(moved, name)
	}

	nextState := s.openSaveState.clone()
	delete(nextState.Scopes, key)
	if err := s.persistOpenSaveState(nextState); err != nil {
		rollback(moved)
		s.openSaveMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist OpenSave scope state")
		return
	}
	s.openSaveState = nextState

	// Catalog cleanup is advisory: the files are already safely in trash.
	nextCatalog := s.openSaveCatalog.clone()
	catalogChanged := false
	for _, name := range moved {
		catalogKey := openSaveCatalogKey(kind, name)
		if _, ok := nextCatalog.Files[catalogKey]; ok {
			delete(nextCatalog.Files, catalogKey)
			catalogChanged = true
		}
	}
	if catalogChanged {
		if err := s.persistOpenSaveCatalog(nextCatalog); err != nil {
			slog.Warn("persist OpenSave trash catalog cleanup", "type", kind, "error", err)
		} else {
			s.openSaveCatalog = nextCatalog
		}
	}
	s.openSaveMu.Unlock()

	s.recordOpenSaveManageEvent(eventDeleteScope,
		fmt.Sprintf("%s/%s %d versions, %d bytes", kind, gameID, nextTombstone.Snapshots, nextTombstone.Bytes),
		kind, gameID, "")
	writeJSON(w, http.StatusOK, nextTombstone)
}

func (s *server) handleRestoreOpenSaveScope(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := openSaveScopeFromRequest(w, r)
	if !ok {
		return
	}
	now := s.now().UTC()
	manageKey := openSaveManageScopeKey(kind, gameID)

	s.openSaveMu.Lock()
	tombstone, hasTombstone := s.openSaveTombstone(kind, gameID)
	if !hasTombstone {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusNotFound, "OpenSave tombstone not found")
		return
	}
	if tombstone.State != openSaveTombstoneDeleted {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusConflict, "only a deleted OpenSave scope can be restored")
		return
	}
	if _, exists := s.openSaveState.Scopes[key]; exists {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusConflict, "OpenSave scope already exists")
		return
	}
	if !tombstone.PurgeAfter.IsZero() && !now.Before(tombstone.PurgeAfter) {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusConflict, "OpenSave restore window has expired")
		return
	}
	trashDir, found := s.openSaveTrashDirectoryForTombstone(kind, gameID, tombstone)
	if !found {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusNotFound, "OpenSave trash directory not found")
		return
	}
	restored := openSaveScopeState{}
	if err := readJSONFile(filepath.Join(trashDir, "scope.json"), &restored); err != nil {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot read OpenSave trash state")
		return
	}
	if restored.Snapshots == nil {
		restored.Snapshots = make(map[string]openSaveScopeSnapshot)
	}

	names, err := openSaveTrashFiles(trashDir)
	if err != nil {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot inspect OpenSave trash directory")
		return
	}
	destinationDir := filepath.Join(s.openSaveNativeDirectory(), kind)
	if err := s.manageMkdirAll(destinationDir, 0o700); err != nil {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot create OpenSave storage")
		return
	}
	moved := make([]string, 0, len(names))
	moveBack := func() {
		for _, name := range moved {
			destination := filepath.Join(destinationDir, name)
			if err := s.manageRename(destination, filepath.Join(trashDir, name)); err != nil {
				slog.Warn("rollback OpenSave restore archive", "type", kind, "name", name, "error", err)
			}
		}
	}
	for _, name := range names {
		destination := filepath.Join(destinationDir, name)
		if _, err := os.Lstat(destination); err == nil {
			moveBack()
			s.openSaveMu.Unlock()
			writeError(w, http.StatusConflict, "OpenSave archive already exists in the live directory")
			return
		}
		if err := s.manageRename(filepath.Join(trashDir, name), destination); err != nil {
			moveBack()
			s.openSaveMu.Unlock()
			writeError(w, http.StatusInternalServerError, "cannot restore OpenSave archive")
			return
		}
		moved = append(moved, name)
	}

	nextState := s.openSaveState.clone()
	nextState.Scopes[key] = restored
	if err := s.persistOpenSaveState(nextState); err != nil {
		moveBack()
		s.openSaveMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist OpenSave scope state")
		return
	}
	s.openSaveState = nextState

	catalogEntries := make(map[string]openSaveFileMetadata)
	_ = readJSONFile(filepath.Join(trashDir, "catalog.json"), &catalogEntries)
	for _, snapshot := range restored.Snapshots {
		if _, ok := catalogEntries[snapshot.Archive]; !ok {
			catalogEntries[snapshot.Archive] = openSaveFileMetadata{
				SHA256:     snapshot.ContentDigest,
				Device:     snapshot.Device,
				DeviceName: snapshot.DeviceName,
				UploadedAt: snapshot.ReceivedAt,
			}
		}
	}
	if len(catalogEntries) > 0 {
		nextCatalog := s.openSaveCatalog.clone()
		for name, metadata := range catalogEntries {
			nextCatalog.Files[openSaveCatalogKey(kind, name)] = metadata
		}
		if err := s.persistOpenSaveCatalog(nextCatalog); err != nil {
			slog.Warn("persist OpenSave restored catalog", "type", kind, "error", err)
		} else {
			s.openSaveCatalog = nextCatalog
		}
	}

	s.manageMu.Lock()
	nextManage := s.openSaveManage.clone()
	delete(nextManage.Scopes, manageKey)
	if err := s.persistOpenSaveManage(nextManage); err != nil {
		s.manageMu.Unlock()
		oldState := s.openSaveState.clone()
		delete(oldState.Scopes, key)
		if persistErr := s.persistOpenSaveState(oldState); persistErr == nil {
			s.openSaveState = oldState
		} else {
			slog.Warn("rollback OpenSave restore state", "error", persistErr)
		}
		moveBack()
		s.openSaveMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist OpenSave tombstone removal")
		return
	}
	s.openSaveManage = nextManage
	s.manageMu.Unlock()

	audit := s.openSaveScopeAuditLocked(kind, gameID, restored)
	if err := s.manageRemoveAll(trashDir); err == nil || errors.Is(err, os.ErrNotExist) {
		s.openSavePruneEmptyTrash(kind, gameID)
	}
	s.openSaveMu.Unlock()

	s.recordOpenSaveManageEvent(eventRestoreScope,
		fmt.Sprintf("%s/%s %d versions", kind, gameID, len(restored.Snapshots)), kind, gameID, "")
	writeJSON(w, http.StatusOK, audit)
}

func openSaveTrashFiles(dir string) ([]string, error) {
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

func (s *server) handleRetireOpenSaveScope(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := openSaveScopeFromRequest(w, r)
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
	if !validOpenSaveGameID(targetID) {
		writeError(w, http.StatusBadRequest, "into must be a valid OpenSave game id")
		return
	}
	if targetID == gameID {
		writeError(w, http.StatusBadRequest, "a scope cannot be merged into itself")
		return
	}
	now := s.now().UTC()
	manageKey := openSaveManageScopeKey(kind, gameID)

	s.openSaveMu.Lock()
	source, exists := s.openSaveState.Scopes[key]
	if !exists {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusNotFound, "OpenSave scope not found")
		return
	}
	if _, hasTombstone := s.openSaveTombstone(kind, gameID); hasTombstone {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusConflict, "OpenSave scope already has a tombstone")
		return
	}
	target, targetExists := s.openSaveState.Scopes[openSaveScopeKey(kind, targetID)]
	_, targetHasTombstone := s.openSaveTombstone(kind, targetID)
	if !targetExists {
		crossType := false
		for stateKey := range s.openSaveState.Scopes {
			otherKind, otherGameID, ok := parseOpenSaveStateKey(stateKey)
			if ok && otherKind != kind && otherGameID == targetID {
				crossType = true
				break
			}
		}
		s.openSaveMu.Unlock()
		switch {
		case targetHasTombstone:
			writeError(w, http.StatusBadRequest, "target OpenSave scope was deleted")
		case crossType:
			writeError(w, http.StatusBadRequest, "a scope can only be merged into the same type")
		default:
			writeError(w, http.StatusNotFound, "target OpenSave scope not found")
		}
		return
	}
	if targetHasTombstone {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusConflict, "target OpenSave scope has a tombstone")
		return
	}
	sourceGameKey, _ := openSaveGameKey(kind, gameID, source)
	s.manageMu.Lock()
	chained := false
	sourceScopeKey := openSaveManageScopeKey(kind, gameID)
	for _, other := range s.openSaveManage.Scopes {
		if other.State == openSaveTombstoneRetired &&
			(other.SuccessorGame == sourceGameKey || other.Successor == sourceScopeKey) {
			chained = true
			break
		}
	}
	s.manageMu.Unlock()
	if chained {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusConflict, "OpenSave scope already has merged members")
		return
	}
	targetGameKey, _ := openSaveGameKey(kind, targetID, target)
	nextTombstone := openSaveManageTombstone{
		State:         openSaveTombstoneRetired,
		At:            now,
		By:            "admin",
		Successor:     openSaveManageScopeKey(kind, targetID),
		SuccessorGame: targetGameKey,
	}

	s.manageMu.Lock()
	nextManage := s.openSaveManage.clone()
	nextManage.Scopes[manageKey] = nextTombstone
	if err := s.persistOpenSaveManage(nextManage); err != nil {
		s.manageMu.Unlock()
		s.openSaveMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist OpenSave tombstone")
		return
	}
	s.openSaveManage = nextManage
	s.manageMu.Unlock()

	nextSource := source.clone()
	nextSource.GameLink = targetGameKey
	nextState := s.openSaveState.clone()
	nextState.Scopes[key] = nextSource
	if err := s.persistOpenSaveState(nextState); err != nil {
		s.manageMu.Lock()
		rollback := s.openSaveManage.clone()
		delete(rollback.Scopes, manageKey)
		if rollbackErr := s.persistOpenSaveManage(rollback); rollbackErr != nil {
			slog.Warn("rollback OpenSave retire tombstone", "scope", manageKey, "error", rollbackErr)
		} else {
			s.openSaveManage = rollback
		}
		s.manageMu.Unlock()
		s.openSaveMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist OpenSave scope state")
		return
	}
	s.openSaveState = nextState
	s.openSaveMu.Unlock()

	s.recordOpenSaveManageEvent(eventRetireScope,
		fmt.Sprintf("%s/%s into %s", kind, gameID, targetGameKey), kind, gameID, "")
	writeJSON(w, http.StatusOK, nextTombstone)
}

// openSaveTombstoneSuccessorGame returns the C1 game key recorded at merge
// time, falling back to the target scope's current key for older tombstones.
// The caller holds openSaveMu.
func (s *server) openSaveTombstoneSuccessorGame(kind string, tombstone openSaveManageTombstone) string {
	if tombstone.SuccessorGame != "" {
		return tombstone.SuccessorGame
	}
	successorKind, targetID, ok := parseOpenSaveManageScopeKey(tombstone.Successor)
	if !ok || successorKind != kind {
		return ""
	}
	target, ok := s.openSaveState.Scopes[openSaveScopeKey(kind, targetID)]
	if !ok {
		return ""
	}
	gameKey, _ := openSaveGameKey(kind, targetID, target)
	return gameKey
}

func (s *server) handleUnretireOpenSaveScope(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := openSaveScopeFromRequest(w, r)
	if !ok {
		return
	}

	manageKey := openSaveManageScopeKey(kind, gameID)
	s.openSaveMu.Lock()
	source, exists := s.openSaveState.Scopes[key]
	if !exists {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusNotFound, "OpenSave scope not found")
		return
	}
	tombstone, hasTombstone := s.openSaveTombstone(kind, gameID)
	if !hasTombstone || tombstone.State != openSaveTombstoneRetired {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusNotFound, "OpenSave retired tombstone not found")
		return
	}

	nextSource := source.clone()
	if successorGame := s.openSaveTombstoneSuccessorGame(kind, tombstone); successorGame != "" &&
		nextSource.GameLink == successorGame {
		nextSource.GameLink = ""
	}
	oldState := s.openSaveState
	nextState := s.openSaveState.clone()
	nextState.Scopes[key] = nextSource
	if err := s.persistOpenSaveState(nextState); err != nil {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist OpenSave scope state")
		return
	}
	s.openSaveState = nextState

	s.manageMu.Lock()
	nextManage := s.openSaveManage.clone()
	delete(nextManage.Scopes, manageKey)
	if err := s.persistOpenSaveManage(nextManage); err != nil {
		s.manageMu.Unlock()
		if rollbackErr := s.persistOpenSaveState(oldState); rollbackErr == nil {
			s.openSaveState = oldState
		} else {
			slog.Warn("rollback OpenSave unretire state", "error", rollbackErr)
		}
		s.openSaveMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist OpenSave tombstone removal")
		return
	}
	s.openSaveManage = nextManage
	s.manageMu.Unlock()

	audit := s.openSaveScopeAuditLocked(kind, gameID, nextSource)
	s.openSaveMu.Unlock()

	s.recordOpenSaveManageEvent(eventUnretireScope, fmt.Sprintf("%s/%s", kind, gameID), kind, gameID, "")
	writeJSON(w, http.StatusOK, audit)
}

type openSaveTombstoneView struct {
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

func (s *server) handleListOpenSaveTombstones(w http.ResponseWriter, _ *http.Request) {
	now := s.now().UTC()
	s.openSaveMu.Lock()
	s.manageMu.Lock()
	views := make([]openSaveTombstoneView, 0, len(s.openSaveManage.Scopes))
	for key, tombstone := range s.openSaveManage.Scopes {
		kind, gameID, ok := parseOpenSaveManageScopeKey(key)
		if !ok {
			continue
		}
		view := openSaveTombstoneView{
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
		_, exists := s.openSaveState.Scopes[openSaveScopeKey(kind, gameID)]
		if tombstone.State == openSaveTombstoneDeleted && exists {
			view.ConflictState = "recreated"
		}
		if tombstone.State == openSaveTombstoneDeleted {
			if _, found := s.openSaveTrashDirectoryForTombstone(kind, gameID, tombstone); found {
				view.Restorable = !exists && (tombstone.PurgeAfter.IsZero() || now.Before(tombstone.PurgeAfter))
			}
		}
		views = append(views, view)
	}
	s.manageMu.Unlock()
	s.openSaveMu.Unlock()
	sort.Slice(views, func(i, j int) bool {
		if views[i].Type != views[j].Type {
			return views[i].Type < views[j].Type
		}
		return views[i].GameID < views[j].GameID
	})
	writeJSON(w, http.StatusOK, map[string]any{"tombstones": views})
}

func (s *server) handleClearOpenSaveTombstone(w http.ResponseWriter, r *http.Request) {
	kind, gameID, _, ok := openSaveScopeFromRequest(w, r)
	if !ok {
		return
	}
	manageKey := openSaveManageScopeKey(kind, gameID)
	s.openSaveMu.Lock()
	tombstone, hasTombstone := s.openSaveTombstone(kind, gameID)
	if !hasTombstone {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusNotFound, "OpenSave tombstone not found")
		return
	}
	if tombstone.State == openSaveTombstoneRetired {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusBadRequest, "a retired scope must be unretired")
		return
	}
	// Persist the tombstone removal before deleting the trash directory so a
	// persistence failure never destroys the recoverable archives irreversibly.
	s.manageMu.Lock()
	nextManage := s.openSaveManage.clone()
	delete(nextManage.Scopes, manageKey)
	if err := s.persistOpenSaveManage(nextManage); err != nil {
		s.manageMu.Unlock()
		s.openSaveMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist OpenSave tombstone removal")
		return
	}
	s.openSaveManage = nextManage
	s.manageMu.Unlock()
	if trashDir, found := s.openSaveTrashDirectoryForTombstone(kind, gameID, tombstone); found {
		if err := s.manageRemoveAll(trashDir); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("remove OpenSave trash directory after clearing tombstone", "scope", manageKey, "error", err)
		} else {
			s.openSavePruneEmptyTrash(kind, gameID)
		}
	}
	s.openSaveMu.Unlock()

	s.recordOpenSaveManageEvent(eventTombstoneCleared, fmt.Sprintf("%s/%s", kind, gameID), kind, gameID, "")
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) recordOpenSaveManageEvent(kind, detail, scopeKind, gameID, snapshot string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordEventLocked(event{
		Kind:     kind,
		Scope:    openSaveEventScope(scopeKind, gameID),
		Snapshot: snapshot,
		Detail:   detail,
	})
}

func validOpenSaveLockNote(note string) bool {
	if !utf8.ValidString(note) || utf8.RuneCountInString(note) > openSaveMaxLockNoteRunes {
		return false
	}
	for _, character := range note {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func (s *server) handlePutOpenSaveSnapshotLock(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := openSaveScopeFromRequest(w, r)
	if !ok {
		return
	}
	snapshotID := r.PathValue("snapshot")
	if !validSnapshotID(snapshotID) {
		writeError(w, http.StatusBadRequest, "invalid OpenSave snapshot id")
		return
	}
	var request struct {
		Note string `json:"note"`
	}
	if !decodeAdminJSON(w, r, &request) {
		return
	}
	note := strings.TrimSpace(request.Note)
	if !validOpenSaveLockNote(note) {
		writeError(w, http.StatusBadRequest, "lock note must be at most 120 characters without control characters")
		return
	}
	manageKey := openSaveManageScopeKey(kind, gameID)

	s.openSaveMu.Lock()
	stored, exists := s.openSaveState.Scopes[key]
	if !exists {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusNotFound, "OpenSave scope not found")
		return
	}
	if _, exists := stored.Snapshots[snapshotID]; !exists {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusNotFound, "OpenSave snapshot not found")
		return
	}
	lock := openSaveManageLock{Note: note, At: s.now().UTC()}
	s.manageMu.Lock()
	nextManage := s.openSaveManage.clone()
	if nextManage.Locks[manageKey] == nil {
		nextManage.Locks[manageKey] = make(map[string]openSaveManageLock)
	}
	nextManage.Locks[manageKey][snapshotID] = lock
	if err := s.persistOpenSaveManage(nextManage); err != nil {
		s.manageMu.Unlock()
		s.openSaveMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist OpenSave lock")
		return
	}
	s.openSaveManage = nextManage
	s.manageMu.Unlock()
	s.openSaveMu.Unlock()

	s.recordOpenSaveManageEvent(eventSnapshotLock, note, kind, gameID, snapshotID)
	writeJSON(w, http.StatusOK, map[string]any{"locked": true, "note": note})
}

func (s *server) handleDeleteOpenSaveSnapshotLock(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := openSaveScopeFromRequest(w, r)
	if !ok {
		return
	}
	snapshotID := r.PathValue("snapshot")
	if !validSnapshotID(snapshotID) {
		writeError(w, http.StatusBadRequest, "invalid OpenSave snapshot id")
		return
	}
	manageKey := openSaveManageScopeKey(kind, gameID)

	s.openSaveMu.Lock()
	stored, exists := s.openSaveState.Scopes[key]
	if !exists {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusNotFound, "OpenSave scope not found")
		return
	}
	if _, exists := stored.Snapshots[snapshotID]; !exists {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusNotFound, "OpenSave snapshot not found")
		return
	}
	s.manageMu.Lock()
	nextManage := s.openSaveManage.clone()
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
		if err := s.persistOpenSaveManage(nextManage); err != nil {
			s.manageMu.Unlock()
			s.openSaveMu.Unlock()
			writeError(w, http.StatusInternalServerError, "cannot persist OpenSave lock removal")
			return
		}
		s.openSaveManage = nextManage
	}
	s.manageMu.Unlock()
	s.openSaveMu.Unlock()

	if removed {
		s.recordOpenSaveManageEvent(eventSnapshotUnlock, "", kind, gameID, snapshotID)
	}
	w.WriteHeader(http.StatusNoContent)
}

type openSaveExportManifest struct {
	Type       string                  `json:"type"`
	GameID     string                  `json:"gameId"`
	Head       string                  `json:"head,omitempty"`
	ExportedAt time.Time               `json:"exportedAt"`
	Versions   []openSaveExportVersion `json:"versions"`
}

type openSaveExportVersion struct {
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

func (s *server) handleExportOpenSaveScope(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := openSaveScopeFromRequest(w, r)
	if !ok {
		return
	}
	s.openSaveMu.Lock()
	stored, exists := s.openSaveState.Scopes[key]
	if !exists {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusNotFound, "OpenSave scope not found")
		return
	}
	snapshots := make([]openSaveScopeSnapshot, 0, len(stored.Snapshots))
	for _, snapshot := range stored.Snapshots {
		snapshots = append(snapshots, snapshot)
	}
	head := stored.Head
	locks := s.openSaveLocks(kind, gameID)
	s.openSaveMu.Unlock()

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
	manifest := openSaveExportManifest{
		Type:       kind,
		GameID:     gameID,
		Head:       head,
		ExportedAt: time.Now().UTC(),
		Versions:   make([]openSaveExportVersion, 0, len(snapshots)),
	}
	for _, snapshot := range snapshots {
		entry := openSaveExportVersion{
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
			path, valid := s.openSavePath(kind, snapshot.Archive)
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
					slog.Warn("write OpenSave export archive", "type", kind, "name", snapshot.Archive, "error", createErr)
					return
				}
				if _, copyErr := io.Copy(part, file); copyErr != nil {
					_ = file.Close()
					slog.Warn("copy OpenSave export archive", "type", kind, "name", snapshot.Archive, "error", copyErr)
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
		slog.Warn("write OpenSave export manifest", "error", err)
		return
	}
	if err := json.NewEncoder(part).Encode(manifest); err != nil {
		slog.Warn("encode OpenSave export manifest", "error", err)
		return
	}
	if err := writer.Close(); err != nil {
		slog.Warn("close OpenSave export", "error", err)
	}
}

func (s *server) openSaveRetentionSettings() openSaveRetentionSettings {
	s.manageMu.Lock()
	defer s.manageMu.Unlock()
	return s.openSaveManage.Retention.clone()
}

// openSaveRetentionResponse exposes the settings both directly and under a
// "retention" key. The direct form is convenient for the endpoint, while the
// nested form matches the fragment stored in opensave-manage.json.
type openSaveRetentionResponse struct {
	openSaveRetentionSettings
	Retention openSaveRetentionSettings `json:"retention"`
}

func writeOpenSaveRetention(w http.ResponseWriter, status int, settings openSaveRetentionSettings) {
	writeJSON(w, status, openSaveRetentionResponse{
		openSaveRetentionSettings: settings,
		Retention:                 settings,
	})
}

func (s *server) handleGetOpenSaveRetention(w http.ResponseWriter, _ *http.Request) {
	writeOpenSaveRetention(w, http.StatusOK, s.openSaveRetentionSettings())
}

func decodeOpenSaveRetentionRequest(w http.ResponseWriter, r *http.Request) (openSaveRetentionSettings, bool) {
	var raw json.RawMessage
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMetadataBytes))
	if err := decoder.Decode(&raw); err != nil {
		writeError(w, http.StatusBadRequest, "invalid OpenSave retention settings")
		return openSaveRetentionSettings{}, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid OpenSave retention settings")
		return openSaveRetentionSettings{}, false
	}
	var wrapper struct {
		Retention *openSaveRetentionSettings `json:"retention"`
	}
	if err := json.Unmarshal(raw, &wrapper); err == nil && wrapper.Retention != nil {
		return *wrapper.Retention, true
	}
	var settings openSaveRetentionSettings
	if err := json.Unmarshal(raw, &settings); err != nil {
		writeError(w, http.StatusBadRequest, "invalid OpenSave retention settings")
		return openSaveRetentionSettings{}, false
	}
	return settings, true
}

func (s *server) handlePutOpenSaveRetention(w http.ResponseWriter, r *http.Request) {
	settings, ok := decodeOpenSaveRetentionRequest(w, r)
	if !ok {
		return
	}
	if settings.Games == nil {
		settings.Games = make(map[string]openSaveRetentionPolicy)
	}
	if settings.Default.KeepVersions == 0 && settings.Default.KeepDays == 0 {
		settings.Default.KeepVersions = 20
	}
	if !settings.valid() {
		writeError(w, http.StatusBadRequest, "invalid OpenSave retention settings")
		return
	}

	s.manageMu.Lock()
	if reflect.DeepEqual(settings, s.openSaveManage.Retention) {
		current := s.openSaveManage.Retention.clone()
		s.manageMu.Unlock()
		writeOpenSaveRetention(w, http.StatusOK, current)
		return
	}
	nextManage := s.openSaveManage.clone()
	nextManage.Retention = settings.clone()
	if err := s.persistOpenSaveManage(nextManage); err != nil {
		s.manageMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist OpenSave retention settings")
		return
	}
	s.openSaveManage = nextManage
	s.manageMu.Unlock()

	detail := "opensave_retention="
	if encoded, err := json.Marshal(settings); err == nil {
		detail += string(encoded)
	}
	s.mu.Lock()
	s.recordEventLocked(event{Kind: eventSettings, Detail: detail})
	s.mu.Unlock()
	writeOpenSaveRetention(w, http.StatusOK, settings)
}

type openSaveRetentionPreviewScope struct {
	Type    string `json:"type"`
	GameID  string `json:"gameId"`
	GameKey string `json:"gameKey"`
	Enabled bool   `json:"enabled"`
	Removed int    `json:"removed"`
	Bytes   int64  `json:"bytes"`
}

type openSaveOrphanArchive struct {
	Type       string    `json:"type"`
	Name       string    `json:"name"`
	GameID     string    `json:"gameId,omitempty"`
	ModifiedAt time.Time `json:"modifiedAt"`
	Size       int64     `json:"size"`
}

type openSaveRetentionPreview struct {
	Scopes      []openSaveRetentionPreviewScope `json:"scopes"`
	Orphans     int                             `json:"orphans"`
	OrphanBytes int64                           `json:"orphanBytes"`
	OrphanFiles []openSaveOrphanArchive         `json:"orphanFiles"`
}

func (s *server) openSaveTypeDirectories() []string {
	types := []string{"pc", "switch"}
	root := s.openSaveNativeDirectory()
	if entries, err := os.ReadDir(root); err == nil {
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), "emulator-") && validOpenSaveType(entry.Name()) {
				types = append(types, entry.Name())
			}
		}
	}
	sort.Strings(types)
	return types
}

// openSaveOrphanArchivesLocked lists regular files that no version of the same
// type references. Dot files are in-progress uploads and are never orphans.
// The caller holds openSaveMu.
func (s *server) openSaveOrphanArchivesLocked(state openSaveNativeState) []openSaveOrphanArchive {
	referenced := make(map[string]struct{})
	for key, stored := range state.Scopes {
		kind, _, ok := parseOpenSaveStateKey(key)
		if !ok {
			continue
		}
		for _, snapshot := range stored.Snapshots {
			referenced[openSaveCatalogKey(kind, snapshot.Archive)] = struct{}{}
		}
	}
	orphans := make([]openSaveOrphanArchive, 0)
	for _, kind := range s.openSaveTypeDirectories() {
		dir := filepath.Join(s.openSaveNativeDirectory(), kind)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			if _, ok := referenced[openSaveCatalogKey(kind, entry.Name())]; ok {
				continue
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			orphan := openSaveOrphanArchive{
				Type:       kind,
				Name:       entry.Name(),
				ModifiedAt: info.ModTime().UTC(),
				Size:       info.Size(),
			}
			if gameID, ok := openSaveArchiveGameID(entry.Name()); ok {
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

func (s *server) handlePreviewOpenSaveRetention(w http.ResponseWriter, _ *http.Request) {
	now := s.now().UTC()
	retention := s.openSaveRetentionSettings()

	s.openSaveMu.Lock()
	nextState := s.openSaveState.clone()
	type removal struct {
		kind     string
		gameID   string
		archives []string
	}
	removals := make([]removal, 0)
	preview := openSaveRetentionPreview{
		Scopes:      make([]openSaveRetentionPreviewScope, 0, len(s.openSaveState.Scopes)),
		OrphanFiles: []openSaveOrphanArchive{},
	}
	for key, stored := range s.openSaveState.Scopes {
		kind, gameID, ok := parseOpenSaveStateKey(key)
		if !ok {
			continue
		}
		gameKey, _ := openSaveGameKey(kind, gameID, stored)
		policy, ok := retention.Games[gameKey]
		if !ok {
			policy = retention.Default
		}
		entry := openSaveRetentionPreviewScope{
			Type:    kind,
			GameID:  gameID,
			GameKey: gameKey,
			Enabled: policy.Enabled,
		}
		if policy.KeepVersions > 0 || policy.KeepDays > 0 {
			nextScope, removedIDs := openSavePruneScopeWithRetention(stored, policy.KeepVersions,
				policy.KeepDays, now, s.openSaveLockedSet(kind, gameID))
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
				if openSaveArchiveReferencedInState(nextState, candidate.kind, archive) {
					continue
				}
				path, ok := s.openSavePath(candidate.kind, archive)
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
	orphans := s.openSaveOrphanArchivesLocked(s.openSaveState)
	preview.Orphans = len(orphans)
	for _, orphan := range orphans {
		preview.OrphanBytes += orphan.Size
	}
	preview.OrphanFiles = orphans
	s.openSaveMu.Unlock()

	sort.Slice(preview.Scopes, func(i, j int) bool {
		if preview.Scopes[i].Type != preview.Scopes[j].Type {
			return preview.Scopes[i].Type < preview.Scopes[j].Type
		}
		return preview.Scopes[i].GameID < preview.Scopes[j].GameID
	})
	writeJSON(w, http.StatusOK, preview)
}

type openSaveMaintenanceReport struct {
	Versions         int
	Archives         int
	Failed           int
	FreedBytes       int64
	OrphanArchives   int
	OrphanBytes      int64
	OrphanRemoved    int
	TombstonesPurged int
}

// runOpenSaveMaintenance purges expired trash, applies automatic retention and
// optionally removes old orphan archives. It is a no-op when no C6 feature has
// been enabled, and dryRun never writes or removes anything.
func (s *server) runOpenSaveMaintenance(dryRun bool, now time.Time) (openSaveMaintenanceReport, error) {
	report := openSaveMaintenanceReport{}
	s.openSaveMu.Lock()

	// Purge trash whose retention window has passed. The tombstone stays.
	s.manageMu.Lock()
	purgeNext := s.openSaveManage.clone()
	for key, tombstone := range purgeNext.Scopes {
		if tombstone.State != openSaveTombstoneDeleted || tombstone.PurgeAfter.IsZero() ||
			now.Before(tombstone.PurgeAfter) {
			continue
		}
		kind, gameID, ok := parseOpenSaveManageScopeKey(key)
		if !ok {
			continue
		}
		if dryRun {
			report.TombstonesPurged++
			continue
		}
		if trashDir, found := s.openSaveTrashDirectoryForTombstone(kind, gameID, tombstone); found {
			if err := s.manageRemoveAll(trashDir); err != nil && !errors.Is(err, os.ErrNotExist) {
				slog.Warn("purge OpenSave trash", "scope", key, "error", err)
				continue
			}
			s.openSavePruneEmptyTrash(kind, gameID)
		}
		tombstone.Head = ""
		tombstone.PurgeAfter = time.Time{}
		purgeNext.Scopes[key] = tombstone
		report.TombstonesPurged++
	}
	if !dryRun && report.TombstonesPurged > 0 {
		if err := s.persistOpenSaveManage(purgeNext); err != nil {
			s.manageMu.Unlock()
			s.openSaveMu.Unlock()
			return report, err
		}
		s.openSaveManage = purgeNext
	}
	retention := s.openSaveManage.Retention.clone()
	s.manageMu.Unlock()

	// Automatic retention. State is committed before archives are unlinked.
	nextState := s.openSaveState.clone()
	type removal struct {
		kind     string
		archives []string
	}
	removals := make([]removal, 0)
	for key, stored := range s.openSaveState.Scopes {
		kind, gameID, ok := parseOpenSaveStateKey(key)
		if !ok {
			continue
		}
		gameKey, _ := openSaveGameKey(kind, gameID, stored)
		policy, ok := retention.Games[gameKey]
		if !ok {
			policy = retention.Default
		}
		if !policy.Enabled || (policy.KeepVersions == 0 && policy.KeepDays == 0) {
			continue
		}
		nextScope, removedIDs := openSavePruneScopeWithRetention(stored, policy.KeepVersions,
			policy.KeepDays, now, s.openSaveLockedSet(kind, gameID))
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
		if err := s.persistOpenSaveState(nextState); err != nil {
			s.openSaveMu.Unlock()
			return report, err
		}
		s.openSaveState = nextState
	}

	// Delete archives that the post-removal state no longer references.
	nextCatalog := s.openSaveCatalog.clone()
	catalogChanged := false
	seenArchives := make(map[string]struct{})
	for _, candidate := range removals {
		for _, archive := range candidate.archives {
			key := openSaveCatalogKey(candidate.kind, archive)
			if _, ok := seenArchives[key]; ok {
				continue
			}
			seenArchives[key] = struct{}{}
			if openSaveArchiveReferencedInState(nextState, candidate.kind, archive) {
				continue
			}
			path, ok := s.openSavePath(candidate.kind, archive)
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
				slog.Warn("delete OpenSave archive during retention", "type", candidate.kind, "name", archive, "error", err)
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
		orphanBase = s.openSaveState
	}
	orphans := s.openSaveOrphanArchivesLocked(orphanBase)
	report.OrphanArchives = len(orphans)
	for _, orphan := range orphans {
		report.OrphanBytes += orphan.Size
	}
	if retention.OrphanCleanup && !dryRun {
		for _, orphan := range orphans {
			if now.Sub(orphan.ModifiedAt) <= openSaveOrphanGrace {
				continue
			}
			path, ok := s.openSavePath(orphan.Type, orphan.Name)
			if !ok {
				continue
			}
			if err := s.manageRemove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				slog.Warn("delete orphan OpenSave archive", "type", orphan.Type, "name", orphan.Name, "error", err)
				continue
			}
			report.OrphanRemoved++
			report.FreedBytes += orphan.Size
			catalogKey := openSaveCatalogKey(orphan.Type, orphan.Name)
			if _, ok := nextCatalog.Files[catalogKey]; ok {
				delete(nextCatalog.Files, catalogKey)
				catalogChanged = true
			}
		}
	}
	if catalogChanged {
		if err := s.persistOpenSaveCatalog(nextCatalog); err != nil {
			slog.Warn("persist OpenSave retention catalog cleanup", "error", err)
		} else {
			s.openSaveCatalog = nextCatalog
		}
	}
	s.openSaveMu.Unlock()

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

// openSaveManageCounts returns effective C6 gauge values. Locks that reference
// a missing scope or snapshot are retained on disk but are not counted.
func (s *server) openSaveManageCounts() (deleted, retired, locked int) {
	s.openSaveMu.Lock()
	defer s.openSaveMu.Unlock()
	s.manageMu.Lock()
	defer s.manageMu.Unlock()
	for _, tombstone := range s.openSaveManage.Scopes {
		switch tombstone.State {
		case openSaveTombstoneDeleted:
			deleted++
		case openSaveTombstoneRetired:
			retired++
		}
	}
	for key, locks := range s.openSaveManage.Locks {
		kind, gameID, ok := parseOpenSaveManageScopeKey(key)
		if !ok {
			continue
		}
		stored, exists := s.openSaveState.Scopes[openSaveScopeKey(kind, gameID)]
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

func (s *server) openSaveOrphanStats() (int, int64) {
	s.openSaveMu.Lock()
	defer s.openSaveMu.Unlock()
	orphans := s.openSaveOrphanArchivesLocked(s.openSaveState)
	var bytes int64
	for _, orphan := range orphans {
		bytes += orphan.Size
	}
	return len(orphans), bytes
}

func (s *server) openSaveLockedSet(kind, gameID string) map[string]struct{} {
	locks := s.openSaveLocks(kind, gameID)
	result := make(map[string]struct{}, len(locks))
	for id := range locks {
		result[id] = struct{}{}
	}
	return result
}
