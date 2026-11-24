// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func promoteSaveVaultScopeSnapshot(t *testing.T, baseURL, kind, game, snapshot, expectedHead string) (*http.Response, []byte) {
	t.Helper()
	return adminDo(t, http.MethodPost, baseURL+"/admin/api/savevault/scopes/"+kind+"/"+game+"/promote",
		map[string]string{"snapshot": snapshot, "expectedHead": expectedHead})
}

func createSaveVaultSnapshot(t *testing.T, baseURL, kind, game, id, parent, expectedHead string, contents []byte) saveVaultScopeCandidate {
	t.Helper()
	archive := game + "__main__" + id + ".zip"
	if status := putSaveVaultScopeArchive(t, baseURL, kind, archive, contents); status != http.StatusCreated {
		t.Fatalf("archive %s status = %d, want 201", archive, status)
	}
	candidate := saveVaultScopeCandidateFor(id, archive, parent, contents, time.Now().UTC())
	response, body := putSaveVaultScopeCandidate(t, baseURL,
		"/v1/savevault/scopes/"+kind+"/"+game+"/snapshots/"+id, expectedHead, candidate)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("snapshot %s status = %d, want 201: %s", id, response.StatusCode, body)
	}
	return candidate
}

func createSaveVaultConflictSnapshot(t *testing.T, baseURL, kind, game, id, parent string, contents []byte) saveVaultScopeCandidate {
	t.Helper()
	archive := game + "__main__" + id + ".zip"
	if status := putSaveVaultScopeArchive(t, baseURL, kind, archive, contents); status != http.StatusCreated {
		t.Fatalf("archive %s status = %d, want 201", archive, status)
	}
	candidate := saveVaultScopeCandidateFor(id, archive, parent, contents, time.Now().UTC())
	response, body := putSaveVaultScopeCandidate(t, baseURL,
		"/v1/savevault/scopes/"+kind+"/"+game+"/conflicts/"+id, "-", candidate)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("conflict snapshot %s status = %d, want 201: %s", id, response.StatusCode, body)
	}
	return candidate
}

func readSaveVaultScopeState(t *testing.T, s *server, kind, game string) saveVaultScopeState {
	t.Helper()
	s.saveVaultMu.Lock()
	defer s.saveVaultMu.Unlock()
	return s.saveVaultState.Scopes[saveVaultScopeKey(kind, game)]
}

func findSaveVaultSnapshot(items []saveVaultScopeSnapshot, id string) *saveVaultScopeSnapshot {
	for index := range items {
		if items[index].ID == id {
			return &items[index]
		}
	}
	return nil
}

func TestPromoteSaveVaultConflictCopyBecomesHead(t *testing.T) {
	s, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()

	first := createSaveVaultSnapshot(t, httpServer.URL, "switch", "odyssey", "snapshot-1", "", "", []byte("first"))
	second := createSaveVaultSnapshot(t, httpServer.URL, "switch", "odyssey", "snapshot-2", first.ID, first.ID, []byte("second"))
	conflict := createSaveVaultConflictSnapshot(t, httpServer.URL, "switch", "odyssey", "conflict-1", first.ID, []byte("conflict"))

	response, body := promoteSaveVaultScopeSnapshot(t, httpServer.URL, "switch", "odyssey", conflict.ID, second.ID)
	if response.StatusCode != http.StatusOK || response.Header.Get("ETag") != `"`+conflict.ID+`"` {
		t.Fatalf("promote status = %d, ETag=%q: %s", response.StatusCode, response.Header.Get("ETag"), body)
	}
	var promoted saveVaultScopeSnapshot
	if err := json.Unmarshal(body, &promoted); err != nil {
		t.Fatal(err)
	}
	if promoted.ID != conflict.ID || promoted.Conflict || promoted.ConflictOf != second.ID || promoted.PromotedAt.IsZero() {
		t.Fatalf("promoted snapshot = %+v", promoted)
	}

	response, body = adminDo(t, http.MethodGet, httpServer.URL+"/admin/api/savevault/scopes/switch/odyssey", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("scope detail status = %d: %s", response.StatusCode, body)
	}
	var audit saveVaultScopeAudit
	if err := json.Unmarshal(body, &audit); err != nil {
		t.Fatal(err)
	}
	if audit.Head == nil || audit.Head.ID != conflict.ID || audit.ConflictCount != 0 || len(audit.Snapshots) != 3 {
		t.Fatalf("promoted audit = %+v", audit)
	}
	oldHead := findSaveVaultSnapshot(audit.Snapshots, second.ID)
	if oldHead == nil || oldHead.Conflict {
		t.Fatalf("old head is not retained as a normal history version: %+v", audit.Snapshots)
	}
	if findSaveVaultSnapshot(audit.Snapshots, first.ID) == nil {
		t.Fatalf("ancestor snapshot was removed: %+v", audit.Snapshots)
	}

	s.mu.Lock()
	events := s.recentEventsLocked(maxEvents)
	s.mu.Unlock()
	if len(events) == 0 || events[0].Kind != eventPromote ||
		events[0].Scope != saveVaultEventScope("switch", "odyssey") || events[0].Snapshot != conflict.ID {
		t.Fatalf("promote event = %+v", events)
	}

	// Promoting the current head is idempotent and must not rewrite state.
	statePath := s.saveVaultStatePath()
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	response, body = promoteSaveVaultScopeSnapshot(t, httpServer.URL, "switch", "odyssey", conflict.ID, conflict.ID)
	if response.StatusCode != http.StatusOK || response.Header.Get("ETag") != `"`+conflict.ID+`"` {
		t.Fatalf("idempotent promote status = %d, ETag=%q: %s", response.StatusCode, response.Header.Get("ETag"), body)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("idempotent promote rewrote SaveVault state")
	}
}

func TestPromoteSaveVaultRejectsStaleExpectedHead(t *testing.T) {
	s, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()

	first := createSaveVaultSnapshot(t, httpServer.URL, "switch", "odyssey", "snapshot-1", "", "", []byte("first"))
	second := createSaveVaultSnapshot(t, httpServer.URL, "switch", "odyssey", "snapshot-2", first.ID, first.ID, []byte("second"))

	statePath := s.saveVaultStatePath()
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	response, body := promoteSaveVaultScopeSnapshot(t, httpServer.URL, "switch", "odyssey", first.ID, first.ID)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("stale promote status = %d, want 409: %s", response.StatusCode, body)
	}
	var conflict struct {
		Error string `json:"error"`
		Head  string `json:"head"`
	}
	if err := json.Unmarshal(body, &conflict); err != nil {
		t.Fatal(err)
	}
	if conflict.Head != second.ID {
		t.Fatalf("stale promote head = %q, want %q", conflict.Head, second.ID)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("stale promote rewrote SaveVault state")
	}
	stored := readSaveVaultScopeState(t, s, "switch", "odyssey")
	if stored.Head != second.ID || !stored.Snapshots[first.ID].PromotedAt.IsZero() {
		t.Fatalf("stale promote changed state: %+v", stored)
	}

	response, body = promoteSaveVaultScopeSnapshot(t, httpServer.URL, "switch", "odyssey", "missing-snapshot", second.ID)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown snapshot status = %d, want 404: %s", response.StatusCode, body)
	}
	response, body = promoteSaveVaultScopeSnapshot(t, httpServer.URL, "switch", "missing-game", first.ID, "")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown scope status = %d, want 404: %s", response.StatusCode, body)
	}
}

func TestPromoteSaveVaultHistoricalVersionPersistsAndBecomesParent(t *testing.T) {
	dataDir := t.TempDir()
	s, httpServer := newAdminTestServer(t, dataDir)
	defer httpServer.Close()

	first := createSaveVaultSnapshot(t, httpServer.URL, "switch", "odyssey", "snapshot-1", "", "", []byte("first"))
	second := createSaveVaultSnapshot(t, httpServer.URL, "switch", "odyssey", "snapshot-2", first.ID, first.ID, []byte("second"))
	third := createSaveVaultSnapshot(t, httpServer.URL, "switch", "odyssey", "snapshot-3", second.ID, second.ID, []byte("third"))

	response, body := promoteSaveVaultScopeSnapshot(t, httpServer.URL, "switch", "odyssey", first.ID, third.ID)
	if response.StatusCode != http.StatusOK || response.Header.Get("ETag") != `"`+first.ID+`"` {
		t.Fatalf("historical promote status = %d, ETag=%q: %s", response.StatusCode, response.Header.Get("ETag"), body)
	}
	var promoted saveVaultScopeSnapshot
	if err := json.Unmarshal(body, &promoted); err != nil {
		t.Fatal(err)
	}
	if promoted.ID != first.ID || promoted.PromotedAt.IsZero() {
		t.Fatalf("historical promoted snapshot = %+v", promoted)
	}
	stored := readSaveVaultScopeState(t, s, "switch", "odyssey")
	if stored.Head != first.ID || len(stored.Snapshots) != 3 {
		t.Fatalf("promoted state = %+v", stored)
	}
	if _, exists := stored.Snapshots[third.ID]; !exists {
		t.Fatalf("old head was removed from state: %+v", stored.Snapshots)
	}

	// The new head is durable and remains the CAS parent after a restart.
	restarted, err := newServer(dataDir, testToken, 1024*1024, testAdminToken)
	if err != nil {
		t.Fatal(err)
	}
	restartedHTTP := httptest.NewServer(restarted.handler())
	defer restartedHTTP.Close()

	headRequest := saveVaultScopeRequest(t, http.MethodGet,
		restartedHTTP.URL+"/v1/savevault/scopes/switch/odyssey/head", nil)
	response, err = http.DefaultClient.Do(headRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("ETag") != `"`+first.ID+`"` {
		t.Fatalf("restarted head status = %d, ETag=%q", response.StatusCode, response.Header.Get("ETag"))
	}
	var reloaded saveVaultScopeSnapshot
	if err := json.NewDecoder(response.Body).Decode(&reloaded); err != nil {
		t.Fatal(err)
	}
	if reloaded.ID != first.ID || reloaded.PromotedAt.IsZero() {
		t.Fatalf("restarted head = %+v", reloaded)
	}

	next := createSaveVaultSnapshot(t, restartedHTTP.URL, "switch", "odyssey",
		"snapshot-4", first.ID, first.ID, []byte("fourth"))
	if next.Parent != first.ID {
		t.Fatalf("follow-up candidate parent = %q", next.Parent)
	}

	staleContents := []byte("stale")
	staleArchive := "odyssey__main__stale.zip"
	if status := putSaveVaultScopeArchive(t, restartedHTTP.URL, "switch", staleArchive, staleContents); status != http.StatusCreated {
		t.Fatalf("stale archive status = %d", status)
	}
	stale := saveVaultScopeCandidateFor("stale", staleArchive, third.ID, staleContents, time.Now().UTC())
	response, body = putSaveVaultScopeCandidate(t, restartedHTTP.URL,
		"/v1/savevault/scopes/switch/odyssey/snapshots/"+stale.ID, next.ID, stale)
	if response.StatusCode != http.StatusConflict || !bytes.Contains(body, []byte(`"head":"`+next.ID+`"`)) {
		t.Fatalf("old-parent publish status = %d, want 409 with current head: %s", response.StatusCode, body)
	}
}

func TestPromoteSaveVaultRequiresAdminWrite(t *testing.T) {
	_, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()

	url := httpServer.URL + "/admin/api/savevault/scopes/switch/odyssey/promote"
	payload := []byte(`{"snapshot":"snapshot-1","expectedHead":"snapshot-1"}`)

	request := saveVaultScopeRequest(t, http.MethodPost, url, payload)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("sync token promote status = %d, want 401", response.StatusCode)
	}

	request, err = http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth("admin", testAdminToken)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("promote without admin header status = %d, want 403", response.StatusCode)
	}
}

func TestPromoteSaveVaultRejectsMissingArchive(t *testing.T) {
	s, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()

	first := createSaveVaultSnapshot(t, httpServer.URL, "switch", "odyssey", "snapshot-1", "", "", []byte("first"))
	second := createSaveVaultSnapshot(t, httpServer.URL, "switch", "odyssey", "snapshot-2", first.ID, first.ID, []byte("second"))

	archivePath, ok := s.saveVaultPath("switch", first.Archive)
	if !ok {
		t.Fatal("invalid persisted archive path")
	}
	if err := os.Remove(archivePath); err != nil {
		t.Fatal(err)
	}

	response, body := promoteSaveVaultScopeSnapshot(t, httpServer.URL, "switch", "odyssey", first.ID, second.ID)
	if response.StatusCode != http.StatusConflict || !bytes.Contains(bytes.ToLower(body), []byte("missing")) {
		t.Fatalf("missing archive promote status = %d: %s", response.StatusCode, body)
	}
	stored := readSaveVaultScopeState(t, s, "switch", "odyssey")
	if stored.Head != second.ID || !stored.Snapshots[first.ID].PromotedAt.IsZero() {
		t.Fatalf("missing archive promote changed state: %+v", stored)
	}
}

func TestPromoteClearConflictsAndClearConflictsEndpoint(t *testing.T) {
	s, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()

	first := createSaveVaultSnapshot(t, httpServer.URL, "pc", "fh5", "snap-1", "", "", []byte("base"))
	second := createSaveVaultSnapshot(t, httpServer.URL, "pc", "fh5", "snap-2", first.ID, first.ID, []byte("head"))

	putConflictFrom := func(id, device string, contents []byte) saveVaultScopeCandidate {
		archive := "fh5__main__" + id + ".zip"
		if status := putSaveVaultScopeArchive(t, httpServer.URL, "pc", archive, contents); status != http.StatusCreated {
			t.Fatalf("conflict archive %s status = %d", archive, status)
		}
		candidate := saveVaultScopeCandidateFor(id, archive, first.ID, contents, time.Now().UTC())
		payload, _ := json.Marshal(candidate)
		req := saveVaultScopeRequest(t, http.MethodPut, httpServer.URL+"/v1/savevault/scopes/pc/fh5/conflicts/"+id, payload)
		req.Header.Set("X-SaveVault-Device-ID", device)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("conflict %s status = %d", id, resp.StatusCode)
		}
		return candidate
	}

	c1 := putConflictFrom("conf-1", "dev-a", []byte("conflict-one"))
	c2 := putConflictFrom("conf-2", "dev-b", []byte("conflict-two"))
	cLocked := putConflictFrom("conf-locked", "dev-c", []byte("conflict-locked"))

	// Lock conf-locked so batch clear must preserve it.
	resp, body := adminDo(t, http.MethodPut,
		httpServer.URL+"/admin/api/savevault/scopes/pc/fh5/snapshots/"+cLocked.ID+"/lock",
		map[string]string{"note": "keep"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lock status = %d: %s", resp.StatusCode, body)
	}

	// Promote c2 with clearConflicts=true: promotes c2, removes c1, preserves cLocked and normal history.
	resp, body = adminDo(t, http.MethodPost, httpServer.URL+"/admin/api/savevault/scopes/pc/fh5/promote",
		map[string]any{"snapshot": c2.ID, "expectedHead": second.ID, "clearConflicts": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("promote+clear status = %d: %s", resp.StatusCode, body)
	}
	var promoted struct {
		saveVaultScopeSnapshot
		ClearedConflicts int   `json:"clearedConflicts"`
		FreedBytes       int64 `json:"freedBytes"`
	}
	if err := json.Unmarshal(body, &promoted); err != nil {
		t.Fatal(err)
	}
	if promoted.ID != c2.ID || promoted.ClearedConflicts != 1 || promoted.FreedBytes != int64(len("conflict-one")) {
		t.Fatalf("promote+clear response = %+v", promoted)
	}
	stored := readSaveVaultScopeState(t, s, "pc", "fh5")
	if stored.Head != c2.ID {
		t.Fatalf("head = %q, want %q", stored.Head, c2.ID)
	}
	if _, ok := stored.Snapshots[c1.ID]; ok {
		t.Fatalf("unlocked conflict %s should have been removed", c1.ID)
	}
	if _, ok := stored.Snapshots[cLocked.ID]; !ok {
		t.Fatalf("locked conflict %s should have been preserved", cLocked.ID)
	}
	if saveVaultArchiveExists(t, s, "pc", c1.Archive) {
		t.Fatalf("removed conflict archive %s still on disk", c1.Archive)
	}

	// Add another unlocked conflict and test POST .../conflicts/clear.
	c3 := createSaveVaultConflictSnapshot(t, httpServer.URL, "pc", "fh5", "conf-3", first.ID, []byte("conflict-three"))
	resp, body = adminDo(t, http.MethodPost, httpServer.URL+"/admin/api/savevault/scopes/pc/fh5/conflicts/clear", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear conflicts status = %d: %s", resp.StatusCode, body)
	}
	var cleared struct {
		Removed       int   `json:"removed"`
		SkippedLocked int   `json:"skippedLocked"`
		FreedBytes    int64 `json:"freedBytes"`
	}
	if err := json.Unmarshal(body, &cleared); err != nil {
		t.Fatal(err)
	}
	if cleared.Removed != 1 || cleared.SkippedLocked != 1 || cleared.FreedBytes != int64(len("conflict-three")) {
		t.Fatalf("clear conflicts response = %+v", cleared)
	}
	if saveVaultArchiveExists(t, s, "pc", c3.Archive) {
		t.Fatalf("cleared conflict archive %s still on disk", c3.Archive)
	}
}

func TestSameDeviceConflictSupersedesOlderUnlockedCopy(t *testing.T) {
	s, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()

	first := createSaveVaultSnapshot(t, httpServer.URL, "pc", "fh5", "snap-1", "", "", []byte("base"))
	_ = createSaveVaultSnapshot(t, httpServer.URL, "pc", "fh5", "snap-2", first.ID, first.ID, []byte("head"))

	putConflictWithDevice := func(id, device string, content []byte) saveVaultScopeCandidate {
		archive := "fh5__main__" + id + ".zip"
		if status := putSaveVaultScopeArchive(t, httpServer.URL, "pc", archive, content); status != http.StatusCreated {
			t.Fatalf("archive %s status = %d", archive, status)
		}
		candidate := saveVaultScopeCandidateFor(id, archive, first.ID, content, time.Now().UTC())
		payload, _ := json.Marshal(candidate)
		req := saveVaultScopeRequest(t, http.MethodPut, httpServer.URL+"/v1/savevault/scopes/pc/fh5/conflicts/"+id, payload)
		req.Header.Set("X-SaveVault-Device-ID", device)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("conflict %s status = %d", id, resp.StatusCode)
		}
		return candidate
	}

	oldTwo := putConflictWithDevice("conf-two-1", "dev-two", []byte("old-two-save"))
	otherDev := putConflictWithDevice("conf-deck-1", "dev-deck", []byte("deck-save"))
	newTwo := putConflictWithDevice("conf-two-2", "dev-two", []byte("new-two-save"))

	stored := readSaveVaultScopeState(t, s, "pc", "fh5")
	if _, ok := stored.Snapshots[oldTwo.ID]; ok {
		t.Fatalf("older same-device conflict %s should have been superseded", oldTwo.ID)
	}
	if _, ok := stored.Snapshots[newTwo.ID]; !ok {
		t.Fatalf("latest same-device conflict %s missing", newTwo.ID)
	}
	if _, ok := stored.Snapshots[otherDev.ID]; !ok {
		t.Fatalf("other-device conflict %s should have been preserved", otherDev.ID)
	}
	if saveVaultArchiveExists(t, s, "pc", oldTwo.Archive) {
		t.Fatalf("superseded conflict archive %s still on disk", oldTwo.Archive)
	}
}
