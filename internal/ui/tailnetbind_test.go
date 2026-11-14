package ui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gruen/tailport/internal/config"
	"github.com/gruen/tailport/internal/portscan"
	"github.com/gruen/tailport/internal/procid"
	"github.com/gruen/tailport/internal/tsserve"
)

// Kata nvv6: an app bound straight to the node's tailnet IP is already
// reachable, so it shows with a tailnet route (not served) and `t` never
// offers to serve it; tailscaled's OWN unattributed tailnet-IP row on a served
// port must still not count as a listening app, so a dangling forward stays
// dangling (and stays auto-pause/clean-eligible).
func TestRefreshTailnetBoundApps(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	oldScan, oldStatus, oldID, oldPeer := scanListeners, serveStatus, identify, peerAPIPorts
	t.Cleanup(func() { scanListeners, serveStatus, identify, peerAPIPorts = oldScan, oldStatus, oldID, oldPeer })
	// tailscaled's own PeerAPI listener: unserved, ownerless, on the tailnet IP
	// -- indistinguishable from an app run by another user by the scan alone.
	peerAPIPorts = func() (map[int]bool, error) { return map[int]bool{18954: true}, nil }
	identify = func(int) procid.Identity { return acIdent }
	scanListeners = func() ([]portscan.Port, error) {
		return []portscan.Port{
			// visible owner: the app's own tailnet bind
			{Number: 18951, Process: "python3", Pid: 5, BindScope: portscan.ScopeTailnet, BindHost: "100.81.94.92", No127: true},
			// unattributed, port NOT served: an app run by root/another user
			{Number: 18952, UnattribTailnet: true, UnattribHost: "100.81.94.92"},
			// unattributed, port served: tailscaled's serve proxy -- dangling
			{Number: 18953, UnattribTailnet: true, UnattribHost: "100.81.94.92"},
			// unattributed, unserved, but tailscaled's own PeerAPI port
			{Number: 18954, UnattribTailnet: true, UnattribHost: "100.81.94.92"},
			// funnel ingress / an HTTPS serve key that is no 1:1 local service
			{Number: 443, UnattribTailnet: true, UnattribHost: "100.81.94.92"},
			{Number: 8443, UnattribTailnet: true, UnattribHost: "100.81.94.92"},
		}, nil
	}
	serveStatus = func() (tsserve.Detail, error) {
		return tsserve.Detail{Active: []int{18953}, OneToOne: map[int]bool{18953: true}, ListenPorts: map[int]bool{18953: true, 443: true, 8443: true}}, nil
	}

	msg := refresh().(refreshMsg)
	have := map[int]portscan.Port{}
	for _, p := range msg.ports {
		have[p.Number] = p
	}
	if _, ok := have[18951]; !ok {
		t.Errorf("owner-visible tailnet bind :18951 must be listed: %+v", msg.ports)
	}
	if p, ok := have[18952]; !ok || p.BindScope != portscan.ScopeTailnet || p.UnattribTailnet {
		t.Errorf("unserved unattributed :18952 must be an app tailnet bind: %+v", msg.ports)
	}
	if _, ok := have[443]; ok {
		t.Errorf("funnel ingress :443 is tailscaled's, not an app: %+v", msg.ports)
	}
	if _, ok := have[8443]; ok {
		t.Errorf("HTTPS serve key :8443 is tailscaled's, not an app: %+v", msg.ports)
	}
	if _, ok := have[18954]; ok {
		t.Errorf("tailscaled's PeerAPI port :18954 is not an app: %+v", msg.ports)
	}
	if _, ok := have[18953]; ok {
		t.Errorf("served :18953 has only tailscaled's row: must not be listening: %+v", msg.ports)
	}

	m := New(config.Config{})
	m.host = "lysander"
	m.showAllPorts = true
	m.cfAvailable = false
	m = mustUpdate(t, m, msg)
	m.showAllPorts = true
	m.rebuildItems()
	found := map[int]portItem{}
	for _, it := range m.list.Items() {
		found[it.(portItem).port.Number] = it.(portItem)
	}
	for _, n := range []int{18951, 18952} {
		it, ok := found[n]
		if !ok {
			t.Fatalf(":%d missing from the list", n)
		}
		rs := it.routes()
		if len(rs) != 1 || rs[0].kind != routeTailnet || rs[0].served || rs[0].stale || rs[0].url != addressFor("lysander", n) {
			t.Errorf(":%d routes = %+v, want one unserved tailnet route %s", n, rs, addressFor("lysander", n))
		}
		// t must not serve it: an info toast, nothing pending.
		m2 := m
		for i, x := range m2.list.Items() {
			if x.(portItem).port.Number == n {
				m2.list.Select(i)
			}
		}
		res, _ := m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
		got := res.(model)
		if got.pending != 0 || !strings.Contains(got.flash, "tailnet IP") {
			t.Errorf("t on tailnet-bound :%d: pending=%d flash=%q, want no serve and the tailnet-IP toast", n, got.pending, got.flash)
		}
	}
	// The dangling forward is still dangling and still clean/pause-eligible.
	if d := m.danglingPorts(); len(d) != 1 || d[0] != 18953 {
		t.Errorf("danglingPorts = %v, want [18953]", d)
	}
	if s := m.clearableStale(); len(s) != 1 || s[0] != 18953 {
		t.Errorf("clearableStale = %v, want [18953]", s)
	}

	// Serve status unreadable: unattributed rows are dropped (as before nvv6).
	serveStatus = func() (tsserve.Detail, error) { return tsserve.Detail{}, errors.New("tailscale: not found") }
	msg = refresh().(refreshMsg)
	for _, p := range msg.ports {
		if p.Number == 18952 || p.Number == 18953 {
			t.Errorf("unknown serve state must drop unattributed rows: %+v", msg.ports)
		}
	}
}

// The legend names the tailnet-bound state honestly (not just "bound wide").
func TestLegendCoversTailnetBoundApp(t *testing.T) {
	m := New(config.Config{})
	for _, emoji := range []bool{false, true} {
		m.markerEmoji = emoji
		if leg := m.markerLegend(); !strings.Contains(leg, "tailnet (app bound)") {
			t.Errorf("legend (emoji=%v) = %q, want it to say \"tailnet (app bound)\"", emoji, leg)
		}
	}
}
