package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jun9100/savevault/internal/store"
)

func listSnapshotZips(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var zips []string
	for _, e := range entries {
		if strings.Contains(e.Name(), "__main__") && filepath.Ext(e.Name()) == ".zip" {
			zips = append(zips, e.Name())
		}
	}
	return zips
}

func TestCloudUploadDeferredWhilePlayingAndFlushedOnExit(t *testing.T) {
	home := t.TempDir()
	cloudDir := t.TempDir()
	saveDir := t.TempDir()
	saveFile := filepath.Join(saveDir, "slot.sav")
	if err := os.WriteFile(saveFile, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	d, err := New(Options{HomeOverride: home, DisableDiscovery: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer d.Stop()

	cfg, err := d.Store.GetCloudConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Enabled = true
	cfg.Provider = "local"
	cfg.URL = cloudDir
	if err := d.Store.UpdateCloudConfig(cfg); err != nil {
		t.Fatal(err)
	}

	game := store.Game{ID: "fh5", Name: "Forza Horizon 5", SavePath: saveDir, AutoSync: true, SaveType: "pc"}
	if err := d.Store.CreateGame(game); err != nil {
		t.Fatal(err)
	}

	// Mark game as actively running.
	t0 := time.Now()
	d.sessions.tracker.Begin("fh5", t0)

	// Take 3 automatic snapshots while playing: none should upload yet.
	for _, content := range []string{"v2", "v3", "v4"} {
		if err := os.WriteFile(saveFile, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Snapshots.Create("fh5", "", true); err != nil {
			t.Fatalf("auto snapshot %s: %v", content, err)
		}
	}
	d.uploads.Wait(2 * time.Second)
	if zips := listSnapshotZips(t, cloudDir); len(zips) != 0 {
		t.Fatalf("expected 0 cloud snapshot zips while playing, got %v", zips)
	}

	// Ending the session flushes only the latest coalesced snapshot (1 zip).
	d.sessions.tracker.Finish("fh5", t0.Add(10*time.Second))
	if !d.uploads.Wait(5 * time.Second) {
		t.Fatal("timed out waiting for flushed cloud upload")
	}
	if zips := listSnapshotZips(t, cloudDir); len(zips) != 1 {
		t.Fatalf("expected exactly 1 coalesced zip after exit, got %d: %v", len(zips), zips)
	}
}

func TestCloudUploadMinIntervalCoalescesAutoSnapshotsButNotManual(t *testing.T) {
	home := t.TempDir()
	cloudDir := t.TempDir()
	saveDir := t.TempDir()
	saveFile := filepath.Join(saveDir, "slot.sav")
	if err := os.WriteFile(saveFile, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	d, err := New(Options{HomeOverride: home, DisableDiscovery: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer d.Stop()

	cfg, err := d.Store.GetCloudConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Enabled = true
	cfg.Provider = "local"
	cfg.URL = cloudDir
	if err := d.Store.UpdateCloudConfig(cfg); err != nil {
		t.Fatal(err)
	}
	settings, err := d.Store.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	settings.CloudUploadOnExitOnly = false
	settings.CloudUploadMinInterval = 15
	if err := d.Store.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 24, 22, 0, 0, 0, time.UTC)
	d.cloudThrottle.now = func() time.Time { return now }

	game := store.Game{ID: "fh5", Name: "Forza Horizon 5", SavePath: saveDir, AutoSync: true, SaveType: "pc"}
	if err := d.Store.CreateGame(game); err != nil {
		t.Fatal(err)
	}

	countZips := func() int {
		return len(listSnapshotZips(t, cloudDir))
	}

	// First auto snapshot uploads immediately.
	if _, err := d.Snapshots.Create("fh5", "", true); err != nil {
		t.Fatal(err)
	}
	d.uploads.Wait(5 * time.Second)
	if got := countZips(); got != 1 {
		t.Fatalf("first auto snapshot zips = %d, want 1", got)
	}

	// Two more auto snapshots within 15 minutes are coalesced.
	now = now.Add(3 * time.Minute)
	_ = os.WriteFile(saveFile, []byte("v2"), 0o600)
	if _, err := d.Snapshots.Create("fh5", "", true); err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * time.Minute)
	_ = os.WriteFile(saveFile, []byte("v3"), 0o600)
	if _, err := d.Snapshots.Create("fh5", "", true); err != nil {
		t.Fatal(err)
	}
	d.uploads.Wait(2 * time.Second)
	if got := countZips(); got != 1 {
		t.Fatalf("throttled auto snapshots uploaded early: zips = %d, want 1", got)
	}

	// Manual snapshot bypasses throttle and uploads immediately.
	_ = os.WriteFile(saveFile, []byte("v4-manual"), 0o600)
	if _, err := d.Snapshots.Create("fh5", "manual checkpoint", false); err != nil {
		t.Fatal(err)
	}
	d.uploads.Wait(5 * time.Second)
	if got := countZips(); got != 2 {
		t.Fatalf("manual snapshot should upload immediately: zips = %d, want 2", got)
	}
}
