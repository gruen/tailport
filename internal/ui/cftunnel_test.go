package ui

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/gruen/tailport/internal/cftunnel"
	"github.com/gruen/tailport/internal/config"
	"github.com/gruen/tailport/internal/portscan"
)

// TestReachTunnel: an owned tunnel drives reachTunnel. th05 RELAXED the
// funnel/publish/tunnel mutual exclusion, so a coexisting funnel/publish no
// longer collapses to the retired drift (reachStale) state -- reach() now
// returns the WIDEST present public path (funnel > publish > tunnel).
func TestReachTunnel(t *testing.T) {
	tun := portItem{listening: true, tunnelActive: true, tunnelHostname: "app.example.com"}
	if got := tun.reach(); got != reachTunnel {
		t.Errorf("tunnel-only reach = %v, want reachTunnel", got)
	}
	// tunnel + funnel => funnel wins (widest public path)
	if got := (portItem{tunnelActive: true, funnelPublic: 443}).reach(); got != reachFunnel {
		t.Errorf("tunnel+funnel reach = %v, want reachFunnel", got)
	}
	// tunnel + publish => publish wins over tunnel
	if got := (portItem{tunnelActive: true, publishHostname: "x.example.com"}).reach(); got != reachPublish {
		t.Errorf("tunnel+publish reach = %v, want reachPublish", got)
	}
}

// TestTunnelRoute pins the tunnel down at the ROUTE level (route-scoped copy,
// kata th05 P5 -- replaces the retired aggregate plainDescription()/markerGlyph()
// and copyTargetURL tailnet-fallback): a tunnelled service's tunnel route carries
// the exact https URL and the orange ◈ marker once the host is known, and an
// empty URL (nothing to copy) while the quick tunnel is still starting.
func TestTunnelRoute(t *testing.T) {
	// host known AND ready -> the tunnel route carries the exact https URL and
	// ◈ marker. tunnelReady must be set: kata aprt marks a known-host tunnel
	// stale (▲) once it has zero ready edge connections, so a healthy tunnel
	// has to say so explicitly (see TestTunnelRouteNotReady for the other case).
	up := portItem{tunnelActive: true, tunnelHostname: "foo.trycloudflare.com", tunnelReady: true, port: portscan.Port{Number: 3000}}
	upRoutes := up.routes()
	r := upRoutes[len(upRoutes)-1]
	if r.kind != routeTunnel {
		t.Fatalf("last route kind = %v, want routeTunnel", r.kind)
	}
	if r.url != "https://foo.trycloudflare.com" {
		t.Errorf("tunnel route url (host known) = %q, want https://foo.trycloudflare.com", r.url)
	}
	if m := stripANSI(r.marker(false)); !strings.Contains(m, "◈") {
		t.Errorf("tunnel mono marker = %q, want to contain ◈", m)
	}
	// still starting -> empty url; route-scoped copy toasts "nothing to copy"
	// rather than the retired aggregate tailnet fallback.
	starting := portItem{tunnelActive: true, port: portscan.Port{Number: 3000}}
	sRoutes := starting.routes()
	sr := sRoutes[len(sRoutes)-1]
	if sr.kind != routeTunnel || sr.url != "" {
		t.Errorf("starting tunnel route = %+v, want routeTunnel with empty url", sr)
	}
}

// TestTunnelRouteNotReady is the regression test for the MEDIUM roborev
// carryover (kata aprt): reachability used to key off nonzero PID alone, so a
// tunnel with a known hostname but zero ready edge connections showed as
// unconditionally, permanently reachable. It must now render stale (▲),
// mirroring a dangling tailnet forward, rather than a plain ◈.
func TestTunnelRouteNotReady(t *testing.T) {
	notReady := portItem{tunnelActive: true, tunnelHostname: "foo.trycloudflare.com", tunnelReady: false, port: portscan.Port{Number: 3000}}
	routes := notReady.routes()
	r := routes[len(routes)-1]
	if r.kind != routeTunnel || !r.stale {
		t.Fatalf("not-ready tunnel route = %+v, want routeTunnel with stale=true", r)
	}
	if r.url != "https://foo.trycloudflare.com" {
		t.Errorf("not-ready tunnel route url = %q, want the hostname preserved", r.url)
	}
	if m := stripANSI(r.marker(false)); !strings.Contains(m, "▲") {
		t.Errorf("not-ready tunnel mono marker = %q, want it to contain ▲", m)
	}
}

// TestTunnelRouteForeign is the regression test for the HIGH roborev
// carryover (kata aprt): a foreign cloudflared (no tailport --logfile
// sentinel) covering a port used to be discarded outright -- invisible, no
// drift warning. It must now surface as its own tunnel route: no URL (never
// probed), the same attention-grabbing marker as stale, and a "· foreign"
// adornment distinguishing it from an owned-but-not-ready tunnel.
func TestTunnelRouteForeign(t *testing.T) {
	foreign := portItem{tunnelForeign: true, port: portscan.Port{Number: 3000}}
	routes := foreign.routes()
	r := routes[len(routes)-1]
	if r.kind != routeTunnel || !r.foreign {
		t.Fatalf("foreign tunnel route = %+v, want routeTunnel with foreign=true", r)
	}
	if r.url != "" {
		t.Errorf("foreign tunnel route url = %q, want empty (never probed)", r.url)
	}
	if m := stripANSI(r.marker(false)); !strings.Contains(m, "▲") {
		t.Errorf("foreign tunnel mono marker = %q, want it to contain ▲", m)
	}
}

// writeCertPem creates ~/.cloudflared/cert.pem under home, so
// cftunnel.LoggedIn() (which checks only that default location) reports true.
func writeCertPem(t *testing.T, home string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".cloudflared"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".cloudflared", "cert.pem"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRequestTunnelGuards exercises `o`'s refuse/de-escalation guards -- the
// ones it shares with requestTunnelNamed (busy, availability, foreign, :22,
// locked) plus its own quick-only de-escalation and the cross-key refusal
// (kata p7c5: `o` never touches a named tunnel running on the port).
func TestRequestTunnelGuards(t *testing.T) {
	base := func() model {
		m := New(config.Config{})
		m.cfAvailable = true
		m.fqdn = "host.tailnet.ts.net"
		return m
	}

	// not available -> stays entryNone, no pending
	t.Run("unavailable", func(t *testing.T) {
		m := base()
		m.cfAvailable = false
		m.requestTunnel(3000)
		if m.mode != entryNone || m.pending != 0 {
			t.Errorf("unavailable: mode=%v pending=%d", m.mode, m.pending)
		}
	})

	// :22 refused
	t.Run("ssh refused", func(t *testing.T) {
		m := base()
		m.requestTunnel(22)
		if m.mode != entryNone || m.pending != 0 {
			t.Errorf(":22: mode=%v pending=%d", m.mode, m.pending)
		}
	})

	// th05 RELAXED mutual exclusion: a funnelled port may ALSO be tunnelled
	// now, so requestTunnel proceeds straight to the quick confirm (kata
	// p7c5: no mode prompt, regardless of login state) instead of refusing.
	t.Run("funnel coexistence proceeds", func(t *testing.T) {
		m := base()
		m.funnel = map[int]int{3000: 443}
		m.requestTunnel(3000)
		if m.mode != entryConfirmTunnelQuick {
			t.Errorf("funnel coexistence should proceed to the quick confirm, not refuse; mode=%v", m.mode)
		}
	})

	// Likewise a published port may ALSO be tunnelled now.
	t.Run("publish coexistence proceeds", func(t *testing.T) {
		m := base()
		m.published = map[int]publishInfo{3000: {hostname: "x.example.com"}}
		m.requestTunnel(3000)
		if m.mode != entryConfirmTunnelQuick {
			t.Errorf("publish coexistence should proceed to the quick confirm, not refuse; mode=%v", m.mode)
		}
	})

	// locked port refused
	t.Run("locked", func(t *testing.T) {
		m := base()
		m.cfg.Ports = map[int]config.PortMeta{3000: {Locked: true}}
		m.requestTunnel(3000)
		if m.mode != entryNone {
			t.Errorf("locked should refuse; mode=%v", m.mode)
		}
	})

	// already tunnelled QUICK -> immediate teardown (pending set, no confirm)
	t.Run("de-escalation", func(t *testing.T) {
		m := base()
		m.tunnels = map[int]tunnelInfo{3000: {pid: 4242, mode: cftunnel.ModeQuick}}
		if cmd := m.requestTunnel(3000); cmd == nil {
			t.Error("de-escalation should return a stop cmd")
		}
		if m.pending != 3000 {
			t.Errorf("de-escalation should set pending=3000; got %d", m.pending)
		}
		if m.mode != entryNone {
			t.Errorf("de-escalation should not open a modal; mode=%v", m.mode)
		}
	})

	// cross-key refusal (kata p7c5): a NAMED tunnel running on this port
	// belongs to `O` -- o must refuse it, exact wording, rather than tearing
	// it down or acting on the wrong mode.
	t.Run("named tunnel on this port refused, points at O", func(t *testing.T) {
		m := base()
		m.tunnels = map[int]tunnelInfo{3000: {pid: 4242, mode: cftunnel.ModeNamed, name: "web"}}
		cmd := m.requestTunnel(3000)
		if m.mode != entryNone || m.pending != 0 {
			t.Errorf("cross-key refusal should not open a modal or start an op: mode=%v pending=%d", m.mode, m.pending)
		}
		if cmd == nil {
			t.Fatal("requestTunnel should still return the error-toast cmd")
		}
		want := "a named tunnel is running on :3000 — press O to stop it"
		if m.flash != want {
			t.Errorf("flash = %q, want %q", m.flash, want)
		}
	})

	// a FOREIGN cloudflared on this port is refused (kata aprt, HIGH roborev
	// carryover): AGENTS.md requires it surface as drift and block a second
	// exposure, not stay invisible and let tailport pile a competing tunnel on
	// top of it.
	t.Run("foreign tunnel on this port refused", func(t *testing.T) {
		m := base()
		m.tunnelForeign = map[int]bool{3000: true}
		m.requestTunnel(3000)
		if m.mode != entryNone || m.pending != 0 {
			t.Errorf("foreign tunnel should refuse without opening a modal or starting an op: mode=%v pending=%d", m.mode, m.pending)
		}
	})
}

// TestRequestTunnelAlwaysGoesStraightToQuickConfirm pins kata p7c5's core
// claim for `o`: there is no more mode select, EVER -- not even for a
// logged-in user (a cert.pem present). o always resolves straight to
// entryConfirmTunnelQuick.
func TestRequestTunnelAlwaysGoesStraightToQuickConfirm(t *testing.T) {
	t.Setenv("TUNNEL_ORIGIN_CERT", "")

	t.Run("not logged in", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		m := New(config.Config{})
		m.cfAvailable = true
		m.fqdn = "host.tailnet.ts.net"
		m.requestTunnel(3000)
		if m.mode != entryConfirmTunnelQuick {
			t.Errorf("mode = %v, want entryConfirmTunnelQuick", m.mode)
		}
		if m.tunnelPort != 3000 {
			t.Errorf("tunnelPort = %d, want 3000", m.tunnelPort)
		}
	})

	t.Run("logged in (cert.pem present)", func(t *testing.T) {
		home := t.TempDir()
		writeCertPem(t, home)
		t.Setenv("HOME", home)
		m := New(config.Config{})
		m.cfAvailable = true
		m.fqdn = "host.tailnet.ts.net"
		m.requestTunnel(3000)
		if m.mode != entryConfirmTunnelQuick {
			t.Errorf("mode = %v, want entryConfirmTunnelQuick (no mode prompt any more)", m.mode)
		}
	})
}

// TestRequestTunnelNamedUnbound: O on a port with no cloudflare: binding (or
// an incomplete one) refuses with a toast explaining how to add it -- never a
// prompt (kata p7c5: config.yaml is the ONLY input `O` has).
func TestRequestTunnelNamedUnbound(t *testing.T) {
	t.Run("no binding at all", func(t *testing.T) {
		m := New(config.Config{})
		m.cfAvailable = true
		cmd := m.requestTunnelNamed(3000)
		if m.mode != entryNone || m.pending != 0 {
			t.Errorf("unbound should refuse without opening a modal: mode=%v pending=%d", m.mode, m.pending)
		}
		if cmd == nil {
			t.Fatal("requestTunnelNamed should still return the error-toast cmd")
		}
		want := ":3000 has no named tunnel — add ports.3000.cloudflare {tunnel, hostname} to config.yaml"
		if m.flash != want {
			t.Errorf("flash = %q, want %q", m.flash, want)
		}
	})

	t.Run("incomplete binding (empty hostname)", func(t *testing.T) {
		m := New(config.Config{Ports: map[int]config.PortMeta{
			3000: {Cloudflare: &config.CloudflareBinding{Tunnel: "web"}},
		}})
		m.cfAvailable = true
		m.requestTunnelNamed(3000)
		want := ":3000 has no named tunnel — add ports.3000.cloudflare {tunnel, hostname} to config.yaml"
		if m.flash != want {
			t.Errorf("flash = %q, want %q", m.flash, want)
		}
	})
}

// TestRequestTunnelNamedNoCertPem: O on a bound port refuses when
// cftunnel.LoggedIn() is false (no ~/.cloudflared/cert.pem) -- a named tunnel
// needs an authenticated account, and there is no fallback to a quick tunnel
// under O (that's o's job).
func TestRequestTunnelNamedNoCertPem(t *testing.T) {
	t.Setenv("TUNNEL_ORIGIN_CERT", "")
	t.Setenv("HOME", t.TempDir()) // no .cloudflared/cert.pem here

	m := New(config.Config{Ports: map[int]config.PortMeta{
		3000: {Cloudflare: &config.CloudflareBinding{Tunnel: "web", Hostname: "app.example.com"}},
	}})
	m.cfAvailable = true
	cmd := m.requestTunnelNamed(3000)
	if m.mode != entryNone {
		t.Errorf("no cert.pem should refuse without opening a modal; mode=%v", m.mode)
	}
	if cmd == nil {
		t.Fatal("requestTunnelNamed should still return the error-toast cmd")
	}
	want := "named tunnels need `cloudflared tunnel login` first"
	if m.flash != want {
		t.Errorf("flash = %q, want %q", m.flash, want)
	}
}

// TestRequestTunnelNamedInvalidBinding: a bound tunnel name or hostname that
// fails cftunnel.ValidTunnelName/ValidHostname refuses -- the binding is a
// hand-editable file value, not something just typed into a confirm, so it's
// re-validated exactly like Start would validate it.
func TestRequestTunnelNamedInvalidBinding(t *testing.T) {
	home := t.TempDir()
	writeCertPem(t, home)
	t.Setenv("HOME", home)
	t.Setenv("TUNNEL_ORIGIN_CERT", "")

	t.Run("invalid tunnel name", func(t *testing.T) {
		m := New(config.Config{Ports: map[int]config.PortMeta{
			3000: {Cloudflare: &config.CloudflareBinding{Tunnel: "-bad name", Hostname: "app.example.com"}},
		}})
		m.cfAvailable = true
		m.requestTunnelNamed(3000)
		if m.mode != entryNone {
			t.Errorf("invalid tunnel name should refuse; mode=%v", m.mode)
		}
		if !strings.Contains(m.flash, "ports.3000.cloudflare.tunnel") || !strings.Contains(m.flash, "-bad name") {
			t.Errorf("flash = %q, want it to name the field and the bad value", m.flash)
		}
	})

	t.Run("invalid hostname", func(t *testing.T) {
		m := New(config.Config{Ports: map[int]config.PortMeta{
			3000: {Cloudflare: &config.CloudflareBinding{Tunnel: "web", Hostname: "not a host"}},
		}})
		m.cfAvailable = true
		m.requestTunnelNamed(3000)
		if m.mode != entryNone {
			t.Errorf("invalid hostname should refuse; mode=%v", m.mode)
		}
		if !strings.Contains(m.flash, "ports.3000.cloudflare.hostname") || !strings.Contains(m.flash, "not a host") {
			t.Errorf("flash = %q, want it to name the field and the bad value", m.flash)
		}
	})
}

// TestRequestTunnelNamedGoesToConfirmAndStarts covers the happy path: a
// well-formed binding, plus a logged-in account, goes straight to
// entryConfirmTunnelNamed with the config's tunnel/hostname (no prompt), and
// "y" (confirmTunnelNamed) starts it -- checked via m.flash/m.pending, per
// AGENTS.md rule A4, NEVER by invoking the returned tea.Cmd (that would exec
// a real cloudflared).
func TestRequestTunnelNamedGoesToConfirmAndStarts(t *testing.T) {
	home := t.TempDir()
	writeCertPem(t, home)
	t.Setenv("HOME", home)
	t.Setenv("TUNNEL_ORIGIN_CERT", "")

	m := New(config.Config{Ports: map[int]config.PortMeta{
		3000: {Cloudflare: &config.CloudflareBinding{Tunnel: "web", Hostname: "app.example.com"}},
	}})
	m.cfAvailable = true
	m.fqdn = "host.tailnet.ts.net"

	m.requestTunnelNamed(3000)
	if m.mode != entryConfirmTunnelNamed {
		t.Fatalf("mode = %v, want entryConfirmTunnelNamed", m.mode)
	}
	if m.tunnelPort != 3000 || m.tunnelHostname != "app.example.com" || m.tunnelName != "web" {
		t.Errorf("tunnel flow state = port=%d host=%q name=%q, want 3000/app.example.com/web",
			m.tunnelPort, m.tunnelHostname, m.tunnelName)
	}

	if cmd := m.confirmTunnelNamed(); cmd == nil {
		t.Fatal("confirmTunnelNamed should return a non-nil cmd")
	}
	if m.pending != 3000 {
		t.Errorf("pending = %d, want 3000", m.pending)
	}
	if !strings.Contains(m.flash, "starting Cloudflare tunnel https://app.example.com") {
		t.Errorf("flash = %q, want it to name the confirmed URL (the spec's Hostname)", m.flash)
	}
}

// TestRequestTunnelNamedDeEscalates: O on a port already running a NAMED
// tunnel tears it down immediately, no confirm; O on a port running a QUICK
// tunnel is the cross-key case and refuses, pointing at o.
func TestRequestTunnelNamedDeEscalates(t *testing.T) {
	t.Run("named -> teardown", func(t *testing.T) {
		m := New(config.Config{})
		m.cfAvailable = true
		m.tunnels = map[int]tunnelInfo{3000: {pid: 4242, mode: cftunnel.ModeNamed, name: "web"}}
		if cmd := m.requestTunnelNamed(3000); cmd == nil {
			t.Error("teardown should return a stop cmd")
		}
		if m.pending != 3000 {
			t.Errorf("pending = %d, want 3000", m.pending)
		}
		if m.mode != entryNone {
			t.Errorf("teardown should not open a modal; mode=%v", m.mode)
		}
	})

	t.Run("quick -> refused, points at o", func(t *testing.T) {
		m := New(config.Config{})
		m.cfAvailable = true
		m.tunnels = map[int]tunnelInfo{3000: {pid: 4242, mode: cftunnel.ModeQuick}}
		cmd := m.requestTunnelNamed(3000)
		if m.mode != entryNone || m.pending != 0 {
			t.Errorf("cross-key refusal should not open a modal or start an op: mode=%v pending=%d", m.mode, m.pending)
		}
		if cmd == nil {
			t.Fatal("requestTunnelNamed should still return the error-toast cmd")
		}
		want := "a quick tunnel is running on :3000 — press o to stop it"
		if m.flash != want {
			t.Errorf("flash = %q, want %q", m.flash, want)
		}
	})
}

// TestTunnelNameInUseRefused is the regression test for audit item 6 (kata
// nc1j, R3): nothing used to stop two ports from running the same named
// tunnel. The owned-only same-tunnel guard must refuse at all three call
// sites -- name entry, re-raise, and confirmTunnelNamed's last check.
func TestTunnelNameInUseRefused(t *testing.T) {
	const wantMsg = `tunnel "web" is already running for :4000`

	// requestTunnelNamed: two ports bound to the SAME tunnel name in
	// config.yaml, one already running it -- O on the OTHER port is refused
	// (kata p7c5: config.yaml replaced the old name-entry prompt as the
	// guard's call site, but the guard itself, tunnelNameInUse, is unchanged).
	t.Run("requestTunnelNamed", func(t *testing.T) {
		home := t.TempDir()
		writeCertPem(t, home)
		t.Setenv("HOME", home)
		t.Setenv("TUNNEL_ORIGIN_CERT", "")

		m := New(config.Config{Ports: map[int]config.PortMeta{
			3000: {Cloudflare: &config.CloudflareBinding{Tunnel: "web", Hostname: "other.example.com"}},
			4000: {Cloudflare: &config.CloudflareBinding{Tunnel: "web", Hostname: "app.example.com"}},
		}})
		m.cfAvailable = true
		m.fqdn = "host.tailnet.ts.net"
		m.tunnels = map[int]tunnelInfo{4000: {mode: cftunnel.ModeNamed, pid: 222, name: "web", hostname: "app.example.com"}}

		cmd := m.requestTunnelNamed(3000)
		if m.mode != entryNone {
			t.Errorf("refused O should not open a modal; mode=%v", m.mode)
		}
		if cmd == nil {
			t.Fatal("requestTunnelNamed should still return the error-toast cmd")
		}
		if !strings.Contains(m.flash, wantMsg) {
			t.Errorf("flash = %q, want it to contain %q", m.flash, wantMsg)
		}
	})

	t.Run("confirm", func(t *testing.T) {
		m := New(config.Config{})
		m.cfAvailable = true
		m.fqdn = "host.tailnet.ts.net"
		m.tunnels = map[int]tunnelInfo{4000: {mode: cftunnel.ModeNamed, pid: 222, name: "web", hostname: "app.example.com"}}
		m.tunnelPort = 3000
		m.tunnelHostname = "other.example.com"
		m.tunnelName = "web"
		cmd := m.confirmTunnelNamed()
		if m.mode != entryNone || m.pending != 0 {
			t.Errorf("refused confirm should abort the flow with no pending op; mode=%v pending=%d", m.mode, m.pending)
		}
		if cmd == nil {
			t.Fatal("confirmTunnelNamed should still return the error-toast cmd")
		}
		if !strings.Contains(m.flash, wantMsg) {
			t.Errorf("flash = %q, want it to contain %q", m.flash, wantMsg)
		}
	})
}

// TestNamedConfirmNamesTunnelAndCaveat is the regression test for audit item 5
// (kata nc1j): the named confirm used to show only the hostname the operator
// typed, with no way to tell WHICH tunnel it would run or that tailport can't
// verify the hostname is actually routed to it. Checks the prompt string
// only -- no hard-coded heights (kata cp2c).
func TestNamedConfirmNamesTunnelAndCaveat(t *testing.T) {
	m := New(config.Config{})
	m.mode = entryConfirmTunnelNamed
	m.tunnelPort = 3000
	m.tunnelHostname = "app.example.com"
	m.tunnelName = "web"

	view := stripANSI(m.renderBottom())
	if !strings.Contains(view, `via tunnel "web"`) {
		t.Errorf("confirm should name the tunnel; view:\n%s", view)
	}
	if !strings.Contains(view, "(from config.yaml)") {
		t.Errorf("confirm should say the tunnel came from config.yaml (kata p7c5); view:\n%s", view)
	}
	if !strings.Contains(view, "hostname unverified") {
		t.Errorf("confirm should carry the routing caveat; view:\n%s", view)
	}
	if !strings.Contains(view, "https://app.example.com") {
		t.Errorf("confirm should still name the exact URL; view:\n%s", view)
	}
}

// TestNamedConfirmFitsEightyColumns: every confirm line must fit an 80-column
// terminal for a typical host/name -- a wider line soft-wraps past what
// lipgloss.Height counts and pushes the header off-screen (kata nc1j review).
func TestNamedConfirmFitsEightyColumns(t *testing.T) {
	m := New(config.Config{})
	m.mode = entryConfirmTunnelNamed
	m.tunnelPort = 3000
	m.tunnelHostname = "app.example.com"
	m.tunnelName = "web"
	for _, line := range strings.Split(stripANSI(m.renderBottom()), "\n") {
		if w := lipgloss.Width(line); w > 80 {
			t.Errorf("confirm line is %d columns (> 80): %q", w, line)
		}
	}
}

// TestTunnelStartClearsStoppingMark: a fresh start on a port the user stopped
// earlier must clear that stop's suppression mark, or the NEW tunnel's own
// later exit would be swallowed silently (kata nc1j W3b review).
func TestTunnelStartClearsStoppingMark(t *testing.T) {
	m := New(config.Config{})
	m.cfAvailable = true
	m.tunnelStopping[3000] = true
	running := &cftunnel.Running{Port: 3000, PID: 4242, Mode: cftunnel.ModeQuick, Owned: true}
	next, _ := m.Update(tunnelDoneMsg{port: 3000, running: running})
	if nm := next.(model); nm.tunnelStopping[3000] {
		t.Error("tunnelStopping[3000] should be cleared by a successful start")
	}
}

// TestNamedStartFlashNamesURL covers confirmTunnelNamed's flash: unlike a
// quick tunnel (whose URL isn't known until it starts), a named tunnel's
// host is known up front, so the "starting…" flash should name it rather
// than just the port.
func TestNamedStartFlashNamesURL(t *testing.T) {
	m := New(config.Config{})
	m.cfAvailable = true
	m.tunnelPort = 3000
	m.tunnelHostname = "app.example.com"
	m.tunnelName = "web"

	if cmd := m.confirmTunnelNamed(); cmd == nil {
		t.Fatal("confirmTunnelNamed should return a non-nil cmd")
	}
	if !strings.Contains(m.flash, "starting Cloudflare tunnel https://app.example.com") {
		t.Errorf("flash = %q, want it to name the URL", m.flash)
	}
}

// TestBarGroupsTunnelGating: `o` shows in the bottom bar only when
// cloudflared is available; `O` NEVER shows there regardless (kata p7c5,
// mirrors Redo); both are always in the full groups() (documented in ?).
func TestBarGroupsTunnelGating(t *testing.T) {
	m := New(config.Config{})

	m.cfAvailable = true
	if !hasKeyInGroup(m.barGroups(false), "Toggle Service Exposure", "o") {
		t.Error("cfAvailable=true: `o` should appear in the Serve Toggles bar group")
	}
	if hasKeyInGroup(m.barGroups(false), "Toggle Service Exposure", "O") {
		t.Error("`O` should never appear in the bar, even when cfAvailable=true")
	}

	m.cfAvailable = false
	if hasKeyInGroup(m.barGroups(false), "Toggle Service Exposure", "o") {
		t.Error("cfAvailable=false: `o` should be dropped from the bar")
	}
	if hasKeyInGroup(m.barGroups(false), "Toggle Service Exposure", "O") {
		t.Error("`O` should never appear in the bar, even when cfAvailable=false")
	}
	// still documented in the full grouping regardless
	if !hasKeyInGroup(m.keys.groups(), "Toggle Service Exposure", "o") {
		t.Error("`o` should always be in the full groups() for the ? overlay")
	}
	if !hasKeyInGroup(m.keys.groups(), "Toggle Service Exposure", "O") {
		t.Error("`O` should always be in the full groups() for the ? overlay")
	}
}

// TestHelpOverlayDocumentsTunnelNamed pins that `O`'s description is actually
// reachable through the SAME source the "?" overlay and `tailport
// quickstart` both render from (KeyLegendGroups/keyLegendDescs) -- not just
// present in groups() with no prose to show for it.
func TestHelpOverlayDocumentsTunnelNamed(t *testing.T) {
	found := false
	for _, g := range KeyLegendGroups(false) {
		for _, r := range g.Rows {
			if r.Key == "O" {
				found = true
				if r.Desc == "" {
					t.Error(`the "?" overlay's O row has no description`)
				}
				if !strings.Contains(r.Desc, "NAMED Cloudflare Tunnel") {
					t.Errorf("O's description should explain the named tunnel; got %q", r.Desc)
				}
			}
		}
	}
	if !found {
		t.Error(`expected an "O" row in KeyLegendGroups (the "?" overlay / quickstart source)`)
	}
}

// TestTunnelDoneInvalidatesInFlightPoll is the regression test for the MEDIUM
// roborev carryover (kata aprt): a start/stop op used to mutate m.tunnels
// directly without invalidating any poll already in flight when it landed. A
// stale poll (snapshotted before the op) landing AFTER could erase a
// just-started tunnel or resurrect a just-torn-down one. Mirrors
// TestPublishPollOutOfOrderDropsStale's gen-based technique.
func TestTunnelDoneInvalidatesInFlightPoll(t *testing.T) {
	m := New(config.Config{})
	m.cfAvailable = true

	// Simulate a poll already in flight (gen 1) when the start lands.
	m.tunnelPollGen = 1

	m = mustUpdate(t, m, tunnelDoneMsg{port: 3000, running: &cftunnel.Running{PID: 111, Port: 3000, Mode: cftunnel.ModeQuick}})
	if _, ok := m.tunnels[3000]; !ok {
		t.Fatalf("start success should record the tunnel immediately")
	}

	// The STALE poll (gen 1, issued before the start) arrives late with an
	// empty map -- it must not erase what the start just applied.
	m = mustUpdate(t, m, tunnelPollMsg{gen: 1, tunnels: map[int]tunnelInfo{}})
	if _, ok := m.tunnels[3000]; !ok {
		t.Errorf("a stale in-flight poll must not erase a just-started tunnel; got %#v", m.tunnels)
	}

	// Tear it down; a poll issued before the STOP (but after the start, so its
	// gen is "fresh" relative to the start) must not resurrect it once the
	// stop lands.
	preStopGen := m.tunnelPollGen
	m = mustUpdate(t, m, tunnelDoneMsg{port: 3000, torndown: true})
	if _, ok := m.tunnels[3000]; ok {
		t.Fatalf("torn-down tunnel should be removed immediately")
	}
	m = mustUpdate(t, m, tunnelPollMsg{gen: preStopGen, tunnels: map[int]tunnelInfo{3000: {pid: 111}}})
	if _, ok := m.tunnels[3000]; ok {
		t.Errorf("a stale in-flight poll must not resurrect a just-torn-down tunnel; got %#v", m.tunnels)
	}
}

// TestTunnelStartupCmdGatedOnAvailability is the regression test for the LOW
// roborev carryover (kata aprt): the tunnel ticker used to reschedule itself
// forever even when cloudflared is unavailable, contradicting the "zero cost
// when absent" gate. tunnelStartupCmd (Init's entry point) must be a true nil
// -- no poll, no ticker -- in that case, and a real batch otherwise.
func TestTunnelStartupCmdGatedOnAvailability(t *testing.T) {
	m := New(config.Config{})
	m.cfAvailable = false
	if cmd := m.tunnelStartupCmd(); cmd != nil {
		t.Error("tunnelStartupCmd should be nil (no poll, no ticker) when cloudflared is unavailable")
	}
	m.cfAvailable = true
	if cmd := m.tunnelStartupCmd(); cmd == nil {
		t.Error("tunnelStartupCmd should batch the poll + ticker when cloudflared is available")
	}
}

// TestTunnelTickStopsWhenUnavailable is the defensive-check half of the same
// fix: even if something reached tunnelTickMsg while unavailable, it must not
// reschedule (return a nil cmd) rather than perpetuating the ticker.
func TestTunnelTickStopsWhenUnavailable(t *testing.T) {
	m := New(config.Config{})
	m.cfAvailable = false
	_, cmd := m.Update(tunnelTickMsg{})
	if cmd != nil {
		t.Error("tunnelTickMsg should not reschedule when cloudflared is unavailable")
	}
}

// TestTunnelSpinnerStartsTicksAndStops covers kata h2ef's full spinner
// lifecycle: confirmTunnelQuick arms it (bumping tunnelSpinnerID off zero and
// targeting the port), each tick advances the frame and reschedules while the
// port's hostname stays empty, a STALE-generation tick is ignored outright
// (no frame advance, no reschedule -- the flashID-style guard), and the tick
// self-stops (nil cmd, tunnelSpinnerPort cleared) the moment the hostname
// resolves -- so it never redraws forever once the URL is known.
func TestTunnelSpinnerStartsTicksAndStops(t *testing.T) {
	m := New(config.Config{})
	m.cfAvailable = true
	m.fqdn = "host.tailnet.ts.net"
	m.tunnelPort = 3000

	if cmd := m.confirmTunnelQuick(); cmd == nil {
		t.Fatal("confirmTunnelQuick should return a non-nil batched cmd")
	}
	if m.tunnelSpinnerPort != 3000 {
		t.Errorf("tunnelSpinnerPort = %d, want 3000", m.tunnelSpinnerPort)
	}
	startID := m.tunnelSpinnerID
	if startID == 0 {
		t.Fatal("startTunnelSpinner should bump tunnelSpinnerID off its zero value")
	}

	// Simulate the start landing (mirrors tunnelDoneMsg's start-success
	// branch): the port is now tracked but its quick hostname is still empty.
	m.tunnels = map[int]tunnelInfo{3000: {mode: cftunnel.ModeQuick}}

	res, tick := m.Update(tunnelSpinnerTickMsg{id: startID})
	m = res.(model)
	if tick == nil {
		t.Error("a tick while the hostname is still empty should reschedule")
	}
	if m.tunnelSpinnerFrame != 1 {
		t.Errorf("tunnelSpinnerFrame = %d, want 1 after one tick", m.tunnelSpinnerFrame)
	}
	if m.tunnelSpinnerPort != 3000 {
		t.Error("tunnelSpinnerPort should stay set while the hostname is still empty")
	}

	// A stale-generation tick must be ignored: no frame advance, no reschedule.
	res, staleTick := m.Update(tunnelSpinnerTickMsg{id: startID - 1})
	m2 := res.(model)
	if staleTick != nil {
		t.Error("a stale-generation tick must not reschedule")
	}
	if m2.tunnelSpinnerFrame != m.tunnelSpinnerFrame {
		t.Error("a stale-generation tick must not advance the frame")
	}

	// The hostname resolves (mirrors a poll landing) -- the NEXT tick must
	// stop: no reschedule, and the target port cleared.
	m.tunnels[3000] = tunnelInfo{mode: cftunnel.ModeQuick, hostname: "witty-fox-42.trycloudflare.com"}
	res, stopTick := m.Update(tunnelSpinnerTickMsg{id: startID})
	m = res.(model)
	if stopTick != nil {
		t.Error("a tick after the hostname resolves should NOT reschedule (spinner must self-stop)")
	}
	if m.tunnelSpinnerPort != 0 {
		t.Errorf("tunnelSpinnerPort should clear once resolved; got %d", m.tunnelSpinnerPort)
	}
}

// TestTunnelSpinnerStopsOnErrorOrTeardown covers kata h2ef's other stop path:
// if a quick tunnel never produces a hostname at all -- the start itself
// fails, or it's torn down before resolving -- the spinner target must clear
// immediately (in the tunnelDoneMsg handler) rather than waiting on a tick
// that will never see a hostname. An unrelated port's error/teardown must
// leave a DIFFERENT port's still-pending spinner target alone.
func TestTunnelSpinnerStopsOnErrorOrTeardown(t *testing.T) {
	t.Run("start error", func(t *testing.T) {
		m := New(config.Config{})
		m.cfAvailable = true
		m.tunnelSpinnerPort = 3000
		m.tunnelSpinnerID = 1
		m = mustUpdate(t, m, tunnelDoneMsg{port: 3000, err: cftunnel.ErrNotInstalled})
		if m.tunnelSpinnerPort != 0 {
			t.Errorf("a failed start should clear tunnelSpinnerPort; got %d", m.tunnelSpinnerPort)
		}
	})
	t.Run("torn down", func(t *testing.T) {
		m := New(config.Config{})
		m.cfAvailable = true
		m.tunnelSpinnerPort = 3000
		m.tunnelSpinnerID = 1
		m.tunnels = map[int]tunnelInfo{3000: {mode: cftunnel.ModeQuick}}
		m = mustUpdate(t, m, tunnelDoneMsg{port: 3000, torndown: true})
		if m.tunnelSpinnerPort != 0 {
			t.Errorf("a teardown should clear tunnelSpinnerPort; got %d", m.tunnelSpinnerPort)
		}
	})
	t.Run("unrelated port is left alone", func(t *testing.T) {
		m := New(config.Config{})
		m.cfAvailable = true
		m.tunnelSpinnerPort = 3000
		m.tunnelSpinnerID = 1
		m = mustUpdate(t, m, tunnelDoneMsg{port: 4000, err: cftunnel.ErrNotInstalled})
		if m.tunnelSpinnerPort != 3000 {
			t.Errorf("a DIFFERENT port's failure must not clear this port's spinner target; got %d", m.tunnelSpinnerPort)
		}
	})
}

// TestTunnelStartMovesSelectionToNewRoute covers kata h2ef's second claim:
// when a tunnel start succeeds, the selection moves to that port's NEW
// cloudflare route sub-row (not just the service), via selectRoute, so the
// user watches the URL -- or, for a quick tunnel, the spinner then the URL --
// resolve right where they're already looking.
func TestTunnelStartMovesSelectionToNewRoute(t *testing.T) {
	m := New(config.Config{})
	m.cfAvailable = true
	m.allPorts = []portscan.Port{
		{Number: 2000, Process: "other", BindScope: portscan.ScopeLoopback},
		{Number: 3000, Process: "app", BindScope: portscan.ScopeLoopback},
	}
	m.showAllPorts = true
	m.rebuildItems()
	// Selection starts on the FIRST service (index 0, port :2000) -- unrelated
	// to the port that's about to get a tunnel.
	if m.list.Index() != 0 {
		t.Fatalf("setup: expected the initial selection on index 0; got %d", m.list.Index())
	}

	m = mustUpdate(t, m, tunnelDoneMsg{port: 3000, running: &cftunnel.Running{PID: 111, Port: 3000, Mode: cftunnel.ModeQuick}})

	pi, _, ok := m.currentService()
	if !ok || pi.port.Number != 3000 {
		t.Fatalf("selection should move to port :3000's service; ok=%v port=%+v", ok, pi.port)
	}
	routes := pi.routes()
	if m.routeIdx < 0 || m.routeIdx >= len(routes) || routes[m.routeIdx].kind != routeTunnel {
		t.Fatalf("routeIdx = %d should select the routeTunnel sub-row; routes=%+v", m.routeIdx, routes)
	}
}

// nonexistentCFClient returns a client whose Binary matches no real process
// on the test box, so Discover() finds nothing owned -- used by the W3b
// vanish tests below to get a deterministic "the tunnel is gone" poll
// without a fake binary (isCloudflaredArgv0 also always accepts the literal
// "cloudflared", but no test box runs one of those under this name either).
func nonexistentCFClient() *cftunnel.Client {
	return &cftunnel.Client{Binary: "tailport-test-nonexistent-cloudflared"}
}

// reapedChildPid runs a trivial child to completion and returns its pid --
// by the time Run() returns, Wait() has already reaped it, so Alive(pid)
// reads false immediately (no zombie window to wait out). Mirrors
// internal/cftunnel's TestAlive.
func reapedChildPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("running a trivial child: %v", err)
	}
	return cmd.Process.Pid
}

// TestTunnelPollReportsExitedTunnel is the regression test for audit item 4
// (kata nc1j W3b): a tunnel that exits on its own (a late auth failure, a
// crash) used to just disappear from m.tunnels with no explanation on the
// next poll. The vanish detection lives INSIDE pollTunnelsCmd's tea.Cmd (per
// AGENTS.md's process-supervision model, never in Update), so this drives it
// end to end: a reaped pid (genuinely gone, no zombie ambiguity) plus a
// console file holding the R1 case-1 text must surface as an "exited" toast
// naming that text.
func TestTunnelPollReportsExitedTunnel(t *testing.T) {
	m := New(config.Config{})
	m.cfAvailable = true
	m.cfClientOverride = nonexistentCFClient()

	pid := reapedChildPid(t)
	consolePath := filepath.Join(t.TempDir(), "cftunnel-3000.console")
	if err := os.WriteFile(consolePath, []byte("2026-09-27T01:00:00Z ERR Cannot determine default origin certificate path\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.tunnels = map[int]tunnelInfo{3000: {mode: cftunnel.ModeQuick, pid: pid, consolePath: consolePath}}

	cmd := m.pollTunnelsCmd()
	if cmd == nil {
		t.Fatal("pollTunnelsCmd should be non-nil when cfAvailable")
	}
	msg, ok := cmd().(tunnelPollMsg)
	if !ok {
		t.Fatalf("cmd() = %#v, want tunnelPollMsg", msg)
	}
	if len(msg.vanished) != 1 || msg.vanished[0].port != 3000 {
		t.Fatalf("vanished = %#v, want exactly port 3000", msg.vanished)
	}
	if msg.vanished[0].alive {
		t.Error("a fully-reaped pid should not read as alive")
	}

	m = mustUpdate(t, m, msg)
	want := "Cloudflare tunnel on :3000 exited — Cannot determine default origin certificate path"
	if m.flash != want {
		t.Errorf("flash = %q, want %q", m.flash, want)
	}
}

// TestTunnelPollExitSuppressedAfterUserStop is the regression test for the
// tunnelStopping race documented on the tunnelDoneMsg torndown branch: a
// port the user just tore down with `o` must never raise a false "exited"
// toast once a later poll confirms it's actually gone, and the suppression
// marker must clear itself once that confirmation lands (so it doesn't leak
// forever).
func TestTunnelPollExitSuppressedAfterUserStop(t *testing.T) {
	m := New(config.Config{})
	m.cfAvailable = true
	m.cfClientOverride = nonexistentCFClient()
	m.tunnels = map[int]tunnelInfo{3000: {mode: cftunnel.ModeQuick, pid: reapedChildPid(t)}}
	m.tunnelStopping = map[int]bool{3000: true}

	cmd := m.pollTunnelsCmd()
	msg, ok := cmd().(tunnelPollMsg)
	if !ok || len(msg.vanished) != 1 {
		t.Fatalf("setup: want exactly one vanished entry; got %#v (ok=%v)", msg.vanished, ok)
	}

	m = mustUpdate(t, m, msg)
	if m.flash != "" {
		t.Errorf("a user-initiated stop must not raise a vanish toast; flash = %q", m.flash)
	}
	if m.tunnelStopping[3000] {
		t.Error("tunnelStopping[3000] should clear once the fresh poll confirms the port is gone")
	}
}

// TestTunnelPollExitStopsSpinner is the regression test for audit item 8
// (kata nc1j): a quick tunnel that dies before its hostname ever resolves
// used to leave the pending spinner animating forever, since the tick loop
// only self-stops once a hostname appears. A vanished port must clear
// tunnelSpinnerPort even when it's the ONLY thing that changed.
func TestTunnelPollExitStopsSpinner(t *testing.T) {
	m := New(config.Config{})
	m.cfAvailable = true
	m.cfClientOverride = nonexistentCFClient()
	m.tunnelSpinnerPort = 3000
	m.tunnelSpinnerID = 1
	m.tunnels = map[int]tunnelInfo{3000: {mode: cftunnel.ModeQuick, pid: reapedChildPid(t)}}

	cmd := m.pollTunnelsCmd()
	msg, ok := cmd().(tunnelPollMsg)
	if !ok || len(msg.vanished) != 1 {
		t.Fatalf("setup: want exactly one vanished entry; got %#v (ok=%v)", msg.vanished, ok)
	}

	m = mustUpdate(t, m, msg)
	if m.tunnelSpinnerPort != 0 {
		t.Errorf("a vanished tunnel's spinner should stop; tunnelSpinnerPort = %d", m.tunnelSpinnerPort)
	}
}

// TestTunnelPollVanishedButAliveWarns is a handler-level test (constructing
// vanished directly, not going through pollTunnelsCmd's own zombie re-check):
// a port whose pid is STILL alive after that re-check means something other
// than a clean exit -- tailport can no longer identify it as its own
// cloudflared, but it may well still be serving traffic -- so the wording
// must differ from the "exited" case and point at `ps`.
func TestTunnelPollVanishedButAliveWarns(t *testing.T) {
	m := New(config.Config{})
	m.cfAvailable = true

	m = mustUpdate(t, m, tunnelPollMsg{
		gen:      m.tunnelPollGen,
		tunnels:  map[int]tunnelInfo{},
		vanished: []vanishedTunnel{{port: 3000, pid: 4242, alive: true}},
	})
	want := "Cloudflare tunnel on :3000 (pid 4242) is still running but tailport can no longer identify it — it may still be public; check ps"
	if m.flash != want {
		t.Errorf("flash = %q, want %q", m.flash, want)
	}
}

// TestTunnelPollStaleGenDropsVanished mirrors TestTunnelDoneInvalidatesInFlightPoll's
// technique for the vanish path specifically: a poll whose generation is
// already stale by the time it lands must be dropped in its entirety --
// including any vanished entries it carries -- never partially applied.
func TestTunnelPollStaleGenDropsVanished(t *testing.T) {
	m := New(config.Config{})
	m.cfAvailable = true
	m.tunnelPollGen = 5
	m.tunnelPollApplied = 5

	m = mustUpdate(t, m, tunnelPollMsg{
		gen:      3,
		tunnels:  map[int]tunnelInfo{},
		vanished: []vanishedTunnel{{port: 3000, pid: 1, alive: false, tail: "boom"}},
	})
	if m.flash != "" {
		t.Errorf("a stale-generation poll's vanished list must be dropped entirely; flash = %q", m.flash)
	}
}

// TestTunnelPollErrDropsVanish pins the quiet-degrade path: an errored poll
// (a transient /proc-scan hiccup) must never raise a vanish toast and must
// keep the last-known tunnels map, exactly like it already does for the
// tunnels/foreign maps. pollTunnelsCmd itself never populates vanished
// alongside err (see its early return), but the handler must not depend on
// that alone -- it returns before ever inspecting msg.vanished.
func TestTunnelPollErrDropsVanish(t *testing.T) {
	m := New(config.Config{})
	m.cfAvailable = true
	m.tunnels = map[int]tunnelInfo{3000: {pid: 111}}

	m = mustUpdate(t, m, tunnelPollMsg{
		gen:      1,
		err:      errors.New("boom"),
		vanished: []vanishedTunnel{{port: 3000, pid: 111, alive: false, tail: "should never surface"}},
	})
	if m.flash != "" {
		t.Errorf("an errored poll must never raise a vanish toast; flash = %q", m.flash)
	}
	if _, ok := m.tunnels[3000]; !ok {
		t.Error("an errored poll must keep the last-known tunnels map (quiet degrade)")
	}
}

// hasKeyInGroup reports whether group named gname contains a binding whose help
// key is want.
func hasKeyInGroup(groups []keyGroup, gname, want string) bool {
	for _, g := range groups {
		if g.name != gname {
			continue
		}
		for _, b := range g.bindings {
			if b.Help().Key == want {
				return true
			}
		}
	}
	return false
}
