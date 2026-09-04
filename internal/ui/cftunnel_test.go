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

// TestReachTunnel: an owned tunnel drives reachTunnel, and it collides with
// funnel/publish into the drift (reachStale) state -- the three-way extension of
// the funnel/publish mutual-exclusion drift case (kata nc1j).
func TestReachTunnel(t *testing.T) {
	tun := portItem{listening: true, tunnelActive: true, tunnelHostname: "app.example.com"}
	if got := tun.reach(); got != reachTunnel {
		t.Errorf("tunnel-only reach = %v, want reachTunnel", got)
	}
	// tunnel + funnel => drift
	if got := (portItem{tunnelActive: true, funnelPublic: 443}).reach(); got != reachStale {
		t.Errorf("tunnel+funnel reach = %v, want reachStale (drift)", got)
	}
	// tunnel + publish => drift
	if got := (portItem{tunnelActive: true, publishHostname: "x.example.com"}).reach(); got != reachStale {
		t.Errorf("tunnel+publish reach = %v, want reachStale (drift)", got)
	}
}

func TestTunnelDriftDescription(t *testing.T) {
	cases := []struct {
		it   portItem
		want string
	}{
		{portItem{funnelPublic: 443, tunnelActive: true}, "funnelled AND tunnelled — remove one"},
		{portItem{publishHostname: "h", tunnelActive: true}, "published AND tunnelled — remove one"},
		{portItem{funnelPublic: 443, publishHostname: "h"}, "funnelled AND published — remove one"},
		{portItem{funnelPublic: 443, publishHostname: "h", tunnelActive: true}, "multiple public exposures — remove all but one"},
	}
	for _, c := range cases {
		if got := c.it.plainDescription(); got != c.want {
			t.Errorf("drift desc = %q, want %q", got, c.want)
		}
	}
}

func TestTunnelDescriptionAndMarker(t *testing.T) {
	// quick tunnel still starting (no host yet)
	starting := portItem{tunnelActive: true}
	if got := starting.plainDescription(); got != "starting Cloudflare quick tunnel…" {
		t.Errorf("starting desc = %q", got)
	}
	// host known
	up := portItem{tunnelActive: true, tunnelHostname: "foo.trycloudflare.com"}
	if got := up.plainDescription(); got != "https://foo.trycloudflare.com · tunnelled to the internet" {
		t.Errorf("up desc = %q", got)
	}
	// mono marker is the orange ◈ (strip styling by checking the glyph is present)
	if m := stripANSI(up.markerGlyph()); !strings.Contains(m, "◈") {
		t.Errorf("tunnel mono marker = %q, want to contain ◈", m)
	}
}

func TestCopyTargetURLTunnel(t *testing.T) {
	m := New(config.Config{})
	m.host = "myhost"
	// host known -> copies the exact https URL
	up := portItem{tunnelActive: true, tunnelHostname: "foo.trycloudflare.com", port: portscan.Port{Number: 3000}}
	if got := m.copyTargetURL(up); got != "https://foo.trycloudflare.com" {
		t.Errorf("copy (host known) = %q", got)
	}
	// still starting -> falls back to the tailnet URL (never a blank https://)
	starting := portItem{tunnelActive: true, port: portscan.Port{Number: 3000}}
	if got := m.copyTargetURL(starting); got != "http://myhost:3000" {
		t.Errorf("copy (starting) = %q", got)
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

	// funnelled port refused
	t.Run("funnel exclusion", func(t *testing.T) {
		m := base()
		m.funnel = map[int]int{3000: 443}
		m.requestTunnel(3000)
		if m.mode != entryNone {
			t.Errorf("funnel exclusion should refuse; mode=%v", m.mode)
		}
	})

	// published port refused
	t.Run("publish exclusion", func(t *testing.T) {
		m := base()
		m.published = map[int]publishInfo{3000: {hostname: "x.example.com"}}
		m.requestTunnel(3000)
		if m.mode != entryNone {
			t.Errorf("publish exclusion should refuse; mode=%v", m.mode)
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

// TestBarGroupsTunnelGating: the `t` key shows in the bottom bar only when
// cloudflared is available; it's always in the full groups() (documented in ?).
func TestBarGroupsTunnelGating(t *testing.T) {
	m := New(config.Config{})

	m.cfAvailable = true
	if !hasKeyInGroup(m.barGroups(false), "Toggle Service Exposure", "t") {
		t.Error("cfAvailable=true: `t` should appear in the Serve Toggles bar group")
	}

	m.cfAvailable = false
	if hasKeyInGroup(m.barGroups(false), "Toggle Service Exposure", "t") {
		t.Error("cfAvailable=false: `t` should be dropped from the bar")
	}
	// still documented in the full grouping regardless
	if !hasKeyInGroup(m.keys.groups(), "Toggle Service Exposure", "t") {
		t.Error("`t` should always be in the full groups() for the ? overlay")
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
