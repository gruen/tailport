package cftunnel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBuildArgs(t *testing.T) {
	quick := buildArgs(Spec{Port: 3000, Mode: ModeQuick, MetricsPort: 20941}, "/state/cftunnel-3000.log", "/state/cftunnel-config.yml")
	wantQuick := []string{
		"tunnel",
		"--config", "/state/cftunnel-config.yml",
		"--url", "http://localhost:3000",
		"--metrics", "127.0.0.1:20941",
		"--logfile", "/state/cftunnel-3000.log",
		"--no-autoupdate",
	}
	if !reflect.DeepEqual(quick, wantQuick) {
		t.Errorf("quick args:\n got %q\nwant %q", quick, wantQuick)
	}

	// Named order (kata nc1j): the tunnel-level flags (--config, --metrics,
	// --logfile, --no-autoupdate) come BEFORE `run` -- real cloudflared
	// 2026.9.1 rejects them after it ("Incorrect Usage: flag provided but not
	// defined: -metrics", exit 0). --url and the trailing tunnel name come
	// after `run`. This pins the exact shape README.md's "Tunnelling to the
	// public internet (Cloudflare Tunnel)" section documents for a named
	// tunnel. See TestNamedArgvAcceptedByRealCloudflared for the real-binary
	// proof, including that --config is rejected after `run` too.
	named := buildArgs(Spec{Port: 8080, Mode: ModeNamed, TunnelName: "web", MetricsPort: 20942}, "/state/cftunnel-8080.log", "/state/cftunnel-config.yml")
	wantNamed := []string{
		"tunnel",
		"--config", "/state/cftunnel-config.yml",
		"--metrics", "127.0.0.1:20942",
		"--logfile", "/state/cftunnel-8080.log",
		"--no-autoupdate",
		"run",
		"--url", "http://localhost:8080",
		"web", // trailing positional -- namedTunnelName relies on this
	}
	if !reflect.DeepEqual(named, wantNamed) {
		t.Errorf("named args:\n got %q\nwant %q", named, wantNamed)
	}
}

// TestBuildArgsNamedFlagPlacement pins the shape TestNamedArgvAcceptedByRealCloudflared
// depends on: every tunnel-level flag (including --config, added for the
// hermetic-config fix, kata nc1j) must sit before `run`, --url must sit
// after it, and the tunnel name must be the last element.
func TestBuildArgsNamedFlagPlacement(t *testing.T) {
	args := buildArgs(Spec{Port: 8080, Mode: ModeNamed, TunnelName: "web", MetricsPort: 20942}, "/state/cftunnel-8080.log", "/state/cftunnel-config.yml")
	runIdx := argIndex(args, "run")
	if runIdx < 0 {
		t.Fatal(`named argv must contain "run"`)
	}
	for _, flag := range []string{"--config", "--metrics", "--logfile", "--no-autoupdate"} {
		i := argIndex(args, flag)
		if i < 0 {
			t.Errorf("named argv is missing tunnel-level flag %q", flag)
			continue
		}
		if i > runIdx {
			t.Errorf("tunnel-level flag %q at index %d must come before \"run\" at index %d -- real cloudflared rejects it after run", flag, i, runIdx)
		}
	}
	if urlIdx := argIndex(args, "--url"); urlIdx < runIdx {
		t.Errorf("--url at index %d must come after \"run\" at index %d", urlIdx, runIdx)
	}
	if got := args[len(args)-1]; got != "web" {
		t.Errorf("tunnel name must be the last argv element, got %q", got)
	}
}

// argIndex returns the index of the first exact match of tok in args, or -1.
func argIndex(args []string, tok string) int {
	for i, a := range args {
		if a == tok {
			return i
		}
	}
	return -1
}

func TestParseRunning(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want Running
		ok   bool
	}{
		{
			name: "quick owned",
			args: []string{"cloudflared", "tunnel",
				"--url", "http://localhost:3000",
				"--metrics", "127.0.0.1:20941",
				"--logfile", "/home/u/.local/state/tailport/cftunnel-3000.log",
				"--no-autoupdate"},
			want: Running{Port: 3000, Mode: ModeQuick, MetricsPort: 20941, Owned: true, LogFile: "/home/u/.local/state/tailport/cftunnel-3000.log"},
			ok:   true,
		},
		{
			// Current (kata nc1j, hermetic-config fix) argv shape: --config
			// first, right after "tunnel", ahead of --url. namedTunnelName's
			// valueFlags already treats --config as value-consuming, so this
			// must not disturb hostname/name recovery.
			name: "quick owned, with --config",
			args: []string{"cloudflared", "tunnel",
				"--config", "/home/u/.local/state/tailport/cftunnel-config.yml",
				"--url", "http://localhost:3001",
				"--metrics", "127.0.0.1:20943",
				"--logfile", "/home/u/.local/state/tailport/cftunnel-3001.log",
				"--no-autoupdate"},
			want: Running{Port: 3001, Mode: ModeQuick, MetricsPort: 20943, Owned: true, LogFile: "/home/u/.local/state/tailport/cftunnel-3001.log"},
			ok:   true,
		},
		{
			// Current (kata nc1j) argv shape: tunnel-level flags before `run`.
			name: "named owned, new flag order (hostname recovered from logfile)",
			args: []string{"/usr/bin/cloudflared", "tunnel",
				"--metrics", "127.0.0.1:20942",
				"--logfile", "/home/u/.local/state/tailport/cftunnel-8080-app.example.com.log",
				"--no-autoupdate", "run",
				"--url", "http://localhost:8080", "web"},
			want: Running{Port: 8080, Mode: ModeNamed, TunnelName: "web", MetricsPort: 20942, Hostname: "app.example.com", Owned: true,
				LogFile: "/home/u/.local/state/tailport/cftunnel-8080-app.example.com.log"},
			ok: true,
		},
		{
			// Current (kata nc1j, hermetic-config fix) named argv shape,
			// including --config: --config/--metrics/--logfile/--no-autoupdate
			// all before `run`, --url and the name after it. namedTunnelName
			// must still recover "web" as the trailing positional, not
			// --config's own value.
			name: "named owned, with --config (hostname recovered from logfile)",
			args: []string{"/usr/bin/cloudflared", "tunnel",
				"--config", "/home/u/.local/state/tailport/cftunnel-config.yml",
				"--metrics", "127.0.0.1:20944",
				"--logfile", "/home/u/.local/state/tailport/cftunnel-8081-app2.example.com.log",
				"--no-autoupdate", "run",
				"--url", "http://localhost:8081", "web"},
			want: Running{Port: 8081, Mode: ModeNamed, TunnelName: "web", MetricsPort: 20944, Hostname: "app2.example.com", Owned: true,
				LogFile: "/home/u/.local/state/tailport/cftunnel-8081-app2.example.com.log"},
			ok: true,
		},
		{
			// parseRunning is order-independent (it scans for tokens/flags by
			// name, not position). This pins that a tunnel started by a
			// PRE-nc1j tailport build -- whose --metrics/--logfile/--no-autoupdate
			// sat AFTER `run`, an argv real cloudflared actually rejects -- is
			// still discoverable and re-toggleable across the upgrade rather than
			// silently dropping out. It carries no --config at all (that flag
			// didn't exist yet), which must also still be discoverable.
			name: "named owned, old (pre-nc1j) flag order",
			args: []string{"/usr/bin/cloudflared", "tunnel", "run",
				"--url", "http://localhost:8080",
				"--metrics", "127.0.0.1:20942",
				"--logfile", "/home/u/.local/state/tailport/cftunnel-8080-app.example.com.log",
				"--no-autoupdate", "web"},
			want: Running{Port: 8080, Mode: ModeNamed, TunnelName: "web", MetricsPort: 20942, Hostname: "app.example.com", Owned: true,
				LogFile: "/home/u/.local/state/tailport/cftunnel-8080-app.example.com.log"},
			ok: true,
		},
		{
			name: "foreign quick (no sentinel logfile)",
			args: []string{"cloudflared", "tunnel", "--url", "http://localhost:5000"},
			want: Running{Port: 5000, Mode: ModeQuick, Owned: false},
			ok:   true,
		},
		{
			name: "foreign with a non-tailport logfile",
			args: []string{"cloudflared", "tunnel", "--url", "http://localhost:5000", "--logfile", "/var/log/cloudflared.log"},
			want: Running{Port: 5000, Mode: ModeQuick, Owned: false},
			ok:   true,
		},
		{
			name: "equals-form flags",
			args: []string{"cloudflared", "tunnel",
				"--url=http://localhost:9000",
				"--metrics=127.0.0.1:20950",
				"--logfile=/x/cftunnel-9000.log"},
			want: Running{Port: 9000, Mode: ModeQuick, MetricsPort: 20950, Owned: true, LogFile: "/x/cftunnel-9000.log"},
			ok:   true,
		},
		{
			name: "foreign: sentinel basename matches but its embedded port is for a DIFFERENT port than --url",
			args: []string{"cloudflared", "tunnel",
				"--url", "http://localhost:5000",
				"--logfile", "/home/u/.local/state/tailport/cftunnel-3000.log"},
			want: Running{Port: 5000, Mode: ModeQuick, Owned: false},
			ok:   true,
		},
		{
			name: "not a tunnel invocation",
			args: []string{"cloudflared", "update"},
			ok:   false,
		},
		{
			name: "tunnel but no --url (config-file ingress -> unmappable)",
			args: []string{"cloudflared", "tunnel", "run", "web"},
			ok:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseRunning(42, tt.args)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			tt.want.PID = 42
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("\n got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestFlagValue(t *testing.T) {
	args := []string{"tunnel", "--url", "http://localhost:1", "--metrics=127.0.0.1:2", "--no-autoupdate"}
	if v, ok := flagValue(args, "--url"); !ok || v != "http://localhost:1" {
		t.Errorf("--url space form: %q %v", v, ok)
	}
	if v, ok := flagValue(args, "--metrics"); !ok || v != "127.0.0.1:2" {
		t.Errorf("--metrics equals form: %q %v", v, ok)
	}
	if _, ok := flagValue(args, "--logfile"); ok {
		t.Error("--logfile should be absent")
	}
	// a trailing flag with no following value must not panic or falsely match
	if _, ok := flagValue([]string{"tunnel", "--url"}, "--url"); ok {
		t.Error("dangling --url should report absent value")
	}
}

func TestNamedTunnelName(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"cloudflared", "tunnel", "run", "--url", "http://localhost:80", "--logfile", "/x/cftunnel-80.log", "web"}, "web"},
		{[]string{"cloudflared", "tunnel", "run", "my-tunnel"}, "my-tunnel"},
		{[]string{"cloudflared", "tunnel", "run", "--metrics", "127.0.0.1:1", "prod-api"}, "prod-api"},
		{[]string{"cloudflared", "tunnel", "run"}, ""}, // no name present
	}
	for _, c := range cases {
		if got := namedTunnelName(c.args); got != c.want {
			t.Errorf("namedTunnelName(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}

func TestSentinelHost(t *testing.T) {
	type yesCase struct {
		path string
		port int
		host string
	}
	// (path, wantPort) -> expected hostname; all of these are ours.
	yes := []yesCase{
		{"/home/u/.local/state/tailport/cftunnel-3000.log", 3000, ""},
		{"cftunnel-22.log", 22, ""},
		{`C:\x\cftunnel-8080.log`, 8080, ""},
		{"/home/u/.local/state/tailport/cftunnel-8080-app.example.com.log", 8080, "app.example.com"},
		{"cftunnel-443-api.corp.internal.log", 443, "api.corp.internal"},
	}
	for _, c := range yes {
		host, ok := sentinelHost(c.path, c.port)
		if !ok {
			t.Errorf("sentinelHost(%q, %d) ok = false, want true", c.path, c.port)
			continue
		}
		if host != c.host {
			t.Errorf("sentinelHost(%q, %d) host = %q, want %q", c.path, c.port, host, c.host)
		}
	}
	type noCase struct {
		path string
		port int
	}
	no := []noCase{
		{"/var/log/cloudflared.log", 3000},
		{"/x/cftunnel.log", 3000},        // no port
		{"/x/cftunnel-3000.txt", 3000},   // wrong ext
		{"/x/mycftunnel-3000.log", 3000}, // prefix garbage
		{"", 3000},
		// roborev carryover (kata aprt): the basename matches, but its embedded
		// port is for a DIFFERENT local port than the one being asked about --
		// must not be reported as ours.
		{"/x/cftunnel-3000.log", 5000},
	}
	for _, c := range no {
		if _, ok := sentinelHost(c.path, c.port); ok {
			t.Errorf("sentinelHost(%q, %d) ok = true, want false", c.path, c.port)
		}
	}
}

func TestParseLocalPort(t *testing.T) {
	if p, ok := parseLocalPort("http://localhost:3000"); !ok || p != 3000 {
		t.Errorf("localhost: %d %v", p, ok)
	}
	if p, ok := parseLocalPort("http://127.0.0.1:8080"); !ok || p != 8080 {
		t.Errorf("127.0.0.1: %d %v", p, ok)
	}
	if _, ok := parseLocalPort("http://localhost"); ok {
		t.Error("missing port should fail")
	}
}

func TestParseMetricsPort(t *testing.T) {
	if parseMetricsPort("127.0.0.1:20941") != 20941 {
		t.Error("127.0.0.1 host")
	}
	if parseMetricsPort("localhost:1") != 1 {
		t.Error("localhost host")
	}
	if parseMetricsPort("garbage") != 0 {
		t.Error("garbage should be 0")
	}
}

func TestHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ready":
			// the real endpoint carries an extra connectorId field, which our
			// parser must tolerate.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":200,"readyConnections":2,"connectorId":"abc"}`))
		case "/quicktunnel":
			_, _ = w.Write([]byte(`{"hostname":"foo-bar-baz.trycloudflare.com"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	port := serverPort(t, srv)
	c := &Client{HTTPClient: srv.Client()}
	h := c.Health(context.Background(), port)
	if !h.Ready || h.ReadyConnections != 2 {
		t.Errorf("ready: %+v", h)
	}
	if h.Hostname != "foo-bar-baz.trycloudflare.com" {
		t.Errorf("hostname: %q", h.Hostname)
	}
}

func TestHealthNotReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ready" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":503,"readyConnections":0}`))
			return
		}
		w.WriteHeader(http.StatusNotFound) // no /quicktunnel yet
	}))
	defer srv.Close()

	c := &Client{HTTPClient: srv.Client()}
	h := c.Health(context.Background(), serverPort(t, srv))
	if h.Ready {
		t.Error("should not be ready on 503")
	}
	if h.Hostname != "" {
		t.Errorf("hostname should be empty, got %q", h.Hostname)
	}
}

func TestHealthZeroMetricsPort(t *testing.T) {
	c := &Client{}
	if h := c.Health(context.Background(), 0); h.Ready || h.Hostname != "" {
		t.Errorf("zero metrics port should short-circuit: %+v", h)
	}
}

// fakeCloudflaredBin writes an executable shell script at <tempdir>/cloudflared
// with the given body and returns its path. Its argv[0] basename is exactly
// "cloudflared", so both discovery (isCloudflaredArgv0) and Start/Stop see it
// exactly like a real binary, without actually running cloudflared.
func fakeCloudflaredBin(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cloudflared")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeProc is a spawned fake-cloudflared process plus a reap-based exit
// signal: done is CLOSED once its Wait() returns, so it's safe for any number
// of readers (assertExited, assertStillRunning, and the t.Cleanup teardown)
// to observe it. This -- not a signal(pid, 0) liveness probe -- is the
// correct way to tell whether OUR OWN child actually exited: a child that has
// been signalled but not yet reaped is a ZOMBIE, and zombies still answer
// signal-0 existence checks as "alive" until reaped, which would make any
// probe based on that always see the process as running.
type fakeProc struct {
	pid  int
	done chan struct{}
	err  error // valid once done is closed
}

// spawnFakeCloudflared starts a long-lived (sleep 30) fake cloudflared with the
// given extra argv, so the platform's real enumerateCloudflared() (Discover)
// picks it up exactly like a genuine tunnel process. Registers a t.Cleanup
// that force-kills and reaps it.
//
// It execs /bin/sh directly (Path) with an explicit, spoofed argv[0] (Args[0])
// of "cloudflared" rather than running a "#!/bin/sh" SCRIPT named cloudflared:
// the kernel's shebang handling rewrites a script's own argv[0] to the
// interpreter ("sh"), which would make isCloudflaredArgv0 miss it entirely.
// Spoofing argv[0] on a directly-exec'd interpreter is unaffected by that
// rewrite, matching how a real cloudflared's own argv[0] shows up verbatim.
// The extra "-c", "sleep 30; true" tokens before args are harmless: every
// parser here (containsToken/flagValue) scans for exact matches and ignores
// unrelated tokens wherever they fall. "; true" matters in its own right:
// without a second command, /bin/sh tail-call-optimizes a lone simple command
// by exec()-ing it directly in place of the shell, which would replace our
// spoofed argv[0] with sleep's own ("sleep 30") the moment it starts.
func spawnFakeCloudflared(t *testing.T, args ...string) *fakeProc {
	t.Helper()
	return spawnFakeProcess(t, "cloudflared", args...)
}

// spawnFakeProcess is spawnFakeCloudflared generalized to an arbitrary spoofed
// argv[0], so a test can simulate a cloudflared installed under a custom
// Binary override (or, unlabelled, a wholly unrelated process for PID-reuse
// scenarios).
func spawnFakeProcess(t *testing.T, argv0 string, args ...string) *fakeProc {
	t.Helper()
	cmd := &exec.Cmd{
		Path: "/bin/sh",
		Args: append([]string{argv0, "-c", "sleep 30; true"}, args...),
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting fake process %q: %v", argv0, err)
	}
	fp := &fakeProc{pid: cmd.Process.Pid, done: make(chan struct{})}
	go func() {
		fp.err = cmd.Wait()
		close(fp.done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-fp.done // closed channel: safe even if a test already drained it
	})
	return fp
}

// assertExited waits up to timeout for fp to be reaped, proving Stop actually
// signalled it (not merely that it returned nil).
func assertExited(t *testing.T, fp *fakeProc, timeout time.Duration) {
	t.Helper()
	select {
	case <-fp.done:
	case <-time.After(timeout):
		t.Errorf("pid %d did not exit within %v of Stop", fp.pid, timeout)
	}
}

// assertStillRunning confirms fp was NOT touched: it must not exit within a
// short grace window. fp is sleeping on its own for 30s, so any exit within
// that window can only mean something signalled it.
func assertStillRunning(t *testing.T, fp *fakeProc, grace time.Duration) {
	t.Helper()
	select {
	case <-fp.done:
		t.Errorf("pid %d exited (err=%v) -- a refused Stop must never touch the process", fp.pid, fp.err)
	case <-time.After(grace):
	}
}

// TestStopRevalidatesOwnership is the regression test for the HIGH-severity
// roborev carryover (kata aprt): Stop must re-check, from the LIVE process
// table, that pid is STILL genuinely our tunnel for port immediately before
// signalling -- never trust a caller-cached pid on faith (PID reuse could
// otherwise hand the signal to an unrelated process).
func TestStopRevalidatesOwnership(t *testing.T) {
	c := &Client{}

	t.Run("genuinely owned -> signalled and exits", func(t *testing.T) {
		logfile := filepath.Join(t.TempDir(), "cftunnel-3000.log")
		fp := spawnFakeCloudflared(t, "tunnel", "--url", "http://localhost:3000", "--logfile", logfile)
		if err := c.Stop(fp.pid, 3000); err != nil {
			t.Fatalf("Stop on a genuinely owned tunnel: %v", err)
		}
		assertExited(t, fp, 2*time.Second)
	})

	t.Run("foreign cloudflared (no sentinel logfile) -> refuses to signal", func(t *testing.T) {
		fp := spawnFakeCloudflared(t, "tunnel", "--url", "http://localhost:4000")
		if err := c.Stop(fp.pid, 4000); err == nil {
			t.Fatal("Stop must refuse a foreign (unowned) cloudflared")
		}
		assertStillRunning(t, fp, 300*time.Millisecond)
	})

	t.Run("owned tunnel but for a DIFFERENT port -> refuses to signal", func(t *testing.T) {
		// Simulates the caller handing Stop a pid+port pair that's gone stale
		// (e.g. reused by a NEW owned tunnel on a different port): the pid is a
		// real tailport-owned cloudflared, just not for the port being asked
		// about, so it must be refused exactly like a foreign process.
		logfile := filepath.Join(t.TempDir(), "cftunnel-3000.log")
		fp := spawnFakeCloudflared(t, "tunnel", "--url", "http://localhost:3000", "--logfile", logfile)
		if err := c.Stop(fp.pid, 9999); err == nil {
			t.Fatal("Stop must refuse when pid's actual tunnel port differs from the caller's expected port")
		}
		assertStillRunning(t, fp, 300*time.Millisecond)
	})

	t.Run("already gone -> treated as success", func(t *testing.T) {
		logfile := filepath.Join(t.TempDir(), "cftunnel-5000.log")
		fp := spawnFakeCloudflared(t, "tunnel", "--url", "http://localhost:5000", "--logfile", logfile)
		if p, err := os.FindProcess(fp.pid); err == nil {
			_ = p.Kill()
		}
		<-fp.done // wait for the real reap BEFORE Stop, so Discover() can no longer see it
		if err := c.Stop(fp.pid, 5000); err != nil {
			t.Errorf("an already-gone tunnel should be treated as a successful stop, got %v", err)
		}
	})

	if err := (&Client{}).Stop(0, 3000); err == nil {
		t.Error("Stop(0, ...) should reject an invalid pid")
	}
}

// TestDiscoverHonorsBinaryOverride is the regression test for the MEDIUM
// roborev carryover (kata aprt): discovery used to match argv[0] against the
// hardcoded literal "cloudflared", so a cloudflared installed under a
// DIFFERENT binary name (a config Binary override, or a wrapper script) was
// undiscoverable and untoggleable even though tailport itself would be the one
// that started it. Discover must match against the CONFIGURED binary name
// instead.
func TestDiscoverHonorsBinaryOverride(t *testing.T) {
	logfile := filepath.Join(t.TempDir(), "cftunnel-3000.log")
	fp := spawnFakeProcess(t, "cloudflared-custom", "tunnel", "--url", "http://localhost:3000", "--logfile", logfile)

	// A default Client (binary() == "cloudflared") must NOT see it: argv[0]
	// doesn't match the name it would have launched.
	def := &Client{}
	running, err := def.Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, r := range running {
		if r.PID == fp.pid {
			t.Fatalf("a default Client should not discover a %q-named process", "cloudflared-custom")
		}
	}

	// A Client configured with the matching Binary override MUST see it, and
	// recognize it as owned.
	custom := &Client{Binary: "cloudflared-custom"}
	running, err = custom.Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	found := false
	for _, r := range running {
		if r.PID == fp.pid {
			found = true
			if !r.Owned || r.Port != 3000 {
				t.Errorf("discovered custom-binary process = %+v, want Owned with Port 3000", r)
			}
		}
	}
	if !found {
		t.Fatal("a Client configured with the matching Binary override should discover it")
	}

	// Stop, likewise, must be able to tear it down using that same override.
	if err := custom.Stop(fp.pid, 3000); err != nil {
		t.Fatalf("Stop on the custom-binary tunnel: %v", err)
	}
	assertExited(t, fp, 2*time.Second)
}

// TestDiscoverFindsExecWrappedCloudflared is the regression test for R2 (kata
// nc1j, 2z0v(a)): a wrapper script configured as Binary that `exec`s into the
// real cloudflared REPLACES its own process image in place (same pid), so
// its post-exec argv0 is whatever it exec'd AS -- "cloudflared" here, never
// the wrapper's own name. Before isCloudflaredArgv0 accepted that literal
// fallback, this tunnel would vanish from Discover the moment the wrapper
// actually exec'd, even though tailport itself started it -- and a second
// `o` would spawn a SECOND cloudflared for the same port. This test fails on
// pre-nc1j code.
func TestDiscoverFindsExecWrappedCloudflared(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on $PATH")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	wrapper := filepath.Join(t.TempDir(), "my-wrapper")
	script := "#!/bin/bash\nexec -a cloudflared /bin/sh -c 'sleep 30; true' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	c := &Client{Binary: wrapper}
	r, err := c.Start(Spec{Port: 3020})
	if err != nil {
		t.Fatalf("Start via exec-wrapper: %v", err)
	}
	t.Cleanup(func() {
		if p, e := os.FindProcess(r.PID); e == nil {
			_ = p.Kill()
		}
	})

	running, err := c.Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var found *Running
	for i := range running {
		if running[i].PID == r.PID {
			found = &running[i]
		}
	}
	if found == nil {
		t.Fatalf("Discover did not find the exec-wrapped cloudflared (pid %d) among %+v", r.PID, running)
	}
	if !found.Owned || found.Port != 3020 {
		t.Errorf("discovered = %+v, want Owned with Port 3020", *found)
	}

	if err := c.Stop(r.PID, 3020); err != nil {
		t.Fatalf("Stop on the exec-wrapped tunnel: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for Alive(r.PID) {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d did not exit within 2s of Stop", r.PID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestStopRefusesUndiscoverableLiveProcess is the defense-in-depth regression
// test for R2 (kata nc1j): a pid Discover() can't map to a recognized
// cloudflared invocation (here, argv0 "strace" -- never matched by
// isCloudflaredArgv0, on purpose: matching the --logfile sentinel ALONE would
// let a debugger/strace wrapper or sudo parent masquerade as ours) must never
// be silently treated as "already gone" by Stop while it's still alive.
func TestStopRefusesUndiscoverableLiveProcess(t *testing.T) {
	logfile := filepath.Join(t.TempDir(), "cftunnel-3021.log")
	fp := spawnFakeProcess(t, "strace", "tunnel", "--url", "http://localhost:3021", "--logfile", logfile)

	c := &Client{}
	if err := c.Stop(fp.pid, 3021); err == nil {
		t.Fatal("Stop must refuse a live but undiscoverable process, not silently report success")
	}
	assertStillRunning(t, fp, 300*time.Millisecond)
}

// TestStartLivenessCheck is the regression test for the MEDIUM roborev
// carryover (kata aprt): Start must not report success when cloudflared
// exits moments after cmd.Start (a bad invocation), and must not add a
// meaningfully longer delay when it stays up.
func TestStartLivenessCheck(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	t.Run("exits immediately -> Start reports an error", func(t *testing.T) {
		c := &Client{Binary: fakeCloudflaredBin(t, "exit 1\n")}
		if _, err := c.Start(Spec{Port: 3000}); err == nil {
			t.Fatal("Start should surface an error when cloudflared exits immediately")
		}
	})

	t.Run("stays up -> Start succeeds", func(t *testing.T) {
		c := &Client{Binary: fakeCloudflaredBin(t, "sleep 5\n")}
		r, err := c.Start(Spec{Port: 3001})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer func() {
			if p, e := os.FindProcess(r.PID); e == nil {
				_ = p.Kill()
			}
		}()
		if r.PID <= 0 {
			t.Error("expected a positive pid")
		}
	})
}

// TestValidTunnelName pins ValidTunnelName's acceptance rules (kata nc1j):
// non-empty, no whitespace or control characters, and no leading '-' (which
// cloudflared's own flag parser would otherwise try to consume as a flag
// rather than the positional tunnel name).
func TestValidTunnelName(t *testing.T) {
	yes := []string{"web", "prod-api", "a", "tunnel_1", "app.example.com", "192-168"}
	for _, s := range yes {
		if !ValidTunnelName(s) {
			t.Errorf("ValidTunnelName(%q) = false, want true", s)
		}
	}
	no := []string{
		"",              // empty
		" ",             // whitespace only
		"web tunnel",    // internal space
		"-web",          // leading '-'
		"\tweb",         // leading control/whitespace
		"web\n",         // trailing control
		"web\x00tunnel", // embedded NUL (control)
	}
	for _, s := range no {
		if ValidTunnelName(s) {
			t.Errorf("ValidTunnelName(%q) = true, want false", s)
		}
	}
}

// TestStartNamedReturnsHostname is the regression test for audit item 3
// (kata nc1j): Start must hand back spec.Hostname on the Running it returns
// for a named tunnel, so the caller can flash/display the URL immediately
// instead of waiting for the next poll to re-derive it from the sentinel
// logfile name.
func TestStartNamedReturnsHostname(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".cloudflared"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".cloudflared", "cert.pem"), []byte("fake-cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("TUNNEL_ORIGIN_CERT", "") // don't let an ambient override win
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	c := &Client{Binary: fakeCloudflaredBin(t, "sleep 5\n")}
	r, err := c.Start(Spec{Port: 3003, Mode: ModeNamed, TunnelName: "web", Hostname: "app.example.com"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if p, e := os.FindProcess(r.PID); e == nil {
			_ = p.Kill()
		}
	}()
	if r.Hostname != "app.example.com" {
		t.Errorf("Hostname = %q, want %q", r.Hostname, "app.example.com")
	}
	if r.TunnelName != "web" {
		t.Errorf("TunnelName = %q, want %q", r.TunnelName, "web")
	}
}

// TestNamedArgvAcceptedByRealCloudflared is the Tier 1 real-binary conformance
// probe (design-v033-final.md, kata nc1j): it proves the CURRENT named argv
// order actually parses under a real cloudflared, and that the OLD (broken)
// order still would not -- so a regression back to the old order fails this
// test, not just a fixture. It is --help-only and carries no positional
// tunnel reference, so `run` just errors on a missing tunnel rather than ever
// reaching the network (see design-v033-final.md's Tier 1 note). Per
// coordinator amendment A1, it skips unless cloudflared is not just present
// but actually usable, so a broken stand-in first on $PATH (as the CI
// simulation puts there) makes it skip, not fail.
func TestNamedArgvAcceptedByRealCloudflared(t *testing.T) {
	bin, err := exec.LookPath("cloudflared")
	if err != nil {
		t.Skip("cloudflared not on $PATH")
	}
	{
		ctx, cancel := context.WithTimeout(context.Background(), detectTimeout)
		out, verr := exec.CommandContext(ctx, bin, "version", "--short").Output()
		cancel()
		if verr != nil || strings.TrimSpace(string(out)) == "" {
			t.Skip("cloudflared present but not usable (version --short failed or empty)")
		}
	}

	home := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The hermetic --config file's own content must be present and valid for
	// cloudflared to parse it at all (see writeHermeticConfig); this probe
	// writes the exact same content Start would.
	configPath := filepath.Join(home, "cftunnel-config.yml")
	if err := os.WriteFile(configPath, []byte(hermeticConfigContent), 0o600); err != nil {
		t.Fatal(err)
	}

	// The named argv's flags minus the trailing positional (there's no real
	// tunnel to run against), plus --help so cloudflared parses the flags and
	// prints usage without ever dialing out.
	newArgs := []string{
		"tunnel",
		"--config", configPath,
		"--metrics", "127.0.0.1:0",
		"--logfile", filepath.Join(home, "cftunnel-test.log"),
		"--no-autoupdate",
		"run",
		"--url", "http://localhost:1",
		"--help",
	}
	newCmd := exec.CommandContext(ctx, bin, newArgs...)
	newCmd.Env = append(os.Environ(), "HOME="+home)
	newOut, _ := newCmd.CombinedOutput()
	if strings.Contains(string(newOut), "Incorrect Usage") {
		t.Errorf("current named argv order (including --config) rejected by real cloudflared:\n%s", newOut)
	}

	// Negative control 1: the OLD (pre-nc1j) order -- tunnel-level flags AFTER
	// `run` -- must still be rejected, proving this test would actually catch
	// a regression back to the broken order.
	oldArgs := []string{"tunnel", "run", "--url", "http://localhost:1", "--metrics", "127.0.0.1:0", "--help"}
	oldCmd := exec.CommandContext(ctx, bin, oldArgs...)
	oldCmd.Env = append(os.Environ(), "HOME="+home)
	oldOut, _ := oldCmd.CombinedOutput()
	if !strings.Contains(string(oldOut), "Incorrect Usage") {
		t.Fatalf("negative control: old flag order should be rejected by real cloudflared but wasn't:\n%s", oldOut)
	}

	// Negative control 2 (hermetic-config fix, kata nc1j): --config is ALSO a
	// tunnel-LEVEL-only flag, like --metrics/--logfile/--no-autoupdate --
	// live-verified that `cloudflared tunnel run --config X --help` prints
	// "Incorrect Usage: flag provided but not defined: -config". This proves
	// --config genuinely must precede `run`, not just that buildArgs happens
	// to put it there.
	configAfterRunArgs := []string{"tunnel", "run", "--config", configPath, "--help"}
	configAfterRunCmd := exec.CommandContext(ctx, bin, configAfterRunArgs...)
	configAfterRunCmd.Env = append(os.Environ(), "HOME="+home)
	configAfterRunOut, _ := configAfterRunCmd.CombinedOutput()
	if !strings.Contains(string(configAfterRunOut), "Incorrect Usage") {
		t.Fatalf("negative control: --config after run should be rejected by real cloudflared but wasn't:\n%s", configAfterRunOut)
	}
}

// TestStartSurfacesConsoleError is the regression test for audit item 4/R1
// (kata nc1j): cloudflared's fatal startup errors go to STDERR ALONE, so
// Start's error must come from the console capture, not the JSON --logfile
// (which R1 found gets at most one line, sometimes none). This replays the
// exact case-1 console text confirmed against real cloudflared with an empty
// HOME (no cert.pem): a console ERR line followed by a plain, unprefixed
// "error parsing tunnel ID: ..." line.
func TestStartSurfacesConsoleError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	body := `printf '2026-09-27T01:31:45Z ERR Cannot determine default origin certificate path. No file cert.pem in [~/.cloudflared ~/.cloudflare-warp ~/cloudflare-warp /etc/cloudflared /usr/local/etc/cloudflared]. You need to specify the origin certificate path by specifying the origincert option in the configuration file, or set TUNNEL_ORIGIN_CERT environment variable originCertPath=\n' >&2
printf 'error parsing tunnel ID: Error locating origin cert: client did not specify origincert path\n' >&2
exit 1
`
	c := &Client{Binary: fakeCloudflaredBin(t, body)}
	if _, err := c.Start(Spec{Port: 3010}); err == nil {
		t.Fatal("expected an error")
	} else if !strings.Contains(err.Error(), "Cannot determine default origin certificate path") {
		t.Errorf("error = %q, want it to contain the console's ERR message", err.Error())
	}
}

// TestStartSurfacesIncorrectUsage pins that a "successful" (exit 0) but
// mis-flagged invocation still surfaces as an error: urfave's own
// "Incorrect Usage" goes to STDOUT and exits 0, which would otherwise look
// exactly like a healthy startup to the liveness check.
func TestStartSurfacesIncorrectUsage(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	body := `printf 'Incorrect Usage: flag provided but not defined: -metrics\n\nNAME:\n  cloudflared tunnel run - Proxy a local web server\n'
exit 0
`
	c := &Client{Binary: fakeCloudflaredBin(t, body)}
	if _, err := c.Start(Spec{Port: 3011}); err == nil {
		t.Fatal("expected an error even though cloudflared exited 0")
	} else if !strings.Contains(err.Error(), "Incorrect Usage") {
		t.Errorf("error = %q, want it to contain \"Incorrect Usage\"", err.Error())
	}
}

// TestStartTruncatesConsole pins that Start opens the console file
// O_TRUNC, so stale content from a PREVIOUS Start on the same port/hostname
// never bleeds into a new one.
func TestStartTruncatesConsole(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logfile, err := logfilePath(3012, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(logfile), 0o755); err != nil {
		t.Fatal(err)
	}
	consolePath := ConsolePath(logfile)
	if err := os.WriteFile(consolePath, []byte("stale junk from a previous run\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &Client{Binary: fakeCloudflaredBin(t, "sleep 5\n")}
	r, err := c.Start(Spec{Port: 3012})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if p, e := os.FindProcess(r.PID); e == nil {
			_ = p.Kill()
		}
	}()

	data, err := os.ReadFile(consolePath)
	if err != nil {
		t.Fatalf("reading console file: %v", err)
	}
	if strings.Contains(string(data), "stale junk") {
		t.Errorf("console file should have been truncated on Start, still contains stale content: %q", data)
	}
}

// TestStartConsoleMode0600 pins the console file's permissions: it can hold
// cloudflared's own console text (never secrets tailport puts there itself,
// but conservative is cheap).
func TestStartConsoleMode0600(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	c := &Client{Binary: fakeCloudflaredBin(t, "sleep 5\n")}
	r, err := c.Start(Spec{Port: 3013})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if p, e := os.FindProcess(r.PID); e == nil {
			_ = p.Kill()
		}
	}()
	logfile, err := logfilePath(3013, "")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(ConsolePath(logfile))
	if err != nil {
		t.Fatalf("stat console file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("console file mode = %o, want 0600", perm)
	}
}

// TestStartWritesHermeticConfig is the regression test for the hermetic
// --config fix (kata nc1j): every Start (quick AND named) must (re)write
// tailport's own --config file with "{}" content and mode 0600, even if the
// file already existed with different (junk) content -- a hand-edit, or a
// stale file from before this feature existed, must never survive a Start
// and silently change what --url the tunnel actually serves. See
// configFilePath/writeHermeticConfig and buildArgs's doc comment for WHY:
// live-verified against cloudflared 2026.9.1, an ingress: config.yml
// anywhere on cloudflared's config search path otherwise silently overrides
// --url with no error and no warning.
func TestStartWritesHermeticConfig(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	configPath, err := configFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("ingress:\n  - service: http_status:418\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &Client{Binary: fakeCloudflaredBin(t, "sleep 5\n")}
	r, err := c.Start(Spec{Port: 3015})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if p, e := os.FindProcess(r.PID); e == nil {
			_ = p.Kill()
		}
	}()

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading hermetic config file: %v", err)
	}
	if strings.Contains(string(data), "ingress:") {
		t.Errorf("pre-seeded junk (ingress: rules) survived Start, should have been O_TRUNC'd away: %q", data)
	}
	if !strings.Contains(string(data), "{}") {
		t.Errorf("hermetic config content = %q, want it to contain \"{}\"", data)
	}

	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat hermetic config file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("hermetic config file mode = %o, want 0600 (pre-seeded as 0644)", perm)
	}
}

// TestConsoleTail pins ConsoleTail's priority order and text handling.
func TestConsoleTail(t *testing.T) {
	write := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "x.console")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("missing file returns empty", func(t *testing.T) {
		if got := ConsoleTail(filepath.Join(t.TempDir(), "nope.console")); got != "" {
			t.Errorf("got %q, want \"\"", got)
		}
	})

	t.Run("empty file returns empty", func(t *testing.T) {
		if got := ConsoleTail(write(t, "")); got != "" {
			t.Errorf("got %q, want \"\"", got)
		}
	})

	t.Run("Incorrect Usage takes priority over an ERR line", func(t *testing.T) {
		content := "2026-09-27T01:00:00Z ERR something else happened\n" +
			"Incorrect Usage: flag provided but not defined: -metrics\n"
		want := "Incorrect Usage: flag provided but not defined: -metrics"
		if got := ConsoleTail(write(t, content)); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("the LAST ERR line wins over an earlier one", func(t *testing.T) {
		content := "2026-09-27T01:00:00Z ERR first error\n" +
			"some unrelated console line\n" +
			"2026-09-27T01:00:01Z ERR second error\n"
		if got := ConsoleTail(write(t, content)); got != "second error" {
			t.Errorf("got %q, want %q", got, "second error")
		}
	})

	t.Run("FTL is recognized like ERR", func(t *testing.T) {
		content := "2026-09-27T01:00:00Z FTL fatal problem\n"
		if got := ConsoleTail(write(t, content)); got != "fatal problem" {
			t.Errorf("got %q, want %q", got, "fatal problem")
		}
	})

	t.Run("ANSI escapes are stripped", func(t *testing.T) {
		content := "\x1b[31m2026-09-27T01:00:00Z ERR colored error\x1b[0m\n"
		if got := ConsoleTail(write(t, content)); got != "colored error" {
			t.Errorf("got %q, want %q", got, "colored error")
		}
	})

	t.Run("fallback to the last non-empty line", func(t *testing.T) {
		content := "some startup banner\nanother info line\n\n"
		if got := ConsoleTail(write(t, content)); got != "another info line" {
			t.Errorf("got %q, want %q", got, "another info line")
		}
	})

	t.Run("truncated to 160 runes with an ellipsis", func(t *testing.T) {
		long := strings.Repeat("x", 200)
		content := "2026-09-27T01:00:00Z ERR " + long + "\n"
		got := ConsoleTail(write(t, content))
		want := string([]rune(long)[:160]) + "…"
		if got != want {
			t.Errorf("got rune len %d, want %d", len([]rune(got)), len([]rune(want)))
		}
	})
}

// TestStartScrubsIdentityEnv is the regression test for audit item 7 (kata
// nc1j): an ambient TUNNEL_TOKEN takes PRECEDENCE over the tunnel name and
// would run a completely different tunnel, and TUNNEL_NAME means "create,
// route, and run", which would mutate the account -- both must never reach
// the child. TUNNEL_ORIGIN_CERT is explicitly NOT scrubbed (a mismatch fails
// closed, it doesn't mutate anything).
func TestStartScrubsIdentityEnv(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("TUNNEL_TOKEN", "should-be-scrubbed")
	t.Setenv("TUNNEL_NAME", "should-be-scrubbed-too")
	t.Setenv("TUNNEL_ORIGIN_CERT", "/keep/this/cert.pem")

	envOut := filepath.Join(t.TempDir(), "env.out")
	t.Setenv("FAKE_ENV_OUT", envOut)
	c := &Client{Binary: fakeCloudflaredBin(t, "env > \"$FAKE_ENV_OUT\"\nsleep 5\n")}
	r, err := c.Start(Spec{Port: 3014})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if p, e := os.FindProcess(r.PID); e == nil {
			_ = p.Kill()
		}
	}()

	data, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatalf("reading captured child env: %v", err)
	}
	env := string(data)
	if strings.Contains(env, "TUNNEL_TOKEN=") {
		t.Error("TUNNEL_TOKEN leaked into the child environment")
	}
	if strings.Contains(env, "TUNNEL_NAME=") {
		t.Error("TUNNEL_NAME leaked into the child environment")
	}
	if !strings.Contains(env, "TUNNEL_ORIGIN_CERT=/keep/this/cert.pem") {
		t.Error("TUNNEL_ORIGIN_CERT should be preserved, not scrubbed")
	}
}

// TestAlive pins the two states Alive must tell apart: the calling test
// process's own pid (definitely alive), and a child that has already been
// fully reaped via Wait (definitely not -- and specifically not a zombie,
// which this is not, precisely because Run() already waited on it).
func TestAlive(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Error("the test's own pid should be alive")
	}

	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("running a trivial child: %v", err)
	}
	if Alive(cmd.Process.Pid) {
		t.Error("a fully-reaped child's pid should not be alive")
	}

	if Alive(0) || Alive(-1) {
		t.Error("Alive should reject a non-positive pid outright")
	}
}

// serverPort extracts the numeric port an httptest.Server is listening on, so a
// test can feed it to Health as the metrics port (Health builds
// http://127.0.0.1:<port>/... itself).
func serverPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	_, p, ok := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	if !ok {
		t.Fatalf("cannot parse server URL %q", srv.URL)
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		t.Fatalf("port %q: %v", p, err)
	}
	return n
}
