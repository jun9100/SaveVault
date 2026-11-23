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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type coverFetchTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func newCoverFetchTestClock() *coverFetchTestClock {
	return &coverFetchTestClock{now: time.Now().UTC().Truncate(time.Second)}
}

func (clock *coverFetchTestClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *coverFetchTestClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	clock.mu.Unlock()
}

// coverFetchTestTransport forwards the production https requests to an
// httptest server while preserving the original URL for redirect handling and
// assertions. It also records concurrency and request order.
var coverFetchTestHTTPTransport = &http.Transport{}

type coverFetchTestTransport struct {
	target *url.URL

	mu      sync.Mutex
	urls    []string
	active  int
	maximum int
	delay   time.Duration
}

func newCoverFetchTestTransport(t *testing.T, upstream *httptest.Server) *coverFetchTestTransport {
	t.Helper()
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &coverFetchTestTransport{target: target}
}

func (transport *coverFetchTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	original := request.URL.Scheme + "://" + request.URL.Host + request.URL.RequestURI()
	transport.mu.Lock()
	transport.urls = append(transport.urls, original)
	transport.active++
	if transport.active > transport.maximum {
		transport.maximum = transport.active
	}
	delay := transport.delay
	transport.mu.Unlock()
	defer func() {
		transport.mu.Lock()
		transport.active--
		transport.mu.Unlock()
	}()
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
	}

	clone := request.Clone(request.Context())
	targetURL := *request.URL
	targetURL.Scheme = transport.target.Scheme
	targetURL.Host = transport.target.Host
	targetURL.User = nil
	clone.URL = &targetURL
	clone.Host = ""
	clone.Header.Set("X-Test-Original-Host", request.URL.Host)
	response, err := coverFetchTestHTTPTransport.RoundTrip(clone)
	if response != nil {
		// Keep redirect resolution on the original, allowlisted URL.
		response.Request = request
	}
	return response, err
}

func (transport *coverFetchTestTransport) Requests() []string {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return append([]string(nil), transport.urls...)
}

func (transport *coverFetchTestTransport) Count() int {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return len(transport.urls)
}

func (transport *coverFetchTestTransport) MaxConcurrency() int {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.maximum
}

func newCoverFetchTestServer(t *testing.T, upstream *httptest.Server) (*server, *httptest.Server, *coverFetchTestTransport) {
	t.Helper()
	s, httpServer := newAdminTestServer(t, t.TempDir())
	transport := newCoverFetchTestTransport(t, upstream)
	s.coverFetchTransport = transport
	s.coverFetchClient.Transport = transport
	clock := newCoverFetchTestClock()
	s.now = clock.Now
	t.Cleanup(func() {
		httpServer.Close()
		s.Close()
	})
	return s, httpServer, transport
}

func setCoverFetchForTest(t *testing.T, s *server, enabled bool) {
	t.Helper()
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	next := s.saveVaultMeta.clone()
	next.Settings.CoverFetch = enabled
	if err := s.persistSaveVaultMeta(next); err != nil {
		t.Fatal(err)
	}
	s.saveVaultMeta = next
}

func waitForSaveVaultMetaRecord(t *testing.T, s *server, gameKey string,
	condition func(saveVaultMetaRecord) bool) saveVaultMetaRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.metaMu.Lock()
		record := s.saveVaultMeta.Games[gameKey]
		s.metaMu.Unlock()
		if condition(record) {
			return record
		}
		if time.Now().After(deadline) {
			t.Fatalf("metadata for %s did not reach the expected state: %+v", gameKey, record)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForCoverRequests(t *testing.T, transport *coverFetchTestTransport, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for transport.Count() < count {
		if time.Now().After(deadline) {
			t.Fatalf("upstream requests = %d, want at least %d", transport.Count(), count)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func writeCoverTestImage(t *testing.T, w http.ResponseWriter, data []byte) {
	t.Helper()
	w.Header().Set("Content-Type", "text/html")
	if _, err := w.Write(data); err != nil {
		t.Errorf("write fake cover: %v", err)
	}
}

func TestCoverFetchDisabledMakesNoRequestsOrFiles(t *testing.T) {
	picture := testCoverImage(t, "png", 32, 20)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeCoverTestImage(t, w, picture)
	}))
	defer upstream.Close()
	s, httpServer, transport := newCoverFetchTestServer(t, upstream)
	publishSaveVaultScope(t, httpServer.URL, "pc", "disabled-game", "a", "", []byte("disabled"))
	if status, body := putSaveVaultIdentity(t, httpServer.URL, "pc", "disabled-game",
		map[string]string{"steamAppId": "1245620"}); status != http.StatusOK {
		t.Fatalf("identity = %d %v", status, body)
	}
	if _, err := s.runMaintenance(false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if count := transport.Count(); count != 0 {
		t.Fatalf("disabled cover fetch made %d requests: %v", count, transport.Requests())
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, saveVaultMetaFileName)); !os.IsNotExist(err) {
		t.Fatalf("disabled cover fetch created %s: %v", saveVaultMetaFileName, err)
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, saveVaultMetaDirectoryName)); !os.IsNotExist(err) {
		t.Fatalf("disabled cover fetch created %s: %v", saveVaultMetaDirectoryName, err)
	}
}

func TestCoverFetchSteamFallsBackAndServesCachedCover(t *testing.T) {
	picture := testCoverImage(t, "png", 48, 24)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/steam/apps/1245620/header.jpg") {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/store_item_assets/steam/apps/1245620/header.jpg") {
			writeCoverTestImage(t, w, picture)
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()
	s, httpServer, transport := newCoverFetchTestServer(t, upstream)
	publishSaveVaultScope(t, httpServer.URL, "pc", "elden-ring", "a", "", []byte("save"))
	putSaveVaultIdentity(t, httpServer.URL, "pc", "elden-ring", map[string]string{"steamAppId": "1245620"})
	setCoverFetchForTest(t, s, true)
	s.queueSaveVaultCoverFetch("pc/steam:1245620")

	record := waitForSaveVaultMetaRecord(t, s, "pc/steam:1245620", func(record saveVaultMetaRecord) bool {
		return record.Upstream != nil
	})
	if record.Rev != 0 || !record.UpdatedAt.IsZero() || record.UpdatedBy != "" {
		t.Fatalf("cover fetch changed user metadata: %+v", record)
	}
	digest := sha256.Sum256(picture)
	wantSHA := hex.EncodeToString(digest[:])
	if record.Upstream.SHA256 != wantSHA || record.Upstream.MediaType != "image/png" ||
		record.Upstream.Width != 48 || record.Upstream.Height != 24 || record.Upstream.FetchedAt.IsZero() {
		t.Fatalf("upstream cover = %+v", record.Upstream)
	}
	requests := transport.Requests()
	if len(requests) < 2 || !strings.HasPrefix(requests[0], "https://cdn.cloudflare.steamstatic.com/") ||
		!strings.HasPrefix(requests[1], "https://shared.cloudflare.steamstatic.com/") {
		t.Fatalf("Steam request order = %v", requests)
	}

	request := saveVaultScopeRequest(t, http.MethodGet, httpServer.URL+"/v1/savevault/games", nil)
	response, body := saveVaultMetaDo(t, request)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("games = %d: %s", response.StatusCode, body)
	}
	var games struct {
		Games []saveVaultGame `json:"games"`
	}
	if err := json.Unmarshal(body, &games); err != nil {
		t.Fatal(err)
	}
	game := gameByKey(games.Games, "pc/steam:1245620")
	if game == nil || game.Meta.Cover == nil || game.Meta.Cover.Source != saveVaultCoverSourceCached ||
		game.Meta.Cover.SHA256 != wantSHA {
		t.Fatalf("game cover = %+v", game)
	}
	if response, downloaded := getSaveVaultCover(t, httpServer.URL, wantSHA); response.StatusCode != http.StatusOK ||
		!bytes.Equal(downloaded, picture) {
		t.Fatalf("cached cover download = %d", response.StatusCode)
	}
	response, body = adminDo(t, http.MethodGet, httpServer.URL+"/metrics", nil)
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`game_save_covers{source="cached"} 1`)) ||
		!bytes.Contains(body, []byte(`game_save_cover_fetch_total{result="ok"} 1`)) {
		t.Fatalf("cover metrics = %d: %s", response.StatusCode, body)
	}
}

func TestCoverFetchSwitchPrefersEShopThenTinfoil(t *testing.T) {
	picture := testCoverImage(t, "jpeg", 40, 20)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/i/") {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/ti/0100000000010000/0/0/") {
			writeCoverTestImage(t, w, picture)
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()
	s, httpServer, transport := newCoverFetchTestServer(t, upstream)
	publishSaveVaultScope(t, httpServer.URL, "switch", "switch-0100000000010000", "a", "", []byte("switch"))
	putSaveVaultIdentity(t, httpServer.URL, "switch", "switch-0100000000010000",
		map[string]string{"titleId": "0100000000010000"})
	setCoverFetchForTest(t, s, true)
	s.queueSaveVaultCoverFetch("switch/title:0100000000010000")
	record := waitForSaveVaultMetaRecord(t, s, "switch/title:0100000000010000", func(record saveVaultMetaRecord) bool {
		return record.Upstream != nil
	})
	if record.Upstream.MediaType != "image/jpeg" {
		t.Fatalf("switch cover = %+v", record.Upstream)
	}
	requests := transport.Requests()
	if len(requests) < 2 || !strings.HasPrefix(requests[0], "https://img-eshop.cdn.nintendo.net/i/") ||
		!strings.HasPrefix(requests[1], "https://tinfoil.media/ti/0100000000010000/0/0/") {
		t.Fatalf("Switch request order = %v", requests)
	}
}

func TestCoverFetchMissBacksOffForOneDay(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	s, _, transport := newCoverFetchTestServer(t, upstream)
	clock := newCoverFetchTestClock()
	s.now = clock.Now
	setCoverFetchForTest(t, s, true)
	s.queueSaveVaultCoverFetch("pc/steam:424242")
	record := waitForSaveVaultMetaRecord(t, s, "pc/steam:424242", func(record saveVaultMetaRecord) bool {
		return !record.UpstreamMissUntil.IsZero()
	})
	firstCount := transport.Count()
	if firstCount != len(s.coverFetchSteamTemplates) {
		t.Fatalf("404 attempts = %d", firstCount)
	}
	if record.UpstreamFailures != 0 {
		t.Fatalf("404 failures = %d", record.UpstreamFailures)
	}
	if !record.UpstreamMissUntil.After(time.Now().Add(23 * time.Hour)) {
		t.Fatalf("miss Until = %v", record.UpstreamMissUntil)
	}

	if _, err := s.runMaintenance(false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if count := transport.Count(); count != firstCount {
		t.Fatalf("maintenance ignored the 24 hour miss backoff: %d -> %d", firstCount, count)
	}
	clock.Advance(24*time.Hour + time.Second)
	if _, err := s.runMaintenance(false); err != nil {
		t.Fatal(err)
	}
	waitForCoverRequests(t, transport, firstCount*2)
	waitForSaveVaultMetaRecord(t, s, "pc/steam:424242", func(record saveVaultMetaRecord) bool {
		return !record.UpstreamMissUntil.IsZero()
	})
}

func TestCoverFetchErrorBackoffSchedule(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusInternalServerError)
	}))
	defer upstream.Close()
	s, _, transport := newCoverFetchTestServer(t, upstream)
	clock := newCoverFetchTestClock()
	s.now = clock.Now
	setCoverFetchForTest(t, s, true)
	key := "pc/steam:777777"
	s.queueSaveVaultCoverFetch(key)
	record := waitForSaveVaultMetaRecord(t, s, key, func(record saveVaultMetaRecord) bool {
		return record.UpstreamFailures == 1
	})
	base := transport.Count()
	assertDelay := func(record saveVaultMetaRecord, want time.Duration) {
		t.Helper()
		delay := record.UpstreamMissUntil.Sub(clock.Now())
		if delay < want-time.Second || delay > want+time.Second {
			t.Fatalf("failure %d retry delay = %v, want %v", record.UpstreamFailures, delay, want)
		}
	}
	assertDelay(record, time.Hour)

	clock.Advance(30 * time.Minute)
	if _, err := s.runMaintenance(false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if transport.Count() != base {
		t.Fatalf("fetched before the one hour backoff: %d -> %d", base, transport.Count())
	}
	clock.Advance(31 * time.Minute)
	if _, err := s.runMaintenance(false); err != nil {
		t.Fatal(err)
	}
	waitForCoverRequests(t, transport, base*2)
	record = waitForSaveVaultMetaRecord(t, s, key, func(record saveVaultMetaRecord) bool {
		return record.UpstreamFailures == 2
	})
	assertDelay(record, 6*time.Hour)

	clock.Advance(6*time.Hour + time.Minute)
	if _, err := s.runMaintenance(false); err != nil {
		t.Fatal(err)
	}
	waitForCoverRequests(t, transport, base*3)
	record = waitForSaveVaultMetaRecord(t, s, key, func(record saveVaultMetaRecord) bool {
		return record.UpstreamFailures == 3
	})
	assertDelay(record, 24*time.Hour)

	clock.Advance(24*time.Hour + time.Minute)
	if _, err := s.runMaintenance(false); err != nil {
		t.Fatal(err)
	}
	waitForCoverRequests(t, transport, base*4)
	waitForSaveVaultMetaRecord(t, s, key, func(record saveVaultMetaRecord) bool {
		return record.UpstreamFailures == 4
	})
}

func TestCoverFetchRedirectPolicy(t *testing.T) {
	t.Run("outside allowlist is rejected", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://evil.example/cover.jpg", http.StatusFound)
		}))
		defer upstream.Close()
		s, _, transport := newCoverFetchTestServer(t, upstream)
		setCoverFetchForTest(t, s, true)
		key := "pc/steam:111111"
		s.queueSaveVaultCoverFetch(key)
		record := waitForSaveVaultMetaRecord(t, s, key, func(record saveVaultMetaRecord) bool {
			return record.UpstreamFailures == 1
		})
		if record.UpstreamMissUntil.IsZero() {
			t.Fatal("redirect error did not set a retry delay")
		}
		if count := transport.Count(); count != len(s.coverFetchSteamTemplates) {
			t.Fatalf("redirect attempts = %d, want one per Steam template: %v", count, transport.Requests())
		}
		for _, target := range transport.Requests() {
			if strings.Contains(target, "evil.example") {
				t.Fatalf("followed a redirect outside the allowlist: %v", transport.Requests())
			}
		}
	})

	t.Run("inside allowlist is followed", func(t *testing.T) {
		picture := testCoverImage(t, "png", 20, 20)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/redirected-cover" {
				writeCoverTestImage(t, w, picture)
				return
			}
			http.Redirect(w, r, "https://shared.cloudflare.steamstatic.com/redirected-cover", http.StatusFound)
		}))
		defer upstream.Close()
		s, _, transport := newCoverFetchTestServer(t, upstream)
		setCoverFetchForTest(t, s, true)
		key := "pc/steam:222222"
		s.queueSaveVaultCoverFetch(key)
		waitForSaveVaultMetaRecord(t, s, key, func(record saveVaultMetaRecord) bool {
			return record.Upstream != nil
		})
		if count := transport.Count(); count != 2 {
			t.Fatalf("allowlisted redirect requests = %d %v", count, transport.Requests())
		}
	})

	t.Run("more than three redirects is rejected", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hop := r.URL.Query().Get("hop")
			if hop == "" {
				hop = "0"
			}
			var next int
			fmt.Sscanf(hop, "%d", &next)
			http.Redirect(w, r,
				fmt.Sprintf("https://cdn.cloudflare.steamstatic.com/redirect?hop=%d", next+1),
				http.StatusFound)
		}))
		defer upstream.Close()
		s, _, transport := newCoverFetchTestServer(t, upstream)
		setCoverFetchForTest(t, s, true)
		key := "pc/steam:333333"
		s.queueSaveVaultCoverFetch(key)
		waitForSaveVaultMetaRecord(t, s, key, func(record saveVaultMetaRecord) bool {
			return record.UpstreamFailures == 1
		})
		if count := transport.Count(); count != 4*len(s.coverFetchSteamTemplates) {
			t.Fatalf("redirect attempts = %d, want initial plus three for each template", count)
		}
	})
}

func TestCoverFetchRejectsInvalidOrOversizedResponses(t *testing.T) {
	tooLarge := bytes.Repeat([]byte{0xff}, int(maxCoverFetchBytes)+1)
	cases := []struct {
		name    string
		body    []byte
		handler http.HandlerFunc
	}{
		{name: "html", body: []byte("<html>not an image</html>")},
		{name: "truncated png", body: []byte("\x89PNG\r\n\x1a\nnot really")},
		{name: "oversized", handler: func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(tooLarge)
		}},
	}
	for index, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			body := test.body
			handler := test.handler
			if handler == nil {
				handler = func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write(body)
				}
			}
			upstream := httptest.NewServer(handler)
			defer upstream.Close()
			s, _, _ := newCoverFetchTestServer(t, upstream)
			setCoverFetchForTest(t, s, true)
			key := fmt.Sprintf("pc/steam:%d", 900000+index)
			s.queueSaveVaultCoverFetch(key)
			record := waitForSaveVaultMetaRecord(t, s, key, func(record saveVaultMetaRecord) bool {
				return record.UpstreamFailures == 1
			})
			if record.Upstream != nil {
				t.Fatalf("invalid upstream cover was cached: %+v", record.Upstream)
			}
			if files := saveVaultCoverFiles(t, s.dataDir); len(files) != 0 {
				t.Fatalf("invalid upstream cover left files: %v", files)
			}
		})
	}
}

func TestCoverFetchReadOnlyEndpointsDoNotFetchOrWrite(t *testing.T) {
	picture := testCoverImage(t, "png", 16, 16)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeCoverTestImage(t, w, picture)
	}))
	defer upstream.Close()
	s, httpServer, transport := newCoverFetchTestServer(t, upstream)
	publishSaveVaultScope(t, httpServer.URL, "pc", "readonly-cover", "a", "", []byte("read"))
	putSaveVaultIdentity(t, httpServer.URL, "pc", "readonly-cover", map[string]string{"steamAppId": "555555"})
	setCoverFetchForTest(t, s, true)
	before := snapshotDataDirectory(t, s.dataDir)
	requests := []*http.Request{
		saveVaultScopeRequest(t, http.MethodGet, httpServer.URL+"/v1/savevault/games", nil),
		saveVaultScopeRequest(t, http.MethodGet, httpServer.URL+"/v1/savevault/games/pc/steam:555555", nil),
		saveVaultScopeRequest(t, http.MethodGet, httpServer.URL+"/v1/savevault/covers/"+strings.Repeat("a", 64), nil),
		saveVaultScopeRequest(t, http.MethodGet, httpServer.URL+"/v1/savevault/titles/switch", nil),
	}
	for _, request := range requests {
		response, body := saveVaultMetaDo(t, request)
		if response.StatusCode >= 500 {
			t.Fatalf("read-only cover endpoint = %d: %s", response.StatusCode, body)
		}
	}
	response, body := adminDo(t, http.MethodGet, httpServer.URL+"/admin/api/meta-settings", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("meta settings GET = %d: %s", response.StatusCode, body)
	}
	time.Sleep(60 * time.Millisecond)
	if count := transport.Count(); count != 0 {
		t.Fatalf("read-only endpoints fetched covers: %v", transport.Requests())
	}
	after := snapshotDataDirectory(t, s.dataDir)
	if len(before) != len(after) {
		t.Fatalf("read-only endpoints changed files: %d -> %d", len(before), len(after))
	}
	for path, modified := range before {
		if !after[path].Equal(modified) {
			t.Fatalf("read-only endpoints rewrote %s", path)
		}
	}
}

func TestSaveVaultCachedCoverFallsBackAfterCustomCover(t *testing.T) {
	picture := testCoverImage(t, "png", 24, 12)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeCoverTestImage(t, w, picture)
	}))
	defer upstream.Close()
	s, httpServer, _ := newCoverFetchTestServer(t, upstream)
	publishSaveVaultScope(t, httpServer.URL, "pc", "cover-priority", "a", "", []byte("priority"))
	putSaveVaultIdentity(t, httpServer.URL, "pc", "cover-priority", map[string]string{"steamAppId": "666666"})
	setCoverFetchForTest(t, s, true)
	s.queueSaveVaultCoverFetch("pc/steam:666666")
	waitForSaveVaultMetaRecord(t, s, "pc/steam:666666", func(record saveVaultMetaRecord) bool {
		return record.Upstream != nil
	})

	custom := testCoverImage(t, "jpeg", 30, 20)
	response, meta := putSaveVaultCustomCover(t, httpServer.URL, "pc", "cover-priority", custom, "image/jpeg")
	if response.StatusCode != http.StatusOK || meta.Cover == nil || meta.Cover.Source != saveVaultCoverSourceCustom {
		t.Fatalf("custom cover = %d %+v", response.StatusCode, meta)
	}
	response, _ = deleteSaveVaultCustomCover(t, httpServer.URL, "pc", "cover-priority")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("clear custom cover = %d", response.StatusCode)
	}
	games := listSaveVaultGames(t, httpServer.URL)
	game := gameByKey(games, "pc/steam:666666")
	if game == nil || game.Meta.Cover == nil || game.Meta.Cover.Source != saveVaultCoverSourceCached {
		t.Fatalf("cover after clearing custom = %+v", game)
	}
}

func TestAdminCoverRefetchClearsMissAndFetches(t *testing.T) {
	picture := testCoverImage(t, "png", 20, 10)
	var success bool
	var successMu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		successMu.Lock()
		ok := success
		successMu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeCoverTestImage(t, w, picture)
	}))
	defer upstream.Close()
	s, httpServer, transport := newCoverFetchTestServer(t, upstream)
	publishSaveVaultScope(t, httpServer.URL, "pc", "refetch-game", "a", "", []byte("refetch"))
	putSaveVaultIdentity(t, httpServer.URL, "pc", "refetch-game", map[string]string{"steamAppId": "888888"})
	setCoverFetchForTest(t, s, true)
	key := "pc/steam:888888"
	s.queueSaveVaultCoverFetch(key)
	waitForSaveVaultMetaRecord(t, s, key, func(record saveVaultMetaRecord) bool {
		return !record.UpstreamMissUntil.IsZero()
	})
	base := transport.Count()

	successMu.Lock()
	success = true
	successMu.Unlock()
	response, body := adminDo(t, http.MethodPost,
		httpServer.URL+"/admin/api/savevault/games/pc/steam%3A888888/cover/refetch", nil)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("refetch status = %d: %s", response.StatusCode, body)
	}
	var meta saveVaultGameMeta
	if err := json.Unmarshal(body, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Cover != nil {
		t.Fatalf("refetch returned stale cached cover: %+v", meta.Cover)
	}
	waitForCoverRequests(t, transport, base+1)
	waitForSaveVaultMetaRecord(t, s, key, func(record saveVaultMetaRecord) bool {
		return record.Upstream != nil
	})

	response, body = adminDo(t, http.MethodPost,
		httpServer.URL+"/admin/api/savevault/games/pc/scope:not-fetchable/cover/refetch", nil)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unfetchable refetch = %d: %s", response.StatusCode, body)
	}
	setCoverFetchForTest(t, s, false)
	response, body = adminDo(t, http.MethodPost,
		httpServer.URL+"/admin/api/savevault/games/pc/steam%3A888888/cover/refetch", nil)
	if response.StatusCode != http.StatusConflict || !bytes.Contains(body, []byte("cover fetch is disabled")) {
		t.Fatalf("disabled refetch = %d: %s", response.StatusCode, body)
	}
}

func TestMetaSettingsGetPutAndEnvironmentLock(t *testing.T) {
	t.Run("file setting", func(t *testing.T) {
		upstream := httptest.NewServer(http.NotFoundHandler())
		defer upstream.Close()
		s, httpServer, _ := newCoverFetchTestServer(t, upstream)
		response, body := adminDo(t, http.MethodGet, httpServer.URL+"/admin/api/meta-settings", nil)
		if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"coverFetch":false`)) ||
			!bytes.Contains(body, []byte(`"lockedByEnv":false`)) {
			t.Fatalf("GET meta settings = %d: %s", response.StatusCode, body)
		}
		response, body = adminDo(t, http.MethodPut, httpServer.URL+"/admin/api/meta-settings",
			map[string]bool{"coverFetch": true})
		if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"coverFetch":true`)) {
			t.Fatalf("PUT meta settings = %d: %s", response.StatusCode, body)
		}
		s.metaMu.Lock()
		enabled := s.saveVaultMeta.Settings.CoverFetch
		s.metaMu.Unlock()
		if !enabled {
			t.Fatal("PUT did not persist coverFetch")
		}
		s.mu.Lock()
		events := append([]event(nil), s.events...)
		s.mu.Unlock()
		found := false
		for _, item := range events {
			if item.Kind == eventSettings && item.Detail == "cover_fetch=on" {
				found = true
			}
		}
		if !found {
			t.Fatalf("settings event missing: %+v", events)
		}
	})

	t.Run("environment lock", func(t *testing.T) {
		t.Setenv("GAME_SAVE_COVER_FETCH", "0")
		upstream := httptest.NewServer(http.NotFoundHandler())
		defer upstream.Close()
		_, httpServer, _ := newCoverFetchTestServer(t, upstream)
		response, body := adminDo(t, http.MethodGet, httpServer.URL+"/admin/api/meta-settings", nil)
		if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"coverFetch":false`)) ||
			!bytes.Contains(body, []byte(`"lockedByEnv":true`)) {
			t.Fatalf("locked GET = %d: %s", response.StatusCode, body)
		}
		response, body = adminDo(t, http.MethodPut, httpServer.URL+"/admin/api/meta-settings",
			map[string]bool{"coverFetch": true})
		if response.StatusCode != http.StatusConflict ||
			!bytes.Contains(body, []byte("cover fetch is set by GAME_SAVE_COVER_FETCH")) {
			t.Fatalf("locked PUT = %d: %s", response.StatusCode, body)
		}
	})
}

func TestCoverFetchConcurrencyAndClose(t *testing.T) {
	picture := testCoverImage(t, "png", 12, 12)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
		writeCoverTestImage(t, w, picture)
	}))
	defer upstream.Close()
	s, _, transport := newCoverFetchTestServer(t, upstream)
	setCoverFetchForTest(t, s, true)
	keys := make([]string, 0, 10)
	for index := 0; index < 10; index++ {
		key := fmt.Sprintf("pc/steam:%d", 700000+index)
		keys = append(keys, key)
		s.queueSaveVaultCoverFetch(key)
	}
	for _, key := range keys {
		waitForSaveVaultMetaRecord(t, s, key, func(record saveVaultMetaRecord) bool {
			return record.Upstream != nil
		})
	}
	if maximum := transport.MaxConcurrency(); maximum > 2 {
		t.Fatalf("cover fetch concurrency = %d, want at most 2", maximum)
	}
	s.Close()
	done := make(chan struct{})
	go func() {
		s.coverFetchWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cover fetch workers did not exit after Close")
	}
}

func TestCoverFetchDoesNotChangeScopeState(t *testing.T) {
	picture := testCoverImage(t, "png", 18, 18)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeCoverTestImage(t, w, picture)
	}))
	defer upstream.Close()
	s, httpServer, _ := newCoverFetchTestServer(t, upstream)
	publishSaveVaultScope(t, httpServer.URL, "pc", "stable-cover", "head", "", []byte("head"))
	conflictBody := []byte("conflict")
	conflictArchive := "stable-cover__main__conflict.zip"
	if status := putSaveVaultScopeArchive(t, httpServer.URL, "pc", conflictArchive, conflictBody); status != http.StatusCreated {
		t.Fatalf("conflict archive = %d", status)
	}
	conflict := saveVaultScopeCandidateFor("conflict", conflictArchive, "other-parent", conflictBody, time.Now().UTC())
	response, body := putSaveVaultScopeCandidate(t, httpServer.URL,
		"/v1/savevault/scopes/pc/stable-cover/conflicts/"+conflict.ID, "-", conflict)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("conflict = %d: %s", response.StatusCode, body)
	}
	putSaveVaultIdentity(t, httpServer.URL, "pc", "stable-cover", map[string]string{"steamAppId": "999999"})
	before := getSaveVaultScopeForTest(t, httpServer.URL, "pc", "stable-cover")
	setCoverFetchForTest(t, s, true)
	s.queueSaveVaultCoverFetch("pc/steam:999999")
	waitForSaveVaultMetaRecord(t, s, "pc/steam:999999", func(record saveVaultMetaRecord) bool {
		return record.Upstream != nil
	})
	after := getSaveVaultScopeForTest(t, httpServer.URL, "pc", "stable-cover")
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	if !bytes.Equal(beforeJSON, afterJSON) {
		t.Fatalf("cover fetch changed scope state\nbefore: %s\nafter: %s", beforeJSON, afterJSON)
	}
}
