package winreg

import (
	"context"
	"testing"
	"time"
)

func TestWatchKeysNormalizeAndCancel(t *testing.T) {
	got := normalizeWatchPaths([]string{
		"HKCU/Software/SaveVault/GameA",
		"HKEY_CURRENT_USER\\Software\\SaveVault\\GameA\\",
		" ",
		"HKCU/Software/SaveVault/GameB",
	})
	if len(got) != 2 {
		t.Fatalf("expected 2 deduplicated normalized paths, got %v", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- WatchKeys(ctx, nil, func(string) {})
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WatchKeys(nil) returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WatchKeys did not return promptly after cancel")
	}
}
