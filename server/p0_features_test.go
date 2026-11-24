// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTrustedProxyClientIPAndSecure(t *testing.T) {
	proxies, err := parseTrustedProxies("127.0.0.1/32, 10.0.0.0/8")
	if err != nil {
		t.Fatalf("parseTrustedProxies: %v", err)
	}

	// Untrusted direct peer cannot spoof X-Forwarded-For or X-Forwarded-Proto.
	untrusted := httptest.NewRequest(http.MethodGet, "/", nil)
	untrusted.RemoteAddr = "198.51.100.25:44321"
	untrusted.Header.Set("X-Forwarded-For", "203.0.113.9")
	untrusted.Header.Set("X-Forwarded-Proto", "https")
	if got := proxies.clientIP(untrusted); got != "198.51.100.25" {
		t.Fatalf("untrusted peer IP = %q, want 198.51.100.25", got)
	}
	if proxies.isSecure(untrusted) {
		t.Fatalf("untrusted peer must not spoof X-Forwarded-Proto")
	}

	// Trusted proxy chain walks right-to-left skipping trusted hops.
	trusted := httptest.NewRequest(http.MethodGet, "/", nil)
	trusted.RemoteAddr = "127.0.0.1:54321"
	trusted.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.77, 10.1.2.3")
	trusted.Header.Set("X-Forwarded-Proto", "https")
	if got := proxies.clientIP(trusted); got != "203.0.113.77" {
		t.Fatalf("trusted proxy clientIP = %q, want 203.0.113.77", got)
	}
	if !proxies.isSecure(trusted) {
		t.Fatalf("trusted proxy should honor X-Forwarded-Proto: https")
	}

	// Invalid CIDR rejected.
	if _, err := parseTrustedProxies("not-an-ip"); err == nil {
		t.Fatalf("expected error on invalid proxy CIDR")
	}
}

func TestRevocableAdminSessions(t *testing.T) {
	reg, err := newVaultRegistry(t.TempDir(), "sync-tok", defaultMaxObjectBytes, serverOptions{
		adminToken: "admin-tok",
	})
	if err != nil {
		t.Fatalf("newVaultRegistry: %v", err)
	}
	defer reg.Close()
	h := reg.handler()

	// Admin login yields revocable session cookie.
	loginReq := httptest.NewRequest(http.MethodPost, "/admin/api/login", strings.NewReader(`{"token":"admin-tok"}`))
	loginReq.Header.Set(csrfHeader, "1")
	loginRec := httptest.NewRecorder()
	h.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("admin login = %d %s", loginRec.Code, loginRec.Body.String())
	}
	var adminCookie *http.Cookie
	for _, c := range loginRec.Result().Cookies() {
		if c.Name == sessionCookie {
			adminCookie = c
		}
	}
	if adminCookie == nil || !strings.HasPrefix(adminCookie.Value, "v2.") {
		t.Fatalf("expected v2 session cookie, got %+v", adminCookie)
	}

	// Cookie works before logout/revoke.
	sessReq := httptest.NewRequest(http.MethodGet, "/admin/api/session", nil)
	sessReq.AddCookie(adminCookie)
	sessRec := httptest.NewRecorder()
	h.ServeHTTP(sessRec, sessReq)
	if !strings.Contains(sessRec.Body.String(), `"authenticated":true`) {
		t.Fatalf("expected authenticated session, got %s", sessRec.Body.String())
	}

	// Revoke-all invalidates the cookie immediately.
	revReq := httptest.NewRequest(http.MethodPost, "/admin/api/sessions/revoke-all", nil)
	revReq.Header.Set(csrfHeader, "1")
	revReq.AddCookie(adminCookie)
	revRec := httptest.NewRecorder()
	h.ServeHTTP(revRec, revReq)
	if revRec.Code != http.StatusOK {
		t.Fatalf("revoke-all = %d %s", revRec.Code, revRec.Body.String())
	}

	afterReq := httptest.NewRequest(http.MethodGet, "/admin/api/session", nil)
	afterReq.AddCookie(adminCookie)
	afterRec := httptest.NewRecorder()
	h.ServeHTTP(afterRec, afterReq)
	if !strings.Contains(afterRec.Body.String(), `"authenticated":false`) {
		t.Fatalf("cookie should be invalidated after revoke-all, got %s", afterRec.Body.String())
	}
}

func TestUserPatchPurgeAndVaultList(t *testing.T) {
	root := t.TempDir()
	reg, err := newVaultRegistry(root, "sync-tok", defaultMaxObjectBytes, serverOptions{
		adminToken: "admin-tok",
	})
	if err != nil {
		t.Fatalf("newVaultRegistry: %v", err)
	}
	defer reg.Close()
	h := reg.handler()

	adminDo := func(method, path string, body any) *httptest.ResponseRecorder {
		var rdr *bytes.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			rdr = bytes.NewReader(raw)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, rdr)
		req.SetBasicAuth("admin", "admin-tok")
		if method != http.MethodGet {
			req.Header.Set(csrfHeader, "1")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// Create tenant user alice.
	createRec := adminDo(http.MethodPost, "/admin/api/users", map[string]any{
		"username":   "alice",
		"password":   "initialpass1",
		"quotaBytes": 1024,
	})
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create user = %d %s", createRec.Code, createRec.Body.String())
	}
	var created userRecord
	_ = json.Unmarshal(createRec.Body.Bytes(), &created)

	// Vault list includes default and alice.
	vaultsRec := adminDo(http.MethodGet, "/admin/api/vaults", nil)
	if vaultsRec.Code != http.StatusOK || !strings.Contains(vaultsRec.Body.String(), `"alice"`) || !strings.Contains(vaultsRec.Body.String(), `"default"`) {
		t.Fatalf("vault list = %d %s", vaultsRec.Code, vaultsRec.Body.String())
	}

	// PATCH user: raise quota, rotate token, set note, set expiry.
	exp := time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)
	patchRec := adminDo(http.MethodPatch, "/admin/api/users/alice", map[string]any{
		"note":        "VIP player",
		"quotaBytes":  8192,
		"expiresAt":   exp,
		"rotateToken": true,
	})
	if patchRec.Code != http.StatusOK {
		t.Fatalf("patch user = %d %s", patchRec.Code, patchRec.Body.String())
	}
	var patched userListItem
	_ = json.Unmarshal(patchRec.Body.Bytes(), &patched)
	if patched.Note != "VIP player" || patched.QuotaBytes != 8192 || patched.Token == "" || patched.Token == created.Token {
		t.Fatalf("unexpected patched user: %+v", patched)
	}

	// Old token must fail, rotated token must succeed and report updated quota.
	oldReq := httptest.NewRequest(http.MethodGet, "/v1/auth/whoami", nil)
	oldReq.Header.Set("Authorization", "Bearer "+created.Token)
	oldRec := httptest.NewRecorder()
	h.ServeHTTP(oldRec, oldReq)
	if oldRec.Code != http.StatusUnauthorized {
		t.Fatalf("old rotated token should be 401, got %d", oldRec.Code)
	}

	newReq := httptest.NewRequest(http.MethodGet, "/v1/auth/whoami", nil)
	newReq.Header.Set("Authorization", "Bearer "+patched.Token)
	newRec := httptest.NewRecorder()
	h.ServeHTTP(newRec, newReq)
	if newRec.Code != http.StatusOK {
		t.Fatalf("new rotated token whoami = %d %s", newRec.Code, newRec.Body.String())
	}
	var who map[string]any
	_ = json.Unmarshal(newRec.Body.Bytes(), &who)
	if who["username"] != "alice" || int64(who["quota_bytes"].(float64)) != 8192 {
		t.Fatalf("whoami missing quota/username: %+v", who)
	}

	// Disable and verify access blocked, then re-enable.
	disRec := adminDo(http.MethodPatch, "/admin/api/users/alice", map[string]any{"status": "disabled"})
	if disRec.Code != http.StatusOK {
		t.Fatalf("disable user = %d %s", disRec.Code, disRec.Body.String())
	}
	blockedRec := httptest.NewRecorder()
	h.ServeHTTP(blockedRec, newReq)
	if blockedRec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled user should get 401, got %d", blockedRec.Code)
	}

	// Delete with purgeData=true removes tenant dir on disk.
	vaultDir := filepath.Join(root, "vaults", created.VaultID)
	if _, err := os.Stat(vaultDir); err != nil {
		t.Fatalf("expected vault dir to exist before purge: %v", err)
	}
	delRec := adminDo(http.MethodDelete, "/admin/api/users/alice?purgeData=true", nil)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("delete purge = %d %s", delRec.Code, delRec.Body.String())
	}
	if _, err := os.Stat(vaultDir); !os.IsNotExist(err) {
		t.Fatalf("expected vault dir purged, stat err=%v", err)
	}
}

func TestTenantSelfPortalAndClientDevicesAndSnapshotLockDelete(t *testing.T) {
	reg, err := newVaultRegistry(t.TempDir(), "sync-tok", defaultMaxObjectBytes, serverOptions{
		adminToken: "admin-tok",
	})
	if err != nil {
		t.Fatalf("newVaultRegistry: %v", err)
	}
	defer reg.Close()
	h := reg.handler()

	bob, err := reg.CreateUser("bob", "bobsecret123", "Steam Deck", 10*1024*1024, time.Time{})
	if err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}

	// 1. Tenant logs into /v1/self/login
	loginReq := httptest.NewRequest(http.MethodPost, "/v1/self/login", strings.NewReader(`{"username":"bob","password":"bobsecret123"}`))
	loginReq.Header.Set(selfCSRFHeader, "1")
	loginRec := httptest.NewRecorder()
	h.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("self login = %d %s", loginRec.Code, loginRec.Body.String())
	}
	var tenantCookie *http.Cookie
	for _, c := range loginRec.Result().Cookies() {
		if c.Name == tenantSessionCookie {
			tenantCookie = c
		}
	}
	if tenantCookie == nil {
		t.Fatalf("expected tenant session cookie")
	}

	// 2. Tenant creates a pairing code via /v1/self/pairings and claims a device token
	pairReq := httptest.NewRequest(http.MethodPost, "/v1/self/pairings", strings.NewReader(`{"name":"Odin2"}`))
	pairReq.Header.Set(selfCSRFHeader, "1")
	pairReq.AddCookie(tenantCookie)
	pairRec := httptest.NewRecorder()
	h.ServeHTTP(pairRec, pairReq)
	if pairRec.Code != http.StatusCreated {
		t.Fatalf("self pairing = %d %s", pairRec.Code, pairRec.Body.String())
	}
	var pairResp struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(pairRec.Body.Bytes(), &pairResp)

	claimBody, _ := json.Marshal(map[string]any{
		"code":        pairResp.Code,
		"device_id":   "odin2-1",
		"device_name": "Odin 2",
		"platform":    "savevault-android",
	})
	claimReq := httptest.NewRequest(http.MethodPost, "/v1/pair/claim", bytes.NewReader(claimBody))
	claimRec := httptest.NewRecorder()
	h.ServeHTTP(claimRec, claimReq)
	if claimRec.Code != http.StatusOK {
		t.Fatalf("claim = %d %s", claimRec.Code, claimRec.Body.String())
	}
	var claimed struct {
		Token   string `json:"token"`
		TokenID string `json:"token_id"`
	}
	_ = json.Unmarshal(claimRec.Body.Bytes(), &claimed)

	// 3. Upload two snapshots to bob's vault using bob's device token
	uploadSnap := func(snapID, archive, parent string, payload []byte) {
		putFile := httptest.NewRequest(http.MethodPut, "/v1/savevault/files/switch/"+archive, bytes.NewReader(payload))
		putFile.Header.Set("Authorization", "Bearer "+claimed.Token)
		putFileRec := httptest.NewRecorder()
		h.ServeHTTP(putFileRec, putFile)
		if putFileRec.Code != http.StatusCreated && putFileRec.Code != http.StatusOK {
			t.Fatalf("upload %s = %d %s", archive, putFileRec.Code, putFileRec.Body.String())
		}
		sum := sha256.Sum256(payload)
		meta, _ := json.Marshal(map[string]any{
			"id":            snapID,
			"archive":       archive,
			"parent":        parent,
			"contentDigest": hex.EncodeToString(sum[:]),
			"createdAt":     time.Now().UTC().Format(time.RFC3339),
		})
		putSnap := httptest.NewRequest(http.MethodPut, "/v1/savevault/scopes/switch/0100000000010000/snapshots/"+snapID, bytes.NewReader(meta))
		putSnap.Header.Set("Authorization", "Bearer "+claimed.Token)
		putSnap.Header.Set("If-Match", `"`+parent+`"`)
		putSnapRec := httptest.NewRecorder()
		h.ServeHTTP(putSnapRec, putSnap)
		if putSnapRec.Code != http.StatusCreated && putSnapRec.Code != http.StatusOK {
			t.Fatalf("put snapshot %s = %d %s", snapID, putSnapRec.Code, putSnapRec.Body.String())
		}
	}
	uploadSnap("snap_1--dev-a", "0100000000010000__main__snap_1--dev-a.zip", "", []byte("PK\x03\x04v1"))
	uploadSnap("snap_2--dev-a", "0100000000010000__main__snap_2--dev-a.zip", "snap_1--dev-a", []byte("PK\x03\x04v2"))

	// 4. Client locks snap_1, cannot delete locked snap_1 or head snap_2, unlocks snap_1, then deletes snap_1
	lockReq := httptest.NewRequest(http.MethodPut, "/v1/savevault/scopes/switch/0100000000010000/snapshots/snap_1--dev-a/lock", strings.NewReader(`{"note":"before boss"}`))
	lockReq.Header.Set("Authorization", "Bearer "+claimed.Token)
	lockRec := httptest.NewRecorder()
	h.ServeHTTP(lockRec, lockReq)
	if lockRec.Code != http.StatusOK {
		t.Fatalf("client lock = %d %s", lockRec.Code, lockRec.Body.String())
	}

	delLocked := httptest.NewRequest(http.MethodDelete, "/v1/savevault/scopes/switch/0100000000010000/snapshots/snap_1--dev-a", nil)
	delLocked.Header.Set("Authorization", "Bearer "+claimed.Token)
	delLockedRec := httptest.NewRecorder()
	h.ServeHTTP(delLockedRec, delLocked)
	if delLockedRec.Code != http.StatusConflict {
		t.Fatalf("deleting locked snapshot should be 409, got %d", delLockedRec.Code)
	}

	delHead := httptest.NewRequest(http.MethodDelete, "/v1/savevault/scopes/switch/0100000000010000/snapshots/snap_2--dev-a", nil)
	delHead.Header.Set("Authorization", "Bearer "+claimed.Token)
	delHeadRec := httptest.NewRecorder()
	h.ServeHTTP(delHeadRec, delHead)
	if delHeadRec.Code != http.StatusConflict {
		t.Fatalf("deleting head snapshot should be 409, got %d", delHeadRec.Code)
	}

	unlockReq := httptest.NewRequest(http.MethodDelete, "/v1/savevault/scopes/switch/0100000000010000/snapshots/snap_1--dev-a/lock", nil)
	unlockReq.Header.Set("Authorization", "Bearer "+claimed.Token)
	unlockRec := httptest.NewRecorder()
	h.ServeHTTP(unlockRec, unlockReq)
	if unlockRec.Code != http.StatusNoContent && unlockRec.Code != http.StatusOK {
		t.Fatalf("client unlock = %d %s", unlockRec.Code, unlockRec.Body.String())
	}

	delOK := httptest.NewRequest(http.MethodDelete, "/v1/savevault/scopes/switch/0100000000010000/snapshots/snap_1--dev-a", nil)
	delOK.Header.Set("Authorization", "Bearer "+claimed.Token)
	delOKRec := httptest.NewRecorder()
	h.ServeHTTP(delOKRec, delOK)
	if delOKRec.Code != http.StatusOK {
		t.Fatalf("deleting unlocked non-head snapshot = %d %s", delOKRec.Code, delOKRec.Body.String())
	}

	// 5. Client lists devices via /v1/auth/devices and revokes its own device token
	devListReq := httptest.NewRequest(http.MethodGet, "/v1/auth/devices", nil)
	devListReq.Header.Set("Authorization", "Bearer "+bob.Token)
	devListRec := httptest.NewRecorder()
	h.ServeHTTP(devListRec, devListReq)
	if devListRec.Code != http.StatusOK || !strings.Contains(devListRec.Body.String(), claimed.TokenID) {
		t.Fatalf("client device list = %d %s", devListRec.Code, devListRec.Body.String())
	}

	revDevReq := httptest.NewRequest(http.MethodPost, "/v1/auth/devices/"+claimed.TokenID+"/revoke", nil)
	revDevReq.Header.Set("Authorization", "Bearer "+bob.Token)
	revDevRec := httptest.NewRecorder()
	h.ServeHTTP(revDevRec, revDevReq)
	if revDevRec.Code != http.StatusNoContent {
		t.Fatalf("client revoke device = %d %s", revDevRec.Code, revDevRec.Body.String())
	}

	// 6. Tenant overview via /v1/self/overview works and tenant cannot access /admin/api/overview
	ovReq := httptest.NewRequest(http.MethodGet, "/v1/self/overview", nil)
	ovReq.AddCookie(tenantCookie)
	ovRec := httptest.NewRecorder()
	h.ServeHTTP(ovRec, ovReq)
	if ovRec.Code != http.StatusOK || !strings.Contains(ovRec.Body.String(), `"bob"`) {
		t.Fatalf("self overview = %d %s", ovRec.Code, ovRec.Body.String())
	}

	adminProbe := httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)
	adminProbe.AddCookie(tenantCookie)
	adminProbeRec := httptest.NewRecorder()
	h.ServeHTTP(adminProbeRec, adminProbe)
	if adminProbeRec.Code != http.StatusUnauthorized {
		t.Fatalf("tenant cookie must NOT authenticate /admin/api/overview, got %d", adminProbeRec.Code)
	}

	// 7. Tenant changes own password -> other tenant sessions revoked
	login2 := httptest.NewRequest(http.MethodPost, "/v1/self/login", strings.NewReader(`{"username":"bob","password":"bobsecret123"}`))
	login2.Header.Set(selfCSRFHeader, "1")
	login2Rec := httptest.NewRecorder()
	h.ServeHTTP(login2Rec, login2)
	var otherCookie *http.Cookie
	for _, c := range login2Rec.Result().Cookies() {
		if c.Name == tenantSessionCookie {
			otherCookie = c
		}
	}

	pwReq := httptest.NewRequest(http.MethodPost, "/v1/self/password", strings.NewReader(`{"currentPassword":"bobsecret123","newPassword":"newbobsecret9"}`))
	pwReq.Header.Set(selfCSRFHeader, "1")
	pwReq.AddCookie(tenantCookie)
	pwRec := httptest.NewRecorder()
	h.ServeHTTP(pwRec, pwReq)
	if pwRec.Code != http.StatusNoContent {
		t.Fatalf("self password = %d %s", pwRec.Code, pwRec.Body.String())
	}

	otherCheck := httptest.NewRequest(http.MethodGet, "/v1/self/overview", nil)
	otherCheck.AddCookie(otherCookie)
	otherCheckRec := httptest.NewRecorder()
	h.ServeHTTP(otherCheckRec, otherCheck)
	if otherCheckRec.Code != http.StatusUnauthorized {
		t.Fatalf("other tenant session should be revoked on password change, got %d", otherCheckRec.Code)
	}
}
