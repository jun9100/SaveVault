// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeArchiveFile(t *testing.T, s *server, kind, name, body string) {
	t.Helper()
	path, ok := s.openSavePath(kind, name)
	if !ok {
		t.Fatalf("bad archive name %q", name)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRebuildOpenSaveHeadsAdoptsTheNewestArchive(t *testing.T) {
	server, _ := newTestServer(t)
	const kind, game = "switch", "switch-01008d7016438000"
	writeArchiveFile(t, server, kind, game+"__main__snap_1000.zip", "old")
	writeArchiveFile(t, server, kind, game+"__main__snap_2000--dev-abc.zip", "new")

	report, err := server.rebuildOpenSaveHeadsFromArchives(false)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if report.Rebuilt != 1 {
		t.Fatalf("rebuilt = %d, want 1", report.Rebuilt)
	}
	stored := server.openSaveState.Scopes[kind+"\x00"+game]
	if stored.Head != "snap_2000--dev-abc" {
		t.Fatalf("head = %q", stored.Head)
	}
	if stored.Snapshots[stored.Head].ContentDigest == "" {
		t.Fatal("the rebuilt snapshot must carry the archive digest")
	}
}

func TestRebuildOpenSaveHeadsLeavesAReadableHeadAlone(t *testing.T) {
	server, _ := newTestServer(t)
	const kind, game = "switch", "switch-0100000000010000"
	name := game + "__main__snap_1000.zip"
	writeArchiveFile(t, server, kind, name, "x")
	server.openSaveState.Scopes[kind+"\x00"+game] = openSaveScopeState{
		Head: "snap_1000",
		Snapshots: map[string]openSaveScopeSnapshot{
			"snap_1000": {openSaveScopeCandidate: openSaveScopeCandidate{ID: "snap_1000", Archive: name}},
		},
	}

	report, err := server.rebuildOpenSaveHeadsFromArchives(false)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if report.Rebuilt != 0 {
		t.Fatalf("rebuilt = %d, want 0", report.Rebuilt)
	}
	if head := server.openSaveState.Scopes[kind+"\x00"+game].Head; head != "snap_1000" {
		t.Fatalf("head = %q", head)
	}
}
