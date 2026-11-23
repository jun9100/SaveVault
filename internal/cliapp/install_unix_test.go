//go:build !windows

package cliapp

import (
	"os"
	"path/filepath"
	"testing"
)

// Someone else's `os` is not replaced by a symlink to us, and a re-install
// still refreshes our own. install.sh and the README promise this;
// `savevault install` used to remove whatever was there first.
func TestWriteAliasesLeavesSomeoneElsesCommandAlone(t *testing.T) {
	dir := t.TempDir()
	theirs := []byte("#!/bin/sh\necho not savevault\n")
	if err := os.WriteFile(filepath.Join(dir, "os"), theirs, 0o755); err != nil {
		t.Fatal(err)
	}

	written, skipped := writeAliases(dir)
	if len(skipped) != 1 || filepath.Base(skipped[0]) != "os" {
		t.Errorf("skipped = %v, want just os", skipped)
	}
	if len(written) != 1 || filepath.Base(written[0]) != "savevault" {
		t.Errorf("written = %v, want just savevault", written)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "os")); string(got) != string(theirs) {
		t.Errorf("their os was changed to %q", got)
	}
	if !aliasPointsAtUs(filepath.Join(dir, "savevault"), dir) {
		t.Error("the savevault link does not point at the installed binary")
	}

	if written, skipped := writeAliases(dir); len(written) != 1 || len(skipped) != 1 {
		t.Errorf("re-run: written=%v skipped=%v, want ours refreshed and theirs still skipped", written, skipped)
	}
}
