package cftunnel

import (
	"regexp"
	"strings"
)

// procInfo is one enumerated cloudflared process: its pid and full argument
// vector (argv, including argv[0]). The per-platform enumerateCloudflared
// (discover_linux.go / discover_darwin.go) produces these; parseRunning
// interprets them. Keeping the interpretation in this shared, pure file is what
// lets it be unit-tested on any platform.
type procInfo struct {
	pid  int
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
func (c *Client) Discover() ([]Running, error) {
	procs, err := enumerateCloudflared()
	if err != nil {
		return nil, err
	}
	out := make([]Running, 0, len(procs))
	for _, p := range procs {
		if r, ok := parseRunning(p.pid, p.args); ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// parseRunning interprets one cloudflared argv into a Running, or (,false) if
// it isn't a tunnel invocation we can map to a local port. Pure and
// exhaustively unit-tested. Ownership is decided ONLY by the --logfile
// sentinel, so a foreign cloudflared -- even one that happens to expose the
// same port -- is correctly reported with Owned=false.
func parseRunning(pid int, args []string) (Running, bool) {
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
	if containsToken(args, "run") {
		r.Mode = ModeNamed
		r.TunnelName = namedTunnelName(args)
	}
	if m, ok := flagValue(args, "--metrics"); ok {
		r.MetricsPort = parseMetricsPort(m)
	}
	if lf, ok := flagValue(args, "--logfile"); ok {
		if host, owned := sentinelHost(lf); owned {
			r.Owned = true
			// A named tunnel's hostname is unrecoverable from the cmdline
			// alone; the sentinel logfile carries it (see logfilePath). Quick
			// tunnels leave it empty here (their hostname comes from
			// /quicktunnel), so never overwrite an already-known value.
			if r.Mode == ModeNamed && r.Hostname == "" {
				r.Hostname = host
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
// trailing (.+) capture is the hostname for a named tunnel, empty for quick.
var sentinelLogRe = regexp.MustCompile(`^cftunnel-\d+(?:-(.+))?\.log$`)

// sentinelHost reports whether a --logfile value is tailport's ownership
// sentinel (see logfilePath) and, when it is, returns the named-tunnel hostname
// folded into it ("" for a quick tunnel's sentinel).
func sentinelHost(path string) (string, bool) {
	// basename without importing path/filepath's OS-specific separator quirks:
	// cloudflared writes whatever we passed, always a forward-slash path here.
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		path = path[i+1:]
	}
	m := sentinelLogRe.FindStringSubmatch(path)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// isCloudflaredArgv0 reports whether an argv[0] is the cloudflared executable,
// comparing its basename so both "cloudflared" and "/usr/bin/cloudflared"
// match. Shared by the Linux (/proc) and Darwin (ps) enumerators.
func isCloudflaredArgv0(argv0 string) bool {
	if i := strings.LastIndexAny(argv0, `/\`); i >= 0 {
		argv0 = argv0[i+1:]
	}
	return argv0 == "cloudflared"
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
