//go:build darwin

package sysintegration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMacOSLaunchAgentAutostartRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if AutostartEnabled() {
		t.Fatal("expected autostart to be disabled in fresh HOME")
	}

	// Place a legacy plist and verify SetAutostart cleans it up.
	agentsDir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(agentsDir, legacyLaunchAgentFile1)
	if err := os.WriteFile(legacyPath, []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SetAutostart(true); err != nil {
		t.Fatalf("SetAutostart(true): %v", err)
	}
	if !AutostartEnabled() {
		t.Fatal("expected AutostartEnabled() == true after enabling")
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("expected legacy plist to be removed, stat err=%v", err)
	}

	raw, err := os.ReadFile(filepath.Join(agentsDir, launchAgentFileName))
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	content := string(raw)
	if !strings.Contains(content, "<string>"+launchAgentLabel+"</string>") {
		t.Fatalf("missing label in plist:\n%s", content)
	}
	if !strings.Contains(content, "<string>"+StartHiddenFlag+"</string>") {
		t.Fatalf("missing start-hidden flag in plist:\n%s", content)
	}

	// Verify XML escaping on paths containing '&' and spaces.
	escaped := string(buildLaunchAgentPlist("/Applications/Save & Vault.app/Contents/MacOS/SaveVault"))
	if !strings.Contains(escaped, "Save &amp; Vault.app") {
		t.Fatalf("expected XML escaping in plist:\n%s", escaped)
	}

	if err := SetAutostart(false); err != nil {
		t.Fatalf("SetAutostart(false): %v", err)
	}
	if AutostartEnabled() {
		t.Fatal("expected AutostartEnabled() == false after disabling")
	}
}
