// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestP1WebhookPayloadFormatsAndHMAC(t *testing.T) {
	notice := webhookNotification{
		Event:     "conflict",
		VaultID:   "default",
		Title:     "SaveVault 存档冲突告警",
		Body:      "范围: switch/zelda · 设备: Odin2",
		Timestamp: time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC),
		Scope:     "switch/zelda",
		Device:    "Odin2",
	}

	cases := []struct {
		typ       string
		secret    string
		checkReq  func(t *testing.T, r *http.Request, body []byte)
	}{
		{
			typ:    webhookTypeGeneric,
			secret: "top-secret",
			checkReq: func(t *testing.T, r *http.Request, body []byte) {
				if got := r.Header.Get("X-SaveVault-Event"); got != "conflict" {
					t.Fatalf("event header = %q", got)
				}
				mac := hmac.New(sha256.New, []byte("top-secret"))
				mac.Write(body)
				wantSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
				if got := r.Header.Get("X-SaveVault-Signature"); got != wantSig {
					t.Fatalf("signature = %q, want %q", got, wantSig)
				}
			},
		},
		{
			typ: webhookTypeDiscord,
			checkReq: func(t *testing.T, _ *http.Request, body []byte) {
				var payload map[string]any
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatal(err)
				}
				if _, ok := payload["embeds"]; !ok {
					t.Fatalf("discord missing embeds: %s", body)
				}
			},
		},
		{
			typ:    webhookTypeTelegram,
			secret: "-10012345",
			checkReq: func(t *testing.T, _ *http.Request, body []byte) {
				var payload map[string]any
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatal(err)
				}
				if payload["chat_id"] != "-10012345" {
					t.Fatalf("telegram chat_id = %v", payload["chat_id"])
				}
			},
		},
		{
			typ:    webhookTypeGotify,
			secret: "app-token",
			checkReq: func(t *testing.T, r *http.Request, body []byte) {
				if r.Header.Get("X-Gotify-Key") != "app-token" {
					t.Fatalf("gotify header = %q", r.Header.Get("X-Gotify-Key"))
				}
				if !strings.Contains(string(body), `"message"`) {
					t.Fatalf("gotify body = %s", body)
				}
			},
		},
		{
			typ: webhookTypeBark,
			checkReq: func(t *testing.T, _ *http.Request, body []byte) {
				if !strings.Contains(string(body), `"group":"SaveVault"`) {
					t.Fatalf("bark body = %s", body)
				}
			},
		},
		{
			typ: webhookTypeFeishu,
			checkReq: func(t *testing.T, _ *http.Request, body []byte) {
				if !strings.Contains(string(body), `"msg_type":"text"`) {
					t.Fatalf("feishu body = %s", body)
				}
			},
		},
		{
			typ: webhookTypeWeCom,
			checkReq: func(t *testing.T, _ *http.Request, body []byte) {
				if !strings.Contains(string(body), `"msgtype":"text"`) {
					t.Fatalf("wecom body = %s", body)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.typ, func(t *testing.T) {
			req, err := buildWebhookRequest(context.Background(), webhookChannel{
				ID:     "wh_1",
				Name:   tc.typ,
				Type:   tc.typ,
				URL:    "https://example.invalid/hook",
				Secret: tc.secret,
			}, notice)
			if err != nil {
				t.Fatalf("buildWebhookRequest: %v", err)
			}
			raw, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			tc.checkReq(t, req, raw)
		})
	}
}

func TestP1WebhooksAdminCRUDTestAndAsyncDelivery(t *testing.T) {
	var mu sync.Mutex
	received := make([]webhookNotification, 0)
	sigs := make([]string, 0)
	doneCh := make(chan struct{}, 4)

	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var notice webhookNotification
		_ = json.Unmarshal(raw, &notice)
		mu.Lock()
		received = append(received, notice)
		sigs = append(sigs, r.Header.Get("X-SaveVault-Signature"))
		mu.Unlock()
		select {
		case doneCh <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	srv, err := newServerWithOptions(t.TempDir(), "sync-token", defaultMaxObjectBytes, serverOptions{
		adminToken: "admin-secret",
		quotaBytes: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.webhookClient = receiver.Client()
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	adminReq := func(method, path string, body any) *http.Response {
		t.Helper()
		var reader io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			reader = bytes.NewReader(b)
		}
		req, err := http.NewRequest(method, ts.URL+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth("admin", "admin-secret")
		req.Header.Set("X-Game-Save-Admin", "1")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Create a webhook channel with secret.
	putResp := adminReq(http.MethodPut, "/admin/api/webhooks", map[string]any{
		"channels": []map[string]any{{
			"id":      "wh_ops",
			"name":    "Ops Alert",
			"type":    "generic",
			"url":     receiver.URL,
			"secret":  "hmac-key-123",
			"events":  []string{"conflict", "quota_exceeded"},
			"enabled": true,
		}},
	})
	defer putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(putResp.Body)
		t.Fatalf("PUT webhooks = %d: %s", putResp.StatusCode, raw)
	}

	// GET must redact secret and expose has_secret=true.
	getResp := adminReq(http.MethodGet, "/admin/api/webhooks", nil)
	defer getResp.Body.Close()
	var list struct {
		Channels []webhookChannelView `json:"channels"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Channels) != 1 || !list.Channels[0].HasSecret {
		t.Fatalf("unexpected channels: %+v", list.Channels)
	}

	// Updating without resending secret must preserve stored secret.
	putResp2 := adminReq(http.MethodPut, "/admin/api/webhooks", map[string]any{
		"channels": []map[string]any{{
			"id":      "wh_ops",
			"name":    "Ops Alert Renamed",
			"type":    "generic",
			"url":     receiver.URL,
			"events":  []string{"conflict", "quota_exceeded"},
			"enabled": true,
		}},
	})
	putResp2.Body.Close()
	if putResp2.StatusCode != http.StatusOK {
		t.Fatalf("second PUT webhooks = %d", putResp2.StatusCode)
	}

	// Sync test delivery should carry preserved HMAC signature.
	testResp := adminReq(http.MethodPost, "/admin/api/webhooks/test", map[string]any{"id": "wh_ops"})
	defer testResp.Body.Close()
	var testOut struct {
		OK         bool `json:"ok"`
		StatusCode int  `json:"status_code"`
	}
	if err := json.NewDecoder(testResp.Body).Decode(&testOut); err != nil {
		t.Fatal(err)
	}
	if !testOut.OK || testOut.StatusCode != http.StatusOK {
		t.Fatalf("webhook test failed: %+v", testOut)
	}

	// Trigger quota exceeded and verify async delivery arrives.
	if srv.checkQuota(200) {
		t.Fatal("expected checkQuota(200) to fail against 100-byte quota")
	}
	deadline := time.After(3 * time.Second)
	for {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-doneCh:
		case <-deadline:
			t.Fatalf("timed out waiting for async webhook delivery (got %d)", n)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if received[0].Event != "test" || !strings.HasPrefix(sigs[0], "sha256=") {
		t.Fatalf("first delivery unexpected: %+v sig=%q", received[0], sigs[0])
	}
	if received[1].Event != "quota_exceeded" || sigs[1] == "" {
		t.Fatalf("second delivery unexpected: %+v sig=%q", received[1], sigs[1])
	}
}

func TestP1ProfileMetadataMergeETagAndRemove(t *testing.T) {
	srv, err := newServerWithOptions(t.TempDir(), "sync-token", defaultMaxObjectBytes, serverOptions{
		adminToken: "admin-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	clientReq := func(method, path string, body any, ifNoneMatch string) *http.Response {
		t.Helper()
		var reader io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			reader = bytes.NewReader(b)
		}
		req, err := http.NewRequest(method, ts.URL+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer sync-token")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if ifNoneMatch != "" {
			req.Header.Set("If-None-Match", ifNoneMatch)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Device A writes a custom title and an excluded scope.
	r1 := clientReq(http.MethodPut, "/v1/savevault/profile/metadata", map[string]any{
		"custom_titles":   map[string]string{"switch:0100000000010000": "超级马力欧 奥德赛"},
		"excluded_scopes": []string{"android:com.example.temp"},
	}, "")
	defer r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("PUT 1 = %d", r1.StatusCode)
	}
	var m1 profileMetadataState
	if err := json.NewDecoder(r1.Body).Decode(&m1); err != nil {
		t.Fatal(err)
	}
	if m1.Revision != 1 || m1.CustomTitles["switch:0100000000010000"] != "超级马力欧 奥德赛" {
		t.Fatalf("unexpected m1: %+v", m1)
	}
	etag1 := r1.Header.Get("ETag")
	if etag1 != `"rev-1"` {
		t.Fatalf("etag1 = %q", etag1)
	}

	// Conditional GET with matching ETag returns 304.
	r304 := clientReq(http.MethodGet, "/v1/savevault/profile/metadata", nil, etag1)
	r304.Body.Close()
	if r304.StatusCode != http.StatusNotModified {
		t.Fatalf("expected 304, got %d", r304.StatusCode)
	}

	// Device B merges another title + removes old exclusion without wiping Device A's title.
	r2 := clientReq(http.MethodPut, "/v1/savevault/profile/metadata", map[string]any{
		"merge":           true,
		"custom_titles":   map[string]string{"3ds:0004000000055D00": "宝可梦 X"},
		"remove_excluded": []string{"android:com.example.temp"},
		"excluded_scopes": []string{"steam:7"},
	}, "")
	defer r2.Body.Close()
	var m2 profileMetadataState
	if err := json.NewDecoder(r2.Body).Decode(&m2); err != nil {
		t.Fatal(err)
	}
	if m2.Revision != 2 || len(m2.CustomTitles) != 2 || len(m2.ExcludedScopes) != 1 || m2.ExcludedScopes[0] != "steam:7" {
		t.Fatalf("unexpected merged metadata: %+v", m2)
	}

	// Remove title via remove_titles.
	r3 := clientReq(http.MethodPut, "/v1/savevault/profile/metadata", map[string]any{
		"remove_titles": []string{"3ds:0004000000055D00"},
	}, "")
	defer r3.Body.Close()
	var m3 profileMetadataState
	if err := json.NewDecoder(r3.Body).Decode(&m3); err != nil {
		t.Fatal(err)
	}
	if m3.Revision != 3 || len(m3.CustomTitles) != 1 {
		t.Fatalf("unexpected m3 after remove: %+v", m3)
	}
}
