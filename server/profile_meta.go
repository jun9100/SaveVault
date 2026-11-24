// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	profileMetaFileName   = "profile-metadata.json"
	maxProfileMetaEntries = 1000
	maxProfileKeyLength   = 160
	maxProfileValueRunes  = 160
	maxProfileBodyBytes   = 256 * 1024
)

// profileMetadataState stores cross-device user display metadata (custom game
// titles, auto-backup exclusions, branch labels, and short notes) per vault.
type profileMetadataState struct {
	Version        int               `json:"version"`
	Revision       int64             `json:"revision"`
	UpdatedAt      time.Time         `json:"updated_at,omitzero"`
	CustomTitles   map[string]string `json:"custom_titles"`
	ExcludedScopes []string          `json:"excluded_scopes"`
	BranchLabels   map[string]string `json:"branch_labels,omitempty"`
	Notes          map[string]string `json:"notes,omitempty"`
}

func defaultProfileMetadata() profileMetadataState {
	return profileMetadataState{
		Version:        1,
		Revision:       0,
		CustomTitles:   map[string]string{},
		ExcludedScopes: []string{},
		BranchLabels:   map[string]string{},
		Notes:          map[string]string{},
	}
}

func (p profileMetadataState) clone() profileMetadataState {
	next := profileMetadataState{
		Version:        1,
		Revision:       p.Revision,
		UpdatedAt:      p.UpdatedAt,
		CustomTitles:   make(map[string]string, len(p.CustomTitles)),
		ExcludedScopes: append([]string(nil), p.ExcludedScopes...),
		BranchLabels:   make(map[string]string, len(p.BranchLabels)),
		Notes:          make(map[string]string, len(p.Notes)),
	}
	for k, v := range p.CustomTitles {
		next.CustomTitles[k] = v
	}
	for k, v := range p.BranchLabels {
		next.BranchLabels[k] = v
	}
	for k, v := range p.Notes {
		next.Notes[k] = v
	}
	if next.ExcludedScopes == nil {
		next.ExcludedScopes = []string{}
	}
	return next
}

func (s *server) profileMetaPath() string {
	return filepath.Join(s.dataDir, profileMetaFileName)
}

func (s *server) loadProfileMetadata() error {
	state := defaultProfileMetadata()
	if err := readJSONFile(s.profileMetaPath(), &state); err != nil {
		return err
	}
	if state.CustomTitles == nil {
		state.CustomTitles = map[string]string{}
	}
	if state.ExcludedScopes == nil {
		state.ExcludedScopes = []string{}
	}
	if state.BranchLabels == nil {
		state.BranchLabels = map[string]string{}
	}
	if state.Notes == nil {
		state.Notes = map[string]string{}
	}
	state.Version = 1
	s.profileMu.Lock()
	s.profileMeta = state
	s.profileMu.Unlock()
	return nil
}

func (s *server) persistProfileMetadataLocked(state profileMetadataState) error {
	state.Version = 1
	return writeJSONFile(s.profileMetaPath(), state)
}

func sanitizeProfileString(raw string, maxRunes int) string {
	clean := strings.TrimSpace(raw)
	runes := []rune(clean)
	if len(runes) > maxRunes {
		clean = string(runes[:maxRunes])
	}
	return clean
}

func sanitizeProfileKey(raw string) (string, bool) {
	clean := strings.TrimSpace(raw)
	if clean == "" || len(clean) > maxProfileKeyLength {
		return "", false
	}
	if strings.ContainsAny(clean, "\x00\r\n") {
		return "", false
	}
	return clean, true
}

func (s *server) handleGetProfileMetadata(w http.ResponseWriter, r *http.Request) {
	s.profileMu.Lock()
	snapshot := s.profileMeta.clone()
	s.profileMu.Unlock()

	etag := fmt.Sprintf(`"rev-%d"`, snapshot.Revision)
	w.Header().Set("ETag", etag)
	if match := strings.TrimSpace(r.Header.Get("If-None-Match")); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *server) handlePutProfileMetadata(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Merge          *bool             `json:"merge"`
		CustomTitles   map[string]string `json:"custom_titles"`
		RemoveTitles   []string          `json:"remove_titles"`
		ExcludedScopes []string          `json:"excluded_scopes"`
		RemoveExcluded []string          `json:"remove_excluded"`
		BranchLabels   map[string]string `json:"branch_labels"`
		Notes          map[string]string `json:"notes"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxProfileBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid profile metadata body")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid profile metadata body")
		return
	}

	mergeMode := true
	if body.Merge != nil {
		mergeMode = *body.Merge
	}

	s.profileMu.Lock()
	defer s.profileMu.Unlock()

	next := s.profileMeta.clone()
	if !mergeMode {
		if body.CustomTitles != nil {
			next.CustomTitles = map[string]string{}
		}
		if body.ExcludedScopes != nil {
			next.ExcludedScopes = []string{}
		}
		if body.BranchLabels != nil {
			next.BranchLabels = map[string]string{}
		}
		if body.Notes != nil {
			next.Notes = map[string]string{}
		}
	}

	for rawKey, rawVal := range body.CustomTitles {
		key, ok := sanitizeProfileKey(rawKey)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid custom_titles key")
			return
		}
		val := sanitizeProfileString(rawVal, maxProfileValueRunes)
		if val == "" {
			delete(next.CustomTitles, key)
		} else {
			next.CustomTitles[key] = val
		}
	}
	for _, rawKey := range body.RemoveTitles {
		if key, ok := sanitizeProfileKey(rawKey); ok {
			delete(next.CustomTitles, key)
		}
	}
	if len(next.CustomTitles) > maxProfileMetaEntries {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("too many custom_titles (max %d)", maxProfileMetaEntries))
		return
	}

	excludedSet := make(map[string]struct{}, len(next.ExcludedScopes)+len(body.ExcludedScopes))
	for _, item := range next.ExcludedScopes {
		excludedSet[item] = struct{}{}
	}
	for _, raw := range body.ExcludedScopes {
		key, ok := sanitizeProfileKey(raw)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid excluded_scopes entry")
			return
		}
		excludedSet[key] = struct{}{}
	}
	for _, raw := range body.RemoveExcluded {
		if key, ok := sanitizeProfileKey(raw); ok {
			delete(excludedSet, key)
		}
	}
	if len(excludedSet) > maxProfileMetaEntries {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("too many excluded_scopes (max %d)", maxProfileMetaEntries))
		return
	}
	next.ExcludedScopes = make([]string, 0, len(excludedSet))
	for item := range excludedSet {
		next.ExcludedScopes = append(next.ExcludedScopes, item)
	}
	sort.Strings(next.ExcludedScopes)

	for rawKey, rawVal := range body.BranchLabels {
		key, ok := sanitizeProfileKey(rawKey)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid branch_labels key")
			return
		}
		val := sanitizeProfileString(rawVal, maxProfileValueRunes)
		if val == "" {
			delete(next.BranchLabels, key)
		} else {
			next.BranchLabels[key] = val
		}
	}
	if len(next.BranchLabels) > maxProfileMetaEntries {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("too many branch_labels (max %d)", maxProfileMetaEntries))
		return
	}

	for rawKey, rawVal := range body.Notes {
		key, ok := sanitizeProfileKey(rawKey)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid notes key")
			return
		}
		val := sanitizeProfileString(rawVal, 512)
		if val == "" {
			delete(next.Notes, key)
		} else {
			next.Notes[key] = val
		}
	}
	if len(next.Notes) > maxProfileMetaEntries {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("too many notes (max %d)", maxProfileMetaEntries))
		return
	}

	next.Revision++
	next.UpdatedAt = s.now().UTC()
	if err := s.persistProfileMetadataLocked(next); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot persist profile metadata")
		return
	}
	s.profileMeta = next
	w.Header().Set("ETag", fmt.Sprintf(`"rev-%d"`, next.Revision))
	writeJSON(w, http.StatusOK, next)
}
