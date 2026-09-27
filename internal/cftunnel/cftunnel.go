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
//     `cloudflared tunnel --config PATH --metrics ADDR --logfile PATH
//     --no-autoupdate run --url http://localhost:PORT <name>`. The
//     tunnel-level flags (--config, --metrics, --logfile, --no-autoupdate)
//     MUST precede `run` -- cloudflared's flag parser rejects them after it
//     ("Incorrect Usage: flag provided but not defined", exit 0; verified
//     against cloudflared 2026.9.1, kata nc1j). tailport never mutates the
//     user's Cloudflare account or DNS.
//
// --config PATH is tailport's own, PER-TUNNEL config file (tunnelConfigPath/
// writeTunnelConfig below -- one file per port[-hostname], living beside
// that tunnel's log, never shared between tunnels), passed for BOTH quick
// and named tunnels alike. It exists for two reasons:
//
//   - It closes a live-verified footgun with no error and no warning: if ANY
//     config.yml with `ingress:` rules exists anywhere on cloudflared's own
//     config search path (~/.cloudflared, ~/.cloudflare-warp,
//     ~/cloudflare-warp, /etc/cloudflared, /usr/local/etc/cloudflared --
//     notably including the file `cloudflared service install` writes to
//     /etc/cloudflared/config.yml), cloudflared SILENTLY IGNORES tailport's
//     --url and serves that config's ingress origin instead. There is no
//     refusal and no warning -- tailport would show a port as publicly
//     served while the tunnel actually served something else entirely
//     (live-verified against cloudflared 2026.9.1, kata nc1j).
//   - For a NAMED tunnel (S4, audit finding 4, verified viable), it PINS that
//     tunnel's ingress to EXACTLY the confirmed hostname
//     (`ingress: [{hostname: H, service: http://localhost:PORT}, {service:
//     http_status:404}]`) instead of the catch-all `--url` alone would
//     produce: without this, --url makes cloudflared serve the local port
//     for EVERY hostname routed to the tunnel, including any wildcard --
//     the confirm's promise ("this hostname reaches this port") was
//     otherwise only as good as the operator having routed nothing else to
//     it. A quick tunnel's hostname isn't known up front, so its config
//     stays the same hermetic "{}" content this used to be for both modes.
//     N2: this pin only holds for a LOCALLY-managed tunnel, i.e. one created
//     with `cloudflared tunnel create` -- the only kind tailport supports.
//     A dashboard/remotely-managed tunnel has its ingress pushed BY
//     Cloudflare instead, which overrides this local --config entirely; such
//     tunnels are out of scope (see the token/dashboard-managed paragraph
//     below), and this is a documented limitation, not something a test can
//     pin (there is no local artifact to assert against).
//
// Rewritten with O_TRUNC on EVERY Start -- for BOTH quick and named -- so a
// hand-edit can never take effect, and it can never match sentinelLogRe
// (which requires a ".log" suffix), so it's invisible to ownership
// detection. See TestNamedTunnelConfigContent/TestQuickTunnelConfigContent
// and TestBuildArgsNamedFlagPlacement.
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
//
// cloudflared's own --logfile is a structured JSON log at info level (a
// user's TUNNEL_LOGLEVEL is respected, never overridden); request lines need
// debug, so a healthy tunnel's logfile stays small. It is NOT the whole
// story, though: fatal startup errors (a missing/empty origin cert, bad
// credentials) print to STDERR alone, and can leave the JSON logfile with at
// most one line, or nothing. Start also captures the child's raw
// stdout+stderr to a separate, truncated-on-every-Start ".console" file (see
// ConsolePath/ConsoleTail in console.go) so that text is always recoverable
// for a toast, without ever parsing the JSON logfile.
//
// Start also scrubs every TUNNEL_* environment variable except a small,
// connection-only allowlist from the child's environment before it inherits
// anything (see scrubTunnelEnv/tunnelEnvAllowlist) -- this, not any
// argv-level check alone, is how "tailport never mutates the account" holds
// against an ambient TUNNEL_TOKEN, TUNNEL_NAME, or TUNNEL_CRED_FILE (S1,
// audit finding 1: an inherited TUNNEL_CRED_FILE was verified to make
// cloudflared run a COMPLETELY DIFFERENT tunnel than the positional name
// says, ignoring it outright -- a previous version of this comment claimed
// TUNNEL_ORIGIN_CERT/TUNNEL_CRED_* "fail closed" and were deliberately kept;
// that was false, and both are scrubbed now). Consequently a named tunnel's
// credentials must sit at cloudflared's DEFAULT location next to cert.pem
// (see LoggedIn) -- the child never sees an override any more. Token- and
// dashboard-managed tunnels are out of scope for this feature entirely:
// tailport never passes --token.
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
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
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
	// Owned reports whether tailport started this process, decided from the
	// --logfile sentinel (see sentinelHost), the process's owner UID matching
	// os.Getuid() (S2(a); this is the /proc/<pid> directory owner on Linux,
	// the ps -o uid= column on macOS -- effectively the EFFECTIVE uid, not
	// the real uid, see discover.go), and -- for a named tunnel -- its recovered
	// hostname/name both passing ValidHostname/ValidTunnelName (S2(c)). A
	// false value means a foreign cloudflared covers this port -- surfaced as
	// drift, never signalled.
	Owned bool
	// LogFile is the --logfile value (tailport's own sentinel path) for an
	// OWNED process; empty for a foreign one. ConsolePath derives that
	// tunnel's console-capture path from it, so a caller holding only a
	// Running (e.g. after re-Discover()ing across a tailport restart) can
	// still find its console output.
	LogFile string
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
// whether the NAMED tunnel path is available. It checks ONLY cloudflared's
// default cert location (~/.cloudflared/cert.pem) -- it does NOT honor a
// TUNNEL_ORIGIN_CERT override in tailport's own environment (a previous
// version of this comment and function claimed it did): Start now scrubs
// TUNNEL_ORIGIN_CERT from the CHILD's environment (S1, audit finding 1)
// precisely because an inherited override can point cloudflared at a
// DIFFERENT credential than the positional tunnel name would suggest, so
// honoring that same override here would just be checking a file the child
// will never actually see. Since the child only ever uses cloudflared's
// default credential locations now, checking that same default location is
// what actually matches what Start will do. It does not validate the cert --
// only that the operator has logged in at least once.
func LoggedIn() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	return fileExists(filepath.Join(home, ".cloudflared", "cert.pem"))
}

// tunnelEnvAllowlist is the ONLY `TUNNEL_*` environment variables the child
// inherits; every other `TUNNEL_*` variable is dropped, whether tailport
// recognizes it or not (S1, audit finding 1, MEDIUM -- verified: an inherited
// TUNNEL_CRED_FILE overrides cloudflared's origin credential file
// independently of the positional tunnel name -- a tunnel named "foo" was
// observed running a completely different tunnelID from the credentials
// file, so an operator who merely had production tunnel credentials exported
// in their shell would silently join THAT tunnel as a connector. A previous
// version of this file and AGENTS.md claimed TUNNEL_CRED_* "fails closed" --
// that claim was FALSE; it does the opposite). A denylist of "the ones we
// thought of" can never keep up with cloudflared's own env var surface (see
// audit item: TUNNEL_ORIGIN_CERT, TUNNEL_CRED_FILE, TUNNEL_CRED_CONTENTS,
// TUNNEL_EDGE were all missed by the previous denylist), so this is a
// default-DENY allowlist instead: every var here affects only HOW the
// connector talks to the edge or what it logs, never WHICH tunnel, origin,
// edge, or account is used.
var tunnelEnvAllowlist = map[string]bool{
	"TUNNEL_TRANSPORT_PROTOCOL": true,
	"TUNNEL_EDGE_IP_VERSION":    true,
	"TUNNEL_EDGE_BIND_ADDRESS":  true,
	"TUNNEL_REGION":             true,
	"TUNNEL_POST_QUANTUM":       true,
	"TUNNEL_LOGLEVEL":           true,
	"TUNNEL_TRANSPORT_LOGLEVEL": true,
	"TUNNEL_PROTO_LOGLEVEL":     true,
	"TUNNEL_RETRIES":            true,
	"TUNNEL_GRACE_PERIOD":       true,
}

// scrubTunnelEnv returns environ (as from os.Environ()) with every `TUNNEL_*`
// variable removed EXCEPT tunnelEnvAllowlist, preserving relative order
// otherwise. Every non-`TUNNEL_*` variable -- including NO_AUTOUPDATE -- is
// always kept untouched.
//
// This is now the ONLY thing standing between tailport's own environment and
// the child's: TUNNEL_ORIGIN_CERT/TUNNEL_CRED_FILE/TUNNEL_CRED_CONTENTS are
// dropped too (S1) -- see the package doc and AGENTS.md's Cloudflare Tunnel
// bullet for the consequence: a named tunnel's credentials must now sit at
// cloudflared's DEFAULT location next to cert.pem (which matches LoggedIn's
// gate above), since the child never sees an override any more.
func scrubTunnelEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if strings.HasPrefix(name, "TUNNEL_") && !tunnelEnvAllowlist[name] {
			continue
		}
		out = append(out, kv)
	}
	return out
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
	if spec.Mode == ModeNamed {
		if !ValidTunnelName(spec.TunnelName) {
			return nil, fmt.Errorf("cftunnel: invalid tunnel name %q -- must be non-empty, contain no whitespace or control characters, and not start with '-'", spec.TunnelName)
		}
		// S4, audit finding 4: the hostname is interpolated straight into the
		// per-tunnel --config's ingress rule (tunnelConfigContent), so it must
		// be validated BEFORE that ever happens -- ValidHostname's restricted
		// charset also makes that interpolation safe even without the quoting
		// tunnelConfigContent still applies as defense in depth.
		if !ValidHostname(spec.Hostname) {
			return nil, fmt.Errorf("cftunnel: invalid hostname %q for named tunnel -- must be a dotted DNS name, e.g. app.example.com", spec.Hostname)
		}
		if !LoggedIn() {
			return nil, ErrNotLoggedIn
		}
	}

	// S5, audit finding 6: refuse a second owned tunnel on the same LOCAL
	// PORT before ever spawning one -- regardless of mode or tunnel identity.
	// This is a defense-in-depth safety net UNDERNEATH internal/ui's own
	// guards (requestTunnel's already-tunnelled de-escalation, and
	// tunnelNameInUse's owned-only same-NAME check): neither of those is
	// this package's job to rely on, since Client.Start is also callable
	// directly, outside the UI's cached m.tunnels state. A Discover error is
	// NOT fatal here -- this is a best-effort guard, not the authoritative
	// source of truth (Stop's re-validate-immediately-before-signalling
	// remains that) -- so Start simply proceeds rather than blocking on a
	// transient scan failure.
	if running, derr := c.Discover(); derr == nil {
		for _, r := range running {
			if r.Owned && r.Port == spec.Port {
				return nil, fmt.Errorf("a tailport tunnel is already running for :%d (pid %d)", spec.Port, r.PID)
			}
		}
	}

	logfile, err := logfilePath(spec.Port, spec.Hostname)
	if err != nil {
		return nil, err
	}
	// State dir hygiene (S3, audit findings 3/7, verified): cloudflared itself
	// creates a --logfile at 0644 and the dir it lives in was previously
	// created at 0755 -- both readable by anyone on the box. ensureStateDir
	// creates it 0700, tightens an existing, self-owned dir back to 0700, and
	// (N1) now REFUSES outright -- Start never proceeds -- if the dir is a
	// symlink, isn't owned by this UID, or is still group/other-writable
	// after that tightening attempt.
	if err := ensureStateDir(filepath.Dir(logfile)); err != nil {
		return nil, fmt.Errorf("cftunnel: preparing state dir: %w", err)
	}

	// Pre-create the --logfile ourselves at 0600, O_NOFOLLOW, O_TRUNC (S3):
	// the audit verified cloudflared KEEPS a pre-existing file's mode when it
	// opens it to append, so this is what actually keeps the log non-world-
	// readable (cloudflared's own O_CREATE would otherwise apply ITS default,
	// 0644, only when the file doesn't already exist). This also bounds the
	// log to one run per Start, which costs nothing: tailport never reads the
	// JSON logfile (see the package doc). O_NOFOLLOW makes a symlink planted
	// at this path a hard Start failure rather than a silent write-through.
	logFD, err := os.OpenFile(logfile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cftunnel: preparing log file: %w", err)
	}
	// The 0o600 above only applies when OpenFile actually CREATES the file;
	// a stale, pre-existing log from an old tailport version (or a manually
	// widened one) keeps ITS OWN mode otherwise -- Chmod explicitly so this
	// is 0600 unconditionally, matching writeTunnelConfig's same pattern.
	if err := logFD.Chmod(0o600); err != nil {
		logFD.Close()
		return nil, fmt.Errorf("cftunnel: preparing log file: %w", err)
	}
	logFD.Close()

	// Per-tunnel --config [S4, audit finding 4; live-verified against
	// cloudflared 2026.9.1, kata nc1j]: see the package doc for WHY this
	// exists (a config.yml with ingress: rules anywhere on cloudflared's
	// search path silently overrides --url, and for a NAMED tunnel this also
	// PINS its ingress to exactly spec.Hostname). Rewritten with O_TRUNC on
	// EVERY Start -- for BOTH quick and named -- so a hand-edit can never
	// take effect. O_NOFOLLOW (S3): a symlink planted at this path must fail
	// Start, not get silently written through to wherever it points.
	configPath, err := tunnelConfigPath(spec.Port, spec.Hostname)
	if err != nil {
		return nil, err
	}
	if err := writeTunnelConfig(configPath, tunnelConfigContent(spec)); err != nil {
		return nil, fmt.Errorf("cftunnel: preparing per-tunnel --config: %w", err)
	}

	metricsPort := spec.MetricsPort
	if metricsPort == 0 {
		metricsPort, err = freeLoopbackPort()
		if err != nil {
			return nil, fmt.Errorf("cftunnel: reserving metrics port: %w", err)
		}
	}
	spec.MetricsPort = metricsPort

	// Console capture [R1]: cloudflared's fatal startup errors (bad cert,
	// bad/missing credentials) go to STDERR ALONE, and urfave's own
	// "Incorrect Usage" flag-parsing errors go to STDOUT and exit 0 -- so the
	// --logfile (JSON, info level) can hold nothing useful, or nothing at all,
	// for exactly the failures callers most need explained (see ConsoleTail).
	// Opened O_TRUNC so a stale file from a previous Start never bleeds into
	// this one, and O_NOFOLLOW (S3) so a symlink planted at this path is a
	// hard Start failure rather than a silent write-through to its target.
	consolePath := ConsolePath(logfile)
	consoleFile, err := os.OpenFile(consolePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cftunnel: preparing console capture: %w", err)
	}
	// As with the log file above: the 0o600 in OpenFile only applies on
	// actual creation, so Chmod explicitly in case a stale file from an old
	// tailport version (or a manually widened one) already existed wider.
	if err := consoleFile.Chmod(0o600); err != nil {
		consoleFile.Close()
		return nil, fmt.Errorf("cftunnel: preparing console capture: %w", err)
	}

	cmd := exec.Command(c.binary(), buildArgs(spec, logfile, configPath)...)
	// Detach: new session (no controlling terminal, survives parent exit).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	// Both stdout and stderr go to the SAME file, deliberately never a pipe:
	// once tailport exits, a pipe reader going away would SIGPIPE the still
	// -running detached child the next time it wrote.
	cmd.Stdout = consoleFile
	cmd.Stderr = consoleFile
	// Scrub every TUNNEL_* env var except a small allowlist before the child
	// inherits anything (see scrubTunnelEnv/tunnelEnvAllowlist) -- an ambient
	// TUNNEL_TOKEN, for instance, takes precedence over the tunnel name and
	// would silently run a DIFFERENT tunnel than the one requested, and an
	// ambient TUNNEL_CRED_FILE overrides which tunnel's credentials get used
	// regardless of the positional name (S1, audit finding 1). This is how
	// "tailport never mutates the account" holds against the environment,
	// not just against tailport's own argv.
	cmd.Env = scrubTunnelEnv(os.Environ())

	err = cmd.Start()
	// The child, once started, keeps its own OS-level reference to this fd;
	// the parent's *os.File is only needed to hand it over, so close our copy
	// right away regardless of outcome.
	consoleFile.Close()
	if err != nil {
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
		// cmd.Wait() has returned by construction of this select (that's what
		// sent on exited), so cmd.ProcessState is safely readable here with no
		// race against the goroutine above.
		if tail := ConsoleTail(consolePath); tail != "" {
			return nil, fmt.Errorf("exited at startup — %s", tail)
		}
		status := "unknown exit status"
		switch {
		case cmd.ProcessState != nil:
			status = cmd.ProcessState.String()
		case werr != nil:
			status = werr.Error()
		}
		return nil, fmt.Errorf("exited at startup (%s)", status)
	case <-time.After(startupGrace):
		// Still running past the grace window -- looks healthy. exited stays
		// buffered for the reaper goroutine's eventual send.
	}

	r := &Running{
		Port:        spec.Port,
		PID:         cmd.Process.Pid,
		Mode:        spec.Mode,
		TunnelName:  spec.TunnelName,
		MetricsPort: metricsPort,
		LogFile:     logfile,
		Owned:       true,
	}
	if spec.Mode == ModeNamed {
		// The named success flash and the route row's URL both key off
		// Running.Hostname; without this, a just-started named tunnel showed no
		// URL until the NEXT poll re-derived it from the sentinel logfile name
		// (audit item 3, kata nc1j) -- Start already knows it from spec, so
		// hand it back immediately.
		r.Hostname = spec.Hostname
	}
	return r, nil
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
		// Discover() didn't map pid to a cloudflared invocation it recognizes.
		// That's normally because it's genuinely gone -- the tunnel is down
		// either way, so treat it as success. But pid could instead be alive
		// and merely UNDISCOVERABLE: a debugger/strace wrapper, a sudo parent,
		// or a non-exec wrapper script that left a live intermediary between
		// itself and the real cloudflared (isCloudflaredArgv0 doesn't, and
		// mustn't, match those [R2] -- see TestStopRefusesUndiscoverableLiveProcess).
		// Defense in depth: re-check liveness directly before declaring
		// success, and refuse to signal a pid we can no longer positively
		// identify as tailport's.
		if !Alive(pid) {
			return nil
		}
		return fmt.Errorf("cftunnel: pid %d is running but tailport can no longer identify it as its cloudflared for :%d -- refusing to signal it", pid, port)
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

// Alive reports whether pid refers to a still-existing process, via a
// signal-0 probe (syscall.Kill(pid, 0)): no signal is actually delivered, but
// the kernel still validates that pid exists and is signalable by this UID.
// ESRCH ("no such process") is the only case treated as false; any other
// error (e.g. EPERM for a process this UID can't signal) still means the
// process EXISTS, so it's treated as alive. A just-exited child is briefly a
// ZOMBIE and still answers this as alive until something reaps it (Wait) --
// callers that need to tell the two apart re-check after a short delay (see
// the poll-based exit detection, kata nc1j).
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return !errors.Is(err, syscall.ESRCH)
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
		// quickTunnelHostnameRe (S2(c), audit finding 2) rejects anything
		// that isn't shaped like a genuine *.trycloudflare.com hostname --
		// cloudflared's metrics server is loopback-only, but a malicious
		// same-host process could still bind the exact metrics port between
		// polls (a metrics port is a freshly reserved, unauthenticated
		// loopback listener) and hand back an arbitrary string tailport
		// would otherwise render as-is in a route row/toast.
		if json.Unmarshal(body, &q) == nil && quickTunnelHostnameRe.MatchString(q.Hostname) {
			h.Hostname = q.Hostname
		}
	}
	return h
}

// quickTunnelHostnameRe is the only shape /quicktunnel's "hostname" field may
// take: a *.trycloudflare.com hostname, case-insensitive (S2(c)).
var quickTunnelHostnameRe = regexp.MustCompile(`(?i)^[a-z0-9-]+\.trycloudflare\.com$`)

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
// I/O) so it is unit-tested directly.
//
// The tunnel-level flags (--config, --metrics, --logfile, --no-autoupdate)
// MUST precede `run`: cloudflared's flag parser only registers them at the
// `tunnel` level, not under the `run` subcommand, so passing any of them
// after `run` fails with "Incorrect Usage: flag provided but not defined:
// -metrics" (or -config, -logfile, -no-autoupdate) and exits 0 -- silently,
// since that's not a nonzero exit (verified against real cloudflared
// 2026.9.1; a PREVIOUS comment here claiming these were "accepted under both
// `tunnel` and `tunnel run`" was false -- the named path had been broken
// since v0.2.1, kata nc1j). `--url` and the trailing tunnel-name positional
// come after `run`, since `run` is what accepts a tunnel reference; Discover
// relies on the name staying the LAST element. The quick argv has no `run`,
// so flag ordering among tunnel-level flags doesn't matter to cloudflared
// there, but --config is placed first, right after "tunnel", for both modes.
//
// configPath is tailport's own PER-TUNNEL --config file (tunnelConfigPath /
// writeTunnelConfig), passed for BOTH quick and named tunnels: without it,
// an ingress: config.yml anywhere on cloudflared's search path silently
// overrides --url, and for a named tunnel it also PINS that tunnel's ingress
// to exactly its confirmed hostname (S4; see the package doc).
// TestBuildArgs and TestBuildArgsNamedFlagPlacement pin the exact shape; see
// TestNamedArgvAcceptedByRealCloudflared for the real-binary proof that
// --config, like the other tunnel-level flags, is rejected after `run`.
func buildArgs(spec Spec, logfile, configPath string) []string {
	urlArgs := []string{"--url", fmt.Sprintf("http://localhost:%d", spec.Port)}
	tunnelFlags := []string{
		"--metrics", fmt.Sprintf("127.0.0.1:%d", spec.MetricsPort),
		"--logfile", logfile,
		"--no-autoupdate",
	}
	if spec.Mode == ModeNamed {
		args := []string{"tunnel", "--config", configPath}
		args = append(args, tunnelFlags...)
		args = append(args, "run")
		args = append(args, urlArgs...)
		return append(args, spec.TunnelName)
	}
	args := []string{"tunnel", "--config", configPath}
	args = append(args, urlArgs...)
	return append(args, tunnelFlags...)
}

// tunnelConfigPath is tailport's own PER-TUNNEL cloudflared --config file
// (S4, audit finding 4): unlike the ONE shared file this used to be, each
// tunnel gets its own, living beside its logfile in the state dir, so a
// named tunnel's ingress-pinning content (tunnelConfigContent) never leaks
// between tunnels. Named its basename the same way logfilePath does
// (cftunnel-<port>[-<host>].yml) except for the extension -- ".yml" can
// never match sentinelLogRe (which requires a ".log" suffix), so it stays
// invisible to ownership detection either way; Discover never scans the
// state dir anyway, it only ever reads flag VALUES off a process's own argv.
//
// See the package doc for WHY this file exists at all: a config.yml with
// ingress: rules anywhere on cloudflared's own config search path silently
// overrides tailport's --url, with no error and no warning (live-verified,
// kata nc1j); for a named tunnel this file ALSO pins its ingress to exactly
// hostname -- but only for a LOCALLY-managed tunnel (N2): a dashboard/
// remotely-managed tunnel gets its ingress pushed by Cloudflare instead,
// which overrides this file regardless of what it contains. See
// TestNamedTunnelConfigContent/TestQuickTunnelConfigContent.
func tunnelConfigPath(port int, hostname string) (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("cftunnel-%d.yml", port)
	if hostname != "" {
		name = fmt.Sprintf("cftunnel-%d-%s.yml", port, hostname)
	}
	return filepath.Join(dir, name), nil
}

// tunnelConfigComment is the leading line of every per-tunnel --config file,
// solely so a curious operator who opens it understands why it exists and
// that hand-editing it is futile -- cloudflared itself ignores a leading
// "#" line in a YAML file.
const tunnelConfigComment = "# tailport-managed (kata nc1j): pins this LOCALLY-managed tunnel's ingress to the confirmed hostname and keeps --url authoritative over any config.yml cloudflared would otherwise silently apply instead of it. Rewritten on every tunnel start -- editing this file has no effect. (A dashboard/remotely-managed tunnel ignores this file; that's unsupported.)\n"

// tunnelConfigContent builds the per-tunnel --config file's content (S4,
// audit finding 4, verified viable):
//
//   - NAMED: an explicit `ingress:` list that pins the tunnel to EXACTLY
//     spec.Hostname, with a catch-all `http_status:404` after it -- so any
//     OTHER hostname that happens to also be routed to this same
//     pre-provisioned tunnel gets a plain 404 instead of this port. Without
//     this, `--url` alone makes cloudflared serve the local port for EVERY
//     hostname routed to the tunnel (including a wildcard), so the confirm's
//     promise was only as good as the operator having routed nothing else to
//     it. This pin only takes effect for a LOCALLY-managed tunnel (N2): a
//     dashboard/remotely-managed tunnel's ingress is pushed by Cloudflare
//     and overrides whatever this file says -- out of scope, documented, not
//     something a unit test can pin. spec.Hostname is expected to have
//     already passed ValidHostname
//     (Start rejects an invalid one before this is ever called), whose
//     restricted charset makes the interpolation safe on its own -- it's
//     quoted here anyway, as defense in depth.
//   - QUICK: the hostname isn't known until cloudflared assigns one after it
//     starts, so there is nothing to pin yet; content stays the same
//     hermetic "{}" both modes used before S4. cloudflared logs
//     "ERR Configuration file ... was empty" for a zero-byte --config
//     (live-verified) even though it otherwise proceeds, so this must never
//     be literally empty either.
func tunnelConfigContent(spec Spec) string {
	if spec.Mode == ModeNamed {
		return fmt.Sprintf("%singress:\n  - hostname: %q\n    service: http://localhost:%d\n  - service: http_status:404\n",
			tunnelConfigComment, spec.Hostname, spec.Port)
	}
	return tunnelConfigComment + "{}\n"
}

// writeTunnelConfig (re)writes the per-tunnel --config file at path with
// content, truncating and fixing its mode to 0600 even if the file already
// existed (e.g. pre-seeded with junk, or left over from a previous tailport
// version): a hand-edit, or stale content from before this feature existed,
// must never survive a Start. See TestNamedTunnelConfigContent/
// TestQuickTunnelConfigContent. O_NOFOLLOW (S3, audit findings 3/7) makes a
// symlink planted at this path a hard error instead of a silent
// write-through to wherever it points.
func writeTunnelConfig(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	_, err = f.WriteString(content)
	return err
}

// ValidTunnelName reports whether s is an acceptable named-tunnel identifier:
// non-empty, containing no whitespace, control, or Unicode FORMAT (category
// Cf) characters (which would either split across argv elements, corrupt the
// sentinel logfile name, or -- Cf specifically, N4 -- visually disguise what
// is displayed without being a control byte at all: e.g. U+202E RIGHT-TO-LEFT
// OVERRIDE can make "web<RLO>gpj.exe" render as if it ended ".exe.jpg" or
// similar, and U+200B ZERO WIDTH SPACE can split a name invisibly), and not
// starting with '-' (which cloudflared's own flag parser would otherwise try
// to interpret as another flag rather than the positional tunnel name).
// Start rejects any spec.TunnelName that fails this before ever shelling out
// to cloudflared. See TestValidTunnelName and SanitizeDisplay (console.go),
// which strips the same Cf category from any string tailport did NOT itself
// validate first (e.g. a name recovered from a foreign sentinel).
func ValidTunnelName(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

// ValidHostname reports whether s is an acceptable named-tunnel public
// hostname: a dotted DNS name restricted to [A-Za-z0-9.-], containing at
// least one dot, not starting with '-' or '.', not ENDING with '.' either
// (N5: a trailing dot is syntactically a valid absolute DNS name, but a
// pinned ingress (tunnelConfigContent) for "app.example.com." would NOT
// match one confirmed as "app.example.com" -- cloudflared's ingress hostname
// match is exact, so this would 404 every request), and with no EMPTY label
// (e.g. "a..b.com") or label starting/ending with '-' (e.g. "a.-b.com",
// "a.b-.com" -- neither is a valid DNS label, and the same silent-404 risk
// applies). This is the single source
// of truth for hostname syntax the UI's own validTunnelHostname used to
// duplicate (kata nc1j W3a) -- moved/exported here (S2(c), audit finding 2)
// so parseRunning can also apply it to a hostname RECOVERED from a
// --logfile sentinel (see sentinelHost): tailport itself only ever writes a
// sentinel whose embedded hostname already passed this exact check at Start
// time (see Start and S4's tunnelConfigPath), so a sentinel that fails it now
// was crafted by something else -- a hostile, same-UID process trying to get
// a bogus string (a leading dot, an escape sequence) rendered as if it were
// a genuine tailport-owned tunnel's hostname -- and must never be classified
// Owned. Because the allowed charset excludes every control byte outright,
// a string that passes this check needs no further SanitizeDisplay pass.
func ValidHostname(s string) bool {
	if s == "" || !strings.Contains(s, ".") {
		return false
	}
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '-':
		default:
			return false
		}
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
	}
	return true
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

// ensureStateDir creates dir (tailport's state dir) at mode 0700 if it
// doesn't exist yet, and otherwise makes a best-effort attempt to TIGHTEN it
// back to 0700 (S3, audit findings 3/7, verified: a previous version of
// Start created it at 0755, world-readable). That chmod only succeeds when
// this UID already owns dir -- it always fails for anyone else's directory
// -- so this now REFUSES to proceed, with a clear error, unless dir, AFTER
// that attempt, is BOTH owned by os.Getuid() AND carries no group/other
// write bit (mode&0o022==0) (N1, a pre-release security-review follow-up: a
// previous version silently proceeded regardless -- verified,
// ensureStateDir("/tmp") used to return nil even though /tmp is neither
// owned by the caller in general nor even close to private). The mode check
// is independent of the ownership one, not merely implied by it: a dir we DO
// own can still end up refused if the chmod attempt itself failed despite
// that ownership (a read-only filesystem, an immutable attribute) and its
// PRE-existing mode was already group/other-writable.
//
// A dir that is itself a SYMLINK is refused too, via the os.Lstat/IsDir
// check below: os.MkdirAll follows a symlink (so an existing target
// directory makes MkdirAll's own Stat call a silent no-op), but os.Lstat
// does not, so a symlink's IsDir() is false and this returns an error before
// ever reaching the ownership/mode checks.
//
// This is deliberately not the only defense: the per-file O_NOFOLLOW opens
// in Start remain the guard against a SAME-uid, otherwise-legitimate dir
// having a symlink planted inside it later.
func ensureStateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("cftunnel: state dir %s is not a directory (is it a symlink?)", dir)
	}
	if info.Mode().Perm() != 0o700 {
		// Best-effort: this only succeeds if we already own dir. Its error
		// is deliberately ignored here -- the ownership/mode re-check below,
		// on dir's ACTUAL resulting state, is what decides whether to refuse,
		// and reports a clear reason either way.
		_ = os.Chmod(dir, 0o700)
		if info, err = os.Lstat(dir); err != nil {
			return err
		}
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cftunnel: cannot determine the owner of state dir %s on this platform", dir)
	}
	if st.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("cftunnel: refusing state dir %s: owned by uid %d, not this process's uid %d -- remove it or fix its ownership", dir, st.Uid, os.Getuid())
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("cftunnel: refusing state dir %s: group/other-writable (mode %04o) -- chmod it to 0700", dir, perm)
	}
	return nil
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
