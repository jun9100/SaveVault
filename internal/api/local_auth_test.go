package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/jun9100/savevault/internal/daemon"
	"github.com/jun9100/savevault/internal/store"
)

func TestAPI_LocalTokenAuthAndTokenFileLifecycle(t *testing.T) {
	home := t.TempDir()
	d, err := daemon.New(daemon.Options{HomeOverride: home})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	d.Scanner.ManifestURL = ""
	srv := New(d)
	srv.RequireLocalAuth = true
	addr, err := srv.Start(0)
	if err != nil {
		d.Stop()
		t.Fatalf("srv.Start: %v", err)
	}
	t.Cleanup(func() {
		srv.Stop()
		d.Stop()
	})

	tokenPath := filepath.Join(home, "daemon.token")
	rawTok, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("daemon.token not written: %v", err)
	}
	if string(rawTok) != srv.LocalToken() || len(rawTok) != 64 {
		t.Fatalf("unexpected daemon.token content: %q", string(rawTok))
	}

	base := "http://" + addr

	// Missing token -> 401
	resp, err := http.Get(base + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /api/settings = %d, want 401", resp.StatusCode)
	}

	// Wrong token -> 401
	badReq, _ := http.NewRequest(http.MethodGet, base+"/api/settings", nil)
	badReq.Header.Set("Authorization", "Bearer wrong-token")
	badResp, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatal(err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong bearer GET /api/settings = %d, want 401", badResp.StatusCode)
	}

	// Valid Bearer header -> 200
	okReq, _ := http.NewRequest(http.MethodGet, base+"/api/settings", nil)
	okReq.Header.Set("Authorization", "Bearer "+srv.LocalToken())
	okResp, err := http.DefaultClient.Do(okReq)
	if err != nil {
		t.Fatal(err)
	}
	okResp.Body.Close()
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("valid bearer GET /api/settings = %d, want 200", okResp.StatusCode)
	}

	// Valid query param token (used by WS and cover <img>) -> 200
	qResp, err := http.Get(base + "/api/status?token=" + srv.LocalToken())
	if err != nil {
		t.Fatal(err)
	}
	qResp.Body.Close()
	if qResp.StatusCode != http.StatusOK {
		t.Fatalf("valid query token GET /api/status = %d, want 200", qResp.StatusCode)
	}

	// Stop cleans up daemon.token.
	srv.Stop()
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatalf("daemon.token should be removed on Stop, stat err=%v", err)
	}
}

func TestAPI_SettingsRedactsCloudSecretsAndPreservesOnEmptySave(t *testing.T) {
	ts := startTestServer(t)
	cfg, err := ts.daemon.Store.GetCloudConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Enabled = true
	cfg.Provider = "webdav"
	cfg.URL = "https://nas.example/dav"
	cfg.Username = "alice"
	cfg.Password = "top-secret-pass"
	cfg.HeadersJSON = `{"Authorization":"Bearer custom-secret"}`
	cfg.CustomClientIDs = map[string]string{"google_drive": "cid-1"}
	cfg.CustomClientSecrets = map[string]string{"google_drive": "oauth-secret-xyz"}
	if err := ts.daemon.Store.UpdateCloudConfig(cfg); err != nil {
		t.Fatal(err)
	}

	resp, raw := ts.do(t, http.MethodGet, "/api/settings", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/settings = %d", resp.StatusCode)
	}
	var cs struct {
		Password               string            `json:"password"`
		HasPassword            bool              `json:"hasPassword"`
		Headers                string            `json:"headers"`
		HasHeaders             bool              `json:"hasHeaders"`
		CustomClientSecrets    map[string]string `json:"customClientSecrets"`
		HasCustomClientSecrets map[string]bool   `json:"hasCustomClientSecrets"`
	}
	if err := json.Unmarshal(raw["cloudSync"], &cs); err != nil {
		t.Fatalf("unmarshal cloudSync: %v", err)
	}
	if cs.Password != "" || !cs.HasPassword {
		t.Fatalf("password not redacted properly: password=%q hasPassword=%v", cs.Password, cs.HasPassword)
	}
	if cs.Headers != "" || !cs.HasHeaders {
		t.Fatalf("headers not redacted properly: headers=%q hasHeaders=%v", cs.Headers, cs.HasHeaders)
	}
	if cs.CustomClientSecrets["google_drive"] != "" || !cs.HasCustomClientSecrets["google_drive"] {
		t.Fatalf("customClientSecrets not redacted: secrets=%+v has=%+v", cs.CustomClientSecrets, cs.HasCustomClientSecrets)
	}

	// Saving settings with blank password/headers/secrets must preserve stored secrets.
	saveResp, _ := ts.do(t, http.MethodPost, "/api/settings", map[string]any{
		"cloudSync": map[string]any{
			"url":                 "https://nas2.example/dav",
			"password":            "",
			"headers":             "",
			"customClientSecrets": map[string]string{"google_drive": ""},
		},
	})
	if saveResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/settings = %d", saveResp.StatusCode)
	}
	after, _ := ts.daemon.Store.GetCloudConfig()
	if after.URL != "https://nas2.example/dav" {
		t.Fatalf("URL not updated: %q", after.URL)
	}
	if after.Password != "top-secret-pass" || after.HeadersJSON != `{"Authorization":"Bearer custom-secret"}` ||
		after.CustomClientSecrets["google_drive"] != "oauth-secret-xyz" {
		t.Fatalf("blank save clobbered stored secrets: %+v", after)
	}

	// Explicit clearPassword clears the password.
	clearResp, _ := ts.do(t, http.MethodPost, "/api/settings", map[string]any{
		"cloudSync": map[string]any{"clearPassword": true},
	})
	if clearResp.StatusCode != http.StatusOK {
		t.Fatalf("clearPassword = %d", clearResp.StatusCode)
	}
	cleared, _ := ts.daemon.Store.GetCloudConfig()
	if cleared.Password != "" {
		t.Fatalf("expected cleared password, got %q", cleared.Password)
	}
	_ = store.CloudConfig{}
}
