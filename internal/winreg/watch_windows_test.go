//go:build windows

package winreg

import (
	"context"
	"fmt"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

func TestWatchKeysRealTimeNotificationWindows(t *testing.T) {
	if !Available() {
		t.Fatal("expected Available() == true on Windows")
	}
	sub := fmt.Sprintf(`Software\SaveVaultWatchTest_%d`, time.Now().UnixNano())
	fullPath := `HKEY_CURRENT_USER\` + sub

	k, _, err := registry.CreateKey(registry.CURRENT_USER, sub, registry.ALL_ACCESS)
	if err != nil {
		t.Fatalf("create test key: %v", err)
	}
	defer func() {
		_ = k.Close()
		_ = registry.DeleteKey(registry.CURRENT_USER, sub)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	changed := make(chan string, 4)
	done := make(chan error, 1)
	go func() {
		done <- WatchKeys(ctx, []string{fullPath}, func(p string) {
			select {
			case changed <- p:
			default:
			}
		})
	}()

	// Give the watcher goroutine a brief moment to register RegNotifyChangeKeyValue.
	time.Sleep(80 * time.Millisecond)

	if err := k.SetStringValue("slot1", "chapter-2"); err != nil {
		cancel()
		t.Fatalf("SetStringValue: %v", err)
	}

	select {
	case p := <-changed:
		if p != fullPath {
			t.Fatalf("unexpected changed path %q want %q", p, fullPath)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for RegNotifyChangeKeyValue callback")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WatchKeys exit err: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WatchKeys did not exit promptly after context cancel")
	}
}
