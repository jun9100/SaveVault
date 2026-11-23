// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type testPairing struct {
	ID        string    `json:"id"`
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}

type testCreatedDeviceToken struct {
	Token  string          `json:"token"`
	Record deviceTokenView `json:"record"`
}

type testClaimResponse struct {
	Token         string `json:"token"`
	TokenID       string `json:"token_id"`
	DeviceID      string `json:"device_id"`
	Name          string `json:"name"`
	ServerVersion string `json:"server_version"`
}

func newDeviceTokenTestServer(t *testing.T) (*server, *httptest.Server, string) {
	t.Helper()
	dataDir := t.TempDir()
	s, httpServer := newAdminTestServer(t, dataDir)
	t.Cleanup(httpServer.Close)
	return s, httpServer, dataDir
}

func deviceTestDo(t *testing.T, request *http.Request) (*http.Response, []byte) {
	t.Helper()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return response, contents
}

func deviceBearerRequest(t *testing.T, method, target, bearer string, body []byte) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set(edenSaveSyncProtocolHeader, strconv.Itoa(minimumEdenSaveSyncProtocol))
	return request
}

func deviceTestErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode error response %q: %v", body, err)
	}
	return payload.Code
}

func createDevicePairing(t *testing.T, baseURL, name string, ttlMinutes int) testPairing {
	t.Helper()
	response, body := adminDo(t, http.MethodPost, baseURL+"/admin/api/pairings",
		map[string]any{"name": name, "ttl_minutes": ttlMinutes})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create pairing status = %d: %s", response.StatusCode, body)
	}
	var pairing testPairing
	if err := json.Unmarshal(body, &pairing); err != nil {
		t.Fatal(err)
	}
	if len(pairing.ID) != 16 || len(pairing.Code) != 9 || pairing.Code[4] != '-' {
		t.Fatalf("unexpected pairing response: %+v", pairing)
	}
	return pairing
}

func claimDevicePairing(t *testing.T, baseURL, code, deviceID, deviceName string) (*http.Response, []byte) {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"code": code, "device_id": deviceID, "device_name": deviceName,
		"platform": "test-platform", "version": "1.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+"/v1/pair/claim", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	return deviceTestDo(t, request)
}

func createDeviceAdminToken(t *testing.T, baseURL, name string) testCreatedDeviceToken {
	t.Helper()
	response, body := adminDo(t, http.MethodPost, baseURL+"/admin/api/device-tokens",
		map[string]string{"name": name})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create device token status = %d: %s", response.StatusCode, body)
	}
	var created testCreatedDeviceToken
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := parseDeviceToken(created.Token); !ok {
		t.Fatalf("created token has invalid format: %q", created.Token)
	}
	if created.Record.ID == "" || created.Record.CreatedVia != "admin" {
		t.Fatalf("unexpected token record: %+v", created.Record)
	}
	return created
}

func revokeDeviceAdminToken(t *testing.T, baseURL, tokenID string) {
	t.Helper()
	response, body := adminDo(t, http.MethodPost,
		baseURL+"/admin/api/device-tokens/"+tokenID+"/revoke", nil)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke device token status = %d: %s", response.StatusCode, body)
	}
}

func listDeviceTokens(t *testing.T, baseURL string) deviceTokenListResponse {
	t.Helper()
	response, body := adminDo(t, http.MethodGet, baseURL+"/admin/api/device-tokens", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("list device tokens status = %d: %s", response.StatusCode, body)
	}
	var list deviceTokenListResponse
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	return list
}

func tokenWithWrongSecret(t *testing.T, token string) string {
	t.Helper()
	id, secret, ok := parseDeviceToken(token)
	if !ok {
		t.Fatalf("invalid token %q", token)
	}
	replacement := byte('0')
	if secret[0] == replacement {
		replacement = '1'
	}
	return deviceTokenPrefix + id + "_" + string(replacement) + secret[1:]
}

func putBlobWithBearer(t *testing.T, baseURL, bearer string, payload []byte) string {
	t.Helper()
	digest := sha256.Sum256(payload)
	id := hex.EncodeToString(digest[:])
	request := deviceBearerRequest(t, http.MethodPut, baseURL+"/v1/blobs/"+id, bearer, payload)
	response, body := deviceTestDo(t, request)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("put blob status = %d: %s", response.StatusCode, body)
	}
	return id
}

func commitSnapshotWithBearer(t *testing.T, baseURL, bearer, scopeID, ifMatch, deviceID string, value snapshot) int {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	request := deviceBearerRequest(t, http.MethodPut,
		baseURL+"/v1/scopes/"+scopeID+"/snapshots/"+value.ID, bearer, payload)
	request.Header.Set("If-Match", `"`+ifMatch+`"`)
	if deviceID != "" {
		request.Header.Set("X-Eden-Device-ID", deviceID)
	}
	response, body := deviceTestDo(t, request)
	if response.StatusCode/100 == 2 {
		return response.StatusCode
	}
	t.Logf("snapshot response: %d %s", response.StatusCode, body)
	return response.StatusCode
}

func TestPairingCodeIsSingleUse(t *testing.T) {
	_, httpServer, _ := newDeviceTokenTestServer(t)
	pairing := createDevicePairing(t, httpServer.URL, "Tablet", 10)

	response, body := claimDevicePairing(t, httpServer.URL, pairing.Code, "pair-device", "Pixel 9")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("claim status = %d: %s", response.StatusCode, body)
	}
	var claimed testClaimResponse
	if err := json.Unmarshal(body, &claimed); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := parseDeviceToken(claimed.Token); !ok || len(claimed.Token) != 86 {
		t.Fatalf("claim returned an invalid token: %q", claimed.Token)
	}
	if claimed.DeviceID != "pair-device" || claimed.Name != "Pixel 9" || claimed.ServerVersion != version {
		t.Fatalf("unexpected claim response: %+v", claimed)
	}

	response, body = claimDevicePairing(t, httpServer.URL, pairing.Code, "pair-device", "Pixel 9")
	if response.StatusCode != http.StatusUnauthorized || deviceTestErrorCode(t, body) != "pairing_invalid" {
		t.Fatalf("reused pairing code = %d %s", response.StatusCode, body)
	}
}

func TestPairingCodeExpires(t *testing.T) {
	s, httpServer, _ := newDeviceTokenTestServer(t)
	base := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	pairing := createDevicePairing(t, httpServer.URL, "Old tablet", 5)
	s.now = func() time.Time { return base.Add(6 * time.Minute) }

	response, body := claimDevicePairing(t, httpServer.URL, pairing.Code, "expired-device", "Old tablet")
	if response.StatusCode != http.StatusUnauthorized || deviceTestErrorCode(t, body) != "pairing_invalid" {
		t.Fatalf("expired pairing code = %d %s", response.StatusCode, body)
	}
}

func TestPairingCodeCanBeRevoked(t *testing.T) {
	_, httpServer, _ := newDeviceTokenTestServer(t)
	pairing := createDevicePairing(t, httpServer.URL, "Cancelled", 10)
	response, body := adminDo(t, http.MethodDelete, httpServer.URL+"/admin/api/pairings/"+pairing.ID, nil)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("delete pairing status = %d: %s", response.StatusCode, body)
	}
	response, body = claimDevicePairing(t, httpServer.URL, pairing.Code, "revoked-pairing-device", "Cancelled")
	if response.StatusCode != http.StatusUnauthorized || deviceTestErrorCode(t, body) != "pairing_invalid" {
		t.Fatalf("revoked pairing code = %d %s", response.StatusCode, body)
	}
}

func TestPairingClaimIsRateLimitedPerIP(t *testing.T) {
	_, httpServer, _ := newDeviceTokenTestServer(t)
	pairing := createDevicePairing(t, httpServer.URL, "Rate limited", 10)
	normalized, ok := normalizePairingCode(pairing.Code)
	if !ok {
		t.Fatal("generated pairing code does not normalize")
	}
	wrong := []byte(normalized)
	if wrong[0] == '0' {
		wrong[0] = '1'
	} else {
		wrong[0] = '0'
	}
	for attempt := 0; attempt < 8; attempt++ {
		response, body := claimDevicePairing(t, httpServer.URL, string(wrong), "rate-device", "")
		if response.StatusCode != http.StatusUnauthorized || deviceTestErrorCode(t, body) != "pairing_invalid" {
			t.Fatalf("failed claim %d = %d %s", attempt+1, response.StatusCode, body)
		}
	}
	response, body := claimDevicePairing(t, httpServer.URL, pairing.Code, "rate-device", "")
	if response.StatusCode != http.StatusTooManyRequests || deviceTestErrorCode(t, body) != "rate_limited" {
		t.Fatalf("blocked claim with a correct code = %d %s", response.StatusCode, body)
	}
}

func TestDeviceTokenSyncUsesBoundDeviceID(t *testing.T) {
	s, httpServer, _ := newDeviceTokenTestServer(t)
	pairing := createDevicePairing(t, httpServer.URL, "Bound tablet", 10)
	response, body := claimDevicePairing(t, httpServer.URL, pairing.Code, "bound-device", "Bound tablet")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("claim status = %d: %s", response.StatusCode, body)
	}
	var claimed testClaimResponse
	if err := json.Unmarshal(body, &claimed); err != nil {
		t.Fatal(err)
	}

	blob := putBlobWithBearer(t, httpServer.URL, claimed.Token, []byte("bound device payload"))
	if status := commitSnapshotWithBearer(t, httpServer.URL, claimed.Token, "scope-token", "", "other-device",
		snapshot{ID: "snap-1", Blob: blob, CreatedAt: time.Now().UTC()}); status != http.StatusCreated {
		t.Fatalf("device token snapshot status = %d", status)
	}

	s.mu.Lock()
	record := s.state.Scopes["scope-token"].Snapshots["snap-1"]
	_, spoofed := s.dashboard.Devices["other-device"]
	device := s.dashboard.Devices["bound-device"]
	s.mu.Unlock()
	if record.Device != "bound-device" {
		t.Fatalf("snapshot device = %q, want bound-device", record.Device)
	}
	if spoofed {
		t.Fatal("spoofed device ID was recorded")
	}
	if device.Auth != "device" || device.TokenID != claimed.TokenID {
		t.Fatalf("dashboard device auth fields = %+v", device)
	}
}

func TestRevokedDeviceTokenIsRejected(t *testing.T) {
	_, httpServer, _ := newDeviceTokenTestServer(t)
	first := createDeviceAdminToken(t, httpServer.URL, "First")
	second := createDeviceAdminToken(t, httpServer.URL, "Second")
	revokeDeviceAdminToken(t, httpServer.URL, first.Record.ID)

	response, body := deviceTestDo(t, deviceBearerRequest(t, http.MethodGet, httpServer.URL+"/v1/scopes", first.Token, nil))
	if response.StatusCode != http.StatusUnauthorized || deviceTestErrorCode(t, body) != "token_revoked" {
		t.Fatalf("revoked token = %d %s", response.StatusCode, body)
	}
	for name, token := range map[string]string{"other device": second.Token, "shared": testToken} {
		response, body = deviceTestDo(t, deviceBearerRequest(t, http.MethodGet, httpServer.URL+"/v1/scopes", token, nil))
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s token = %d %s", name, response.StatusCode, body)
		}
	}
}

func TestWrongDeviceTokenSecretIsUnauthorized(t *testing.T) {
	_, httpServer, _ := newDeviceTokenTestServer(t)
	created := createDeviceAdminToken(t, httpServer.URL, "Secret check")
	wrong := tokenWithWrongSecret(t, created.Token)
	response, body := deviceTestDo(t, deviceBearerRequest(t, http.MethodGet, httpServer.URL+"/v1/scopes", wrong, nil))
	if response.StatusCode != http.StatusUnauthorized || deviceTestErrorCode(t, body) != "unauthorized" {
		t.Fatalf("wrong secret = %d %s", response.StatusCode, body)
	}

	// A wrong secret must not reveal that the token ID exists and is revoked.
	revokeDeviceAdminToken(t, httpServer.URL, created.Record.ID)
	response, body = deviceTestDo(t, deviceBearerRequest(t, http.MethodGet, httpServer.URL+"/v1/scopes", wrong, nil))
	if response.StatusCode != http.StatusUnauthorized || deviceTestErrorCode(t, body) != "unauthorized" {
		t.Fatalf("wrong secret on revoked token = %d %s", response.StatusCode, body)
	}
	response, body = deviceTestDo(t, deviceBearerRequest(t, http.MethodGet, httpServer.URL+"/v1/scopes", created.Token, nil))
	if response.StatusCode != http.StatusUnauthorized || deviceTestErrorCode(t, body) != "token_revoked" {
		t.Fatalf("correct secret on revoked token = %d %s", response.StatusCode, body)
	}
}

func TestMalformedDeviceTokenIsUnauthorized(t *testing.T) {
	_, httpServer, _ := newDeviceTokenTestServer(t)
	cases := []string{
		"gsdt_",
		"gsdt_not-a-token",
		"gsdt_" + strings.Repeat("0", 16) + "_" + strings.Repeat("0", 63),
		"gsdt_" + strings.Repeat("0", 15) + "_" + strings.Repeat("0", 64),
		"gsdt_" + strings.Repeat("A", 16) + "_" + strings.Repeat("0", 64),
		"gsdt_0123456789abcdef_" + strings.Repeat("a", 64),
	}
	for _, candidate := range cases {
		response, body := deviceTestDo(t, deviceBearerRequest(t, http.MethodGet, httpServer.URL+"/v1/scopes", candidate, nil))
		if response.StatusCode != http.StatusUnauthorized || deviceTestErrorCode(t, body) != "unauthorized" {
			t.Fatalf("candidate %q = %d %s", candidate, response.StatusCode, body)
		}
	}
}

func TestSharedTokenCannotUseDevicePrefix(t *testing.T) {
	reserved := "gsdt_" + strings.Repeat("0", 16) + "_" + strings.Repeat("1", 64)
	if _, err := newServer(t.TempDir(), reserved, 1024*1024); err == nil ||
		!strings.Contains(err.Error(), `GAME_SAVE_TOKEN must not start with "gsdt_"`) {
		t.Fatalf("reserved shared token error = %v", err)
	}
}

func TestSharedTokenCompatibilityModes(t *testing.T) {
	s, httpServer, _ := newDeviceTokenTestServer(t)
	device := createDeviceAdminToken(t, httpServer.URL, "Compatibility device")

	cases := []struct {
		mode       string
		wantStatus int
		wantWarn   bool
	}{
		{mode: sharedTokenModeAllow, wantStatus: http.StatusOK},
		{mode: sharedTokenModeWarn, wantStatus: http.StatusOK, wantWarn: true},
		{mode: sharedTokenModeDeny, wantStatus: http.StatusUnauthorized},
	}
	for _, testCase := range cases {
		response, body := adminDo(t, http.MethodPut, httpServer.URL+"/admin/api/auth-settings",
			map[string]string{"shared_token_mode": testCase.mode})
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("set mode %s = %d: %s", testCase.mode, response.StatusCode, body)
		}
		sharedRequest := deviceBearerRequest(t, http.MethodGet, httpServer.URL+"/v1/scopes", testToken, nil)
		sharedRequest.Header.Set("X-Eden-Device-ID", "shared-compat-device")
		response, body = deviceTestDo(t, sharedRequest)
		if response.StatusCode != testCase.wantStatus {
			t.Fatalf("mode %s shared token = %d %s", testCase.mode, response.StatusCode, body)
		}
		warning := response.Header.Get(sharedTokenWarningHeader)
		if testCase.wantWarn && warning != sharedTokenWarningValue {
			t.Fatalf("mode %s warning = %q", testCase.mode, warning)
		}
		if !testCase.wantWarn && warning != "" {
			t.Fatalf("mode %s unexpected warning = %q", testCase.mode, warning)
		}
		if testCase.wantStatus == http.StatusUnauthorized &&
			deviceTestErrorCode(t, body) != "shared_token_disabled" {
			t.Fatalf("mode %s error code = %s", testCase.mode, body)
		}
	}
	response, body := deviceTestDo(t, deviceBearerRequest(t, http.MethodGet, httpServer.URL+"/v1/scopes", device.Token, nil))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("device token in deny mode = %d %s", response.StatusCode, body)
	}
	s.mu.Lock()
	sharedDevices := 0
	for _, info := range s.dashboard.Devices {
		if info.Auth == "" {
			sharedDevices++
		}
	}
	s.mu.Unlock()
	if sharedDevices == 0 {
		t.Fatal("allow/warn shared requests should have recorded a shared-token device")
	}
}

func TestAdminTokenIsNotASyncToken(t *testing.T) {
	_, httpServer, _ := newDeviceTokenTestServer(t)
	response, body := deviceTestDo(t, deviceBearerRequest(t, http.MethodGet, httpServer.URL+"/v1/scopes", testAdminToken, nil))
	if response.StatusCode != http.StatusUnauthorized || deviceTestErrorCode(t, body) != "unauthorized" {
		t.Fatalf("admin token as bearer = %d %s", response.StatusCode, body)
	}
}

func TestDeviceAndSharedTokensCannotAuthenticateAdmin(t *testing.T) {
	_, httpServer, _ := newDeviceTokenTestServer(t)
	device := createDeviceAdminToken(t, httpServer.URL, "Not an admin")
	for name, credential := range map[string]string{"device": device.Token, "shared": testToken} {
		request, err := http.NewRequest(http.MethodGet, httpServer.URL+"/admin/api/overview", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.SetBasicAuth("admin", credential)
		response, body := deviceTestDo(t, request)
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s token as admin Basic Auth = %d %s", name, response.StatusCode, body)
		}
	}
}

func TestDeviceTokenCannotAuthenticateWebDAV(t *testing.T) {
	dataDir := t.TempDir()
	s, err := newServerWithOptions(dataDir, testToken, 1024*1024, serverOptions{
		adminToken:       testAdminToken,
		saveVaultUsername: testSaveVaultUsername,
		saveVaultPassword: testSaveVaultPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s.handler())
	t.Cleanup(httpServer.Close)
	device := createDeviceAdminToken(t, httpServer.URL, "WebDAV rejected")

	request, err := http.NewRequest("PROPFIND", httpServer.URL+"/savevault/", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth(testSaveVaultUsername, device.Token)
	response, body := deviceTestDo(t, request)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("device token as WebDAV password = %d %s", response.StatusCode, body)
	}
}

func TestReadOnlyEndpointsDoNotTouchDeviceTokenState(t *testing.T) {
	s, httpServer, dataDir := newDeviceTokenTestServer(t)

	heartbeat := authorizedRequest(t, http.MethodPost, httpServer.URL+"/v1/devices/heartbeat", nil)
	heartbeat.Header.Set("X-Eden-Device-ID", "shared-device")
	response, body := deviceTestDo(t, heartbeat)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("shared heartbeat = %d %s", response.StatusCode, body)
	}
	device := createDeviceAdminToken(t, httpServer.URL, "Read only")
	pairing := createDevicePairing(t, httpServer.URL, "Bound read only", 10)
	response, body = claimDevicePairing(t, httpServer.URL, pairing.Code, "bound-read-only", "Bound read only")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("claim status = %d: %s", response.StatusCode, body)
	}
	var bound testClaimResponse
	if err := json.Unmarshal(body, &bound); err != nil {
		t.Fatal(err)
	}

	dashboardBefore, err := os.ReadFile(filepath.Join(dataDir, "dashboard.json"))
	if err != nil {
		t.Fatal(err)
	}
	tokensBefore, err := os.ReadFile(filepath.Join(dataDir, "device-tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{device.Token, bound.Token} {
		for _, target := range []string{
			"/v1/auth/whoami",
			"/v1/scopes/read-only-scope/status",
			"/v1/savevault/files",
		} {
			request := deviceBearerRequest(t, http.MethodGet, httpServer.URL+target, token, nil)
			request.Header.Set("X-Eden-Device-ID", "attacker-device")
			response, body := deviceTestDo(t, request)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("read-only %s = %d %s", target, response.StatusCode, body)
			}
		}
	}
	dashboardAfter, err := os.ReadFile(filepath.Join(dataDir, "dashboard.json"))
	if err != nil {
		t.Fatal(err)
	}
	tokensAfter, err := os.ReadFile(filepath.Join(dataDir, "device-tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dashboardBefore, dashboardAfter) {
		t.Fatalf("read-only requests changed dashboard.json\nbefore: %s\nafter: %s", dashboardBefore, dashboardAfter)
	}
	if !bytes.Equal(tokensBefore, tokensAfter) {
		t.Fatalf("read-only requests changed device-tokens.json\nbefore: %s\nafter: %s", tokensBefore, tokensAfter)
	}
	s.tokensMu.Lock()
	record := s.deviceTokens.Tokens[device.Record.ID]
	boundRecord := s.deviceTokens.Tokens[bound.TokenID]
	s.tokensMu.Unlock()
	if !record.LastUsedAt.IsZero() || record.DeviceID != "" {
		t.Fatalf("read-only request mutated the unbound token record: %+v", record)
	}
	if !boundRecord.LastUsedAt.IsZero() || boundRecord.DeviceID != "bound-read-only" {
		t.Fatalf("read-only request mutated the bound token record: %+v", boundRecord)
	}
	s.mu.Lock()
	_, attackerRecorded := s.dashboard.Devices["attacker-device"]
	s.mu.Unlock()
	if attackerRecorded {
		t.Fatal("read-only request bound an unbound device token")
	}
}

func TestDeletingDeviceRevokesBoundTokens(t *testing.T) {
	s, httpServer, _ := newDeviceTokenTestServer(t)
	pairing := createDevicePairing(t, httpServer.URL, "Delete me", 10)
	response, body := claimDevicePairing(t, httpServer.URL, pairing.Code, "delete-device", "Delete me")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("claim status = %d: %s", response.StatusCode, body)
	}
	var claimed testClaimResponse
	if err := json.Unmarshal(body, &claimed); err != nil {
		t.Fatal(err)
	}

	response, body = adminDo(t, http.MethodDelete, httpServer.URL+"/admin/api/devices/delete-device", nil)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("delete device status = %d: %s", response.StatusCode, body)
	}
	response, body = deviceTestDo(t, deviceBearerRequest(t, http.MethodGet, httpServer.URL+"/v1/scopes", claimed.Token, nil))
	if response.StatusCode != http.StatusUnauthorized || deviceTestErrorCode(t, body) != "token_revoked" {
		t.Fatalf("token after deleting device = %d %s", response.StatusCode, body)
	}
	s.tokensMu.Lock()
	record := s.deviceTokens.Tokens[claimed.TokenID]
	s.tokensMu.Unlock()
	if record.RevokedAt.IsZero() {
		t.Fatal("token bound to the deleted device was not revoked")
	}

	response, body = adminDo(t, http.MethodGet, httpServer.URL+"/admin/api/events", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("events status = %d: %s", response.StatusCode, body)
	}
	var eventsPayload struct {
		Events []event `json:"events"`
	}
	if err := json.Unmarshal(body, &eventsPayload); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range eventsPayload.Events {
		if item.Kind == eventDeviceTokenRevoked && item.Device == "delete-device" &&
			strings.Contains(item.Detail, claimed.TokenID) {
			found = true
		}
	}
	if !found {
		t.Fatalf("device_token_revoked event missing: %+v", eventsPayload.Events)
	}
}

func TestPairingDisabledWithoutAdminToken(t *testing.T) {
	s, err := newServer(t.TempDir(), testToken, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s.handler())
	t.Cleanup(httpServer.Close)

	response, body := claimDevicePairing(t, httpServer.URL, "ABCD-EFGH", "no-admin-device", "No admin")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("claim without an admin token = %d %s", response.StatusCode, body)
	}
	response, body = deviceTestDo(t, deviceBearerRequest(t, http.MethodGet, httpServer.URL+"/v1/scopes", testToken, nil))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("shared token without admin = %d %s", response.StatusCode, body)
	}
}

func TestDeviceTokensSurviveRestart(t *testing.T) {
	dataDir := t.TempDir()
	first, err := newServer(dataDir, testToken, 1024*1024, testAdminToken)
	if err != nil {
		t.Fatal(err)
	}
	firstHTTP := httptest.NewServer(first.handler())
	kept := createDeviceAdminToken(t, firstHTTP.URL, "Kept")
	revoked := createDeviceAdminToken(t, firstHTTP.URL, "Revoked")
	revokeDeviceAdminToken(t, firstHTTP.URL, revoked.Record.ID)
	bind := deviceBearerRequest(t, http.MethodPost, firstHTTP.URL+"/v1/devices/heartbeat", kept.Token, nil)
	bind.Header.Set("X-Eden-Device-ID", "restart-device")
	if response, body := deviceTestDo(t, bind); response.StatusCode != http.StatusNoContent {
		t.Fatalf("bind before restart = %d %s", response.StatusCode, body)
	}
	pairing := createDevicePairing(t, firstHTTP.URL, "Never claimed", 10)
	firstHTTP.Close()
	first.Close()

	second, err := newServer(dataDir, testToken, 1024*1024, testAdminToken)
	if err != nil {
		t.Fatal(err)
	}
	secondHTTP := httptest.NewServer(second.handler())
	t.Cleanup(secondHTTP.Close)

	response, body := deviceTestDo(t, deviceBearerRequest(t, http.MethodGet, secondHTTP.URL+"/v1/scopes", kept.Token, nil))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("token after restart = %d %s", response.StatusCode, body)
	}
	response, body = deviceTestDo(t, deviceBearerRequest(t, http.MethodGet, secondHTTP.URL+"/v1/scopes", revoked.Token, nil))
	if response.StatusCode != http.StatusUnauthorized || deviceTestErrorCode(t, body) != "token_revoked" {
		t.Fatalf("revoked token after restart = %d %s", response.StatusCode, body)
	}
	response, body = claimDevicePairing(t, secondHTTP.URL, pairing.Code, "restart-device", "")
	if response.StatusCode != http.StatusUnauthorized || deviceTestErrorCode(t, body) != "pairing_invalid" {
		t.Fatalf("pairing across restart = %d %s", response.StatusCode, body)
	}
	list := listDeviceTokens(t, secondHTTP.URL)
	if len(list.Tokens) != 2 {
		t.Fatalf("tokens after restart = %+v", list.Tokens)
	}
	var restored deviceTokenView
	for _, token := range list.Tokens {
		if token.ID == kept.Record.ID {
			restored = token
		}
	}
	if restored.DeviceID != "restart-device" {
		t.Fatalf("device binding after restart = %+v", restored)
	}
	rebind := deviceBearerRequest(t, http.MethodPost, secondHTTP.URL+"/v1/devices/heartbeat", kept.Token, nil)
	rebind.Header.Set("X-Eden-Device-ID", "restart-other-device")
	if response, body := deviceTestDo(t, rebind); response.StatusCode != http.StatusNoContent {
		t.Fatalf("heartbeat after restart = %d %s", response.StatusCode, body)
	}
	second.mu.Lock()
	_, spoofed := second.dashboard.Devices["restart-other-device"]
	device := second.dashboard.Devices["restart-device"]
	second.mu.Unlock()
	if spoofed || device.TokenID != kept.Record.ID {
		t.Fatalf("binding not restored after restart: %+v", device)
	}
}

func TestSharedOnlyDeploymentDoesNotCreateDeviceTokenFile(t *testing.T) {
	dataDir := t.TempDir()
	s, err := newServer(dataDir, testToken, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s.handler())
	t.Cleanup(httpServer.Close)

	object := putObject(t, httpServer.URL, []byte("shared-only object"))
	commitSnapshot(t, httpServer.URL, "shared-scope",
		snapshot{ID: "shared-snapshot", Blob: object, CreatedAt: time.Now().UTC()}, "", "shared-only-device")

	if _, err := os.Stat(filepath.Join(dataDir, "device-tokens.json")); !os.IsNotExist(err) {
		t.Fatalf("device-tokens.json should not exist, stat error = %v", err)
	}
	dashboard, err := os.ReadFile(filepath.Join(dataDir, "dashboard.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(dashboard, []byte(`"token_id"`)) || bytes.Contains(dashboard, []byte(`"auth"`)) {
		t.Fatalf("shared-only dashboard contains device-token fields: %s", dashboard)
	}
}

func TestAdminCreatedDeviceTokenIsSafeAndBindsOnFirstWrite(t *testing.T) {
	s, httpServer, _ := newDeviceTokenTestServer(t)
	created := createDeviceAdminToken(t, httpServer.URL, "Web token")

	response, body := adminDo(t, http.MethodGet, httpServer.URL+"/admin/api/device-tokens", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d: %s", response.StatusCode, body)
	}
	if bytes.Contains(body, []byte(created.Token)) || bytes.Contains(body, []byte("secret_sha256")) ||
		bytes.Contains(body, []byte(`"token"`)) {
		t.Fatalf("token list leaked a secret: %s", body)
	}
	list := listDeviceTokens(t, httpServer.URL)
	if len(list.Tokens) != 1 || list.Tokens[0].DeviceID != "" || list.Tokens[0].CreatedVia != "admin" {
		t.Fatalf("admin-created token view = %+v", list.Tokens)
	}

	first := deviceBearerRequest(t, http.MethodPost, httpServer.URL+"/v1/devices/heartbeat", created.Token, nil)
	first.Header.Set("X-Eden-Device-ID", "web-device")
	first.Header.Set("X-Eden-Device-Name", "Reported Name")
	response, body = deviceTestDo(t, first)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("first device token write = %d %s", response.StatusCode, body)
	}
	second := deviceBearerRequest(t, http.MethodPost, httpServer.URL+"/v1/devices/heartbeat", created.Token, nil)
	second.Header.Set("X-Eden-Device-ID", "spoofed-device")
	second.Header.Set("X-Eden-Device-Name", "Spoofed Name")
	response, body = deviceTestDo(t, second)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("second device token write = %d %s", response.StatusCode, body)
	}

	s.mu.Lock()
	device := s.dashboard.Devices["web-device"]
	_, spoofed := s.dashboard.Devices["spoofed-device"]
	s.mu.Unlock()
	if spoofed || device.ID != "web-device" {
		t.Fatalf("token binding used the header over the bound ID: %+v", s.dashboard.Devices)
	}
	s.tokensMu.Lock()
	record := s.deviceTokens.Tokens[created.Record.ID]
	s.tokensMu.Unlock()
	if record.DeviceID != "web-device" {
		t.Fatalf("token device binding = %q", record.DeviceID)
	}

	list = listDeviceTokens(t, httpServer.URL)
	if len(list.Tokens) != 1 || list.Tokens[0].DisplayName != "Web token" {
		t.Fatalf("token name should precede the reported device name: %+v", list.Tokens)
	}
	response, body = adminDo(t, http.MethodPatch, httpServer.URL+"/admin/api/devices/web-device",
		map[string]string{"alias": "Desk tablet"})
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("device alias status = %d: %s", response.StatusCode, body)
	}
	list = listDeviceTokens(t, httpServer.URL)
	if list.Tokens[0].DisplayName != "Desk tablet" {
		t.Fatalf("device alias should take priority: %+v", list.Tokens)
	}
}

func TestWhoAmIReportsAuthenticationType(t *testing.T) {
	_, httpServer, _ := newDeviceTokenTestServer(t)
	created := createDeviceAdminToken(t, httpServer.URL, "Who token")
	write := deviceBearerRequest(t, http.MethodPost, httpServer.URL+"/v1/devices/heartbeat", created.Token, nil)
	write.Header.Set("X-Eden-Device-ID", "who-device")
	response, body := deviceTestDo(t, write)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("bind whoami device = %d %s", response.StatusCode, body)
	}

	response, body = deviceTestDo(t, deviceBearerRequest(t, http.MethodGet, httpServer.URL+"/v1/auth/whoami", created.Token, nil))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("device whoami status = %d: %s", response.StatusCode, body)
	}
	var deviceIdentity struct {
		Auth            string `json:"auth"`
		TokenID         string `json:"token_id"`
		DeviceID        string `json:"device_id"`
		Name            string `json:"name"`
		SharedTokenMode string `json:"shared_token_mode"`
	}
	if err := json.Unmarshal(body, &deviceIdentity); err != nil {
		t.Fatal(err)
	}
	if deviceIdentity.Auth != "device" || deviceIdentity.TokenID != created.Record.ID ||
		deviceIdentity.DeviceID != "who-device" || deviceIdentity.Name != "Who token" ||
		deviceIdentity.SharedTokenMode != sharedTokenModeAllow {
		t.Fatalf("device whoami = %+v", deviceIdentity)
	}

	response, body = deviceTestDo(t, deviceBearerRequest(t, http.MethodGet, httpServer.URL+"/v1/auth/whoami", testToken, nil))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("shared whoami status = %d: %s", response.StatusCode, body)
	}
	var sharedIdentity map[string]any
	if err := json.Unmarshal(body, &sharedIdentity); err != nil {
		t.Fatal(err)
	}
	if sharedIdentity["auth"] != "shared" || sharedIdentity["shared_token_mode"] != sharedTokenModeAllow {
		t.Fatalf("shared whoami = %s", body)
	}
	for _, field := range []string{"token_id", "device_id", "name"} {
		if _, exists := sharedIdentity[field]; exists {
			t.Fatalf("shared whoami unexpectedly contains %s: %s", field, body)
		}
	}

	if response, body := adminDo(t, http.MethodPut, httpServer.URL+"/admin/api/auth-settings",
		map[string]string{"shared_token_mode": sharedTokenModeWarn}); response.StatusCode != http.StatusNoContent {
		t.Fatalf("set warn mode = %d: %s", response.StatusCode, body)
	}
	response, body = deviceTestDo(t, deviceBearerRequest(t, http.MethodGet, httpServer.URL+"/v1/auth/whoami", testToken, nil))
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"shared_token_mode":"warn"`)) {
		t.Fatalf("shared whoami in warn mode = %d %s", response.StatusCode, body)
	}
}

func TestOnlyRevokedDeviceTokensCanBeDeleted(t *testing.T) {
	_, httpServer, _ := newDeviceTokenTestServer(t)
	created := createDeviceAdminToken(t, httpServer.URL, "Delete token")
	path := httpServer.URL + "/admin/api/device-tokens/" + created.Record.ID
	response, body := adminDo(t, http.MethodDelete, path, nil)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("delete active token = %d %s", response.StatusCode, body)
	}
	revokeDeviceAdminToken(t, httpServer.URL, created.Record.ID)
	// Repeated revocation is idempotent.
	revokeDeviceAdminToken(t, httpServer.URL, created.Record.ID)
	response, body = adminDo(t, http.MethodDelete, path, nil)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("delete revoked token = %d %s", response.StatusCode, body)
	}
	if list := listDeviceTokens(t, httpServer.URL); len(list.Tokens) != 0 {
		t.Fatalf("token was not deleted: %+v", list.Tokens)
	}
	response, body = adminDo(t, http.MethodDelete, path, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("delete missing token = %d %s", response.StatusCode, body)
	}
}

func TestPairingLimitIsEnforced(t *testing.T) {
	_, httpServer, _ := newDeviceTokenTestServer(t)
	for index := 0; index < maxActivePairings; index++ {
		createDevicePairing(t, httpServer.URL, "Pairing "+strconv.Itoa(index), 10)
	}
	response, body := adminDo(t, http.MethodPost, httpServer.URL+"/admin/api/pairings",
		map[string]any{"name": "Too many", "ttl_minutes": 10})
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("sixth pairing = %d %s", response.StatusCode, body)
	}
	response, body = adminDo(t, http.MethodGet, httpServer.URL+"/admin/api/pairings", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("list pairings = %d %s", response.StatusCode, body)
	}
	var listed struct {
		Pairings []pairingView `json:"pairings"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Pairings) != maxActivePairings {
		t.Fatalf("active pairings = %d", len(listed.Pairings))
	}
}

func TestDeviceTokenMetricsAndOverviewCounts(t *testing.T) {
	_, httpServer, _ := newDeviceTokenTestServer(t)
	active := createDeviceAdminToken(t, httpServer.URL, "Active")
	revoked := createDeviceAdminToken(t, httpServer.URL, "Revoked")
	revokeDeviceAdminToken(t, httpServer.URL, revoked.Record.ID)

	if response, body := deviceTestDo(t, deviceBearerRequest(t, http.MethodGet, httpServer.URL+"/v1/scopes", active.Token, nil)); response.StatusCode != http.StatusOK {
		t.Fatalf("active token request = %d %s", response.StatusCode, body)
	}
	sharedRequest := deviceBearerRequest(t, http.MethodGet, httpServer.URL+"/v1/scopes", testToken, nil)
	sharedRequest.Header.Set("X-Eden-Device-ID", "shared-metrics-device")
	if response, body := deviceTestDo(t, sharedRequest); response.StatusCode != http.StatusOK {
		t.Fatalf("shared token request = %d %s", response.StatusCode, body)
	}
	if response, body := deviceTestDo(t, deviceBearerRequest(t, http.MethodGet, httpServer.URL+"/v1/scopes", "gsdt_invalid", nil)); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("rejected token request = %d %s", response.StatusCode, body)
	}

	response, body := adminDo(t, http.MethodGet, httpServer.URL+"/metrics", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d: %s", response.StatusCode, body)
	}
	for _, expected := range []string{
		`game_save_device_tokens{state="active"} 1`,
		`game_save_device_tokens{state="revoked"} 1`,
		`game_save_auth_requests_total{kind="device"} 1`,
		`game_save_auth_requests_total{kind="shared"} 1`,
		`game_save_auth_requests_total{kind="rejected"} 1`,
	} {
		if !bytes.Contains(body, []byte(expected)) {
			t.Fatalf("metrics missing %q:\n%s", expected, body)
		}
	}

	overview := fetchOverview(t, httpServer.URL)
	if overview.Stats.SharedTokenDevices != 1 {
		t.Fatalf("overview shared token devices = %d", overview.Stats.SharedTokenDevices)
	}
}

func TestPairingClaimGlobalLimit(t *testing.T) {
	s, httpServer, _ := newDeviceTokenTestServer(t)
	pairing := createDevicePairing(t, httpServer.URL, "Global limit", 10)
	now := s.now()
	for failure := 0; failure < globalClaimFailureLimit; failure++ {
		s.claimFailures.fail(now)
	}
	response, body := claimDevicePairing(t, httpServer.URL, pairing.Code, "global-limit-device", "")
	if response.StatusCode != http.StatusTooManyRequests || deviceTestErrorCode(t, body) != "rate_limited" {
		t.Fatalf("global claim limit = %d %s", response.StatusCode, body)
	}
}

func TestDeviceTokenFileValidation(t *testing.T) {
	cases := []struct {
		name     string
		contents string
		wantErr  bool
		wantMode string
	}{
		{name: "version one", contents: `{"version":1,"shared_token_mode":"warn","tokens":{}}`, wantMode: sharedTokenModeWarn},
		{name: "legacy zero values", contents: `{"tokens":null}`, wantMode: sharedTokenModeAllow},
		{name: "unsupported version", contents: `{"version":2,"shared_token_mode":"allow","tokens":{}}`, wantErr: true},
		{name: "invalid mode", contents: `{"version":1,"shared_token_mode":"sometimes","tokens":{}}`, wantErr: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			dataDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dataDir, "device-tokens.json"),
				[]byte(testCase.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := newServer(dataDir, testToken, 1024*1024, testAdminToken)
			if testCase.wantErr {
				if err == nil {
					s.Close()
					t.Fatal("invalid device-tokens.json was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if s.deviceTokens.SharedTokenMode != testCase.wantMode {
				t.Fatalf("shared mode = %q, want %q", s.deviceTokens.SharedTokenMode, testCase.wantMode)
			}
			if s.deviceTokens.Tokens == nil {
				t.Fatal("token map was not initialized")
			}
		})
	}
}

func TestDeviceTokenLastUsedIsThrottled(t *testing.T) {
	s, httpServer, dataDir := newDeviceTokenTestServer(t)
	base := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	created := createDeviceAdminToken(t, httpServer.URL, "Throttled")

	write := func() {
		request := deviceBearerRequest(t, http.MethodPost, httpServer.URL+"/v1/devices/heartbeat", created.Token, nil)
		request.Header.Set("X-Eden-Device-ID", "throttle-device")
		response, body := deviceTestDo(t, request)
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("heartbeat = %d %s", response.StatusCode, body)
		}
	}
	lastUsed := func() time.Time {
		s.tokensMu.Lock()
		defer s.tokensMu.Unlock()
		return s.deviceTokens.Tokens[created.Record.ID].LastUsedAt
	}
	write()
	if !lastUsed().Equal(base) {
		t.Fatalf("initial last_used_at = %s", lastUsed())
	}
	before, err := os.ReadFile(filepath.Join(dataDir, "device-tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return base.Add(time.Minute) }
	write()
	after, err := os.ReadFile(filepath.Join(dataDir, "device-tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !lastUsed().Equal(base) || !bytes.Equal(before, after) {
		t.Fatalf("last_used_at was refreshed inside the throttle window: %s", lastUsed())
	}
	s.now = func() time.Time { return base.Add(6 * time.Minute) }
	write()
	if !lastUsed().Equal(base.Add(6 * time.Minute)) {
		t.Fatalf("last_used_at after the window = %s", lastUsed())
	}
}

func TestDeviceTokenAdminValidationAndEvents(t *testing.T) {
	_, httpServer, _ := newDeviceTokenTestServer(t)
	response, body := adminDo(t, http.MethodPut, httpServer.URL+"/admin/api/auth-settings",
		map[string]string{"shared_token_mode": "sometimes"})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid auth mode = %d %s", response.StatusCode, body)
	}
	created := createDeviceAdminToken(t, httpServer.URL, "Lifecycle")
	response, body = adminDo(t, http.MethodPatch,
		httpServer.URL+"/admin/api/device-tokens/"+created.Record.ID, map[string]string{"name": "Renamed"})
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("rename token = %d %s", response.StatusCode, body)
	}
	list := listDeviceTokens(t, httpServer.URL)
	if len(list.Tokens) != 1 || list.Tokens[0].Name != "Renamed" {
		t.Fatalf("renamed token list = %+v", list.Tokens)
	}
	response, body = adminDo(t, http.MethodPatch,
		httpServer.URL+"/admin/api/device-tokens/0123456789abcdef", map[string]string{"name": "Missing"})
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("rename missing token = %d %s", response.StatusCode, body)
	}
	revokeDeviceAdminToken(t, httpServer.URL, created.Record.ID)
	revokeDeviceAdminToken(t, httpServer.URL, created.Record.ID)

	response, body = adminDo(t, http.MethodGet, httpServer.URL+"/admin/api/events", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("events status = %d: %s", response.StatusCode, body)
	}
	var payload struct {
		Events []event `json:"events"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	createdEvents, revokedEvents := 0, 0
	for _, item := range payload.Events {
		switch item.Kind {
		case eventDeviceTokenCreated:
			createdEvents++
		case eventDeviceTokenRevoked:
			revokedEvents++
		}
	}
	if createdEvents != 1 || revokedEvents != 1 {
		t.Fatalf("token events: created=%d revoked=%d (%+v)", createdEvents, revokedEvents, payload.Events)
	}
}
