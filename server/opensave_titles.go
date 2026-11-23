// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
)

// The Switch title table is the same data and format as the Android built-in
// table (android/app/src/main/assets/switch-titles.tsv in OpenSave), gzipped
// with `gzip -n -9`. It is embedded in the binary, updated for each release and
// never fetched at runtime. titledb is the upstream source; its licensing is
// unclear, so the table is only meant for self-hosted use.
//
//go:embed titles/switch-titles.tsv.gz
var switchTitlesGzip []byte

//go:embed titles/switch-titles.version
var switchTitlesVersionFile string

type switchTitle struct {
	Name     string
	IconHash string
}

var (
	switchTitlesOnce  sync.Once
	switchTitlesTable map[string]switchTitle
)

// parseSwitchTitles accepts the Android table format:
// TitleID<TAB>name<TAB>eShop icon hash. Malformed lines are skipped; a broken
// stream or a table with no usable rows is an error.
func parseSwitchTitles(data []byte) (map[string]switchTitle, error) {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	table := make(map[string]switchTitle, 25000)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) < 2 {
			continue
		}
		titleID := strings.ToUpper(strings.TrimSpace(fields[0]))
		if !switchTitleIDPattern.MatchString(titleID) {
			continue
		}
		name := strings.TrimSpace(fields[1])
		if name == "" {
			continue
		}
		title := switchTitle{Name: name}
		if len(fields) == 3 {
			title.IconHash = strings.TrimSpace(fields[2])
		}
		table[titleID] = title
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(table) == 0 {
		return nil, errors.New("Switch title table is empty")
	}
	return table, nil
}

// switchTitles loads the embedded table on first use. A parse failure is
// logged and treated as an empty table so a damaged binary never panics while
// serving a read-only request.
func switchTitles() map[string]switchTitle {
	switchTitlesOnce.Do(func() {
		switchTitlesTable = make(map[string]switchTitle)
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("parse embedded Switch title table", "error", recovered)
				switchTitlesTable = make(map[string]switchTitle)
			}
		}()
		table, err := parseSwitchTitles(switchTitlesGzip)
		if err != nil {
			slog.Error("parse embedded Switch title table", "error", err)
			return
		}
		switchTitlesTable = table
	})
	return switchTitlesTable
}

// switchTitleFor looks a TitleID up case-insensitively.
func switchTitleFor(titleID string) (switchTitle, bool) {
	titleID = strings.ToUpper(strings.TrimSpace(titleID))
	if !switchTitleIDPattern.MatchString(titleID) {
		return switchTitle{}, false
	}
	title, exists := switchTitles()[titleID]
	return title, exists && title.Name != ""
}

func switchTitlesVersion() string {
	return strings.TrimSpace(switchTitlesVersionFile)
}

// httpETagMatches implements the common If-None-Match forms: a list, "*", and
// weak validators.
func httpETagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" {
			return true
		}
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == etag {
			return true
		}
	}
	return false
}

func (s *server) handleSwitchTitleTable(w http.ResponseWriter, r *http.Request) {
	etag := `"` + switchTitlesVersion() + `"`
	w.Header().Set("ETag", etag)
	if match := r.Header.Get("If-None-Match"); match != "" && httpETagMatches(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	if _, err := w.Write(switchTitlesGzip); err != nil {
		slog.Warn("write Switch title table", "error", err)
	}
}
