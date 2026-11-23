// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const (
	maxCoverFetchBytes     int64 = 2 << 20
	maxCoverFetchDimension       = 4096
	coverFetchQueueSize          = 1000
	coverFetchWorkerCount        = 2
	coverFetchTimeout            = 8 * time.Second

	coverFetchMissDuration      = 24 * time.Hour
	coverFetchFailureHour       = time.Hour
	coverFetchFailureSixHours   = 6 * time.Hour
	coverFetchFailureTwentyFour = 24 * time.Hour
)

const defaultCoverFetchSwitchIconTemplate = "https://img-eshop.cdn.nintendo.net/i/{hash}.jpg"

// The templates are fixed data, never built from a client supplied URL. The
// server fields below are initialized from these defaults; tests replace only
// the HTTP transport while these URLs keep their real host names.
var (
	defaultCoverFetchSteamTemplates = []string{
		"https://cdn.cloudflare.steamstatic.com/steam/apps/{appid}/header.jpg",
		"https://shared.cloudflare.steamstatic.com/store_item_assets/steam/apps/{appid}/header.jpg",
		"https://cdn.akamai.steamstatic.com/steam/apps/{appid}/header.jpg",
		"https://shared.akamai.steamstatic.com/store_item_assets/steam/apps/{appid}/header.jpg",
	}
	defaultCoverFetchSwitchTemplates = []string{
		"https://tinfoil.media/ti/{titleid}/0/0/",
	}
	coverFetchAllowedHosts = map[string]struct{}{
		"cdn.cloudflare.steamstatic.com":    {},
		"shared.cloudflare.steamstatic.com": {},
		"cdn.akamai.steamstatic.com":        {},
		"shared.akamai.steamstatic.com":     {},
		"img-eshop.cdn.nintendo.net":        {},
		"tinfoil.media":                     {},
	}
)

type coverFetchOutcome int

const (
	coverFetchOK coverFetchOutcome = iota
	coverFetchMiss
	coverFetchError
	coverFetchDisabled
)

type coverFetchMetric struct {
	ok    atomic.Uint64
	miss  atomic.Uint64
	err   atomic.Uint64
	drops atomic.Uint64
}

// coverFetchEnvSetting reads the process-wide lock. Invalid values are ignored
// so a typo never changes the persisted administrator setting.
func coverFetchEnvSetting() *bool {
	value, exists := os.LookupEnv("GAME_SAVE_COVER_FETCH")
	if !exists {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true":
		enabled := true
		return &enabled
	case "0", "false":
		enabled := false
		return &enabled
	default:
		return nil
	}
}

func newCoverFetchClient() (*http.Client, http.RoundTripper) {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	client := &http.Client{
		Timeout:   coverFetchTimeout,
		Transport: transport,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) > 3 {
				return errors.New("too many upstream cover redirects")
			}
			if !validCoverFetchURL(request.URL) {
				return fmt.Errorf("upstream cover redirect left the allowlist: %s", request.URL.Host)
			}
			// net/http adds Referer automatically for redirects. The upstream
			// cover request must stay free of cookies and referrers.
			request.Header.Del("Referer")
			request.Header.Del("Cookie")
			return nil
		},
	}
	return client, transport
}

func validCoverFetchURL(target *url.URL) bool {
	if target == nil || target.Scheme != "https" || target.User != nil {
		return false
	}
	if target.Port() != "" && target.Port() != "443" {
		return false
	}
	_, ok := coverFetchAllowedHosts[strings.ToLower(target.Hostname())]
	return ok
}

func (s *server) coverFetchEnabledLocked() bool {
	if s.coverFetchEnv != nil {
		return *s.coverFetchEnv
	}
	return s.openSaveMeta.Settings.CoverFetch
}

func (s *server) coverFetchEnabled() bool {
	if s.coverFetchEnv != nil {
		return *s.coverFetchEnv
	}
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	return s.openSaveMeta.Settings.CoverFetch
}

func (s *server) metaSettings() (enabled, locked bool) {
	if s.coverFetchEnv != nil {
		return *s.coverFetchEnv, true
	}
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	return s.openSaveMeta.Settings.CoverFetch, false
}

// coverFetchURLs validates the game key before expanding the fixed templates.
// Unknown keys, malformed AppIDs, and malformed TitleIDs produce no URLs.
func (s *server) coverFetchURLs(gameKey string) []string {
	kind, rest, found := strings.Cut(gameKey, "/")
	if !found || !validOpenSaveType(kind) {
		return nil
	}
	if appID, ok := strings.CutPrefix(rest, "steam:"); ok {
		if !steamAppIDPattern.MatchString(appID) {
			return nil
		}
		urls := make([]string, 0, len(s.coverFetchSteamTemplates))
		for _, template := range s.coverFetchSteamTemplates {
			urls = append(urls, strings.ReplaceAll(template, "{appid}", appID))
		}
		return urls
	}
	if kind != "switch" {
		return nil
	}
	titleID, ok := strings.CutPrefix(rest, "title:")
	if !ok || !switchTitleIDPattern.MatchString(titleID) {
		return nil
	}
	titleID = strings.ToUpper(titleID)
	urls := make([]string, 0, 2)
	if title, found := switchTitleFor(titleID); found && validLowerHex(title.IconHash, 64) {
		urls = append(urls, strings.ReplaceAll(s.coverFetchSwitchIconTemplate, "{hash}", title.IconHash))
	}
	for _, template := range s.coverFetchSwitchTemplates {
		urls = append(urls, strings.ReplaceAll(template, "{titleid}", titleID))
	}
	return urls
}

func (s *server) queueOpenSaveCoverFetch(gameKey string) {
	if !s.coverFetchEnabled() || len(s.coverFetchURLs(gameKey)) == 0 {
		return
	}
	s.coverFetchMu.Lock()
	defer s.coverFetchMu.Unlock()
	if s.coverFetchClosed {
		return
	}
	if _, queued := s.coverFetchQueued[gameKey]; queued {
		return
	}
	select {
	case s.coverFetchQueue <- gameKey:
		s.coverFetchQueued[gameKey] = struct{}{}
		if !s.coverFetchStarted {
			s.coverFetchStarted = true
			for index := 0; index < coverFetchWorkerCount; index++ {
				s.coverFetchWG.Add(1)
				go s.coverFetchWorker()
			}
		}
	default:
		s.coverFetchMetrics.drops.Add(1)
	}
}

func (s *server) coverFetchWorker() {
	defer s.coverFetchWG.Done()
	for {
		select {
		case <-s.coverFetchCtx.Done():
			return
		case gameKey := <-s.coverFetchQueue:
			if s.coverFetchCtx.Err() != nil {
				return
			}
			s.fetchOpenSaveCover(s.coverFetchCtx, gameKey)
			s.coverFetchMu.Lock()
			delete(s.coverFetchQueued, gameKey)
			s.coverFetchMu.Unlock()
		}
	}
}

func (s *server) coverFetchQueueLength() int {
	if s.coverFetchQueue == nil {
		return 0
	}
	return len(s.coverFetchQueue)
}

func (s *server) shouldFetchOpenSaveCover(gameKey string, now time.Time) bool {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	record := s.openSaveMeta.Games[gameKey]
	if record.Upstream != nil {
		return false
	}
	return record.UpstreamMissUntil.IsZero() || !now.Before(record.UpstreamMissUntil)
}

func (s *server) fetchOpenSaveCover(ctx context.Context, gameKey string) {
	if !s.coverFetchEnabled() {
		return
	}
	if now := s.now().UTC(); !s.shouldFetchOpenSaveCover(gameKey, now) {
		return
	}
	urls := s.coverFetchURLs(gameKey)
	if len(urls) == 0 {
		return
	}
	cover, data, outcome, fetchErr := s.downloadOpenSaveCover(ctx, urls)
	if ctx.Err() != nil {
		return
	}
	switch outcome {
	case coverFetchOK:
		s.recordOpenSaveCoverSuccess(gameKey, cover, data)
	case coverFetchMiss:
		s.recordOpenSaveCoverMiss(gameKey)
	case coverFetchError:
		s.recordOpenSaveCoverError(gameKey, fetchErr)
	}
}

func (s *server) downloadOpenSaveCover(ctx context.Context,
	urls []string) (openSaveMetaCover, []byte, coverFetchOutcome, error) {
	sawMiss := false
	sawError := false
	var lastErr error
	for _, target := range urls {
		if ctx.Err() != nil {
			return openSaveMetaCover{}, nil, coverFetchError, ctx.Err()
		}
		if !s.coverFetchEnabled() {
			return openSaveMetaCover{}, nil, coverFetchDisabled, nil
		}
		parsed, err := url.Parse(target)
		if err != nil || !validCoverFetchURL(parsed) {
			sawError = true
			lastErr = fmt.Errorf("invalid upstream cover URL")
			continue
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			sawError = true
			lastErr = err
			continue
		}
		request.Header.Set("User-Agent", "game-save/"+version)
		request.Header.Del("Referer")
		request.Header.Del("Cookie")
		response, err := s.coverFetchClient.Do(request)
		if err != nil {
			sawError = true
			lastErr = fmt.Errorf("%s: %w", parsed.Host, err)
			continue
		}
		status := response.StatusCode
		if status != http.StatusOK {
			_ = response.Body.Close()
			if status >= 400 && status < 500 {
				sawMiss = true
				lastErr = fmt.Errorf("%s: upstream status %d", parsed.Host, status)
			} else {
				sawError = true
				lastErr = fmt.Errorf("%s: upstream status %d", parsed.Host, status)
			}
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, maxCoverFetchBytes+1))
		_ = response.Body.Close()
		if readErr != nil {
			sawError = true
			lastErr = fmt.Errorf("%s: %w", parsed.Host, readErr)
			continue
		}
		if int64(len(data)) > maxCoverFetchBytes {
			sawError = true
			lastErr = fmt.Errorf("%s: upstream cover exceeds 2 MiB", parsed.Host)
			continue
		}
		cover, valid := openSaveCoverFromImage(data, maxCoverFetchDimension)
		if !valid {
			sawError = true
			lastErr = fmt.Errorf("%s: upstream cover is not a valid JPEG or PNG", parsed.Host)
			continue
		}
		return cover, data, coverFetchOK, nil
	}
	if sawError {
		return openSaveMetaCover{}, nil, coverFetchError, lastErr
	}
	if sawMiss {
		return openSaveMetaCover{}, nil, coverFetchMiss, lastErr
	}
	return openSaveMetaCover{}, nil, coverFetchError, errors.New("no upstream cover URL")
}

func (s *server) recordOpenSaveCoverSuccess(gameKey string, cover openSaveMetaCover, data []byte) {
	if !s.coverFetchEnabled() {
		return
	}
	cover.FetchedAt = s.now().UTC()
	// The cover file is written and referenced under metaMu, like custom
	// covers, so maintenance cannot collect a reused file in between and a
	// failed persist never removes a file another record just referenced.
	s.metaMu.Lock()
	if !s.coverFetchEnabledLocked() {
		s.metaMu.Unlock()
		return
	}
	record := s.openSaveMeta.Games[gameKey]
	if record.Upstream != nil {
		s.metaMu.Unlock()
		s.coverFetchMetrics.ok.Add(1)
		return
	}
	created, err := s.storeOpenSaveCover(cover.SHA256, data)
	if err != nil {
		s.metaMu.Unlock()
		s.coverFetchMetrics.err.Add(1)
		slog.Warn("fetch upstream cover", "game", gameKey, "error", err)
		return
	}
	record.Upstream = &cover
	record.UpstreamFailures = 0
	record.UpstreamMissUntil = time.Time{}
	next := s.openSaveMeta.clone()
	next.Games[gameKey] = record
	if err := s.persistOpenSaveMeta(next); err != nil {
		if created {
			_ = os.Remove(s.openSaveMetaCoverPath(cover.SHA256))
		}
		s.metaMu.Unlock()
		s.coverFetchMetrics.err.Add(1)
		slog.Warn("fetch upstream cover", "game", gameKey, "error", err)
		return
	}
	s.openSaveMeta = next
	s.metaMu.Unlock()
	s.coverFetchMetrics.ok.Add(1)
}

func (s *server) recordOpenSaveCoverMiss(gameKey string) {
	if !s.coverFetchEnabled() {
		return
	}
	s.metaMu.Lock()
	if !s.coverFetchEnabledLocked() {
		s.metaMu.Unlock()
		return
	}
	record := s.openSaveMeta.Games[gameKey]
	if record.Upstream != nil {
		s.metaMu.Unlock()
		return
	}
	record.UpstreamMissUntil = s.now().UTC().Add(coverFetchMissDuration)
	next := s.openSaveMeta.clone()
	next.Games[gameKey] = record
	if err := s.persistOpenSaveMeta(next); err != nil {
		s.metaMu.Unlock()
		s.coverFetchMetrics.err.Add(1)
		slog.Warn("fetch upstream cover", "game", gameKey, "error", err)
		return
	}
	s.openSaveMeta = next
	s.metaMu.Unlock()
	s.coverFetchMetrics.miss.Add(1)
}

func (s *server) recordOpenSaveCoverError(gameKey string, fetchErr error) {
	if !s.coverFetchEnabled() {
		return
	}
	now := s.now().UTC()
	s.metaMu.Lock()
	if !s.coverFetchEnabledLocked() {
		s.metaMu.Unlock()
		return
	}
	record := s.openSaveMeta.Games[gameKey]
	if record.Upstream != nil {
		s.metaMu.Unlock()
		return
	}
	record.UpstreamFailures++
	retry := coverFetchFailureHour
	switch {
	case record.UpstreamFailures == 2:
		retry = coverFetchFailureSixHours
	case record.UpstreamFailures >= 3:
		retry = coverFetchFailureTwentyFour
	}
	record.UpstreamMissUntil = now.Add(retry)
	next := s.openSaveMeta.clone()
	next.Games[gameKey] = record
	if err := s.persistOpenSaveMeta(next); err != nil {
		s.metaMu.Unlock()
		s.coverFetchMetrics.err.Add(1)
		slog.Warn("fetch upstream cover", "game", gameKey, "error", err)
		return
	}
	s.openSaveMeta = next
	s.metaMu.Unlock()
	s.coverFetchMetrics.err.Add(1)
	slog.Warn("fetch upstream cover", "game", gameKey, "error", fetchErr)
}

// queueMaintenanceCoverFetches scans every known game key. It runs after the
// maintenance lock is released so it never holds s.mu while a worker starts.
func (s *server) queueMaintenanceCoverFetches() {
	if !s.coverFetchEnabled() {
		return
	}
	keys := make(map[string]struct{})
	s.openSaveMu.Lock()
	for key, stored := range s.openSaveState.Scopes {
		kind, gameID, _ := strings.Cut(key, "\x00")
		gameKey, _ := openSaveGameKey(kind, gameID, stored)
		keys[gameKey] = struct{}{}
	}
	s.openSaveMu.Unlock()
	s.metaMu.Lock()
	for gameKey := range s.openSaveMeta.Games {
		keys[gameKey] = struct{}{}
	}
	s.metaMu.Unlock()
	for gameKey := range keys {
		s.queueOpenSaveCoverFetch(gameKey)
	}
}

func (s *server) handleGetMetaSettings(w http.ResponseWriter, _ *http.Request) {
	enabled, locked := s.metaSettings()
	writeJSON(w, http.StatusOK, map[string]bool{"coverFetch": enabled, "lockedByEnv": locked})
}

func (s *server) handlePutMetaSettings(w http.ResponseWriter, r *http.Request) {
	var request struct {
		CoverFetch bool `json:"coverFetch"`
	}
	if !decodeAdminJSON(w, r, &request) {
		return
	}
	if s.coverFetchEnv != nil {
		writeError(w, http.StatusConflict, "cover fetch is set by GAME_SAVE_COVER_FETCH")
		return
	}
	s.metaMu.Lock()
	changed := s.openSaveMeta.Settings.CoverFetch != request.CoverFetch
	if changed {
		next := s.openSaveMeta.clone()
		next.Settings.CoverFetch = request.CoverFetch
		if err := s.persistOpenSaveMeta(next); err != nil {
			s.metaMu.Unlock()
			writeError(w, http.StatusInternalServerError, "cannot persist OpenSave metadata")
			return
		}
		s.openSaveMeta = next
	}
	enabled := s.openSaveMeta.Settings.CoverFetch
	s.metaMu.Unlock()
	if changed {
		state := "off"
		if enabled {
			state = "on"
		}
		s.mu.Lock()
		s.recordEventLocked(event{Kind: eventSettings, Detail: "cover_fetch=" + state})
		s.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"coverFetch": enabled, "lockedByEnv": false})
}

func (s *server) handleAdminOpenSaveGameCoverRefetch(w http.ResponseWriter, r *http.Request) {
	value := r.PathValue("key")
	gameKey, ok := strings.CutSuffix(value, "/cover/refetch")
	if !ok {
		writeError(w, http.StatusNotFound, "OpenSave metadata endpoint not found")
		return
	}
	if !validOpenSaveGameKey(gameKey) {
		writeError(w, http.StatusBadRequest, "invalid OpenSave game key")
		return
	}
	if !s.coverFetchEnabled() {
		writeError(w, http.StatusConflict, "cover fetch is disabled")
		return
	}
	if len(s.coverFetchURLs(gameKey)) == 0 {
		writeError(w, http.StatusBadRequest, "OpenSave game key has no upstream cover")
		return
	}

	s.openSaveMu.Lock()
	members := s.openSaveGameMembersForKeyLocked(gameKey)
	s.metaMu.Lock()
	record := s.openSaveMeta.Games[gameKey]
	changed := record.Upstream != nil || !record.UpstreamMissUntil.IsZero() || record.UpstreamFailures != 0
	if changed {
		record.Upstream = nil
		record.UpstreamMissUntil = time.Time{}
		record.UpstreamFailures = 0
		next := s.openSaveMeta.clone()
		next.Games[gameKey] = record
		if err := s.persistOpenSaveMeta(next); err != nil {
			s.metaMu.Unlock()
			s.openSaveMu.Unlock()
			writeError(w, http.StatusInternalServerError, "cannot persist OpenSave metadata")
			return
		}
		s.openSaveMeta = next
	}
	meta := openSaveGameMetaView(gameKey, s.openSaveMeta.Games[gameKey], members, true)
	s.metaMu.Unlock()
	s.openSaveMu.Unlock()
	s.queueOpenSaveCoverFetch(gameKey)
	writeJSON(w, http.StatusAccepted, meta)
}
