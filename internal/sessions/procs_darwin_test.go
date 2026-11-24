//go:build darwin

package sessions

import (
	"os"
	"testing"
)

func TestParseDarwinPSAndRunningMatch(t *testing.T) {
	commSample := []byte(`
  101 /Applications/Ori - Will of the Wisps.app/Contents/MacOS/Ori - Will of the Wisps
  202 /Users/ron/Library/Application Support/Steam/steamapps/common/Hades/Hades.app/Contents/MacOS/Hades
  303 /Users/ron/Library/Application Support/Steam/Steam.AppBundle/Steam/Contents/MacOS/reaper
  404 /Volumes/External Drive/Games/Celeste/Celeste
`)
	cmdSample := []byte(`
  101 /Applications/Ori - Will of the Wisps.app/Contents/MacOS/Ori - Will of the Wisps -screen-fullscreen 1
  202 /Users/ron/Library/Application Support/Steam/steamapps/common/Hades/Hades.app/Contents/MacOS/Hades --windowed
  303 /Users/ron/Library/Application Support/Steam/Steam.AppBundle/Steam/Contents/MacOS/reaper SteamLaunch AppId=1145360 -- /tmp/game
  404 /Volumes/External Drive/Games/Celeste/Celeste save-slot-1
`)
	procs := parseDarwinPS(commSample, cmdSample)
	if len(procs) != 4 {
		t.Fatalf("expected 4 procs, got %d: %+v", len(procs), procs)
	}
	if procs[0].Exe != "/Applications/Ori - Will of the Wisps.app/Contents/MacOS/Ori - Will of the Wisps" {
		t.Fatalf("unexpected spaced .app exe with dash: %q", procs[0].Exe)
	}
	if procs[2].Exe != "/Users/ron/Library/Application Support/Steam/Steam.AppBundle/Steam/Contents/MacOS/reaper" {
		t.Fatalf("expected clean reaper exe, got %q", procs[2].Exe)
	}
	if procs[2].SteamAppID != "1145360" {
		t.Fatalf("expected SteamAppID 1145360, got %q", procs[2].SteamAppID)
	}
	if procs[3].Exe != "/Volumes/External Drive/Games/Celeste/Celeste" || len(procs[3].Args) != 2 || procs[3].Args[1] != "save-slot-1" {
		t.Fatalf("unexpected external-volume exe/args: %+v", procs[3])
	}

	running := Running(procs, []Target{
		{GameID: "ori", Exe: "/Applications/Ori - Will of the Wisps.app/Contents/MacOS/Ori - Will of the Wisps"},
		{GameID: "hades-dir", Dirs: []string{"/Users/ron/Library/Application Support/Steam/steamapps/common/Hades"}},
		{GameID: "hades-steam", AppID: "1145360"},
		{GameID: "celeste", Dirs: []string{"/Volumes/External Drive/Games/Celeste"}},
		{GameID: "not-running", AppID: "999999"},
	})
	if running["ori"] != 101 || running["hades-dir"] != 202 || running["hades-steam"] != 303 || running["celeste"] != 404 {
		t.Fatalf("unexpected running map: %v", running)
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
	for _, p := range procs {
		if p.PID == self {
			return
		}
	}
	t.Fatalf("expected live listProcesses() on macOS to include current PID %d (saw %d procs)", self, len(procs))
}
