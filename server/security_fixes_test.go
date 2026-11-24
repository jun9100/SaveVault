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
	// Legacy Bark opensave_meta alias remains accepted.
	if events, err := parseBarkEvents("conflict,opensave_meta"); err != nil {
		t.Fatalf("parseBarkEvents(opensave_meta): %v", err)
	} else if _, ok := events[eventSaveVaultMeta]; !ok {
		t.Fatalf("expected opensave_meta mapped to %q, got %v", eventSaveVaultMeta, events)
	}
}

func TestMultiTenantDeviceTokensPairingQuotaExpiryRecreateAndRateLimit(t *testing.T) {
	root := t.TempDir()
	registry, err := newVaultRegistry(root, "default-token", 8<<20, serverOptions{adminToken: "admin-secret"})
	if err != nil {
		t.Fatalf("newVaultRegistry: %v", err)
	}
	t.Cleanup(registry.Close)
	handler := registry.handler()

	// Negative quota rejected.
	if _, err := registry.CreateUser("negquota", "s3cret-pass", "", -1, time.Time{}); err == nil {
		t.Fatal("negative quota must be rejected")
	}

	// 1. Delete + recreate user works immediately (no stale handler cache 401).
	alice1, err := registry.CreateUser("alice", "s3cret-pass", "", 64, time.Time{})
	if err != nil {
		t.Fatalf("create alice1: %v", err)
	}
	req1 := httptest.NewRequest(http.MethodGet, "/v1/auth/whoami", nil)
	req1.Header.Set("Authorization", "Bearer "+alice1.Token)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("alice1 whoami = %d", rec1.Code)
	}
	if err := registry.DeleteUser("alice"); err != nil {
		t.Fatalf("delete alice1: %v", err)
	}
	// Old token must fail now.
	reqOld := httptest.NewRequest(http.MethodGet, "/v1/auth/whoami", nil)
	reqOld.Header.Set("Authorization", "Bearer "+alice1.Token)
	recOld := httptest.NewRecorder()
	handler.ServeHTTP(recOld, reqOld)
	if recOld.Code != http.StatusUnauthorized {
		t.Fatalf("deleted user token status = %d, want 401", recOld.Code)
	}
	alice2, err := registry.CreateUser("alice", "s3cret-pass", "", 64, time.Time{})
	if err != nil {
		t.Fatalf("recreate alice2: %v", err)
	}
	req2 := httptest.NewRequest(http.MethodGet, "/v1/auth/whoami", nil)
	req2.Header.Set("Authorization", "Bearer "+alice2.Token)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("recreated alice2 whoami = %d, want 200 (stale handler cache bug)", rec2.Code)
	}

	// 2. Quota enforcement on SaveVault native upload.
	smallPut := httptest.NewRequest(http.MethodPut, "/v1/savevault/files/pc/g1__main__snap_1.zip", bytes.NewReader(bytes.Repeat([]byte("a"), 32)))
	smallPut.Header.Set("Authorization", "Bearer "+alice2.Token)
	smallRec := httptest.NewRecorder()
	handler.ServeHTTP(smallRec, smallPut)
	if smallRec.Code != http.StatusCreated {
		t.Fatalf("within-quota PUT = %d %s", smallRec.Code, smallRec.Body.String())
	}
	overPut := httptest.NewRequest(http.MethodPut, "/v1/savevault/files/pc/g2__main__snap_2.zip", bytes.NewReader(bytes.Repeat([]byte("b"), 40)))
	overPut.Header.Set("Authorization", "Bearer "+alice2.Token)
	overRec := httptest.NewRecorder()
	handler.ServeHTTP(overRec, overPut)
	if overRec.Code != http.StatusInsufficientStorage {
		t.Fatalf("over-quota PUT = %d, want 507 (%s)", overRec.Code, overRec.Body.String())
	}

	// 3. Tenant pairing claim and tenant gsdt_* device token routing.
	pairCreate := httptest.NewRequest(http.MethodPost, "/admin/api/pairings", strings.NewReader(`{"name":"deck"}`))
	pairCreate.SetBasicAuth("admin", "admin-secret")
	pairCreate.Header.Set(csrfHeader, "1")
	pairCreate.Header.Set("X-Game-Save-Vault", "alice")
	pairCreateRec := httptest.NewRecorder()
	handler.ServeHTTP(pairCreateRec, pairCreate)
	if pairCreateRec.Code != http.StatusCreated {
		t.Fatalf("create tenant pairing = %d %s", pairCreateRec.Code, pairCreateRec.Body.String())
	}
	var createdPairing struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(pairCreateRec.Body.Bytes(), &createdPairing); err != nil {
		t.Fatal(err)
	}
	claimBody, _ := json.Marshal(map[string]string{
		"code":      createdPairing.Code,
		"device_id": "alice-deck-1",
	})
	claimReq := httptest.NewRequest(http.MethodPost, "/v1/pair/claim", bytes.NewReader(claimBody))
	claimRec := httptest.NewRecorder()
	handler.ServeHTTP(claimRec, claimReq)
	if claimRec.Code != http.StatusOK {
		t.Fatalf("tenant pair claim = %d %s", claimRec.Code, claimRec.Body.String())
	}
	var claimed struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(claimRec.Body.Bytes(), &claimed); err != nil || !strings.HasPrefix(claimed.Token, deviceTokenPrefix) {
		t.Fatalf("unexpected claimed payload: %s", claimRec.Body.String())
	}
	devWhoami := httptest.NewRequest(http.MethodGet, "/v1/savevault/files", nil)
	devWhoami.Header.Set("Authorization", "Bearer "+claimed.Token)
	devRec := httptest.NewRecorder()
	handler.ServeHTTP(devRec, devWhoami)
	if devRec.Code != http.StatusOK || !strings.Contains(devRec.Body.String(), "g1__main__snap_1.zip") {
		t.Fatalf("tenant gsdt token GET /v1/savevault/files = %d %s", devRec.Code, devRec.Body.String())
	}

	// 4. Expired user bearer token is rejected even on non-login endpoints.
	expiredUser, err := registry.CreateUser("expired", "s3cret-pass", "", 0, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("create expired user: %v", err)
	}
	expReq := httptest.NewRequest(http.MethodGet, "/v1/auth/whoami", nil)
	expReq.Header.Set("Authorization", "Bearer "+expiredUser.Token)
	expRec := httptest.NewRecorder()
	handler.ServeHTTP(expRec, expReq)
	if expRec.Code != http.StatusUnauthorized {
		t.Fatalf("expired user bearer token status = %d, want 401", expRec.Code)
	}

	// 5. Unknown X-Game-Save-Vault returns 404 (not 500) for authenticated admin.
	unkReq := httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)
	unkReq.SetBasicAuth("admin", "admin-secret")
	unkReq.Header.Set("X-Game-Save-Vault", "does-not-exist")
	unkRec := httptest.NewRecorder()
	handler.ServeHTTP(unkRec, unkReq)
	if unkRec.Code != http.StatusNotFound {
		t.Fatalf("unknown admin vault = %d, want 404", unkRec.Code)
	}

	// 6. Admin Basic Auth brute-force is rate limited (429).
	for i := 0; i < loginFailureLimit; i++ {
		bad := httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)
		bad.RemoteAddr = "203.0.113.55:4444"
		bad.SetBasicAuth("admin", "wrong-token")
		badRec := httptest.NewRecorder()
		handler.ServeHTTP(badRec, bad)
		if badRec.Code != http.StatusUnauthorized {
			t.Fatalf("basic auth attempt %d = %d, want 401", i+1, badRec.Code)
		}
	}
	blocked := httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)
	blocked.RemoteAddr = "203.0.113.55:4444"
	blocked.SetBasicAuth("admin", "admin-secret")
	blockedRec := httptest.NewRecorder()
	handler.ServeHTTP(blockedRec, blocked)
	if blockedRec.Code != http.StatusTooManyRequests {
		t.Fatalf("blocked admin basic auth = %d, want 429", blockedRec.Code)
	}

	// 7. Leading-dot and traversal identifiers are rejected.
	for _, badID := range []string{".", "..", ".upload-abc"} {
		if validIdentifier(badID, 128) {
			t.Fatalf("validIdentifier(%q) must be false", badID)
		}
	}

	// 8. Admin token == default token disables admin across all vaults too.
	disabledReg, err := newVaultRegistry(t.TempDir(), "same-token", 8<<20, serverOptions{adminToken: "same-token"})
	if err != nil {
		t.Fatalf("disabledReg: %v", err)
	}
	t.Cleanup(disabledReg.Close)
	u, err := disabledReg.CreateUser("tenant1", "s3cret-pass", "", 0, time.Time{})
	if err != nil {
		t.Fatalf("create tenant1: %v", err)
	}
	adminProbe := httptest.NewRequest(http.MethodGet, "/admin/api/users", nil)
	adminProbe.Header.Set("Authorization", "Bearer "+u.Token)
	adminProbe.SetBasicAuth("admin", "same-token")
	adminProbeRec := httptest.NewRecorder()
	disabledReg.handler().ServeHTTP(adminProbeRec, adminProbe)
	if adminProbeRec.Code != http.StatusNotFound {
		t.Fatalf("disabled admin via tenant bearer token = %d, want 404", adminProbeRec.Code)
	}
}

func TestRound3SecurityAndQuotaRegressions(t *testing.T) {
	registry, err := newVaultRegistry(t.TempDir(), "default-token", 8<<20, serverOptions{adminToken: "admin-secret"})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	t.Cleanup(registry.Close)
	handler := registry.handler()

	// 1. Uploads > 50% of quota must succeed (in-flight .upload-* temp file is not double-counted).
	qUser, err := registry.CreateUser("quotauser", "s3cret-pass", "", 64, time.Time{})
	if err != nil {
		t.Fatalf("create quotauser: %v", err)
	}
	bigWithinQuota := httptest.NewRequest(http.MethodPut, "/v1/savevault/files/pc/g1__main__snap_1.zip", bytes.NewReader(bytes.Repeat([]byte("z"), 48)))
	bigWithinQuota.Header.Set("Authorization", "Bearer "+qUser.Token)
	bigRec := httptest.NewRecorder()
	handler.ServeHTTP(bigRec, bigWithinQuota)
	if bigRec.Code != http.StatusCreated {
		t.Fatalf("48B upload into 64B quota = %d, want 201 (%s)", bigRec.Code, bigRec.Body.String())
	}

	// 2. Scope metadata endpoint enforces quotaBytes.
	metaBody := `{"title":"` + strings.Repeat("x", 100) + `"}`
	metaReq := httptest.NewRequest(http.MethodPut, "/v1/scopes/synth-1/metadata", strings.NewReader(metaBody))
	metaReq.Header.Set("Authorization", "Bearer "+qUser.Token)
	metaRec := httptest.NewRecorder()
	handler.ServeHTTP(metaRec, metaReq)
	if metaRec.Code != http.StatusInsufficientStorage {
		t.Fatalf("over-quota scope metadata = %d, want 507 (%s)", metaRec.Code, metaRec.Body.String())
	}

	// 3. Pairing claim rate limit cannot be bypassed via tenant vault.
	tenant, err := registry.CreateUser("bob", "s3cret-pass", "", 0, time.Time{})
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	pairCreate := httptest.NewRequest(http.MethodPost, "/admin/api/pairings", strings.NewReader(`{"name":"handheld"}`))
	pairCreate.SetBasicAuth("admin", "admin-secret")
	pairCreate.Header.Set(csrfHeader, "1")
	pairCreate.Header.Set("X-Game-Save-Vault", tenant.VaultID)
	pairRec := httptest.NewRecorder()
	handler.ServeHTTP(pairRec, pairCreate)
	if pairRec.Code != http.StatusCreated {
		t.Fatalf("create bob pairing = %d %s", pairRec.Code, pairRec.Body.String())
	}
	var createdPairing struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(pairRec.Body.Bytes(), &createdPairing); err != nil {
		t.Fatal(err)
	}
	attackerIP := "198.51.100.77:5555"
	for i := 0; i < loginFailureLimit; i++ {
		badClaim := httptest.NewRequest(http.MethodPost, "/v1/pair/claim", strings.NewReader(`{"code":"AAAAAAAA","device_id":"probe"}`))
		badClaim.RemoteAddr = attackerIP
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, badClaim)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("bad pair claim %d = %d, want 401", i+1, rec.Code)
		}
	}
	validClaimBody, _ := json.Marshal(map[string]string{"code": createdPairing.Code, "device_id": "probe"})
	blockedClaim := httptest.NewRequest(http.MethodPost, "/v1/pair/claim", bytes.NewReader(validClaimBody))
	blockedClaim.RemoteAddr = attackerIP
	blockedRec := httptest.NewRecorder()
	handler.ServeHTTP(blockedRec, blockedClaim)
	if blockedRec.Code != http.StatusTooManyRequests {
		t.Fatalf("blocked IP claiming valid tenant pairing code = %d, want 429", blockedRec.Code)
	}

	// 4. Claim from clean IP, then DeleteUser revokes the gsdt_* token permanently even if user is recreated.
	cleanClaim := httptest.NewRequest(http.MethodPost, "/v1/pair/claim", bytes.NewReader(validClaimBody))
	cleanClaim.RemoteAddr = "198.51.100.88:5555"
	cleanRec := httptest.NewRecorder()
	handler.ServeHTTP(cleanRec, cleanClaim)
	if cleanRec.Code != http.StatusOK {
		t.Fatalf("clean IP pair claim = %d %s", cleanRec.Code, cleanRec.Body.String())
	}
	var claimed struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(cleanRec.Body.Bytes(), &claimed); err != nil {
		t.Fatal(err)
	}
	if err := registry.DeleteUser("bob"); err != nil {
		t.Fatalf("delete bob: %v", err)
	}
	if _, err := registry.CreateUser("bob", "new-s3cret", "", 0, time.Time{}); err != nil {
		t.Fatalf("recreate bob: %v", err)
	}
	oldDevReq := httptest.NewRequest(http.MethodGet, "/v1/auth/whoami", nil)
	oldDevReq.Header.Set("Authorization", "Bearer "+claimed.Token)
	oldDevRec := httptest.NewRecorder()
	handler.ServeHTTP(oldDevRec, oldDevReq)
	if oldDevRec.Code != http.StatusUnauthorized {
		t.Fatalf("deleted tenant gsdt_* token after recreate = %d, want 401", oldDevRec.Code)
	}

	// 5. Wrong password on expired user returns "invalid credentials" (no status leak).
	if _, err := registry.CreateUser("expuser", "s3cret-pass", "", 0, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("create expuser: %v", err)
	}
	if _, err := registry.Authenticate("expuser", "wrong-pass"); err == nil || err.Error() != "invalid credentials" {
		t.Fatalf("wrong password on expired user = %v, want invalid credentials", err)
	}
	// 6. WebDAV rejects leading-dot filenames.
	if validSaveVaultFileName(".upload-123.part") {
		t.Fatal("validSaveVaultFileName must reject leading-dot names")
	}
}
