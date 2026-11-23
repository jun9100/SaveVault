// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSecuritySVGLogoRejectedAndSandboxCSP(t *testing.T) {
	svg := "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(`<svg onload="alert(1)"/>`))
	if _, _, ok := decodeLogoData(svg); ok {
		t.Fatal("SVG data URI must be rejected")
	}
	png := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte{0x89, 0x50, 0x4e, 0x47})
	mediaType, decoded, ok := decodeLogoData(png)
	if !ok || mediaType != "image/png" || len(decoded) != 4 {
		t.Fatalf("valid PNG logo rejected: ok=%v mediaType=%q len=%d", ok, mediaType, len(decoded))
	}

	s, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()
	s.mu.Lock()
	s.dashboard.Scopes["scope-1"] = scopeMetadata{LogoData: png}
	s.mu.Unlock()

	req := adminRequest(t, http.MethodGet, httpServer.URL+"/admin/api/scopes/scope-1/logo", nil, testAdminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logo status = %d", resp.StatusCode)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Fatalf("expected sandbox CSP header on logo response, got %q", csp)
	}
}

func TestSecurityLogoutRequiresCSRFHeader(t *testing.T) {
	_, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()

	req, err := http.NewRequest(http.MethodPost, httpServer.URL+"/admin/api/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("logout without CSRF header = %d, want 403", resp.StatusCode)
	}

	req2, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/admin/api/logout", nil)
	req2.Header.Set(csrfHeader, "1")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("logout with CSRF header = %d, want 204", resp2.StatusCode)
	}
}

func TestSecurityUserLoginRateLimitedAndVaultIDCollisionPrevented(t *testing.T) {
	root := t.TempDir()
	registry, err := newVaultRegistry(root, "default-token", 8<<20, serverOptions{adminToken: "admin-token"})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	t.Cleanup(registry.Close)
	handler := registry.handler()

	if _, err := registry.CreateUser("a.b", "s3cret-pass", "", 0, time.Time{}); err != nil {
		t.Fatalf("create a.b: %v", err)
	}
	if _, err := registry.CreateUser("a-b", "s3cret-pass", "", 0, time.Time{}); err == nil {
		t.Fatal("colliding vault ID username a-b must be rejected")
	}
	if _, err := registry.CreateUser("default", "s3cret-pass", "", 0, time.Time{}); err == nil {
		t.Fatal("reserved default vault ID must be rejected")
	}

	// Admin user list must not leak password hashes.
	listReq := httptest.NewRequest(http.MethodGet, "/admin/api/users", nil)
	listReq.SetBasicAuth("admin", "admin-token")
	listRec := httptest.NewRecorder()
	handler.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list users status = %d", listRec.Code)
	}
	if bytes.Contains(listRec.Body.Bytes(), []byte("pbkdf2-sha256")) {
		t.Fatalf("admin user list leaked password hash: %s", listRec.Body.String())
	}

	// Unauthenticated X-Game-Save-Vault must never create new vault dirs.
	probeReq := httptest.NewRequest(http.MethodGet, "/admin/api/session", nil)
	probeReq.Header.Set("X-Game-Save-Vault", "attacker-vault")
	probeRec := httptest.NewRecorder()
	handler.ServeHTTP(probeRec, probeReq)
	if _, statErr := os.Stat(filepath.Join(root, "vaults", "attacker-vault")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unauthenticated X-Game-Save-Vault created vault directory: %v", statErr)
	}

	// Rate limit /v1/login after loginFailureLimit failures from same client IP.
	badBody, _ := json.Marshal(map[string]string{"username": "a.b", "password": "wrong-password"})
	for i := 0; i < loginFailureLimit; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/login", bytes.NewReader(badBody))
		req.RemoteAddr = "198.51.100.10:12345"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401", i+1, rec.Code)
		}
	}
	blockedReq := httptest.NewRequest(http.MethodPost, "/v1/login", bytes.NewReader(badBody))
	blockedReq.RemoteAddr = "198.51.100.10:12345"
	blockedRec := httptest.NewRecorder()
	handler.ServeHTTP(blockedRec, blockedReq)
	if blockedRec.Code != http.StatusTooManyRequests {
		t.Fatalf("blocked login status = %d, want 429", blockedRec.Code)
	}
}

func TestSecurityBarkErrorDoesNotLeakSecretPath(t *testing.T) {
	const secretKey = "SUPER_SECRET_BARK_KEY_XYZ"
	notifier, err := newBarkNotifier(barkConfig{
		Endpoint: "http://127.0.0.1:1/" + secretKey,
		Timeout:  200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newBarkNotifier: %v", err)
	}
	defer notifier.Close()

	sendErr := notifier.send(barkMessage{Title: "t", Body: "b", Key: "k"})
	if sendErr == nil {
		t.Fatal("expected send error to unreachable port")
	}
	if strings.Contains(sendErr.Error(), secretKey) {
		t.Fatalf("Bark error leaked secret key: %v", sendErr)
	}
}

func TestSecurityLoginLimiterBoundedMemory(t *testing.T) {
	limiter := loginLimiter{failures: make(map[string]loginFailures)}
	now := time.Now()
	for i := 0; i < maxLoginFailureEntries+50; i++ {
		limiter.fail("10.0.0."+itoa(i), now.Add(time.Duration(i)*time.Millisecond))
	}
	if len(limiter.failures) > maxLoginFailureEntries {
		t.Fatalf("loginLimiter grew beyond bound: %d > %d", len(limiter.failures), maxLoginFailureEntries)
	}
}
