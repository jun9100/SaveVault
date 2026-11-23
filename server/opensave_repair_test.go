// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRepairOpenSaveHeadsMovesBackToASurvivingArchive(t *testing.T) {
	server, _ := newTestServer(t)
	const kind, game = "switch", "switch-01008d7016438000"
	key := kind + "\x00" + game

	writeArchive := func(name string) string {
		path, ok := server.openSavePath(kind, name)
		if !ok {
			t.Fatalf("bad archive name %q", name)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		return name
	}

	oldest := writeArchive(game + "__main__snap_1.zip")
	newest := game + "__main__snap_2.zip" // never written: the head dangles
	now := time.Now().UTC()
	server.openSaveState.Scopes[key] = openSaveScopeState{
		Head: "snap_2",
		Snapshots: map[string]openSaveScopeSnapshot{
			"snap_1": {
				openSaveScopeCandidate: openSaveScopeCandidate{ID: "snap_1", Archive: oldest, ContentDigest: digestOf(oldest)},
				ReceivedAt:             now,
			},
			"snap_2": {
				openSaveScopeCandidate: openSaveScopeCandidate{ID: "snap_2", Archive: newest, Parent: "snap_1", ContentDigest: digestOf(newest)},
				ReceivedAt:             now,
			},
		},
	}

	repaired, err := server.repairOpenSaveHeads()
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if repaired != 1 {
		t.Fatalf("repaired = %d, want 1", repaired)
	}
	if head := server.openSaveState.Scopes[key].Head; head != "snap_1" {
		t.Fatalf("head = %q, want snap_1", head)
	}
}

func TestRepairOpenSaveHeadsClearsAHeadWithNoSurvivingArchive(t *testing.T) {
	server, _ := newTestServer(t)
	const kind, game = "switch", "switch-01000sdummy0000"
	key := kind + "\x00" + game
	server.openSaveState.Scopes[key] = openSaveScopeState{
		Head: "snap_1",
		Snapshots: map[string]openSaveScopeSnapshot{
			"snap_1": {
				openSaveScopeCandidate: openSaveScopeCandidate{ID: "snap_1", Archive: game + "__main__snap_1.zip"},
				ReceivedAt:             time.Now().UTC(),
			},
		},
	}

	repaired, err := server.repairOpenSaveHeads()
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if repaired != 1 {
		t.Fatalf("repaired = %d, want 1", repaired)
	}
	if head := server.openSaveState.Scopes[key].Head; head != "" {
		t.Fatalf("head = %q, want empty", head)
	}
	if len(server.openSaveState.Scopes[key].Snapshots) != 1 {
		t.Fatal("snapshots must be kept for history")
	}
}

func TestRepairOpenSaveHeadsLeavesHealthyScopesAlone(t *testing.T) {
	server, _ := newTestServer(t)
	const kind, game = "switch", "switch-01008d7016438000"
	key := kind + "\x00" + game
	name := game + "__main__snap_1.zip"
	path, _ := server.openSavePath(kind, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	server.openSaveState.Scopes[key] = openSaveScopeState{
		Head: "snap_1",
		Snapshots: map[string]openSaveScopeSnapshot{
			"snap_1": {openSaveScopeCandidate: openSaveScopeCandidate{ID: "snap_1", Archive: name}},
		},
	}

	repaired, err := server.repairOpenSaveHeads()
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if repaired != 0 {
		t.Fatalf("repaired = %d, want 0", repaired)
	}
	if head := server.openSaveState.Scopes[key].Head; head != "snap_1" {
		t.Fatalf("head = %q, want snap_1", head)
	}
}

func digestOf(name string) string {
	return "sha256:" + name
}
