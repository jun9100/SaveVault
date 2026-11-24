//go:build darwin

package sessions

import (
	"bytes"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

var darwinAppIDArg = regexp.MustCompile(`(?:^|\s)AppId=(\d+)(?:\s|$)`)

// listProcesses inspects running macOS processes via `/bin/ps -axo pid=,command=`
// without CGo dependencies, preserving `.app` bundle executable paths that
// contain spaces and extracting Steam `AppId=` tags when present.
func listProcesses() ([]Proc, error) {
	out, err := exec.Command("/bin/ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil, err
	}
	return parseDarwinPS(out), nil
}

func parseDarwinPS(raw []byte) []Proc {
	var procs []Proc
	for _, line := range bytes.Split(raw, []byte("\n")) {
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" {
			continue
		}
		sp := strings.IndexAny(trimmed, " \t")
		if sp <= 0 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(trimmed[:sp]))
		if err != nil || pid <= 1 {
			continue
		}
		cmd := strings.TrimSpace(trimmed[sp:])
		if cmd == "" {
			continue
		}
		exe, args := splitDarwinCommand(cmd)
		p := Proc{
			PID:  pid,
			Exe:  exe,
			Args: args,
		}
		if m := darwinAppIDArg.FindStringSubmatch(cmd); m != nil {
			p.SteamAppID = m[1]
		}
		procs = append(procs, p)
	}
	return procs
}

// splitDarwinCommand extracts the executable path and arguments from a `ps`
// command column. macOS app bundles frequently contain spaces in their paths
// (e.g. `/Users/x/Library/Application Support/Steam/steamapps/common/Hollow Knight/Hollow Knight.app/Contents/MacOS/Hollow Knight -screen-fullscreen 1`),
// so we detect `.app/Contents/MacOS/` boundaries before falling back to
// whitespace splitting.
func splitDarwinCommand(cmd string) (exe string, args []string) {
	const marker = ".app/Contents/MacOS/"
	if idx := strings.Index(cmd, marker); idx >= 0 {
		after := idx + len(marker)
		rest := cmd[after:]
		// Executable name inside Contents/MacOS ends before the first option
		// flag (` -`) or `--` separator, or extends to end of line if no flags.
		cut := len(rest)
		if opt := strings.Index(rest, " -"); opt >= 0 {
			cut = opt
		}
		exe = strings.TrimSpace(cmd[:after+cut])
		tail := strings.TrimSpace(rest[cut:])
		args = append(args, exe)
		if tail != "" {
			args = append(args, strings.Fields(tail)...)
		}
		return exe, args
	}

	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return "", nil
	}
	// If the path starts with `/` and contains `Application Support` (common for
	// Steam/native binaries outside .app bundles), preserve up to the first flag.
	if strings.HasPrefix(cmd, "/") && strings.Contains(cmd, "Application Support/") {
		cut := len(cmd)
		if opt := strings.Index(cmd, " -"); opt >= 0 {
			cut = opt
		}
		exe = strings.TrimSpace(cmd[:cut])
		args = append(args, exe)
		if tail := strings.TrimSpace(cmd[cut:]); tail != "" {
			args = append(args, strings.Fields(tail)...)
		}
		return exe, args
	}
	return fields[0], fields
}
