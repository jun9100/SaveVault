// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

type openSavePruneResponse struct {
	Removed    int   `json:"removed"`
	FreedBytes int64 `json:"freedBytes"`
	Failed     int   `json:"failed"`
	Kept       int   `json:"kept"`
	DryRun     bool  `json:"dryRun"`
}

// openSavePruneScope returns a copy of stored without the normal versions that
// fall outside the newest keep. The head, every conflict copy and every version
// named by a conflict copy are always retained. Removed snapshot IDs are
// returned sorted for deterministic archive cleanup.
func openSavePruneScope(stored openSaveScopeState, keep int,
	extraRetained map[string]struct{}) (openSaveScopeState, []string) {
	return openSavePruneScopeWithRetention(stored, keep, 0, time.Time{}, extraRetained)
}

// openSavePruneScopeWithRetention is the shared prune used by the v1.7.0
// manual endpoint and by C6 automatic retention. extraRetained holds locked
// snapshot IDs, which are never removed and never count toward keep. keepDays
// greater than zero also retains normal versions received within that window.
func openSavePruneScopeWithRetention(stored openSaveScopeState, keep, keepDays int, now time.Time,
	extraRetained map[string]struct{}) (openSaveScopeState, []string) {
	retained := make(map[string]struct{}, len(stored.Snapshots))
	for id := range extraRetained {
		retained[id] = struct{}{}
	}
	if stored.Head != "" {
		retained[stored.Head] = struct{}{}
	}
	normal := make([]openSaveScopeSnapshot, 0, len(stored.Snapshots))
	for id, snapshot := range stored.Snapshots {
		if snapshot.Conflict {
			retained[id] = struct{}{}
			continue
		}
		if _, locked := extraRetained[id]; locked {
			// Locked versions are always kept and do not consume a keep slot.
			continue
		}
		normal = append(normal, snapshot)
	}
	for _, snapshot := range stored.Snapshots {
		if snapshot.Conflict && snapshot.ConflictOf != "" {
			retained[snapshot.ConflictOf] = struct{}{}
		}
	}
	sort.Slice(normal, func(i, j int) bool {
		if !normal[i].ReceivedAt.Equal(normal[j].ReceivedAt) {
			return normal[i].ReceivedAt.After(normal[j].ReceivedAt)
		}
		return normal[i].ID > normal[j].ID
	})
	for index := 0; index < keep && index < len(normal); index++ {
		retained[normal[index].ID] = struct{}{}
	}
	if keepDays > 0 {
		cutoff := now.AddDate(0, 0, -keepDays)
		for _, snapshot := range normal {
			if snapshot.ReceivedAt.After(cutoff) {
				retained[snapshot.ID] = struct{}{}
			}
		}
	}

	next := stored.clone()
	removed := make([]string, 0, len(stored.Snapshots))
	for id := range next.Snapshots {
		if _, ok := retained[id]; ok {
			continue
		}
		delete(next.Snapshots, id)
		removed = append(removed, id)
	}
	sort.Strings(removed)
	return next, removed
}

// openSaveArchiveReferencedInState reports whether any scope of the same type
// in state still names the archive. Prune uses the post-removal state so an
// archive shared by a removed and a retained version is never deleted.
func openSaveArchiveReferencedInState(state openSaveNativeState, kind, name string) bool {
	prefix := kind + "\x00"
	for key, scope := range state.Scopes {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		for _, snapshot := range scope.Snapshots {
			if snapshot.Archive == name {
				return true
			}
		}
	}
	return false
}

func (s *server) handleOpenSaveScopePrune(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := openSaveScopeFromRequest(w, r)
	if !ok {
		return
	}
	var request struct {
		Keep   int  `json:"keep"`
		DryRun bool `json:"dryRun"`
	}
	if !decodeAdminJSON(w, r, &request) {
		return
	}
	if request.Keep < 1 || request.Keep > 1000 {
		writeError(w, http.StatusBadRequest, "keep must be between 1 and 1000")
		return
	}

	s.openSaveMu.Lock()
	stored, exists := s.openSaveState.Scopes[key]
	if !exists {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusNotFound, "OpenSave scope not found")
		return
	}
	nextScope, removedIDs := openSavePruneScope(stored, request.Keep, s.openSaveLockedSet(kind, gameID))
	nextState := s.openSaveState.clone()
	nextState.Scopes[key] = nextScope
	response := openSavePruneResponse{
		Removed: len(removedIDs),
		Kept:    len(nextScope.Snapshots),
		DryRun:  request.DryRun,
	}

	// An archive can back more than one version, so collect each candidate once
	// and only after checking the state that would remain.
	archives := make([]string, 0, len(removedIDs))
	seen := make(map[string]struct{}, len(removedIDs))
	for _, id := range removedIDs {
		archive := stored.Snapshots[id].Archive
		if archive == "" {
			continue
		}
		if _, ok := seen[archive]; ok {
			continue
		}
		seen[archive] = struct{}{}
		if !openSaveArchiveReferencedInState(nextState, kind, archive) {
			archives = append(archives, archive)
		}
	}
	sort.Strings(archives)

	if request.DryRun {
		for _, archive := range archives {
			path, ok := s.openSavePath(kind, archive)
			if !ok {
				continue
			}
			if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
				response.FreedBytes += info.Size()
			}
		}
		s.openSaveMu.Unlock()
		writeJSON(w, http.StatusOK, response)
		return
	}

	// State is authoritative: commit it before unlinking any archive so a
	// persistence failure cannot leave references to files that are gone.
	if err := s.persistOpenSaveState(nextState); err != nil {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusInternalServerError, "cannot persist OpenSave scope state")
		return
	}
	s.openSaveState = nextState

	nextCatalog := s.openSaveCatalog.clone()
	catalogChanged := false
	for _, archive := range archives {
		path, ok := s.openSavePath(kind, archive)
		if !ok {
			continue
		}
		size := int64(0)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			size = info.Size()
		}
		err := os.Remove(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("delete OpenSave archive during prune", "type", kind, "name", archive, "error", err)
			response.Failed++
			continue
		}
		// A missing archive is already in the desired state; it frees no bytes
		// but still drops its stale catalog entry.
		if err == nil {
			response.FreedBytes += size
		}
		catalogKey := openSaveCatalogKey(kind, archive)
		if _, exists := nextCatalog.Files[catalogKey]; exists {
			delete(nextCatalog.Files, catalogKey)
			catalogChanged = true
		}
	}
	if catalogChanged {
		if err := s.persistOpenSaveCatalog(nextCatalog); err != nil {
			slog.Warn("persist OpenSave archive cleanup", "type", kind, "error", err)
		} else {
			s.openSaveCatalog = nextCatalog
		}
	}
	s.openSaveMu.Unlock()

	detail := fmt.Sprintf("removed %d snapshots, freed %d bytes", response.Removed, response.FreedBytes)
	if response.Failed > 0 {
		detail += fmt.Sprintf(", failed %d archives", response.Failed)
	}
	s.mu.Lock()
	s.recordEventLocked(event{
		Kind:   eventPruneScope,
		Scope:  openSaveEventScope(kind, gameID),
		Detail: detail,
	})
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, response)
}
