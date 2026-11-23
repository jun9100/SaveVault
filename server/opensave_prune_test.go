// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func publishOpenSavePruneVersion(t *testing.T, baseURL, kind, gameID, snapshotID, archive string, contents []byte, createdAt time.Time, parent string) {
	t.Helper()
	status := putOpenSaveScopeArchive(t, baseURL, kind, archive, contents)
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("archive %s status = %d", archive, status)
	}
	candidate := openSaveScopeCandidateFor(snapshotID, archive, parent, contents, createdAt)
	response, body := putOpenSaveScopeCandidate(t, baseURL,
		"/v1/opensave/scopes/"+kind+"/"+gameID+"/snapshots/"+snapshotID, parent, candidate)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("snapshot %s status = %d: %s", snapshotID, response.StatusCode, body)
	}
}

func publishOpenSavePruneConflict(t *testing.T, baseURL, kind, gameID, snapshotID, archive string, contents []byte, createdAt time.Time, parent string) {
	t.Helper()
	if status := putOpenSaveScopeArchive(t, baseURL, kind, archive, contents); status != http.StatusCreated {
		t.Fatalf("conflict archive %s status = %d", archive, status)
	}
	candidate := openSaveScopeCandidateFor(snapshotID, archive, parent, contents, createdAt)
	response, body := putOpenSaveScopeCandidate(t, baseURL,
		"/v1/opensave/scopes/"+kind+"/"+gameID+"/conflicts/"+snapshotID, "-", candidate)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("conflict %s status = %d: %s", snapshotID, response.StatusCode, body)
	}
}

func fetchOpenSaveScopeAudit(t *testing.T, baseURL, kind, gameID string) openSaveScopeAudit {
	t.Helper()
	response, body := adminDo(t, http.MethodGet,
		baseURL+"/admin/api/opensave/scopes/"+kind+"/"+gameID, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("scope audit status = %d: %s", response.StatusCode, body)
	}
	var audit openSaveScopeAudit
	if err := json.Unmarshal(body, &audit); err != nil {
		t.Fatal(err)
	}
	return audit
}

func pruneOpenSaveScope(t *testing.T, baseURL, kind, gameID string, keep int, dryRun bool) openSavePruneResponse {
	t.Helper()
	response, body := adminDo(t, http.MethodPost,
		baseURL+"/admin/api/opensave/scopes/"+kind+"/"+gameID+"/prune",
		map[string]any{"keep": keep, "dryRun": dryRun})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("prune status = %d: %s", response.StatusCode, body)
	}
	var report openSavePruneResponse
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func openSaveArchiveExists(t *testing.T, s *server, kind, name string) bool {
	t.Helper()
	path, ok := s.openSavePath(kind, name)
	if !ok {
		t.Fatalf("invalid archive path for %s", name)
	}
	_, err := os.Stat(path)
	return err == nil
}

func readOpenSaveCatalogForTest(t *testing.T, dataDir string) openSaveCatalog {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(dataDir, "opensave-native.json"))
	if err != nil {
		t.Fatal(err)
	}
	var catalog openSaveCatalog
	if err := json.Unmarshal(contents, &catalog); err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestOpenSaveScopePruneKeepsHeadConflictsAndRecentVersions(t *testing.T) {
	dataDir := t.TempDir()
	s, httpServer := newAdminTestServer(t, dataDir)
	defer httpServer.Close()

	const kind, gameID = "pc", "prune-game"
	v1 := []byte("version-one")
	v2 := []byte("version-two!")
	v3 := []byte("version-three")
	v4 := []byte("version-four!")
	v5 := []byte("version-five!")
	conflictBody := []byte("conflict-copy!!")
	archive := func(id string) string { return gameID + "__main__" + id + ".zip" }
	started := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)

	publishOpenSavePruneVersion(t, httpServer.URL, kind, gameID, "v1", archive("v1"), v1, started, "")
	publishOpenSavePruneConflict(t, httpServer.URL, kind, gameID, "conflict", archive("conflict"), conflictBody, started.Add(30*time.Minute), "other-branch")
	publishOpenSavePruneVersion(t, httpServer.URL, kind, gameID, "v2", archive("v2"), v2, started.Add(time.Hour), "v1")
	publishOpenSavePruneVersion(t, httpServer.URL, kind, gameID, "v3", archive("v3"), v3, started.Add(2*time.Hour), "v2")
	publishOpenSavePruneVersion(t, httpServer.URL, kind, gameID, "v4", archive("v4"), v4, started.Add(3*time.Hour), "v3")
	publishOpenSavePruneVersion(t, httpServer.URL, kind, gameID, "v5", archive("v5"), v5, started.Add(4*time.Hour), "v4")

	wantBefore := int64(len(v1) + len(v2) + len(v3) + len(v4) + len(v5) + len(conflictBody))
	wantAfter := int64(len(v1) + len(v4) + len(v5) + len(conflictBody))
	wantFreed := int64(len(v2) + len(v3))

	audit := fetchOpenSaveScopeAudit(t, httpServer.URL, kind, gameID)
	if audit.Head == nil || audit.Head.ID != "v5" || audit.SnapshotCount != 6 || audit.ConflictCount != 1 ||
		len(audit.Conflicts) != 1 || audit.Conflicts[0].ID != "conflict" || audit.Conflicts[0].ConflictOf != "v1" {
		t.Fatalf("initial audit = %+v", audit)
	}
	if audit.ArchiveBytes != wantBefore {
		t.Fatalf("initial archiveBytes = %d, want %d", audit.ArchiveBytes, wantBefore)
	}

	response, body := adminDo(t, http.MethodGet, httpServer.URL+"/admin/api/opensave/scopes", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("scope list status = %d: %s", response.StatusCode, body)
	}
	var listed struct {
		Scopes []openSaveScopeAudit `json:"scopes"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Scopes) != 1 || listed.Scopes[0].ArchiveBytes != wantBefore {
		t.Fatalf("scope list = %+v", listed.Scopes)
	}

	dryRun := pruneOpenSaveScope(t, httpServer.URL, kind, gameID, 2, true)
	if dryRun != (openSavePruneResponse{Removed: 2, FreedBytes: wantFreed, Kept: 4, DryRun: true}) {
		t.Fatalf("dry-run report = %+v", dryRun)
	}
	audit = fetchOpenSaveScopeAudit(t, httpServer.URL, kind, gameID)
	if audit.SnapshotCount != 6 || audit.ArchiveBytes != wantBefore {
		t.Fatalf("dry run changed scope: %+v", audit)
	}
	for _, id := range []string{"v2", "v3"} {
		if !openSaveArchiveExists(t, s, kind, archive(id)) {
			t.Fatalf("dry run deleted %s", archive(id))
		}
	}

	report := pruneOpenSaveScope(t, httpServer.URL, kind, gameID, 2, false)
	if report != (openSavePruneResponse{Removed: 2, FreedBytes: wantFreed, Kept: 4, DryRun: false}) {
		t.Fatalf("prune report = %+v", report)
	}

	audit = fetchOpenSaveScopeAudit(t, httpServer.URL, kind, gameID)
	if audit.Head == nil || audit.Head.ID != "v5" || audit.SnapshotCount != 4 || audit.ConflictCount != 1 ||
		audit.ArchiveBytes != wantAfter {
		t.Fatalf("pruned audit = %+v", audit)
	}
	response, body = adminDo(t, http.MethodGet, httpServer.URL+"/admin/api/opensave/scopes", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("pruned scope list status = %d: %s", response.StatusCode, body)
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Scopes) != 1 || listed.Scopes[0].ArchiveBytes != wantAfter {
		t.Fatalf("pruned scope list = %+v", listed.Scopes)
	}
	retained := make(map[string]bool, len(audit.Snapshots))
	for _, snapshot := range audit.Snapshots {
		retained[snapshot.ID] = true
	}
	for _, id := range []string{"v1", "v4", "v5", "conflict"} {
		if !retained[id] {
			t.Fatalf("prune removed retained snapshot %s: %+v", id, audit.Snapshots)
		}
	}
	for _, id := range []string{"v2", "v3"} {
		if retained[id] {
			t.Fatalf("prune kept removed snapshot %s: %+v", id, audit.Snapshots)
		}
		if openSaveArchiveExists(t, s, kind, archive(id)) {
			t.Fatalf("prune left archive %s on disk", archive(id))
		}
	}
	for _, id := range []string{"v1", "v4", "v5", "conflict"} {
		if !openSaveArchiveExists(t, s, kind, archive(id)) {
			t.Fatalf("prune deleted retained archive %s", archive(id))
		}
	}

	catalog := readOpenSaveCatalogForTest(t, dataDir)
	for _, id := range []string{"v2", "v3"} {
		if _, ok := catalog.Files[openSaveCatalogKey(kind, archive(id))]; ok {
			t.Fatalf("catalog retained pruned archive %s", archive(id))
		}
	}
	for _, id := range []string{"v1", "v4", "v5", "conflict"} {
		if _, ok := catalog.Files[openSaveCatalogKey(kind, archive(id))]; !ok {
			t.Fatalf("catalog lost retained archive %s", archive(id))
		}
	}

	s.mu.Lock()
	events := s.recentEventsLocked(maxEvents)
	s.mu.Unlock()
	pruneEvents := 0
	for _, item := range events {
		if item.Kind == eventPruneScope && item.Scope == openSaveEventScope(kind, gameID) {
			pruneEvents++
		}
	}
	if pruneEvents != 1 {
		t.Fatalf("prune events = %d, want 1: %+v", pruneEvents, events)
	}

	restarted, err := newServer(dataDir, testToken, 1024*1024, testAdminToken)
	if err != nil {
		t.Fatal(err)
	}
	restartedAudits := restarted.listOpenSaveScopeAudits()
	if len(restartedAudits) != 1 {
		t.Fatalf("restarted audits = %+v", restartedAudits)
	}
	restartedAudit := restartedAudits[0]
	if restartedAudit.Head == nil || restartedAudit.Head.ID != "v5" || restartedAudit.SnapshotCount != 4 ||
		restartedAudit.ConflictCount != 1 || restartedAudit.ArchiveBytes != wantAfter {
		t.Fatalf("restarted audit = %+v", restartedAudit)
	}
	if restartedAudit.ArchiveBytes >= wantBefore {
		t.Fatalf("restarted archiveBytes did not decrease: before=%d after=%d", wantBefore, restartedAudit.ArchiveBytes)
	}
}

func TestOpenSaveScopePruneRequiresAdminAndValidKeep(t *testing.T) {
	_, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()

	const kind, gameID = "switch", "prune-auth"
	archiveName := gameID + "__main__v1.zip"
	publishOpenSavePruneVersion(t, httpServer.URL, kind, gameID, "v1", archiveName, []byte("save"), time.Now().UTC(), "")
	url := httpServer.URL + "/admin/api/opensave/scopes/" + kind + "/" + gameID + "/prune"

	for _, request := range []map[string]any{
		{"keep": 0},
		{"dryRun": true},
		{"keep": -1},
		{"keep": 1001},
	} {
		response, body := adminDo(t, http.MethodPost, url, request)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("keep request %+v status = %d: %s", request, response.StatusCode, body)
		}
	}

	request := openSaveScopeRequest(t, http.MethodPost, url, []byte(`{"keep":2}`))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("non-admin prune status = %d, want 401", response.StatusCode)
	}

	audit := fetchOpenSaveScopeAudit(t, httpServer.URL, kind, gameID)
	if audit.SnapshotCount != 1 || audit.Head == nil || audit.Head.ID != "v1" {
		t.Fatalf("invalid requests changed scope: %+v", audit)
	}
}

func TestOpenSaveScopePruneKeepsSharedArchiveWhileReferenced(t *testing.T) {
	dataDir := t.TempDir()
	s, httpServer := newAdminTestServer(t, dataDir)
	defer httpServer.Close()

	const kind, gameID = "pc", "shared-game"
	sharedName := gameID + "__main__shared.zip"
	uniqueName := gameID + "__main__unique.zip"
	shared := []byte("shared archive bytes")
	unique := []byte("unique archive bytes")
	started := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)

	publishOpenSavePruneVersion(t, httpServer.URL, kind, gameID, "v1", sharedName, shared, started, "")
	publishOpenSavePruneVersion(t, httpServer.URL, kind, gameID, "v2", sharedName, shared, started.Add(time.Hour), "v1")
	publishOpenSavePruneVersion(t, httpServer.URL, kind, gameID, "v3", uniqueName, unique, started.Add(2*time.Hour), "v2")

	report := pruneOpenSaveScope(t, httpServer.URL, kind, gameID, 2, false)
	if report != (openSavePruneResponse{Removed: 1, Kept: 2}) {
		t.Fatalf("first prune report = %+v", report)
	}
	if !openSaveArchiveExists(t, s, kind, sharedName) {
		t.Fatalf("shared archive was deleted while v2 still references it")
	}
	catalog := readOpenSaveCatalogForTest(t, dataDir)
	if _, ok := catalog.Files[openSaveCatalogKey(kind, sharedName)]; !ok {
		t.Fatalf("catalog lost shared archive while referenced")
	}
	audit := fetchOpenSaveScopeAudit(t, httpServer.URL, kind, gameID)
	if audit.ArchiveBytes != int64(len(shared)+len(unique)) {
		t.Fatalf("archiveBytes after first prune = %d", audit.ArchiveBytes)
	}

	report = pruneOpenSaveScope(t, httpServer.URL, kind, gameID, 1, false)
	if report != (openSavePruneResponse{Removed: 1, FreedBytes: int64(len(shared)), Kept: 1}) {
		t.Fatalf("second prune report = %+v", report)
	}
	if openSaveArchiveExists(t, s, kind, sharedName) {
		t.Fatalf("shared archive was not deleted after its last reference")
	}
	catalog = readOpenSaveCatalogForTest(t, dataDir)
	if _, ok := catalog.Files[openSaveCatalogKey(kind, sharedName)]; ok {
		t.Fatalf("catalog retained unreferenced shared archive")
	}
	audit = fetchOpenSaveScopeAudit(t, httpServer.URL, kind, gameID)
	if audit.SnapshotCount != 1 || audit.ArchiveBytes != int64(len(unique)) {
		t.Fatalf("audit after second prune = %+v", audit)
	}
}

func TestOpenSaveScopePrunePersistsStateBeforeDeletingArchives(t *testing.T) {
	dataDir := t.TempDir()
	s, httpServer := newAdminTestServer(t, dataDir)
	defer httpServer.Close()

	const kind, gameID = "pc", "persist-failure"
	v1Name := gameID + "__main__v1.zip"
	v2Name := gameID + "__main__v2.zip"
	started := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	publishOpenSavePruneVersion(t, httpServer.URL, kind, gameID, "v1", v1Name, []byte("first"), started, "")
	publishOpenSavePruneVersion(t, httpServer.URL, kind, gameID, "v2", v2Name, []byte("second"), started.Add(time.Hour), "v1")

	statePath := s.openSaveStatePath()
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0o700); err != nil {
		t.Fatal(err)
	}

	response, body := adminDo(t, http.MethodPost,
		httpServer.URL+"/admin/api/opensave/scopes/"+kind+"/"+gameID+"/prune",
		map[string]any{"keep": 1})
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("persist failure status = %d: %s", response.StatusCode, body)
	}
	for _, name := range []string{v1Name, v2Name} {
		if !openSaveArchiveExists(t, s, kind, name) {
			t.Fatalf("persist failure deleted archive %s", name)
		}
	}
	audit := fetchOpenSaveScopeAudit(t, httpServer.URL, kind, gameID)
	if audit.SnapshotCount != 2 || audit.Head == nil || audit.Head.ID != "v2" {
		t.Fatalf("persist failure changed in-memory state: %+v", audit)
	}
	s.mu.Lock()
	events := s.recentEventsLocked(maxEvents)
	s.mu.Unlock()
	for _, item := range events {
		if item.Kind == eventPruneScope {
			t.Fatalf("persist failure recorded a prune event: %+v", item)
		}
	}
}

func TestOpenSaveScopePruneReportsArchiveDeletionFailures(t *testing.T) {
	dataDir := t.TempDir()
	s, httpServer := newAdminTestServer(t, dataDir)
	defer httpServer.Close()

	const kind, gameID = "pc", "delete-failure"
	v1Name := gameID + "__main__v1.zip"
	v2Name := gameID + "__main__v2.zip"
	started := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	publishOpenSavePruneVersion(t, httpServer.URL, kind, gameID, "v1", v1Name, []byte("first"), started, "")
	publishOpenSavePruneVersion(t, httpServer.URL, kind, gameID, "v2", v2Name, []byte("second"), started.Add(time.Hour), "v1")

	// Replace the archive that prune wants to remove with a non-empty
	// directory so its unlink fails after the state has already committed.
	v1Path, ok := s.openSavePath(kind, v1Name)
	if !ok {
		t.Fatal("invalid archive path")
	}
	if err := os.Remove(v1Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(v1Path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v1Path, "blocker"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	report := pruneOpenSaveScope(t, httpServer.URL, kind, gameID, 1, false)
	if report != (openSavePruneResponse{Removed: 1, Failed: 1, Kept: 1}) {
		t.Fatalf("delete failure report = %+v", report)
	}
	audit := fetchOpenSaveScopeAudit(t, httpServer.URL, kind, gameID)
	if audit.SnapshotCount != 1 || audit.Head == nil || audit.Head.ID != "v2" {
		t.Fatalf("audit after delete failure = %+v", audit)
	}
	catalog := readOpenSaveCatalogForTest(t, dataDir)
	if _, ok := catalog.Files[openSaveCatalogKey(kind, v1Name)]; !ok {
		t.Fatalf("catalog dropped an archive whose deletion failed")
	}
}
