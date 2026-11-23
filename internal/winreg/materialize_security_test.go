package winreg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestoreFromDirAllowedBlocksDisallowedAndDangerousKeys(t *testing.T) {
	dir := t.TempDir()
	snap := Snapshot{
		Requested: []string{`HKEY_CURRENT_USER/Software/ExampleGame`},
		Keys: []Key{
			{
				Path: `HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Run`,
				Values: []Value{
					{Name: "Evil", Type: "REG_SZ", Data: "calc.exe"},
				},
			},
			{
				Path: `HKEY_CURRENT_USER/Software/OtherApp`,
				Values: []Value{
					{Name: "Foo", Type: "REG_SZ", Data: "bar"},
				},
			},
			{
				Path: `HKEY_CURRENT_USER/Software/ExampleGame`,
				Subkeys: []Key{
					{
						Path: `HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Run`,
						Values: []Value{
							{Name: "NestedEvil", Type: "REG_SZ", Data: "cmd.exe"},
						},
					},
				},
			},
		},
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	warnings, err := RestoreFromDirAllowed(dir, []string{`HKEY_CURRENT_USER/Software/ExampleGame`})
	if err != nil {
		t.Fatalf("RestoreFromDirAllowed returned unexpected error: %v", err)
	}
	disallowedCount := 0
	for _, w := range warnings {
		if strings.Contains(w, "refused to restore disallowed registry key") {
			disallowedCount++
		}
	}
	if disallowedCount < 3 {
		t.Fatalf("expected at least 3 disallowed registry key warnings, got %d: %v", disallowedCount, warnings)
	}
}
