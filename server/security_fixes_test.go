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

func TestLegacyOpenSaveCompat(t *testing.T) {
	dataDir := t.TempDir()

	// Seed pre-rebrand on-disk opensave* files and directories.
	if err := os.MkdirAll(filepath.Join(dataDir, "opensave-native", "pc"), 0o700); err != nil {
		t.Fatal(err)
	}
	archiveName := "game1__main__snap1.zip"
	if err := os.WriteFile(filepath.Join(dataDir, "opensave-native", "pc", archiveName), []byte("zip-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "opensave-native.json"), []byte(`{"files":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "opensave-native-state.json"), []byte(`{"scopes":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "opensave-manage.json"), []byte(`{"version":1,"retention":{"default":{"keepVersions":20}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "opensave-meta.json"), []byte(`{"version":1,"games":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "opensave"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "opensave", "legacy.zip"), []byte("webdav-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := newServerWithOptions(dataDir, testToken, 8<<20, serverOptions{
		adminToken:        testAdminToken,
		saveVaultUsername: testSaveVaultUsername,
		saveVaultPassword: testSaveVaultPassword,
	})
	if err != nil {
		t.Fatalf("newServerWithOptions: %v", err)
	}
	t.Cleanup(s.Close)

	// Verify on-disk migration occurred.
	for _, migrated := range []string{
		filepath.Join("savevault-native", "pc", archiveName),
		"savevault-native.json",
		"savevault-native-state.json",
		"savevault-manage.json",
		"savevault-meta.json",
		filepath.Join("savevault", "legacy.zip"),
	} {
		if _, err := os.Stat(filepath.Join(dataDir, migrated)); err != nil {
			t.Fatalf("expected migrated path %s: %v", migrated, err)
		}
	}

	handler := s.handler()

	// Legacy /v1/opensave/files route works and sees the migrated archive.
	req := httptest.NewRequest(http.MethodGet, "/v1/opensave/files", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), archiveName) {
		t.Fatalf("GET /v1/opensave/files = %d %s", rec.Code, rec.Body.String())
	}

	// Legacy X-OpenSave-* headers populate device tracking on writes.
	putReq := httptest.NewRequest(http.MethodPut, "/v1/opensave/files/pc/game2__main__snap2.zip", bytes.NewReader([]byte("zip2")))
	putReq.Header.Set("Authorization", "Bearer "+testToken)
	putReq.Header.Set("X-OpenSave-Device-ID", "legacy-dev-1")
	putReq.Header.Set("X-OpenSave-Device-Name", "Legacy Steam Deck")
	putRec := httptest.NewRecorder()
	handler.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusCreated && putRec.Code != http.StatusOK {
		t.Fatalf("PUT /v1/opensave/files/... = %d %s", putRec.Code, putRec.Body.String())
	}
	s.mu.Lock()
	dev, ok := s.dashboard.Devices["legacy-dev-1"]
	s.mu.Unlock()
	if !ok || dev.Name != "Legacy Steam Deck" {
		t.Fatalf("expected legacy X-OpenSave device headers recorded, got ok=%v dev=%+v", ok, dev)
	}

	// Legacy /admin/api/opensave/files route works.
	adminReq := httptest.NewRequest(http.MethodGet, "/admin/api/opensave/files", nil)
	adminReq.SetBasicAuth("admin", testAdminToken)
	adminRec := httptest.NewRecorder()
	handler.ServeHTTP(adminRec, adminReq)
	if adminRec.Code != http.StatusOK || !strings.Contains(adminRec.Body.String(), archiveName) {
		t.Fatalf("GET /admin/api/opensave/files = %d %s", adminRec.Code, adminRec.Body.String())
	}

	// Legacy /opensave/ WebDAV endpoint lists the migrated file with /opensave/ hrefs.
	davReq := httptest.NewRequest("PROPFIND", "/opensave/", nil)
	davReq.SetBasicAuth(testSaveVaultUsername, testSaveVaultPassword)
	davReq.Header.Set("Depth", "1")
	davRec := httptest.NewRecorder()
	handler.ServeHTTP(davRec, davReq)
	if davRec.Code != http.StatusMultiStatus || !strings.Contains(davRec.Body.String(), "/opensave/legacy.zip") {
		t.Fatalf("PROPFIND /opensave/ = %d %s", davRec.Code, davRec.Body.String())
	}

	// Legacy event scope names remain parsable.
	if name, ok := saveVaultEventScopeName("opensave:pc/game1"); !ok || !strings.Contains(name, "game1") {
		t.Fatalf("saveVaultEventScopeName legacy = %q, %v", name, ok)
	}
}
