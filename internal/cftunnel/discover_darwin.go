//go:build darwin

package cftunnel

import (
	"os/exec"
	"regexp"
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
// then fails to match it, misclassifying an OWNED tunnel as foreign. splitPSCommand
// below recovers that one value before falling back to a plain whitespace
// split for everything else. A foreign invocation whose OWN arguments contain
// spaces is still parsed best-effort; ownership hinges on our sentinel
// logfile, which a foreign process doesn't carry regardless.
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

// logfileValueRe locates a space-form "--logfile VALUE" in a raw ps command
// line and captures VALUE, tolerating internal spaces. It anchors to the end
// of the line: the non-greedy capture grows only as far as it must for the
// remainder to be either another " --flag..." or pure trailing whitespace, so
// it stops at the FIRST real flag boundary (a space immediately followed by
// "--") rather than the first space of any kind -- an intermediate space
// inside the path (e.g. "/Users/Jane Doe/...") doesn't satisfy that
// alternation and is correctly folded into the captured value. A path
// containing the literal substring " --" defeats this; pathological, not
// supported (matches this file's pre-existing space caveat for foreign argv).
var logfileValueRe = regexp.MustCompile(`--logfile\s+(.+?)(\s+--\S.*|\s*)$`)

// splitPSCommand splits one ps `command=` value into an argv-like slice,
// protecting a space-form --logfile VALUE (see logfileValueRe) from being torn
// apart by a naive whitespace split before falling back to strings.Fields for
// everything else.
func splitPSCommand(command string) []string {
	protected := command
	if m := logfileValueRe.FindStringSubmatchIndex(command); m != nil {
		val := command[m[2]:m[3]]
		if strings.Contains(val, " ") {
			// Hide the value's internal spaces from Fields with a byte that can
			// never appear in ps text output, then restore it below.
			protected = command[:m[2]] + strings.ReplaceAll(val, " ", "\x00") + command[m[3]:]
		}
	}
	fields := strings.Fields(protected)
	for i, f := range fields {
		if strings.Contains(f, "\x00") {
			fields[i] = strings.ReplaceAll(f, "\x00", " ")
		}
	}
	return fields
}
