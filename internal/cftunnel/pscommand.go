// pscommand.go holds the pure argv-splitting logic behind macOS's ps-based
// discovery (discover_darwin.go's enumerateCloudflared): turning a
// space-joined `ps -o command=` string back into individual arguments while
// protecting a --config or --logfile path that itself contains a space. It
// deliberately carries NO build tag, unlike discover_darwin.go, so this
// parsing logic is compiled and unit-tested on Linux CI too, not only
// exercised indirectly on a darwin runner (kata nc1j).
package cftunnel

import (
	"regexp"
	"strconv"
	"strings"
)

// flagValueBoundaryRe builds a regex that locates a space-form "--flag
// VALUE" occurrence of flag in a raw ps command line and captures VALUE,
// tolerating internal spaces. It anchors to the end of the line: the
// non-greedy capture grows only as far as it must for the remainder to be
// either another " --flag..." or pure trailing whitespace, so it stops at
// the FIRST real flag boundary (a space immediately followed by "--") after
// flag's OWN value, rather than the first space of any kind -- an
// intermediate space inside the path (e.g. "/Users/Jane Doe/...") doesn't
// satisfy that alternation and is correctly folded into the captured value.
// A path containing the literal substring " --" defeats this; pathological,
// not supported (matches this file's pre-existing space caveat for foreign
// argv).
//
// This depends on flag's value being directly followed by another "--"
// flag, or the end of the line, with nothing else in between -- buildArgs
// guarantees that adjacency for both flags this is used with (kata nc1j):
// --config's value is always immediately followed by --url (quick) or
// --metrics (named), and --logfile's is always immediately followed by
// --no-autoupdate. See splitPSCommand.
func flagValueBoundaryRe(flag string) *regexp.Regexp {
	return regexp.MustCompile(regexp.QuoteMeta(flag) + `\s+(.+?)(\s+--\S.*|\s*)$`)
}

// configValueRe and logfileValueRe are the two flags whose value can contain
// a space (both derive from the state dir; see stateDir, configFilePath,
// logfilePath) and therefore need splitPSCommand's boundary protection.
// logfileValueRe is also referenced by name in comments elsewhere in this
// package.
var (
	configValueRe  = flagValueBoundaryRe("--config")
	logfileValueRe = flagValueBoundaryRe("--logfile")
)

// splitPSCommand splits one ps `command=` value into an argv-like slice,
// protecting a space-form --config VALUE and --logfile VALUE (see
// flagValueBoundaryRe) from being torn apart by a naive whitespace split
// before falling back to strings.Fields for everything else. The two flags
// are protected independently -- each regex locates only its OWN value,
// bounded by whichever "--flag" immediately follows it in buildArgs's fixed
// shape -- so protecting one can never swallow or corrupt the other's.
func splitPSCommand(command string) []string {
	protected := command
	for _, re := range [...]*regexp.Regexp{configValueRe, logfileValueRe} {
		if m := re.FindStringSubmatchIndex(protected); m != nil {
			val := protected[m[2]:m[3]]
			if strings.Contains(val, " ") {
				// Hide the value's internal spaces from Fields with a byte that
				// can never appear in ps text output, then restore it below.
				protected = protected[:m[2]] + strings.ReplaceAll(val, " ", "\x00") + protected[m[3]:]
			}
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

// splitPidUidCommand splits one `ps -axww -o pid=,uid=,command=` line into
// (pid, uid, command), tolerating ps's whitespace-padded alignment of the two
// numeric columns (S2(a), audit finding 2: ownership now additionally
// requires the process's real UID to equal os.Getuid(), so Darwin's
// enumerator needs a uid column alongside pid and command -- see
// discover_darwin.go). Kept build-tag-free, like the rest of this file, so
// this parsing logic is unit-tested on Linux CI too, not only exercised
// indirectly on a darwin runner.
func splitPidUidCommand(line string) (pid, uid int, command string, ok bool) {
	line = strings.TrimLeft(line, " ")
	sp := strings.IndexByte(line, ' ')
	if sp < 0 {
		return 0, 0, "", false
	}
	pid, err := strconv.Atoi(line[:sp])
	if err != nil {
		return 0, 0, "", false
	}
	rest := strings.TrimLeft(line[sp+1:], " ")
	sp2 := strings.IndexByte(rest, ' ')
	if sp2 < 0 {
		return 0, 0, "", false
	}
	uid, err = strconv.Atoi(rest[:sp2])
	if err != nil {
		return 0, 0, "", false
	}
	return pid, uid, strings.TrimLeft(rest[sp2+1:], " "), true
}
