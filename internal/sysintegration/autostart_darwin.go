//go:build darwin

package sysintegration

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
)

const (
	launchAgentLabel       = "com.jun9100.savevault"
	launchAgentFileName    = launchAgentLabel + ".plist"
	legacyLaunchAgentFile1 = "com.opensave.app.plist"
	legacyLaunchAgentFile2 = "com.opensave.agent.plist"
)

func launchAgentsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents"), nil
}

func autostartPlistPath() (string, error) {
	dir, err := launchAgentsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, launchAgentFileName), nil
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func buildLaunchAgentPlist(exe string) []byte {
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<false/>
	<key>ProcessType</key>
	<string>Interactive</string>
</dict>
</plist>
`, xmlEscape(launchAgentLabel), xmlEscape(exe), xmlEscape(StartHiddenFlag)))
}

// SetAutostart writes or removes ~/Library/LaunchAgents/com.jun9100.savevault.plist
// so SaveVault starts automatically at macOS login, cleaning up any legacy plist.
func SetAutostart(enabled bool) error {
	dir, err := launchAgentsDir()
	if err != nil {
		return err
	}
	for _, legacy := range []string{legacyLaunchAgentFile1, legacyLaunchAgentFile2} {
		_ = os.Remove(filepath.Join(dir, legacy))
	}
	plistPath := filepath.Join(dir, launchAgentFileName)
	if !enabled {
		if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove launch agent plist: %w", err)
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create LaunchAgents directory: %w", err)
	}
	return os.WriteFile(plistPath, buildLaunchAgentPlist(exe), 0o644)
}

// AutostartEnabled reports whether the macOS LaunchAgent plist is installed.
func AutostartEnabled() bool {
	path, err := autostartPlistPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}
