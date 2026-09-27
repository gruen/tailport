package cftunnel

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// ConsolePath returns the console-capture file for a tunnel whose ownership
// sentinel logfile is logfile (see logfilePath): the same BASENAME with its
// ".log" extension swapped for ".console", joined with tailport's OWN state
// dir -- NEVER whatever directory component logfile itself carries (S2(d),
// audit finding 2). logfile normally already lives in that same state dir
// (Start only ever writes it there), so this changes nothing for a
// legitimately owned tunnel; the point is defense in depth for the one
// caller that passes in a RECOVERED, not self-generated, value: a
// Running.LogFile from Discover()/parseRunning is read off a process's own
// argv, and while parseRunning already requires that value's BASENAME to
// match the sentinel pattern for ownership, it never validated the
// directory -- so a hostile same-UID process could otherwise redirect a
// toast's ConsoleTail into reading an arbitrary file elsewhere on disk by
// crafting a --logfile whose basename merely matches. Forcing the directory
// back to stateDir() closes that off regardless of what parseRunning ever
// validates.
//
// The result can never match sentinelLogRe (which requires a ".log" suffix),
// and Discover never scans that directory anyway -- it only ever reads the
// --logfile VALUE off a process's own argv -- so a console file is invisible
// to ownership detection either way; it's purely for ConsoleTail to read.
//
// See the package doc for why this file exists at all: cloudflared's
// --logfile is a structured JSON log that can hold nothing useful for a
// fatal startup error (R1), because those go to stderr alone.
func ConsolePath(logfile string) string {
	base := strings.TrimSuffix(filepath.Base(logfile), ".log") + ".console"
	dir, err := stateDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, base)
}

// ansiEscapeRe strips ANSI/VT escape sequences: cloudflared colorizes its
// console output when it thinks it's attached to a terminal-ish stream, and a
// toast must never show raw escape bytes. It only matches CSI sequences
// (ESC '[' ... letter); SanitizeDisplay below is the backstop for anything
// this misses, e.g. an OSC sequence (ESC ']' ... BEL).
var ansiEscapeRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// SanitizeDisplay strips every C0 control byte (0x00-0x1F), DEL (0x7F), C1
// control byte (U+0080-U+009F) (S2(b), audit finding 2), and every Unicode
// FORMAT character, category Cf (N4), from s. It is the backstop for any
// string tailport did not itself construct -- cloudflared's own console
// output, or a tunnel name/hostname recovered from a foreign or hostile
// process's argv -- before it ever reaches a toast or route row rendered
// directly to the terminal: without the control-byte stripping, an embedded
// OSC 52 (clipboard-write) or bare CSI sequence ansiEscapeRe doesn't happen
// to catch could be smuggled straight into the terminal; without the Cf
// stripping, a character like U+202E RIGHT-TO-LEFT OVERRIDE or U+200B ZERO
// WIDTH SPACE -- neither a control byte, so untouched by the check above --
// could still make displayed text render as something other than what it
// literally is (the classic RLO trick used to disguise a filename's real
// extension). It is otherwise content-blind -- printable non-Cf non-ASCII
// text (e.g. an IDN hostname) passes through untouched. ValidTunnelName
// rejects Cf outright at input time, so this is defense in depth for a
// string tailport did NOT validate itself.
//
// Exported (kata p7c5) so internal/ui can run the SAME pass over a
// config.yaml-supplied tunnel name/hostname before naming it in a refusal
// toast: that value comes from a hand-editable file too, not just a
// recovered process argv, and is exactly as untrusted for display purposes.
func SanitizeDisplay(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r <= 0x1F, r == 0x7F, r >= 0x80 && r <= 0x9F:
			continue
		case unicode.Is(unicode.Cf, r):
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// consoleLevelLineRe matches one cloudflared console log line of the form
// "<timestamp> LEVEL message", e.g.
// `2026-09-27T01:31:45Z ERR Cannot determine default origin certificate path...`.
// It captures the level and the message; the timestamp is discarded -- a
// toast has no use for it.
var consoleLevelLineRe = regexp.MustCompile(`^\S+\s+(ERR|FTL)\s+(.*)$`)

// consoleLeveledLineRe matches ANY leveled cloudflared console line --
// "<timestamp> DBG|INF|WRN|ERR|FTL <msg>" -- regardless of which level. The
// priority-3 fallback below uses this to SKIP a leveled line: an ERR/FTL
// line is already handled by priority 2 (so if one exists anywhere in the
// tail, priority 3 never even runs), which leaves only routine DBG/INF/WRN
// noise for this to filter out. That matters: a real named tunnel SIGKILLed
// live (kata nc1j) left a console tail of nothing but
// "... INF Registered tunnel connection connIndex=3 connection=...", and the
// old fallback (last non-empty line, unconditionally) surfaced that as if it
// were the reason the tunnel exited -- a routine connection-registration
// line, not an error. Skipping it is safe specifically because cloudflared's
// actual fatal lines are either caught by priority 2 (ERR/FTL) or have NO
// timestamp/level at all (e.g. the real
// "error parsing tunnel ID: Error decoding origin cert: cannot decode empty
// certificate" line) and so still fall through and match here as bare.
var consoleLeveledLineRe = regexp.MustCompile(`^\S+\s+(DBG|INF|WRN|ERR|FTL)\s+.*$`)

// consoleTailMaxRunes bounds ConsoleTail's result so a toast never has to
// truncate cloudflared's raw text itself.
const consoleTailMaxRunes = 160

// ConsoleTail returns a short, single-line summary of a tunnel's console
// capture file (path from ConsolePath), for use in a toast. It reads at most
// the file's last 16 KiB (a healthy tunnel's console only grows by
// connection register/reconnect lines at info level, so the newest, most
// relevant line is always near the end), strips ANSI escapes, then tries, in
// priority order:
//
//  1. the LAST line starting "Incorrect Usage:" -- urfave's own flag-parsing
//     error, which prints to STDOUT and exits 0 (a bad argv shape tailport
//     itself is responsible for; see buildArgs);
//  2. the LAST "<ts> ERR|FTL <msg>" console line, with the timestamp and
//     level stripped, leaving just <msg> (R1: cloudflared's fatal startup
//     errors land on stderr, sometimes as the ONLY output there is);
//  3. otherwise, the last non-empty line that is NOT a leveled console line
//     (a "<ts> DBG|INF|WRN ..." line is routine noise, never presented as
//     if it were the reason a tunnel exited -- see consoleLeveledLineRe and
//     TestConsoleTail's INF/WRN-only cases, kata nc1j). This still returns a
//     BARE, unleveled line: that's essential, since cloudflared's real fatal
//     errors -- e.g. "error parsing tunnel ID: Error decoding origin cert:
//     cannot decode empty certificate" -- have no timestamp or level at all.
//
// If nothing qualifies, ConsoleTail returns "" -- callers show just the bare
// "exited" fact with no fabricated reason, never a misleading INF/WRN line
// dressed up as an explanation.
//
// The result is truncated to 160 runes with a trailing "…" and always passed
// through SanitizeDisplay (S2(b)) before it is: cloudflared's console output
// is not tailport's own text, so the returned string can never carry a raw
// ESC or other control byte, even if ansiEscapeRe's CSI-only stripping above
// missed something (an OSC 52 sequence, say). It returns "" if the file
// doesn't exist (a foreign, non-tailport-owned tunnel never had one) or
// holds nothing usable. There is deliberately NO parsing of the JSON
// --logfile here -- see the package doc.
func ConsoleTail(path string) string {
	data, err := readTail(path, 16<<10)
	if err != nil {
		return ""
	}
	text := ansiEscapeRe.ReplaceAllString(string(data), "")
	lines := strings.Split(text, "\n")

	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); strings.HasPrefix(line, "Incorrect Usage:") {
			return truncateRunes(SanitizeDisplay(line), consoleTailMaxRunes)
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if m := consoleLevelLineRe.FindStringSubmatch(line); m != nil {
			return truncateRunes(SanitizeDisplay(m[2]), consoleTailMaxRunes)
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || consoleLeveledLineRe.MatchString(line) {
			continue
		}
		return truncateRunes(SanitizeDisplay(line), consoleTailMaxRunes)
	}
	return ""
}

// readTail reads at most the final max bytes of the file at path. A missing
// file is an error (so ConsoleTail can distinguish "no file" from "empty
// file", though both currently yield the same "" result); an empty file
// returns a zero-length, error-free slice.
func readTail(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	var start int64
	if size > max {
		start = size - max
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, size-start)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// truncateRunes shortens s to at most n runes, appending "…" if it was
// longer. It operates on runes, not bytes, so a multi-byte UTF-8 sequence is
// never split in half.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
