// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// OpenSave metadata is stored next to the archives but never inside them. A
// metadata write must not touch a head, a snapshot, or a conflict copy.
//
// Lock order: s.mu -> tokensMu -> metaMu, and s.mu -> metaMu. openSaveMu may
// be held before metaMu (openSaveMu -> metaMu). metaMu is never held while
// acquiring s.mu, tokensMu, or openSaveMu.

const (
	openSaveMetaFileName      = "opensave-meta.json"
	openSaveMetaDirectoryName = "opensave-meta"

	maxOpenSaveCustomCoverBytes int64 = 1 << 20
	maxOpenSaveCoverDimension         = 2048

	openSaveCoverSourceCustom = "custom"
	openSaveCoverSourceCached = "cached"

	openSaveNameSourceCustom = "custom"
	openSaveNameSourceTable  = "table"
	openSaveNameSourceClient = "client"
	openSaveNameSourceKey    = "key"
)

// openSaveMetaCover is both the custom and the upstream cover description. The
// upstream-only FetchedAt field is omitted for custom covers.
type openSaveMetaCover struct {
	SHA256    string    `json:"sha256"`
	MediaType string    `json:"mediaType"`
	Width     int       `json:"width"`
	Height    int       `json:"height"`
	FetchedAt time.Time `json:"fetchedAt,omitzero"`
}

type openSaveMetaRecord struct {
	Rev               int                `json:"rev,omitempty"`
	CustomName        string             `json:"customName,omitempty"`
	CustomCover       *openSaveMetaCover `json:"customCover,omitempty"`
	Upstream          *openSaveMetaCover `json:"upstream,omitempty"`
	UpstreamMissUntil time.Time          `json:"upstreamMissUntil,omitzero"`
	UpstreamFailures  int                `json:"upstreamFailures,omitempty"`
	UpdatedAt         time.Time          `json:"updatedAt,omitzero"`
	UpdatedBy         string             `json:"updatedBy,omitempty"`
}

type openSaveMetaSettings struct {
	CoverFetch bool `json:"coverFetch"`
}

type openSaveMetaState struct {
	Version  int                           `json:"version"`
	Settings openSaveMetaSettings          `json:"settings"`
	Games    map[string]openSaveMetaRecord `json:"games"`
}

func defaultOpenSaveMetaState() openSaveMetaState {
	return openSaveMetaState{
		Version: 1,
		Games:   make(map[string]openSaveMetaRecord),
	}
}

func (record openSaveMetaRecord) clone() openSaveMetaRecord {
	next := record
	if record.CustomCover != nil {
		cover := *record.CustomCover
		next.CustomCover = &cover
	}
	if record.Upstream != nil {
		cover := *record.Upstream
		next.Upstream = &cover
	}
	return next
}

func (state openSaveMetaState) clone() openSaveMetaState {
	next := openSaveMetaState{
		Version:  state.Version,
		Settings: state.Settings,
		Games:    make(map[string]openSaveMetaRecord, len(state.Games)+1),
	}
	for key, record := range state.Games {
		next.Games[key] = record.clone()
	}
	return next
}

// openSaveGameMeta is the client-facing meta field of a game.
type openSaveGameMeta struct {
	Rev        int                    `json:"rev"`
	Name       string                 `json:"name"`
	NameSource string                 `json:"nameSource"`
	Cover      *openSaveGameMetaCover `json:"cover,omitempty"`
	UpdatedAt  time.Time              `json:"updatedAt,omitzero"`
	UpdatedBy  string                 `json:"updatedBy,omitempty"`
}

type openSaveGameMetaCover struct {
	Source    string `json:"source"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"mediaType"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	URL       string `json:"url"`
}

func (s *server) openSaveMetaPath() string {
	return filepath.Join(s.dataDir, openSaveMetaFileName)
}

func (s *server) openSaveMetaCoverPath(digest string) string {
	return filepath.Join(s.dataDir, openSaveMetaDirectoryName, "covers", digest[:2], digest)
}

// loadOpenSaveMeta never creates the file. A deployment that has not used a C5
// write endpoint keeps no metadata state at all.
func (s *server) loadOpenSaveMeta() error {
	state := openSaveMetaState{}
	if err := readJSONFile(s.openSaveMetaPath(), &state); err != nil {
		return err
	}
	if state.Version != 0 && state.Version != 1 {
		return fmt.Errorf("%s contains an unsupported version", openSaveMetaFileName)
	}
	if state.Games == nil {
		state.Games = make(map[string]openSaveMetaRecord)
	}
	state.Version = 1
	s.openSaveMeta = state
	return nil
}

func (s *server) persistOpenSaveMeta(state openSaveMetaState) error {
	state.Version = 1
	return writeJSONFile(s.openSaveMetaPath(), state)
}

func openSaveGameKeyID(gameKey string) string {
	if _, id, ok := strings.Cut(gameKey, ":"); ok && id != "" {
		return id
	}
	if _, id, ok := strings.Cut(gameKey, "/"); ok && id != "" {
		return id
	}
	return gameKey
}

func switchTitleForGameKey(gameKey string) (switchTitle, bool) {
	titleID, ok := strings.CutPrefix(gameKey, "switch/title:")
	if !ok {
		return switchTitle{}, false
	}
	return switchTitleFor(titleID)
}

func openSaveGameDisplayName(gameKey string, record openSaveMetaRecord, members []openSaveGameMember) (string, string) {
	if record.CustomName != "" {
		return record.CustomName, openSaveNameSourceCustom
	}
	if title, ok := switchTitleForGameKey(gameKey); ok {
		return title.Name, openSaveNameSourceTable
	}
	if name, ok := openSaveGameClientName(members); ok && name != "" {
		return name, openSaveNameSourceClient
	}
	return openSaveGameKeyID(gameKey), openSaveNameSourceKey
}

func openSaveCoverURL(digest string, admin bool) string {
	if admin {
		return "/admin/api/opensave/covers/" + digest
	}
	return "/v1/opensave/covers/" + digest
}

func openSaveGameMetaView(gameKey string, record openSaveMetaRecord, members []openSaveGameMember, admin bool) openSaveGameMeta {
	name, source := openSaveGameDisplayName(gameKey, record, members)
	view := openSaveGameMeta{
		Rev:        record.Rev,
		Name:       name,
		NameSource: source,
		UpdatedAt:  record.UpdatedAt,
		UpdatedBy:  record.UpdatedBy,
	}
	cover, source := record.CustomCover, openSaveCoverSourceCustom
	if cover == nil && record.Upstream != nil {
		cover, source = record.Upstream, openSaveCoverSourceCached
	}
	if cover != nil {
		view.Cover = &openSaveGameMetaCover{
			Source:    source,
			SHA256:    cover.SHA256,
			MediaType: cover.MediaType,
			Width:     cover.Width,
			Height:    cover.Height,
			URL:       openSaveCoverURL(cover.SHA256, admin),
		}
	}
	return view
}

// openSaveGameMetaLocked returns the current view of one game. The caller must
// hold metaMu.
func (s *server) openSaveGameMetaLocked(gameKey string, members []openSaveGameMember, admin bool) openSaveGameMeta {
	return openSaveGameMetaView(gameKey, s.openSaveMeta.Games[gameKey], members, admin)
}

func parseOpenSaveMetaRevision(r *http.Request) (int, bool, bool) {
	raw := strings.TrimSpace(r.Header.Get("If-Match"))
	if raw == "" {
		return 0, false, true
	}
	if strings.HasPrefix(raw, "W/") {
		return 0, true, false
	}
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		raw = raw[1 : len(raw)-1]
	}
	revision, err := strconv.Atoi(raw)
	if err != nil || revision < 0 {
		return 0, true, false
	}
	return revision, true, true
}

func decodeOpenSaveCustomName(w http.ResponseWriter, r *http.Request) (string, bool) {
	var request struct {
		Name string `json:"name"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMetadataBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid OpenSave custom name")
		return "", false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid OpenSave custom name")
		return "", false
	}
	name := strings.TrimSpace(request.Name)
	if !validGameName(name) {
		writeError(w, http.StatusBadRequest, "invalid OpenSave custom name")
		return "", false
	}
	return name, true
}

var openSavePNGSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

func openSaveImageMediaType(data []byte) string {
	switch {
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "image/jpeg"
	case len(data) >= len(openSavePNGSignature) && bytes.Equal(data[:len(openSavePNGSignature)], openSavePNGSignature):
		return "image/png"
	}
	return ""
}

func decodeOpenSaveCoverUpload(w http.ResponseWriter, r *http.Request) (openSaveMetaCover, []byte, bool) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mediaType != "image/jpeg" && mediaType != "image/png") {
		writeError(w, http.StatusBadRequest, "Content-Type must be image/jpeg or image/png")
		return openSaveMetaCover{}, nil, false
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxOpenSaveCustomCoverBytes))
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(w, http.StatusRequestEntityTooLarge, "OpenSave cover exceeds 1 MiB")
		} else {
			writeError(w, http.StatusBadRequest, "cannot read OpenSave cover")
		}
		return openSaveMetaCover{}, nil, false
	}
	if len(data) == 0 {
		writeError(w, http.StatusBadRequest, "OpenSave cover is empty")
		return openSaveMetaCover{}, nil, false
	}
	if openSaveImageMediaType(data) != mediaType {
		writeError(w, http.StatusBadRequest, "OpenSave cover does not match Content-Type")
		return openSaveMetaCover{}, nil, false
	}
	cover, ok := openSaveCoverFromImage(data, maxOpenSaveCoverDimension)
	if !ok {
		writeError(w, http.StatusBadRequest, "unsupported OpenSave cover image or invalid dimensions")
		return openSaveMetaCover{}, nil, false
	}
	return cover, data, true
}

// openSaveCoverFromImage validates image bytes using their magic and decoded
// dimensions. Content-Type headers are never trusted for this decision.
func openSaveCoverFromImage(data []byte, maxDimension int) (openSaveMetaCover, bool) {
	mediaType := openSaveImageMediaType(data)
	if mediaType == "" {
		return openSaveMetaCover{}, false
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "jpeg" && format != "png") {
		return openSaveMetaCover{}, false
	}
	if (mediaType == "image/jpeg" && format != "jpeg") || (mediaType == "image/png" && format != "png") {
		return openSaveMetaCover{}, false
	}
	if config.Width <= 0 || config.Height <= 0 ||
		config.Width > maxDimension || config.Height > maxDimension {
		return openSaveMetaCover{}, false
	}
	digest := sha256.Sum256(data)
	return openSaveMetaCover{
		SHA256:    hex.EncodeToString(digest[:]),
		MediaType: mediaType,
		Width:     config.Width,
		Height:    config.Height,
	}, true
}

func (s *server) storeOpenSaveCover(digest string, data []byte) (bool, error) {
	path := s.openSaveMetaCoverPath(digest)
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		return false, nil
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return false, err
	}
	temporary, err := os.CreateTemp(directory, ".cover-*")
	if err != nil {
		return false, err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return false, err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return false, err
	}
	if err := temporary.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return false, err
	}
	return true, nil
}

func (s *server) applyOpenSaveCustomNameLocked(gameKey, name, actor string, ifMatch int, hasIfMatch bool,
	members []openSaveGameMember, admin bool) (int, openSaveGameMeta, error) {
	record := s.openSaveMeta.Games[gameKey]
	current := openSaveGameMetaView(gameKey, record, members, admin)
	if hasIfMatch && ifMatch != record.Rev {
		return http.StatusPreconditionFailed, current, nil
	}
	if record.CustomName == name {
		return http.StatusNoContent, current, nil
	}
	next := s.openSaveMeta.clone()
	record.CustomName = name
	record.Rev++
	record.UpdatedAt = s.now().UTC()
	record.UpdatedBy = actor
	next.Games[gameKey] = record
	if err := s.persistOpenSaveMeta(next); err != nil {
		return http.StatusInternalServerError, current, err
	}
	s.openSaveMeta = next
	return http.StatusOK, openSaveGameMetaView(gameKey, record, members, admin), nil
}

func (s *server) applyOpenSaveCustomCoverLocked(gameKey string, cover openSaveMetaCover, data []byte, actor string,
	ifMatch int, hasIfMatch bool, members []openSaveGameMember, admin bool) (int, openSaveGameMeta, error) {
	record := s.openSaveMeta.Games[gameKey]
	current := openSaveGameMetaView(gameKey, record, members, admin)
	if hasIfMatch && ifMatch != record.Rev {
		return http.StatusPreconditionFailed, current, nil
	}
	if record.CustomCover != nil && record.CustomCover.SHA256 == cover.SHA256 {
		return http.StatusNoContent, current, nil
	}
	created, err := s.storeOpenSaveCover(cover.SHA256, data)
	if err != nil {
		return http.StatusInternalServerError, current, err
	}
	next := s.openSaveMeta.clone()
	record.CustomCover = &cover
	record.Rev++
	record.UpdatedAt = s.now().UTC()
	record.UpdatedBy = actor
	next.Games[gameKey] = record
	if err := s.persistOpenSaveMeta(next); err != nil {
		if created {
			_ = os.Remove(s.openSaveMetaCoverPath(cover.SHA256))
		}
		return http.StatusInternalServerError, current, err
	}
	s.openSaveMeta = next
	return http.StatusOK, openSaveGameMetaView(gameKey, record, members, admin), nil
}

func (s *server) applyClearOpenSaveCustomCoverLocked(gameKey, actor string, ifMatch int, hasIfMatch bool,
	members []openSaveGameMember, admin bool) (int, openSaveGameMeta, error) {
	record := s.openSaveMeta.Games[gameKey]
	current := openSaveGameMetaView(gameKey, record, members, admin)
	if hasIfMatch && ifMatch != record.Rev {
		return http.StatusPreconditionFailed, current, nil
	}
	if record.CustomCover == nil {
		return http.StatusNoContent, current, nil
	}
	next := s.openSaveMeta.clone()
	record.CustomCover = nil
	record.Rev++
	record.UpdatedAt = s.now().UTC()
	record.UpdatedBy = actor
	next.Games[gameKey] = record
	if err := s.persistOpenSaveMeta(next); err != nil {
		return http.StatusInternalServerError, current, err
	}
	s.openSaveMeta = next
	return http.StatusOK, openSaveGameMetaView(gameKey, record, members, admin), nil
}

func writeOpenSaveMetaResult(w http.ResponseWriter, status int, meta openSaveGameMeta) {
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	writeJSON(w, status, meta)
}

func (s *server) recordOpenSaveMetaEvent(gameKey, action string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordEventLocked(event{Kind: eventOpenSaveMeta, Detail: strings.TrimSpace(gameKey + " " + action)})
}

func (s *server) handlePutOpenSaveCustomName(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := openSaveScopeFromRequest(w, r)
	if !ok {
		return
	}
	if tombstone, blocked := s.openSaveScopeBlocked(kind, gameID); blocked {
		writeOpenSaveTombstone(w, tombstone)
		return
	}
	name, ok := decodeOpenSaveCustomName(w, r)
	if !ok {
		return
	}
	revision, hasRevision, valid := parseOpenSaveMetaRevision(r)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid If-Match revision")
		return
	}
	actor := requestDeviceID(r)

	s.openSaveMu.Lock()
	if tombstone, blocked := s.openSaveScopeBlockedLocked(kind, gameID); blocked {
		s.openSaveMu.Unlock()
		writeOpenSaveTombstone(w, tombstone)
		return
	}
	stored, exists := s.openSaveState.Scopes[key]
	if !exists {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusNotFound, "OpenSave scope not found")
		return
	}
	gameKey, _ := openSaveGameKey(kind, gameID, stored)
	members := s.openSaveGameMembersForKeyLocked(gameKey)
	s.metaMu.Lock()
	status, meta, err := s.applyOpenSaveCustomNameLocked(gameKey, name, actor, revision, hasRevision, members, false)
	s.metaMu.Unlock()
	s.openSaveMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot persist OpenSave metadata")
		return
	}
	if status == http.StatusOK {
		s.recordOpenSaveMetaEvent(gameKey, "custom-name")
	}
	writeOpenSaveMetaResult(w, status, meta)
}

func (s *server) handlePutOpenSaveCustomCover(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := openSaveScopeFromRequest(w, r)
	if !ok {
		return
	}
	if tombstone, blocked := s.openSaveScopeBlocked(kind, gameID); blocked {
		writeOpenSaveTombstone(w, tombstone)
		return
	}
	revision, hasRevision, valid := parseOpenSaveMetaRevision(r)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid If-Match revision")
		return
	}
	cover, data, ok := decodeOpenSaveCoverUpload(w, r)
	if !ok {
		return
	}
	actor := requestDeviceID(r)

	s.openSaveMu.Lock()
	if tombstone, blocked := s.openSaveScopeBlockedLocked(kind, gameID); blocked {
		s.openSaveMu.Unlock()
		writeOpenSaveTombstone(w, tombstone)
		return
	}
	stored, exists := s.openSaveState.Scopes[key]
	if !exists {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusNotFound, "OpenSave scope not found")
		return
	}
	gameKey, _ := openSaveGameKey(kind, gameID, stored)
	members := s.openSaveGameMembersForKeyLocked(gameKey)
	s.metaMu.Lock()
	status, meta, err := s.applyOpenSaveCustomCoverLocked(gameKey, cover, data, actor, revision, hasRevision, members, false)
	s.metaMu.Unlock()
	s.openSaveMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot store OpenSave cover")
		return
	}
	if status == http.StatusOK {
		s.recordOpenSaveMetaEvent(gameKey, "custom-cover")
	}
	writeOpenSaveMetaResult(w, status, meta)
}

func (s *server) handleDeleteOpenSaveCustomCover(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := openSaveScopeFromRequest(w, r)
	if !ok {
		return
	}
	if tombstone, blocked := s.openSaveScopeBlocked(kind, gameID); blocked {
		writeOpenSaveTombstone(w, tombstone)
		return
	}
	revision, hasRevision, valid := parseOpenSaveMetaRevision(r)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid If-Match revision")
		return
	}
	actor := requestDeviceID(r)

	s.openSaveMu.Lock()
	if tombstone, blocked := s.openSaveScopeBlockedLocked(kind, gameID); blocked {
		s.openSaveMu.Unlock()
		writeOpenSaveTombstone(w, tombstone)
		return
	}
	stored, exists := s.openSaveState.Scopes[key]
	if !exists {
		s.openSaveMu.Unlock()
		writeError(w, http.StatusNotFound, "OpenSave scope not found")
		return
	}
	gameKey, _ := openSaveGameKey(kind, gameID, stored)
	members := s.openSaveGameMembersForKeyLocked(gameKey)
	s.metaMu.Lock()
	status, meta, err := s.applyClearOpenSaveCustomCoverLocked(gameKey, actor, revision, hasRevision, members, false)
	s.metaMu.Unlock()
	s.openSaveMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot persist OpenSave metadata")
		return
	}
	if status == http.StatusOK {
		s.recordOpenSaveMetaEvent(gameKey, "custom-cover cleared")
	}
	writeOpenSaveMetaResult(w, status, meta)
}

func validOpenSaveGameKey(gameKey string) bool {
	kind, _, found := strings.Cut(gameKey, "/")
	return found && validOpenSaveType(kind) && validManualGameKey(kind, gameKey)
}

func (s *server) handleAdminOpenSaveGameMetaPut(w http.ResponseWriter, r *http.Request) {
	value := r.PathValue("key")
	switch {
	case strings.HasSuffix(value, "/custom-name"):
		gameKey := strings.TrimSuffix(value, "/custom-name")
		if !validOpenSaveGameKey(gameKey) {
			writeError(w, http.StatusBadRequest, "invalid OpenSave game key")
			return
		}
		name, ok := decodeOpenSaveCustomName(w, r)
		if !ok {
			return
		}
		revision, hasRevision, valid := parseOpenSaveMetaRevision(r)
		if !valid {
			writeError(w, http.StatusBadRequest, "invalid If-Match revision")
			return
		}
		s.openSaveMu.Lock()
		members := s.openSaveGameMembersForKeyLocked(gameKey)
		s.metaMu.Lock()
		status, meta, err := s.applyOpenSaveCustomNameLocked(gameKey, name, "admin", revision, hasRevision, members, true)
		s.metaMu.Unlock()
		s.openSaveMu.Unlock()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "cannot persist OpenSave metadata")
			return
		}
		if status == http.StatusOK {
			s.recordOpenSaveMetaEvent(gameKey, "custom-name")
		}
		writeOpenSaveMetaResult(w, status, meta)
	case strings.HasSuffix(value, "/custom-cover"):
		gameKey := strings.TrimSuffix(value, "/custom-cover")
		if !validOpenSaveGameKey(gameKey) {
			writeError(w, http.StatusBadRequest, "invalid OpenSave game key")
			return
		}
		revision, hasRevision, valid := parseOpenSaveMetaRevision(r)
		if !valid {
			writeError(w, http.StatusBadRequest, "invalid If-Match revision")
			return
		}
		cover, data, ok := decodeOpenSaveCoverUpload(w, r)
		if !ok {
			return
		}
		s.openSaveMu.Lock()
		members := s.openSaveGameMembersForKeyLocked(gameKey)
		s.metaMu.Lock()
		status, meta, err := s.applyOpenSaveCustomCoverLocked(gameKey, cover, data, "admin", revision, hasRevision, members, true)
		s.metaMu.Unlock()
		s.openSaveMu.Unlock()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "cannot store OpenSave cover")
			return
		}
		if status == http.StatusOK {
			s.recordOpenSaveMetaEvent(gameKey, "custom-cover")
		}
		writeOpenSaveMetaResult(w, status, meta)
	default:
		writeError(w, http.StatusNotFound, "OpenSave metadata endpoint not found")
	}
}

func (s *server) handleAdminOpenSaveGameMetaDelete(w http.ResponseWriter, r *http.Request) {
	value := r.PathValue("key")
	gameKey, ok := strings.CutSuffix(value, "/custom-cover")
	if !ok {
		writeError(w, http.StatusNotFound, "OpenSave metadata endpoint not found")
		return
	}
	if !validOpenSaveGameKey(gameKey) {
		writeError(w, http.StatusBadRequest, "invalid OpenSave game key")
		return
	}
	revision, hasRevision, valid := parseOpenSaveMetaRevision(r)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid If-Match revision")
		return
	}
	s.openSaveMu.Lock()
	members := s.openSaveGameMembersForKeyLocked(gameKey)
	s.metaMu.Lock()
	status, meta, err := s.applyClearOpenSaveCustomCoverLocked(gameKey, "admin", revision, hasRevision, members, true)
	s.metaMu.Unlock()
	s.openSaveMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot persist OpenSave metadata")
		return
	}
	if status == http.StatusOK {
		s.recordOpenSaveMetaEvent(gameKey, "custom-cover cleared")
	}
	writeOpenSaveMetaResult(w, status, meta)
}

func (s *server) handleOpenSaveCover(w http.ResponseWriter, r *http.Request) {
	digest := r.PathValue("sha256")
	if !validLowerHex(digest, sha256.Size*2) {
		writeError(w, http.StatusNotFound, "OpenSave cover not found")
		return
	}
	s.metaMu.Lock()
	mediaType := ""
	for _, record := range s.openSaveMeta.Games {
		if record.CustomCover != nil && record.CustomCover.SHA256 == digest {
			mediaType = record.CustomCover.MediaType
			break
		}
		if record.Upstream != nil && record.Upstream.SHA256 == digest {
			mediaType = record.Upstream.MediaType
			break
		}
	}
	s.metaMu.Unlock()
	if mediaType == "" {
		writeError(w, http.StatusNotFound, "OpenSave cover not found")
		return
	}
	if mediaType != "image/jpeg" && mediaType != "image/png" {
		mediaType = "application/octet-stream"
	}
	file, err := os.Open(s.openSaveMetaCoverPath(digest))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "OpenSave cover not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot open OpenSave cover")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "OpenSave cover not found")
		return
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("ETag", `"`+digest+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, digest, info.ModTime(), file)
}

// openSaveCoverCounts returns the number of distinct cover files currently
// referenced by metadata, split by source.
func (s *server) openSaveCoverCounts() (custom, cached int) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	customFiles := make(map[string]struct{})
	cachedFiles := make(map[string]struct{})
	for _, record := range s.openSaveMeta.Games {
		if record.CustomCover != nil {
			customFiles[record.CustomCover.SHA256] = struct{}{}
		}
		if record.Upstream != nil {
			cachedFiles[record.Upstream.SHA256] = struct{}{}
		}
	}
	return len(customFiles), len(cachedFiles)
}

// openSaveCoverCount returns the number of distinct custom cover files
// currently referenced by metadata.
func (s *server) openSaveCoverCount() int {
	custom, _ := s.openSaveCoverCounts()
	return custom
}

// removeUnreferencedOpenSaveCovers deletes content-addressed cover files that
// no record references and that are older than one hour. It acquires metaMu
// itself so callers can hold s.mu (the documented s.mu -> metaMu order).
func (s *server) removeUnreferencedOpenSaveCovers(now time.Time, dryRun bool) (int, error) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()

	referenced := make(map[string]struct{})
	for _, record := range s.openSaveMeta.Games {
		if record.CustomCover != nil {
			referenced[record.CustomCover.SHA256] = struct{}{}
		}
		if record.Upstream != nil {
			referenced[record.Upstream.SHA256] = struct{}{}
		}
	}
	removed := 0
	err := filepath.WalkDir(filepath.Join(s.dataDir, openSaveMetaDirectoryName, "covers"),
		func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if errors.Is(walkErr, fs.ErrNotExist) {
					return nil
				}
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			name := entry.Name()
			if strings.HasPrefix(name, ".cover-") {
				if info, err := entry.Info(); err == nil && now.Sub(info.ModTime()) > 24*time.Hour {
					if !dryRun {
						_ = os.Remove(path)
					}
				}
				return nil
			}
			if !validLowerHex(name, sha256.Size*2) {
				return nil
			}
			if _, ok := referenced[name]; ok {
				return nil
			}
			info, err := entry.Info()
			if err != nil || now.Sub(info.ModTime()) <= time.Hour {
				return nil
			}
			removed++
			if dryRun {
				return nil
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return nil
		})
	if err != nil {
		slog.Warn("collect OpenSave covers", "error", err)
		return removed, err
	}
	return removed, nil
}
