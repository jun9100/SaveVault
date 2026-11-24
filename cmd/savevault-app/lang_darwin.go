//go:build darwin

package main

import (
	"os"
	"os/exec"
	"strings"
)

// systemLanguage inspects POSIX locale env vars first, and when launched from
// Finder/LaunchServices (where LANG is typically unset on macOS), falls back
// to macOS's AppleLanguages preference.
func systemLanguage() string {
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		v := os.Getenv(k)
		if v != "" && v != "C" && v != "POSIX" && !strings.EqualFold(v, "C.UTF-8") {
			return v
		}
	}
	out, err := exec.Command("/usr/bin/defaults", "read", "-g", "AppleLanguages").Output()
	if err != nil {
		return ""
	}
	return parseAppleLanguages(string(out))
}

func parseAppleLanguages(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed == "(" || trimmed == ")" {
			continue
		}
		trimmed = strings.TrimRight(trimmed, ",")
		trimmed = strings.Trim(trimmed, `"`)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}
