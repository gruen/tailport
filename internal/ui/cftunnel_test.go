package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// TestRequestTunnelGuards exercises the refuse/de-escalation guards that don't
// depend on the login state.
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

	// th05 RELAXED mutual exclusion: a funnelled port may ALSO be tunnelled now,
	// so requestTunnel proceeds to its own setup/confirm instead of refusing.
	// Pin the login state (not logged in -> quick confirm) so the branch is
	// deterministic regardless of the host's ~/.cloudflared.
	t.Run("funnel coexistence proceeds", func(t *testing.T) {
		t.Setenv("TUNNEL_ORIGIN_CERT", "")
		t.Setenv("HOME", t.TempDir())
		m := base()
		m.funnel = map[int]int{3000: 443}
		m.requestTunnel(3000)
		if m.mode != entryConfirmTunnelQuick {
			t.Errorf("funnel coexistence should proceed to tunnel setup, not refuse; mode=%v", m.mode)
		}
	})

	// Likewise a published port may ALSO be tunnelled now.
	t.Run("publish coexistence proceeds", func(t *testing.T) {
		t.Setenv("TUNNEL_ORIGIN_CERT", "")
		t.Setenv("HOME", t.TempDir())
		m := base()
		m.published = map[int]publishInfo{3000: {hostname: "x.example.com"}}
		m.requestTunnel(3000)
		if m.mode != entryConfirmTunnelQuick {
			t.Errorf("publish coexistence should proceed to tunnel setup, not refuse; mode=%v", m.mode)
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

	// already tunnelled -> immediate teardown (pending set, no confirm)
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

// TestRequestTunnelFreshSetup pins the login state via $HOME so the mode branch
// is deterministic: no cert.pem -> straight to the quick confirm; a cert.pem
// present -> the quick/named mode select.
func TestRequestTunnelFreshSetup(t *testing.T) {
	t.Setenv("TUNNEL_ORIGIN_CERT", "")

	t.Run("not logged in -> quick confirm", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		m := New(config.Config{})
		m.cfAvailable = true
		m.fqdn = "host.tailnet.ts.net"
		m.requestTunnel(3000)
		if m.mode != entryConfirmTunnelQuick {
			t.Errorf("not-logged-in mode = %v, want entryConfirmTunnelQuick", m.mode)
		}
		if m.tunnelPort != 3000 {
			t.Errorf("tunnelPort = %d, want 3000", m.tunnelPort)
		}
	})

	t.Run("logged in -> mode select", func(t *testing.T) {
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, ".cloudflared"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".cloudflared", "cert.pem"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", home)
		m := New(config.Config{})
		m.cfAvailable = true
		m.fqdn = "host.tailnet.ts.net"
		m.requestTunnel(3000)
		if m.mode != entryTunnelMode {
			t.Errorf("logged-in mode = %v, want entryTunnelMode", m.mode)
		}
	})
}

// TestRequestTunnelReraise: a port tunnelled earlier this session re-raises from
// memory straight to the confirm, skipping the setup prompts.
func TestRequestTunnelReraise(t *testing.T) {
	m := New(config.Config{})
	m.cfAvailable = true
	m.fqdn = "host.tailnet.ts.net"
	m.lastTunnel = map[int]tunnelMemory{
		3000: {mode: cftunnel.ModeNamed, hostname: "app.example.com", name: "web"},
	}
	m.requestTunnel(3000)
	if m.mode != entryConfirmTunnelNamed {
		t.Errorf("named re-raise mode = %v, want entryConfirmTunnelNamed", m.mode)
	}
	if m.tunnelHostname != "app.example.com" || m.tunnelName != "web" {
		t.Errorf("re-raise did not restore host/name: %q %q", m.tunnelHostname, m.tunnelName)
	}
}

// TestBarGroupsTunnelGating: the `o` key shows in the bottom bar only when
// cloudflared is available; it's always in the full groups() (documented in ?).
func TestBarGroupsTunnelGating(t *testing.T) {
	m := New(config.Config{})

	m.cfAvailable = true
	if !hasKeyInGroup(m.barGroups(false), "Toggle Service Exposure", "o") {
		t.Error("cfAvailable=true: `o` should appear in the Serve Toggles bar group")
	}

	m.cfAvailable = false
	if hasKeyInGroup(m.barGroups(false), "Toggle Service Exposure", "o") {
		t.Error("cfAvailable=false: `o` should be dropped from the bar")
	}
	// still documented in the full grouping regardless
	if !hasKeyInGroup(m.keys.groups(), "Toggle Service Exposure", "o") {
		t.Error("`o` should always be in the full groups() for the ? overlay")
	}
}

func TestValidTunnelHostname(t *testing.T) {
	ok := []string{"app.example.com", "api.corp.internal"}
	for _, s := range ok {
		if !validTunnelHostname(s) {
			t.Errorf("validTunnelHostname(%q) = false, want true", s)
		}
	}
	bad := []string{"", "nodot", "has space.com", "http://app.example.com", "app.example.com:8080", "a/b.com"}
	for _, s := range bad {
		if validTunnelHostname(s) {
			t.Errorf("validTunnelHostname(%q) = true, want false", s)
		}
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
	m.tunnelSetupMode = cftunnel.ModeQuick

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
