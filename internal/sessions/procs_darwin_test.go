//go:build darwin

package sessions

import (
	"os"
	"testing"
)

func TestParseDarwinPSAndRunningMatch(t *testing.T) {
	sample := []byte(`
  101 /Applications/Hollow Knight.app/Contents/MacOS/Hollow Knight -screen-fullscreen 1
  202 /Users/ron/Library/Application Support/Steam/steamapps/common/Hades/Hades.app/Contents/MacOS/Hades --windowed
  303 /Users/ron/Library/Application Support/Steam/Steam.AppBundle/Steam/Contents/MacOS/reaper SteamLaunch AppId=1145360 -- /tmp/game
  404 /usr/local/bin/retroarch -L core.dylib
`)
	procs := parseDarwinPS(sample)
	if len(procs) != 4 {
		t.Fatalf("expected 4 procs, got %d: %+v", len(procs), procs)
	}
	if procs[0].Exe != "/Applications/Hollow Knight.app/Contents/MacOS/Hollow Knight" {
		t.Fatalf("unexpected .app exe: %q", procs[0].Exe)
	}
	if procs[2].SteamAppID != "1145360" {
		t.Fatalf("expected SteamAppID 1145360, got %q", procs[2].SteamAppID)
	}

	running := Running(procs, []Target{
		{GameID: "hk", Exe: "/Applications/Hollow Knight.app/Contents/MacOS/Hollow Knight"},
		{GameID: "hades-dir", Dirs: []string{"/Users/ron/Library/Application Support/Steam/steamapps/common/Hades"}},
		{GameID: "hades-steam", AppID: "1145360"},
		{GameID: "not-running", AppID: "999999"},
	})
	if running["hk"] != 101 {
		t.Fatalf("expected hk pid 101, got %v", running)
	}
	if running["hades-dir"] != 202 {
		t.Fatalf("expected hades-dir pid 202, got %v", running)
	}
	if running["hades-steam"] != 303 {
		t.Fatalf("expected hades-steam pid 303, got %v", running)
	}
	if _, ok := running["not-running"]; ok {
		t.Fatalf("unexpected match for not-running: %v", running)
	}
}

func TestListProcessesDarwinFindsSelf(t *testing.T) {
	procs, err := listProcesses()
	if err != nil {
		t.Fatalf("listProcesses: %v", err)
	}
	self := os.Getpid()
	found := false
	for _, p := range procs {
		if p.PID == self {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected live listProcesses() on macOS to include current PID %d (saw %d procs)", self, len(procs))
	}
}
