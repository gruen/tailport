//go:build darwin

package cftunnel

import (
	"os/exec"
	"strconv"
	"strings"
)

// enumerateCloudflared finds running cloudflared processes via `ps`, whose
// `-o command=` column gives the full argv as a space-joined string. tailport's
// own invocations never put spaces in an argument VALUE -- ports are integers
// and the sentinel logfile lives under a space-free state dir
// (~/.local/state/tailport) -- so splitting on whitespace recovers our argv
// faithfully. A foreign invocation with a space inside a path is parsed
// best-effort; ownership still hinges on our sentinel logfile, which has none.
//
// Note: a cloudflared installed under a DIFFERENT binary name (config Binary
// override) won't be enumerated here; accepted v1 limitation, as on Linux.
func enumerateCloudflared() ([]procInfo, error) {
	out, err := exec.Command("ps", "-axww", "-o", "pid=,command=").Output()
	if err != nil {
		return nil, err
	}
	var res []procInfo
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		args := fields[1:]
		if !isCloudflaredArgv0(args[0]) {
			continue
		}
		res = append(res, procInfo{pid: pid, args: args})
	}
	return res, nil
}
