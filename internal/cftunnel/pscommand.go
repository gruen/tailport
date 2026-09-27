// pscommand.go holds the pure argv-splitting logic behind macOS's ps-based
// discovery (discover_darwin.go's enumerateCloudflared): turning a
// space-joined `ps -o command=` string back into individual arguments while
// protecting a --logfile path that itself contains a space. It deliberately
// carries NO build tag, unlike discover_darwin.go, so this parsing logic is
// compiled and unit-tested on Linux CI too, not only exercised indirectly on
// a darwin runner (kata nc1j).
package cftunnel

import (
	"regexp"
	"strings"
)

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
//
// This depends on --logfile's value being directly followed by --no-autoupdate
// (or, for a bare foreign invocation, nothing) with no other flag in between --
// buildArgs guarantees that adjacency for BOTH the quick and named argv (the
// named argv's tunnel-level flags -- --metrics, --logfile, --no-autoupdate --
// all sit before `run`; see buildArgs), so the " --" boundary this regex looks
// for is always the next real flag, never torn out of the middle of a path.
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
