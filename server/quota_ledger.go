// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// initUsedBytesLedger computes the exact initial vault byte usage once at
// startup so subsequent quota checks run in O(1) via atomic loads instead of
// walking the entire data directory tree on every upload.
func (s *server) initUsedBytesLedger() {
	s.reconcileUsedBytes()
}

// usedBytes returns the vault's current byte usage in O(1) once initialized.
func (s *server) usedBytes() int64 {
	if !s.usedBytesInit.Load() {
		return s.reconcileUsedBytes()
	}
	v := s.usedBytesVal.Load()
	if v < 0 {
		return 0
	}
	return v
}

// adjustUsedBytes updates the O(1) usage ledger by delta (positive on write,
// negative on deletion). Clamped at zero so concurrent deletions never go
// negative.
func (s *server) adjustUsedBytes(delta int64) {
	if delta == 0 || !s.usedBytesInit.Load() {
		return
	}
	for {
		cur := s.usedBytesVal.Load()
		next := cur + delta
		if next < 0 {
			next = 0
		}
		if s.usedBytesVal.CompareAndSwap(cur, next) {
			s.usedBytesUpdatedAt.Store(s.now().UTC().UnixNano())
			return
		}
	}
}

// reconcileUsedBytes scans the vault directory tree and resets the atomic
// usage ledger to the exact on-disk total. Used at startup, after bulk
// prune/import operations, and on explicit admin reconciliation.
func (s *server) reconcileUsedBytes() int64 {
	actual := vaultDirectoryUsedBytes(s.dataDir)
	s.usedBytesVal.Store(actual)
	s.usedBytesInit.Store(true)
	s.usedBytesReconciles.Add(1)
	s.usedBytesUpdatedAt.Store(s.now().UTC().UnixNano())
	return actual
}

func vaultDirectoryUsedBytes(dataDir string) int64 {
	var used int64
	for _, sub := range []string{"objects", "savevault-native", "savevault", "savevault-meta", "chunks"} {
		_ = filepath.Walk(filepath.Join(dataDir, sub), func(_ string, info os.FileInfo, err error) error {
			if err != nil || info == nil || !info.Mode().IsRegular() {
				return nil
			}
			// Ignore staged temporary upload files (.upload-*, .part, etc.) so
			// in-flight writes are not double-counted against additional.
			if strings.HasPrefix(info.Name(), ".") {
				return nil
			}
			used += info.Size()
			return nil
		})
	}
	if info, err := os.Stat(filepath.Join(dataDir, "dashboard.json")); err == nil && info.Mode().IsRegular() {
		used += info.Size()
	}
	return used
}

func (s *server) handleReconcileQuota(w http.ResponseWriter, _ *http.Request) {
	actual := s.reconcileUsedBytes()
	writeJSON(w, http.StatusOK, map[string]any{
		"used_bytes":  actual,
		"quota_bytes": s.getQuotaBytes(),
		"reconciles":  s.usedBytesReconciles.Load(),
	})
}
