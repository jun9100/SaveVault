// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRepairSaveVaultHeadsMovesBackToASurvivingArchive(t *testing.T) {
	server, _ := newTestServer(t)
	const kind, game = "switch", "switch-01008d7016438000"
	key := kind + "\x00" + game

	writeArchive := func(name string) string {
		path, ok := server.saveVaultPath(kind, name)
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
	server.saveVaultState.Scopes[key] = saveVaultScopeState{
		Head: "snap_2",
		Snapshots: map[string]saveVaultScopeSnapshot{
			"snap_1": {
				saveVaultScopeCandidate: saveVaultScopeCandidate{ID: "snap_1", Archive: oldest, ContentDigest: digestOf(oldest)},
				ReceivedAt:             now,
			},
			"snap_2": {
				saveVaultScopeCandidate: saveVaultScopeCandidate{ID: "snap_2", Archive: newest, Parent: "snap_1", ContentDigest: digestOf(newest)},
				ReceivedAt:             now,
			},
		},
	}

	repaired, err := server.repairSaveVaultHeads()
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if repaired != 1 {
		t.Fatalf("repaired = %d, want 1", repaired)
	}
	if head := server.saveVaultState.Scopes[key].Head; head != "snap_1" {
		t.Fatalf("head = %q, want snap_1", head)
	}
}

func TestRepairSaveVaultHeadsClearsAHeadWithNoSurvivingArchive(t *testing.T) {
	server, _ := newTestServer(t)
	const kind, game = "switch", "switch-01000sdummy0000"
	key := kind + "\x00" + game
	server.saveVaultState.Scopes[key] = saveVaultScopeState{
		Head: "snap_1",
		Snapshots: map[string]saveVaultScopeSnapshot{
			"snap_1": {
				saveVaultScopeCandidate: saveVaultScopeCandidate{ID: "snap_1", Archive: game + "__main__snap_1.zip"},
				ReceivedAt:             time.Now().UTC(),
			},
		},
	}

	repaired, err := server.repairSaveVaultHeads()
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if repaired != 1 {
		t.Fatalf("repaired = %d, want 1", repaired)
	}
	if head := server.saveVaultState.Scopes[key].Head; head != "" {
		t.Fatalf("head = %q, want empty", head)
	}
	if len(server.saveVaultState.Scopes[key].Snapshots) != 1 {
		t.Fatal("snapshots must be kept for history")
	}
}

func TestRepairSaveVaultHeadsLeavesHealthyScopesAlone(t *testing.T) {
	server, _ := newTestServer(t)
	const kind, game = "switch", "switch-01008d7016438000"
	key := kind + "\x00" + game
	name := game + "__main__snap_1.zip"
	path, _ := server.saveVaultPath(kind, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	server.saveVaultState.Scopes[key] = saveVaultScopeState{
		Head: "snap_1",
		Snapshots: map[string]saveVaultScopeSnapshot{
			"snap_1": {saveVaultScopeCandidate: saveVaultScopeCandidate{ID: "snap_1", Archive: name}},
		},
	}

	repaired, err := server.repairSaveVaultHeads()
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if repaired != 0 {
		t.Fatalf("repaired = %d, want 0", repaired)
	}
	if head := server.saveVaultState.Scopes[key].Head; head != "snap_1" {
		t.Fatalf("head = %q, want snap_1", head)
	}
}

func digestOf(name string) string {
	return "sha256:" + name
}
