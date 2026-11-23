// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// snapshotStamp matches the snapshot ids clients generate: `snap_<millis>`,
// optionally followed by `--dev-<device>`.
var snapshotStamp = regexp.MustCompile(`^snap_(\d+)(?:--dev-[0-9a-zA-Z]+)?$`)

type rebuildReport struct {
	Scanned  int      `json:"scanned"`
	Rebuilt  int      `json:"rebuilt"`
	Orphaned int      `json:"orphaned_archives"`
	Details  []string `json:"details,omitempty"`
}

// rebuildSaveVaultHeadsFromArchives restores a current version for scopes whose
// head is gone but whose archives are still on disk. It is the recovery path
// after a data copy, a lost state file, or a bug that cleared a head: the
// archive name itself carries the game id and the snapshot id, so the newest
// archive per game becomes the head again with its digest recomputed from the
// bytes.
//
// Nothing is deleted. A scope that already has a readable head is left alone.
func (s *server) rebuildSaveVaultHeadsFromArchives(dryRun bool) (rebuildReport, error) {
	report := rebuildReport{}
	type candidate struct {
		kind, gameID, snapshot, archive, digest string
		stamp                                   int64
	}
	newest := make(map[string]candidate) // kind\x00gameID -> newest archive

	root := s.saveVaultNativeDirectory()
	kinds, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		return report, err
	}
	for _, entry := range kinds {
		if !entry.IsDir() || !validSaveVaultType(entry.Name()) {
			continue
		}
		kind := entry.Name()
		names, err := os.ReadDir(filepath.Join(root, kind))
		if err != nil {
			continue
		}
		for _, file := range names {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".zip") {
				continue
			}
			parts := strings.SplitN(strings.TrimSuffix(file.Name(), ".zip"), "__", 3)
			if len(parts) != 3 {
				continue
			}
			gameID, archiveName := parts[0], file.Name()
			match := snapshotStamp.FindStringSubmatch(parts[2])
			if match == nil {
				continue
			}
			millis, err := strconv.ParseInt(match[1], 10, 64)
			if err != nil {
				continue
			}
			key := kind + "\x00" + gameID
			if current, ok := newest[key]; ok && current.stamp >= millis {
				continue
			}
			digest, err := s.saveVaultArchiveDigestLocked(kind, archiveName)
			if err != nil {
				report.Orphaned++
				continue
			}
			newest[key] = candidate{kind: kind, gameID: gameID, snapshot: parts[2], archive: archiveName, digest: digest, stamp: millis}
		}
	}

	report.Scanned = len(newest)
	if dryRun {
		for _, item := range newest {
			report.Details = append(report.Details, item.kind+"/"+item.gameID+" -> "+item.snapshot)
		}
		sort.Strings(report.Details)
		return report, nil
	}

	s.saveVaultMu.Lock()
	defer s.saveVaultMu.Unlock()
	next := s.saveVaultState.clone()
	for key, item := range newest {
		stored, exists := s.saveVaultState.Scopes[key]
		if exists && stored.Head != "" {
			if snapshot, ok := stored.Snapshots[stored.Head]; ok && s.saveVaultArchiveExistsLocked(item.kind, snapshot.Archive) {
				continue
			}
		}
		scope := stored
		if scope.Snapshots == nil {
			scope.Snapshots = map[string]saveVaultScopeSnapshot{}
		} else {
			scope = stored.clone()
		}
		if _, present := scope.Snapshots[item.snapshot]; !present {
			scope.Snapshots[item.snapshot] = saveVaultScopeSnapshot{
				saveVaultScopeCandidate: saveVaultScopeCandidate{
					ID:            item.snapshot,
					Archive:       item.archive,
					ContentDigest: item.digest,
					CreatedAt:     time.UnixMilli(item.stamp).UTC(),
				},
				ReceivedAt: time.Now().UTC(),
			}
		}
		scope.Head = item.snapshot
		next.Scopes[key] = scope
		report.Rebuilt++
		report.Details = append(report.Details, item.kind+"/"+item.gameID+" -> "+item.snapshot)
	}
	if report.Rebuilt == 0 {
		return report, nil
	}
	if err := s.persistSaveVaultState(next); err != nil {
		return report, err
	}
	s.saveVaultState = next
	sort.Strings(report.Details)
	slog.Warn("rebuilt SaveVault heads from archives", "rebuilt", report.Rebuilt, "scanned", report.Scanned)
	return report, nil
}

func (s *server) handlePreviewRebuildSaveVaultHeads(w http.ResponseWriter, r *http.Request) {
	report, err := s.rebuildSaveVaultHeadsFromArchives(true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot scan archives")
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *server) handleRebuildSaveVaultHeads(w http.ResponseWriter, r *http.Request) {
	report, err := s.rebuildSaveVaultHeadsFromArchives(false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot rebuild heads")
		return
	}
	writeJSON(w, http.StatusOK, report)
}
