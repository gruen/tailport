package cftunnel

import (
	"os"
	"regexp"
	"strconv"
	"strings"
)

// procInfo is one enumerated cloudflared process: its pid, its real UID, and
// full argument vector (argv, including argv[0]). The per-platform
// enumerateCloudflared (discover_linux.go / discover_darwin.go) produces
// these; parseRunning interprets them. Keeping the interpretation in this
// shared, pure file is what lets it be unit-tested on any platform.
//
// uid is the process's real UID (S2(a), audit finding 2): ownership now
// requires it to equal os.Getuid(), in addition to the sentinel+port match
// below -- a same-named sentinel started by a DIFFERENT user is never ours,
// no matter how well it matches otherwise.
type procInfo struct {
	pid  int
	uid  int
	args []string
}

// Discover returns every cloudflared TUNNEL process currently running that
// tailport can map to a local port -- both tailport-owned tunnels (Owned true,
// carrying our --logfile sentinel) and foreign ones (Owned false). It is the
// live source of truth for tunnel state: it re-derives everything from the
// process table on each call, so it round-trips across a tailport restart with
// nothing persisted. Processes it cannot map to a local port (no --url, e.g. a
// config-file ingress tunnel) are omitted -- out of scope for the per-port
// model.
//
// Matching argv[0] against c.binary() (not JUST a hardcoded "cloudflared")
// matters: Start launches whatever binary the config points at (a custom
// path, or a wrapper script under a different name), so Discover must also
// look for that SAME name -- otherwise a renamed binary/wrapper is
// undiscoverable and untoggleable even though tailport itself started it
// (roborev carryover, kata aprt). isCloudflaredArgv0 ALSO always accepts the
// literal "cloudflared" regardless of c.binary() [R2]: a wrapper script
// configured as Binary that `exec`s into the real cloudflared REPLACES the
// process image in place (same pid), so the wrapper's own name never appears
// in the post-exec argv0 -- only whatever the exec'd process presents itself
// as, normally "cloudflared". Without that, a wrapper-launched tunnel would
// vanish from discovery (and become double-startable) the moment the wrapper
// actually execs (kata nc1j, 2z0v(a)). This alone never grants ownership:
// parseRunning still requires the --logfile sentinel AND its embedded port to
// match (see sentinelHost), so widening the argv0 match doesn't let a foreign
// "cloudflared"-argv0 process masquerade as ours.
func (c *Client) Discover() ([]Running, error) {
	procs, err := enumerateCloudflared(c.binary())
	if err != nil {
		return nil, err
	}
	out := make([]Running, 0, len(procs))
	for _, p := range procs {
		if r, ok := parseRunning(p.pid, p.uid, p.args); ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// parseRunning interprets one cloudflared argv into a Running, or (,false) if
// it isn't a tunnel invocation we can map to a local port. Pure and
// exhaustively unit-tested. Ownership is decided by:
//   - the --logfile sentinel basename AND its embedded port matching THIS
//     process's own --url port (roborev carryover, kata aprt) -- without that
//     port check, a foreign cloudflared whose --logfile happens to collide
//     with our basename pattern for a DIFFERENT port would be misclassified
//     as owned;
//   - uid matching os.Getuid() (S2(a), audit finding 2) -- a process started
//     by a DIFFERENT user can never be ours, however well its sentinel
//     matches otherwise;
//   - for a NAMED tunnel, the recovered hostname and tunnel name both passing
//     ValidHostname/ValidTunnelName (S2(c)) -- tailport itself never writes a
//     sentinel whose embedded hostname or positional name would fail either
//     check, so one that does was crafted by something else (a hostile,
//     same-UID process trying to get a bogus string -- a leading dot, an
//     escape sequence -- rendered as if it were a genuine tailport-owned
//     tunnel's hostname/name) and must never be trusted as owned.
//
// So a foreign cloudflared -- even one that happens to expose the same port,
// run as a different user, or carry a coincidentally-matching but invalid
// sentinel -- is correctly reported with Owned=false.
func parseRunning(pid, uid int, args []string) (Running, bool) {
	if !containsToken(args, "tunnel") {
		return Running{}, false
	}
	rawURL, ok := flagValue(args, "--url")
	if !ok {
		return Running{}, false
	}
	port, ok := parseLocalPort(rawURL)
	if !ok {
		return Running{}, false
	}
	r := Running{PID: pid, Port: port, Mode: ModeQuick}
	var rawName string
	if containsToken(args, "run") {
		r.Mode = ModeNamed
		rawName = namedTunnelName(args)
		// sanitizeDisplay (S2(b)) is defense in depth on top of the
		// ValidTunnelName ownership gate below: it guarantees this value can
		// never carry a raw ESC/OSC byte into a toast or route row even in a
		// context that didn't go through that gate.
		r.TunnelName = sanitizeDisplay(rawName)
	}
	if m, ok := flagValue(args, "--metrics"); ok {
		r.MetricsPort = parseMetricsPort(m)
	}
	if lf, ok := flagValue(args, "--logfile"); ok {
		if host, sentinelOK := sentinelHost(lf, port); sentinelOK && uid == os.Getuid() {
			owned := true
			if r.Mode == ModeNamed && (!ValidHostname(host) || !ValidTunnelName(rawName)) {
				owned = false
			}
			if owned {
				r.Owned = true
				// The --logfile value itself (not just its derived hostname)
				// is what ConsolePath needs to find this tunnel's
				// console-capture file after a tailport restart
				// re-Discover()s it.
				r.LogFile = lf
				// A named tunnel's hostname is unrecoverable from the cmdline
				// alone; the sentinel logfile carries it (see logfilePath).
				// Quick tunnels leave it empty here (their hostname comes
				// from /quicktunnel), so never overwrite an already-known
				// value.
				if r.Mode == ModeNamed && r.Hostname == "" {
					r.Hostname = host
				}
			}
		}
	}
	return r, true
}

// sentinelLogRe matches tailport's per-port log basename, optionally carrying a
// named tunnel's hostname (cftunnel-<port>-<hostname>.log). Matching the
// basename (not the full path) keeps ownership detection robust even if
// $XDG_STATE_HOME changed between the session that started the tunnel and the
// one discovering it -- nobody else names a log cftunnel-<port>*.log. The
// leading (\d+) capture is the embedded port (sentinelHost verifies it
// matches the process's own --url port before trusting the match); the
// trailing (.+) capture is the hostname for a named tunnel, empty for quick.
var sentinelLogRe = regexp.MustCompile(`^cftunnel-(\d+)(?:-(.+))?\.log$`)

// sentinelHost reports whether a --logfile value is tailport's ownership
// sentinel for wantPort (see logfilePath) and, when it is, returns the
// named-tunnel hostname folded into it ("" for a quick tunnel's sentinel). The
// embedded port MUST equal wantPort -- the caller's own --url port -- so a
// foreign process whose --logfile happens to match our basename pattern for a
// DIFFERENT port is never mistaken for ours (roborev carryover, kata aprt).
func sentinelHost(path string, wantPort int) (string, bool) {
	// basename without importing path/filepath's OS-specific separator quirks:
	// cloudflared writes whatever we passed, always a forward-slash path here.
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		path = path[i+1:]
	}
	m := sentinelLogRe.FindStringSubmatch(path)
	if m == nil {
		return "", false
	}
	port, err := strconv.Atoi(m[1])
	if err != nil || port != wantPort {
		return "", false
	}
	return m[2], true
}

// isCloudflaredArgv0 reports whether an argv[0] is EITHER the configured
// cloudflared executable wantBin (Client.binary(): "cloudflared" by default,
// or a config Binary override) OR the literal "cloudflared", comparing
// basenames so both a bare name and an absolute path to it match on either
// side (e.g. "cloudflared" and "/usr/bin/cloudflared", or "my-wrapper" and
// "/opt/bin/my-wrapper"). Shared by the Linux (/proc) and Darwin (ps)
// enumerators.
//
// The "cloudflared" fallback [R2, kata nc1j/2z0v(a)] exists for an
// exec-wrapper Binary override: a wrapper script that does
// `exec -a cloudflared /path/to/real/cloudflared "$@"` REPLACES its own
// process image with the real binary's, in the SAME pid -- so the argv0
// visible afterward is whatever the wrapper exec'd AS (normally
// "cloudflared"), never the wrapper's own name. Matching wantBin ALONE would
// make that process undiscoverable the moment the wrapper actually execs,
// even though tailport itself started it (TestDiscoverFindsExecWrappedCloudflared
// pins this). This never grants ownership by itself -- callers still require
// parseRunning's --logfile sentinel AND matching embedded port (see
// sentinelHost) -- so accepting the wider literal doesn't let an unrelated
// "cloudflared"-argv0 process pass as ours; it only widens which processes
// are even considered.
func isCloudflaredArgv0(argv0, wantBin string) bool {
	basename := func(s string) string {
		if i := strings.LastIndexAny(s, `/\`); i >= 0 {
			return s[i+1:]
		}
		return s
	}
	argv0 = basename(argv0)
	return argv0 == basename(wantBin) || argv0 == "cloudflared"
}

// containsToken reports whether tok appears as a standalone argv element (a
// subcommand like "tunnel"/"run"), not as a flag value or substring.
func containsToken(args []string, tok string) bool {
	for _, a := range args {
		if a == tok {
			return true
		}
	}
	return false
}

// valueFlags are the cloudflared flags tailport passes that consume the
// following argv element as their value. It lets namedTunnelName skip a flag's
// value when hunting for the trailing positional tunnel name, so a name is
// never confused with (say) the --logfile path.
var valueFlags = map[string]bool{
	"--url":     true,
	"--metrics": true,
	"--logfile": true,
	"--config":  true,
	"--token":   true,
}

// flagValue returns the value of the named flag, supporting both
// "--flag value" and "--flag=value" forms. Only the first occurrence is
// returned.
func flagValue(args []string, name string) (string, bool) {
	prefix := name + "="
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == name:
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		case strings.HasPrefix(args[i], prefix):
			return strings.TrimPrefix(args[i], prefix), true
		}
	}
	return "", false
}

// namedTunnelName recovers the positional tunnel name from a `tunnel run ...`
// argv. The name is a positional that follows the "run" subcommand, so the
// search is bounded to indices AFTER "run" -- which excludes argv[0] (the
// cloudflared binary itself) and the "tunnel"/"run" tokens. tailport always
// puts the name last (see buildArgs), so this walks from the end and returns
// the first bare positional, skipping flags and any value a value-flag
// consumes. Returns "" when no name is present. Best-effort for a foreign
// invocation whose flag shape we don't control.
func namedTunnelName(args []string) string {
	runIdx := -1
	for i, a := range args {
		if a == "run" {
			runIdx = i
			break
		}
	}
	if runIdx < 0 {
		return ""
	}
	// Mark indices that are values consumed by a preceding space-separated
	// value flag, so we don't mistake a path/URL for the name.
	consumed := make([]bool, len(args))
	for i := 0; i < len(args); i++ {
		if valueFlags[args[i]] && i+1 < len(args) {
			consumed[i+1] = true
		}
	}
	for i := len(args) - 1; i > runIdx; i-- {
		a := args[i]
		if consumed[i] || strings.HasPrefix(a, "-") {
			continue
		}
		return a
	}
	return ""
}
