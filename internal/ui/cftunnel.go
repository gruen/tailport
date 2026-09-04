package ui

// Cloudflare Tunnel (the `t` key, kata nc1j): a THIRD public-exposure path
// alongside funnel (`P`) and Caddy-publish (`p`), modeled on publish but adapted
// to cloudflared's process model. Unlike publish -- a stateless client of a
// remote edge -- a tunnel is a LONG-RUNNING LOCAL process tailport supervises
// (internal/cftunnel). The whole feature is gated on cfAvailable: when
// cloudflared isn't installed the `t` key is inert (barGroups drops it) and the
// poll never runs, mirroring how the Caddy poll stays dark until caddy.domain is
// set.
//
// State model (the "tunnels survive tailport" choice): tunnels are discovered
// live from the process table each poll (never persisted), so a tunnel started
// in a prior session is re-found and re-toggleable after a restart. Only
// tailport-OWNED tunnels (carrying the sentinel logfile) participate; a foreign
// cloudflared is left alone.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/gruen/tailport/internal/cftunnel"
)

// tunnelInfo is the live per-port tunnel state a poll returns: the mode, the
// supervising process, its metrics port, the public hostname (a
// *.trycloudflare.com for quick -- "" until cloudflared assigns it -- or the
// operator's custom host for named), and whether it has an active edge
// connection. Keyed by local port in m.tunnels. Never persisted.
type tunnelInfo struct {
	mode        cftunnel.Mode
	pid         int
	metricsPort int
	hostname    string
	ready       bool
}

// tunnelMemory is the session-only shortcut (mirrors publishInfo/lastPublish):
// what a port was last tunnelled as, so `t` can re-raise a torn-down tunnel
// without re-running the setup prompts. Never persisted.
type tunnelMemory struct {
	mode     cftunnel.Mode
	hostname string
	name     string
}

// tunnelDoneMsg reports a completed start/stop supervise op. Like
// publishDoneMsg, its handler clears m.pending and re-polls rather than trusting
// an optimistic outcome -- though it also stores the started process
// immediately so the marker flips without waiting a poll cycle.
type tunnelDoneMsg struct {
	port     int
	err      error
	running  *cftunnel.Running
	torndown bool
}

// tunnelPollMsg carries a completed tunnel poll (process-table scan + health).
// gen versions it so an out-of-order completion can't clobber newer state
// (mirrors publishPollMsg). A scan failure sets err and the handler keeps the
// last-known map (quiet degrade).
type tunnelPollMsg struct {
	tunnels map[int]tunnelInfo
	gen     int
	err     error
}

// tunnelTickMsg fires the tunnel poll timer. Like publishTick it reschedules
// unconditionally; the poll it triggers is nil when cloudflared is unavailable.
type tunnelTickMsg struct{}

// tunnelPollInterval is faster than the Caddy poll: a tunnel poll is a cheap
// LOCAL process-table scan plus a couple of loopback metrics GETs, not a remote
// round-trip -- and a quick tunnel's URL only appears a few seconds after start,
// so a snappy cadence surfaces it promptly.
const tunnelPollInterval = 4 * time.Second

func tunnelTick() tea.Cmd {
	return tea.Tick(tunnelPollInterval, func(time.Time) tea.Msg { return tunnelTickMsg{} })
}

// cfClient builds the cloudflared client from cfg.Cloudflared (binary path
// override), or returns the test override when injected.
func (m model) cfClient() *cftunnel.Client {
	if m.cfClientOverride != nil {
		return m.cfClientOverride
	}
	return &cftunnel.Client{Binary: m.cfg.Cloudflared.Binary}
}

// pollTunnelsCmd is the tunnel-state poll: Discover() the process table, then
// Health()-scrape each owned tunnel's metrics endpoint for its readiness and
// (quick) hostname. Nil -- zero cost -- when cloudflared is unavailable. A
// Discover error degrades quietly (the handler keeps the last-known map).
func (m *model) pollTunnelsCmd() tea.Cmd {
	if !m.cfAvailable {
		return nil
	}
	client := m.cfClient()
	m.tunnelPollGen++
	gen := m.tunnelPollGen
	return func() tea.Msg {
		running, err := client.Discover()
		if err != nil {
			return tunnelPollMsg{gen: gen, err: err}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		tunnels := make(map[int]tunnelInfo, len(running))
		for _, r := range running {
			// Only OWNED tunnels are tracked (like the Caddy owned-only filter):
			// a foreign cloudflared is out of tailport's view.
			if !r.Owned {
				continue
			}
			h := client.Health(ctx, r.MetricsPort)
			hostname := r.Hostname // named: recovered from the sentinel logfile
			if hostname == "" {
				hostname = h.Hostname // quick: from /quicktunnel once assigned
			}
			tunnels[r.Port] = tunnelInfo{
				mode:        r.Mode,
				pid:         r.PID,
				metricsPort: r.MetricsPort,
				hostname:    hostname,
				ready:       h.Ready,
			}
		}
		return tunnelPollMsg{tunnels: tunnels, gen: gen}
	}
}

// tunnelStartCmd starts a tunnel off the render path and reports the outcome.
func tunnelStartCmd(client *cftunnel.Client, spec cftunnel.Spec) tea.Cmd {
	return func() tea.Msg {
		running, err := client.Start(spec)
		return tunnelDoneMsg{port: spec.Port, running: running, err: err}
	}
}

// tunnelStopCmd tears a tunnel down (SIGTERM its cloudflared) off the render path.
func tunnelStopCmd(pid, port int) tea.Cmd {
	return func() tea.Msg {
		err := cftunnel.Stop(pid)
		return tunnelDoneMsg{port: port, err: err, torndown: true}
	}
}

// requestTunnel is the `t` key's up-front gate, mirroring requestPublish. Guards
// run in order, then it toggles: an already-tunnelled port tears down
// immediately (de-escalation, never gated); a port tunnelled earlier this
// session re-raises from memory (still confirmed); otherwise it runs the full
// setup (mode select for a logged-in user, else straight to a quick tunnel).
func (m *model) requestTunnel(port int) tea.Cmd {
	// 1. busy: an exposure op is already in flight.
	if m.pending != 0 {
		return nil
	}
	// 2. feature gate: cloudflared must actually be installed.
	if !m.cfAvailable {
		return m.setFlash("cloudflared not found on PATH — install it to expose ports via Cloudflare", flashWarn)
	}
	// 3. already tunnelled on THIS port -> de-escalation: tear down now, no
	// confirm (reducing exposure is never gated).
	if info, ok := m.tunnels[port]; ok {
		m.pending = port
		return tunnelStopCmd(info.pid, port)
	}
	// 4. :22 (SSH) is hard-blocked from the public internet, same as funnel/publish.
	if port == 22 {
		return m.setErr("refusing to tunnel :22 (SSH) to the public internet")
	}
	// 5. th05 RELAXED mutual exclusion: a funnelled or published port may ALSO be
	// tunnelled now -- each public path is its own route sub-row. The tunnel
	// setup still runs its own public-internet confirm before going live.
	// 6. locked port: a tunnel must not bypass the `x` lock any more than serve/
	// funnel/publish do.
	if m.cfg.Ports[port].Locked {
		return m.setErr(fmt.Sprintf("port :%d is locked — press x to unlock", port))
	}

	m.tunnelPort = port

	// 7. Session-remembered RE-raise (mirrors publish's lastPublish shortcut): a
	// port tunnelled earlier this session skips the setup prompts and goes
	// straight to the confirm with its remembered mode/host/name.
	if mem, ok := m.lastTunnel[port]; ok {
		if mem.mode == cftunnel.ModeNamed && mem.hostname != "" {
			m.tunnelSetupMode = cftunnel.ModeNamed
			m.tunnelHostname = mem.hostname
			m.tunnelName = mem.name
			m.mode = entryConfirmTunnelNamed
			return nil
		}
		m.tunnelSetupMode = cftunnel.ModeQuick
		m.mode = entryConfirmTunnelQuick
		return nil
	}

	// 8. Fresh setup. A logged-in user chooses quick vs named; without an account
	// only the quick path exists, so skip straight to its confirm.
	if cftunnel.LoggedIn() {
		m.mode = entryTunnelMode
		return nil
	}
	m.tunnelSetupMode = cftunnel.ModeQuick
	m.mode = entryConfirmTunnelQuick
	return nil
}

// enterTunnelNamedHost opens the named-tunnel hostname prompt, prefilled from
// cloudflared.domain when set.
func (m *model) enterTunnelNamedHost() tea.Cmd {
	m.tunnelSetupMode = cftunnel.ModeNamed
	m.tunnelInput.Reset()
	m.tunnelInput.EchoMode = textinput.EchoNormal
	m.tunnelInput.Placeholder = "app.example.com"
	if d := m.cfg.Cloudflared.Domain; d != "" {
		m.tunnelInput.SetValue(d)
		m.tunnelInput.CursorEnd()
	}
	m.tunnelInput.Focus()
	m.mode = entryTunnelHost
	return nil
}

// updateTunnelEntry drives the two TEXT steps of the named-tunnel flow
// (hostname, then tunnel name). The y/n confirms and the mode select are handled
// inline in Update's key switch, like funnel/publish.
func (m *model) updateTunnelEntry(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.clearTunnelFlow()
		return nil
	case "enter":
		switch m.mode {
		case entryTunnelHost:
			host := strings.TrimSpace(m.tunnelInput.Value())
			if !validTunnelHostname(host) {
				return m.setErr("enter a public hostname you've routed to a cloudflared tunnel, e.g. app.example.com")
			}
			m.tunnelHostname = host
			m.tunnelInput.Reset()
			m.tunnelInput.Placeholder = "my-tunnel"
			m.tunnelInput.Focus()
			m.mode = entryTunnelName
			return nil
		case entryTunnelName:
			name := strings.TrimSpace(m.tunnelInput.Value())
			if name == "" {
				return m.setErr("enter the name of a cloudflared tunnel you created (cloudflared tunnel list)")
			}
			m.tunnelName = name
			m.mode = entryConfirmTunnelNamed
			return nil
		}
	}
	var cmd tea.Cmd
	m.tunnelInput, cmd = m.tunnelInput.Update(msg)
	return cmd
}

// confirmTunnelQuick is the entryConfirmTunnelQuick "yes" path: spawn a quick
// tunnel and remember the port as quick for re-raise. The URL isn't known yet
// (it appears via the poll), so the confirm named nothing and the flash says
// "starting…".
func (m *model) confirmTunnelQuick() tea.Cmd {
	port := m.tunnelPort
	m.rememberTunnel(port, tunnelMemory{mode: cftunnel.ModeQuick})
	spec := cftunnel.Spec{Port: port, Mode: cftunnel.ModeQuick}
	m.clearTunnelFlow()
	m.pending = port
	return tea.Batch(
		m.setFlash(fmt.Sprintf("starting Cloudflare quick tunnel for :%d…", port), flashInfo),
		tunnelStartCmd(m.cfClient(), spec),
	)
}

// confirmTunnelNamed is the entryConfirmTunnelNamed "yes" path: run the
// operator's pre-provisioned named tunnel bound to the confirmed hostname.
func (m *model) confirmTunnelNamed() tea.Cmd {
	port := m.tunnelPort
	host := m.tunnelHostname
	name := m.tunnelName
	m.rememberTunnel(port, tunnelMemory{mode: cftunnel.ModeNamed, hostname: host, name: name})
	spec := cftunnel.Spec{Port: port, Mode: cftunnel.ModeNamed, TunnelName: name, Hostname: host}
	m.clearTunnelFlow()
	m.pending = port
	return tunnelStartCmd(m.cfClient(), spec)
}

// rememberTunnel records a port's tunnel config for the session-only re-raise
// shortcut (see lastTunnel).
func (m *model) rememberTunnel(port int, mem tunnelMemory) {
	if m.lastTunnel == nil {
		m.lastTunnel = map[int]tunnelMemory{}
	}
	m.lastTunnel[port] = mem
}

// clearTunnelFlow resets the tunnel-setup flow fields and input, so an aborted
// or completed flow leaves nothing behind.
func (m *model) clearTunnelFlow() {
	m.mode = entryNone
	m.tunnelPort = 0
	m.tunnelSetupMode = cftunnel.ModeQuick
	m.tunnelHostname = ""
	m.tunnelName = ""
	m.tunnelInput.Reset()
}

// tunnelErrText maps a cftunnel op error to a user-facing toast, giving the
// two actionable sentinels a clear remedy and otherwise surfacing the raw error.
func tunnelErrText(err error) string {
	switch {
	case errors.Is(err, cftunnel.ErrNotInstalled):
		return "cloudflared not found on PATH — install it to expose ports via Cloudflare"
	case errors.Is(err, cftunnel.ErrNotLoggedIn):
		return "no Cloudflare account — run 'cloudflared tunnel login', or use a quick tunnel"
	default:
		return "cloudflared: " + err.Error()
	}
}

// validTunnelHostname is a light sanity check on a named-tunnel hostname: a
// dotted DNS name, no scheme/slashes/spaces. tailport can't verify the DNS
// route exists (that's the operator's pre-provisioning job), so this only
// catches obvious typos.
func validTunnelHostname(s string) bool {
	if s == "" || !strings.Contains(s, ".") {
		return false
	}
	if strings.ContainsAny(s, " /:") {
		return false
	}
	return true
}
