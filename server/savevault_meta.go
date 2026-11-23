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

// SaveVault metadata is stored next to the archives but never inside them. A
// metadata write must not touch a head, a snapshot, or a conflict copy.
//
// Lock order: s.mu -> tokensMu -> metaMu, and s.mu -> metaMu. saveVaultMu may
// be held before metaMu (saveVaultMu -> metaMu). metaMu is never held while
// acquiring s.mu, tokensMu, or saveVaultMu.

const (
	saveVaultMetaFileName      = "savevault-meta.json"
	saveVaultMetaDirectoryName = "savevault-meta"

	maxSaveVaultCustomCoverBytes int64 = 1 << 20
	maxSaveVaultCoverDimension         = 2048

	saveVaultCoverSourceCustom = "custom"
	saveVaultCoverSourceCached = "cached"

	saveVaultNameSourceCustom = "custom"
	saveVaultNameSourceTable  = "table"
	saveVaultNameSourceClient = "client"
	saveVaultNameSourceKey    = "key"
)

// saveVaultMetaCover is both the custom and the upstream cover description. The
// upstream-only FetchedAt field is omitted for custom covers.
type saveVaultMetaCover struct {
	SHA256    string    `json:"sha256"`
	MediaType string    `json:"mediaType"`
	Width     int       `json:"width"`
	Height    int       `json:"height"`
	FetchedAt time.Time `json:"fetchedAt,omitzero"`
}

type saveVaultMetaRecord struct {
	Rev               int                `json:"rev,omitempty"`
	CustomName        string             `json:"customName,omitempty"`
	CustomCover       *saveVaultMetaCover `json:"customCover,omitempty"`
	Upstream          *saveVaultMetaCover `json:"upstream,omitempty"`
	UpstreamMissUntil time.Time          `json:"upstreamMissUntil,omitzero"`
	UpstreamFailures  int                `json:"upstreamFailures,omitempty"`
	UpdatedAt         time.Time          `json:"updatedAt,omitzero"`
	UpdatedBy         string             `json:"updatedBy,omitempty"`
}

type saveVaultMetaSettings struct {
	CoverFetch bool `json:"coverFetch"`
}

type saveVaultMetaState struct {
	Version  int                           `json:"version"`
	Settings saveVaultMetaSettings          `json:"settings"`
	Games    map[string]saveVaultMetaRecord `json:"games"`
}

func defaultSaveVaultMetaState() saveVaultMetaState {
	return saveVaultMetaState{
		Version: 1,
		Games:   make(map[string]saveVaultMetaRecord),
	}
}

func (record saveVaultMetaRecord) clone() saveVaultMetaRecord {
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

func (state saveVaultMetaState) clone() saveVaultMetaState {
	next := saveVaultMetaState{
		Version:  state.Version,
		Settings: state.Settings,
		Games:    make(map[string]saveVaultMetaRecord, len(state.Games)+1),
	}
	for key, record := range state.Games {
		next.Games[key] = record.clone()
	}
	return next
}

// saveVaultGameMeta is the client-facing meta field of a game.
type saveVaultGameMeta struct {
	Rev        int                    `json:"rev"`
	Name       string                 `json:"name"`
	NameSource string                 `json:"nameSource"`
	Cover      *saveVaultGameMetaCover `json:"cover,omitempty"`
	UpdatedAt  time.Time              `json:"updatedAt,omitzero"`
	UpdatedBy  string                 `json:"updatedBy,omitempty"`
}

type saveVaultGameMetaCover struct {
	Source    string `json:"source"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"mediaType"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	URL       string `json:"url"`
}

func (s *server) saveVaultMetaPath() string {
	return filepath.Join(s.dataDir, saveVaultMetaFileName)
}

func (s *server) saveVaultMetaCoverPath(digest string) string {
	return filepath.Join(s.dataDir, saveVaultMetaDirectoryName, "covers", digest[:2], digest)
}

// loadSaveVaultMeta never creates the file. A deployment that has not used a C5
// write endpoint keeps no metadata state at all.
func (s *server) loadSaveVaultMeta() error {
	state := saveVaultMetaState{}
	if err := readJSONFile(s.saveVaultMetaPath(), &state); err != nil {
		return err
	}
	if state.Version != 0 && state.Version != 1 {
		return fmt.Errorf("%s contains an unsupported version", saveVaultMetaFileName)
	}
	if state.Games == nil {
		state.Games = make(map[string]saveVaultMetaRecord)
	}
	state.Version = 1
	s.saveVaultMeta = state
	return nil
}

func (s *server) persistSaveVaultMeta(state saveVaultMetaState) error {
	state.Version = 1
	return writeJSONFile(s.saveVaultMetaPath(), state)
}

func saveVaultGameKeyID(gameKey string) string {
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

func saveVaultGameDisplayName(gameKey string, record saveVaultMetaRecord, members []saveVaultGameMember) (string, string) {
	if record.CustomName != "" {
		return record.CustomName, saveVaultNameSourceCustom
	}
	if title, ok := switchTitleForGameKey(gameKey); ok {
		return title.Name, saveVaultNameSourceTable
	}
	if name, ok := saveVaultGameClientName(members); ok && name != "" {
		return name, saveVaultNameSourceClient
	}
	return saveVaultGameKeyID(gameKey), saveVaultNameSourceKey
}

func saveVaultCoverURL(digest string, admin bool) string {
	if admin {
		return "/admin/api/savevault/covers/" + digest
	}
	return "/v1/savevault/covers/" + digest
}

func saveVaultGameMetaView(gameKey string, record saveVaultMetaRecord, members []saveVaultGameMember, admin bool) saveVaultGameMeta {
	name, source := saveVaultGameDisplayName(gameKey, record, members)
	view := saveVaultGameMeta{
		Rev:        record.Rev,
		Name:       name,
		NameSource: source,
		UpdatedAt:  record.UpdatedAt,
		UpdatedBy:  record.UpdatedBy,
	}
	cover, source := record.CustomCover, saveVaultCoverSourceCustom
	if cover == nil && record.Upstream != nil {
		cover, source = record.Upstream, saveVaultCoverSourceCached
	}
	if cover != nil {
		view.Cover = &saveVaultGameMetaCover{
			Source:    source,
			SHA256:    cover.SHA256,
			MediaType: cover.MediaType,
			Width:     cover.Width,
			Height:    cover.Height,
			URL:       saveVaultCoverURL(cover.SHA256, admin),
		}
	}
	return view
}

// saveVaultGameMetaLocked returns the current view of one game. The caller must
// hold metaMu.
func (s *server) saveVaultGameMetaLocked(gameKey string, members []saveVaultGameMember, admin bool) saveVaultGameMeta {
	return saveVaultGameMetaView(gameKey, s.saveVaultMeta.Games[gameKey], members, admin)
}

func parseSaveVaultMetaRevision(r *http.Request) (int, bool, bool) {
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

func decodeSaveVaultCustomName(w http.ResponseWriter, r *http.Request) (string, bool) {
	var request struct {
		Name string `json:"name"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMetadataBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid SaveVault custom name")
		return "", false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid SaveVault custom name")
		return "", false
	}
	name := strings.TrimSpace(request.Name)
	if !validGameName(name) {
		writeError(w, http.StatusBadRequest, "invalid SaveVault custom name")
		return "", false
	}
	return name, true
}

var saveVaultPNGSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

func saveVaultImageMediaType(data []byte) string {
	switch {
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "image/jpeg"
	case len(data) >= len(saveVaultPNGSignature) && bytes.Equal(data[:len(saveVaultPNGSignature)], saveVaultPNGSignature):
		return "image/png"
	}
	return ""
}

func decodeSaveVaultCoverUpload(w http.ResponseWriter, r *http.Request) (saveVaultMetaCover, []byte, bool) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mediaType != "image/jpeg" && mediaType != "image/png") {
		writeError(w, http.StatusBadRequest, "Content-Type must be image/jpeg or image/png")
		return saveVaultMetaCover{}, nil, false
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSaveVaultCustomCoverBytes))
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(w, http.StatusRequestEntityTooLarge, "SaveVault cover exceeds 1 MiB")
		} else {
			writeError(w, http.StatusBadRequest, "cannot read SaveVault cover")
		}
		return saveVaultMetaCover{}, nil, false
	}
	if len(data) == 0 {
		writeError(w, http.StatusBadRequest, "SaveVault cover is empty")
		return saveVaultMetaCover{}, nil, false
	}
	if saveVaultImageMediaType(data) != mediaType {
		writeError(w, http.StatusBadRequest, "SaveVault cover does not match Content-Type")
		return saveVaultMetaCover{}, nil, false
	}
	cover, ok := saveVaultCoverFromImage(data, maxSaveVaultCoverDimension)
	if !ok {
		writeError(w, http.StatusBadRequest, "unsupported SaveVault cover image or invalid dimensions")
		return saveVaultMetaCover{}, nil, false
	}
	return cover, data, true
}

// saveVaultCoverFromImage validates image bytes using their magic and decoded
// dimensions. Content-Type headers are never trusted for this decision.
func saveVaultCoverFromImage(data []byte, maxDimension int) (saveVaultMetaCover, bool) {
	mediaType := saveVaultImageMediaType(data)
	if mediaType == "" {
		return saveVaultMetaCover{}, false
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "jpeg" && format != "png") {
		return saveVaultMetaCover{}, false
	}
	if (mediaType == "image/jpeg" && format != "jpeg") || (mediaType == "image/png" && format != "png") {
		return saveVaultMetaCover{}, false
	}
	if config.Width <= 0 || config.Height <= 0 ||
		config.Width > maxDimension || config.Height > maxDimension {
		return saveVaultMetaCover{}, false
	}
	digest := sha256.Sum256(data)
	return saveVaultMetaCover{
		SHA256:    hex.EncodeToString(digest[:]),
		MediaType: mediaType,
		Width:     config.Width,
		Height:    config.Height,
	}, true
}

func (s *server) storeSaveVaultCover(digest string, data []byte) (bool, error) {
	path := s.saveVaultMetaCoverPath(digest)
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

func (s *server) applySaveVaultCustomNameLocked(gameKey, name, actor string, ifMatch int, hasIfMatch bool,
	members []saveVaultGameMember, admin bool) (int, saveVaultGameMeta, error) {
	record := s.saveVaultMeta.Games[gameKey]
	current := saveVaultGameMetaView(gameKey, record, members, admin)
	if hasIfMatch && ifMatch != record.Rev {
		return http.StatusPreconditionFailed, current, nil
	}
	if record.CustomName == name {
		return http.StatusNoContent, current, nil
	}
	next := s.saveVaultMeta.clone()
	record.CustomName = name
	record.Rev++
	record.UpdatedAt = s.now().UTC()
	record.UpdatedBy = actor
	next.Games[gameKey] = record
	if err := s.persistSaveVaultMeta(next); err != nil {
		return http.StatusInternalServerError, current, err
	}
	s.saveVaultMeta = next
	return http.StatusOK, saveVaultGameMetaView(gameKey, record, members, admin), nil
}

func (s *server) applySaveVaultCustomCoverLocked(gameKey string, cover saveVaultMetaCover, data []byte, actor string,
	ifMatch int, hasIfMatch bool, members []saveVaultGameMember, admin bool) (int, saveVaultGameMeta, error) {
	record := s.saveVaultMeta.Games[gameKey]
	current := saveVaultGameMetaView(gameKey, record, members, admin)
	if hasIfMatch && ifMatch != record.Rev {
		return http.StatusPreconditionFailed, current, nil
	}
	if record.CustomCover != nil && record.CustomCover.SHA256 == cover.SHA256 {
		return http.StatusNoContent, current, nil
	}
	created, err := s.storeSaveVaultCover(cover.SHA256, data)
	if err != nil {
		return http.StatusInternalServerError, current, err
	}
	next := s.saveVaultMeta.clone()
	record.CustomCover = &cover
	record.Rev++
	record.UpdatedAt = s.now().UTC()
	record.UpdatedBy = actor
	next.Games[gameKey] = record
	if err := s.persistSaveVaultMeta(next); err != nil {
		if created {
			_ = os.Remove(s.saveVaultMetaCoverPath(cover.SHA256))
		}
		return http.StatusInternalServerError, current, err
	}
	s.saveVaultMeta = next
	return http.StatusOK, saveVaultGameMetaView(gameKey, record, members, admin), nil
}

func (s *server) applyClearSaveVaultCustomCoverLocked(gameKey, actor string, ifMatch int, hasIfMatch bool,
	members []saveVaultGameMember, admin bool) (int, saveVaultGameMeta, error) {
	record := s.saveVaultMeta.Games[gameKey]
	current := saveVaultGameMetaView(gameKey, record, members, admin)
	if hasIfMatch && ifMatch != record.Rev {
		return http.StatusPreconditionFailed, current, nil
	}
	if record.CustomCover == nil {
		return http.StatusNoContent, current, nil
	}
	next := s.saveVaultMeta.clone()
	record.CustomCover = nil
	record.Rev++
	record.UpdatedAt = s.now().UTC()
	record.UpdatedBy = actor
	next.Games[gameKey] = record
	if err := s.persistSaveVaultMeta(next); err != nil {
		return http.StatusInternalServerError, current, err
	}
	s.saveVaultMeta = next
	return http.StatusOK, saveVaultGameMetaView(gameKey, record, members, admin), nil
}

func writeSaveVaultMetaResult(w http.ResponseWriter, status int, meta saveVaultGameMeta) {
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	writeJSON(w, status, meta)
}

func (s *server) recordSaveVaultMetaEvent(gameKey, action string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordEventLocked(event{Kind: eventSaveVaultMeta, Detail: strings.TrimSpace(gameKey + " " + action)})
}

func (s *server) handlePutSaveVaultCustomName(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := saveVaultScopeFromRequest(w, r)
	if !ok {
		return
	}
	if tombstone, blocked := s.saveVaultScopeBlocked(kind, gameID); blocked {
		writeSaveVaultTombstone(w, tombstone)
		return
	}
	name, ok := decodeSaveVaultCustomName(w, r)
	if !ok {
		return
	}
	revision, hasRevision, valid := parseSaveVaultMetaRevision(r)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid If-Match revision")
		return
	}
	actor := requestDeviceID(r)

	s.saveVaultMu.Lock()
	if tombstone, blocked := s.saveVaultScopeBlockedLocked(kind, gameID); blocked {
		s.saveVaultMu.Unlock()
		writeSaveVaultTombstone(w, tombstone)
		return
	}
	stored, exists := s.saveVaultState.Scopes[key]
	if !exists {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusNotFound, "SaveVault scope not found")
		return
	}
	gameKey, _ := saveVaultGameKey(kind, gameID, stored)
	members := s.saveVaultGameMembersForKeyLocked(gameKey)
	s.metaMu.Lock()
	status, meta, err := s.applySaveVaultCustomNameLocked(gameKey, name, actor, revision, hasRevision, members, false)
	s.metaMu.Unlock()
	s.saveVaultMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot persist SaveVault metadata")
		return
	}
	if status == http.StatusOK {
		s.recordSaveVaultMetaEvent(gameKey, "custom-name")
	}
	writeSaveVaultMetaResult(w, status, meta)
}

func (s *server) handlePutSaveVaultCustomCover(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := saveVaultScopeFromRequest(w, r)
	if !ok {
		return
	}
	if tombstone, blocked := s.saveVaultScopeBlocked(kind, gameID); blocked {
		writeSaveVaultTombstone(w, tombstone)
		return
	}
	revision, hasRevision, valid := parseSaveVaultMetaRevision(r)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid If-Match revision")
		return
	}
	cover, data, ok := decodeSaveVaultCoverUpload(w, r)
	if !ok {
		return
	}
	actor := requestDeviceID(r)

	s.saveVaultMu.Lock()
	if tombstone, blocked := s.saveVaultScopeBlockedLocked(kind, gameID); blocked {
		s.saveVaultMu.Unlock()
		writeSaveVaultTombstone(w, tombstone)
		return
	}
	stored, exists := s.saveVaultState.Scopes[key]
	if !exists {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusNotFound, "SaveVault scope not found")
		return
	}
	gameKey, _ := saveVaultGameKey(kind, gameID, stored)
	members := s.saveVaultGameMembersForKeyLocked(gameKey)
	s.metaMu.Lock()
	status, meta, err := s.applySaveVaultCustomCoverLocked(gameKey, cover, data, actor, revision, hasRevision, members, false)
	s.metaMu.Unlock()
	s.saveVaultMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot store SaveVault cover")
		return
	}
	if status == http.StatusOK {
		s.recordSaveVaultMetaEvent(gameKey, "custom-cover")
	}
	writeSaveVaultMetaResult(w, status, meta)
}

func (s *server) handleDeleteSaveVaultCustomCover(w http.ResponseWriter, r *http.Request) {
	kind, gameID, key, ok := saveVaultScopeFromRequest(w, r)
	if !ok {
		return
	}
	if tombstone, blocked := s.saveVaultScopeBlocked(kind, gameID); blocked {
		writeSaveVaultTombstone(w, tombstone)
		return
	}
	revision, hasRevision, valid := parseSaveVaultMetaRevision(r)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid If-Match revision")
		return
	}
	actor := requestDeviceID(r)

	s.saveVaultMu.Lock()
	if tombstone, blocked := s.saveVaultScopeBlockedLocked(kind, gameID); blocked {
		s.saveVaultMu.Unlock()
		writeSaveVaultTombstone(w, tombstone)
		return
	}
	stored, exists := s.saveVaultState.Scopes[key]
	if !exists {
		s.saveVaultMu.Unlock()
		writeError(w, http.StatusNotFound, "SaveVault scope not found")
		return
	}
	gameKey, _ := saveVaultGameKey(kind, gameID, stored)
	members := s.saveVaultGameMembersForKeyLocked(gameKey)
	s.metaMu.Lock()
	status, meta, err := s.applyClearSaveVaultCustomCoverLocked(gameKey, actor, revision, hasRevision, members, false)
	s.metaMu.Unlock()
	s.saveVaultMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot persist SaveVault metadata")
		return
	}
	if status == http.StatusOK {
		s.recordSaveVaultMetaEvent(gameKey, "custom-cover cleared")
	}
	writeSaveVaultMetaResult(w, status, meta)
}

func validSaveVaultGameKey(gameKey string) bool {
	kind, _, found := strings.Cut(gameKey, "/")
	return found && validSaveVaultType(kind) && validManualGameKey(kind, gameKey)
}

func (s *server) handleAdminSaveVaultGameMetaPut(w http.ResponseWriter, r *http.Request) {
	value := r.PathValue("key")
	switch {
	case strings.HasSuffix(value, "/custom-name"):
		gameKey := strings.TrimSuffix(value, "/custom-name")
		if !validSaveVaultGameKey(gameKey) {
			writeError(w, http.StatusBadRequest, "invalid SaveVault game key")
			return
		}
		name, ok := decodeSaveVaultCustomName(w, r)
		if !ok {
			return
		}
		revision, hasRevision, valid := parseSaveVaultMetaRevision(r)
		if !valid {
			writeError(w, http.StatusBadRequest, "invalid If-Match revision")
			return
		}
		s.saveVaultMu.Lock()
		members := s.saveVaultGameMembersForKeyLocked(gameKey)
		s.metaMu.Lock()
		status, meta, err := s.applySaveVaultCustomNameLocked(gameKey, name, "admin", revision, hasRevision, members, true)
		s.metaMu.Unlock()
		s.saveVaultMu.Unlock()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "cannot persist SaveVault metadata")
			return
		}
		if status == http.StatusOK {
			s.recordSaveVaultMetaEvent(gameKey, "custom-name")
		}
		writeSaveVaultMetaResult(w, status, meta)
	case strings.HasSuffix(value, "/custom-cover"):
		gameKey := strings.TrimSuffix(value, "/custom-cover")
		if !validSaveVaultGameKey(gameKey) {
			writeError(w, http.StatusBadRequest, "invalid SaveVault game key")
			return
		}
		revision, hasRevision, valid := parseSaveVaultMetaRevision(r)
		if !valid {
			writeError(w, http.StatusBadRequest, "invalid If-Match revision")
			return
		}
		cover, data, ok := decodeSaveVaultCoverUpload(w, r)
		if !ok {
			return
		}
		s.saveVaultMu.Lock()
		members := s.saveVaultGameMembersForKeyLocked(gameKey)
		s.metaMu.Lock()
		status, meta, err := s.applySaveVaultCustomCoverLocked(gameKey, cover, data, "admin", revision, hasRevision, members, true)
		s.metaMu.Unlock()
		s.saveVaultMu.Unlock()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "cannot store SaveVault cover")
			return
		}
		if status == http.StatusOK {
			s.recordSaveVaultMetaEvent(gameKey, "custom-cover")
		}
		writeSaveVaultMetaResult(w, status, meta)
	default:
		writeError(w, http.StatusNotFound, "SaveVault metadata endpoint not found")
	}
}

func (s *server) handleAdminSaveVaultGameMetaDelete(w http.ResponseWriter, r *http.Request) {
	value := r.PathValue("key")
	gameKey, ok := strings.CutSuffix(value, "/custom-cover")
	if !ok {
		writeError(w, http.StatusNotFound, "SaveVault metadata endpoint not found")
		return
	}
	if !validSaveVaultGameKey(gameKey) {
		writeError(w, http.StatusBadRequest, "invalid SaveVault game key")
		return
	}
	revision, hasRevision, valid := parseSaveVaultMetaRevision(r)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid If-Match revision")
		return
	}
	s.saveVaultMu.Lock()
	members := s.saveVaultGameMembersForKeyLocked(gameKey)
	s.metaMu.Lock()
	status, meta, err := s.applyClearSaveVaultCustomCoverLocked(gameKey, "admin", revision, hasRevision, members, true)
	s.metaMu.Unlock()
	s.saveVaultMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot persist SaveVault metadata")
		return
	}
	if status == http.StatusOK {
		s.recordSaveVaultMetaEvent(gameKey, "custom-cover cleared")
	}
	writeSaveVaultMetaResult(w, status, meta)
}

func (s *server) handleSaveVaultCover(w http.ResponseWriter, r *http.Request) {
	digest := r.PathValue("sha256")
	if !validLowerHex(digest, sha256.Size*2) {
		writeError(w, http.StatusNotFound, "SaveVault cover not found")
		return
	}
	s.metaMu.Lock()
	mediaType := ""
	for _, record := range s.saveVaultMeta.Games {
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
		writeError(w, http.StatusNotFound, "SaveVault cover not found")
		return
	}
	if mediaType != "image/jpeg" && mediaType != "image/png" {
		mediaType = "application/octet-stream"
	}
	file, err := os.Open(s.saveVaultMetaCoverPath(digest))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "SaveVault cover not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot open SaveVault cover")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "SaveVault cover not found")
		return
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("ETag", `"`+digest+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, digest, info.ModTime(), file)
}

// saveVaultCoverCounts returns the number of distinct cover files currently
// referenced by metadata, split by source.
func (s *server) saveVaultCoverCounts() (custom, cached int) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	customFiles := make(map[string]struct{})
	cachedFiles := make(map[string]struct{})
	for _, record := range s.saveVaultMeta.Games {
		if record.CustomCover != nil {
			customFiles[record.CustomCover.SHA256] = struct{}{}
		}
		if record.Upstream != nil {
			cachedFiles[record.Upstream.SHA256] = struct{}{}
		}
	}
	return len(customFiles), len(cachedFiles)
}

// saveVaultCoverCount returns the number of distinct custom cover files
// currently referenced by metadata.
func (s *server) saveVaultCoverCount() int {
	custom, _ := s.saveVaultCoverCounts()
	return custom
}

// removeUnreferencedSaveVaultCovers deletes content-addressed cover files that
// no record references and that are older than one hour. It acquires metaMu
// itself so callers can hold s.mu (the documented s.mu -> metaMu order).
func (s *server) removeUnreferencedSaveVaultCovers(now time.Time, dryRun bool) (int, error) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()

	referenced := make(map[string]struct{})
	for _, record := range s.saveVaultMeta.Games {
		if record.CustomCover != nil {
			referenced[record.CustomCover.SHA256] = struct{}{}
		}
		if record.Upstream != nil {
			referenced[record.Upstream.SHA256] = struct{}{}
		}
	}
	removed := 0
	err := filepath.WalkDir(filepath.Join(s.dataDir, saveVaultMetaDirectoryName, "covers"),
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
		slog.Warn("collect SaveVault covers", "error", err)
		return removed, err
	}
	return removed, nil
}
