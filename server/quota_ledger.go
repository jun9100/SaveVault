// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// usedBytes returns the vault's current byte usage in O(1).
func (s *server) usedBytes() int64 {
	return s.usedBytesVal.Load()
}

// adjustUsedBytes updates the O(1) usage ledger by delta (positive on write,
// negative on deletion) and fires quota_warning once when committed usage
// crosses 80% of the configured quota.
func (s *server) adjustUsedBytes(delta int64) {
	if delta == 0 {
		return
	}
	var prev, next int64
	for {
		prev = s.usedBytesVal.Load()
		next = prev + delta
		if next < 0 {
			slog.Warn("quota ledger underflow; clamping to zero", "vault", s.vaultID, "prev", prev, "delta", delta)
			next = 0
		}
		if s.usedBytesVal.CompareAndSwap(prev, next) {
			break
		}
	}
	if delta > 0 {
		if quota := s.getQuotaBytes(); quota > 0 {
			warnThreshold := quota * 80 / 100
			if prev < warnThreshold && next >= warnThreshold {
				s.notifyWebhooks(event{
					Time:   s.now().UTC(),
					Kind:   eventQuotaWarning,
					Detail: fmt.Sprintf("usage reached %d%% (%d / %d bytes)", next*100/quota, next, quota),
				})
			}
		}
	}
}

// reconcileUsedBytes scans the vault directory tree under s.quotaMu so
// concurrent uploads cannot have their increments overwritten by a stale scan.
func (s *server) reconcileUsedBytes() int64 {
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	actual := vaultDirectoryUsedBytes(s.dataDir)
	s.usedBytesVal.Store(actual)
	s.usedBytesReconciles.Add(1)
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
