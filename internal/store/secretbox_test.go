package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCloudConfigSecretsEncryptedAtRest(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "savevault.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if err := s.EnsureDefaultSettings(dir, dir); err != nil {
		t.Fatalf("EnsureDefaultSettings: %v", err)
	}

	want := CloudConfig{
		Enabled:             true,
		Provider:            "webdav",
		URL:                 "https://nas.example/dav",
		Username:            "alice",
		Password:            "super-secret-password",
		HeadersJSON:         `{"Authorization":"Bearer secret-hdr"}`,
		CustomClientIDs:     map[string]string{"google_drive": "client-1"},
		CustomClientSecrets: map[string]string{"google_drive": "oauth-secret-1"},
		AccessToken:         "access-tok-123",
		RefreshToken:        "refresh-tok-456",
		UserEmail:           "alice@example.com",
	}
	if err := s.UpdateCloudConfig(want); err != nil {
		t.Fatalf("UpdateCloudConfig: %v", err)
	}

	// Raw SQLite row must store encrypted ciphertext, not plaintext secrets.
	var rawPass, rawHeaders, rawSecrets, rawAccess, rawRefresh string
	if err := s.db.QueryRow(`SELECT password, headers_json, custom_client_secrets, access_token, refresh_token FROM cloud_config WHERE id = 1`).
		Scan(&rawPass, &rawHeaders, &rawSecrets, &rawAccess, &rawRefresh); err != nil {
		t.Fatalf("raw query: %v", err)
	}
	for name, val := range map[string]string{
		"password":      rawPass,
		"headers":       rawHeaders,
		"clientSecrets": rawSecrets,
		"accessToken":   rawAccess,
		"refreshToken":  rawRefresh,
	} {
		if !strings.HasPrefix(val, secretPrefix) {
			t.Fatalf("%s stored unencrypted: %q", name, val)
		}
		if strings.Contains(val, "secret") || strings.Contains(val, "tok-") {
			t.Fatalf("%s leaks plaintext in ciphertext: %q", name, val)
		}
	}

	// Reopening the database decrypts cleanly using persisted .secret.key.
	_ = s.Close()
	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	got, err := s2.GetCloudConfig()
	if err != nil {
		t.Fatalf("GetCloudConfig: %v", err)
	}
	if got.Password != want.Password || got.HeadersJSON != want.HeadersJSON ||
		got.CustomClientSecrets["google_drive"] != "oauth-secret-1" ||
		got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken {
		t.Fatalf("decrypted mismatch: got=%+v want=%+v", got, want)
	}
}
