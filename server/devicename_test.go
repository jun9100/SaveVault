// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func randomDeviceTestToken(t *testing.T) string {
	t.Helper()
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(token[:])
}

func deviceTestRequest(t *testing.T, method, target, token, deviceName string, body []byte) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-OpenSave-Device-ID", "device-1")
	request.Header.Set("X-OpenSave-Device-Name", deviceName)
	request.Header.Set("X-OpenSave-Platform", "opensave-windows")
	return request
}

func uploadDeviceNameSnapshot(t *testing.T, baseURL, token, gameID, deviceName string) openSaveScopeSnapshot {
	t.Helper()
	contents := []byte("save-" + gameID)
	archive := gameID + "__main__snapshot-1.zip"
	response, err := http.DefaultClient.Do(deviceTestRequest(t, http.MethodPut,
		baseURL+"/v1/opensave/files/switch/"+archive, token, deviceName, contents))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("archive upload status = %d, want 201", response.StatusCode)
	}

	digest := sha256.Sum256(contents)
	candidate := openSaveScopeCandidate{
		ID:            "snapshot-1",
		Archive:       archive,
		ContentDigest: hex.EncodeToString(digest[:]),
		CreatedAt:     time.Now().UTC(),
	}
	payload, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	request := deviceTestRequest(t, http.MethodPut,
		baseURL+"/v1/opensave/scopes/switch/"+gameID+"/snapshots/"+candidate.ID, token, deviceName, payload)
	request.Header.Set("If-Match", `""`)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("snapshot upload status = %d, want 201", response.StatusCode)
	}
	var snapshot openSaveScopeSnapshot
	if err := json.NewDecoder(response.Body).Decode(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestDecodeDeviceHeader(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"rfc8187", "UTF-8''%E5%B0%8F%E6%98%8E%E7%9A%84%E6%89%8B%E6%9C%BA", "小明的手机"},
		{"lowercase prefix", "utf-8''Odin%202", "Odin 2"},
		{"plus is literal", "UTF-8''Odin+2", "Odin+2"},
		{"plain ascii", "Test Desktop", "Test Desktop"},
		{"direct utf8", "小明的手机", "小明的手机"},
		{"malformed escape", "UTF-8''%ZZ", "UTF-8''%ZZ"},
		{"invalid utf8", "UTF-8''%FF", "UTF-8''%FF"},
		{"control character", "UTF-8''bad%0Aname", "UTF-8''bad%0Aname"},
		{"empty value", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeDeviceHeader(tc.value); got != tc.want {
				t.Fatalf("decodeDeviceHeader(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

func TestRequestClientHeaderDecodesOnlyDeviceName(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("X-Eden-Device-ID", "UTF-8''device")
	request.Header.Set("X-Eden-Device-Name", "UTF-8''%E5%B0%8F%E6%98%8E")
	request.Header.Set("X-Eden-Platform", "UTF-8''Odin%202")
	request.Header.Set("X-Eden-Version", "UTF-8''1.0")

	if got := requestClientHeader(request, "X-Eden-Device-Name", "X-OpenSave-Device-Name"); got != "小明" {
		t.Fatalf("Eden device name = %q, want %q", got, "小明")
	}
	if got := requestClientHeader(request, "X-Eden-Device-ID", "X-OpenSave-Device-ID"); got != "UTF-8''device" {
		t.Fatalf("device id = %q, want unchanged", got)
	}
	if got := requestClientHeader(request, "X-Eden-Platform", "X-OpenSave-Platform"); got != "UTF-8''Odin%202" {
		t.Fatalf("platform = %q, want unchanged", got)
	}
	if got := requestClientHeader(request, "X-Eden-Version", "X-OpenSave-Version"); got != "UTF-8''1.0" {
		t.Fatalf("version = %q, want unchanged", got)
	}

	request.Header.Set("X-OpenSave-Device-Name", "utf-8''Odin%202")
	if got := requestClientHeader(request, "X-Eden-Device-Name", "X-OpenSave-Device-Name"); got != "Odin 2" {
		t.Fatalf("OpenSave device name = %q, want %q", got, "Odin 2")
	}
}

func TestOpenSaveSnapshotRecordsDecodedDeviceName(t *testing.T) {
	token := randomDeviceTestToken(t)
	s, err := newServer(t.TempDir(), token, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s.handler())
	defer httpServer.Close()

	longName := strings.Repeat("小", 100)
	cases := []struct {
		name       string
		header     string
		deviceName string
	}{
		{"rfc8187", "UTF-8''%E5%B0%8F%E6%98%8E%E7%9A%84%E6%89%8B%E6%9C%BA", "小明的手机"},
		{"lowercase prefix", "utf-8''Odin%202", "Odin 2"},
		{"direct utf8", "小明的手机", "小明的手机"},
		{"plain ascii", "Test Desktop", "Test Desktop"},
		{"malformed escape", "UTF-8''%ZZ", "UTF-8''%ZZ"},
		{"invalid utf8", "UTF-8''%FF", "UTF-8''%FF"},
		{"control character", "UTF-8''bad%0Aname", "UTF-8''bad%0Aname"},
		{"truncated after decoding", "UTF-8''" + url.PathEscape(longName), strings.Repeat("小", 80)},
	}
	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gameID := fmt.Sprintf("device-name-%d", index)
			snapshot := uploadDeviceNameSnapshot(t, httpServer.URL, token, gameID, tc.header)
			if snapshot.DeviceName != tc.deviceName {
				t.Fatalf("snapshot deviceName = %q, want %q", snapshot.DeviceName, tc.deviceName)
			}
		})
	}
}
