// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDashboardServesPairingQRCodeLibrary(t *testing.T) {
	_, httpServer := newAdminTestServer(t, t.TempDir())
	defer httpServer.Close()

	response, err := http.Get(httpServer.URL + "/admin/vendor/qrcode-generator.js")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("qrcode vendor status = %d", response.StatusCode)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.Contains(strings.ToLower(contentType), "javascript") {
		t.Fatalf("qrcode vendor Content-Type = %q, want JavaScript", contentType)
	}
	if !strings.Contains(string(body), "var qrcode = function") {
		t.Fatal("qrcode vendor body does not look like qrcode-generator")
	}

	response, err = http.Get(httpServer.URL + "/admin/")
	if err != nil {
		t.Fatal(err)
	}
	page, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("dashboard shell status = %d", response.StatusCode)
	}
	if !strings.Contains(string(page), `vendor/qrcode-generator.js`) {
		t.Fatal("dashboard shell does not load the qrcode vendor script")
	}
}
