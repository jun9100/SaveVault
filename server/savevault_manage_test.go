// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func newC6TestServer(t *testing.T) (*server, *httptest.Server) {
	t.Helper()
	s, err := newServer(t.TempDir(), testToken, 4*1024*1024, testAdminToken)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s.handler())
	return s, httpServer
}

func adminDoIfMatch(t *testing.T, method, url string, body any, ifMatch string) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth("admin", testAdminToken)
	request.Header.Set(csrfHeader, "1")
	if ifMatch != "" {
		request.Header.Set("If-Match", ifMatch)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return response, contents
}

func deleteSaveVaultScope(t *testing.T, baseURL, kind, game, ifMatch string) (*http.Response, []byte) {
	t.Helper()
	return adminDoIfMatch(t, http.MethodDelete,
		baseURL+"/admin/api/savevault/scopes/"+kind+"/"+game, nil, ifMatch)
}

func restoreSaveVaultScope(t *testing.T, baseURL, kind, game string) (*http.Response, []byte) {
	t.Helper()
	return adminDo(t, http.MethodPost,
		baseURL+"/admin/api/savevault/scopes/"+kind+"/"+game+"/restore", nil)
}

func retireSaveVaultScope(t *testing.T, baseURL, kind, game, into string) (*http.Response, []byte) {
	t.Helper()
	return adminDo(t, http.MethodPost,
		baseURL+"/admin/api/savevault/scopes/"+kind+"/"+game+"/retire", map[string]string{"into": into})
}

func unretireSaveVaultScope(t *testing.T, baseURL, kind, game string) (*http.Response, []byte) {
	t.Helper()
	return adminDo(t, http.MethodPost,
		baseURL+"/admin/api/savevault/scopes/"+kind+"/"+game+"/unretire", nil)
}

func getSaveVaultClientScope(t *testing.T, baseURL, kind, game string) (*http.Response, []byte) {
	t.Helper()
	request := saveVaultScopeRequest(t, http.MethodGet, baseURL+"/v1/savevault/scopes/"+kind+"/"+game, nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return response, body
}

func decodeSaveVaultTombstoneResponse(t *testing.T, body []byte) saveVaultTombstoneResponse {
	t.Helper()
	var response saveVaultTombstoneResponse
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode tombstone response %q: %v", body, err)
	}
	return response
}

func listSaveVaultTombstonesForTest(t *testing.T, baseURL string) []saveVaultTombstoneView {
	t.Helper()
	response, body := adminDo(t, http.MethodGet, baseURL+"/admin/api/savevault/tombstones", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("tombstone list status = %d: %s", response.StatusCode, body)
	}
	var payload struct {
		Tombstones []saveVaultTombstoneView `json:"tombstones"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Tombstones
}

func putSaveVaultRetention(t *testing.T, baseURL string, settings saveVaultRetentionSettings) saveVaultRetentionSettings {
	t.Helper()
	response, body := adminDo(t, http.MethodPut, baseURL+"/admin/api/savevault/retention", settings)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("retention put status = %d: %s", response.StatusCode, body)
	}
	var result saveVaultRetentionSettings
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func previewSaveVaultRetention(t *testing.T, baseURL string) saveVaultRetentionPreview {
	t.Helper()
	response, body := adminDo(t, http.MethodPost, baseURL+"/admin/api/savevault/retention/preview", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("retention preview status = %d: %s", response.StatusCode, body)
	}
	var preview saveVaultRetentionPreview
	if err := json.Unmarshal(body, &preview); err != nil {
		t.Fatal(err)
	}
	return preview
}

func setSaveVaultSnapshotReceivedAt(t *testing.T, s *server, kind, gameID string, received map[string]time.Time) {
	t.Helper()
	s.saveVaultMu.Lock()
	key := saveVaultScopeKey(kind, gameID)
	stored := s.saveVaultState.Scopes[key]
	for id, at := range received {
		snapshot, ok := stored.Snapshots[id]
		if !ok {
			s.saveVaultMu.Unlock()
			t.Fatalf("snapshot %s not found", id)
		}
		snapshot.ReceivedAt = at
		stored.Snapshots[id] = snapshot
	}
	next := s.saveVaultState.clone()
	next.Scopes[key] = stored
	err := s.persistSaveVaultState(next)
	if err == nil {
		s.saveVaultState = next
	}
	s.saveVaultMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}

func setSaveVaultTombstonePurgeAfter(t *testing.T, s *server, kind, gameID string, at time.Time) {
	t.Helper()
	s.manageMu.Lock()
	next := s.saveVaultManage.clone()
	key := saveVaultManageScopeKey(kind, gameID)
	tombstone, ok := next.Scopes[key]
	if !ok {
		s.manageMu.Unlock()
		t.Fatalf("tombstone %s not found", key)
	}
	tombstone.PurgeAfter = at
	next.Scopes[key] = tombstone
	err := s.persistSaveVaultManage(next)
	if err == nil {
		s.saveVaultManage = next
	}
	s.manageMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}

func readSaveVaultManageForTest(t *testing.T, s *server) saveVaultManageState {
	t.Helper()
	s.manageMu.Lock()
	defer s.manageMu.Unlock()
	return s.saveVaultManage.clone()
}

func archiveDirectoryContents(t *testing.T, s *server) map[string][]byte {
	t.Helper()
	result := make(map[string][]byte)
	root := s.saveVaultNativeDirectory()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result[relative] = data
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return result
}

func TestSaveVaultManageStateIsAbsentUntilFirstC6Use(t *testing.T) {
	s, httpServer := newC6TestServer(t)
	defer httpServer.Close()

	createSaveVaultSnapshot(t, httpServer.URL, "pc", "plain-game", "v1", "", "", []byte("save-one"))
	if _, err := s.runMaintenance(false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.runMaintenance(true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, saveVaultManageFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("savevault-manage.json exists after unrelated use: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, saveVaultTrashDirectoryName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("savevault-trash exists after unrelated use: %v", err)
	}
}

func TestSaveVaultDeletedScopeBlocksOldClientFlow(t *testing.T) {
	s, httpServer := newC6TestServer(t)
	defer httpServer.Close()

	const kind, gameID = "pc", "steamui"
	createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, "v1", "", "", []byte("first"))
	head := createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, "v2", "v1", "v1", []byte("second"))

	response, body := deleteSaveVaultScope(t, httpServer.URL, kind, gameID, `"`+head.ID+`"`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d: %s", response.StatusCode, body)
	}
	var tombstone saveVaultManageTombstone
	if err := json.Unmarshal(body, &tombstone); err != nil {
		t.Fatal(err)
	}
	if tombstone.State != saveVaultTombstoneDeleted || tombstone.Head != head.ID ||
		tombstone.Snapshots != 2 || tombstone.Bytes == 0 || tombstone.PurgeAfter.IsZero() {
		t.Fatalf("tombstone = %+v", tombstone)
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, saveVaultTrashDirectoryName)); err != nil {
		t.Fatalf("trash directory missing: %v", err)
	}
	tombstones := listSaveVaultTombstonesForTest(t, httpServer.URL)
	if len(tombstones) != 1 || !tombstones[0].Restorable || tombstones[0].ConflictState != "" {
		t.Fatalf("tombstone list = %+v", tombstones)
	}

	// Simulate the old client flow: GET the scope first and only upload when it
	// succeeds. A 410 must stop the flow before the archive PUT.
	legacyStatus, legacyUploaded := oldSaveVaultClientFlow(t, httpServer.URL, kind, gameID)
	if legacyStatus != http.StatusGone || legacyUploaded {
		t.Fatalf("legacy client flow = %d uploaded=%v, want 410 and no upload", legacyStatus, legacyUploaded)
	}
	response, body = getSaveVaultClientScope(t, httpServer.URL, kind, gameID)
	if response.StatusCode != http.StatusGone {
		t.Fatalf("deleted scope GET = %d, want 410: %s", response.StatusCode, body)
	}
	payload := decodeSaveVaultTombstoneResponse(t, body)
	if payload.Code != "scope_deleted" || payload.DeletedAt.IsZero() {
		t.Fatalf("deleted scope payload = %+v", payload)
	}
	if status := putSaveVaultScopeArchive(t, httpServer.URL, kind, gameID+"__main__late.zip", []byte("late")); status != http.StatusGone {
		t.Fatalf("deleted scope archive PUT = %d, want 410", status)
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, "savevault-native", kind, gameID+"__main__late.zip")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected archive was written: %v", err)
	}
	if _, exists := readSaveVaultManageForTest(t, s).Scopes[saveVaultManageScopeKey(kind, gameID)]; !exists {
		t.Fatal("tombstone disappeared")
	}
	s.saveVaultMu.Lock()
	_, exists := s.saveVaultState.Scopes[saveVaultScopeKey(kind, gameID)]
	s.saveVaultMu.Unlock()
	if exists {
		t.Fatal("deleted scope was recreated")
	}
}

func TestSaveVaultRetiredScopeBlocksWritesButKeepsArchives(t *testing.T) {
	_, httpServer := newC6TestServer(t)
	defer httpServer.Close()

	const kind = "pc"
	createSaveVaultSnapshot(t, httpServer.URL, kind, "forza-horizon-5", "v1", "", "", []byte("target"))
	source := createSaveVaultSnapshot(t, httpServer.URL, kind, "forza-horizon-5-2", "v1", "", "", []byte("source"))

	response, body := retireSaveVaultScope(t, httpServer.URL, kind, "forza-horizon-5-2", "forza-horizon-5")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("retire status = %d: %s", response.StatusCode, body)
	}
	var tombstone saveVaultManageTombstone
	if err := json.Unmarshal(body, &tombstone); err != nil {
		t.Fatal(err)
	}
	if tombstone.State != saveVaultTombstoneRetired || tombstone.Successor == "" {
		t.Fatalf("retired tombstone = %+v", tombstone)
	}

	legacyStatus, legacyUploaded := oldSaveVaultClientFlow(t, httpServer.URL, kind, "forza-horizon-5-2")
	if legacyStatus != http.StatusGone || legacyUploaded {
		t.Fatalf("retired legacy client flow = %d uploaded=%v, want 410 and no upload", legacyStatus, legacyUploaded)
	}
	response, body = getSaveVaultClientScope(t, httpServer.URL, kind, "forza-horizon-5-2")
	if response.StatusCode != http.StatusGone {
		t.Fatalf("retired scope GET = %d, want 410: %s", response.StatusCode, body)
	}
	payload := decodeSaveVaultTombstoneResponse(t, body)
	if payload.Code != "scope_retired" || payload.Successor == nil ||
		payload.Successor.Type != kind || payload.Successor.GameID != "forza-horizon-5" {
		t.Fatalf("retired scope payload = %+v", payload)
	}
	if status := putSaveVaultScopeArchive(t, httpServer.URL, kind, "forza-horizon-5-2__main__late.zip", []byte("late")); status != http.StatusGone {
		t.Fatalf("retired scope archive PUT = %d, want 410", status)
	}
	candidate := saveVaultScopeCandidateFor("v2", "forza-horizon-5-2__main__v2.zip", "v1", []byte("late"), time.Now().UTC())
	if status := putSaveVaultScopeArchive(t, httpServer.URL, kind, candidate.Archive, []byte("late")); status != http.StatusGone {
		t.Fatalf("retired scope snapshot archive PUT = %d, want 410", status)
	}
	if response, body := putSaveVaultScopeCandidate(t, httpServer.URL,
		"/v1/savevault/scopes/"+kind+"/forza-horizon-5-2/snapshots/v2", source.ID, candidate); response.StatusCode != http.StatusGone {
		t.Fatalf("retired scope snapshot PUT = %d: %s", response.StatusCode, body)
	}

	// The archive is still downloadable after the merge.
	request := saveVaultScopeRequest(t, http.MethodGet,
		httpServer.URL+"/v1/savevault/files/"+kind+"/forza-horizon-5-2__main__v1.zip", nil)
	download, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	contents, _ := io.ReadAll(download.Body)
	download.Body.Close()
	if download.StatusCode != http.StatusOK || string(contents) != "source" {
		t.Fatalf("retired archive GET = %d %q", download.StatusCode, contents)
	}

	request = saveVaultScopeRequest(t, http.MethodGet, httpServer.URL+"/v1/savevault/games", nil)
	gamesResponse, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(gamesResponse.Body)
	gamesResponse.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if gamesResponse.StatusCode != http.StatusOK {
		t.Fatalf("games status = %d: %s", gamesResponse.StatusCode, body)
	}
	var games struct {
		Games []saveVaultGame `json:"games"`
	}
	if err := json.Unmarshal(body, &games); err != nil {
		t.Fatal(err)
	}
	var foundTarget, foundSource bool
	for _, game := range games.Games {
		for _, member := range game.Scopes {
			if member.GameID == "forza-horizon-5" {
				foundTarget = true
			}
			if member.GameID == "forza-horizon-5-2" {
				foundSource = member.Retired
			}
		}
	}
	if !foundTarget || !foundSource {
		t.Fatalf("retired game grouping = %+v", games.Games)
	}
	response, body = unretireSaveVaultScope(t, httpServer.URL, kind, "forza-horizon-5-2")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("unretire status = %d: %s", response.StatusCode, body)
	}
}

func TestSaveVaultDeletePreconditions(t *testing.T) {
	s, httpServer := newC6TestServer(t)
	defer httpServer.Close()

	const kind, gameID = "pc", "precondition"
	head := createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, "v1", "", "", []byte("save"))
	before := archiveDirectoryContents(t, s)

	response, body := deleteSaveVaultScope(t, httpServer.URL, kind, gameID, "")
	if response.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("missing If-Match status = %d: %s", response.StatusCode, body)
	}
	response, body = deleteSaveVaultScope(t, httpServer.URL, kind, gameID, `"wrong"`)
	if response.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match status = %d: %s", response.StatusCode, body)
	}
	var conflict struct {
		Head string `json:"head"`
	}
	if err := json.Unmarshal(body, &conflict); err != nil || conflict.Head != head.ID {
		t.Fatalf("precondition body = %s", body)
	}
	if !reflect.DeepEqual(before, archiveDirectoryContents(t, s)) {
		t.Fatal("failed delete changed archives")
	}
	if len(listSaveVaultTombstonesForTest(t, httpServer.URL)) != 0 {
		t.Fatal("failed delete left a tombstone")
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, saveVaultTrashDirectoryName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed delete created trash: %v", err)
	}
}

func TestSaveVaultDeleteRestoreRoundTrip(t *testing.T) {
	s, httpServer := newC6TestServer(t)
	defer httpServer.Close()

	const kind, gameID = "pc", "round-trip"
	createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, "v1", "", "", []byte("first"))
	head := createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, "v2", "v1", "v1", []byte("second"))
	createSaveVaultConflictSnapshot(t, httpServer.URL, kind, gameID, "conflict", "other", []byte("conflict"))
	orphanName := gameID + "__main__orphan.zip"
	if err := os.WriteFile(filepath.Join(s.dataDir, "savevault-native", kind, orphanName), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := readSaveVaultScopeState(t, s, kind, gameID)
	beforeArchives := archiveDirectoryContents(t, s)

	response, body := deleteSaveVaultScope(t, httpServer.URL, kind, gameID, `"`+head.ID+`"`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d: %s", response.StatusCode, body)
	}
	if _, exists := readSaveVaultManageForTest(t, s).Scopes[saveVaultManageScopeKey(kind, gameID)]; !exists {
		t.Fatal("delete did not write a tombstone")
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, "savevault-native", kind, gameID+"__main__v1.zip")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("delete left archive in live directory: %v", err)
	}

	response, body = restoreSaveVaultScope(t, httpServer.URL, kind, gameID)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("restore status = %d: %s", response.StatusCode, body)
	}
	after := readSaveVaultScopeState(t, s, kind, gameID)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("restored scope differs:\nbefore=%+v\nafter=%+v", before, after)
	}
	if !reflect.DeepEqual(beforeArchives, archiveDirectoryContents(t, s)) {
		t.Fatal("restored archives differ")
	}
	if len(listSaveVaultTombstonesForTest(t, httpServer.URL)) != 0 {
		t.Fatal("restore left a tombstone")
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, saveVaultTrashDirectoryName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restore left trash: %v", err)
	}
}

func TestSaveVaultDeleteRollbackBranches(t *testing.T) {
	t.Run("trash creation", func(t *testing.T) {
		s, httpServer := newC6TestServer(t)
		defer httpServer.Close()
		const kind, gameID = "pc", "trash-failure"
		head := createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, "v1", "", "", []byte("save"))
		before := archiveDirectoryContents(t, s)
		s.manageFileOps.MkdirAll = func(string, os.FileMode) error { return errors.New("injected") }
		response, body := deleteSaveVaultScope(t, httpServer.URL, kind, gameID, `"`+head.ID+`"`)
		if response.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d: %s", response.StatusCode, body)
		}
		if !reflect.DeepEqual(before, archiveDirectoryContents(t, s)) {
			t.Fatal("rollback did not restore archives")
		}
		if len(listSaveVaultTombstonesForTest(t, httpServer.URL)) != 0 {
			t.Fatal("rollback left a tombstone")
		}
	})

	t.Run("move", func(t *testing.T) {
		s, httpServer := newC6TestServer(t)
		defer httpServer.Close()
		const kind, gameID = "pc", "move-failure"
		head := createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, "v1", "", "", []byte("save"))
		before := archiveDirectoryContents(t, s)
		s.manageFileOps.Rename = func(string, string) error { return errors.New("injected") }
		response, body := deleteSaveVaultScope(t, httpServer.URL, kind, gameID, `"`+head.ID+`"`)
		if response.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d: %s", response.StatusCode, body)
		}
		if !reflect.DeepEqual(before, archiveDirectoryContents(t, s)) {
			t.Fatal("rollback did not restore archives")
		}
		if len(listSaveVaultTombstonesForTest(t, httpServer.URL)) != 0 {
			t.Fatal("rollback left a tombstone")
		}
	})

	t.Run("scope json", func(t *testing.T) {
		s, httpServer := newC6TestServer(t)
		defer httpServer.Close()
		const kind, gameID = "pc", "scope-json-failure"
		head := createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, "v1", "", "", []byte("save"))
		before := archiveDirectoryContents(t, s)
		s.manageFileOps.WriteJSON = func(path string, _ any) error {
			if strings.HasSuffix(path, "scope.json") {
				return errors.New("injected")
			}
			return writeJSONFile(path, nil)
		}
		response, body := deleteSaveVaultScope(t, httpServer.URL, kind, gameID, `"`+head.ID+`"`)
		if response.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d: %s", response.StatusCode, body)
		}
		if !reflect.DeepEqual(before, archiveDirectoryContents(t, s)) {
			t.Fatal("rollback did not restore archives")
		}
		if len(listSaveVaultTombstonesForTest(t, httpServer.URL)) != 0 {
			t.Fatal("rollback left a tombstone")
		}
	})

	t.Run("state persist", func(t *testing.T) {
		s, httpServer := newC6TestServer(t)
		defer httpServer.Close()
		const kind, gameID = "pc", "state-failure"
		head := createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, "v1", "", "", []byte("save"))
		before := archiveDirectoryContents(t, s)
		statePath := s.saveVaultStatePath()
		if err := os.Remove(statePath); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(statePath, 0o700); err != nil {
			t.Fatal(err)
		}
		response, body := deleteSaveVaultScope(t, httpServer.URL, kind, gameID, `"`+head.ID+`"`)
		if response.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d: %s", response.StatusCode, body)
		}
		if !reflect.DeepEqual(before, archiveDirectoryContents(t, s)) {
			t.Fatal("rollback did not restore archives")
		}
		if len(listSaveVaultTombstonesForTest(t, httpServer.URL)) != 0 {
			t.Fatal("rollback left a tombstone")
		}
	})

	t.Run("rollback failure retries", func(t *testing.T) {
		s, httpServer := newC6TestServer(t)
		defer httpServer.Close()
		const kind, gameID = "pc", "rollback-failure"
		head := createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, "v1", "", "", []byte("save"))
		statePath := s.saveVaultStatePath()
		if err := os.Remove(statePath); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(statePath, 0o700); err != nil {
			t.Fatal(err)
		}
		renameCalls := 0
		s.manageFileOps.Rename = func(oldpath, newpath string) error {
			renameCalls++
			if renameCalls == 2 {
				return errors.New("injected rollback failure")
			}
			return os.Rename(oldpath, newpath)
		}
		response, body := deleteSaveVaultScope(t, httpServer.URL, kind, gameID, `"`+head.ID+`"`)
		if response.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d: %s", response.StatusCode, body)
		}
		tombstones := listSaveVaultTombstonesForTest(t, httpServer.URL)
		if len(tombstones) != 1 || tombstones[0].State != saveVaultTombstoneDeleted {
			t.Fatalf("rollback failure tombstone = %+v", tombstones)
		}
		// The tombstone is present while the scope still exists. Repair the
		// state file and retry the same delete; it must finish.
		if err := os.Remove(statePath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(statePath, []byte(`{"scopes":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		// The previous state file was a directory, so the in-memory state still
		// has the scope. Retry now uses the same tombstone.
		s.manageFileOps.Rename = nil
		response, body = deleteSaveVaultScope(t, httpServer.URL, kind, gameID, `"`+head.ID+`"`)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("retry delete status = %d: %s", response.StatusCode, body)
		}
	})
}

func TestSaveVaultMaintenancePurgesExpiredTrash(t *testing.T) {
	s, httpServer := newC6TestServer(t)
	defer httpServer.Close()

	const kind, gameID = "pc", "expired-trash"
	head := createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, "v1", "", "", []byte("save"))
	if response, body := deleteSaveVaultScope(t, httpServer.URL, kind, gameID, `"`+head.ID+`"`); response.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d: %s", response.StatusCode, body)
	}
	setSaveVaultTombstonePurgeAfter(t, s, kind, gameID, time.Now().UTC().Add(-time.Hour))
	if response, body := restoreSaveVaultScope(t, httpServer.URL, kind, gameID); response.StatusCode != http.StatusConflict {
		t.Fatalf("expired restore status = %d: %s", response.StatusCode, body)
	}

	report, err := s.runMaintenance(true)
	if err != nil {
		t.Fatal(err)
	}
	if report.TombstonesPurged != 1 {
		t.Fatalf("dry-run report = %+v", report)
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, saveVaultTrashDirectoryName)); err != nil {
		t.Fatalf("dry run removed trash: %v", err)
	}
	if _, exists := readSaveVaultManageForTest(t, s).Scopes[saveVaultManageScopeKey(kind, gameID)]; !exists {
		t.Fatal("dry run removed tombstone")
	}

	report, err = s.runMaintenance(false)
	if err != nil {
		t.Fatal(err)
	}
	if report.TombstonesPurged != 1 {
		t.Fatalf("purge report = %+v", report)
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, saveVaultTrashDirectoryName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("purge left trash: %v", err)
	}
	tombstone, exists := readSaveVaultManageForTest(t, s).Scopes[saveVaultManageScopeKey(kind, gameID)]
	if !exists || tombstone.Head != "" || !tombstone.PurgeAfter.IsZero() {
		t.Fatalf("purged tombstone = %+v exists=%v", tombstone, exists)
	}
}

func TestSaveVaultClearTombstoneAllowsRecreation(t *testing.T) {
	s, httpServer := newC6TestServer(t)
	defer httpServer.Close()

	const kind, gameID = "pc", "clear-me"
	head := createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, "v1", "", "", []byte("save"))
	if response, body := deleteSaveVaultScope(t, httpServer.URL, kind, gameID, `"`+head.ID+`"`); response.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d: %s", response.StatusCode, body)
	}
	response, body := adminDo(t, http.MethodDelete, httpServer.URL+"/admin/api/savevault/tombstones/"+kind+"/"+gameID, nil)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("clear status = %d: %s", response.StatusCode, body)
	}
	if len(listSaveVaultTombstonesForTest(t, httpServer.URL)) != 0 {
		t.Fatal("clear left a tombstone")
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, saveVaultTrashDirectoryName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clear left trash: %v", err)
	}
	createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, "v1", "", "", []byte("new save"))
	if _, exists := readSaveVaultScopeState(t, s, kind, gameID).Snapshots["v1"]; !exists {
		t.Fatal("scope was not recreated")
	}
}

func TestSaveVaultRetireValidationAndUnretire(t *testing.T) {
	s, httpServer := newC6TestServer(t)
	defer httpServer.Close()

	const kind = "pc"
	createSaveVaultSnapshot(t, httpServer.URL, kind, "merge-a", "v1", "", "", []byte("a"))
	createSaveVaultSnapshot(t, httpServer.URL, kind, "merge-b", "v1", "", "", []byte("b"))
	createSaveVaultSnapshot(t, httpServer.URL, kind, "merge-c", "v1", "", "", []byte("c"))
	createSaveVaultSnapshot(t, httpServer.URL, kind, "merge-d", "v1", "", "", []byte("d"))
	deletedTarget := createSaveVaultSnapshot(t, httpServer.URL, kind, "merge-deleted", "v1", "", "", []byte("deleted"))
	createSaveVaultSnapshot(t, httpServer.URL, "switch", "merge-switch", "v1", "", "", []byte("s"))
	if response, body := deleteSaveVaultScope(t, httpServer.URL, kind, "merge-deleted", `"`+deletedTarget.ID+`"`); response.StatusCode != http.StatusOK {
		t.Fatalf("delete target status = %d: %s", response.StatusCode, body)
	}

	if response, _ := retireSaveVaultScope(t, httpServer.URL, kind, "merge-a", "merge-a"); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("self merge status = %d", response.StatusCode)
	}
	if response, _ := retireSaveVaultScope(t, httpServer.URL, kind, "merge-a", "missing"); response.StatusCode != http.StatusNotFound {
		t.Fatalf("missing target status = %d", response.StatusCode)
	}
	if response, _ := retireSaveVaultScope(t, httpServer.URL, kind, "merge-a", "switch/merge-switch"); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid cross-type target status = %d", response.StatusCode)
	}
	if response, _ := retireSaveVaultScope(t, httpServer.URL, kind, "merge-a", "merge-switch"); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("existing cross-type target status = %d", response.StatusCode)
	}
	if response, _ := retireSaveVaultScope(t, httpServer.URL, kind, "merge-d", "merge-deleted"); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("deleted target status = %d", response.StatusCode)
	}

	if response, body := retireSaveVaultScope(t, httpServer.URL, kind, "merge-b", "merge-a"); response.StatusCode != http.StatusOK {
		t.Fatalf("first merge status = %d: %s", response.StatusCode, body)
	}
	if response, _ := retireSaveVaultScope(t, httpServer.URL, kind, "merge-c", "merge-b"); response.StatusCode != http.StatusConflict {
		t.Fatalf("chained merge status = %d", response.StatusCode)
	}
	if response, _ := retireSaveVaultScope(t, httpServer.URL, kind, "merge-c", "merge-a"); response.StatusCode != http.StatusOK {
		t.Fatalf("second merge status = %d", response.StatusCode)
	}
	// A scope that is already retired cannot be a target.
	if response, _ := retireSaveVaultScope(t, httpServer.URL, kind, "merge-a", "merge-b"); response.StatusCode != http.StatusConflict {
		t.Fatalf("retired target status = %d", response.StatusCode)
	}
	if response, _ := retireSaveVaultScope(t, httpServer.URL, kind, "merge-b", "merge-a"); response.StatusCode != http.StatusConflict {
		t.Fatalf("retired source status = %d", response.StatusCode)
	}

	if response, body := adminDo(t, http.MethodDelete,
		httpServer.URL+"/admin/api/savevault/tombstones/"+kind+"/merge-b", nil); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("clear retired tombstone status = %d: %s", response.StatusCode, body)
	}
	if response, body := unretireSaveVaultScope(t, httpServer.URL, kind, "merge-b"); response.StatusCode != http.StatusOK {
		t.Fatalf("unretire status = %d: %s", response.StatusCode, body)
	}
	state := readSaveVaultScopeState(t, s, kind, "merge-b")
	if state.GameLink != "" {
		t.Fatalf("unretire left GameLink %q", state.GameLink)
	}
	createSaveVaultSnapshot(t, httpServer.URL, kind, "merge-b", "v2", "v1", "v1", []byte("b2"))
}

func TestSaveVaultExportZip(t *testing.T) {
	s, httpServer := newC6TestServer(t)
	defer httpServer.Close()

	const kind, gameID = "pc", "export-game"
	createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, "v1", "", "", []byte("first bytes"))
	head := createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, "v2", "v1", "v1", []byte("second bytes"))
	createSaveVaultConflictSnapshot(t, httpServer.URL, kind, gameID, "conflict", "other", []byte("conflict bytes"))
	if response, body := adminDo(t, http.MethodPut,
		httpServer.URL+"/admin/api/savevault/scopes/"+kind+"/"+gameID+"/snapshots/v1/lock",
		map[string]string{"note": "keep me"}); response.StatusCode != http.StatusOK {
		t.Fatalf("lock status = %d: %s", response.StatusCode, body)
	}
	// Remove one archive to exercise the manifest's missing marker.
	missingPath, _ := s.saveVaultPath(kind, gameID+"__main__conflict.zip")
	if err := os.Remove(missingPath); err != nil {
		t.Fatal(err)
	}

	response, body := adminDo(t, http.MethodGet,
		httpServer.URL+"/admin/api/savevault/scopes/"+kind+"/"+gameID+"/export", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d: %s", response.StatusCode, body)
	}
	if response.Header.Get("Content-Type") != "application/zip" {
		t.Fatalf("export content type = %q", response.Header.Get("Content-Type"))
	}
	if !strings.Contains(response.Header.Get("Content-Disposition"), kind+"-"+gameID+".zip") {
		t.Fatalf("export disposition = %q", response.Header.Get("Content-Disposition"))
	}
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string][]byte)
	for _, file := range reader.File {
		opened, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		contents, err := io.ReadAll(opened)
		opened.Close()
		if err != nil {
			t.Fatal(err)
		}
		files[file.Name] = contents
	}
	if !bytes.Equal(files["versions/"+gameID+"__main__v1.zip"], []byte("first bytes")) {
		t.Fatalf("v1 archive = %q", files["versions/"+gameID+"__main__v1.zip"])
	}
	if !bytes.Equal(files["versions/"+gameID+"__main__v2.zip"], []byte("second bytes")) {
		t.Fatalf("v2 archive = %q", files["versions/"+gameID+"__main__v2.zip"])
	}
	var manifest saveVaultExportManifest
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Head != head.ID || manifest.Type != kind || manifest.GameID != gameID || len(manifest.Versions) != 3 {
		t.Fatalf("manifest = %+v", manifest)
	}
	byID := make(map[string]saveVaultExportVersion)
	for _, version := range manifest.Versions {
		byID[version.ID] = version
	}
	if !byID["v1"].Locked || byID["v1"].LockNote != "keep me" || byID["v1"].Device == "" {
		t.Fatalf("v1 manifest = %+v", byID["v1"])
	}
	if !byID["conflict"].Conflict || byID["conflict"].ConflictOf == "" || !byID["conflict"].Missing {
		t.Fatalf("conflict manifest = %+v", byID["conflict"])
	}
	if byID["v2"].Parent != "v1" || byID["v2"].ContentDigest == "" || byID["v2"].ReceivedAt.IsZero() {
		t.Fatalf("v2 manifest = %+v", byID["v2"])
	}
}

func TestSaveVaultLockManualPruneAndAutomaticRetention(t *testing.T) {
	s, httpServer := newC6TestServer(t)
	defer httpServer.Close()

	const kind, gameID = "pc", "retention-game"
	started := time.Now().UTC().Add(-10 * 24 * time.Hour)
	for index := 1; index <= 6; index++ {
		id := fmt.Sprintf("v%d", index)
		parent := ""
		if index > 1 {
			parent = fmt.Sprintf("v%d", index-1)
		}
		createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, id, parent, parent, []byte("version-"+id))
	}
	createSaveVaultConflictSnapshot(t, httpServer.URL, kind, gameID, "conflict", "other", []byte("conflict"))
	setSaveVaultSnapshotReceivedAt(t, s, kind, gameID, map[string]time.Time{
		"v1": started, "v2": started.Add(time.Hour), "v3": started.Add(2 * time.Hour),
		"v4": started.Add(3 * time.Hour), "v5": started.Add(4 * time.Hour), "v6": started.Add(5 * time.Hour),
	})

	if response, body := adminDo(t, http.MethodPut,
		httpServer.URL+"/admin/api/savevault/scopes/"+kind+"/"+gameID+"/snapshots/v2/lock",
		map[string]string{"note": "keep this"}); response.StatusCode != http.StatusOK {
		t.Fatalf("lock status = %d: %s", response.StatusCode, body)
	}
	// Locked snapshots must not appear in the persisted state file.
	stateBytes, err := os.ReadFile(s.saveVaultStatePath())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stateBytes, []byte(`"locked"`)) || bytes.Contains(stateBytes, []byte(`"lockNote"`)) {
		t.Fatalf("state file contains response-only lock fields: %s", stateBytes)
	}
	response, body := getSaveVaultClientScope(t, httpServer.URL, kind, gameID)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("scope GET = %d: %s", response.StatusCode, body)
	}
	var clientScope saveVaultScopeResponse
	if err := json.Unmarshal(body, &clientScope); err != nil {
		t.Fatal(err)
	}
	var locked bool
	for _, snapshot := range clientScope.Snapshots {
		if snapshot.ID == "v2" {
			locked = snapshot.Locked && snapshot.LockNote == "keep this"
		}
	}
	if !locked {
		t.Fatalf("client scope did not expose lock: %s", body)
	}
	audit := fetchSaveVaultScopeAudit(t, httpServer.URL, kind, gameID)
	var auditLocked bool
	for _, snapshot := range audit.Snapshots {
		if snapshot.ID == "v2" {
			auditLocked = snapshot.Locked && snapshot.LockNote == "keep this"
		}
	}
	if !auditLocked {
		t.Fatalf("admin audit did not expose lock: %+v", audit.Snapshots)
	}

	// Manual prune keeps the locked version and does not count it toward keep.
	report := pruneSaveVaultScope(t, httpServer.URL, kind, gameID, 2, false)
	if report.Removed != 3 {
		t.Fatalf("manual prune report = %+v", report)
	}
	audit = fetchSaveVaultScopeAudit(t, httpServer.URL, kind, gameID)
	remaining := make(map[string]bool)
	for _, snapshot := range audit.Snapshots {
		remaining[snapshot.ID] = true
	}
	for _, id := range []string{"v2", "v5", "v6", "conflict"} {
		if !remaining[id] {
			t.Fatalf("manual prune removed retained %s: %+v", id, remaining)
		}
	}
	for _, id := range []string{"v1", "v3", "v4"} {
		if remaining[id] {
			t.Fatalf("manual prune kept pruned %s: %+v", id, remaining)
		}
	}

	// Add more versions and exercise automatic retention with N=3.
	createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, "v7", "v6", "v6", []byte("version-v7"))
	createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, "v8", "v7", "v7", []byte("version-v8"))
	createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, "v9", "v8", "v8", []byte("version-v9"))
	setSaveVaultSnapshotReceivedAt(t, s, kind, gameID, map[string]time.Time{
		"v7": started.Add(6 * time.Hour), "v8": started.Add(7 * time.Hour), "v9": started.Add(8 * time.Hour),
	})
	putSaveVaultRetention(t, httpServer.URL, saveVaultRetentionSettings{
		Default: saveVaultRetentionPolicy{Enabled: true, KeepVersions: 3},
	})
	maintenance, err := s.runMaintenance(false)
	if err != nil {
		t.Fatal(err)
	}
	if maintenance.SaveVaultVersions == 0 {
		t.Fatalf("automatic retention removed nothing: %+v", maintenance)
	}
	audit = fetchSaveVaultScopeAudit(t, httpServer.URL, kind, gameID)
	remaining = make(map[string]bool)
	for _, snapshot := range audit.Snapshots {
		remaining[snapshot.ID] = true
	}
	for _, id := range []string{"v2", "v6", "v7", "v8", "v9", "conflict"} {
		if !remaining[id] {
			t.Fatalf("automatic retention removed protected %s: %+v", id, remaining)
		}
	}
	if remaining["v5"] {
		t.Fatalf("automatic retention kept v5 outside keep=3: %+v", remaining)
	}
}

func TestSaveVaultAutomaticRetentionKeepDaysUnion(t *testing.T) {
	s, httpServer := newC6TestServer(t)
	defer httpServer.Close()

	const kind, gameID = "pc", "days-union"
	for index := 1; index <= 5; index++ {
		id := fmt.Sprintf("v%d", index)
		parent := ""
		if index > 1 {
			parent = fmt.Sprintf("v%d", index-1)
		}
		createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, id, parent, parent, []byte("version-"+id))
	}
	now := time.Now().UTC()
	setSaveVaultSnapshotReceivedAt(t, s, kind, gameID, map[string]time.Time{
		"v1": now.Add(-60 * 24 * time.Hour),
		"v2": now.Add(-40 * 24 * time.Hour),
		"v3": now.Add(-10 * 24 * time.Hour),
		"v4": now.Add(-2 * 24 * time.Hour),
		"v5": now.Add(-time.Hour),
	})
	putSaveVaultRetention(t, httpServer.URL, saveVaultRetentionSettings{
		Default: saveVaultRetentionPolicy{Enabled: true, KeepVersions: 1, KeepDays: 30},
	})
	if _, err := s.runMaintenance(false); err != nil {
		t.Fatal(err)
	}
	audit := fetchSaveVaultScopeAudit(t, httpServer.URL, kind, gameID)
	remaining := make(map[string]bool)
	for _, snapshot := range audit.Snapshots {
		remaining[snapshot.ID] = true
	}
	for _, id := range []string{"v3", "v4", "v5"} {
		if !remaining[id] {
			t.Fatalf("keepDays union removed recent %s: %+v", id, remaining)
		}
	}
	for _, id := range []string{"v1", "v2"} {
		if remaining[id] {
			t.Fatalf("keepDays union kept old %s: %+v", id, remaining)
		}
	}
}

func TestSaveVaultRetentionDisabledDoesNothing(t *testing.T) {
	s, httpServer := newC6TestServer(t)
	defer httpServer.Close()
	const kind, gameID = "pc", "disabled"
	for index := 1; index <= 4; index++ {
		id := fmt.Sprintf("v%d", index)
		parent := ""
		if index > 1 {
			parent = fmt.Sprintf("v%d", index-1)
		}
		createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, id, parent, parent, []byte("version-"+id))
	}
	report, err := s.runMaintenance(false)
	if err != nil {
		t.Fatal(err)
	}
	if report.SaveVaultVersions != 0 {
		t.Fatalf("disabled retention removed versions: %+v", report)
	}
	audit := fetchSaveVaultScopeAudit(t, httpServer.URL, kind, gameID)
	if audit.SnapshotCount != 4 {
		t.Fatalf("disabled retention changed scope: %+v", audit)
	}
}

func TestSaveVaultOrphanArchives(t *testing.T) {
	s, httpServer := newC6TestServer(t)
	defer httpServer.Close()

	const kind, gameID = "pc", "orphan-game"
	createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, "v1", "", "", []byte("referenced"))
	oldName := gameID + "__main__old-orphan.zip"
	recentName := gameID + "__main__recent-orphan.zip"
	oldPath, _ := s.saveVaultPath(kind, oldName)
	recentPath, _ := s.saveVaultPath(kind, recentName)
	if err := os.WriteFile(oldPath, []byte("old orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recentPath, []byte("recent orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().UTC().Add(-48 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	preview := previewSaveVaultRetention(t, httpServer.URL)
	if preview.Orphans != 2 || preview.OrphanBytes != int64(len("old orphan")+len("recent orphan")) ||
		len(preview.OrphanFiles) != 2 {
		t.Fatalf("orphan preview = %+v", preview)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("preview deleted orphan: %v", err)
	}

	report, err := s.runMaintenance(false)
	if err != nil {
		t.Fatal(err)
	}
	if report.SaveVaultOrphanArchives != 2 || report.SaveVaultOrphansRemoved != 0 {
		t.Fatalf("orphan cleanup off report = %+v", report)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("orphan cleanup off deleted old orphan: %v", err)
	}

	putSaveVaultRetention(t, httpServer.URL, saveVaultRetentionSettings{
		Default:       saveVaultRetentionPolicy{Enabled: false, KeepVersions: 20},
		OrphanCleanup: true,
	})
	report, err = s.runMaintenance(false)
	if err != nil {
		t.Fatal(err)
	}
	if report.SaveVaultOrphanArchives != 2 || report.SaveVaultOrphansRemoved != 1 {
		t.Fatalf("orphan cleanup on report = %+v", report)
	}
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old orphan was not removed: %v", err)
	}
	if _, err := os.Stat(recentPath); err != nil {
		t.Fatalf("recent orphan was removed: %v", err)
	}
}

func TestSaveVaultRecreatedTombstoneDoesNotExecute(t *testing.T) {
	s, httpServer := newC6TestServer(t)
	defer httpServer.Close()

	const kind, gameID = "pc", "recreated"
	head := createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, "v1", "", "", []byte("original"))
	if response, body := deleteSaveVaultScope(t, httpServer.URL, kind, gameID, `"`+head.ID+`"`); response.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d: %s", response.StatusCode, body)
	}
	// Simulate a v1.8.0 rollback that recreated the scope while the tombstone
	// remains. The live upload path is blocked, so recreate the state directly.
	s.saveVaultMu.Lock()
	next := s.saveVaultState.clone()
	next.Scopes[saveVaultScopeKey(kind, gameID)] = saveVaultScopeState{
		Head: "new",
		Snapshots: map[string]saveVaultScopeSnapshot{
			"new": {
				saveVaultScopeCandidate: saveVaultScopeCandidate{
					ID: "new", Archive: gameID + "__main__new.zip", ContentDigest: strings.Repeat("a", 64),
				},
				ReceivedAt: time.Now().UTC(),
			},
		},
	}
	err := s.persistSaveVaultState(next)
	if err == nil {
		s.saveVaultState = next
	}
	s.saveVaultMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	response, body := getSaveVaultClientScope(t, httpServer.URL, kind, gameID)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("recreated scope GET = %d, want 200: %s", response.StatusCode, body)
	}
	tombstones := listSaveVaultTombstonesForTest(t, httpServer.URL)
	if len(tombstones) != 1 || tombstones[0].ConflictState != "recreated" {
		t.Fatalf("recreated tombstone list = %+v", tombstones)
	}
	if _, err := s.runMaintenance(false); err != nil {
		t.Fatal(err)
	}
	if _, exists := readSaveVaultScopeState(t, s, kind, gameID).Snapshots["new"]; !exists {
		t.Fatal("maintenance removed a recreated scope")
	}
	if response, body := restoreSaveVaultScope(t, httpServer.URL, kind, gameID); response.StatusCode != http.StatusConflict {
		t.Fatalf("restore over recreated scope status = %d: %s", response.StatusCode, body)
	}
	if response, body := adminDo(t, http.MethodDelete,
		httpServer.URL+"/admin/api/savevault/tombstones/"+kind+"/"+gameID, nil); response.StatusCode != http.StatusNoContent {
		t.Fatalf("clear recreated tombstone status = %d: %s", response.StatusCode, body)
	}
}

func TestSaveVaultC6DoesNotChangeUnrelatedScope(t *testing.T) {
	s, httpServer := newC6TestServer(t)
	defer httpServer.Close()

	const kind = "pc"
	createSaveVaultSnapshot(t, httpServer.URL, kind, "unrelated", "u1", "", "", []byte("unrelated one"))
	createSaveVaultSnapshot(t, httpServer.URL, kind, "unrelated", "u2", "u1", "u1", []byte("unrelated two"))
	createSaveVaultConflictSnapshot(t, httpServer.URL, kind, "unrelated", "uc", "other", []byte("unrelated conflict"))

	unrelatedState := readSaveVaultScopeState(t, s, kind, "unrelated")
	unrelatedStateJSON, err := json.Marshal(unrelatedState)
	if err != nil {
		t.Fatal(err)
	}
	unrelatedArchives := archiveDirectoryContents(t, s)

	createSaveVaultSnapshot(t, httpServer.URL, kind, "subject", "v1", "", "", []byte("subject one"))
	head := createSaveVaultSnapshot(t, httpServer.URL, kind, "subject", "v2", "v1", "v1", []byte("subject two"))
	if response, body := adminDo(t, http.MethodPut,
		httpServer.URL+"/admin/api/savevault/scopes/"+kind+"/subject/snapshots/v1/lock",
		map[string]string{"note": "lock"}); response.StatusCode != http.StatusOK {
		t.Fatalf("lock status = %d: %s", response.StatusCode, body)
	}
	if response, body := deleteSaveVaultScope(t, httpServer.URL, kind, "subject", `"`+head.ID+`"`); response.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d: %s", response.StatusCode, body)
	}
	if response, body := restoreSaveVaultScope(t, httpServer.URL, kind, "subject"); response.StatusCode != http.StatusOK {
		t.Fatalf("restore status = %d: %s", response.StatusCode, body)
	}
	if response, body := retireSaveVaultScope(t, httpServer.URL, kind, "subject", "unrelated"); response.StatusCode != http.StatusOK {
		t.Fatalf("retire status = %d: %s", response.StatusCode, body)
	}
	if response, body := unretireSaveVaultScope(t, httpServer.URL, kind, "subject"); response.StatusCode != http.StatusOK {
		t.Fatalf("unretire status = %d: %s", response.StatusCode, body)
	}
	if _, err := s.runMaintenance(false); err != nil {
		t.Fatal(err)
	}

	afterStateJSON, err := json.Marshal(readSaveVaultScopeState(t, s, kind, "unrelated"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(unrelatedStateJSON, afterStateJSON) {
		t.Fatalf("C6 operations changed an unrelated scope state:\nbefore=%s\nafter=%s",
			unrelatedStateJSON, afterStateJSON)
	}
	after := archiveDirectoryContents(t, s)
	for name, contents := range unrelatedArchives {
		if !bytes.Equal(after[name], contents) {
			t.Fatalf("C6 operations changed unrelated archive %s", name)
		}
	}
}

func TestSaveVaultDeleteUploadRace(t *testing.T) {
	s, httpServer := newC6TestServer(t)
	defer httpServer.Close()

	const kind, gameID = "pc", "upload-race"
	head := createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, "v1", "", "", []byte("first"))

	reader, writer := io.Pipe()
	request, err := http.NewRequest(http.MethodPut,
		httpServer.URL+"/v1/savevault/files/"+kind+"/"+gameID+"__main__race.zip", reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	result := make(chan int, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			result <- -1
			return
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		result <- response.StatusCode
	}()
	if _, err := writer.Write(bytes.Repeat([]byte("x"), 1024)); err != nil {
		t.Fatal(err)
	}
	// Give the upload time to pass its early tombstone check and start reading.
	time.Sleep(50 * time.Millisecond)
	if response, body := deleteSaveVaultScope(t, httpServer.URL, kind, gameID, `"`+head.ID+`"`); response.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d: %s", response.StatusCode, body)
	}
	_ = writer.Close()
	status := <-result
	if status != http.StatusGone {
		t.Fatalf("racing upload status = %d, want 410", status)
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, "savevault-native", kind, gameID+"__main__race.zip")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("racing upload was published: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "savevault-native", kind))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".upload-") {
			t.Fatalf("racing upload left temporary file %s", entry.Name())
		}
	}
}

func TestSaveVaultManagePersistsAndClones(t *testing.T) {
	dataDir := t.TempDir()
	s, err := newServer(dataDir, testToken, 1024*1024, testAdminToken)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s.handler())
	defer httpServer.Close()

	createSaveVaultSnapshot(t, httpServer.URL, "pc", "persist", "v1", "", "", []byte("save"))
	if response, body := adminDo(t, http.MethodPut,
		httpServer.URL+"/admin/api/savevault/scopes/pc/persist/snapshots/v1/lock",
		map[string]string{"note": "note"}); response.StatusCode != http.StatusOK {
		t.Fatalf("lock status = %d: %s", response.StatusCode, body)
	}
	putSaveVaultRetention(t, httpServer.URL, saveVaultRetentionSettings{
		Default:       saveVaultRetentionPolicy{Enabled: true, KeepVersions: 5, KeepDays: 2},
		OrphanCleanup: true,
		Games: map[string]saveVaultRetentionPolicy{
			"pc/scope:persist": {Enabled: true, KeepVersions: 7},
		},
	})
	info, err := os.Stat(filepath.Join(dataDir, saveVaultManageFileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("savevault-manage.json mode = %o, want 600", info.Mode().Perm())
	}
	original := readSaveVaultManageForTest(t, s)
	clone := original.clone()
	clone.Scopes["pc/other"] = saveVaultManageTombstone{State: saveVaultTombstoneDeleted}
	clone.Locks["pc/persist"]["v1"] = saveVaultManageLock{Note: "changed"}
	clone.Retention.Games["pc/scope:persist"] = saveVaultRetentionPolicy{KeepVersions: 99}
	if _, ok := original.Scopes["pc/other"]; ok {
		t.Fatal("clone shares the scope map")
	}
	if original.Locks["pc/persist"]["v1"].Note != "note" {
		t.Fatal("clone shares the lock map")
	}
	if original.Retention.Games["pc/scope:persist"].KeepVersions != 7 {
		t.Fatal("clone shares the retention map")
	}

	reloaded, err := newServer(dataDir, testToken, 1024*1024, testAdminToken)
	if err != nil {
		t.Fatal(err)
	}
	reloadedManage := readSaveVaultManageForTest(t, reloaded)
	if reloadedManage.Locks["pc/persist"]["v1"].Note != "note" ||
		!reloadedManage.Retention.OrphanCleanup ||
		reloadedManage.Retention.Games["pc/scope:persist"].KeepVersions != 7 {
		t.Fatalf("reloaded manage state = %+v", reloadedManage)
	}
}

func TestSaveVaultManageMetrics(t *testing.T) {
	s, httpServer := newC6TestServer(t)
	defer httpServer.Close()

	const kind, gameID = "pc", "metrics"
	head := createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, "v1", "", "", []byte("save"))
	if response, body := adminDo(t, http.MethodPut,
		httpServer.URL+"/admin/api/savevault/scopes/"+kind+"/"+gameID+"/snapshots/v1/lock",
		map[string]string{"note": "metric lock"}); response.StatusCode != http.StatusOK {
		t.Fatalf("lock status = %d: %s", response.StatusCode, body)
	}
	if response, body := deleteSaveVaultScope(t, httpServer.URL, kind, gameID, `"`+head.ID+`"`); response.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d: %s", response.StatusCode, body)
	}
	orphanPath, _ := s.saveVaultPath(kind, "orphan__main__x.zip")
	if err := os.WriteFile(orphanPath, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	response, body := adminDo(t, http.MethodGet, httpServer.URL+"/metrics", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d", response.StatusCode)
	}
	for _, expected := range []string{
		`game_save_savevault_tombstones{state="deleted"} 1`,
		`game_save_savevault_tombstones{state="retired"} 0`,
		"game_save_savevault_locked_snapshots 0",
		"game_save_savevault_orphan_archives 1",
		"game_save_savevault_orphan_bytes 6",
	} {
		if !bytes.Contains(body, []byte(expected)) {
			t.Fatalf("metrics missing %q:\n%s", expected, body)
		}
	}
}

func doSaveVaultScopeRequest(t *testing.T, method, url string, body []byte) (int, []byte) {
	t.Helper()
	request := saveVaultScopeRequest(t, method, url, body)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, contents
}

func TestSaveVaultDeletedScopeRejectsEveryClientWrite(t *testing.T) {
	s, httpServer := newC6TestServer(t)
	defer httpServer.Close()

	const kind, gameID = "pc", "deleted-writes"
	head := createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, "v1", "", "", []byte("save"))
	if response, body := deleteSaveVaultScope(t, httpServer.URL, kind, gameID, `"`+head.ID+`"`); response.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d: %s", response.StatusCode, body)
	}
	base := httpServer.URL + "/v1/savevault/scopes/" + kind + "/" + gameID

	candidate := saveVaultScopeCandidateFor("v2", gameID+"__main__v2.zip", "v1", []byte("late"), time.Now().UTC())
	if status := putSaveVaultScopeArchive(t, httpServer.URL, kind, candidate.Archive, []byte("late")); status != http.StatusGone {
		t.Fatalf("archive PUT status = %d", status)
	}
	if response, body := putSaveVaultScopeCandidate(t, httpServer.URL,
		"/v1/savevault/scopes/"+kind+"/"+gameID+"/snapshots/v2", "v1", candidate); response.StatusCode != http.StatusGone {
		t.Fatalf("snapshot PUT status = %d: %s", response.StatusCode, body)
	}
	if response, body := putSaveVaultScopeCandidate(t, httpServer.URL,
		"/v1/savevault/scopes/"+kind+"/"+gameID+"/conflicts/v2", "-", candidate); response.StatusCode != http.StatusGone {
		t.Fatalf("conflict PUT status = %d: %s", response.StatusCode, body)
	}
	if status, body := putSaveVaultIdentity(t, httpServer.URL, kind, gameID, map[string]string{"name": "late"}); status != http.StatusGone {
		t.Fatalf("identity PUT status = %d: %s", status, body)
	}
	if status, body := doSaveVaultScopeRequest(t, http.MethodPut, base+"/custom-name", []byte(`{"name":"late"}`)); status != http.StatusGone {
		t.Fatalf("custom-name PUT status = %d: %s", status, body)
	}
	if status, body := doSaveVaultScopeRequest(t, http.MethodPut, base+"/custom-cover", nil); status != http.StatusGone {
		t.Fatalf("custom-cover PUT status = %d: %s", status, body)
	}
	if status, body := doSaveVaultScopeRequest(t, http.MethodDelete, base+"/custom-cover", nil); status != http.StatusGone {
		t.Fatalf("custom-cover DELETE status = %d: %s", status, body)
	}
	if status, body := doSaveVaultScopeRequest(t, http.MethodGet, base+"/head", nil); status != http.StatusGone {
		t.Fatalf("head GET status = %d: %s", status, body)
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, "savevault-native", kind, candidate.Archive)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected write left archive: %v", err)
	}
	if _, exists := readSaveVaultScopeState(t, s, kind, gameID).Snapshots["v2"]; exists {
		t.Fatal("rejected write recreated the scope")
	}
	if len(listSaveVaultTombstonesForTest(t, httpServer.URL)) != 1 {
		t.Fatal("rejected writes changed tombstones")
	}
}

// oldSaveVaultClientFlow is the pre-C6 client sequence: read the scope first and
// only upload an archive and snapshot when the read succeeded.
func oldSaveVaultClientFlow(t *testing.T, baseURL, kind, gameID string) (int, bool) {
	t.Helper()
	response, body := getSaveVaultClientScope(t, baseURL, kind, gameID)
	if response.StatusCode != http.StatusOK {
		return response.StatusCode, false
	}
	var scope saveVaultScopeResponse
	if err := json.Unmarshal(body, &scope); err != nil {
		t.Fatal(err)
	}
	parent := ""
	if scope.Head != nil {
		parent = scope.Head.ID
	}
	archive := gameID + "__main__legacy.zip"
	if status := putSaveVaultScopeArchive(t, baseURL, kind, archive, []byte("legacy")); status != http.StatusCreated {
		t.Fatalf("legacy archive status = %d", status)
	}
	candidate := saveVaultScopeCandidateFor("legacy", archive, parent, []byte("legacy"), time.Now().UTC())
	putResponse, putBody := putSaveVaultScopeCandidate(t, baseURL,
		"/v1/savevault/scopes/"+kind+"/"+gameID+"/snapshots/legacy", parent, candidate)
	if putResponse.StatusCode != http.StatusCreated {
		t.Fatalf("legacy snapshot status = %d: %s", putResponse.StatusCode, putBody)
	}
	return http.StatusCreated, true
}

func TestSaveVaultDeleteEmptyScopeUsesEmptyIfMatch(t *testing.T) {
	s, httpServer := newC6TestServer(t)
	defer httpServer.Close()

	const kind, gameID = "pc", "empty-scope"
	if status, body := putSaveVaultIdentity(t, httpServer.URL, kind, gameID, map[string]string{"name": "Empty"}); status != http.StatusOK {
		t.Fatalf("identity status = %d: %s", status, body)
	}
	response, body := deleteSaveVaultScope(t, httpServer.URL, kind, gameID, `""`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("empty-head delete status = %d: %s", response.StatusCode, body)
	}
	var tombstone saveVaultManageTombstone
	if err := json.Unmarshal(body, &tombstone); err != nil {
		t.Fatal(err)
	}
	if tombstone.Head != "" || tombstone.Snapshots != 0 {
		t.Fatalf("empty tombstone = %+v", tombstone)
	}
	if response, body := restoreSaveVaultScope(t, httpServer.URL, kind, gameID); response.StatusCode != http.StatusOK {
		t.Fatalf("restore status = %d: %s", response.StatusCode, body)
	}
	if readSaveVaultScopeState(t, s, kind, gameID).Identity == nil {
		t.Fatal("restore lost the empty scope identity")
	}
}

func TestSaveVaultSnapshotLockValidation(t *testing.T) {
	_, httpServer := newC6TestServer(t)
	defer httpServer.Close()

	const kind, gameID = "pc", "lock-validation"
	createSaveVaultSnapshot(t, httpServer.URL, kind, gameID, "v1", "", "", []byte("save"))
	base := httpServer.URL + "/admin/api/savevault/scopes/" + kind + "/" + gameID + "/snapshots/"
	for _, note := range []string{strings.Repeat("x", 121), "line\nbreak", "tab\tbreak"} {
		response, body := adminDo(t, http.MethodPut, base+"v1/lock", map[string]string{"note": note})
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("note %q status = %d: %s", note, response.StatusCode, body)
		}
	}
	response, body := adminDo(t, http.MethodPut, base+"missing/lock", map[string]string{"note": "x"})
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("missing snapshot lock status = %d: %s", response.StatusCode, body)
	}
	response, body = adminDo(t, http.MethodDelete, base+"missing/lock", nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("missing snapshot unlock status = %d: %s", response.StatusCode, body)
	}
	if response, body := adminDo(t, http.MethodPut, base+"v1/lock", map[string]string{"note": strings.Repeat("x", 120)}); response.StatusCode != http.StatusOK {
		t.Fatalf("max-length lock status = %d: %s", response.StatusCode, body)
	}
	if response, body := adminDo(t, http.MethodDelete, base+"v1/lock", nil); response.StatusCode != http.StatusNoContent {
		t.Fatalf("unlock status = %d: %s", response.StatusCode, body)
	}
}

func TestSaveVaultDeleteRejectsScopeWithMergedMembers(t *testing.T) {
	_, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()
	publishSaveVaultScope(t, httpServer.URL, "pc", "forza-horizon-5", "s1", "", []byte("main"))
	publishSaveVaultScope(t, httpServer.URL, "pc", "forza-horizon-5-2", "s2", "", []byte("dup"))
	retire, _ := adminDo(t, http.MethodPost, httpServer.URL+
		"/admin/api/savevault/scopes/pc/forza-horizon-5-2/retire", map[string]string{"into": "forza-horizon-5"})
	if retire.StatusCode != http.StatusOK {
		t.Fatalf("retire status = %d", retire.StatusCode)
	}
	response, body := adminDoIfMatch(t, http.MethodDelete, httpServer.URL+
		"/admin/api/savevault/scopes/pc/forza-horizon-5", nil, `"s1"`)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("delete of a merge target status = %d, want 409: %s", response.StatusCode, body)
	}
	unretire, _ := adminDo(t, http.MethodPost, httpServer.URL+
		"/admin/api/savevault/scopes/pc/forza-horizon-5-2/unretire", nil)
	if unretire.StatusCode != http.StatusOK {
		t.Fatalf("unretire status = %d", unretire.StatusCode)
	}
	response, body = adminDoIfMatch(t, http.MethodDelete, httpServer.URL+
		"/admin/api/savevault/scopes/pc/forza-horizon-5", nil, `"s1"`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("delete after unretire status = %d: %s", response.StatusCode, body)
	}
}

func TestSaveVaultRetentionPreviewDoesNotCountRetentionCandidatesAsOrphans(t *testing.T) {
	_, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()
	publishSaveVaultScope(t, httpServer.URL, "pc", "forza-horizon-5", "s1", "", []byte("one"))
	publishSaveVaultScope(t, httpServer.URL, "pc", "forza-horizon-5", "s2", "s1", []byte("two"))
	publishSaveVaultScope(t, httpServer.URL, "pc", "forza-horizon-5", "s3", "s2", []byte("three"))
	if status := putSaveVaultScopeArchive(t, httpServer.URL, "pc", "forza-horizon-5__main__orphan.zip", []byte("orphan")); status != http.StatusCreated {
		t.Fatalf("orphan upload status = %d", status)
	}
	settings := map[string]any{
		"default":       map[string]any{"enabled": true, "keepVersions": 1},
		"orphanCleanup": false,
	}
	if response, body := adminDo(t, http.MethodPut, httpServer.URL+"/admin/api/savevault/retention", settings); response.StatusCode != http.StatusOK {
		t.Fatalf("retention status = %d: %s", response.StatusCode, body)
	}
	response, body := adminDo(t, http.MethodPost, httpServer.URL+"/admin/api/savevault/retention/preview", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("preview status = %d: %s", response.StatusCode, body)
	}
	var preview struct {
		Orphans int `json:"orphans"`
	}
	if err := json.Unmarshal(body, &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Orphans != 1 {
		t.Fatalf("preview orphans = %d, want 1 (retention candidates must not be counted): %s", preview.Orphans, body)
	}
}
