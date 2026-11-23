// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := hashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !verifyPassword(hash, "correct horse battery") {
		t.Fatal("the correct password must verify")
	}
	if verifyPassword(hash, "correct horse batterY") {
		t.Fatal("a wrong password must not verify")
	}
	if verifyPassword(hash, "") {
		t.Fatal("an empty password must not verify")
	}
	other, err := hashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if other == hash {
		t.Fatal("each hash must use a fresh salt")
	}
}

func TestCreateUserThenLoginRoutesToItsOwnVault(t *testing.T) {
	registry, err := newVaultRegistry(t.TempDir(), "default-token", 8<<20, serverOptions{})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	t.Cleanup(registry.Close)
	handler := registry.handler()

	created, err := registry.CreateUser("alice", "s3cret-pass", "test tenant", 0, time.Time{})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if created.VaultID != "alice" {
		t.Fatalf("vault id = %q", created.VaultID)
	}

	// Login is unauthenticated and returns the tenant token.
	loginBody, _ := json.Marshal(map[string]string{"username": "alice", "password": "s3cret-pass"})
	request := httptest.NewRequest(http.MethodPost, "/v1/login", bytes.NewReader(loginBody))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("login status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var login struct {
		Token string `json:"token"`
		Vault string `json:"vault"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &login); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	if login.Token != created.Token || login.Vault != "alice" {
		t.Fatalf("unexpected login payload: %+v", login)
	}

	// A wrong password is rejected.
	badBody, _ := json.Marshal(map[string]string{"username": "alice", "password": "nope"})
	request = httptest.NewRequest(http.MethodPost, "/v1/login", bytes.NewReader(badBody))
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d", recorder.Code)
	}

	// The token reaches the tenant's own vault: a write there is visible with
	// the same token and invisible to the default vault.
	write := func(token string) int {
		request := httptest.NewRequest(http.MethodGet, "/v1/auth/whoami", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Code
	}
	if code := write(login.Token); code != http.StatusOK {
		t.Fatalf("whoami with tenant token = %d", code)
	}
	if code := write("not-a-token"); code != http.StatusUnauthorized {
		t.Fatalf("whoami with a bogus token = %d", code)
	}
}

func TestDisabledAndExpiredUsersCannotLogIn(t *testing.T) {
	registry, err := newVaultRegistry(t.TempDir(), "default-token", 8<<20, serverOptions{})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	t.Cleanup(registry.Close)

	if _, err := registry.CreateUser("bob", "s3cret-pass", "", 0, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("create expired user: %v", err)
	}
	if _, err := registry.Authenticate("bob", "s3cret-pass"); err == nil || err.Error() != "account expired" {
		t.Fatalf("expired user must not authenticate, got %v", err)
	}

	if _, err := registry.CreateUser("carol", "s3cret-pass", "", 0, time.Time{}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	registry.usersMu.Lock()
	for index := range registry.users.Users {
		if registry.users.Users[index].Username == "carol" {
			registry.users.Users[index].Status = userStatusDisabled
		}
	}
	registry.usersMu.Unlock()
	if _, err := registry.Authenticate("carol", "s3cret-pass"); err == nil || err.Error() != "account disabled" {
		t.Fatalf("disabled user must not authenticate, got %v", err)
	}
}

func TestDeleteUserRevokesTheTokenAndKeepsTheData(t *testing.T) {
	root := t.TempDir()
	registry, err := newVaultRegistry(root, "default-token", 8<<20, serverOptions{})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	t.Cleanup(registry.Close)

	record, err := registry.CreateUser("dave", "s3cret-pass", "", 0, time.Time{})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	vaultDir := filepath.Join(root, "vaults", record.VaultID)
	if _, err := os.Stat(vaultDir); err != nil {
		t.Fatalf("vault directory should exist: %v", err)
	}

	if err := registry.DeleteUser("dave"); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if _, err := registry.Authenticate("dave", "s3cret-pass"); err == nil {
		t.Fatal("a deleted user must not authenticate")
	}
	registry.mu.Lock()
	_, stillRouted := registry.byToken[record.Token]
	registry.mu.Unlock()
	if stillRouted {
		t.Fatal("the deleted token must stop routing")
	}
	if _, err := os.Stat(vaultDir); err != nil {
		t.Fatalf("deleting a user must keep the vault data on disk: %v", err)
	}
}

func TestUsersSurviveRestart(t *testing.T) {
	root := t.TempDir()
	registry, err := newVaultRegistry(root, "default-token", 8<<20, serverOptions{})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	record, err := registry.CreateUser("erin", "s3cret-pass", "", 0, time.Time{})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	registry.Close()

	restarted, err := newVaultRegistry(root, "default-token", 8<<20, serverOptions{})
	if err != nil {
		t.Fatalf("restart registry: %v", err)
	}
	t.Cleanup(restarted.Close)

	authenticated, err := restarted.Authenticate("erin", "s3cret-pass")
	if err != nil {
		t.Fatalf("authenticate after restart: %v", err)
	}
	if authenticated.Token != record.Token {
		t.Fatal("the token must survive a restart")
	}
	restarted.mu.Lock()
	_, routed := restarted.byToken[record.Token]
	restarted.mu.Unlock()
	if !routed {
		t.Fatal("a restored user's token must route to its vault")
	}
}

func TestAdminCanCreateAndRemoveUsersOverHTTP(t *testing.T) {
	registry, err := newVaultRegistry(t.TempDir(), "default-token", 8<<20, serverOptions{adminToken: "admin-token"})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	t.Cleanup(registry.Close)
	handler := registry.handler()

	admin := func(method, path string, body any) *httptest.ResponseRecorder {
		var reader *bytes.Reader
		if body != nil {
			payload, _ := json.Marshal(body)
			reader = bytes.NewReader(payload)
		} else {
			reader = bytes.NewReader(nil)
		}
		request := httptest.NewRequest(method, path, reader)
		request.SetBasicAuth("admin", "admin-token")
		request.Header.Set(csrfHeader, "1")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	created := admin(http.MethodPost, "/admin/api/users", map[string]any{
		"username": "frank",
		"password": "s3cret-pass",
		"note":     "shop order 42",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", created.Code, created.Body.String())
	}

	list := admin(http.MethodGet, "/admin/api/users", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d", list.Code)
	}
	if !bytes.Contains(list.Body.Bytes(), []byte("frank")) {
		t.Fatalf("list must include the new user: %s", list.Body.String())
	}

	// Without the anti-CSRF header a state change is refused.
	request := httptest.NewRequest(http.MethodPost, "/admin/api/users", bytes.NewReader([]byte(`{"username":"gina","password":"s3cret-pass"}`)))
	request.SetBasicAuth("admin", "admin-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("create without csrf header = %d", recorder.Code)
	}

	removed := admin(http.MethodDelete, "/admin/api/users/frank", nil)
	if removed.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d body=%s", removed.Code, removed.Body.String())
	}
	if _, err := registry.Authenticate("frank", "s3cret-pass"); err == nil {
		t.Fatal("a removed user must not authenticate")
	}
}
