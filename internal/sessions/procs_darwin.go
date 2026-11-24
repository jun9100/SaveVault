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

// listProcesses inspects running macOS processes using `/bin/ps`:
//   - `pid=,comm=` yields each process's exact untruncated executable path
//     with spaces intact (unlike Linux's 15-char comm).
//   - `pid=,command=` yields the full command line for arguments and Steam
//     `AppId=` launcher tags.
func listProcesses() ([]Proc, error) {
	commOut, err := exec.Command("/bin/ps", "-axo", "pid=,comm=").Output()
	if err != nil {
		return nil, err
	}
	cmdOut, err := exec.Command("/bin/ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil, err
	}
	return parseDarwinPS(commOut, cmdOut), nil
}

func parsePidColumn(raw []byte) map[int]string {
	out := make(map[int]string)
	for _, line := range bytes.Split(raw, []byte("\n")) {
		trimmed := strings.TrimSpace(string(line))
		sp := strings.IndexAny(trimmed, " \t")
		if sp <= 0 {
			continue
		}
		pid, err := strconv.Atoi(trimmed[:sp])
		if err != nil || pid <= 1 {
			continue
		}
		val := strings.TrimSpace(trimmed[sp:])
		if val != "" {
			out[pid] = val
		}
	}
	return out
}

func parseDarwinPS(commRaw, cmdRaw []byte) []Proc {
	comms := parsePidColumn(commRaw)
	var procs []Proc
	for _, line := range bytes.Split(cmdRaw, []byte("\n")) {
		trimmed := strings.TrimSpace(string(line))
		sp := strings.IndexAny(trimmed, " \t")
		if sp <= 0 {
			continue
		}
		pid, err := strconv.Atoi(trimmed[:sp])
		if err != nil || pid <= 1 {
			continue
		}
		cmd := strings.TrimSpace(trimmed[sp:])
		if cmd == "" {
			continue
		}
		exe := comms[pid]
		var args []string
		if exe != "" && strings.HasPrefix(cmd, exe) {
			tail := strings.TrimSpace(cmd[len(exe):])
			args = append(args, exe)
			if tail != "" {
				args = append(args, strings.Fields(tail)...)
			}
		} else {
			fields := strings.Fields(cmd)
			if exe == "" {
				exe = fields[0]
			}
			args = fields
		}
		p := Proc{PID: pid, Exe: exe, Args: args}
		if m := darwinAppIDArg.FindStringSubmatch(cmd); m != nil {
			p.SteamAppID = m[1]
		}
		procs = append(procs, p)
	}
	return procs
}
