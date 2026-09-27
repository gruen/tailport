// Command fakecloudflared is a minimal, from-scratch stand-in for the real
// `cloudflared` binary (kata nc1j, W4 -- design-v033-final.md's Tier 2). It
// exists so tailport's internal/cftunnel can be exercised end to end --
// argv shape, console capture, health polling, exit detection -- WITHOUT an
// account, a network call, or the real binary at all.
//
// It deliberately does NOT try to be a faithful cloudflared reimplementation:
// it only reproduces the exact surface tailport's own Client depends on --
//
//   - `version --short` (Client.Detect)
//   - the SAME "Incorrect Usage" rejection real cloudflared gives when a
//     tunnel-level flag (--metrics/--logfile/--no-autoupdate) is placed AFTER
//     `run` (see cftunnel.buildArgs's doc comment) -- this is what lets
//     TestFakeMatchesRealPlacement compare the fake against the real binary
//     on the SAME argv and expect the SAME verdict;
//   - a `--metrics 127.0.0.1:PORT` HTTP server serving /ready and
//     /quicktunnel, matching Client.Health's expectations;
//   - graceful exit on SIGTERM/SIGINT (what Client.Stop sends), and two
//     failure knobs (FAKE_CF_FAIL_AFTER) so a caller can simulate cloudflared
//     dying after startupGrace has already passed.
//
// It never touches the network beyond the loopback address it's told to
// listen on, never calls out to Cloudflare, and never implements
// `tunnel list` (tailport doesn't use it -- see cftunnel's package doc,
// audit item 9).
//
// Every knob is an environment variable so both the CI-safe lifecycle test
// (fake_lifecycle_test.go) and the opt-in e2e script
// (scripts/e2e-cftunnel-fake.sh) can drive it without any flags of their own
// beyond the ones tailport itself already passes:
//
//	FAKE_CF_ARGV_LOG      path to append one JSON line per invocation's argv to.
//	FAKE_CF_READY_AFTER   duration before /ready flips to 200 (default 500ms).
//	FAKE_CF_FAIL_AFTER    "<duration>:<message>" -- after duration, print
//	                      "<RFC3339> ERR <message>" to stderr and exit 1.
//	FAKE_CF_MAX_LIFETIME  safety-net duration after which the process exits on
//	                      its own even with nothing else telling it to
//	                      (default 120s) -- so a crashed test can't leak it.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// fakeVersion is what `version --short` prints. The "-fake" suffix keeps it
// visibly distinct from any real cloudflared version string in logs/output.
const fakeVersion = "2026.9.1-fake"

// tunnelLevelFlags are the flags real cloudflared only registers at the
// `tunnel` level, not under `run` -- see cftunnel.buildArgs's doc comment.
// Placing any of them after `run` is exactly the bug nc1j fixed (the named
// argv was broken since v0.2.1), so the fake must reject it identically to
// the real binary for TestFakeMatchesRealPlacement to mean anything.
var tunnelLevelFlags = []string{"--metrics", "--logfile", "--no-autoupdate"}

func main() {
	args := os.Args[1:]
	logArgv(args)

	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "fakecloudflared: no arguments given")
		os.Exit(1)
	}

	if args[0] == "version" {
		fmt.Println(fakeVersion)
		os.Exit(0)
	}

	runIdx := indexOfToken(args, "run")
	if runIdx >= 0 {
		for _, flag := range tunnelLevelFlags {
			if idx := flagIndex(args, flag); idx > runIdx {
				name := strings.TrimPrefix(flag, "-") // "--metrics" -> "-metrics"
				fmt.Printf("Incorrect Usage: flag provided but not defined: %s\n\n"+
					"NAME:\n   cloudflared tunnel run - Proxy a local web server by running a Cloudflare Tunnel\n", name)
				os.Exit(0)
			}
		}
	}

	if indexOfToken(args, "--help") >= 0 || indexOfToken(args, "-h") >= 0 {
		fmt.Println("NAME:\n   cloudflared tunnel - Manage and run Cloudflare Tunnels")
		os.Exit(0)
	}

	if args[0] != "tunnel" {
		fmt.Fprintf(os.Stderr, "fakecloudflared: unsupported invocation %q\n", args)
		os.Exit(1)
	}

	rawURL, ok := flagValue(args, "--url")
	if !ok {
		fmt.Fprintln(os.Stderr, "fakecloudflared: missing --url (tunnel list / config-file ingress is unsupported by design)")
		os.Exit(1)
	}
	port, ok := portFromURL(rawURL)
	if !ok {
		fmt.Fprintf(os.Stderr, "fakecloudflared: cannot parse port from --url %q\n", rawURL)
		os.Exit(1)
	}
	named := runIdx >= 0

	stderrf("INF Fake cloudflared starting: mode=%s port=%d pid=%d", modeString(named), port, os.Getpid())

	start := time.Now()
	readyAfter := envDuration("FAKE_CF_READY_AFTER", 500*time.Millisecond)
	const quicktunnelAfter = 1 * time.Second

	if addr, ok := flagValue(args, "--metrics"); ok {
		serveMetrics(addr, named, port, start, readyAfter, quicktunnelAfter)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	var failCh <-chan time.Time
	var failMsg string
	if raw := os.Getenv("FAKE_CF_FAIL_AFTER"); raw != "" {
		if d, msg, ok := parseFailAfter(raw); ok {
			failCh = time.After(d)
			failMsg = msg
		}
	}

	maxLifetime := envDuration("FAKE_CF_MAX_LIFETIME", 120*time.Second)

	select {
	case <-sigCh:
		os.Exit(0)
	case <-failCh:
		stderrf("ERR %s", failMsg)
		os.Exit(1)
	case <-time.After(maxLifetime):
		// Safety net only: a crashed test/script must never leave this running
		// forever. Not itself a failure, so exit 0.
		os.Exit(0)
	}
}

// serveMetrics starts cloudflared's --metrics HTTP server in the background,
// bound to exactly the loopback address it was told to (never a wildcard) --
// the fake never touches the network beyond that.
func serveMetrics(addr string, named bool, port int, start time.Time, readyAfter, quicktunnelAfter time.Duration) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		if time.Since(start) < readyAfter {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"readyConnections":0}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"readyConnections":4}`))
	})
	mux.HandleFunc("/quicktunnel", func(w http.ResponseWriter, r *http.Request) {
		hostname := ""
		if !named && time.Since(start) >= quicktunnelAfter {
			hostname = fmt.Sprintf("fake-%d.trycloudflare.com", port)
		}
		body, _ := json.Marshal(struct {
			Hostname string `json:"hostname"`
		}{hostname})
		_, _ = w.Write(body)
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		// A bind failure here (e.g. the loopback port raced away between
		// Client.Start reserving it and this exec) has no good recovery for a
		// test fake; best-effort only -- ListenAndServe's error is otherwise
		// silently dropped, same as a real cloudflared crash would leave no
		// working /ready either.
		_ = srv.ListenAndServe()
	}()
}

// modeString renders named/quick for the startup log line only.
func modeString(named bool) string {
	if named {
		return "named"
	}
	return "quick"
}

// logArgv appends one JSON line (the argv this invocation was called with,
// NOT including argv[0]) to $FAKE_CF_ARGV_LOG, if set. Every invocation logs
// -- version probes, --help probes, and the long-running run alike -- so a
// caller that wants just the real run's argv should look for the line
// containing "run" (named) or lacking it (quick), typically the LAST line
// once any earlier detect/help probes have already exited.
func logArgv(args []string) {
	path := os.Getenv("FAKE_CF_ARGV_LOG")
	if path == "" {
		return
	}
	line, err := json.Marshal(args)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// stderrf writes one "<RFC3339> <rest>" line to stderr, matching real
// cloudflared's own console line shape (see cftunnel/console.go's
// consoleLevelLineRe) closely enough for ConsoleTail to find it.
func stderrf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, a...))
}

// envDuration parses the named environment variable as a duration, falling
// back to def when unset or unparseable.
func envDuration(name string, def time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return def
	}
	return d
}

// parseFailAfter splits FAKE_CF_FAIL_AFTER's "<duration>:<message>" shape.
func parseFailAfter(raw string) (time.Duration, string, bool) {
	parts := strings.SplitN(raw, ":", 2)
	d, err := time.ParseDuration(parts[0])
	if err != nil {
		return 0, "", false
	}
	msg := ""
	if len(parts) == 2 {
		msg = parts[1]
	}
	return d, msg, true
}

// indexOfToken returns the index of the first standalone argv element equal
// to tok, or -1. Mirrors cftunnel's containsToken/argIndex, kept independent
// here on purpose -- the fake must not depend on cftunnel's own code to stay
// an honest, from-scratch stand-in.
func indexOfToken(args []string, tok string) int {
	for i, a := range args {
		if a == tok {
			return i
		}
	}
	return -1
}

// flagIndex returns the index of the ARGV ELEMENT that sets flag name
// (space or "=" form), or -1. Used to test flag position relative to `run`.
func flagIndex(args []string, name string) int {
	prefix := name + "="
	for i, a := range args {
		if a == name || strings.HasPrefix(a, prefix) {
			return i
		}
	}
	return -1
}

// flagValue returns the value of the named flag, space or "=" form.
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

// portFromURL extracts the port from a --url value like
// "http://localhost:3000".
func portFromURL(raw string) (int, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return 0, false
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil || p <= 0 {
		return 0, false
	}
	return p, true
}
