package ui

// Cloudflare Tunnel (kata nc1j; remapped from `t`, kata 7nss BREAKING): a
// THIRD public-exposure path alongside funnel (`P`) and Caddy-publish (`p`),
// modeled on publish but adapted to cloudflared's process model. Unlike
// publish -- a stateless client of a remote edge -- a tunnel is a
// LONG-RUNNING LOCAL process tailport supervises (internal/cftunnel). The
// whole feature is gated on cfAvailable: when cloudflared isn't installed
// neither key does anything (barGroups drops `o`; `O` never appeared on the
// bar in the first place) and the poll never runs, mirroring how the Caddy
// poll stays dark until caddy.domain is set.
//
// Split in two (kata p7c5, owner's call: the old single `o` toggle's
// q/n-mode-select-then-two-text-prompts flow was confusing): `o` now ONLY
// ever runs a QUICK tunnel, straight to its confirm, no prompt. `O` ONLY ever
// runs a NAMED tunnel, driven entirely by a per-port config.yaml binding
// (config.CloudflareBinding, under PortMeta.Cloudflare) -- also straight to
// its confirm, no prompt. There is no more session-only "remembered tunnel"
// re-raise for EITHER key: the config file is `O`'s only memory, and a quick
// tunnel never had settings worth remembering in the first place. Each key
// only ever touches its OWN mode on a given port; pressing the other key on a
// port whose running tunnel is the other mode refuses with a hint pointing at
// the key that owns it (requestTunnel/requestTunnelNamed).
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
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gruen/tailport/internal/cftunnel"
)

// tunnelInfo is the live per-port tunnel state a poll returns: the mode, the
// supervising process, its metrics port, the public hostname (a
// *.trycloudflare.com for quick -- "" until cloudflared assigns it -- or the
// operator's custom host for named), the named tunnel's name (ModeNamed only;
// recovered from the sentinel logfile -- audit item 2, kata nc1j: without
// this a re-raise had no name to hand `Start`, which then rejected it),
// consolePath (its console-capture file, from cftunnel.ConsolePath -- kata
// nc1j W3b: pollTunnelsCmd's next poll needs this to read a since-vanished
// tunnel's last error, so it's snapshotted here rather than re-derived after
// the fact), and whether it has an active edge connection. Keyed by local
// port in m.tunnels. Never persisted.
type tunnelInfo struct {
	mode        cftunnel.Mode
	pid         int
	metricsPort int
	hostname    string
	name        string
	consolePath string
	ready       bool
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
// last-known map (quiet degrade). foreign is the set of ports covered by a
// cloudflared tailport does NOT own (no --logfile sentinel): per AGENTS.md a
// foreign tunnel must surface as drift rather than stay invisible, so unlike
// owned tunnels it's kept (not discarded) for the UI to show and for
// requestTunnel to refuse layering a second exposure on top of (kata aprt).
// vanished lists ports that were owned/running immediately before THIS poll
// and are missing now (audit item 4, kata nc1j W3b) -- computed inside the
// poll's own tea.Cmd (see pollTunnelsCmd), never in Update, so it's empty
// (not merely unchecked) whenever err != nil: an errored Discover has no
// reliable "missing" list to offer, so the handler must not raise a vanish
// toast off it (the quiet-degrade path already keeps the last-known map for
// exactly this reason).
type tunnelPollMsg struct {
	tunnels  map[int]tunnelInfo
	foreign  map[int]bool
	vanished []vanishedTunnel
	gen      int
	err      error
}

// tunnelSnapshot is what pollTunnelsCmd needs to know about a port's owned
// tunnel from BEFORE a poll's Discover ran, captured AT ISSUE TIME (never
// inside the returned tea.Cmd, which must not read m.tunnels directly -- it
// runs off the render path) so the cmd can tell a genuinely VANISHED tunnel
// (was here, isn't now) from one that simply was never running.
type tunnelSnapshot struct {
	pid         int
	consolePath string
}

// vanishedTunnel is one port whose owned tunnel was running immediately
// before a poll's Discover ran and is missing from it now (audit item 4,
// kata nc1j W3b): a late failure (auth, name lookup, retries exhausted) or a
// crash, caught on the next poll cycle rather than any fixed timeout. alive
// distinguishes cloudflared having actually exited (Alive(pid) false, even
// after the brief zombie re-check below) from the port simply becoming
// unrecognizable while the pid is still running (Alive true -- something
// else changed) -- the handler's toast wording differs between the two. tail
// is ConsoleTail's last line for the dead case, "" if unavailable.
type vanishedTunnel struct {
	port  int
	pid   int
	alive bool
	tail  string
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

// tunnelSpinnerTickMsg advances the pending-quick-tunnel spinner one frame
// (kata h2ef). id is a flashID-style generation guard: startTunnelSpinner
// bumps m.tunnelSpinnerID, so a tick from an earlier, superseded animation is
// dropped on arrival rather than reviving a loop that already stopped itself.
type tunnelSpinnerTickMsg struct{ id int }

// tunnelSpinnerInterval is a plain animation cadence -- fast enough to read as
// motion, cheap enough that redrawing on every tick is a non-issue (a handful
// of ticks total: quick tunnels resolve in a few seconds, and the loop
// self-stops the moment they do -- see the tunnelSpinnerTickMsg handler).
const tunnelSpinnerInterval = 120 * time.Millisecond

func tunnelSpinnerTick(id int) tea.Cmd {
	return tea.Tick(tunnelSpinnerInterval, func(time.Time) tea.Msg { return tunnelSpinnerTickMsg{id: id} })
}

// startTunnelSpinner arms the pending-URL spinner for port and returns its
// first tick cmd. Called only from the quick-tunnel confirm path
// (confirmTunnelQuick) -- a named tunnel's hostname is known up front, so it
// never needs this. Bumping tunnelSpinnerID invalidates any previous
// animation's in-flight tick, so two quick-tunnel starts in a row can't have
// their ticks cross-talk. The loop stops itself (no explicit "stop" call
// needed): the tunnelSpinnerTickMsg handler clears tunnelSpinnerPort once
// m.tunnels[port] reports a non-empty hostname, and the tunnelDoneMsg error/
// teardown branches clear it early if the start never produces one.
func (m *model) startTunnelSpinner(port int) tea.Cmd {
	m.tunnelSpinnerID++
	m.tunnelSpinnerFrame = 0
	m.tunnelSpinnerPort = port
	return tunnelSpinnerTick(m.tunnelSpinnerID)
}

// tunnelStartupCmd is Init's tunnel entry point: the first poll plus the
// recurring ticker, or a true nil -- not even a timer -- when cloudflared is
// unavailable. cfAvailable is decided once at New() and never changes, so an
// uninstalled host pays nothing at all this way, not even a 4s wakeup (LOW,
// kata aprt: the ticker used to reschedule itself forever regardless of
// availability, contradicting the "zero cost when absent" gate).
func (m *model) tunnelStartupCmd() tea.Cmd {
	if !m.cfAvailable {
		return nil
	}
	return tea.Batch(m.pollTunnelsCmd(), tunnelTick())
}

// invalidateTunnelPolls bumps the tunnel poll generation and marks it
// already-applied, so a poll issued BEFORE this call -- one already in flight
// when a start/stop op lands -- is dropped on arrival instead of clobbering
// the optimistic state the op just applied (kata aprt: a delayed poll erasing
// a just-started tunnel, or resurrecting a just-torn-down one). Call it
// immediately before tunnelDoneMsg's handler mutates m.tunnels directly.
func (m *model) invalidateTunnelPolls() {
	m.tunnelPollGen++
	m.tunnelPollApplied = m.tunnelPollGen
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
// Discover error degrades quietly (the handler keeps the last-known map, and
// reports no vanished tunnels -- see tunnelPollMsg).
//
// It also snapshots, AT ISSUE TIME, which owned ports are running right now
// (prev) so the returned tea.Cmd -- which runs off the render path and must
// never read m.tunnels directly -- can tell a genuinely VANISHED tunnel from
// one that was simply never running (audit item 4, kata nc1j W3b): a port in
// prev but missing from this poll's fresh Discover has either exited on its
// own or become otherwise unrecognizable, and either way must never just
// disappear (see tunnelPollMsg's handler).
func (m *model) pollTunnelsCmd() tea.Cmd {
	if !m.cfAvailable {
		return nil
	}
	client := m.cfClient()
	m.tunnelPollGen++
	gen := m.tunnelPollGen
	prev := make(map[int]tunnelSnapshot, len(m.tunnels))
	for port, info := range m.tunnels {
		prev[port] = tunnelSnapshot{pid: info.pid, consolePath: info.consolePath}
	}
	return func() tea.Msg {
		running, err := client.Discover()
		if err != nil {
			return tunnelPollMsg{gen: gen, err: err}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		tunnels := make(map[int]tunnelInfo, len(running))
		foreign := make(map[int]bool)
		for _, r := range running {
			if !r.Owned {
				// A cloudflared tailport didn't start (no --logfile sentinel).
				// AGENTS.md: surface it as drift -- never signalled, but never
				// invisible either -- so record the port rather than discarding
				// it outright (kata aprt; unlike Caddy's DELIBERATE v1
				// foreign-route punt in pollPublishedCmd, this one IS in scope).
				foreign[r.Port] = true
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
				name:        r.TunnelName,
				consolePath: cftunnel.ConsolePath(r.LogFile),
				ready:       h.Ready,
			}
		}
		// Vanish detection (audit item 4, kata nc1j W3b): a port that WAS
		// owned/running before this poll (prev) and isn't in the fresh
		// tunnels map now is either genuinely gone or unrecognizable.
		var vanished []vanishedTunnel
		for port, snap := range prev {
			if _, ok := tunnels[port]; ok {
				continue // still there
			}
			alive := cftunnel.Alive(snap.pid)
			if alive {
				// A just-exited child is briefly a zombie and still answers a
				// signal-0 probe until something reaps it -- re-check once
				// more after a short delay before believing "still running"
				// (mirrors Start's own reaper-timing concern).
				time.Sleep(250 * time.Millisecond)
				alive = cftunnel.Alive(snap.pid)
			}
			vanished = append(vanished, vanishedTunnel{
				port:  port,
				pid:   snap.pid,
				alive: alive,
				tail:  cftunnel.ConsoleTail(snap.consolePath),
			})
		}
		return tunnelPollMsg{tunnels: tunnels, foreign: foreign, vanished: vanished, gen: gen}
	}
}

// tunnelStartCmd starts a tunnel off the render path and reports the outcome.
func tunnelStartCmd(client *cftunnel.Client, spec cftunnel.Spec) tea.Cmd {
	return func() tea.Msg {
		running, err := client.Start(spec)
		return tunnelDoneMsg{port: spec.Port, running: running, err: err}
	}
}

// tunnelStopCmd tears a tunnel down (SIGTERM its cloudflared) off the render
// path. client.Stop re-validates ownership from the live process table
// immediately before signalling (kata aprt) -- it never trusts pid on faith.
func tunnelStopCmd(client *cftunnel.Client, pid, port int) tea.Cmd {
	return func() tea.Msg {
		err := client.Stop(pid, port)
		return tunnelDoneMsg{port: port, err: err, torndown: true}
	}
}

// requestTunnel is the `o` key's up-front gate, mirroring requestPublish. `o`
// ONLY ever touches a QUICK tunnel (kata p7c5 split the old single toggle in
// two -- see requestTunnelNamed for `O`, its named sibling): guards run in
// order, then either an already-running quick tunnel on this port tears down
// immediately (de-escalation, never gated), or a fresh setup goes STRAIGHT to
// the quick confirm -- no mode prompt, even when cftunnel.LoggedIn() is true.
// A named tunnel already running on this port is the cross-key case: refuse
// with a hint pointing at `O`, the key that owns it, rather than silently
// tearing down the wrong mode or layering a second tunnel on the port.
func (m *model) requestTunnel(port int) tea.Cmd {
	// 1. busy: an exposure op is already in flight.
	if m.pending != 0 {
		return nil
	}
	// 2. feature gate: cloudflared must actually be installed.
	if !m.cfAvailable {
		return m.setFlash("cloudflared not found on PATH — install it to expose ports via Cloudflare", flashWarn)
	}
	// 3. already tunnelled on THIS port: o only ever de-escalates a QUICK
	// tunnel (no confirm -- reducing exposure is never gated); a NAMED one
	// here belongs to `O`, so refuse and say so rather than acting on it.
	if info, ok := m.tunnels[port]; ok {
		if info.mode == cftunnel.ModeNamed {
			return m.setErr(fmt.Sprintf("a named tunnel is running on :%d — press O to stop it", port))
		}
		m.pending = port
		return tunnelStopCmd(m.cfClient(), info.pid, port)
	}
	// 3.5. a FOREIGN cloudflared already covers this port (AGENTS.md: surfaced
	// as drift, never signalled). It must not be invisible, and layering a
	// second tunnel on the same local port would just be confusing -- which one
	// is "the" tunnel? -- so refuse rather than pile on (kata aprt).
	if m.tunnelForeign[port] {
		return m.setErr(fmt.Sprintf("port :%d already has a cloudflared tunnel tailport doesn't own — resolve it outside tailport first", port))
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

	// 7. Fresh setup: straight to the quick confirm. No mode prompt (kata
	// p7c5, owner's call) -- a named tunnel now lives entirely behind `O`.
	m.tunnelPort = port
	m.mode = entryConfirmTunnelQuick
	return nil
}

// requestTunnelNamed is the `O` key's up-front gate (kata p7c5): the NAMED
// sibling of requestTunnel, sharing the same busy/availability/foreign/:22/
// locked guards, but resolving its tunnel/hostname from the port's
// config.yaml binding (config.CloudflareBinding, PortMeta.Cloudflare)
// instead of any prompt -- there is nothing left to type. `O` only ever
// touches a NAMED tunnel; a QUICK one already running on this port is the
// cross-key case, refused with a hint pointing at `o`.
func (m *model) requestTunnelNamed(port int) tea.Cmd {
	// 1. busy: an exposure op is already in flight.
	if m.pending != 0 {
		return nil
	}
	// 2. feature gate: cloudflared must actually be installed.
	if !m.cfAvailable {
		return m.setFlash("cloudflared not found on PATH — install it to expose ports via Cloudflare", flashWarn)
	}
	// 3. already tunnelled on THIS port: O only ever de-escalates a NAMED
	// tunnel (no confirm); a QUICK one here belongs to `o`.
	if info, ok := m.tunnels[port]; ok {
		if info.mode == cftunnel.ModeQuick {
			return m.setErr(fmt.Sprintf("a quick tunnel is running on :%d — press o to stop it", port))
		}
		m.pending = port
		return tunnelStopCmd(m.cfClient(), info.pid, port)
	}
	// 3.5. a FOREIGN cloudflared already covers this port -- same guard as `o`.
	if m.tunnelForeign[port] {
		return m.setErr(fmt.Sprintf("port :%d already has a cloudflared tunnel tailport doesn't own — resolve it outside tailport first", port))
	}
	// 4. :22 (SSH) is hard-blocked from the public internet, same as `o`.
	if port == 22 {
		return m.setErr("refusing to tunnel :22 (SSH) to the public internet")
	}
	// 5. th05 RELAXED mutual exclusion: a funnelled or published port may ALSO
	// be tunnelled now -- same as `o`.
	// 6. locked port -- same guard as `o`.
	if m.cfg.Ports[port].Locked {
		return m.setErr(fmt.Sprintf("port :%d is locked — press x to unlock", port))
	}

	// 7. Resolve the binding. This IS the setup, and it's the ONLY memory `O`
	// has -- there is no session re-raise any more, and no prompt: an
	// incomplete or absent binding just refuses, naming the fix.
	bind := m.cfg.Ports[port].Cloudflare
	if bind == nil || bind.Tunnel == "" || bind.Hostname == "" {
		return m.setErr(fmt.Sprintf(":%d has no named tunnel — add ports.%d.cloudflare {tunnel, hostname} to config.yaml", port, port))
	}
	if !cftunnel.LoggedIn() {
		return m.setErr("named tunnels need `cloudflared tunnel login` first")
	}
	// The binding is a hand-editable file value, not something the user just
	// typed into a confirm -- re-validate it exactly like Start would, and
	// name which field is bad. SanitizeDisplay (S2(b)/N4, exported for this)
	// guards the toast against a hostile or merely mangled config value.
	if !cftunnel.ValidTunnelName(bind.Tunnel) {
		return m.setErr(fmt.Sprintf("ports.%d.cloudflare.tunnel %q is not a valid tunnel name — fix config.yaml", port, cftunnel.SanitizeDisplay(bind.Tunnel)))
	}
	if !cftunnel.ValidHostname(bind.Hostname) {
		return m.setErr(fmt.Sprintf("ports.%d.cloudflare.hostname %q is not a valid hostname — fix config.yaml", port, cftunnel.SanitizeDisplay(bind.Hostname)))
	}
	// Owned-only same-tunnel guard [R3]: this named tunnel might already be
	// serving a DIFFERENT port -- refuse rather than let Start spawn a second
	// cloudflared for the same pre-provisioned tunnel.
	if other := m.tunnelNameInUse(bind.Tunnel, port); other != 0 {
		return m.setErr(tunnelNameRefusedMsg(bind.Tunnel, other))
	}

	m.tunnelPort = port
	m.tunnelHostname = bind.Hostname
	m.tunnelName = bind.Tunnel
	m.mode = entryConfirmTunnelNamed
	return nil
}

// tunnelNameInUse reports the port (nonzero) already running an OWNED named
// tunnel called name, other than exceptPort -- returning 0 when name isn't in
// use anywhere else. This is the owned-only same-tunnel guard [R3, kata
// nc1j]: a pre-provisioned named tunnel is a single Cloudflare-side connector
// set, and tailport running it twice for two different local ports would
// leave both routes pointed at the same tunnel with no way to tell which
// serves which -- so name entry, re-raise, and confirmTunnelNamed all call
// this before starting one. It checks ONLY tailport's own currently-discovered
// m.tunnels (no foreign or process-table scan -- that's out of scope here,
// same as the rest of this feature's ownership model): the same tunnel
// running elsewhere entirely (another machine, a system service, a dashboard
// connector) can't be seen this way, which the README documents as a limit
// rather than a guarantee this guard doesn't make.
func (m *model) tunnelNameInUse(name string, exceptPort int) int {
	if name == "" {
		return 0
	}
	for port, info := range m.tunnels {
		if port == exceptPort {
			continue
		}
		if info.mode == cftunnel.ModeNamed && info.name == name {
			return port
		}
	}
	return 0
}

// tunnelNameRefusedMsg is the shared refusal toast for tunnelNameInUse's two
// call sites (requestTunnelNamed and confirmTunnelNamed's last-moment
// recheck), kept in one place so the wording can't drift between them. Says
// "press O", not "o" (kata p7c5): the port already running this name is
// running it NAMED, and O -- not o -- is what tears down a named tunnel now.
func tunnelNameRefusedMsg(name string, port int) string {
	return fmt.Sprintf("tunnel %q is already running for :%d — a named tunnel serves one port; press O on :%d first", name, port, port)
}

// confirmTunnelQuick is the entryConfirmTunnelQuick "yes" path: spawn a quick
// tunnel. The URL isn't known yet (it appears via the poll), so the confirm
// named nothing and the flash says "starting…" -- and, since kata h2ef, the
// route row shows an animated spinner in the URL's place until it resolves
// (startTunnelSpinner). No memory to seed any more (kata p7c5): a repeat `o`
// on this port just runs this same path again.
func (m *model) confirmTunnelQuick() tea.Cmd {
	port := m.tunnelPort
	spec := cftunnel.Spec{Port: port, Mode: cftunnel.ModeQuick}
	m.clearTunnelFlow()
	m.pending = port
	return tea.Batch(
		m.setFlash(fmt.Sprintf("starting Cloudflare quick tunnel for :%d…", port), flashInfo),
		tunnelStartCmd(m.cfClient(), spec),
		m.startTunnelSpinner(port),
	)
}

// confirmTunnelNamed is the entryConfirmTunnelNamed "yes" path: run the
// operator's pre-provisioned named tunnel bound to the confirmed hostname.
// Re-checks the owned-only same-tunnel guard [R3] one last time -- state can
// have moved between the confirm opening and "y" landing (a poll, another
// port's O) -- before ever shelling out, and flashes the exact URL (named
// tunnels know their host up front, unlike quick's spinner-then-URL). No
// memory to seed any more (kata p7c5): config.yaml already IS the memory,
// via requestTunnelNamed re-reading it on every `O` press.
func (m *model) confirmTunnelNamed() tea.Cmd {
	port := m.tunnelPort
	host := m.tunnelHostname
	name := m.tunnelName
	if other := m.tunnelNameInUse(name, port); other != 0 {
		m.clearTunnelFlow()
		return m.setErr(tunnelNameRefusedMsg(name, other))
	}
	spec := cftunnel.Spec{Port: port, Mode: cftunnel.ModeNamed, TunnelName: name, Hostname: host}
	m.clearTunnelFlow()
	m.pending = port
	return tea.Batch(
		m.setFlash(fmt.Sprintf("starting Cloudflare tunnel https://%s…", host), flashInfo),
		tunnelStartCmd(m.cfClient(), spec),
	)
}

// clearTunnelFlow resets the tunnel-setup flow fields, so an aborted or
// completed flow leaves nothing behind.
func (m *model) clearTunnelFlow() {
	m.mode = entryNone
	m.tunnelPort = 0
	m.tunnelHostname = ""
	m.tunnelName = ""
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
