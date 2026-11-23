// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"strings"
)

// repairSaveVaultHeads walks every native scope whose head points at a snapshot
// whose archive is no longer on disk and moves the head back to the newest
// ancestor that still has one. Without this a scope advertises a head the
// download endpoint answers 404 for, and a client can neither read nor replace
// it without deleting the whole scope.
//
// A head with no surviving ancestor is cleared: the snapshots stay listed, but
// the scope no longer claims a current version.
func (s *server) repairSaveVaultHeads() (int, error) {
	s.saveVaultMu.Lock()
	defer s.saveVaultMu.Unlock()

	next := s.saveVaultState.clone()
	repaired := 0
	for key, stored := range s.saveVaultState.Scopes {
		kind, _, _ := strings.Cut(key, "\x00")
		head := stored.Head
		seen := make(map[string]struct{})
		for head != "" {
			if _, repeat := seen[head]; repeat {
				head = ""
				break
			}
			seen[head] = struct{}{}
			snapshot, exists := stored.Snapshots[head]
			if !exists {
				head = ""
				break
			}
			if s.saveVaultArchiveExistsLocked(kind, snapshot.Archive) {
				break
			}
			head = snapshot.Parent
		}
		if head == stored.Head {
			continue
		}
		scope := stored.clone()
		scope.Head = head
		next.Scopes[key] = scope
		repaired++
		slog.Warn("repaired SaveVault head with a missing archive",
			"scope", key, "previous_head", stored.Head, "head", head)
	}
	if repaired == 0 {
		return 0, nil
	}
	if err := s.persistSaveVaultState(next); err != nil {
		return 0, err
	}
	s.saveVaultState = next
	return repaired, nil
}

func (s *server) saveVaultArchiveExistsLocked(kind, name string) bool {
	path, ok := s.saveVaultPath(kind, name)
	if !ok {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false
		}
		return false
	}
	return !info.IsDir()
}
