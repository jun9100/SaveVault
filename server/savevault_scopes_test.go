// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

func saveVaultScopeRequest(t *testing.T, method, url string, body []byte) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("X-SaveVault-Device-ID", "desktop-1")
	request.Header.Set("X-SaveVault-Device-Name", "Test Desktop")
	request.Header.Set("X-SaveVault-Platform", "savevault-windows")
	return request
}

func putSaveVaultScopeArchive(t *testing.T, baseURL, kind, name string, contents []byte) int {
	t.Helper()
	request := saveVaultScopeRequest(t, http.MethodPut, baseURL+"/v1/savevault/files/"+kind+"/"+name, contents)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	return response.StatusCode
}

func saveVaultScopeCandidateFor(id, archive, parent string, contents []byte, createdAt time.Time) saveVaultScopeCandidate {
	digest := sha256.Sum256(contents)
	return saveVaultScopeCandidate{
		ID:            id,
		Archive:       archive,
		Parent:        parent,
		ContentDigest: hex.EncodeToString(digest[:]),
		CreatedAt:     createdAt.UTC(),
	}
}

func putSaveVaultScopeCandidate(t *testing.T, baseURL, path, expectedHead string, candidate saveVaultScopeCandidate) (*http.Response, []byte) {
	t.Helper()
	payload, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	request := saveVaultScopeRequest(t, http.MethodPut, baseURL+path, payload)
	if expectedHead != "-" {
		request.Header.Set("If-Match", `"`+expectedHead+`"`)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response, body
}

func TestSaveVaultScopesUseCASRetainConflictsAndPersist(t *testing.T) {
	dataDir := t.TempDir()
	s, err := newServer(dataDir, testToken, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s.handler())
	defer httpServer.Close()

	base := httpServer.URL + "/v1/savevault/scopes/switch/odyssey"
	unauthorized, err := http.NewRequest(http.MethodGet, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(unauthorized)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized scope status = %d, want 401", response.StatusCode)
	}

	// A new endpoint answers an empty scope so clients can distinguish a CAS-capable
	// game-save service from an older service that returns route-level 404.
	emptyScope := saveVaultScopeRequest(t, http.MethodGet, base, nil)
	response, err = http.DefaultClient.Do(emptyScope)
	if err != nil {
		t.Fatal(err)
	}
	var empty saveVaultScopeResponse
	if err := json.NewDecoder(response.Body).Decode(&empty); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || empty.Head != nil || len(empty.Snapshots) != 0 {
		t.Fatalf("empty scope status = %d, response=%+v", response.StatusCode, empty)
	}

	archiveOne := []byte("odyssey-progress-1788")
	archiveOneName := "odyssey__main__snapshot-1.zip"
	if status := putSaveVaultScopeArchive(t, httpServer.URL, "switch", archiveOneName, archiveOne); status != http.StatusCreated {
		t.Fatalf("first archive status = %d, want 201", status)
	}
	first := saveVaultScopeCandidateFor("snapshot-1", archiveOneName, "", archiveOne, time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC))
	response, body := putSaveVaultScopeCandidate(t, httpServer.URL,
		"/v1/savevault/scopes/switch/odyssey/snapshots/"+first.ID, "", first)
	if response.StatusCode != http.StatusCreated {
		response.Body.Close()
		t.Fatalf("first CAS status = %d: %s", response.StatusCode, body)
	}
	if response.Header.Get("ETag") != `"snapshot-1"` {
		response.Body.Close()
		t.Fatalf("first ETag = %q", response.Header.Get("ETag"))
	}
	response.Body.Close()

	archiveTwo := []byte("odyssey-progress-1791")
	archiveTwoName := "odyssey__main__snapshot-2.zip"
	if status := putSaveVaultScopeArchive(t, httpServer.URL, "switch", archiveTwoName, archiveTwo); status != http.StatusCreated {
		t.Fatalf("second archive status = %d, want 201", status)
	}
	second := saveVaultScopeCandidateFor("snapshot-2", archiveTwoName, first.ID, archiveTwo, time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC))
	response, body = putSaveVaultScopeCandidate(t, httpServer.URL,
		"/v1/savevault/scopes/switch/odyssey/snapshots/"+second.ID, first.ID, second)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("second CAS status = %d: %s", response.StatusCode, body)
	}

	archiveStale := []byte("odyssey-progress-1789")
	archiveStaleName := "odyssey__main__snapshot-stale.zip"
	if status := putSaveVaultScopeArchive(t, httpServer.URL, "switch", archiveStaleName, archiveStale); status != http.StatusCreated {
		t.Fatalf("stale archive status = %d, want 201", status)
	}
	stale := saveVaultScopeCandidateFor("snapshot-stale", archiveStaleName, first.ID, archiveStale, time.Date(2026, 10, 26, 8, 30, 0, 0, time.UTC))
	response, body = putSaveVaultScopeCandidate(t, httpServer.URL,
		"/v1/savevault/scopes/switch/odyssey/snapshots/"+stale.ID, first.ID, stale)
	if response.StatusCode != http.StatusConflict || !bytes.Contains(body, []byte(`"head":"snapshot-2"`)) {
		response.Body.Close()
		t.Fatalf("stale CAS status = %d: %s", response.StatusCode, body)
	}
	response.Body.Close()

	response, body = putSaveVaultScopeCandidate(t, httpServer.URL,
		"/v1/savevault/scopes/switch/odyssey/conflicts/"+stale.ID, "-", stale)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("retain conflict status = %d: %s", response.StatusCode, body)
	}

	readScope := saveVaultScopeRequest(t, http.MethodGet, base, nil)
	response, err = http.DefaultClient.Do(readScope)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("scope status = %d", response.StatusCode)
	}
	var scope saveVaultScopeResponse
	if err := json.NewDecoder(response.Body).Decode(&scope); err != nil {
		t.Fatal(err)
	}
	if scope.Head == nil || scope.Head.ID != second.ID || len(scope.Snapshots) != 3 || len(scope.Conflicts) != 1 ||
		scope.Conflicts[0].ID != stale.ID || scope.Conflicts[0].ConflictOf != second.ID {
		t.Fatalf("scope response = %+v", scope)
	}

	s.mu.Lock()
	events := s.recentEventsLocked(maxEvents)
	s.mu.Unlock()
	saveVaultEvents := make([]event, 0, 3)
	for _, item := range events {
		if item.Scope == saveVaultEventScope("switch", "odyssey") {
			saveVaultEvents = append(saveVaultEvents, item)
		}
	}
	if len(saveVaultEvents) != 3 || saveVaultEvents[0].Kind != eventConflict || saveVaultEvents[0].Snapshot != stale.ID ||
		saveVaultEvents[1].Kind != eventUpload || saveVaultEvents[1].Snapshot != second.ID ||
		saveVaultEvents[2].Kind != eventUpload || saveVaultEvents[2].Snapshot != first.ID {
		t.Fatalf("SaveVault activity events = %+v", events)
	}

	// Both the authoritative archive and divergent progress remain recoverable.
	for _, name := range []string{archiveTwoName, archiveStaleName} {
		remove := saveVaultScopeRequest(t, http.MethodDelete, httpServer.URL+"/v1/savevault/files/switch/"+name, nil)
		response, err := http.DefaultClient.Do(remove)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusConflict {
			t.Fatalf("referenced archive deletion (%s) status = %d, want 409", name, response.StatusCode)
		}
	}

	// State survives a service restart and still identifies the same authority.
	restarted, err := newServer(dataDir, testToken, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	restartedHTTP := httptest.NewServer(restarted.handler())
	defer restartedHTTP.Close()
	head := saveVaultScopeRequest(t, http.MethodGet, restartedHTTP.URL+"/v1/savevault/scopes/switch/odyssey/head", nil)
	response, err = http.DefaultClient.Do(head)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("ETag") != `"snapshot-2"` {
		t.Fatalf("restarted head status = %d, ETag=%q", response.StatusCode, response.Header.Get("ETag"))
	}
	var restoredHead saveVaultScopeSnapshot
	if err := json.NewDecoder(response.Body).Decode(&restoredHead); err != nil {
		t.Fatal(err)
	}
	if restoredHead.ID != second.ID || restoredHead.ContentDigest != second.ContentDigest {
		t.Fatalf("restarted head = %+v", restoredHead)
	}
}

func TestSaveVaultNativeReadsDoNotRecordDeviceHeartbeat(t *testing.T) {
	s, err := newServer(t.TempDir(), testToken, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s.handler())
	defer httpServer.Close()

	const archiveName = "read-only-game__main__snapshot-1.zip"
	const archiveBody = "test save"
	if status := putSaveVaultScopeArchive(t, httpServer.URL, "pc", archiveName, []byte(archiveBody)); status != http.StatusCreated {
		t.Fatalf("archive upload status = %d", status)
	}
	candidate := saveVaultScopeCandidateFor("snapshot-1", archiveName, "", []byte(archiveBody), time.Now())
	response, body := putSaveVaultScopeCandidate(t, httpServer.URL,
		"/v1/savevault/scopes/pc/read-only-game/snapshots/"+candidate.ID, "", candidate)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("scope upload status = %d: %s", response.StatusCode, body)
	}

	s.mu.Lock()
	devicesBefore := len(s.dashboard.Devices)
	lastSeen := s.dashboard.Devices["desktop-1"].LastSeen
	s.mu.Unlock()
	for _, path := range []string{
		"/v1/savevault/files",
		"/v1/savevault/files/pc/" + archiveName,
		"/v1/savevault/scopes/pc/read-only-game",
		"/v1/savevault/scopes/pc/read-only-game/head",
	} {
		request := saveVaultScopeRequest(t, http.MethodGet, httpServer.URL+path, nil)
		request.Header.Set("X-SaveVault-Device-ID", "read-only-client")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("read %s status = %d", path, response.StatusCode)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.dashboard.Devices) != devicesBefore || !s.dashboard.Devices["desktop-1"].LastSeen.Equal(lastSeen) {
		t.Fatalf("read-only requests changed device state: %+v", s.dashboard.Devices)
	}
	if _, exists := s.dashboard.Devices["read-only-client"]; exists {
		t.Fatalf("read-only requests recorded a device: %+v", s.dashboard.Devices)
	}
}

func TestSaveVaultArchivesAreImmutableAndTypesAreIsolated(t *testing.T) {
	s, err := newServer(t.TempDir(), testToken, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s.handler())
	defer httpServer.Close()

	if status := putSaveVaultScopeArchive(t, httpServer.URL, "pc", "same.zip", []byte("pc-save")); status != http.StatusCreated {
		t.Fatalf("first archive status = %d", status)
	}
	if status := putSaveVaultScopeArchive(t, httpServer.URL, "pc", "same.zip", []byte("pc-save")); status != http.StatusOK {
		t.Fatalf("idempotent archive retry status = %d, want 200", status)
	}
	if status := putSaveVaultScopeArchive(t, httpServer.URL, "pc", "same.zip", []byte("different-save")); status != http.StatusConflict {
		t.Fatalf("different archive retry status = %d, want 409", status)
	}
	if status := putSaveVaultScopeArchive(t, httpServer.URL, "switch", "same.zip", []byte("switch-save")); status != http.StatusCreated {
		t.Fatalf("different type archive status = %d, want 201", status)
	}
}

func TestSaveVaultArchivePublicationIsCreateOnlyUnderConcurrency(t *testing.T) {
	s, err := newServer(t.TempDir(), testToken, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s.handler())
	defer httpServer.Close()

	const attempts = 16
	start := make(chan struct{})
	results := make(chan int, attempts)
	bodies := make(map[string]struct{}, attempts)
	var bodiesMu sync.Mutex
	var workers sync.WaitGroup
	for index := 0; index < attempts; index++ {
		body := []byte(fmt.Sprintf("candidate-save-%02d", index))
		bodiesMu.Lock()
		bodies[string(body)] = struct{}{}
		bodiesMu.Unlock()
		workers.Add(1)
		go func(body []byte) {
			defer workers.Done()
			<-start
			request, err := http.NewRequest(http.MethodPut,
				httpServer.URL+"/v1/savevault/files/switch/odyssey__main__snap_1--dev-odin2.zip",
				bytes.NewReader(body))
			if err != nil {
				results <- -1
				return
			}
			request.Header.Set("Authorization", "Bearer "+testToken)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				results <- -1
				return
			}
			response.Body.Close()
			results <- response.StatusCode
		}(body)
	}
	close(start)
	workers.Wait()
	close(results)

	created, conflicts := 0, 0
	for status := range results {
		switch status {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("concurrent upload status = %d, want only 201 or 409", status)
		}
	}
	if created != 1 || conflicts != attempts-1 {
		t.Fatalf("concurrent publication created=%d conflicts=%d, want 1 and %d", created, conflicts, attempts-1)
	}

	request := saveVaultScopeRequest(t, http.MethodGet,
		httpServer.URL+"/v1/savevault/files/switch/odyssey__main__snap_1--dev-odin2.zip", nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("published archive read status = %d", response.StatusCode)
	}
	bodiesMu.Lock()
	_, known := bodies[string(got)]
	bodiesMu.Unlock()
	if !known {
		t.Fatalf("published archive contains unexpected or partial data: %q", got)
	}
}

func TestSaveVaultScopeAuditShowsAuthorityAndRetainedConflict(t *testing.T) {
	s, err := newServer(t.TempDir(), testToken, 1024*1024, testAdminToken)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s.handler())
	defer httpServer.Close()

	firstBody := []byte("authoritative progress")
	firstArchive := "odyssey__main__snapshot-head.zip"
	if status := putSaveVaultScopeArchive(t, httpServer.URL, "switch", firstArchive, firstBody); status != http.StatusCreated {
		t.Fatalf("head archive status = %d", status)
	}
	first := saveVaultScopeCandidateFor("head-1", firstArchive, "", firstBody, time.Now().UTC())
	response, body := putSaveVaultScopeCandidate(t, httpServer.URL,
		"/v1/savevault/scopes/switch/odyssey/snapshots/"+first.ID, "", first)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("head status = %d: %s", response.StatusCode, body)
	}

	conflictBody := []byte("retained divergent progress")
	conflictArchive := "odyssey__main__snapshot-conflict.zip"
	if status := putSaveVaultScopeArchive(t, httpServer.URL, "switch", conflictArchive, conflictBody); status != http.StatusCreated {
		t.Fatalf("conflict archive status = %d", status)
	}
	conflict := saveVaultScopeCandidateFor("conflict-1", conflictArchive, "other-parent", conflictBody, time.Now().UTC())
	response, body = putSaveVaultScopeCandidate(t, httpServer.URL,
		"/v1/savevault/scopes/switch/odyssey/conflicts/"+conflict.ID, "-", conflict)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("conflict status = %d: %s", response.StatusCode, body)
	}

	audits := s.listSaveVaultScopeAudits()
	if len(audits) != 1 {
		t.Fatalf("audits = %+v", audits)
	}
	audit := audits[0]
	if audit.Type != "switch" || audit.GameID != "odyssey" || audit.Head == nil || audit.Head.ID != first.ID ||
		audit.SnapshotCount != 2 || audit.ConflictCount != 1 || audit.Conflicts[0].ID != conflict.ID ||
		audit.HeadDeviceName != "Test Desktop" || audit.HeadPlatform != "savevault-windows" {
		t.Fatalf("audit = %+v", audit)
	}

	response, body = adminDo(t, http.MethodGet, httpServer.URL+"/admin/api/savevault/scopes", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("scope audit API status = %d: %s", response.StatusCode, body)
	}
	var listed struct {
		Scopes []saveVaultScopeAudit `json:"scopes"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Scopes) != 1 || listed.Scopes[0].Head == nil || listed.Scopes[0].Head.ID != first.ID ||
		listed.Scopes[0].ConflictCount != 1 {
		t.Fatalf("scope audit API = %+v", listed)
	}

	response, body = adminDo(t, http.MethodGet,
		httpServer.URL+"/admin/api/savevault/scopes/switch/odyssey", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("scope detail API status = %d: %s", response.StatusCode, body)
	}
	var detail saveVaultScopeAudit
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Head == nil || detail.Head.ID != first.ID || len(detail.Snapshots) != 2 || len(detail.Conflicts) != 1 {
		t.Fatalf("scope detail API = %+v", detail)
	}
}

func TestVerifyDetectsCorruptAndMissingSaveVaultArchives(t *testing.T) {
	s, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()

	publish := func(game, archive string, body []byte) {
		t.Helper()
		if status := putSaveVaultScopeArchive(t, httpServer.URL, "pc", archive, body); status != http.StatusCreated {
			t.Fatalf("archive %s status = %d", archive, status)
		}
		candidate := saveVaultScopeCandidateFor("snapshot-1", archive, "", body, time.Now().UTC())
		response, raw := putSaveVaultScopeCandidate(t, httpServer.URL, "/v1/savevault/scopes/pc/"+game+"/snapshots/snapshot-1", "", candidate)
		response.Body.Close()
		if response.StatusCode != http.StatusCreated {
			t.Fatalf("publish %s status = %d: %s", game, response.StatusCode, raw)
		}
	}
	publish("good", "good__main__snapshot-1.zip", []byte("good save"))
	publish("tampered", "tampered__main__snapshot-1.zip", []byte("original save"))
	publish("gone", "gone__main__snapshot-1.zip", []byte("deleted save"))

	tampered, _ := s.saveVaultPath("pc", "tampered__main__snapshot-1.zip")
	if err := os.WriteFile(tampered, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	gone, _ := s.saveVaultPath("pc", "gone__main__snapshot-1.zip")
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}

	response, body := adminDo(t, http.MethodPost, httpServer.URL+"/admin/api/maintenance/verify", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("verify status = %d", response.StatusCode)
	}
	var report verifyReport
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	problems := map[string]string{}
	for _, problem := range report.Problems {
		if problem.Type != "pc" || len(problem.Scopes) != 1 {
			t.Fatalf("problem = %+v", problem)
		}
		problems[problem.Scopes[0]] = problem.Problem
	}
	if report.Objects != 2 || len(problems) != 2 || problems["pc/tampered"] != "corrupt" || problems["pc/gone"] != "missing" {
		t.Fatalf("verify report = %+v", report)
	}
}
