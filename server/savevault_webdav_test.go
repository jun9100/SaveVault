// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testSaveVaultUsername = "savevault-user"
	testSaveVaultPassword = "savevault-password"
)

func newSaveVaultTestServer(t *testing.T, maxBytes int64) (*server, *httptest.Server, string) {
	t.Helper()
	dataDir := t.TempDir()
	s, err := newServerWithOptions(dataDir, testToken, maxBytes, serverOptions{
		saveVaultUsername: testSaveVaultUsername,
		saveVaultPassword: testSaveVaultPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, httptest.NewServer(s.handler()), dataDir
}

func saveVaultRequest(t *testing.T, method, target string, body []byte) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.SetBasicAuth(testSaveVaultUsername, testSaveVaultPassword)
	return request
}

func TestSaveVaultWebDAVIsDisabledWithoutSeparateCredentials(t *testing.T) {
	s, err := newServer(t.TempDir(), testToken, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s.handler())
	defer httpServer.Close()

	response, err := http.Get(httpServer.URL + "/savevault/")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled /savevault/ status = %d, want 404", response.StatusCode)
	}

	if _, err := newServerWithOptions(t.TempDir(), testToken, 1024*1024, serverOptions{
		saveVaultUsername: testSaveVaultUsername,
	}); err == nil {
		t.Fatal("server accepted a partial SaveVault credential pair")
	}
}

func TestSaveVaultWebDAVRequiresIndependentBasicAuth(t *testing.T) {
	_, httpServer, _ := newSaveVaultTestServer(t, 1024*1024)
	defer httpServer.Close()

	request, err := http.NewRequest("PROPFIND", httpServer.URL+"/savevault/", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || !strings.Contains(response.Header.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("missing credentials response = %d, WWW-Authenticate=%q", response.StatusCode, response.Header.Get("WWW-Authenticate"))
	}

	request = saveVaultRequest(t, "PROPFIND", httpServer.URL+"/savevault/", nil)
	request.SetBasicAuth(testSaveVaultUsername, "wrong-password")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong credentials response = %d, want 401", response.StatusCode)
	}
}

func TestSaveVaultWebDAVRoundTripUsesIsolatedStorage(t *testing.T) {
	_, httpServer, dataDir := newSaveVaultTestServer(t, 1024*1024)
	defer httpServer.Close()

	options := saveVaultRequest(t, http.MethodOptions, httpServer.URL+"/savevault/", nil)
	response, err := http.DefaultClient.Do(options)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent || response.Header.Get("DAV") != "1" ||
		!strings.Contains(response.Header.Get("Allow"), "MOVE") {
		t.Fatalf("OPTIONS = %d DAV=%q Allow=%q", response.StatusCode, response.Header.Get("DAV"), response.Header.Get("Allow"))
	}

	stageName := ".savevault-upload.part"
	finalName := "game__main__snap_1.zip"
	payload := []byte("complete snapshot bytes")
	put := saveVaultRequest(t, http.MethodPut, httpServer.URL+"/savevault/"+stageName, payload)
	response, err = http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("PUT status = %d", response.StatusCode)
	}

	head := saveVaultRequest(t, http.MethodHead, httpServer.URL+"/savevault/"+stageName, nil)
	response, err = http.DefaultClient.Do(head)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength != int64(len(payload)) {
		t.Fatalf("HEAD status=%d length=%d, want %d", response.StatusCode, response.ContentLength, len(payload))
	}

	move := saveVaultRequest(t, "MOVE", httpServer.URL+"/savevault/"+stageName, nil)
	move.Header.Set("Destination", httpServer.URL+"/savevault/"+finalName)
	move.Header.Set("Overwrite", "F")
	response, err = http.DefaultClient.Do(move)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("MOVE status = %d", response.StatusCode)
	}

	stored, err := os.ReadFile(filepath.Join(dataDir, "savevault", finalName))
	if err != nil || !bytes.Equal(stored, payload) {
		t.Fatalf("isolated SaveVault file = %q, err=%v", stored, err)
	}
	objects, err := os.ReadDir(filepath.Join(dataDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 0 {
		t.Fatalf("SaveVault upload wrote Eden objects: %v", objects)
	}

	propfind := saveVaultRequest(t, "PROPFIND", httpServer.URL+"/savevault/", nil)
	propfind.Header.Set("Depth", "1")
	response, err = http.DefaultClient.Do(propfind)
	if err != nil {
		t.Fatal(err)
	}
	listing, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusMultiStatus || !strings.Contains(string(listing), finalName) ||
		strings.Contains(string(listing), stageName) {
		t.Fatalf("PROPFIND status=%d listing=%s", response.StatusCode, listing)
	}

	get := saveVaultRequest(t, http.MethodGet, httpServer.URL+"/savevault/"+finalName, nil)
	response, err = http.DefaultClient.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	downloaded, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !bytes.Equal(downloaded, payload) {
		t.Fatalf("GET status=%d body=%q", response.StatusCode, downloaded)
	}

	deleteRequest := saveVaultRequest(t, http.MethodDelete, httpServer.URL+"/savevault/"+finalName, nil)
	response, err = http.DefaultClient.Do(deleteRequest)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d", response.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "savevault", finalName)); !os.IsNotExist(err) {
		t.Fatalf("DELETE left the snapshot behind: %v", err)
	}
}

func TestSaveVaultWebDAVMoveDoesNotOverwriteAndRejectsUnsafeNames(t *testing.T) {
	_, httpServer, _ := newSaveVaultTestServer(t, 4)
	defer httpServer.Close()

	for _, name := range []string{"nested/file.zip", "%2Foutside.zip", "%5Coutside.zip", "%252Foutside.zip"} {
		request := saveVaultRequest(t, http.MethodPut, httpServer.URL+"/savevault/"+name, []byte("x"))
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode < http.StatusBadRequest {
			t.Errorf("unsafe name %q accepted with HTTP %d", name, response.StatusCode)
		}
	}

	tooLarge := saveVaultRequest(t, http.MethodPut, httpServer.URL+"/savevault/.large.part", []byte("12345"))
	response, err := http.DefaultClient.Do(tooLarge)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("large PUT status = %d, want 413", response.StatusCode)
	}

	for name, value := range map[string]string{
		"existing.zip": "old",
		".new.part":    "new",
	} {
		request := saveVaultRequest(t, http.MethodPut, httpServer.URL+"/savevault/"+name, []byte(value))
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusCreated {
			t.Fatalf("PUT %s status = %d", name, response.StatusCode)
		}
	}
	move := saveVaultRequest(t, "MOVE", httpServer.URL+"/savevault/.new.part", nil)
	move.Header.Set("Destination", httpServer.URL+"/savevault/existing.zip")
	move.Header.Set("Overwrite", "F")
	response, err = http.DefaultClient.Do(move)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("MOVE collision status = %d, want 412", response.StatusCode)
	}

	get := saveVaultRequest(t, http.MethodGet, httpServer.URL+"/savevault/existing.zip", nil)
	response, err = http.DefaultClient.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	contents, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(contents) != "old" {
		t.Fatalf("MOVE collision overwrote existing data: %q", contents)
	}
}
