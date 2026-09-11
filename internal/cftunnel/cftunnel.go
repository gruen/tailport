// Package cftunnel supervises `cloudflared` to expose a local port to the
// public internet through a Cloudflare Tunnel (kata nc1j -- the `t` key).
//
// Unlike tailport's other exposure paths, cloudflared is fundamentally a
// LONG-RUNNING LOCAL PROCESS: the binary *is* the connector, so a tunnel is
// up only while its cloudflared process stays alive. There is no server-side
// data plane. tailport therefore SUPERVISES the process rather than poking a
// remote control plane the way internal/caddyedge does.
//
// Two flavours, matching Cloudflare's two account scenarios:
//
//   - Quick (ModeQuick): no account, no config --
//     `cloudflared tunnel --url http://localhost:PORT` -- hands back a random
//     `*.trycloudflare.com` hostname, unauthenticated and ephemeral (a new
//     hostname every run).
//   - Named (ModeNamed): an authenticated account whose operator has already
//     run `cloudflared tunnel login`, created a tunnel, and routed a hostname
//     to it (`route dns`). tailport only *runs* that pre-provisioned tunnel:
//     `cloudflared tunnel run --url http://localhost:PORT <name>`. It never
//     mutates the user's Cloudflare account or DNS.
//
// Because the user chose "tunnels survive tailport", cloudflared is started
// DETACHED (its own session via Setsid) so it outlives the TUI, and the
// SOURCE OF TRUTH for tunnel state is the OS process table, not any file
// tailport persists -- exactly the "read live, foreign shows as drift"
// philosophy serve/funnel/publish already follow. Discover() re-finds running
// tunnels on every poll (and across restarts) by scanning for cloudflared
// processes and parsing their command lines; the ownership sentinel is a
// `--logfile .../cftunnel-<port>.log` flag tailport always passes (see
// logfilePath), which is both cmdline-visible on every platform and a place to
// capture logs. A cloudflared WITHOUT that sentinel is a foreign tunnel: it is
// surfaced, never signalled.
package cftunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Mode is the tunnel flavour (quick vs named); see the package doc.
type Mode int

const (
	// ModeQuick is an unauthenticated ad-hoc tunnel to a random
	// *.trycloudflare.com hostname -- no Cloudflare account required.
	ModeQuick Mode = iota
	// ModeNamed runs a pre-provisioned, account-backed named tunnel bound to
	// a stable custom hostname the operator already routed.
	ModeNamed
)

// String renders a Mode for logs/tests.
func (m Mode) String() string {
	if m == ModeNamed {
		return "named"
	}
	return "quick"
}

// ErrNotInstalled is returned by Detect when the `cloudflared` binary is not
// found on $PATH (or the configured Binary path). The whole feature stays
// dormant in this case -- no `t` control, no discovery, no polling.
var ErrNotInstalled = errors.New("cloudflared is not installed -- install it from https://developers.cloudflare.com/cloudflare-one/connections/connect-apps/install-and-setup/installation/ to expose ports via Cloudflare Tunnel")

// ErrNotLoggedIn is returned when a NAMED tunnel is requested but no Cloudflare
// account credential (cert.pem) is present. The quick path needs no account and
// never returns this.
var ErrNotLoggedIn = errors.New("no Cloudflare account credential found -- run 'cloudflared tunnel login' once, or use a quick tunnel instead")

// Spec describes a tunnel tailport is about to start.
type Spec struct {
	// Port is the local TCP port to expose (proxied as http://localhost:PORT).
	Port int
	// Mode selects quick vs named.
	Mode Mode
	// TunnelName is the pre-provisioned named tunnel to run (ModeNamed only).
	TunnelName string
	// Hostname is the public hostname the named tunnel's DNS CNAME already
	// routes (ModeNamed only). tailport does not set this up; it carries it for
	// display/confirm/copy. Ignored for ModeQuick (the hostname isn't known
	// until cloudflared reports it).
	Hostname string
	// MetricsPort pins cloudflared's local metrics HTTP server
	// (--metrics 127.0.0.1:MetricsPort), which tailport polls for the
	// quick-tunnel hostname (/quicktunnel) and readiness (/ready). Zero means
	// "pick a free loopback port".
	MetricsPort int
}

// Running is a discovered cloudflared tunnel process. It is built purely from
// the process table + command line, so it round-trips across a tailport
// restart with no persisted state.
type Running struct {
	// Port is the local port the tunnel exposes (from --url).
	Port int
	// PID is the cloudflared process id.
	PID int
	// Mode is quick or named (named iff the cmdline is `tunnel run ...`).
	Mode Mode
	// TunnelName is the named tunnel's name (ModeNamed; best-effort for foreign
	// processes).
	TunnelName string
	// MetricsPort is cloudflared's metrics port parsed from --metrics, or 0 if
	// it wasn't pinned (e.g. a foreign tunnel started without --metrics).
	MetricsPort int
	// Hostname is the public hostname for a NAMED tunnel, recovered from the
	// sentinel logfile name (a named tunnel's hostname is bound via DNS, not
	// passed to cloudflared, so it is otherwise unrecoverable across a tailport
	// restart -- see logfilePath). Empty for a quick tunnel (whose hostname
	// comes from /quicktunnel instead) and for foreign processes.
	Hostname string
	// Owned reports whether tailport started this process, decided solely from
	// the --logfile sentinel (see sentinelHost). A false value means a foreign
	// cloudflared covers this port -- surfaced as drift, never signalled.
	Owned bool
}

// Health is the live status scraped from a tunnel's metrics endpoints.
type Health struct {
	// Ready reports whether cloudflared has at least one active edge
	// connection (/ready returned HTTP 200 with readyConnections > 0).
	Ready bool
	// ReadyConnections is the edge-connection count from /ready (0..4).
	ReadyConnections int
	// Hostname is the quick-tunnel's assigned *.trycloudflare.com hostname
	// from /quicktunnel. Empty for a named tunnel, or before it's assigned.
	Hostname string
}

// Client shells out to cloudflared and polls its metrics endpoints. The
// exported fields are injectable so tests can point at a fake binary / server,
// mirroring internal/caddyedge's house style. The zero value is usable
// (cloudflared on $PATH, a default-timeout HTTP client).
type Client struct {
	// Binary is the cloudflared executable (path or bare name resolved on
	// $PATH). Empty means "cloudflared". Comes from config.Cloudflared.Binary.
	Binary string
	// HTTPClient polls the loopback metrics endpoints; nil means a default
	// short-timeout client (metricsTimeout).
	HTTPClient *http.Client
}

// metricsTimeout bounds a single metrics-endpoint GET. These are loopback
// requests, so anything slower than this means cloudflared is unhealthy or
// gone, not merely busy.
const metricsTimeout = 2 * time.Second

// detectTimeout bounds the startup `cloudflared version` probe so a wedged
// binary can't hang the TUI's construction.
const detectTimeout = 3 * time.Second

// startupGrace bounds how long Start waits after spawning cloudflared to
// catch an IMMEDIATE exit (a bad --url/tunnel name, an already-bound metrics
// port, etc.) so callers get a real failure reason instead of a false
// "success" (roborev carryover, kata aprt). It is NOT a readiness check -- an
// actual edge connection can take several more seconds and is Health's job --
// so it stays short.
const startupGrace = 300 * time.Millisecond

func (c *Client) binary() string {
	if c.Binary != "" {
		return c.Binary
	}
	return "cloudflared"
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: metricsTimeout}
}

// Detect reports whether cloudflared is usable, returning its version string
// (e.g. "2026.8.2"). It returns ErrNotInstalled when the binary isn't on
// $PATH. A present-but-unrunnable binary returns a wrapped error. The caller
// (the TUI) gates the entire feature on a nil error.
func (c *Client) Detect() (string, error) {
	bin := c.binary()
	if _, err := exec.LookPath(bin); err != nil {
		return "", ErrNotInstalled
	}
	// Bound the version probe: it runs synchronously at TUI startup, so a wedged
	// binary must not hang the whole app.
	ctx, cancel := context.WithTimeout(context.Background(), detectTimeout)
	defer cancel()
	// `version --short` prints just the bare version (e.g. "2026.8.2"); older
	// builds may not accept it, so fall back to `--version` and best-effort the
	// first line.
	if out, err := exec.CommandContext(ctx, bin, "version", "--short").Output(); err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("cloudflared present but not runnable: %w", err)
	}
	return strings.TrimSpace(firstLine(string(out))), nil
}

// LoggedIn reports whether a Cloudflare account credential is present, i.e.
// whether the NAMED tunnel path is available. It checks cloudflared's default
// cert location (~/.cloudflared/cert.pem), honoring the TUNNEL_ORIGIN_CERT
// override cloudflared itself reads. It does not validate the cert -- only that
// the operator has logged in at least once.
func LoggedIn() bool {
	if p := os.Getenv("TUNNEL_ORIGIN_CERT"); p != "" {
		return fileExists(p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	return fileExists(filepath.Join(home, ".cloudflared", "cert.pem"))
}

// Start launches a detached cloudflared tunnel for spec and returns the
// Running descriptor (PID + resolved metrics port) so the caller can begin
// polling immediately. The process is put in its OWN session (Setsid) with its
// stdio sent to the null device, so it survives tailport exiting and never
// touches the TUI's terminal. It waits out startupGrace before declaring
// success (roborev carryover, kata aprt): cmd.Start succeeding only means the
// OS could exec the binary, not that cloudflared accepted its arguments -- a
// bad --url or tunnel name exits moments later, and without this check the
// caller would see a "successfully started" tunnel that's already dead. A
// background goroutine reaps the process once it actually exits (so a crashed
// or later-stopped tunnel doesn't leave a zombie); if tailport exits first,
// the OS reparents and keeps the tunnel running.
func (c *Client) Start(spec Spec) (*Running, error) {
	if spec.Port <= 0 {
		return nil, fmt.Errorf("cftunnel: invalid port %d", spec.Port)
	}
	if spec.Mode == ModeNamed && spec.TunnelName == "" {
		return nil, errors.New("cftunnel: named tunnel requires a tunnel name")
	}
	if spec.Mode == ModeNamed && !LoggedIn() {
		return nil, ErrNotLoggedIn
	}

	logfile, err := logfilePath(spec.Port, spec.Hostname)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(logfile), 0o755); err != nil {
		return nil, fmt.Errorf("cftunnel: preparing log dir: %w", err)
	}

	metricsPort := spec.MetricsPort
	if metricsPort == 0 {
		metricsPort, err = freeLoopbackPort()
		if err != nil {
			return nil, fmt.Errorf("cftunnel: reserving metrics port: %w", err)
		}
	}
	spec.MetricsPort = metricsPort

	cmd := exec.Command(c.binary(), buildArgs(spec, logfile)...)
	// Detach: new session (no controlling terminal, survives parent exit) and
	// stdio to the null device (never write to tailport's TUI).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("cftunnel: starting cloudflared: %w", err)
	}
	// Reap when it exits so a tunnel that dies during this tailport session
	// doesn't become a zombie. If tailport exits first this goroutine simply
	// dies with it and the kernel reparents/reaps the still-running child.
	// Buffered so the send never blocks even if nobody ends up reading it (the
	// healthy path below doesn't).
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// Liveness check: give cloudflared startupGrace to prove it didn't
	// immediately exit before reporting success.
	select {
	case werr := <-exited:
		if werr != nil {
			return nil, fmt.Errorf("cftunnel: cloudflared exited immediately after starting: %w", werr)
		}
		return nil, errors.New("cftunnel: cloudflared exited immediately after starting")
	case <-time.After(startupGrace):
		// Still running past the grace window -- looks healthy. exited stays
		// buffered for the reaper goroutine's eventual send.
	}

	return &Running{
		Port:        spec.Port,
		PID:         cmd.Process.Pid,
		Mode:        spec.Mode,
		TunnelName:  spec.TunnelName,
		MetricsPort: metricsPort,
		Owned:       true,
	}, nil
}

// Stop tears down the tunnel that was running at pid on port: it
// RE-VALIDATES ownership from the LIVE process table immediately before
// signalling, then sends SIGTERM (cloudflared drains and exits cleanly).
//
// pid is a snapshot from the last poll -- up to tunnelPollInterval stale by
// the time a user presses `t`. In that window the tailport-owned cloudflared
// could have already exited and the OS handed pid to an unrelated process
// (PID reuse); blindly signalling the cached pid could kill that unrelated
// process (roborev carryover, kata aprt -- the HIGH-severity finding: a
// cached PID must never be trusted without re-checking it's still genuinely
// ours right before the signal). Re-Discover()ing and matching pid+port+Owned
// here closes that window down to the tiny gap between the check and the
// signal itself, which no check-then-act API can fully eliminate without
// OS-level support (e.g. Linux pidfd) -- out of scope for this fix.
func (c *Client) Stop(pid, port int) error {
	if pid <= 0 {
		return fmt.Errorf("cftunnel: invalid pid %d", pid)
	}
	running, err := c.Discover()
	if err != nil {
		return fmt.Errorf("cftunnel: re-validating ownership before stopping pid %d: %w", pid, err)
	}
	found := false
	for _, r := range running {
		if r.PID != pid {
			continue
		}
		found = true
		if !r.Owned || r.Port != port {
			// pid is alive but is no longer -- or never was -- OUR tunnel for
			// this port. Almost certainly PID reuse: refuse rather than risk
			// signalling an unrelated process.
			return fmt.Errorf("cftunnel: pid %d is no longer tailport's tunnel for :%d (process identity changed) -- refusing to signal it", pid, port)
		}
		break
	}
	if !found {
		// Already gone -- the tunnel is down either way.
		return nil
	}
	p, err := os.FindProcess(pid) // always non-nil on unix
	if err != nil {
		return err
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		// Already gone is success -- the tunnel is down either way.
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return fmt.Errorf("cftunnel: signalling pid %d: %w", pid, err)
	}
	return nil
}

// Health scrapes a tunnel's metrics server (127.0.0.1:metricsPort) for its
// readiness and, for a quick tunnel, its assigned hostname. It never errors:
// an unreachable or still-starting endpoint yields a zeroed Health (Ready
// false, Hostname ""), which the caller combines with the process-liveness it
// already has from Discover. metricsPort == 0 (a foreign tunnel with no pinned
// metrics) short-circuits to a zero Health.
func (c *Client) Health(ctx context.Context, metricsPort int) Health {
	var h Health
	if metricsPort <= 0 {
		return h
	}
	if status, body, err := c.metricsGet(ctx, metricsPort, "/ready"); err == nil {
		var r struct {
			ReadyConnections int `json:"readyConnections"`
		}
		_ = json.Unmarshal(body, &r)
		h.ReadyConnections = r.ReadyConnections
		h.Ready = status == http.StatusOK && r.ReadyConnections > 0
	}
	if _, body, err := c.metricsGet(ctx, metricsPort, "/quicktunnel"); err == nil {
		var q struct {
			Hostname string `json:"hostname"`
		}
		if json.Unmarshal(body, &q) == nil {
			h.Hostname = q.Hostname
		}
	}
	return h
}

// metricsGet performs one GET against cloudflared's loopback metrics server and
// returns the status code and body. The endpoints only ever bind loopback.
func (c *Client) metricsGet(ctx context.Context, metricsPort int, path string) (int, []byte, error) {
	u := fmt.Sprintf("http://127.0.0.1:%d%s", metricsPort, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// buildArgs assembles the cloudflared argument vector for spec. It is pure (no
// I/O) so it is unit-tested directly. All four flags are accepted under both
// `tunnel` and `tunnel run` (verified against cloudflared 2026.8.2); the named
// tunnel's name is the trailing positional, which Discover relies on when
// recovering the name from a running process.
func buildArgs(spec Spec, logfile string) []string {
	base := []string{
		"--url", fmt.Sprintf("http://localhost:%d", spec.Port),
		"--metrics", fmt.Sprintf("127.0.0.1:%d", spec.MetricsPort),
		"--logfile", logfile,
		"--no-autoupdate",
	}
	if spec.Mode == ModeNamed {
		return append(append([]string{"tunnel", "run"}, base...), spec.TunnelName)
	}
	return append([]string{"tunnel"}, base...)
}

// logfilePath is where a tunnel for port writes its log, AND the ownership
// sentinel Discover keys off. The basename is cftunnel-<port>.log for a quick
// tunnel; for a NAMED tunnel the hostname is folded in
// (cftunnel-<port>-<hostname>.log) so Discover can recover it after a tailport
// restart -- a named tunnel's hostname is bound via DNS and never appears on
// the cloudflared command line, so the logfile name is the only cmdline-visible
// place to carry it. It lives under tailport's state dir ($XDG_STATE_HOME/
// tailport, else ~/.local/state/tailport). DNS hostnames are filename-safe
// (letters, digits, hyphens, dots), so no escaping is needed.
func logfilePath(port int, hostname string) (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("cftunnel-%d.log", port)
	if hostname != "" {
		name = fmt.Sprintf("cftunnel-%d-%s.log", port, hostname)
	}
	return filepath.Join(dir, name), nil
}

func stateDir() (string, error) {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "tailport"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "tailport"), nil
}

// freeLoopbackPort reserves a free 127.0.0.1 TCP port by briefly binding :0 and
// reading back the assigned port. There is a small TOCTOU window between close
// and cloudflared's bind, but the port is recovered authoritatively from the
// process cmdline afterwards regardless, so a lost race only fails one Start
// (cloudflared errors on the taken port), never corrupts discovery.
func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// parseLocalPort extracts the port from a --url value like
// "http://localhost:3000". Any host is accepted (we only need the port); a
// missing/zero port fails.
func parseLocalPort(raw string) (int, bool) {
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

// parseMetricsPort extracts the port from a --metrics value like
// "127.0.0.1:20941" or "localhost:20941".
func parseMetricsPort(hostport string) int {
	_, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return 0
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return p
}
