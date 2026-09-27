//go:build darwin

package cftunnel

import (
	"os/exec"
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
//
// The `uid=` column (S2(a), audit finding 2) is what lets ownership also
// require the process's real UID to equal os.Getuid() -- macOS's ps has no
// /proc-style out-of-band UID lookup, so it has to come from ps itself.
// splitPidUidCommand (pscommand.go, build-tag-free so it's unit-tested on
// Linux CI too) peels the pid and uid columns off before splitPSCommand ever
// sees the command string.
func enumerateCloudflared(binName string) ([]procInfo, error) {
	out, err := exec.Command("ps", "-axww", "-o", "pid=,uid=,command=").Output()
	if err != nil {
		return nil, err
	}
	var res []procInfo
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimLeft(line, " ")
		if line == "" {
			continue
		}
		pid, uid, command, ok := splitPidUidCommand(line)
		if !ok {
			continue // a bare pid with no uid/command -- shouldn't happen
		}
		args := splitPSCommand(command)
		if len(args) == 0 || !isCloudflaredArgv0(args[0], binName) {
			continue
		}
		res = append(res, procInfo{pid: pid, uid: uid, args: args})
	}
	return res, nil
}
