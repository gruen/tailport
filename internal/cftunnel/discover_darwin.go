//go:build darwin

package cftunnel

import (
	"os/exec"
	"strconv"
	"strings"
)

// enumerateCloudflared finds running cloudflared processes via `ps`, whose
// `-o command=` column gives the full argv RECONSTRUCTED as a single
// space-joined string -- unlike Linux's /proc/<pid>/cmdline, macOS's ps gives
// no NUL-delimited raw argv, so the original argument boundaries are lost the
// moment any argument contains a space. Most of tailport's own arguments can
// never contain one (ports are integers, --url/--metrics are host:port
// values), but --logfile's VALUE is a real filesystem path built from the
// state dir (see logfilePath/stateDir), which CAN contain a space if
// $HOME or $XDG_STATE_HOME does (roborev carryover, kata aprt) -- splitting
// that naively on whitespace tears the sentinel path apart, and sentinelHost
// then fails to match it, misclassifying an OWNED tunnel as foreign.
// splitPSCommand (pscommand.go) recovers that one value before falling back
// to a plain whitespace split for everything else. A foreign invocation whose
// OWN arguments contain spaces is still parsed best-effort; ownership hinges
// on our sentinel logfile, which a foreign process doesn't carry regardless.
//
// A cloudflared installed under a DIFFERENT binary name (config Binary
// override) IS enumerated here: isCloudflaredArgv0 matches against binName,
// the configured Client.binary() (roborev carryover, kata aprt).
func enumerateCloudflared(binName string) ([]procInfo, error) {
	out, err := exec.Command("ps", "-axww", "-o", "pid=,command=").Output()
	if err != nil {
		return nil, err
	}
	var res []procInfo
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimLeft(line, " ")
		if line == "" {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			continue // a bare pid with no command -- shouldn't happen
		}
		pid, err := strconv.Atoi(line[:sp])
		if err != nil {
			continue
		}
		args := splitPSCommand(strings.TrimLeft(line[sp+1:], " "))
		if len(args) == 0 || !isCloudflaredArgv0(args[0], binName) {
			continue
		}
		res = append(res, procInfo{pid: pid, args: args})
	}
	return res, nil
}
