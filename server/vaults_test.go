// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrateLegacyVaultLayoutMovesStateIntoDefaultVault(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(`{"scopes":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "objects", "ab"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := migrateLegacyVaultLayout(root); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	moved := filepath.Join(root, "vaults", defaultVaultID)
	if _, err := os.Stat(filepath.Join(moved, "state.json")); err != nil {
		t.Fatalf("state.json should live in the default vault: %v", err)
	}
	if _, err := os.Stat(filepath.Join(moved, "objects", "ab")); err != nil {
		t.Fatalf("objects should live in the default vault: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "state.json")); !os.IsNotExist(err) {
		t.Fatalf("legacy state.json should be gone, got %v", err)
	}

	// A second start must not touch the migrated files.
	if err := os.WriteFile(filepath.Join(moved, "dashboard.json"), []byte(`{"scopes":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyVaultLayout(root); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(moved, "dashboard.json")); err != nil {
		t.Fatalf("second migrate must leave the vault alone: %v", err)
	}
}

func TestVaultsDoNotSeeEachOtherSaves(t *testing.T) {
	root := t.TempDir()
	registry, err := newVaultRegistry(root, "default-token", 8<<20, serverOptions{})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	t.Cleanup(registry.Close)
	if _, err := registry.vaultWithToken("acme", "acme-token"); err != nil {
		t.Fatalf("create vault: %v", err)
	}
	handler := registry.handler()

	const game = "switch-01008D7016438000"
	archive := fmt.Sprintf("%s__main__snap_1.zip", game)
	archiveBytes := []byte("native archive bytes")
	digest := sha256.Sum256(archiveBytes)

	// Upload the archive and publish a head inside the acme vault.
	put := func(token, path string, body []byte, headers map[string]string) int {
		request := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Code
	}
	if code := put("acme-token", "/v1/savevault/files/emulator-android/"+archive, archiveBytes, nil); code != http.StatusCreated {
		t.Fatalf("archive upload status = %d", code)
	}
	candidate, _ := json.Marshal(map[string]any{
		"id":            "snap_1",
		"archive":       archive,
		"parent":        "",
		"contentDigest": hex.EncodeToString(digest[:]),
		"createdAt":     time.Now().UTC().Format(time.RFC3339Nano),
	})
	snapshotRequest := httptest.NewRequest(http.MethodPut, "/v1/savevault/scopes/emulator-android/"+game+"/snapshots/snap_1", bytes.NewReader(candidate))
	snapshotRequest.Header.Set("Authorization", "Bearer acme-token")
	snapshotRequest.Header.Set("If-Match", `""`)
	snapshotRequest.Header.Set("Content-Type", "application/json")
	snapshotRecorder := httptest.NewRecorder()
	handler.ServeHTTP(snapshotRecorder, snapshotRequest)
	if snapshotRecorder.Code != http.StatusCreated {
		t.Fatalf("snapshot status = %d body=%s", snapshotRecorder.Code, snapshotRecorder.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/savevault/scopes/emulator-android/"+game, nil)
	request.Header.Set("Authorization", "Bearer acme-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("scope read status = %d", recorder.Code)
	}
	var scope saveVaultScopeResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &scope); err != nil {
		t.Fatalf("decode scope: %v", err)
	}
	// The same request in the default vault must be empty: a tenant never sees
	// another tenant's head.
	request = httptest.NewRequest(http.MethodGet, "/v1/savevault/scopes/emulator-android/"+game, nil)
	request.Header.Set("Authorization", "Bearer default-token")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("default scope read status = %d", recorder.Code)
	}
	var other saveVaultScopeResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &other); err != nil {
		t.Fatalf("decode other scope: %v", err)
	}
	if other.Head != nil {
		t.Fatalf("default vault must not see the acme head, got %+v", other.Head)
	}
	if scope.Head == nil {
		t.Fatal("acme vault should have a head")
	}
}
