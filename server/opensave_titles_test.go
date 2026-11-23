// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"net/http"
	"strings"
	"testing"
)

func TestSwitchTitleTableDownloadAndConditionalRequest(t *testing.T) {
	_, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()

	request := openSaveScopeRequest(t, http.MethodGet, httpServer.URL+"/v1/opensave/titles/switch", nil)
	response, body := openSaveMetaDo(t, request)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("titles status = %d", response.StatusCode)
	}
	etag := `"` + switchTitlesVersion() + `"`
	if response.Header.Get("Content-Type") != "application/gzip" {
		t.Fatalf("titles Content-Type = %q", response.Header.Get("Content-Type"))
	}
	if response.Header.Get("ETag") != etag {
		t.Fatalf("titles ETag = %q, want %q", response.Header.Get("ETag"), etag)
	}
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("titles are not gzip: %v", err)
	}
	scanner := bufio.NewScanner(reader)
	lines := 0
	foundOdyssey := false
	for scanner.Scan() {
		lines++
		if strings.HasPrefix(scanner.Text(), "0100000000010000\t"+testSwitchOdysseyName+"\t") {
			foundOdyssey = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if lines <= 20000 || !foundOdyssey {
		t.Fatalf("title table lines = %d, odyssey = %v", lines, foundOdyssey)
	}

	request = openSaveScopeRequest(t, http.MethodGet, httpServer.URL+"/v1/opensave/titles/switch", nil)
	request.Header.Set("If-None-Match", etag)
	response, body = openSaveMetaDo(t, request)
	if response.StatusCode != http.StatusNotModified || len(body) != 0 {
		t.Fatalf("matching If-None-Match = %d bytes=%d", response.StatusCode, len(body))
	}
	if response.Header.Get("ETag") != etag {
		t.Fatalf("304 ETag = %q", response.Header.Get("ETag"))
	}

	request = openSaveScopeRequest(t, http.MethodGet, httpServer.URL+"/v1/opensave/titles/switch", nil)
	request.Header.Set("If-None-Match", `"2020-01-01"`)
	response, body = openSaveMetaDo(t, request)
	if response.StatusCode != http.StatusOK || len(body) == 0 {
		t.Fatalf("non-matching If-None-Match = %d bytes=%d", response.StatusCode, len(body))
	}
}

func TestSwitchTitleLookupIsCaseInsensitive(t *testing.T) {
	title, ok := switchTitleFor(strings.ToLower("0100000000010000"))
	if !ok || title.Name != testSwitchOdysseyName {
		t.Fatalf("lowercase lookup = %+v, %v", title, ok)
	}
	if title.IconHash == "" {
		t.Fatal("expected an eShop icon hash")
	}
	if _, ok := switchTitleFor("not-a-title"); ok {
		t.Fatal("malformed TitleID was found")
	}
	if _, err := parseSwitchTitles([]byte("not gzip")); err == nil {
		t.Fatal("broken gzip was accepted")
	}
}
