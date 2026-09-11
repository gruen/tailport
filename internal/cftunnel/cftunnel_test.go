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
	quick := buildArgs(Spec{Port: 3000, Mode: ModeQuick, MetricsPort: 20941}, "/state/cftunnel-3000.log")
	wantQuick := []string{
		"tunnel",
		"--url", "http://localhost:3000",
		"--metrics", "127.0.0.1:20941",
		"--logfile", "/state/cftunnel-3000.log",
		"--no-autoupdate",
	}
	if !reflect.DeepEqual(quick, wantQuick) {
		t.Errorf("quick args:\n got %q\nwant %q", quick, wantQuick)
	}

	named := buildArgs(Spec{Port: 8080, Mode: ModeNamed, TunnelName: "web", MetricsPort: 20942}, "/state/cftunnel-8080.log")
	wantNamed := []string{
		"tunnel", "run",
		"--url", "http://localhost:8080",
		"--metrics", "127.0.0.1:20942",
		"--logfile", "/state/cftunnel-8080.log",
		"--no-autoupdate",
		"web", // trailing positional -- namedTunnelName relies on this
	}
	if !reflect.DeepEqual(named, wantNamed) {
		t.Errorf("named args:\n got %q\nwant %q", named, wantNamed)
	}
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
			want: Running{Port: 3000, Mode: ModeQuick, MetricsPort: 20941, Owned: true},
			ok:   true,
		},
		{
			name: "named owned (hostname recovered from logfile)",
			args: []string{"/usr/bin/cloudflared", "tunnel", "run",
				"--url", "http://localhost:8080",
				"--metrics", "127.0.0.1:20942",
				"--logfile", "/home/u/.local/state/tailport/cftunnel-8080-app.example.com.log",
				"--no-autoupdate", "web"},
			want: Running{Port: 8080, Mode: ModeNamed, TunnelName: "web", MetricsPort: 20942, Hostname: "app.example.com", Owned: true},
			ok:   true,
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
			want: Running{Port: 9000, Mode: ModeQuick, MetricsPort: 20950, Owned: true},
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
