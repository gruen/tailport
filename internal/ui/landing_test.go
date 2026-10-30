package ui

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/gruen/tailport/internal/cftunnel"
	"github.com/gruen/tailport/internal/config"
	"github.com/gruen/tailport/internal/portscan"
	"github.com/gruen/tailport/internal/tsserve"
)

// Tests for kata 4hcg's selection-by-identity redesign: the cursor is an index
// pair, but the user's selection is a (port, route kind) identity that is
// re-resolved after every rebuild, and a successful turn-ON registers a
// pending landing that resolves once the route it creates appears.

// applyFilter models an APPLIED "/" filter: SetFilterText runs bubbles'
// normally-async match synchronously, SetFilterState puts the list back in
// FilterApplied (SetFilterText leaves it in Filtering).
func applyFilter(m *model, text string) {
	m.list.SetFilterText(text)
	m.list.SetFilterState(list.FilterApplied)
	m.filtering = true
}

// filterLanding delivers the async filter re-match result a rebuild under an
// active filter requested -- the list.FilterMatchesMsg that repopulates
// VisibleItems. The message is minted the way bubbles does (SetItems under a
// filter returns the match cmd) on a copy, so m itself is untouched until
// Update consumes it.
func filterLanding(t *testing.T, m model) model {
	t.Helper()
	cp := m.list
	cmd := cp.SetItems(cp.Items())
	if cmd == nil {
		t.Fatal("filterLanding: no filter is active")
	}
	return mustUpdate(t, m, cmd())
}

// Bug 1: publish on a previously UNSERVED port. Publish auto-enables serve, so
// the confirming refresh inserts routeTailnet above routePublish; the cursor
// must stay on publish (index shifts 1 -> 2), not slide onto the tailnet row.
func TestLandingPublishOnUnservedPortSurvivesServeAppearing(t *testing.T) {
	m := landingModel(t)
	m.pendingPublish = pendingPublish{hostname: "app.example.com", label: "dev-box", port: 3000}
	m = mustUpdate(t, m, publishDoneMsg{port: 3000})
	m = confirmPublishPoll(t, m, 3000, "app.example.com")
	if p, k := selectedRoute(t, m); p != 3000 || k != routePublish {
		t.Fatalf("after the publish poll cursor on (:%d, %v), want routePublish", p, k)
	}
	// The confirming status refresh records the auto-enabled serve and inserts
	// routeTailnet above publish.
	m = confirmRefresh(t, m, map[int]bool{3000: true}, map[int]int{})
	if p, k := selectedRoute(t, m); p != 3000 || k != routePublish {
		t.Fatalf("after the serve refresh cursor on (:%d, %v), want routePublish", p, k)
	}
	if kind, url := copiedBy(t, m); kind != routePublish || url != "https://app.example.com" {
		t.Errorf("c copied (%v, %q), want (routePublish, https://app.example.com)", kind, url)
	}
}

// Bug 2: an applied "/" filter. A poll rebuild lands just before the done
// message and empties VisibleItems until the async match arrives; the
// don't-yank guard must not mistake that for the user having moved.
func TestLandingUnderAppliedFilterAfterPollRebuild(t *testing.T) {
	m := landingModel(t)
	applyFilter(&m, "3000")
	m = filterLanding(t, m) // the filter is settled, cursor on :3000
	if p, _ := selectedRoute(t, m); p != 3000 {
		t.Fatalf("setup: cursor on :%d, want :3000", p)
	}
	// A poll rebuild just before the done message: visible set now empty.
	m = confirmRefresh(t, m, map[int]bool{}, map[int]int{})
	if len(m.list.VisibleItems()) != 0 {
		t.Fatal("setup: the poll rebuild under a filter should clear the visible set")
	}
	m = mustUpdate(t, m, toggleDoneMsg{port: 3000, kind: routeTailnet, turnOn: true})
	if !m.pendingLand.set {
		t.Fatal("the done message should register a pending landing despite the empty visible set")
	}
	m = confirmRefresh(t, m, map[int]bool{3000: true}, map[int]int{})
	m = filterLanding(t, m)
	if p, k := selectedRoute(t, m); p != 3000 || k != routeTailnet {
		t.Fatalf("after the filter result cursor on (:%d, %v), want (:3000, routeTailnet)", p, k)
	}
	if m.pendingLand.set {
		t.Error("the landing should be consumed once resolved")
	}
	if kind, url := copiedBy(t, m); kind != routeTailnet || url != "http://dev-box:3000" {
		t.Errorf("c copied (%v, %q), want the tailnet URL", kind, url)
	}
}

// Bug 3: the previous status refresh failed, so tailnetUnavailable hides the
// tailnet route; a successful serve-on has no route to land on yet, and the
// confirming refresh (which clears the flag) must complete the landing.
func TestLandingAfterFailedStatusRefresh(t *testing.T) {
	m := landingModel(t)
	m = mustUpdate(t, m, refreshMsg{ports: append([]portscan.Port(nil), m.allPorts...), statusErr: errors.New("tailscale down")})
	if !m.tailnetUnavailable {
		t.Fatal("setup: the failed status refresh should set tailnetUnavailable")
	}
	m.selectPort(3000)
	m.routeIdx = 0
	m.syncSelKey()
	m = mustUpdate(t, m, toggleDoneMsg{port: 3000, kind: routeTailnet, turnOn: true})
	if p, k := selectedRoute(t, m); p != 3000 || k != routeLocalhostKind(t, m) {
		t.Fatalf("with no tailnet route yet the cursor should stay put; on (:%d, %v)", p, k)
	}
	m = confirmRefresh(t, m, map[int]bool{3000: true}, map[int]int{})
	if p, k := selectedRoute(t, m); p != 3000 || k != routeTailnet {
		t.Fatalf("after the confirming refresh cursor on (:%d, %v), want (:3000, routeTailnet)", p, k)
	}
}

// routeLocalhostKind is the kind of :3000's first route (the localhost row the
// fixtures start on).
func routeLocalhostKind(t *testing.T, m model) routeKind {
	t.Helper()
	pi, _, _ := m.currentService()
	return pi.routes()[0].kind
}

func TestPendingLandCancelledByNavigation(t *testing.T) {
	for _, key := range []string{"j", "k", "J", "K", "down", "up", "home", "end", "pgdown", "pgup"} {
		t.Run(key, func(t *testing.T) {
			m := landingModel(t)
			m.tailnetUnavailable = true // the tailnet route is not displayable yet
			m.rebuildItems()
			m = mustUpdate(t, m, toggleDoneMsg{port: 3000, kind: routeTailnet, turnOn: true})
			if !m.pendingLand.set {
				t.Fatal("setup: a landing should be pending")
			}
			m = mustUpdate(t, m, keyMsg(key))
			if m.pendingLand.set {
				t.Fatalf("%q should cancel the pending landing", key)
			}
			p0, k0 := selectedRoute(t, m)
			// The route appearing later must not drag the cursor.
			m = confirmRefresh(t, m, map[int]bool{3000: true}, map[int]int{})
			if p, k := selectedRoute(t, m); p != p0 || k != k0 {
				t.Fatalf("cursor moved (:%d,%v) -> (:%d,%v) after a cancelled landing", p0, k0, p, k)
			}
		})
	}
}

func TestPendingLandCancelledByDeadline(t *testing.T) {
	m := landingModel(t)
	m.tailnetUnavailable = true // the tailnet route is not displayable yet
	m.rebuildItems()
	m = mustUpdate(t, m, toggleDoneMsg{port: 3000, kind: routeTailnet, turnOn: true})
	m.pendingLand.deadline = time.Now().Add(-time.Second)
	m = confirmRefresh(t, m, map[int]bool{3000: true}, map[int]int{})
	if m.pendingLand.set {
		t.Error("an expired landing should be dropped on the next rebuild")
	}
	if _, k := selectedRoute(t, m); k == routeTailnet {
		t.Error("an expired landing must not move the cursor")
	}
}

// The selection follows its route when a rebuild inserts rows ABOVE it.
func TestSelectionIdentitySurvivesRouteInsertedAbove(t *testing.T) {
	m := landingModel(t)
	m = confirmPublishPoll(t, m, 3000, "app.example.com")
	m = mustUpdate(t, m, keyMsg("j")) // localhost -> publish
	if _, k := selectedRoute(t, m); k != routePublish {
		t.Fatalf("setup: cursor on %v, want routePublish", k)
	}
	m = confirmRefresh(t, m, map[int]bool{3000: true}, map[int]int{})
	if p, k := selectedRoute(t, m); p != 3000 || k != routePublish {
		t.Fatalf("cursor on (:%d, %v), want it to stay on routePublish", p, k)
	}
}

// The service, too, is held by identity: a port appearing above the selected
// one must not slide the cursor onto a different service.
func TestSelectionIdentitySurvivesServiceInsertedAbove(t *testing.T) {
	m := landingModel(t)
	m = mustUpdate(t, m, refreshMsg{
		ports: []portscan.Port{
			{Number: 1000, Process: "new", BindScope: portscan.ScopeLoopback},
			{Number: 2000, Process: "other", BindScope: portscan.ScopeLoopback},
			{Number: 3000, Process: "app", BindScope: portscan.ScopeLoopback},
		},
		active: map[int]bool{}, funnel: map[int]int{},
	})
	if p, _ := selectedRoute(t, m); p != 3000 {
		t.Fatalf("cursor on :%d, want it to stay on :3000", p)
	}
}

func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "home":
		return tea.KeyMsg{Type: tea.KeyHome}
	case "end":
		return tea.KeyMsg{Type: tea.KeyEnd}
	case "pgup":
		return tea.KeyMsg{Type: tea.KeyPgUp}
	case "pgdown":
		return tea.KeyMsg{Type: tea.KeyPgDown}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// TestRandomizedUpdateViewInvariants drives Update and View with seeded
// pseudo-random message sequences (navigation, "/" filtering, sort and view
// toggles, resizes, refresh/poll messages that add and remove ports and
// exposures, and done messages of every exposure kind, success and failure)
// and asserts it never panics, that routeIdx stays inside the selected
// service's routes, and that View renders. Added with kata 4hcg after an
// unexplained tailport exit that three replays could not reproduce: a stale
// routeIdx under a filter was the leading suspect.
func TestRandomizedUpdateViewInvariants(t *testing.T) {
	for _, seed := range []int64{1, 2, 3, 4} {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			runRandomized(t, seed, 1500)
		})
	}
}

func runRandomized(t *testing.T, seed int64, steps int) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rng := rand.New(rand.NewSource(seed))
	pool := []int{22, 1000, 2000, 3000, 3001, 5000, 8080, 9090}

	m := New(config.Config{})
	m.host = "dev-box"
	m.fqdn = "dev-box.tailnet.ts.net"
	m.cfAvailable = true
	var trace []string
	step := func(desc string, msg tea.Msg) {
		trace = append(trace, desc)
		if len(trace) > 40 {
			trace = trace[1:]
		}
		res, _ := m.Update(msg)
		m = res.(model)
	}
	check := func() {
		if vis := m.list.VisibleItems(); len(vis) > 0 {
			pi, _, ok := m.currentService()
			if !ok {
				t.Fatalf("visible items but no current service\n%s", strings.Join(trace, "\n"))
			}
			if n := len(pi.routes()); m.routeIdx < 0 || m.routeIdx >= n {
				t.Fatalf("routeIdx %d out of range [0,%d) for :%d\n%s", m.routeIdx, n, pi.port.Number, strings.Join(trace, "\n"))
			}
			if m.list.FilterState() != list.Filtering && (!m.selKey.set || m.selKey.port != pi.port.Number) {
				t.Fatalf("selKey %+v disagrees with the cursor on :%d\n%s", m.selKey, pi.port.Number, strings.Join(trace, "\n"))
			}
		}
		if v := m.View(); strings.TrimSpace(v) == "" {
			t.Fatalf("View rendered nothing\n%s", strings.Join(trace, "\n"))
		}
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PANIC: %v\nlast messages:\n%s", r, strings.Join(trace, "\n"))
		}
	}()

	step("size", tea.WindowSizeMsg{Width: 100, Height: 30})
	keys := []string{"up", "down", "j", "k", "J", "K", "left", "right", "home", "end", "pgup", "pgdown", "g", "G", "s", "a", "c", "y"}
	for i := 0; i < steps; i++ {
		switch r := rng.Intn(100); {
		case r < 35:
			k := keys[rng.Intn(len(keys))]
			step("key "+k, keyMsg(k))
		case r < 40:
			step("key /", keyMsg("/"))
		case r < 46:
			txt := string(rune('0' + rng.Intn(10)))
			step("filter text "+txt, keyMsg(txt))
		case r < 49:
			step("key enter", keyMsg("enter"))
		case r < 52:
			step("key esc", keyMsg("esc"))
		case r < 60:
			// The async filter result a rebuild/keystroke asked for.
			if m.list.FilterState() != list.Unfiltered {
				cp := m.list
				if cmd := cp.SetItems(cp.Items()); cmd != nil {
					step("filter result", cmd())
				}
			}
		case r < 65:
			step("resize", tea.WindowSizeMsg{Width: 20 + rng.Intn(160), Height: 6 + rng.Intn(50)})
		case r < 78:
			var ports []portscan.Port
			active, funnel := map[int]bool{}, map[int]int{}
			for _, n := range pool {
				if rng.Intn(3) == 0 {
					continue
				}
				scope := portscan.ScopeLoopback
				if rng.Intn(3) == 0 {
					scope = portscan.ScopeWildcard
				}
				ports = append(ports, portscan.Port{Number: n, Process: fmt.Sprintf("p%d", rng.Intn(5)), BindScope: scope})
				if rng.Intn(3) == 0 {
					active[n] = true
				}
				if rng.Intn(5) == 0 {
					funnel[n] = 443
				}
			}
			msg := refreshMsg{ports: ports, active: active, funnel: funnel, auto: rng.Intn(2) == 0}
			if rng.Intn(6) == 0 {
				msg.statusErr = errors.New("status failed")
			}
			step("refresh", msg)
		case r < 84:
			pub := map[int]publishInfo{}
			for _, n := range pool {
				if rng.Intn(4) == 0 {
					pub[n] = publishInfo{hostname: fmt.Sprintf("h%d.example.com", n)}
				}
			}
			step("publish poll", publishPollMsg{published: pub, gen: m.publishPollApplied + 1})
		case r < 88:
			tun, foreign := map[int]tunnelInfo{}, map[int]bool{}
			for _, n := range pool {
				switch rng.Intn(6) {
				case 0:
					tun[n] = tunnelInfo{mode: cftunnel.ModeQuick, pid: 100 + n, ready: rng.Intn(2) == 0, hostname: "q.trycloudflare.com"}
				case 1:
					foreign[n] = true
				case 2:
					// owned AND foreign on one port: two routeTunnel rows
					tun[n] = tunnelInfo{mode: cftunnel.ModeQuick, pid: 100 + n, ready: true, hostname: "q.trycloudflare.com"}
					foreign[n] = true
				}
			}
			step("tunnel poll", tunnelPollMsg{tunnels: tun, foreign: foreign, gen: m.tunnelPollApplied + 1})
		default:
			port := pool[rng.Intn(len(pool))]
			var err error
			if rng.Intn(4) == 0 {
				err = errors.New("boom")
			}
			switch rng.Intn(5) {
			case 0:
				step(fmt.Sprintf("serve done :%d err=%v", port, err), toggleDoneMsg{port: port, kind: routeTailnet, turnOn: rng.Intn(4) != 0, err: err})
			case 1:
				step(fmt.Sprintf("funnel done :%d err=%v", port, err), toggleDoneMsg{port: port, kind: routeFunnel, turnOn: rng.Intn(4) != 0, err: err})
			case 2:
				m.pendingPublish = pendingPublish{hostname: fmt.Sprintf("h%d.example.com", port), port: port}
				step(fmt.Sprintf("publish done :%d err=%v", port, err), publishDoneMsg{port: port, err: err, unpublish: rng.Intn(4) == 0})
			case 3:
				step(fmt.Sprintf("tunnel done :%d err=%v", port, err), tunnelDoneMsg{port: port, err: err, running: &cftunnel.Running{PID: 500 + port, Port: port, Mode: cftunnel.ModeQuick}})
			default:
				step(fmt.Sprintf("tunnel teardown :%d", port), tunnelDoneMsg{port: port, torndown: true})
			}
		}
		check()
	}
}

// Pressing "/" makes bubbles/list jump the service cursor to the first row
// (GoToStart). The route cursor used to keep the OLD service's index, which
// could be past the end of the new service's routes -- the minimal sequence the
// randomized test found: select a later service's second route, press "/".
func TestFilterStartKeepsRouteIdxInRange(t *testing.T) {
	m := landingModel(t)
	m = confirmPublishPoll(t, m, 3000, "app.example.com")
	m = mustUpdate(t, m, keyMsg("j")) // :3000 localhost -> publish (routeIdx 1)
	if m.routeIdx != 1 {
		t.Fatalf("setup: routeIdx = %d, want 1", m.routeIdx)
	}
	m = mustUpdate(t, m, keyMsg("/"))
	if p, _ := selectedRoute(t, m); p != 2000 {
		t.Fatalf("filtering starts on the first service; on :%d", p)
	}
	if m.routeIdx != 0 {
		t.Errorf("routeIdx = %d, want 0 on the new service", m.routeIdx)
	}
}

// A successful publish is authoritative: with NO edge poll delivered (or a
// failed one), c copies the public URL at once rather than the previously
// selected localhost URL.
func TestPublishSuccessLandsWithoutPoll(t *testing.T) {
	m := landingModel(t)
	m.pendingPublish = pendingPublish{hostname: "app.example.com", label: "dev-box", port: 3000, withAuth: true}
	m = mustUpdate(t, m, publishDoneMsg{port: 3000})
	if p, k := selectedRoute(t, m); p != 3000 || k != routePublish {
		t.Fatalf("cursor on (:%d, %v), want (:3000, routePublish)", p, k)
	}
	if kind, url := copiedBy(t, m); kind != routePublish || url != "https://app.example.com" {
		t.Errorf("c copied (%v, %q), want the public URL", kind, url)
	}
	// A failed poll leaves it be.
	m = mustUpdate(t, m, publishPollMsg{err: errors.New("edge down"), gen: m.publishPollApplied + 1})
	if _, k := selectedRoute(t, m); k != routePublish {
		t.Errorf("a failed poll moved the cursor to %v", k)
	}
}

// routeTunnel is the one kind that can appear twice on a port (owned plus
// foreign cloudflared). The selection must stay on the row the user chose.
func TestSelectionIdentityDistinguishesOwnedAndForeignTunnel(t *testing.T) {
	m := landingModel(t)
	tun := map[int]tunnelInfo{3000: {mode: cftunnel.ModeQuick, pid: 111, hostname: "q.trycloudflare.com", ready: true}}
	m = mustUpdate(t, m, tunnelPollMsg{tunnels: tun, foreign: map[int]bool{3000: true}, gen: m.tunnelPollApplied + 1})
	pi, _, _ := m.currentService()
	var n, foreignIdx int
	for i, r := range pi.routes() {
		if r.kind == routeTunnel {
			n++
			if r.foreign {
				foreignIdx = i
			}
		}
	}
	if n != 2 {
		t.Fatalf("setup: want an owned and a foreign tunnel row, got %d tunnel rows in %+v", n, pi.routes())
	}
	m.routeIdx = foreignIdx
	m.syncSelKey()
	m = mustUpdate(t, m, tunnelPollMsg{tunnels: tun, foreign: map[int]bool{3000: true}, gen: m.tunnelPollApplied + 1})
	pi, _, _ = m.currentService()
	if !pi.routes()[m.routeIdx].foreign {
		t.Fatalf("selection moved off the foreign tunnel row to %+v", pi.routes()[m.routeIdx])
	}
	// c on the foreign row has no URL (surfaced as drift), so it must NOT
	// copy the owned row's URL.
	if kind, url := copiedBy(t, m); url == "https://q.trycloudflare.com" {
		t.Errorf("c copied the owned tunnel URL (%v, %q) from the foreign row", kind, url)
	}
}

// A snapshot ISSUED before a successful mutation must never overwrite the
// state that mutation just wrote (kata 4hcg): the stale one is dropped, the
// cursor stays on the new route throughout, and a fresh snapshot applies.
func TestStaleSnapshotDoesNotEraseLanding(t *testing.T) {
	cases := []struct {
		name string
		kind routeKind
		url  string
		done tea.Msg
	}{
		{
			name: "serve", kind: routeTailnet, url: "http://dev-box:3000",
			done: toggleDoneMsg{port: 3000, kind: routeTailnet, turnOn: true},
		},
		{
			name: "funnel", kind: routeFunnel, url: "https://dev-box.tailnet.ts.net",
			done: toggleDoneMsg{port: 3000, kind: routeFunnel, turnOn: true, publicPort: 443},
		},
		{
			name: "publish", kind: routePublish, url: "https://app.example.com",
			done: publishDoneMsg{port: 3000},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := landingModel(t)
			m.pendingPublish = pendingPublish{hostname: "app.example.com", label: "dev-box", port: 3000}
			// Snapshots issued BEFORE the mutation.
			m.refreshGen++
			staleRefresh := refreshMsg{ports: append([]portscan.Port(nil), m.allPorts...), active: map[int]bool{}, funnel: map[int]int{}, gen: m.refreshGen}
			m.publishPollGen++
			stalePoll := publishPollMsg{published: map[int]publishInfo{}, gen: m.publishPollGen}

			m = mustUpdate(t, m, tc.done)
			wantURL := tc.url
			if tc.kind == routeFunnel {
				wantURL = tsserve.PublicURL("dev-box.tailnet.ts.net", 443)
			}
			check := func(stage string) {
				t.Helper()
				if p, k := selectedRoute(t, m); p != 3000 || k != tc.kind {
					t.Fatalf("%s: cursor on (:%d, %v), want (:3000, %v)", stage, p, k, tc.kind)
				}
				if kind, url := copiedBy(t, m); kind != tc.kind || url != wantURL {
					t.Fatalf("%s: c copied (%v, %q), want (%v, %q)", stage, kind, url, tc.kind, wantURL)
				}
			}
			check("after the done message")

			m = mustUpdate(t, m, staleRefresh)
			m = mustUpdate(t, m, stalePoll)
			check("after the stale snapshots")

			// Fresh snapshots, issued after the mutation, include the route.
			fresh := refreshMsg{ports: append([]portscan.Port(nil), m.allPorts...), active: map[int]bool{3000: true}, funnel: map[int]int{}, gen: m.refreshGen}
			pub := map[int]publishInfo{}
			if tc.kind == routeFunnel {
				fresh.funnel = map[int]int{3000: 443}
			}
			if tc.kind == routePublish {
				pub[3000] = publishInfo{hostname: "app.example.com"}
			}
			m = mustUpdate(t, m, fresh)
			m = mustUpdate(t, m, publishPollMsg{published: pub, gen: m.publishPollApplied + 1})
			check("after the fresh snapshots")
		})
	}
}

// Dropping a stale refresh must not break the refresh cycle: the next refresh
// still applies, and the periodic tick is not part of the dropped message.
func TestDroppedStaleRefreshDoesNotStallCycle(t *testing.T) {
	m := landingModel(t)
	m.refreshGen++
	stale := refreshMsg{ports: []portscan.Port{{Number: 4000, Process: "gone-by-now", BindScope: portscan.ScopeLoopback}}, active: map[int]bool{}, funnel: map[int]int{}, gen: m.refreshGen}
	m = mustUpdate(t, m, toggleDoneMsg{port: 3000, kind: routeTailnet, turnOn: true})
	before := len(m.allPorts)
	m = mustUpdate(t, m, stale)
	if len(m.allPorts) != before {
		t.Fatal("the stale refresh should have been dropped")
	}
	// The periodic tick still issues a refresh (stamped after the floor)...
	m.pending = 0
	res, cmd := m.Update(refreshTickMsg{})
	m = res.(model)
	if cmd == nil || m.refreshGen <= m.refreshFloor {
		t.Fatalf("the tick should issue a fresh refresh above the floor; gen=%d floor=%d", m.refreshGen, m.refreshFloor)
	}
	// ...and its result applies.
	next := refreshMsg{ports: []portscan.Port{{Number: 3000, Process: "app", BindScope: portscan.ScopeLoopback}, {Number: 4000, Process: "new", BindScope: portscan.ScopeLoopback}}, active: map[int]bool{3000: true}, funnel: map[int]int{}, gen: m.refreshGen}
	m = mustUpdate(t, m, next)
	if len(m.allPorts) != 2 {
		t.Fatalf("the fresh refresh should apply; allPorts=%v", m.allPorts)
	}
	// An unstamped (startup) refresh always applies.
	m = mustUpdate(t, m, refreshMsg{ports: next.ports[:1], active: map[int]bool{}, funnel: map[int]int{}})
	if len(m.allPorts) != 1 {
		t.Fatalf("an unstamped refresh should apply; allPorts=%v", m.allPorts)
	}
}
