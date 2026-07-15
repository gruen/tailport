package ui

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/gruen/tailport/internal/config"
	"github.com/gruen/tailport/internal/portscan"
	"github.com/gruen/tailport/internal/tsserve"
)

// portNumbers returns the port number of every item currently in the list,
// in order -- the visible result of rebuildItems for a given view.
func portNumbers(m model) []int {
	items := m.list.Items()
	out := make([]int, len(items))
	for i, it := range items {
		out[i] = it.(portItem).port.Number
	}
	return out
}

// TestDanglingPorts covers the core exposed-but-not-listening detection that
// drives both the ▲ warning render and the "c" clean affordance. A port is
// dangling iff it's active (a serve mapping exists) AND nothing is bound
// locally. The result must be sorted and needs no live tailscale.
func TestDanglingPorts(t *testing.T) {
	tests := []struct {
		name   string
		ports  []int        // locally listening ports
		active map[int]bool // exposed via serve
		want   []int
	}{
		{
			name:   "none active",
			ports:  []int{8080, 3000},
			active: map[int]bool{},
			want:   nil,
		},
		{
			name:   "healthy forward is not dangling",
			ports:  []int{8080},
			active: map[int]bool{8080: true},
			want:   nil,
		},
		{
			name:   "exposed with no listener is dangling",
			ports:  []int{3000},
			active: map[int]bool{8080: true},
			want:   []int{8080},
		},
		{
			name:   "active:false is never dangling",
			ports:  nil,
			active: map[int]bool{8080: false},
			want:   nil,
		},
		{
			name:   "mixed, result sorted",
			ports:  []int{3000, 9000},
			active: map[int]bool{9000: true, 8080: true, 3000: true, 5000: true},
			want:   []int{5000, 8080},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allPorts := make([]portscan.Port, len(tt.ports))
			for i, n := range tt.ports {
				allPorts[i] = portscan.Port{Number: n}
			}
			m := model{allPorts: allPorts, active: tt.active}

			got := m.danglingPorts()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("danglingPorts() = %v, want %v", got, tt.want)
			}
			if wantHas := len(tt.want) > 0; m.hasDangling() != wantHas {
				t.Errorf("hasDangling() = %v, want %v", m.hasDangling(), wantHas)
			}
		})
	}
}

// TestSelectIndexForPort covers the cursor-anchoring helper behind the "a"
// view toggle (vk30): the cursor tracks the port number, not the row index.
// numbers is always sorted ascending (rebuildItems sorts before setItems).
func TestSelectIndexForPort(t *testing.T) {
	tests := []struct {
		name    string
		numbers []int
		target  int
		want    int
	}{
		{"exact match first", []int{22, 3000, 8080}, 22, 0},
		{"exact match middle", []int{22, 3000, 8080}, 3000, 1},
		{"exact match last", []int{22, 3000, 8080}, 8080, 2},
		// vk30's :9000 example: not a favorite, so favorites view lacks it;
		// cursor lands on the nearest next-lowest favorite (:8080).
		{"missing lands on next-lowest", []int{3000, 8080}, 9000, 1},
		{"missing between two", []int{3000, 8080}, 5000, 0},
		{"missing above all", []int{3000, 8080}, 65535, 1},
		{"missing below all", []int{3000, 8080}, 80, 0},
		{"single item", []int{3000}, 9000, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := selectIndexForPort(tt.numbers, tt.target); got != tt.want {
				t.Errorf("selectIndexForPort(%v, %d) = %d, want %d", tt.numbers, tt.target, got, tt.want)
			}
		})
	}
}

// TestRebuildItemsViews covers the two "a" views (vk30): Favorites shows only
// meta.Favorite ports -- including a synthetic entry for a favorite whose
// process isn't running -- while All ports shows every currently-listening
// port UNIONed with all favorites (qqkx), so a favorite stays visible in both
// views even when down. A non-favorite non-listening port appears in neither.
func TestRebuildItemsViews(t *testing.T) {
	cfg := config.Config{Ports: map[int]config.PortMeta{
		3000: {Favorite: true},
		8080: {Favorite: true, Label: "web"},
		4000: {Favorite: true}, // favorite but not listening -> synthetic entry
		5000: {Label: "api"},   // labeled, not favorited, not listening -> nowhere
	}}
	m := New(cfg)
	m.allPorts = []portscan.Port{{Number: 3000, Process: "node"}, {Number: 9000, Process: "x"}, {Number: 8080, Process: "srv"}}
	m.active = map[int]bool{}

	m.showAllPorts = false
	m.rebuildItems()
	if got, want := portNumbers(m), []int{3000, 4000, 8080}; !reflect.DeepEqual(got, want) {
		t.Errorf("favorites view = %v, want %v", got, want)
	}

	m.showAllPorts = true
	m.rebuildItems()
	// Listening {3000,8080,9000} UNION favorites {3000,4000,8080} = the below;
	// :4000 (down favorite) is now included, :5000 (non-fav, down) is not, and
	// :3000/:8080 (favorite AND listening) appear once each (deduped).
	if got, want := portNumbers(m), []int{3000, 4000, 8080, 9000}; !reflect.DeepEqual(got, want) {
		t.Errorf("all ports view = %v, want %v", got, want)
	}
	// The down favorite renders as a synthetic not-listening entry.
	for _, it := range m.list.Items() {
		pi := it.(portItem)
		if pi.port.Number == 4000 && pi.listening {
			t.Error(":4000 (down favorite) should be a non-listening synthetic entry in All ports")
		}
	}
}

// TestRequestToggle covers the toggle gate (weyy): the lock guard blocks
// turning a locked port on, port :22 defers to a y/n confirm in BOTH
// directions (off is what drops SSH), and every other port toggles now.
func TestRequestToggle(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // isolate any config.Save

	// Locked :22 turn-on: lock guard fires -- an error toast, no confirm, no
	// toggle. (The returned cmd is the toast's expiry tick, not a toggle.)
	m := New(config.Config{Ports: map[int]config.PortMeta{22: {Locked: true}}})
	m.requestToggle(22, true)
	if m.flash == "" || m.flashLevel != flashError {
		t.Errorf("locked :22 turn-on should raise an error toast; flash=%q level=%v", m.flash, m.flashLevel)
	}
	if m.pending != 0 {
		t.Error("locked :22 turn-on should not begin a toggle")
	}
	if m.mode != entryNone {
		t.Errorf("locked :22 turn-on mode = %v, want entryNone", m.mode)
	}

	// Unlocked :22 turn-on: opens the confirm, defers the toggle.
	m = New(config.Config{Ports: map[int]config.PortMeta{}})
	if cmd := m.requestToggle(22, true); cmd != nil {
		t.Error(":22 turn-on should defer (nil cmd) pending confirm")
	}
	if m.mode != entryConfirm22 || m.confirmPort != 22 || !m.confirmTurnOn {
		t.Errorf(":22 turn-on state = mode:%v port:%d on:%v", m.mode, m.confirmPort, m.confirmTurnOn)
	}

	// :22 turn-off confirms too, even when locked (lock only guards turn-on).
	m = New(config.Config{Ports: map[int]config.PortMeta{22: {Locked: true}}})
	if cmd := m.requestToggle(22, false); cmd != nil {
		t.Error(":22 turn-off should defer pending confirm")
	}
	if m.mode != entryConfirm22 || m.confirmTurnOn {
		t.Errorf(":22 turn-off state = mode:%v on:%v", m.mode, m.confirmTurnOn)
	}

	// A normal port toggles immediately: real cmd, pending set, no confirm.
	m = New(config.Config{Ports: map[int]config.PortMeta{8080: {}}})
	if cmd := m.requestToggle(8080, true); cmd == nil {
		t.Error("normal port should return a toggle cmd")
	}
	if m.mode != entryNone {
		t.Errorf("normal port mode = %v, want entryNone", m.mode)
	}
	if m.pending != 8080 {
		t.Errorf("normal port pending = %d, want 8080", m.pending)
	}
}

// TestUpdateToggleKeys covers g87s at the Update layer: space toggles the
// selected port, enter no longer does.
func TestUpdateToggleKeys(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	newModel := func() model {
		m := New(config.Config{Ports: map[int]config.PortMeta{8080: {Favorite: true}}})
		m.allPorts = []portscan.Port{{Number: 8080, Process: "srv"}}
		m.active = map[int]bool{}
		m.showAllPorts = true
		m.rebuildItems()
		return m
	}

	m := newModel()
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	if cmd == nil {
		t.Error("space should return a toggle cmd")
	}
	if got := res.(model); got.pending != 8080 {
		t.Errorf("after space, pending = %d, want 8080", got.pending)
	}

	m = newModel()
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := res.(model); got.pending != 0 {
		t.Errorf("enter should not toggle; pending = %d, want 0", got.pending)
	}
}

// TestSpaceGuardForReachablePorts covers 79xb pt3's footgun guard: `tailscale
// serve` always proxies tailnet -> 127.0.0.1:PORT, so turning it ON only ever
// makes sense for a loopback-bound port (state A). Selecting an already
// tailnet-reachable port (B, wildcard/tailnet-IP bind) or a LAN-only port (B',
// a specific non-tailnet IP) and pressing space must NO-OP with an
// informational toast rather than begin a serve. A (loopback, serve-ON) and C
// (served, serve-OFF) must be unaffected.
func TestSpaceGuardForReachablePorts(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	newModel := func(port int, scope portscan.BindScope, active bool) model {
		m := New(config.Config{Ports: map[int]config.PortMeta{port: {Favorite: true}}})
		m.allPorts = []portscan.Port{{Number: port, Process: "srv", BindScope: scope}}
		if active {
			m.active = map[int]bool{port: true}
		} else {
			m.active = map[int]bool{}
		}
		m.showAllPorts = true
		m.rebuildItems()
		return m
	}

	// B (wildcard bind, unserved, non-:22): space no-ops with the general
	// "app bound wide (0.0.0.0)" info toast (83wv pt2 -- reworded from the
	// old "nothing to serve" line to be honest about WHY and actionable
	// about how to make it toggleable), no toggle begun.
	const wantGeneralWildcard = "on tailnet — app bound wide (0.0.0.0); rebind to localhost (or 127.0.0.1) to make toggleable"
	m := newModel(8080, portscan.ScopeWildcard, false)
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	got := res.(model)
	if got.pending != 0 {
		t.Errorf("space on a B (tailnet) port should not begin a toggle; pending = %d", got.pending)
	}
	if got.flashLevel != flashInfo || got.flash != wantGeneralWildcard {
		t.Errorf("space on a B port flash = %q (level=%v), want %q at flashInfo", got.flash, got.flashLevel, wantGeneralWildcard)
	}
	if cmd == nil {
		t.Error("space on a B port should still return the toast's flash cmd")
	}

	// B (wildcard bind, unserved, :22 -- the operator's own live SSH port):
	// space no-ops with the DEDICATED SSH variant, not the general
	// "rebind to localhost" line, which would be nonsensical (and
	// self-locking) advice for sshd (83wv pt2).
	const wantSSHVariant = "on tailnet as SSH — this is how you're connected; nothing for tailport to serve"
	m = newModel(22, portscan.ScopeWildcard, false)
	res, cmd = m.Update(tea.KeyMsg{Type: tea.KeySpace})
	got = res.(model)
	if got.pending != 0 {
		t.Errorf("space on a wildcard-bound :22 port should not begin a toggle; pending = %d", got.pending)
	}
	if got.flashLevel != flashInfo || got.flash != wantSSHVariant {
		t.Errorf("space on a wildcard-bound :22 port flash = %q (level=%v), want %q at flashInfo", got.flash, got.flashLevel, wantSSHVariant)
	}
	if cmd == nil {
		t.Error("space on a wildcard-bound :22 port should still return the toast's flash cmd")
	}

	// B' (specific LAN IP, unserved): space no-ops with the "can't reach this
	// bind" info toast, no toggle begun -- unchanged by 83wv pt2.
	const wantLAN = "on your LAN only; serve can't reach this bind"
	m = newModel(3000, portscan.ScopeLAN, false)
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace})
	got = res.(model)
	if got.pending != 0 {
		t.Errorf("space on a B' (LAN) port should not begin a toggle; pending = %d", got.pending)
	}
	if got.flashLevel != flashInfo || got.flash != wantLAN {
		t.Errorf("space on a B' port flash = %q (level=%v), want %q at flashInfo", got.flash, got.flashLevel, wantLAN)
	}

	// A (loopback bind, unserved): space still initiates the toggle -- the
	// only state serve-ON is meaningful for.
	m = newModel(9000, portscan.ScopeLoopback, false)
	res, cmd = m.Update(tea.KeyMsg{Type: tea.KeySpace})
	got = res.(model)
	if cmd == nil {
		t.Error("space on an A (loopback) port should return a toggle cmd")
	}
	if got.pending != 9000 {
		t.Errorf("space on an A port should begin a toggle; pending = %d, want 9000", got.pending)
	}

	// C (already served): space still toggles OFF, regardless of bind scope.
	m = newModel(8080, portscan.ScopeWildcard, true)
	res, cmd = m.Update(tea.KeyMsg{Type: tea.KeySpace})
	got = res.(model)
	if cmd == nil {
		t.Error("space on a served (C) port should return a toggle-off cmd")
	}
	if got.pending != 8080 {
		t.Errorf("space on a served (C) port should begin a toggle; pending = %d, want 8080", got.pending)
	}
}

// settle runs a command and feeds the resulting message(s) back into the
// model, so bubbles/list's ASYNC filtering (a FilterMatchesMsg produced by a
// tea.Cmd) is actually applied -- without this, VisibleItems never reflects a
// typed query in tests. tea.Batch is unwrapped recursively; each cmd runs with
// a short deadline so blocking ticks (textinput's cursor blink) are skipped
// rather than stalling the suite.
func settle(m model, cmd tea.Cmd) model {
	for _, msg := range collectMsgs(cmd) {
		res, _ := m.Update(msg)
		m = res.(model)
	}
	return m
}

func collectMsgs(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	select {
	case msg := <-ch:
		if batch, ok := msg.(tea.BatchMsg); ok {
			var out []tea.Msg
			for _, c := range batch {
				out = append(out, collectMsgs(c)...)
			}
			return out
		}
		if msg == nil {
			return nil
		}
		return []tea.Msg{msg}
	case <-time.After(50 * time.Millisecond):
		return nil // a blocking cmd (e.g. cursor blink tick) -- skip it
	}
}

// typeRunes feeds each rune of s to Update as a key press, threading the model
// and settling the async filter after each keystroke.
func typeRunes(m model, s string) model {
	for _, r := range s {
		res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = settle(res.(model), cmd)
	}
	return m
}

// TestFilterScope covers 4ye6's core: pressing "/" from the Favorites view
// widens the search to ALL listening ports, dims the non-favorite matches,
// keeps fuzzy matching, and esc restores the Favorites view.
func TestFilterScope(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := New(config.Config{Ports: map[int]config.PortMeta{8808: {Favorite: true}}})
	m.allPorts = []portscan.Port{{Number: 8808, Process: "web"}, {Number: 3000, Process: "node"}, {Number: 9000, Process: "x"}}
	m.active = map[int]bool{}
	m.showAllPorts = false
	m.rebuildItems()

	// Favorites view shows only the favorite.
	if got := portNumbers(m); !reflect.DeepEqual(got, []int{8808}) {
		t.Fatalf("favorites view = %v, want [8808]", got)
	}

	// "/" widens scope to every listening port and enters filtering.
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m = settle(res.(model), cmd)
	if m.list.FilterState() != list.Filtering {
		t.Fatalf("after '/', FilterState = %v, want Filtering", m.list.FilterState())
	}
	if got := portNumbers(m); !reflect.DeepEqual(got, []int{3000, 8808, 9000}) {
		t.Errorf("filtering scope = %v, want all listening [3000 8808 9000]", got)
	}

	// Non-favorite matches are dimmed; the favorite is not.
	for _, it := range m.list.Items() {
		pi := it.(portItem)
		if wantDim := pi.port.Number != 8808; pi.dimmed != wantDim {
			t.Errorf(":%d dimmed = %v, want %v", pi.port.Number, pi.dimmed, wantDim)
		}
	}

	// Fuzzy match kept: "80" narrows to :8808 (the issue's example), not :3000.
	m = typeRunes(m, "80")
	vis := m.list.VisibleItems()
	if len(vis) != 1 || vis[0].(portItem).port.Number != 8808 {
		var nums []int
		for _, it := range vis {
			nums = append(nums, it.(portItem).port.Number)
		}
		t.Errorf("filter '80' visible = %v, want [8808]", nums)
	}

	// esc clears the filter and restores the Favorites view.
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = res.(model)
	if m.list.FilterState() != list.Unfiltered {
		t.Errorf("after esc, FilterState = %v, want Unfiltered", m.list.FilterState())
	}
	if got := portNumbers(m); !reflect.DeepEqual(got, []int{8808}) {
		t.Errorf("after esc, view = %v, want [8808]", got)
	}
}

// TestFilterNoMatch covers 4ye6's no-match state: a query that matches nothing
// yields the dedicated "no ports match" message (naming the query), not the
// fresh-install empty-state explainer or bubbles/list's bare "No items.".
func TestFilterNoMatch(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := New(config.Config{})
	m.allPorts = []portscan.Port{{Number: 8080, Process: "web"}}
	m.active = map[int]bool{}
	m.showAllPorts = true
	m.rebuildItems()

	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m = typeRunes(settle(res.(model), cmd), "zzzz")

	if n := len(m.list.VisibleItems()); n != 0 {
		t.Fatalf("expected no matches for 'zzzz', got %d visible", n)
	}
	if msg := m.noMatchMessage(); !strings.Contains(msg, "No ports match") || !strings.Contains(msg, "zzzz") {
		t.Errorf("noMatchMessage = %q, want it to name the query", msg)
	}
	// View() must render the no-match message, not the empty-state explainer.
	if v := m.View(); !strings.Contains(v, "No ports match") || strings.Contains(v, "haven't favorited") {
		t.Errorf("View during no-match should show the no-match message, not the explainer")
	}
}

// TestFilterDiscoverable covers 4ye6's discoverability: "/" is in the legend
// and in the help overlay's key list.
func TestFilterDiscoverable(t *testing.T) {
	m := New(config.Config{})
	m.help.Width = 200 // wide enough that wrapBindings keeps every binding
	if legend := m.renderLegend(); !strings.Contains(legend, "filter") {
		t.Errorf("legend should advertise the filter binding; got %q", legend)
	}
	if help := m.helpContent(); !strings.Contains(help, "Filter by port number") {
		t.Errorf("help overlay should describe '/' filter; got %q", help)
	}
}

// TestHelpViewUsesSharedKeyLegend covers the single-source-of-truth invariant
// (kata x4cg, evolved by p39s): the in-TUI "?" overlay's key sections are
// exactly RenderKeyLegendGroups(KeyLegendGroups(m.markerEmoji)) -- not a
// hand-copied duplicate -- so it and `tailport quickstart` (cmd/tailport,
// which calls the same two functions) can never drift apart. Checked in both
// EXPOSURE-marker modes (m.markerEmoji, not the egg's m.emoji -- qwcw split
// the two), since the space/p/C rows quote the mode-specific exposure glyph.
// Asserted on helpContent (the full overlay text) with width unset, where the
// legend is a single vertical column so the shared block appears verbatim;
// helpView windows that content to the terminal height (v10j) and the
// wide-terminal layout re-flows the same shared rows into side-by-side
// columns.
func TestHelpViewUsesSharedKeyLegend(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	for _, emoji := range []bool{false, true} {
		m := New(config.Config{})
		m.markerEmoji = emoji

		want := RenderKeyLegendGroups(KeyLegendGroups(emoji))
		if got := m.helpContent(); !strings.Contains(got, want) {
			t.Errorf("helpContent() (markerEmoji=%v) does not contain RenderKeyLegendGroups(KeyLegendGroups(%v)) verbatim.\nwant substring:\n%s\ngot:\n%s", emoji, emoji, want, got)
		}
	}
}

// addPort drives the "n" flow: open the input, type digits, submit.
func addPort(m model, digits string) model {
	res, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m = res.(model)
	for _, r := range digits {
		res, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = res.(model)
	}
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	return res.(model)
}

// TestAddPortFavorites covers ykgj: "n" registers + favorites a port (even one
// not listening), it shows up in the Favorites view, it does NOT serve, and it
// persists -- so an added port for a not-yet-running service sticks instead of
// vanishing.
func TestAddPortFavorites(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := New(config.Config{Ports: map[int]config.PortMeta{}})
	m.allPorts = []portscan.Port{{Number: 8080, Process: "web"}} // :3000 is NOT listening
	m.active = map[int]bool{}
	m.showAllPorts = false // Favorites view
	m.rebuildItems()

	m = addPort(m, "3000")

	// (1) Favorited, and visible in the Favorites view as a synthetic entry.
	if !m.cfg.Ports[3000].Favorite {
		t.Error("n should set Favorite=true for :3000")
	}
	found := false
	for _, n := range portNumbers(m) {
		if n == 3000 {
			found = true
		}
	}
	if !found {
		t.Errorf("Favorites view should include the added :3000; got %v", portNumbers(m))
	}

	// (2) No serve state change, no toggle in flight.
	if len(m.active) != 0 {
		t.Errorf("n must not change serve state; active = %v", m.active)
	}
	if m.pending != 0 {
		t.Errorf("n must not toggle serve; pending = %d", m.pending)
	}

	// (4) Persisted to disk so it survives a restart.
	loaded, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Ports[3000].Favorite {
		t.Error("the favorite should persist to disk")
	}
}

// TestAddPortPreservesMeta covers ykgj point 3: "n" sets Favorite while
// preserving any existing label and lock on that port.
func TestAddPortPreservesMeta(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := New(config.Config{Ports: map[int]config.PortMeta{3000: {Label: "api", Locked: true}}})
	m.allPorts = nil
	m.active = map[int]bool{}
	m.rebuildItems()

	m = addPort(m, "3000")

	meta := m.cfg.Ports[3000]
	if !meta.Favorite || meta.Label != "api" || !meta.Locked {
		t.Errorf("n should set Favorite while preserving label/lock; got %+v", meta)
	}
}

// TestEggArt covers amac's invariants (the shape itself is visual): the
// borderless egg's rows never exceed the width budget, its height matches the
// clamp, it's deterministic per (frame,size), it changes with the frame
// (animation), and it never overflows a narrow budget.
func TestEggArt(t *testing.T) {
	a := eggSpin(0, 21, 15)
	if len(a) != 15 {
		t.Errorf("egg height = %d, want 15", len(a))
	}

	// Every row has the SAME display width (all padded to the field width),
	// and none exceeds the budget.
	fieldW := lipgloss.Width(a[0])
	fill := make([]int, len(a)) // shimmer (non-space) run per row
	for i, ln := range a {
		if w := lipgloss.Width(ln); w != fieldW {
			t.Errorf("row %d width %d != field width %d (rows must be equal width)", i, w, fieldW)
		}
		if fieldW > 21 {
			t.Errorf("egg field width %d exceeds the 21-col budget", fieldW)
		}
		for _, r := range stripANSI(ln) {
			if r != ' ' {
				fill[i]++
			}
			switch r { // borderless: no outline glyphs
			case '|', '/', '\\', '-', '.', '\'', '‾', '_':
				t.Errorf("egg should be borderless; found outline glyph %q", r)
			}
		}
	}

	// Rounded caps: neither the top nor bottom row collapses to a spike.
	if fill[0] < 5 || fill[len(fill)-1] < 5 {
		t.Errorf("egg caps should be rounded (>=5 wide); top=%d bottom=%d", fill[0], fill[len(fill)-1])
	}

	// Egg asymmetry: the widest row is BELOW the vertical centre.
	widest := 0
	for i, f := range fill {
		if f > fill[widest] {
			widest = i
		}
	}
	if widest <= len(fill)/2 {
		t.Errorf("widest row %d should be below centre (%d)", widest, len(fill)/2)
	}

	// Deterministic per (frame,size); animates across the frame range. The
	// glint eases in/out, so adjacent frames near an extreme are visually
	// identical by design -- compare against the far end of the swing (~half
	// the ~22-frame breathing period) to confirm it does move.
	if !reflect.DeepEqual(a, eggSpin(0, 21, 15)) {
		t.Error("eggSpin must be deterministic for a given (frame,size)")
	}
	if reflect.DeepEqual(a, eggSpin(11, 21, 15)) {
		t.Error("eggSpin should change across the frame range")
	}

	// Narrow budget: clamps down, never overflows.
	for _, ln := range eggSpin(3, 8, 15) {
		if w := lipgloss.Width(ln); w > 8 {
			t.Errorf("clamped egg row width %d exceeds the 8-col budget", w)
		}
	}
}

// stripANSI removes SGR escape sequences so the underlying glyphs can be
// inspected in tests.
func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case r == '\x1b':
			inEsc = true
		case inEsc && r == 'm':
			inEsc = false
		case inEsc:
			// skip
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// TestEasterEgg covers 28mv at the state-machine level (visuals aren't unit
// tested): 'E' opens the overlay + schedules the animation tick; it's modal;
// 'c' copies the author's link with a toast without closing; esc/q/'E' close
// it and the tick stops (no leak); and it's hidden from help + legend.
func TestEasterEgg(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := New(config.Config{})
	m.active = map[int]bool{}
	m.rebuildItems()

	// 'E' opens and schedules the tick.
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'E'}})
	m = res.(model)
	if !m.showEgg {
		t.Fatal("E should open the egg overlay")
	}
	if cmd == nil {
		t.Error("opening the egg should schedule the animation tick")
	}

	// The tick advances the frame and reschedules while open.
	res, cmd = m.Update(eggTickMsg{})
	m = res.(model)
	if m.eggFrame == 0 || cmd == nil {
		t.Errorf("a tick while open should advance the frame (%d) and reschedule (%v)", m.eggFrame, cmd != nil)
	}

	// Modality: normal keys are swallowed; the egg stays open, nothing acts.
	for _, k := range []rune{' ', 'p', 'n', 'x', 'a'} {
		m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{k}})
	}
	if !m.showEgg || m.pending != 0 || m.mode != entryNone {
		t.Errorf("keys must be swallowed while the egg is open; showEgg=%v pending=%d mode=%v", m.showEgg, m.pending, m.mode)
	}

	// 'c' copies the author's link with a toast and stays open.
	if eggURL != "https://michaelgruen.com/" {
		t.Errorf("egg link = %q, want the author's site", eggURL)
	}
	res, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	m = res.(model)
	if !m.showEgg {
		t.Error("'c' must not close the egg")
	}
	if cmd == nil || !strings.Contains(m.flash, eggDomain) {
		t.Errorf("'c' should copy the link and toast; flash=%q cmd=%v", m.flash, cmd != nil)
	}

	// 'g' copies the GitHub repo link (2b4r), toasts, and stays open.
	if eggRepoURL != "https://github.com/gruen/tailport" {
		t.Errorf("egg repo link = %q, want the GitHub repo", eggRepoURL)
	}
	res, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	m = res.(model)
	if !m.showEgg {
		t.Error("'g' must not close the egg")
	}
	if cmd == nil || !strings.Contains(m.flash, eggRepoDomain) {
		t.Errorf("'g' should copy the repo link and toast; flash=%q cmd=%v", m.flash, cmd != nil)
	}

	// esc closes; a subsequent tick must NOT reschedule (no ticker leak).
	m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.showEgg {
		t.Error("esc should close the egg")
	}
	if _, cmd := m.Update(eggTickMsg{}); cmd != nil {
		t.Error("a tick after the egg closes must not reschedule (leak)")
	}

	// Pressing 'E' while open also closes it.
	m = mustUpdate(t, New(config.Config{}), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'E'}})
	if m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'E'}}); m.showEgg {
		t.Error("pressing E again should close the egg")
	}

	// 'g' is inert outside the egg overlay (only handled under showEgg).
	m4 := New(config.Config{})
	m4.active = map[int]bool{}
	m4.rebuildItems()
	m4 = mustUpdate(t, m4, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	if m4.showEgg || strings.Contains(m4.flash, eggRepoDomain) {
		t.Errorf("'g' outside the egg must do nothing; showEgg=%v flash=%q", m4.showEgg, m4.flash)
	}

	// Hidden: neither link is advertised in the help overlay or the legend.
	m3 := New(config.Config{})
	m3.help.Width = 200
	if strings.Contains(m3.helpView(), eggDomain) || strings.Contains(m3.helpView(), eggRepoDomain) {
		t.Error("the egg must not be documented in the help overlay")
	}
	if strings.Contains(m3.renderLegend(), eggDomain) || strings.Contains(m3.renderLegend(), eggRepoDomain) {
		t.Error("the egg must not appear in the bottom legend")
	}
}

// TestAutoRefresh covers e40f: the periodic tick reschedules itself and polls
// only when idle; a periodic poll's error fades silently while a manual/toggle
// refresh error still toasts; and the FQDN arrives via its own cached message.
func TestAutoRefresh(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// Idle tick -> reschedules and polls (non-nil cmd), no state change.
	m := New(config.Config{})
	res, cmd := m.Update(refreshTickMsg{})
	if cmd == nil {
		t.Error("an idle refreshTickMsg should return a cmd (poll + reschedule)")
	}
	_ = res.(model)

	// Tick while a toggle is in flight -> still reschedules, but must not clear
	// pending / stomp the op.
	m = New(config.Config{})
	m.pending = 8080
	res, cmd = m.Update(refreshTickMsg{})
	if cmd == nil {
		t.Error("a tick during an in-flight toggle should still reschedule")
	}
	if got := res.(model); got.pending != 8080 {
		t.Errorf("a tick must not disturb an in-flight toggle; pending = %d", got.pending)
	}

	// A periodic-poll error fades silently (no toast).
	m = New(config.Config{})
	m = mustUpdate(t, m, refreshMsg{auto: true, err: fmt.Errorf("tailscaled down")})
	if m.flash != "" {
		t.Errorf("an auto-refresh error must not raise a toast; got %q", m.flash)
	}

	// A manual/toggle refresh error still toasts.
	m = New(config.Config{})
	m = mustUpdate(t, m, refreshMsg{err: fmt.Errorf("tailscaled down")})
	if m.flash == "" || m.flashLevel != flashError {
		t.Errorf("a non-auto refresh error should toast; flash=%q level=%v", m.flash, m.flashLevel)
	}

	// FQDN arrives via its own message and is cached.
	m = New(config.Config{})
	m = mustUpdate(t, m, fqdnMsg{fqdn: "host.example.ts.net"})
	if m.fqdn != "host.example.ts.net" {
		t.Errorf("fqdnMsg should set m.fqdn; got %q", m.fqdn)
	}
}

// TestAddPortAlreadyFavorited covers 7ac3: 'n' on an already-favorited port
// is a no-op with an info toast; 'n' on a new port favorites it silently.
func TestAddPortAlreadyFavorited(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// Already favorited -> info toast, still favorited, no success on a new one.
	m := New(config.Config{Ports: map[int]config.PortMeta{8080: {Favorite: true}}})
	m.allPorts = []portscan.Port{{Number: 8080, Process: "web"}}
	m.active = map[int]bool{}
	m.showAllPorts = true
	m.rebuildItems()

	m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	for _, r := range "8080" {
		m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = res.(model)
	if !strings.Contains(m.flash, "already favorited") || m.flashLevel != flashInfo {
		t.Errorf("re-adding a favorite should show an info toast; flash=%q level=%v", m.flash, m.flashLevel)
	}
	if cmd == nil {
		t.Error("the toast should schedule its expiry")
	}
	if !m.cfg.Ports[8080].Favorite {
		t.Error(":8080 should remain favorited")
	}

	// A brand-new add favorites silently (no toast).
	m2 := New(config.Config{Ports: map[int]config.PortMeta{}})
	m2.active = map[int]bool{}
	m2.rebuildItems()
	m2 = addPort(m2, "3000")
	if !m2.cfg.Ports[3000].Favorite {
		t.Error("a new 'n' add should favorite the port")
	}
	if m2.flash != "" {
		t.Errorf("a new 'n' add should be silent; got toast %q", m2.flash)
	}
}

// TestErrorToasts covers q89g: errors are unified into the auto-dismissing
// toast system (severity error/red), schedule an expiry, honour flashID so a
// stale timer can't clear a newer toast, clear on the next keypress, and a
// failed toggle / "invalid port" both fade rather than persisting.
func TestErrorToasts(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// (1) Setting an error schedules expiry and tags severity.
	m := New(config.Config{})
	if cmd := m.setErr("boom"); cmd == nil {
		t.Error("setErr should return an expiry cmd")
	}
	if m.flash != "boom" || m.flashLevel != flashError {
		t.Errorf("setErr should set a red error toast; flash=%q level=%v", m.flash, m.flashLevel)
	}

	// (2) flashExpireMsg clears on a matching id, no-ops on a stale one.
	id := m.flashID
	if got := mustUpdate(t, m, flashExpireMsg{id: id - 1}); got.flash == "" {
		t.Error("a stale flashExpireMsg must not clear the toast")
	}
	if got := mustUpdate(t, m, flashExpireMsg{id: id}); got.flash != "" {
		t.Error("a matching flashExpireMsg should clear the toast")
	}

	// (3) A newer toast supersedes an older; the old timer no-ops.
	m = New(config.Config{})
	m.setFlash("first", flashInfo)
	m.setFlash("second", flashError)
	if got := mustUpdate(t, m, flashExpireMsg{id: 1}); got.flash != "second" {
		t.Errorf("an older timer must not clear the newer toast; got %q", got.flash)
	}

	// (6) A failed toggle raises an auto-dismissing error toast (not persistent).
	m = New(config.Config{})
	res, cmd := m.Update(toggleDoneMsg{port: 8080, err: fmt.Errorf("serve failed")})
	m = res.(model)
	if m.flash == "" || m.flashLevel != flashError {
		t.Errorf("failed toggle should raise an error toast; flash=%q level=%v", m.flash, m.flashLevel)
	}
	if cmd == nil {
		t.Error("failed toggle should schedule the toast's expiry (batched cmd)")
	}

	// (7) "invalid port" (n + 0) is an auto-dismissing error toast.
	m = New(config.Config{Ports: map[int]config.PortMeta{}})
	m.rebuildItems()
	m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'0'}})
	res, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = res.(model)
	if m.flash != "invalid port" || m.flashLevel != flashError {
		t.Errorf("invalid port should raise an error toast; flash=%q level=%v", m.flash, m.flashLevel)
	}
	if cmd == nil {
		t.Error("invalid port should schedule the toast's expiry")
	}
	// A subsequent keypress dismisses it.
	if got := mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}}); got.flash != "" {
		t.Errorf("the error toast should clear on the next keypress; got %q", got.flash)
	}
}

func mustUpdate(t *testing.T, m model, msg tea.Msg) model {
	t.Helper()
	res, _ := m.Update(msg)
	return res.(model)
}

// TestLabelPrefill covers vgn5: the 'l' label input prefills with the current
// label if set, else the process name, else empty; confirming the prefill
// persists it, editing replaces it, and esc leaves the existing label alone.
func TestLabelPrefill(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	lKey := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}}
	build := func(cfg config.Config, ports []portscan.Port) model {
		m := New(cfg)
		m.allPorts = ports
		m.active = map[int]bool{}
		m.showAllPorts = true
		m.rebuildItems()
		return m
	}
	openLabel := func(m model) model {
		res, _ := m.Update(lKey)
		return res.(model)
	}

	// (1) Existing label wins over the process name.
	m := openLabel(build(
		config.Config{Ports: map[int]config.PortMeta{8080: {Favorite: true, Label: "web"}}},
		[]portscan.Port{{Number: 8080, Process: "srv"}}))
	if m.mode != entryLabel {
		t.Fatalf("l should open entryLabel; mode = %v", m.mode)
	}
	if got := m.labelInput.Value(); got != "web" {
		t.Errorf("prefill with a label = %q, want \"web\"", got)
	}

	// (2) No label, has process -> process name.
	m = openLabel(build(config.Config{Ports: map[int]config.PortMeta{}},
		[]portscan.Port{{Number: 8080, Process: "srv"}}))
	if got := m.labelInput.Value(); got != "srv" {
		t.Errorf("prefill = %q, want the process name \"srv\"", got)
	}

	// (3) Neither (down favorite, no process) -> empty.
	m = openLabel(build(config.Config{Ports: map[int]config.PortMeta{9000: {Favorite: true}}}, nil))
	if got := m.labelInput.Value(); got != "" {
		t.Errorf("prefill = %q, want empty", got)
	}

	// (4) Confirming a prefilled process name (enter, no edits) persists it.
	m = openLabel(build(config.Config{Ports: map[int]config.PortMeta{}},
		[]portscan.Port{{Number: 8080, Process: "srv"}}))
	res, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = res.(model)
	if m.cfg.Ports[8080].Label != "srv" {
		t.Errorf("confirming the prefill should save the process name; got %q", m.cfg.Ports[8080].Label)
	}

	// (5a) Editing then confirming replaces the label.
	m = openLabel(build(config.Config{Ports: map[int]config.PortMeta{8080: {Label: "web"}}},
		[]portscan.Port{{Number: 8080}}))
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}}) // "web" -> "web2"
	m = res.(model)
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = res.(model)
	if m.cfg.Ports[8080].Label != "web2" {
		t.Errorf("edit+confirm should save the new label; got %q", m.cfg.Ports[8080].Label)
	}

	// (5b) esc leaves the existing label unchanged.
	m = openLabel(build(config.Config{Ports: map[int]config.PortMeta{8080: {Label: "web"}}},
		[]portscan.Port{{Number: 8080}}))
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = res.(model)
	if m.cfg.Ports[8080].Label != "web" {
		t.Errorf("esc should leave the label unchanged; got %q", m.cfg.Ports[8080].Label)
	}
}

// TestNoDefaultLabel pins the "nothing" non-bug (vgn5): newly registered ports
// carry an empty label -- there is no placeholder/default label.
func TestNoDefaultLabel(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// 'n' (favorite()) -> empty label.
	m := New(config.Config{Ports: map[int]config.PortMeta{}})
	m.active = map[int]bool{}
	m.rebuildItems()
	m = addPort(m, "3000")
	if m.cfg.Ports[3000].Label != "" {
		t.Errorf("n-added :3000 should have an empty label; got %q", m.cfg.Ports[3000].Label)
	}

	// A bare remembered port -> empty label.
	m2 := New(config.Config{Ports: map[int]config.PortMeta{}})
	m2.remember(4000)
	if m2.cfg.Ports[4000].Label != "" {
		t.Errorf("remembered :4000 should have an empty label; got %q", m2.cfg.Ports[4000].Label)
	}

	// The seeded default (:22) also carries no label.
	if config.Default().Ports[22].Label != "" {
		t.Error("default :22 should have an empty label")
	}
}

// TestUnlockSSHConfirm covers ah23: unlocking :22 is gated behind a
// type-"ssh" confirm, while locking :22 and non-:22 toggles stay instant.
func TestUnlockSSHConfirm(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	xKey := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}}

	// A model with a locked, selected :22 in the All ports view.
	lockedModel := func() model {
		m := New(config.Config{Ports: map[int]config.PortMeta{22: {Locked: true}}})
		m.allPorts = []portscan.Port{{Number: 22, Process: "sshd"}}
		m.active = map[int]bool{}
		m.showAllPorts = true
		m.rebuildItems()
		return m
	}
	// Type input then press enter, from within the confirm mode.
	confirm := func(m model, input string) model {
		for _, r := range input {
			res, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
			m = res.(model)
		}
		res, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		return res.(model)
	}

	// (1) 'x' on locked :22 opens the confirm and does NOT unlock yet.
	m := lockedModel()
	res, _ := m.Update(xKey)
	m = res.(model)
	if m.mode != entryConfirmUnlockSSH {
		t.Fatalf("x on locked :22 should open the ssh confirm; mode = %v", m.mode)
	}
	if !m.cfg.Ports[22].Locked {
		t.Error("x must not unlock :22 before confirmation")
	}

	// (2) Wrong/empty inputs never unlock; mode resets.
	for _, bad := range []string{"", "y", "no", "sshh", "s s h"} {
		m := lockedModel()
		res, _ := m.Update(xKey)
		m = confirm(res.(model), bad)
		if !m.cfg.Ports[22].Locked {
			t.Errorf("input %q must NOT unlock :22", bad)
		}
		if m.mode != entryNone {
			t.Errorf("input %q should reset mode to entryNone; got %v", bad, m.mode)
		}
	}

	// (3) Exact "ssh" unlocks and persists to disk.
	m = lockedModel()
	res, _ = m.Update(xKey)
	m = confirm(res.(model), "ssh")
	if m.cfg.Ports[22].Locked {
		t.Error(`"ssh" should unlock :22`)
	}
	if m.mode != entryNone {
		t.Errorf("mode should reset after unlock; got %v", m.mode)
	}
	if loaded, err := config.Load(""); err != nil {
		t.Fatal(err)
	} else if loaded.Ports[22].Locked {
		t.Error("the unlock should persist to disk")
	}

	// (4) Case-insensitive + trimmed accept.
	for _, good := range []string{"SSH", "Ssh", " ssh "} {
		m := lockedModel()
		res, _ := m.Update(xKey)
		m = confirm(res.(model), good)
		if m.cfg.Ports[22].Locked {
			t.Errorf("%q should unlock :22 (case-insensitive/trimmed)", good)
		}
	}

	// (5) esc cancels: :22 stays locked.
	m = lockedModel()
	res, _ = m.Update(xKey)
	m = res.(model)
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = res.(model)
	if !m.cfg.Ports[22].Locked {
		t.Error("esc should leave :22 locked")
	}
	if m.mode != entryNone {
		t.Errorf("esc should reset mode; got %v", m.mode)
	}

	// (6) 'x' on UNLOCKED :22 locks instantly, no prompt.
	m = New(config.Config{Ports: map[int]config.PortMeta{22: {Locked: false}}})
	m.allPorts = []portscan.Port{{Number: 22}}
	m.active = map[int]bool{}
	m.showAllPorts = true
	m.rebuildItems()
	res, _ = m.Update(xKey)
	m = res.(model)
	if m.mode != entryNone {
		t.Errorf("locking :22 should not prompt; mode = %v", m.mode)
	}
	if !m.cfg.Ports[22].Locked {
		t.Error("x on unlocked :22 should lock instantly")
	}

	// (7) 'x' on a non-:22 port toggles instantly (regression).
	m = New(config.Config{Ports: map[int]config.PortMeta{8080: {}}})
	m.allPorts = []portscan.Port{{Number: 8080}}
	m.active = map[int]bool{}
	m.showAllPorts = true
	m.rebuildItems()
	res, _ = m.Update(xKey)
	m = res.(model)
	if m.mode != entryNone || !m.cfg.Ports[8080].Locked {
		t.Errorf("x on :8080 should lock instantly; mode=%v locked=%v", m.mode, m.cfg.Ports[8080].Locked)
	}

	// (8) Modality: space/p while confirming must NOT toggle serve/funnel.
	m = lockedModel()
	res, _ = m.Update(xKey)
	m = res.(model)
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = res.(model)
	if m.pending != 0 || m.mode != entryConfirmUnlockSSH {
		t.Errorf("space in ssh-confirm must not toggle; pending=%d mode=%v", m.pending, m.mode)
	}
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	m = res.(model)
	if m.pending != 0 || m.mode != entryConfirmUnlockSSH {
		t.Errorf("p in ssh-confirm must not funnel; pending=%d mode=%v", m.pending, m.mode)
	}
}

// TestCopyKeymap covers vnq7's remap: "c" is copy, clean moved to "C", and
// the two don't collide.
func TestCopyKeymap(t *testing.T) {
	k := newKeyMap()
	if k.Copy.Help().Key != "c" {
		t.Errorf("Copy help key = %q, want c", k.Copy.Help().Key)
	}
	if k.Clean.Help().Key != "C" {
		t.Errorf("Clean help key = %q, want C", k.Clean.Help().Key)
	}
	cLower := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}}
	cUpper := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'C'}}
	if !key.Matches(cLower, k.Copy) || key.Matches(cLower, k.Clean) {
		t.Error("'c' should match Copy, not Clean")
	}
	if !key.Matches(cUpper, k.Clean) || key.Matches(cUpper, k.Copy) {
		t.Error("'C' should match Clean, not Copy")
	}
}

// TestCopyURL covers vnq7's copy action, updated for py5b and vqa3: state C
// (served + listening, not funnelled) goes INLINE -- the row's own
// "✓ copied" annotation, no toast -- since the row already shows the copied
// URL. Since vqa3, the "not served" case below is ALSO inline: with no
// explicit bind scope the port resolves to reachLocalhost, one of the four
// healthy states whose row text ("localhost only") already states what was
// copied.
func TestCopyURL(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	newModel := func(active bool) model {
		m := New(config.Config{Ports: map[int]config.PortMeta{8080: {Favorite: true}}})
		m.host = "host"
		m.width = 80 // wide enough that inlineCopyFits always succeeds here
		m.allPorts = []portscan.Port{{Number: 8080, Process: "web"}}
		if active {
			m.active = map[int]bool{8080: true}
		} else {
			m.active = map[int]bool{}
		}
		m.showAllPorts = true
		m.rebuildItems()
		return m
	}

	// State C: inline "✓ copied" on the row, copiedPort set, NO toast.
	m := newModel(true)
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	m = res.(model)
	if cmd == nil {
		t.Error("c should return a copy/rebuild/expire cmd")
	}
	if m.flash != "" {
		t.Errorf("state-C copy should NOT toast; flash = %q", m.flash)
	}
	if m.copiedPort != 8080 {
		t.Errorf("copiedPort = %d, want 8080", m.copiedPort)
	}
	sel, ok := m.list.SelectedItem().(portItem)
	if !ok || !sel.justCopied {
		t.Errorf("selected item justCopied = %v, want true", ok && sel.justCopied)
	}
	if got := stripANSI(sel.Description()); !strings.Contains(got, "✓ copied") || !strings.Contains(got, "http://host:8080") {
		t.Errorf("Description() = %q, want the tailnet URL plus the ✓ copied suffix", got)
	}
	// Unlike the toast, the inline annotation is NOT cleared by the next
	// keypress -- it fades only via copiedExpireMsg's id-guarded timer.
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if got := res.(model); got.copiedPort != 8080 {
		t.Errorf("copiedPort should survive an unrelated keypress; got %d", got.copiedPort)
	}

	// Not served (reachLocalhost: listening, unclassified/loopback bind,
	// nothing active): since vqa3 this is ALSO inline -- the row's own
	// "localhost only" text already says what got copied, so no toast.
	m = newModel(false)
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	m = res.(model)
	if m.flash != "" {
		t.Errorf("not-served (reachLocalhost) copy should not toast; flash = %q", m.flash)
	}
	if m.copiedPort != 8080 {
		t.Errorf("not-served (reachLocalhost) copy should set copiedPort; got %d", m.copiedPort)
	}
}

// TestInlineCopyFits covers py5b's width-fit boundary: the "✓ copied"
// annotation fits exactly at its required width, and is one cell too wide
// just below it -- the rule that keeps a narrow terminal from silently
// dropping the confirmation off the end-truncated row.
func TestInlineCopyFits(t *testing.T) {
	suffixWidth := lipgloss.Width(copiedSuffix)
	const descWidth = 30
	avail := descWidth + suffixWidth
	if !inlineCopyFits(descWidth, avail) {
		t.Errorf("inlineCopyFits(%d, %d) = false, want true (exact fit)", descWidth, avail)
	}
	if inlineCopyFits(descWidth, avail-1) {
		t.Errorf("inlineCopyFits(%d, %d) = true, want false (one cell too narrow)", descWidth, avail-1)
	}
}

// TestCopiedExpire covers the inline annotation's timed clear (py5b),
// mirroring TestFlashExpire: a matching copiedExpireMsg clears m.copiedPort,
// a stale one (an id superseded by a later copy) does not.
func TestCopiedExpire(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := New(config.Config{})
	m.host = "host"
	m.width = 80
	m.allPorts = []portscan.Port{{Number: 8080}}
	m.active = map[int]bool{8080: true}
	m.showAllPorts = true
	m.rebuildItems()

	res, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	m = res.(model)
	if m.copiedPort != 8080 {
		t.Fatalf("copiedPort = %d, want 8080 (state C should go inline)", m.copiedPort)
	}
	id := m.copiedID

	res, _ = m.Update(copiedExpireMsg{id: id - 1})
	if got := res.(model); got.copiedPort == 0 {
		t.Error("a stale copiedExpireMsg should not clear a newer annotation")
	}
	res, _ = m.Update(copiedExpireMsg{id: id})
	if got := res.(model); got.copiedPort != 0 {
		t.Errorf("a matching copiedExpireMsg should clear copiedPort; got %d", got.copiedPort)
	}
}

// TestCopyURLInlineVsToast covers py5b's precise inline-vs-toast boundary:
// only state C (served + listening + not funnelled), and only when the
// annotation actually fits, goes inline. Funnelled, dangling, and a state-C
// copy too wide for the terminal all keep the toast with copiedPort staying
// 0 -- see AGENTS.md's "When to go inline vs toast".
func TestCopyURLInlineVsToast(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	base := func() model {
		m := New(config.Config{Ports: map[int]config.PortMeta{8080: {Favorite: true}}})
		m.host = "host"
		m.width = 80
		m.allPorts = []portscan.Port{{Number: 8080, Process: "web"}}
		m.active = map[int]bool{8080: true}
		m.showAllPorts = true
		return m
	}

	// Funnelled: the row shows the PUBLIC url but "c" copies the TAILNET
	// url -- a mismatch, so the toast (which names what was copied) stays,
	// no inline.
	m := base()
	m.funnel = map[int]int{8080: 443}
	m.rebuildItems()
	res, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	m = res.(model)
	if m.copiedPort != 0 || m.flash == "" {
		t.Errorf("funnelled copy: copiedPort=%d flash=%q, want copiedPort 0 and a toast", m.copiedPort, m.flash)
	}

	// Dangling (active, but nothing listening): the row shows the stale
	// warning, no URL at all -- toast, no inline.
	m = base()
	m.allPorts = nil // nothing listening locally -> the active favorite is dangling
	m.rebuildItems()
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	m = res.(model)
	if m.copiedPort != 0 || m.flash == "" {
		t.Errorf("dangling copy: copiedPort=%d flash=%q, want copiedPort 0 and a toast", m.copiedPort, m.flash)
	}

	// State C, but the terminal is too narrow for the suffix to fit: falls
	// back to the toast rather than silently truncating the confirmation
	// off the row.
	m = base()
	m.width = 5
	m.rebuildItems()
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	m = res.(model)
	if m.copiedPort != 0 || m.flash == "" {
		t.Errorf("narrow state-C copy: copiedPort=%d flash=%q, want copiedPort 0 and a toast fallback", m.copiedPort, m.flash)
	}
}

// TestCopyURLReachAware covers 83wv/vqa3: copyURL's confirmation must be
// reach()-aware (parallel to Description()/markerGlyph()), not the pre-79xb
// binary sel.active, so it can never contradict the space guard
// (TestSpaceGuardForReachablePorts) for the same state. Since vqa3, the four
// healthy states -- A (reachLocalhost), B (reachTailnet), B' (reachLAN), and
// C (reachServed) -- all go inline (copiedPort set, no toast) at a wide
// width; the three principled exceptions -- D (reachFunnel), E (reachStale),
// F (reachOffline) -- keep the toast, unchanged.
func TestCopyURLReachAware(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	newModel := func(port int, scope portscan.BindScope, active, listening bool, bindHost string) model {
		m := New(config.Config{Ports: map[int]config.PortMeta{port: {Favorite: true}}})
		m.host = "host"
		m.width = 80 // wide enough that every inline state actually goes inline
		if listening {
			m.allPorts = []portscan.Port{{Number: port, Process: "srv", BindScope: scope, BindHost: bindHost}}
		}
		if active {
			m.active = map[int]bool{port: true}
		} else {
			m.active = map[int]bool{}
		}
		m.showAllPorts = true
		m.rebuildItems()
		return m
	}

	press := func(m model) model {
		res, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
		return res.(model)
	}

	// B: reachTailnet (wildcard bind, listening, unserved). The copied URL
	// already resolves across the tailnet, so the row's own "on tailnet"
	// description is enough -- no toast needed, and (crucially) nothing here
	// can say "localhost only" or "press space", which would contradict the
	// "already on tailnet — nothing to serve" no-op asserted by
	// TestSpaceGuardForReachablePorts.
	m := press(newModel(8080, portscan.ScopeWildcard, false, true, ""))
	if m.copiedPort != 8080 || m.flash != "" {
		t.Errorf("reachTailnet copy: copiedPort=%d flash=%q, want inline (copiedPort 8080, no toast)", m.copiedPort, m.flash)
	}

	// B': reachLAN (specific LAN IP, listening, unserved). The row already
	// says "local network only", so the inline ✓ is sufficient.
	m = press(newModel(3000, portscan.ScopeLAN, false, true, "192.168.1.50"))
	if m.copiedPort != 3000 || m.flash != "" {
		t.Errorf("reachLAN copy: copiedPort=%d flash=%q, want inline (copiedPort 3000, no toast)", m.copiedPort, m.flash)
	}

	// A: reachLocalhost (loopback bind, listening, unserved). The row already
	// says "localhost only", so the inline ✓ is sufficient -- no separate
	// "press space" toast is needed to convey that.
	m = press(newModel(9000, portscan.ScopeLoopback, false, true, ""))
	if m.copiedPort != 9000 || m.flash != "" {
		t.Errorf("reachLocalhost copy: copiedPort=%d flash=%q, want inline (copiedPort 9000, no toast)", m.copiedPort, m.flash)
	}

	// C: reachServed (served AND listening). Unchanged behavior from py5b --
	// still goes inline.
	m = press(newModel(8080, portscan.ScopeLoopback, true, true, ""))
	if m.copiedPort != 8080 || m.flash != "" {
		t.Errorf("reachServed copy: copiedPort=%d flash=%q, want inline (copiedPort 8080, no toast)", m.copiedPort, m.flash)
	}

	// F: reachOffline (down favorite, unserved). STILL a toast: nothing live
	// to copy, so "press space to serve it" is the actionable guidance.
	m = press(newModel(8025, portscan.ScopeLoopback, false, false, ""))
	if m.copiedPort != 0 {
		t.Errorf("reachOffline copy should not go inline; copiedPort = %d", m.copiedPort)
	}
	if m.flashLevel != flashWarn || !strings.Contains(m.flash, "localhost only; press space to serve it") || !strings.Contains(m.flash, "http://localhost:8025") {
		t.Errorf("reachOffline copy flash = %q (level=%v), want the localhost-only press-space toast naming http://localhost:8025", m.flash, m.flashLevel)
	}

	// E: reachStale (served, but nothing listening). STILL a toast: the
	// copied URL is dangling and resolves to nothing.
	m = press(newModel(8025, portscan.ScopeLoopback, true, false, ""))
	if m.copiedPort != 0 {
		t.Errorf("reachStale copy should not go inline; copiedPort = %d", m.copiedPort)
	}
	if m.flashLevel != flashInfo || !strings.HasPrefix(m.flash, "copied ✓") {
		t.Errorf("reachStale copy flash = %q (level=%v), want the plain copied-checkmark toast", m.flash, m.flashLevel)
	}

	// D: reachFunnel. STILL a toast: the row shows the PUBLIC url but c
	// copies the TAILNET url, so a bare inline ✓ would misstate what got
	// copied -- the toast names it explicitly.
	fm := newModel(8080, portscan.ScopeWildcard, true, true, "")
	fm.funnel = map[int]int{8080: 443}
	fm.rebuildItems()
	m = press(fm)
	if m.copiedPort != 0 {
		t.Errorf("reachFunnel copy should not go inline; copiedPort = %d", m.copiedPort)
	}
	if m.flashLevel != flashInfo || !strings.Contains(m.flash, "the tailnet url") {
		t.Errorf("reachFunnel copy flash = %q (level=%v), want a toast naming 'the tailnet url'", m.flash, m.flashLevel)
	}

	// C, but too narrow to inline: falls back to the toast rather than
	// silently truncating the confirmation off the row.
	nm := newModel(8080, portscan.ScopeWildcard, true, true, "")
	nm.width = 5
	nm.rebuildItems()
	m = press(nm)
	if m.copiedPort != 0 {
		t.Errorf("narrow reachServed copy should not go inline; copiedPort = %d", m.copiedPort)
	}
	if m.flashLevel != flashInfo || !strings.HasPrefix(m.flash, "copied ✓") {
		t.Errorf("narrow reachServed copy flash = %q (level=%v), want the plain copied-checkmark toast", m.flash, m.flashLevel)
	}
}

// TestInlineCopyUniversal covers vqa3's core change: the inline "✓ copied"
// confirmation is no longer state-C-only. It fires for every healthy
// copyable state (localhost/LAN/tailnet/served) when the annotation fits,
// gracefully falls back to the toast when the row is too narrow, and the
// annotation correctly migrates when the selection moves between two
// eligible rows.
func TestInlineCopyUniversal(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	newModel := func(port int, scope portscan.BindScope, active, listening bool, bindHost string) model {
		m := New(config.Config{Ports: map[int]config.PortMeta{port: {Favorite: true}}})
		m.host = "host"
		m.width = 80
		if listening {
			m.allPorts = []portscan.Port{{Number: port, Process: "srv", BindScope: scope, BindHost: bindHost}}
		}
		if active {
			m.active = map[int]bool{port: true}
		} else {
			m.active = map[int]bool{}
		}
		m.showAllPorts = true
		m.rebuildItems()
		return m
	}

	press := func(m model) model {
		res, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
		return res.(model)
	}

	// The four inline-eligible states, at a wide width: c goes inline (no
	// toast), and the annotated row's Description carries the suffix.
	inlineCases := []struct {
		name      string
		port      int
		scope     portscan.BindScope
		active    bool
		listening bool
		bindHost  string
	}{
		{"localhost", 9000, portscan.ScopeLoopback, false, true, ""},
		{"LAN", 3000, portscan.ScopeLAN, false, true, "10.0.0.9"},
		{"tailnet", 8080, portscan.ScopeWildcard, false, true, ""},
		{"served", 8080, portscan.ScopeLoopback, true, true, ""},
	}
	for _, tc := range inlineCases {
		t.Run(tc.name, func(t *testing.T) {
			m := press(newModel(tc.port, tc.scope, tc.active, tc.listening, tc.bindHost))
			if m.copiedPort != tc.port || m.flash != "" {
				t.Fatalf("%s copy: copiedPort=%d flash=%q, want inline (copiedPort %d, no toast)", tc.name, m.copiedPort, m.flash, tc.port)
			}
			sel, ok := m.list.SelectedItem().(portItem)
			if !ok || !sel.justCopied {
				t.Fatalf("%s: selected item justCopied = %v, want true", tc.name, ok && sel.justCopied)
			}
			if got := stripANSI(sel.Description()); !strings.Contains(got, "✓ copied") {
				t.Errorf("%s Description() = %q, want it to carry the ✓ copied suffix", tc.name, got)
			}
		})
	}

	// inlineCopyState() itself: true for the four healthy states, false for
	// the three toast exceptions (funnel/stale/offline).
	elig := []struct {
		name string
		item portItem
		want bool
	}{
		{"reachLocalhost", portItem{port: portscan.Port{Number: 9000, BindScope: portscan.ScopeLoopback}, listening: true}, true},
		{"reachLAN", portItem{port: portscan.Port{Number: 3000, BindScope: portscan.ScopeLAN, BindHost: "10.0.0.9"}, listening: true}, true},
		{"reachTailnet", portItem{port: portscan.Port{Number: 8080, BindScope: portscan.ScopeWildcard}, listening: true}, true},
		{"reachServed", portItem{port: portscan.Port{Number: 8080}, listening: true, active: true}, true},
		{"reachFunnel", portItem{port: portscan.Port{Number: 8080}, listening: true, active: true, funnelPublic: 443}, false},
		{"reachStale", portItem{port: portscan.Port{Number: 8025}, active: true}, false},
		{"reachOffline", portItem{port: portscan.Port{Number: 8025}}, false},
	}
	for _, tc := range elig {
		if got := tc.item.inlineCopyState(); got != tc.want {
			t.Errorf("%s.inlineCopyState() = %v, want %v", tc.name, got, tc.want)
		}
	}

	// NARROW fallback: an inline-eligible state whose annotation wouldn't
	// fit the row falls back to the toast, exactly like state C always has.
	nm := newModel(9000, portscan.ScopeLoopback, false, true, "")
	nm.width = 5
	nm.rebuildItems()
	m := press(nm)
	if m.copiedPort != 0 || m.flash == "" {
		t.Errorf("narrow localhost copy: copiedPort=%d flash=%q, want a toast fallback (copiedPort 0, non-empty flash)", m.copiedPort, m.flash)
	}

	// Rapid A -> B: copying port A (inline), then moving the selection to a
	// second eligible port B and copying again, must move the annotation --
	// not leave A's copiedPort stuck.
	m2 := New(config.Config{Ports: map[int]config.PortMeta{
		8080: {Favorite: true},
		9000: {Favorite: true},
	}})
	m2.host = "host"
	m2.width = 80
	m2.allPorts = []portscan.Port{
		{Number: 8080, Process: "a", BindScope: portscan.ScopeWildcard},
		{Number: 9000, Process: "b", BindScope: portscan.ScopeLoopback},
	}
	m2.active = map[int]bool{}
	m2.showAllPorts = true
	m2.rebuildItems()
	m2.selectPort(8080)
	res, _ := m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	m2 = res.(model)
	if m2.copiedPort != 8080 {
		t.Fatalf("copy A: copiedPort = %d, want 8080", m2.copiedPort)
	}
	firstID := m2.copiedID

	m2.selectPort(9000)
	res, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	m2 = res.(model)
	if m2.copiedPort != 9000 {
		t.Errorf("copy B: copiedPort = %d, want 9000 (annotation should move)", m2.copiedPort)
	}
	if m2.copiedID <= firstID {
		t.Errorf("copiedID should increment on the second copy; first=%d second=%d", firstID, m2.copiedID)
	}
}

// TestCopyTargetURL is the direct check on copyTargetURL, the single source
// of truth for what "c" writes to the clipboard (83wv): it asserts the exact
// URL string per reach state, independent of the toast wording covered by
// TestCopyURLReachAware.
func TestCopyTargetURL(t *testing.T) {
	m := model{host: "host"}

	for _, tc := range []struct {
		name string
		item portItem
		want string
	}{
		{
			name: "reachLocalhost",
			item: portItem{port: portscan.Port{Number: 9000, BindScope: portscan.ScopeLoopback}, listening: true},
			want: "http://localhost:9000",
		},
		{
			name: "reachOffline",
			item: portItem{port: portscan.Port{Number: 8025, BindScope: portscan.ScopeLoopback}},
			want: "http://localhost:8025",
		},
		{
			name: "reachLAN/v4",
			item: portItem{port: portscan.Port{Number: 3000, BindScope: portscan.ScopeLAN, BindHost: "192.168.1.50"}, listening: true},
			want: "http://192.168.1.50:3000",
		},
		{
			name: "reachLAN/ipv6",
			item: portItem{port: portscan.Port{Number: 3000, BindScope: portscan.ScopeLAN, BindHost: "fe80::1"}, listening: true},
			want: "http://[fe80::1]:3000",
		},
		{
			name: "reachLAN/emptyBindHost",
			item: portItem{port: portscan.Port{Number: 3000, BindScope: portscan.ScopeLAN, BindHost: ""}, listening: true},
			want: "http://host:3000",
		},
		{
			name: "reachTailnet",
			item: portItem{port: portscan.Port{Number: 8080, BindScope: portscan.ScopeWildcard}, listening: true},
			want: "http://host:8080",
		},
		{
			name: "reachServed",
			item: portItem{port: portscan.Port{Number: 8080, BindScope: portscan.ScopeLoopback}, active: true, listening: true},
			want: "http://host:8080",
		},
		{
			name: "reachFunnel",
			item: portItem{port: portscan.Port{Number: 8080, BindScope: portscan.ScopeLoopback}, active: true, listening: true, funnelPublic: 443},
			want: "http://host:8080",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := m.copyTargetURL(tc.item); got != tc.want {
				t.Errorf("copyTargetURL(%+v) = %q, want %q", tc.item, got, tc.want)
			}
		})
	}
}

// TestFlashExpire covers the toast's timed clear: a matching flashExpireMsg
// clears it, a stale one (older id, from a superseded toast) does not.
func TestFlashExpire(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := New(config.Config{})
	m.host = "host"
	m.allPorts = []portscan.Port{{Number: 8080}}
	m.active = map[int]bool{8080: true}
	m.showAllPorts = true
	m.rebuildItems()

	res, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	m = res.(model)
	id := m.flashID

	res, _ = m.Update(flashExpireMsg{id: id - 1})
	if got := res.(model); got.flash == "" {
		t.Error("a stale flashExpireMsg should not clear a newer toast")
	}
	res, _ = m.Update(flashExpireMsg{id: id})
	if got := res.(model); got.flash != "" {
		t.Error("a matching flashExpireMsg should clear the toast")
	}
}

// TestCleanMovedToShiftC covers vnq7's other half: "C" still opens the clean
// confirm when dangling forwards exist, and "c" no longer does.
func TestCleanMovedToShiftC(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := New(config.Config{})
	m.host = "host"
	m.allPorts = []portscan.Port{{Number: 3000}} // listening
	m.active = map[int]bool{8080: true}          // served but not listening -> dangling
	m.showAllPorts = true
	m.rebuildItems()

	res, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'C'}})
	if got := res.(model); got.mode != entryConfirmClean {
		t.Errorf("'C' should open the clean confirm; mode = %v", got.mode)
	}

	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	if got := res.(model); got.mode == entryConfirmClean {
		t.Error("'c' should copy, not open the clean confirm")
	}
}

// TestStatusText covers k4ph's multi-state breakdown: listening (portscan
// total), on tailnet (served count -- 79xb dropped the "exposed" qualifier),
// and public (funnel count), with in-flight operation messages taking
// precedence and narrow terminals degrading gracefully.
func TestStatusText(t *testing.T) {
	base := model{
		host:     "host",
		width:    120,
		allPorts: []portscan.Port{{Number: 22}, {Number: 3000}, {Number: 8080}, {Number: 9000}, {Number: 5000}},
		active:   map[int]bool{8080: true, 3000: true},
		funnel:   map[int]int{9000: 443},
	}

	got := base.statusText()
	// Host on the listening segment (20w6), funnel count labelled "public
	// (funnel)" (67zk), no trailing "— host".
	for _, want := range []string{"5 listening on host", "2 on tailnet", "1 public (funnel)"} {
		if !strings.Contains(got, want) {
			t.Errorf("statusText = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "— host") {
		t.Errorf("statusText should not use the trailing '— host' form; got %q", got)
	}

	// Zero of everything reads cleanly, not "no ports"; no host -> no "on".
	empty := model{host: "", width: 120}
	if got := empty.statusText(); got != "0 listening · 0 on tailnet · 0 public (funnel)" {
		t.Errorf("empty (no host) statusText = %q", got)
	}

	// In-flight operations take precedence over the breakdown.
	pending := base
	pending.pending = 8080
	if got := pending.statusText(); got != "toggling :8080..." {
		t.Errorf("pending statusText = %q, want the toggling message", got)
	}
	cleaning := base
	cleaning.cleaning = 2
	if got := cleaning.statusText(); !strings.Contains(got, "cleaning 2 stale") {
		t.Errorf("cleaning statusText = %q", got)
	}

	// A narrow terminal abbreviates instead of overflowing.
	narrow := base
	narrow.width = 18
	got = narrow.statusText()
	if lipgloss.Width(got) > narrow.width {
		t.Errorf("narrow statusText %q width %d exceeds %d", got, lipgloss.Width(got), narrow.width)
	}
	if !strings.Contains(got, "5") { // still conveys the listening count
		t.Errorf("narrow statusText = %q, expected to keep the counts", got)
	}
}

// TestRequestFunnel covers yt69's escalation gate: :22 is hard-blocked, a
// normal turn-on defers to the entryConfirmFunnel prompt with the auto-assigned
// public port, a 4th funnel is refused, and turning an already-funnelled port
// off runs immediately (no confirm) back toward tailnet-served.
func TestRequestFunnel(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// :22 hard-block: an error toast, no confirm opened. (The returned cmd is
	// the toast expiry tick, not a funnel op.)
	m := New(config.Config{Ports: map[int]config.PortMeta{}})
	m.requestFunnel(22)
	if m.flash == "" || m.flashLevel != flashError {
		t.Errorf("funnel :22 should raise an error toast; flash=%q level=%v", m.flash, m.flashLevel)
	}
	if m.pending != 0 {
		t.Error("funnel :22 should not begin a funnel")
	}
	if m.mode != entryNone {
		t.Errorf("funnel :22 mode = %v, want entryNone", m.mode)
	}

	// Normal port turn-on: defers to the confirm, auto-assigns :443 first.
	m = New(config.Config{Ports: map[int]config.PortMeta{}})
	if cmd := m.requestFunnel(3000); cmd != nil {
		t.Error("funnel turn-on should defer (nil cmd) pending confirm")
	}
	if m.mode != entryConfirmFunnel || m.funnelPort != 3000 || m.funnelPublic != 443 || !m.funnelTurnOn {
		t.Errorf("funnel state = mode:%v port:%d pub:%d on:%v", m.mode, m.funnelPort, m.funnelPublic, m.funnelTurnOn)
	}

	// From that confirm, "y" begins the funnel: real cmd, pending set, closed.
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if cmd == nil {
		t.Error("y should begin the funnel (non-nil cmd)")
	}
	gm := res.(model)
	if gm.pending != 3000 {
		t.Errorf("after confirm, pending = %d, want 3000", gm.pending)
	}
	if gm.mode != entryNone {
		t.Errorf("after confirm, mode = %v, want entryNone", gm.mode)
	}

	// Any other key cancels the confirm with no funnel call.
	m = New(config.Config{Ports: map[int]config.PortMeta{}})
	m.requestFunnel(3000)
	res, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if cmd != nil {
		t.Error("non-y should cancel the funnel confirm (nil cmd)")
	}
	if gm := res.(model); gm.mode != entryNone || gm.pending != 0 {
		t.Errorf("cancel state = mode:%v pending:%d", gm.mode, gm.pending)
	}

	// All three ingress ports taken: a 4th funnel is refused with an error toast.
	m = New(config.Config{Ports: map[int]config.PortMeta{}})
	m.funnel = map[int]int{3000: 443, 3001: 8443, 3002: 10000}
	m.requestFunnel(9999)
	if m.flash == "" || m.flashLevel != flashError || m.mode != entryNone {
		t.Errorf("4th funnel should raise an error toast without a confirm; flash=%q level=%v mode=%v", m.flash, m.flashLevel, m.mode)
	}
	if m.pending != 0 {
		t.Error("4th funnel should not begin a funnel")
	}

	// Already funnelled: "p" turns it off immediately (no confirm), pending set.
	m = New(config.Config{Ports: map[int]config.PortMeta{}})
	m.funnel = map[int]int{3000: 443}
	if cmd := m.requestFunnel(3000); cmd == nil {
		t.Error("funnel-off should return a cmd")
	}
	if m.mode != entryNone {
		t.Errorf("funnel-off should not open a confirm; mode = %v", m.mode)
	}
	if m.pending != 3000 {
		t.Errorf("funnel-off pending = %d, want 3000", m.pending)
	}
}

// TestNextFunnelPort covers the 443 -> 8443 -> 10000 auto-assign order and the
// "all three taken" refusal.
func TestNextFunnelPort(t *testing.T) {
	cases := []struct {
		name string
		used map[int]int
		want int
		ok   bool
	}{
		{"none used -> 443", map[int]int{}, 443, true},
		{"443 used -> 8443", map[int]int{3000: 443}, 8443, true},
		{"443+8443 used -> 10000", map[int]int{3000: 443, 3001: 8443}, 10000, true},
		{"all three used -> refuse", map[int]int{3000: 443, 3001: 8443, 3002: 10000}, 0, false},
		{"gap at 443 -> lowest free", map[int]int{3001: 8443}, 443, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := model{funnel: c.used}
			got, ok := m.nextFunnelPort()
			if got != c.want || ok != c.ok {
				t.Errorf("nextFunnelPort(%v) = (%d,%v), want (%d,%v)", c.used, got, ok, c.want, c.ok)
			}
		})
	}
}

// TestFunnelItemRender covers the list row for a funnelled port: the distinct
// public ● marker (ASCII mode) and a public HTTPS URL in the description, both
// overriding the tailnet-serve presentation even when the port is also served.
func TestFunnelItemRender(t *testing.T) {
	it := portItem{
		port:         portscan.Port{Number: 3000, Process: "node"},
		active:       true, // also served on the tailnet ...
		listening:    true,
		host:         "host",
		fqdn:         "host.example.ts.net",
		funnelPublic: 8443, // ... but funnel outranks it
	}
	got := it.Title()
	if !strings.Contains(got, "●") {
		t.Errorf("funnelled Title should carry the public ● marker; got %q", got)
	}
	if strings.Contains(got, "◉") {
		t.Errorf("funnelled Title should not show the tailnet ◉ marker; got %q", got)
	}
	if got := it.reach(); got != reachFunnel {
		t.Errorf("reach() = %v, want reachFunnel", got)
	}
	desc := it.Description()
	if !strings.Contains(desc, "on the internet · https://host.example.ts.net:8443") {
		t.Errorf("funnelled Description should show the honest 'on the internet' prefix and public URL; got %q", desc)
	}
	if strings.Contains(desc, "http://host:3000") {
		t.Errorf("funnelled Description should not show the tailnet URL; got %q", desc)
	}
}

// TestUpdateFunnelKey covers the "p" key at the Update layer: on a selected
// non-:22 port it opens the public-internet confirm.
func TestUpdateFunnelKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := New(config.Config{Ports: map[int]config.PortMeta{8080: {Favorite: true}}})
	m.allPorts = []portscan.Port{{Number: 8080, Process: "srv"}}
	m.active = map[int]bool{}
	m.showAllPorts = true
	m.rebuildItems()

	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if cmd != nil {
		t.Error("p should defer to the confirm (nil cmd)")
	}
	if got := res.(model); got.mode != entryConfirmFunnel || got.funnelPort != 8080 {
		t.Errorf("after p, mode=%v funnelPort=%d, want entryConfirmFunnel/8080", got.mode, got.funnelPort)
	}
}

// TestHeader covers ttny: the cyan "tailport" wordmark and the Favorites|All
// toggle live in one persistent top header, drawn above both the list and the
// empty state -- so the logo survives an empty view -- and the toggle no
// longer appears in the bottom bar.
func TestHeader(t *testing.T) {
	m := New(config.Config{Ports: map[int]config.PortMeta{}})
	m.width = 80
	m.height = 24

	header := m.renderHeader()
	if !strings.Contains(header, "tailport") {
		t.Errorf("header should contain the logo; got %q", header)
	}
	for _, seg := range []string{"Favorites", "All ports"} {
		if !strings.Contains(header, seg) {
			t.Errorf("header should contain view toggle segment %q; got %q", seg, header)
		}
	}

	// Empty favorites view (fresh config): the whole View still leads with the
	// logo, even though the body is the empty-state message, not the list.
	if got := m.View(); !strings.Contains(got, "tailport") {
		t.Error("empty-state View should still contain the persistent logo")
	}

	// The toggle moved to the header, so the bottom bar must not duplicate it.
	// (p39s: "Favorites" now legitimately appears as a key-group column header,
	// so match on "All ports" -- the toggle's distinguishing segment, which the
	// grouped legend never contains.)
	if bottom := m.renderBottom(); strings.Contains(bottom, "All ports") {
		t.Errorf("bottom bar should not contain the view toggle; got %q", bottom)
	}
}

// buildHistoryModel is a model with one selected listening port, ready to
// drive registry edits through Update.
func buildHistoryModel(t *testing.T, cfg config.Config, ports []portscan.Port) model {
	t.Helper()
	m := New(cfg)
	m.allPorts = ports
	m.active = map[int]bool{}
	m.showAllPorts = true
	m.rebuildItems()
	return m
}

func pressRune(t *testing.T, m model, r rune) model {
	t.Helper()
	return mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
}

// TestForgetKey covers 3cwx's rename half: shift-F now does what "u" used to
// (clear ★, dropping the entry when nothing else is worth keeping), and "u"
// no longer unfavorites anything.
func TestForgetKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ports := []portscan.Port{{Number: 8080, Process: "srv"}}

	m := buildHistoryModel(t, config.Config{Ports: map[int]config.PortMeta{8080: {Favorite: true}}}, ports)
	m = pressRune(t, m, 'F')
	if _, ok := m.cfg.Ports[8080]; ok {
		t.Errorf("F should forget :8080 and drop its bare entry; registry still has %+v", m.cfg.Ports[8080])
	}

	// A labelled/locked port keeps its entry, same as the old "u" did.
	m = buildHistoryModel(t, config.Config{Ports: map[int]config.PortMeta{8080: {Favorite: true, Label: "web"}}}, ports)
	m = pressRune(t, m, 'F')
	if got := m.cfg.Ports[8080]; got.Favorite || got.Label != "web" {
		t.Errorf("F on a labelled port should clear ★ but keep the entry+label; got %+v", got)
	}

	// "u" is undo now -- with an empty history it must not touch the registry.
	m = buildHistoryModel(t, config.Config{Ports: map[int]config.PortMeta{8080: {Favorite: true}}}, ports)
	m = pressRune(t, m, 'u')
	if !m.cfg.Ports[8080].Favorite {
		t.Error("u must not unfavorite any more (it's undo); :8080 lost its ★")
	}
}

// TestUndoRedo covers the core of 3cwx: u steps registry edits back, ctrl+r
// steps them forward, and a new edit clears the redo stack.
func TestUndoRedo(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ports := []portscan.Port{{Number: 8080, Process: "srv"}}
	ctrlR := tea.KeyMsg{Type: tea.KeyCtrlR}

	// favorite -> undo restores "no entry at all" (not a zeroed entry: the two
	// are different states to the Favorites view).
	m := buildHistoryModel(t, config.Config{Ports: map[int]config.PortMeta{}}, ports)
	m = pressRune(t, m, 'f')
	if !m.cfg.Ports[8080].Favorite {
		t.Fatalf("f should favorite :8080; got %+v", m.cfg.Ports[8080])
	}
	m = pressRune(t, m, 'u')
	if _, ok := m.cfg.Ports[8080]; ok {
		t.Errorf("undo of a favorite on a previously-unregistered port should leave NO entry; got %+v", m.cfg.Ports[8080])
	}
	// redo puts it back.
	m = mustUpdate(t, m, ctrlR)
	if !m.cfg.Ports[8080].Favorite {
		t.Errorf("redo should re-apply the favorite; got %+v", m.cfg.Ports[8080])
	}

	// Undo is multi-step and ordered: lock, then label, then undo twice peels
	// them off newest-first.
	m = buildHistoryModel(t, config.Config{Ports: map[int]config.PortMeta{}}, ports)
	m = pressRune(t, m, 'f')
	m = pressRune(t, m, 'x')
	if !m.cfg.Ports[8080].Locked {
		t.Fatalf("x should lock :8080; got %+v", m.cfg.Ports[8080])
	}
	m = pressRune(t, m, 'u')
	if m.cfg.Ports[8080].Locked {
		t.Errorf("first undo should reverse the lock; got %+v", m.cfg.Ports[8080])
	}
	if !m.cfg.Ports[8080].Favorite {
		t.Errorf("first undo should reverse ONLY the lock, leaving the earlier favorite; got %+v", m.cfg.Ports[8080])
	}
	m = pressRune(t, m, 'u')
	if _, ok := m.cfg.Ports[8080]; ok {
		t.Errorf("second undo should reverse the favorite too; got %+v", m.cfg.Ports[8080])
	}

	// A fresh edit clears the redo stack: you can't redo onto a registry that
	// has moved on.
	m = buildHistoryModel(t, config.Config{Ports: map[int]config.PortMeta{}}, ports)
	m = pressRune(t, m, 'f')
	m = pressRune(t, m, 'u')
	if len(m.redoStack) != 1 {
		t.Fatalf("undo should leave one redoable edit; got %d", len(m.redoStack))
	}
	m = pressRune(t, m, 'x') // a new edit
	if len(m.redoStack) != 0 {
		t.Errorf("a new edit must clear the redo stack; got %d entries", len(m.redoStack))
	}

	// Empty stacks are a no-op with a toast, not a crash.
	m = buildHistoryModel(t, config.Config{Ports: map[int]config.PortMeta{}}, ports)
	m = pressRune(t, m, 'u')
	if m.flash == "" {
		t.Error("u with nothing to undo should flash 'nothing to undo'")
	}
	m = mustUpdate(t, m, ctrlR)
	if m.flash == "" {
		t.Error("ctrl+r with nothing to redo should flash 'nothing to redo'")
	}
}

// TestUndoIgnoresBookkeeping is why registryEdit holds per-port deltas rather
// than whole-config snapshots. The registry is written by things the user never
// asked for -- remember() when a port is served, rememberProcesses() on every
// background refresh -- and undo must neither step through those nor clobber
// them when reversing an unrelated port's edit.
func TestUndoIgnoresBookkeeping(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := buildHistoryModel(t, config.Config{Ports: map[int]config.PortMeta{}},
		[]portscan.Port{{Number: 8080, Process: "srv"}})

	m = pressRune(t, m, 'f') // the only deliberate edit
	m.remember(3000)         // bookkeeping: :3000 was served
	if _, ok := m.cfg.Ports[3000]; !ok {
		t.Fatal("remember(3000) should have registered :3000")
	}

	if len(m.undoStack) != 1 {
		t.Errorf("only the deliberate edit belongs on the undo stack; got %d entries", len(m.undoStack))
	}
	m = pressRune(t, m, 'u')
	if _, ok := m.cfg.Ports[3000]; !ok {
		t.Error("undo of :8080's favorite must not erase :3000's remembered entry -- per-port deltas, not config snapshots")
	}
	if _, ok := m.cfg.Ports[8080]; ok {
		t.Error("undo should still have reversed :8080's favorite")
	}
}

// TestUndoCannotUnlockSSH is the safety invariant: undo/redo may never land the
// registry somewhere the user couldn't have reached by pressing keys directly
// without a confirm. Unlocking :22 is gated behind typing "ssh" (ah23) because
// it guards SSH access, so an undo that would strip that lock is refused --
// otherwise "x" then "u" would be an unprompted back door around the gate.
func TestUndoCannotUnlockSSH(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := buildHistoryModel(t, config.Config{Ports: map[int]config.PortMeta{}},
		[]portscan.Port{{Number: 22, Process: "sshd"}})

	m = pressRune(t, m, 'x') // lock :22 -- instant, no confirm needed
	if !m.cfg.Ports[22].Locked {
		t.Fatalf("x should lock :22; got %+v", m.cfg.Ports[22])
	}

	m = pressRune(t, m, 'u') // undo would unlock it -> must be refused
	if !m.cfg.Ports[22].Locked {
		t.Error("undo must NOT unlock :22 -- that would bypass the typed ssh confirm")
	}
	if !strings.Contains(m.flash, ":22") {
		t.Errorf("the refusal should say why and name :22; flash = %q", m.flash)
	}
	// Refused, not consumed: the edit stays available for after a deliberate unlock.
	if len(m.undoStack) != 1 {
		t.Errorf("a refused undo must leave the stack untouched; got %d entries", len(m.undoStack))
	}

	// The safe direction is fine: undo that RE-locks :22 needs no confirm.
	m = buildHistoryModel(t, config.Config{Ports: map[int]config.PortMeta{22: {Locked: true}}},
		[]portscan.Port{{Number: 22, Process: "sshd"}})
	m = pressRune(t, m, 'x') // opens the ssh confirm rather than unlocking
	if m.mode != entryConfirmUnlockSSH {
		t.Fatalf("x on a locked :22 should open the ssh confirm; mode = %v", m.mode)
	}
	for _, r := range "ssh" {
		m = pressRune(t, m, r)
	}
	m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.cfg.Ports[22].Locked {
		t.Fatalf("typing ssh should unlock :22; got %+v", m.cfg.Ports[22])
	}
	m = pressRune(t, m, 'u')
	if !m.cfg.Ports[22].Locked {
		t.Error("undo of an ssh-confirmed unlock should re-lock :22 -- the safe direction needs no confirm")
	}
}

// TestUndoStackBounded pins the cap: a long session can't grow the history
// without bound, and the OLDEST edits are the ones dropped.
func TestUndoStackBounded(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := buildHistoryModel(t, config.Config{Ports: map[int]config.PortMeta{}},
		[]portscan.Port{{Number: 8080, Process: "srv"}})

	for i := 0; i < undoStackLimit+10; i++ {
		m = pressRune(t, m, 'x') // lock/unlock toggles, one edit each
	}
	if len(m.undoStack) != undoStackLimit {
		t.Errorf("undo stack = %d entries, want it capped at %d", len(m.undoStack), undoStackLimit)
	}
}

// TestKeyLegendDescsCoverEveryBinding guards the drift KeyLegendGroups can't:
// it looks descriptions up by key, so a binding with no entry silently renders
// a blank "?" overlay row rather than failing. 3cwx added three keys at once.
func TestKeyLegendDescsCoverEveryBinding(t *testing.T) {
	descs := keyLegendDescs(false)
	for _, g := range newKeyMap().groups() {
		for _, b := range g.bindings {
			k := b.Help().Key
			if strings.TrimSpace(descs[k]) == "" {
				t.Errorf("group %q binding %q has no keyLegendDescs entry -- the ? overlay would show a blank row", g.name, k)
			}
		}
	}
}

// TestDisplayVersion covers 0qy8's version formatting. The header wants the
// tag-shaped "v0.1.4", but build.yml stamps main.version with the tag MINUS
// its v, so the prefix has to be added back -- without ever producing "vdev"
// for an unstamped local build, and without doubling an already-present v.
func TestDisplayVersion(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"0.1.4", "v0.1.4"},  // the release case: build.yml's ${GITHUB_REF_NAME#v}
		{"dev", "dev"},       // unstamped local build -- never "vdev"
		{"", ""},             // unknown -> caller draws nothing
		{"v0.1.4", "v0.1.4"}, // already tag-shaped -> no "vv0.1.4"
		{"0.2.0-rc1", "v0.2.0-rc1"},
	} {
		if got := displayVersion(tc.in); got != tc.want {
			t.Errorf("displayVersion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestHeaderVersion covers 0qy8: the build version rides in the top bar just
// after the wordmark, in a color distinct from it, and an unknown version
// degrades to the bare wordmark rather than a stray "v" or empty styling.
func TestHeaderVersion(t *testing.T) {
	m := New(config.Config{Ports: map[int]config.PortMeta{}})
	m.width = 80
	m.height = 24

	// Unset (every New(cfg) call site): wordmark only, no version artifacts.
	if got := m.renderHeader(); strings.Contains(got, "v0.") || strings.Contains(got, "dev") {
		t.Errorf("header with no version should show the bare wordmark; got %q", got)
	}

	m.version = "0.1.4"
	header := m.renderHeader()
	// Order matters: the version sits AFTER the wordmark, not before it.
	plain := stripANSI(header)
	if !strings.Contains(plain, "tailport v0.1.4") {
		t.Errorf("header should read 'tailport v0.1.4'; got %q", plain)
	}
	// "use a different color" is the ticket's actual ask, so assert it on the
	// style definitions rather than on rendered output: under `go test` there's
	// no TTY, lipgloss degrades to the Ascii profile, and EVERY style renders
	// as bare text -- a rendered-string comparison would pass no matter what
	// color versionStyle carried. theme_test.go reads GetForeground() for the
	// same reason.
	logoFg, ok := logoStyle.GetForeground().(lipgloss.AdaptiveColor)
	if !ok {
		t.Fatalf("logoStyle foreground is %T, want lipgloss.AdaptiveColor", logoStyle.GetForeground())
	}
	verFg, ok := versionStyle.GetForeground().(lipgloss.AdaptiveColor)
	if !ok {
		t.Fatalf("versionStyle foreground is %T, want lipgloss.AdaptiveColor", versionStyle.GetForeground())
	}
	if verFg == logoFg {
		t.Errorf("versionStyle must differ from logoStyle; both are %+v", verFg)
	}

	// The toggle still right-aligns on the same row, and the version didn't
	// push the header onto a second line at a normal width.
	if lipgloss.Height(header) != 1 {
		t.Errorf("header should stay one row at width 80; got %d rows: %q", lipgloss.Height(header), header)
	}
	if !strings.Contains(header, "All ports") {
		t.Errorf("header should still carry the view toggle; got %q", header)
	}
}

// TestHeaderSpacer covers 0qy8's "not crowded" half: View puts a blank row
// between the header and the body, and listBodyHeight RESERVES that row --
// if the two ever disagree the grid sizes one row too tall and pushes the
// bottom bar off the viewport.
func TestHeaderSpacer(t *testing.T) {
	m := New(config.Config{Ports: map[int]config.PortMeta{}})
	m.width = 80
	m.height = 24

	lines := strings.Split(m.View(), "\n")
	if len(lines) < 2 {
		t.Fatalf("View should render multiple lines; got %q", m.View())
	}
	if strings.TrimSpace(stripANSI(lines[1])) != "" {
		t.Errorf("line 2 of View should be the blank spacer under the header; got %q", stripANSI(lines[1]))
	}

	// The whole view still fits the viewport -- the spacer is reserved, not
	// bolted on top of a body already sized to fill the height.
	if got := lipgloss.Height(m.View()); got > m.height {
		t.Errorf("View is %d rows, exceeds the %d-row viewport -- spacer not reserved in listBodyHeight", got, m.height)
	}
}

// TestEmptyStateMessage covers the contextual empty-view text: both views
// must lead with what tailport is and the commands that power it (ss/lsof,
// tailscale serve), then name the current view.
func TestEmptyStateMessage(t *testing.T) {
	m := New(config.Config{Ports: map[int]config.PortMeta{}})

	for _, all := range []bool{false, true} {
		m.showAllPorts = all
		msg := m.emptyStateMessage()
		// Leads with the tool + its underlying commands, in both views.
		for _, want := range []string{"tailport", "tailscale serve", "ss", "lsof"} {
			if !strings.Contains(msg, want) {
				t.Errorf("showAllPorts=%v: empty-state should mention %q; got %q", all, want, msg)
			}
		}
		// Then the view-specific hint.
		wantView := "Favorites"
		if all {
			wantView = "listening"
		}
		if !strings.Contains(msg, wantView) {
			t.Errorf("showAllPorts=%v: empty-state should mention %q; got %q", all, wantView, msg)
		}
	}

	// 2fgk: the Favorites empty state drops the redundant green heading, so it
	// now leads with the intro line, not a "Favorites" heading. The symmetric
	// All ports heading is kept.
	m.showAllPorts = false
	if favFirst := strings.SplitN(m.emptyStateMessage(), "\n", 2)[0]; strings.Contains(favFirst, "Favorites") {
		t.Errorf("Favorites empty state should not lead with a 'Favorites' heading; first line = %q", favFirst)
	} else if !strings.Contains(favFirst, "tailport") {
		t.Errorf("Favorites empty state should lead with the intro; first line = %q", favFirst)
	}
	m.showAllPorts = true
	if allFirst := strings.SplitN(m.emptyStateMessage(), "\n", 2)[0]; !strings.Contains(allFirst, "All ports") {
		t.Errorf("All ports empty state should keep its heading; first line = %q", allFirst)
	}
}

// TestHelpConfigPath covers gahj: the "?" help overlay states where settings
// live and shows the ACTUAL resolved config path, honoring XDG_CONFIG_HOME.
func TestHelpConfigPath(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	// New() captures the resolved path; it must reflect the XDG override.
	m := New(config.Config{Ports: map[int]config.PortMeta{}})
	want, err := config.Path("")
	if err != nil {
		t.Fatalf("config.Path() error: %v", err)
	}
	if m.configPath != want {
		t.Fatalf("model.configPath = %q, want %q", m.configPath, want)
	}
	if !strings.HasPrefix(want, xdg) {
		t.Fatalf("resolved path %q should sit under XDG_CONFIG_HOME %q", want, xdg)
	}

	// The overlay names where settings save and shows that exact path. Assert
	// on helpContent (the full, unclipped overlay text) rather than helpView,
	// which windows the content to the terminal height (v10j).
	view := stripANSI(m.helpContent())
	if !strings.Contains(view, "saved to") {
		t.Errorf("help overlay should state where settings are saved; got:\n%s", view)
	}
	if !strings.Contains(view, want) {
		t.Errorf("help overlay should show the resolved path %q; got:\n%s", want, view)
	}
}

// TestConfigSaveLines covers the display helper directly: the default rule
// (unset XDG) ends at .config/tailport/config.yaml, an explicit path is shown
// verbatim, $HOME is abbreviated to ~, and an empty path falls back to the rule.
func TestConfigSaveLines(t *testing.T) {
	// Default rule: with XDG unset the resolved path ends at the ~/.config leaf.
	t.Setenv("XDG_CONFIG_HOME", "")
	def, err := config.Path("")
	if err != nil {
		t.Fatalf("config.Path() error: %v", err)
	}
	if !strings.HasSuffix(def, ".config/tailport/config.yaml") {
		t.Errorf("default config path = %q, want it to end with .config/tailport/config.yaml", def)
	}

	// An explicit absolute path is shown as-is (joined into one of the lines).
	lines := configSaveLines("/etc/xdg/tailport/config.yaml")
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "/etc/xdg/tailport/config.yaml") {
		t.Errorf("configSaveLines should show the literal path; got %q", joined)
	}

	// Empty path -> fall back to describing the rule, never nothing.
	fb := strings.Join(configSaveLines(""), "\n")
	if !strings.Contains(fb, "XDG_CONFIG_HOME") || !strings.Contains(fb, ".config/tailport/config.yaml") {
		t.Errorf("configSaveLines(\"\") should describe the rule; got %q", fb)
	}
}

// TestMarkerGlyph covers 1exs: the exposure-state marker resolves to the
// moon-phase reach ramp in emoji mode and the styled ASCII fallback
// otherwise, one case per reachState (79xb), with the SAME field
// combinations TestReachStateDescriptions/TestFunnelItemRender use to reach
// each state -- so a glyph and its state can never quietly drift apart.
// Every emoji case pads to a stable 2-cell column, including the naturally
// 1-cell ✕ (reachOffline) and the VS16-bearing 🌫️ (reachStale).
func TestMarkerGlyph(t *testing.T) {
	cases := []struct {
		name                 string
		item                 portItem
		wantState            reachState
		wantEmoji, wantASCII string
	}{
		{
			name:      "A reachLocalhost",
			item:      portItem{port: portscan.Port{Number: 3000, BindScope: portscan.ScopeLoopback}, listening: true},
			wantState: reachLocalhost,
			wantEmoji: "🌕", wantASCII: "○",
		},
		{
			name:      "B' reachLAN",
			item:      portItem{port: portscan.Port{Number: 3000, BindScope: portscan.ScopeLAN}, listening: true},
			wantState: reachLAN,
			wantEmoji: "🌔", wantASCII: "◔",
		},
		{
			name:      "B reachTailnet",
			item:      portItem{port: portscan.Port{Number: 8080, BindScope: portscan.ScopeWildcard}, listening: true},
			wantState: reachTailnet,
			wantEmoji: "🌒", wantASCII: "◉",
		},
		{
			name:      "C reachServed",
			item:      portItem{port: portscan.Port{Number: 8080}, active: true, listening: true},
			wantState: reachServed,
			wantEmoji: "🌒", wantASCII: "◉",
		},
		{
			name:      "D reachFunnel",
			item:      portItem{port: portscan.Port{Number: 8080}, active: true, listening: true, funnelPublic: 443},
			wantState: reachFunnel,
			wantEmoji: "🌑", wantASCII: "●",
		},
		{
			name:      "D reachFunnel outranks a dangling forward",
			item:      portItem{port: portscan.Port{Number: 8080}, active: true, listening: false, funnelPublic: 443},
			wantState: reachFunnel,
			wantEmoji: "🌑", wantASCII: "●",
		},
		{
			name:      "E reachStale",
			item:      portItem{port: portscan.Port{Number: 8025}, active: true, listening: false},
			wantState: reachStale,
			wantEmoji: "🌫️", wantASCII: "▲",
		},
		{
			name:      "F reachOffline",
			item:      portItem{port: portscan.Port{Number: 8025}, active: false, listening: false, meta: config.PortMeta{Favorite: true, LastProcess: "mailpit"}},
			wantState: reachOffline,
			wantEmoji: "✕", wantASCII: "✕",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.item.reach(); got != c.wantState {
				t.Fatalf("reach() = %v, want %v (fix the test fixture, not the glyph mapping)", got, c.wantState)
			}

			em := c.item
			em.emoji = true
			got := em.markerGlyph()
			if !strings.Contains(got, c.wantEmoji) {
				t.Errorf("emoji marker = %q, want to contain %q", got, c.wantEmoji)
			}
			if lipgloss.Width(got) < 2 {
				t.Errorf("emoji marker %q should pad to a 2-cell column, width=%d", got, lipgloss.Width(got))
			}

			as := c.item
			as.emoji = false
			if got := stripANSI(as.markerGlyph()); got != c.wantASCII {
				t.Errorf("ascii marker = %q, want %q", got, c.wantASCII)
			}
		})
	}
}

// TestFilterNoHighlight covers ykxh: the custom filter ranks exactly like the
// default (so filtering still works) but returns no matched indices, so the
// delegate's ANSI-unaware highlighter never mangles our styled titles.
func TestFilterNoHighlight(t *testing.T) {
	targets := []string{"8025  was mailpit", "3000  node", "8808  agentsview"}
	def := list.DefaultFilter("mail", targets)
	got := filterNoHighlight("mail", targets)

	if len(got) != len(def) {
		t.Fatalf("ranking differs: got %d ranks, default %d", len(got), len(def))
	}
	for i := range got {
		if got[i].Index != def[i].Index {
			t.Errorf("rank %d index = %d, want %d (ranking must match DefaultFilter)", i, got[i].Index, def[i].Index)
		}
		if got[i].MatchedIndexes != nil {
			t.Errorf("rank %d should carry no matched indices, got %v", i, got[i].MatchedIndexes)
		}
	}
	// Sanity: "mail" still selects the mailpit row.
	if len(got) == 0 || got[0].Index != 0 {
		t.Errorf("'mail' should rank the mailpit row first; got %+v", got)
	}
}

// TestFilterValue covers e518: the filter string includes the port number,
// live process, user label, AND the remembered process -- so a down favorite
// showing "was mailpit" is still matched by filtering "mail".
func TestFilterValue(t *testing.T) {
	// A down favorite: no live process, no label, but LastProcess remembered.
	down := portItem{
		port: portscan.Port{Number: 8025},
		meta: config.PortMeta{Favorite: true, LastProcess: "mailpit"},
	}
	fv := down.FilterValue()
	if !strings.Contains(fv, "mailpit") {
		t.Errorf("FilterValue %q should include the remembered process for filtering", fv)
	}
	if !strings.Contains(fv, "8025") {
		t.Errorf("FilterValue %q should include the port number", fv)
	}

	// Live + labelled: process and label both filterable.
	live := portItem{
		port: portscan.Port{Number: 3000, Process: "node"},
		meta: config.PortMeta{Label: "dev server"},
	}
	fv = live.FilterValue()
	for _, want := range []string{"3000", "node", "dev server"} {
		if !strings.Contains(fv, want) {
			t.Errorf("FilterValue %q should include %q", fv, want)
		}
	}
}

// TestDanglingDescription covers km8x (as retargeted by 79xb): a
// served-but-not-listening row explains itself -- names the stale state and
// the unbind key -- while a healthy served row and an offline row keep their
// plain descriptions.
func TestDanglingDescription(t *testing.T) {
	dangling := portItem{port: portscan.Port{Number: 8025}, active: true, listening: false, host: "host"}
	got := stripANSI(dangling.Description())
	// Names why it looks served-yet-empty (tailscale still holds the port) and
	// the key to unbind it. The loopback fix lives in ? help / README.
	for _, want := range []string{"bound to tailnet", "stale", "space", "unbind"} {
		if !strings.Contains(got, want) {
			t.Errorf("dangling description %q should mention %q", got, want)
		}
	}

	// Healthy serve: the tailnet URL, no scary hint.
	healthy := portItem{port: portscan.Port{Number: 8025}, active: true, listening: true, host: "host"}
	if got := stripANSI(healthy.Description()); got != "on tailnet · http://host:8025" {
		t.Errorf("healthy description = %q, want the tailnet URL", got)
	}

	// Offline: not served, not listening -- distinct from the reachable states.
	idle := portItem{port: portscan.Port{Number: 8025}}
	if got := idle.Description(); got != "offline" {
		t.Errorf("idle description = %q, want %q", got, "offline")
	}
}

// TestReachStateDescriptions covers 79xb pt2's honest 7-state lexicon end to
// end: reach() resolves the right state from a portItem's fields, and
// Description() renders the exact row text for each, per the truth table in
// the issue. D (funnel) is covered separately by TestFunnelItemRender since
// it needs fqdn/PublicURL wiring.
func TestReachStateDescriptions(t *testing.T) {
	cases := []struct {
		name  string
		item  portItem
		state reachState
		desc  string
	}{
		{
			name:  "A loopback unserved -> localhost only",
			item:  portItem{port: portscan.Port{Number: 3000, BindScope: portscan.ScopeLoopback}, listening: true, host: "host"},
			state: reachLocalhost,
			desc:  "localhost only",
		},
		{
			name:  "A unknown bind scope also reads localhost only (conservative default)",
			item:  portItem{port: portscan.Port{Number: 3000}, listening: true, host: "host"},
			state: reachLocalhost,
			desc:  "localhost only",
		},
		{
			name:  "B wildcard unserved -> on tailnet",
			item:  portItem{port: portscan.Port{Number: 8080, BindScope: portscan.ScopeWildcard}, listening: true, host: "host"},
			state: reachTailnet,
			desc:  "on tailnet · http://host:8080",
		},
		{
			name:  "B :22 on a wildcard bind -> on tailnet, reachable via SSH",
			item:  portItem{port: portscan.Port{Number: 22, BindScope: portscan.ScopeWildcard}, listening: true, host: "host"},
			state: reachTailnet,
			desc:  "on tailnet · reachable via SSH",
		},
		{
			name:  "B' LAN-bound unserved -> local network only",
			item:  portItem{port: portscan.Port{Number: 3000, BindScope: portscan.ScopeLAN}, listening: true, host: "host"},
			state: reachLAN,
			desc:  "local network only",
		},
		{
			name:  "C served and listening -> on tailnet URL",
			item:  portItem{port: portscan.Port{Number: 8080}, active: true, listening: true, host: "host"},
			state: reachServed,
			desc:  "on tailnet · http://host:8080",
		},
		{
			name:  "E served but nothing listening -> stale",
			item:  portItem{port: portscan.Port{Number: 8025}, active: true, listening: false, host: "host"},
			state: reachStale,
			desc:  "bound to tailnet, but stale — space to unbind",
		},
		{
			name:  "F down favorite -> offline",
			item:  portItem{port: portscan.Port{Number: 8025}, active: false, listening: false, meta: config.PortMeta{Favorite: true, LastProcess: "mailpit"}},
			state: reachOffline,
			desc:  "offline",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.item.reach(); got != c.state {
				t.Errorf("reach() = %v, want %v", got, c.state)
			}
			if got := stripANSI(c.item.Description()); got != c.desc {
				t.Errorf("Description() = %q, want %q", got, c.desc)
			}
		})
	}
}

// TestBindPrefix covers qptn: the netstat-style host prefix shown left of
// ":PORT" on the row title is now the ONLY channel that distinguishes a
// bound-wide-on-tailnet port from a served one, since both share the green
// ◉ glyph and an identical description (Change 1/2). Wildcard -> "*", a
// specific LAN bind -> its bare host, everything else (loopback, served,
// unclassified) -> "" (quiet).
func TestBindPrefix(t *testing.T) {
	cases := []struct {
		name  string
		scope portscan.BindScope
		host  string
		want  string
	}{
		{"wildcard -> *", portscan.ScopeWildcard, "0.0.0.0", "*"},
		{"LAN -> bare host", portscan.ScopeLAN, "192.168.1.5", "192.168.1.5"},
		{"loopback -> quiet", portscan.ScopeLoopback, "127.0.0.1", ""},
		{"unknown/unclassified -> quiet", portscan.ScopeUnknown, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			it := portItem{port: portscan.Port{Number: 3000, BindScope: c.scope, BindHost: c.host}}
			if got := it.bindPrefix(); got != c.want {
				t.Errorf("bindPrefix() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestTitleBindPrefix covers the other half of qptn: Title() actually
// renders bindPrefix() left of ":PORT" -- "*:3000" for a wildcard-bound
// (bound-wide) port, ":3000" (no host, unchanged) for a loopback/served
// port, and "192.168.1.5:3000" for a LAN-bound port. A wildcard FAVORITE row
// must show BOTH the "*" prefix and the "★" favorite marker without
// collision, since the star sits to the right of the port, not the left.
func TestTitleBindPrefix(t *testing.T) {
	t.Run("wildcard bound-wide shows * prefix", func(t *testing.T) {
		it := portItem{port: portscan.Port{Number: 3000, Process: "node", BindScope: portscan.ScopeWildcard}, listening: true}
		got := stripANSI(it.Title())
		if !strings.Contains(got, "*:3000") {
			t.Errorf("Title() = %q, want to contain %q", got, "*:3000")
		}
	})

	t.Run("loopback/served shows no host prefix", func(t *testing.T) {
		it := portItem{port: portscan.Port{Number: 3000, Process: "node", BindScope: portscan.ScopeLoopback}, listening: true}
		got := stripANSI(it.Title())
		if !strings.Contains(got, " :3000") {
			t.Errorf("Title() = %q, want to contain %q", got, " :3000")
		}
		if strings.Contains(got, "*:3000") {
			t.Errorf("Title() = %q, should not contain the wildcard prefix", got)
		}
	})

	t.Run("LAN bind shows the bare LAN IP prefix", func(t *testing.T) {
		it := portItem{port: portscan.Port{Number: 3000, Process: "node", BindScope: portscan.ScopeLAN, BindHost: "192.168.1.5"}, listening: true}
		got := stripANSI(it.Title())
		if !strings.Contains(got, "192.168.1.5:3000") {
			t.Errorf("Title() = %q, want to contain %q", got, "192.168.1.5:3000")
		}
	})

	t.Run("wildcard favorite shows both * prefix and star without collision", func(t *testing.T) {
		it := portItem{
			port:      portscan.Port{Number: 3000, Process: "node", BindScope: portscan.ScopeWildcard},
			listening: true,
			meta:      config.PortMeta{Favorite: true},
		}
		got := stripANSI(it.Title())
		if !strings.Contains(got, "*:3000") {
			t.Errorf("Title() = %q, want to contain %q", got, "*:3000")
		}
		if !strings.Contains(got, "★") {
			t.Errorf("Title() = %q, want to contain the favorite star %q", got, "★")
		}
	})
}

// TestWasName covers znrg: the Title name precedence -- label > live process >
// remembered "was <name>" (italic) > "?".
func TestWasName(t *testing.T) {
	cases := []struct {
		name      string
		label     string
		process   string // live process (listening) when non-empty
		listening bool
		last      string // meta.LastProcess
		wantSub   string
		wantNoSub string
	}{
		{"label wins", "My Mail", "mailpit", true, "mailpit", "My Mail", "was"},
		{"live process", "", "mailpit", true, "postfix", "mailpit", "was"},
		{"down remembers", "", "", false, "mailpit", "was mailpit", ""},
		{"down, nothing known", "", "", false, "", "?", "was"},
		{"label beats remembered when down", "My Mail", "", false, "mailpit", "My Mail", "was"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			it := portItem{
				port:      portscan.Port{Number: 8025, Process: c.process},
				listening: c.listening,
				meta:      config.PortMeta{Label: c.label, Favorite: true, LastProcess: c.last},
			}
			got := stripANSI(it.Title())
			if !strings.Contains(got, c.wantSub) {
				t.Errorf("Title = %q, want to contain %q", got, c.wantSub)
			}
			if c.wantNoSub != "" && strings.Contains(got, c.wantNoSub) {
				t.Errorf("Title = %q, should not contain %q", got, c.wantNoSub)
			}
		})
	}
}

// TestRememberProcesses covers the capture side: favorite listening ports get
// their process recorded (returning changed), non-favorite / non-listening
// ports are left alone, and a steady state reports no change.
func TestRememberProcesses(t *testing.T) {
	m := New(config.Config{Ports: map[int]config.PortMeta{
		8025: {Favorite: true}, // favorite, will be captured
		3000: {},               // registered but not favorite -> skip
	}})
	m.allPorts = []portscan.Port{
		{Number: 8025, Process: "mailpit"},
		{Number: 3000, Process: "node"},
		{Number: 9999, Process: "stray"}, // not registered -> skip
	}

	if !m.rememberProcesses() {
		t.Fatal("rememberProcesses should report a change on first capture")
	}
	if got := m.cfg.Ports[8025].LastProcess; got != "mailpit" {
		t.Errorf("favorite :8025 LastProcess = %q, want mailpit", got)
	}
	if got := m.cfg.Ports[3000].LastProcess; got != "" {
		t.Errorf("non-favorite :3000 LastProcess = %q, want empty (not remembered)", got)
	}
	if _, ok := m.cfg.Ports[9999]; ok {
		t.Errorf("unregistered :9999 should not gain a registry entry")
	}
	// Steady state: same ports, nothing new -> no change, no re-write.
	if m.rememberProcesses() {
		t.Error("rememberProcesses should report no change when nothing moved")
	}
}

// TestResolveMarkerEmoji covers the EXPOSURE-marker mode resolution (qwcw):
// emoji/ascii force it, "auto" is an explicit opt-in to the terminal
// heuristic (UTF-8 locale + sane TERM), and -- the new behavior split off
// from the old resolveEmoji -- an empty/unset mode is MONO regardless of
// terminal capability, no longer identical to "auto".
func TestResolveMarkerEmoji(t *testing.T) {
	if !resolveMarkerEmoji("emoji") {
		t.Error("markers=emoji should force emoji")
	}
	if resolveMarkerEmoji("ascii") {
		t.Error("markers=ascii should force ascii")
	}

	// auto: a UTF-8 locale on a normal terminal -> emoji.
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_CTYPE", "")
	t.Setenv("LANG", "en_US.UTF-8")
	if !resolveMarkerEmoji("auto") {
		t.Error("auto with UTF-8 LANG on xterm should resolve emoji")
	}
	// Empty (unset) is the new mono default -- deliberately NOT the same as
	// "auto" anymore, even though this terminal is UTF-8-capable.
	if resolveMarkerEmoji("") {
		t.Error("empty (unset) should resolve mono regardless of terminal capability")
	}
	// Case-insensitive/trimmed, same as the other modes.
	if resolveMarkerEmoji("  ") {
		t.Error("whitespace-only should resolve mono, same as empty")
	}
	if resolveMarkerEmoji("bogus") {
		t.Error("an unrecognized mode should resolve mono, same as empty")
	}

	// The bare Linux console can't render emoji, even with a UTF-8 locale.
	t.Setenv("TERM", "linux")
	if resolveMarkerEmoji("auto") {
		t.Error("auto on the linux console should resolve ascii")
	}

	// A non-UTF-8 locale -> ascii.
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("LANG", "C")
	if resolveMarkerEmoji("auto") {
		t.Error("auto with a non-UTF-8 locale should resolve ascii")
	}
}

// TestNewMarkersOverride covers zn2x's precedence and persistence contract
// at the New()/model level (the CLI-flag validation itself is covered in
// cmd/tailport): the override passed to New wins for EXPOSURE-marker
// rendering (m.markerEmoji, split from the egg's m.emoji by qwcw), but must
// never leak into cfg.Markers -- and therefore never into what a later,
// unrelated Save() (e.g. from favoriting a port) writes to disk.
func TestNewMarkersOverride(t *testing.T) {
	// Override wins over the persisted config value.
	m := New(config.Config{Ports: map[int]config.PortMeta{}, Markers: "emoji"}, "ascii")
	if m.markerEmoji {
		t.Error("New(cfg{Markers:emoji}, \"ascii\") should resolve ascii (flag beats config)")
	}
	if m.cfg.Markers != "emoji" {
		t.Errorf("New should not mutate cfg.Markers: got %q, want the original %q", m.cfg.Markers, "emoji")
	}

	// No override (variadic omitted) falls back to cfg.Markers, exactly as
	// before zn2x -- the common existing-call-site case stays unaffected.
	m2 := New(config.Config{Ports: map[int]config.PortMeta{}, Markers: "emoji"})
	if !m2.markerEmoji {
		t.Error("New(cfg{Markers:emoji}) with no override should still resolve emoji")
	}

	// An empty override (as when --markers wasn't passed at all) must not
	// clobber a real config value either.
	m3 := New(config.Config{Ports: map[int]config.PortMeta{}, Markers: "emoji"}, "")
	if !m3.markerEmoji {
		t.Error("New(cfg{Markers:emoji}, \"\") should still resolve emoji (empty override = no override)")
	}
}

// TestNewEggEmojiDecoupledFromMarkers covers qwcw's central split: the egg/
// fireworks glyph choice (m.emoji) always tracks emojiCapable() alone, no
// matter what --markers/cfg.Markers says, while the exposure markers
// (m.markerEmoji) obey markersMode and default to mono when it's unset.
func TestNewEggEmojiDecoupledFromMarkers(t *testing.T) {
	want := emojiCapable() // whatever this test process's env resolves to

	for _, tc := range []struct {
		markersMode     string
		wantMarkerEmoji bool
	}{
		{"", false},      // unset -> mono (new default)
		{"ascii", false}, // forced mono
		{"emoji", true},  // forced emoji
		{"auto", emojiCapable()},
	} {
		m := New(config.Config{Ports: map[int]config.PortMeta{}}, tc.markersMode)
		if m.emoji != want {
			t.Errorf("New(cfg, %q).emoji = %v, want emojiCapable() = %v -- egg/fireworks must stay decoupled from --markers", tc.markersMode, m.emoji, want)
		}
		if m.markerEmoji != tc.wantMarkerEmoji {
			t.Errorf("New(cfg, %q).markerEmoji = %v, want %v", tc.markersMode, m.markerEmoji, tc.wantMarkerEmoji)
		}
	}
}

// TestMarkersOverrideNeverPersisted is the end-to-end regression for the bug
// this scaffolding guards against: launching with a --markers override, then
// triggering an unrelated Save() (favoriting a port, exactly as "n" does),
// must NOT write the override into the on-disk config's markers field.
// Caught during manual verification of zn2x: an earlier implementation
// mutated cfg.Markers directly before calling New, which looked correct in
// isolation but leaked into every subsequent save for the rest of the
// session.
func TestMarkersOverrideNeverPersisted(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// Config on disk has no markers preference set (the common case).
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Markers != "" {
		t.Fatalf("precondition: expected a fresh config with no Markers set, got %q", cfg.Markers)
	}

	m := New(cfg, "ascii")
	if m.markerEmoji {
		t.Fatal("--markers ascii override should resolve ascii for this session")
	}

	// An unrelated mutation (favoriting a port, as the "n" key does) saves
	// m.cfg to disk.
	if cmd := m.favorite(4242); cmd != nil {
		cmd() // saveConfig's returned cmd is only non-nil on a Save() error
	}

	reloaded, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Markers != "" {
		t.Errorf("the run-only --markers override leaked into the persisted config: Markers = %q, want empty", reloaded.Markers)
	}
	if !reloaded.Ports[4242].Favorite {
		t.Fatal("sanity check: the favorite mutation itself should have persisted")
	}
}

// --- Fireworks (5x1e) --------------------------------------------------------

// TestBellRange covers the bell-curve sampler that every firework characteristic
// draws from: values stay strictly within bounds (no NormFloat64 tail to clamp)
// and cluster toward the midpoint ("most values central").
func TestBellRange(t *testing.T) {
	for i := 0; i < 200000; i++ {
		if u := bellUnit(); u < -1 || u > 1 {
			t.Fatalf("bellUnit %v escaped [-1,1]", u)
		}
		if v := bellRange(-3, 7); v < -3 || v > 7 {
			t.Fatalf("bellRange %v escaped [-3,7]", v)
		}
	}
	// Central bias: the mean sits near the midpoint of the range.
	const n = 300000
	var sum float64
	for i := 0; i < n; i++ {
		sum += bellRange(-3, 7)
	}
	if mean := sum / n; math.Abs(mean-2) > 0.1 {
		t.Errorf("bellRange mean %.3f should cluster near the midpoint 2", mean)
	}
}

// TestEggLayout covers the shared geometry source of truth: the tiny-terminal
// gate, the clamps, and that the FLOATING fanfare rows are recorded correctly
// against the vertically-centred block.
func TestEggLayout(t *testing.T) {
	if eggLayout(40, 40).ok {
		t.Error("w<52 must be the not-ok fallback")
	}
	if eggLayout(80, 16).ok {
		t.Error("h<17 must be the not-ok fallback")
	}
	l := eggLayout(100, 40)
	if !l.ok {
		t.Fatal("100x40 should be ok")
	}
	if l.sw != 96 {
		t.Errorf("sw=%d want 96 (w-4)", l.sw)
	}
	if l.eggRows != 15 {
		t.Errorf("eggRows=%d want 15 (clamped)", l.eggRows)
	}
	// The block is vertically centred: the top fanfare sits exactly at topPad.
	if l.topFanfareRow != l.topPad {
		t.Errorf("top fanfare row %d should equal topPad %d", l.topFanfareRow, l.topPad)
	}
	// Bottom fanfare frames the egg: it's eggRows+1 rows below the top one.
	if gap := l.botFanfareRow - l.topFanfareRow; gap != l.eggRows+1 {
		t.Errorf("fanfare gap %d want eggRows+1=%d", gap, l.eggRows+1)
	}
	// The whole block fits inside the viewport.
	if l.topPad+l.blockH > 40 {
		t.Errorf("block bottom %d exceeds height 40", l.topPad+l.blockH)
	}
	// A taller terminal floats the fanfare further down (rows are NOT fixed).
	if tall := eggLayout(100, 60); tall.topFanfareRow <= l.topFanfareRow {
		t.Errorf("taller viewport should float the fanfare lower: 60->%d vs 40->%d",
			tall.topFanfareRow, l.topFanfareRow)
	}
}

// TestNewFireworkGeometry covers the launch/burst sampling: launch is
// centre-bottom within +/-8% of width, and every explosion lands inside the
// band (viewport top down to a few rows below the floating bottom fanfare).
func TestNewFireworkGeometry(t *testing.T) {
	const w, h = 100, 40
	lay := eggLayout(w, h)
	bandTop := float64(fwTopMargin)
	bandBot := float64(lay.botFanfareRow + 3)
	maxOff := fwLaunchSpreadPct * float64(w)
	const eps = 1e-6
	for i := 0; i < 20000; i++ {
		fw := newFirework(w, h, i%2 == 0)
		if fw.y0 != float64(h-1) {
			t.Fatalf("launch row %.1f want center-bottom %d", fw.y0, h-1)
		}
		if fw.x0 < float64(w)/2-maxOff-eps || fw.x0 > float64(w)/2+maxOff+eps {
			t.Fatalf("launch col %.3f outside +/-8%% of centre", fw.x0)
		}
		if fw.yExp < bandTop-eps || fw.yExp > bandBot+eps {
			t.Fatalf("explosion row %.3f outside band [%.0f,%.0f]", fw.yExp, bandTop, bandBot)
		}
		if fw.count < 1 || fw.v0 <= 0 || fw.tExp <= 0 {
			t.Fatalf("degenerate firework: count=%d v0=%.3f tExp=%.3f", fw.count, fw.v0, fw.tExp)
		}
	}
}

// TestFireworkBurstReachesTopButClusters covers part 2: the raised ceiling lets
// a burst reach near the viewport top (row ~1), but the bell distribution is
// UNCHANGED -- most bursts still cluster centrally near the egg and hitting the
// very top is the rare exception (NOT a flatten toward uniform/top).
func TestFireworkBurstReachesTopButClusters(t *testing.T) {
	const w, h = 100, 40
	lay := eggLayout(w, h)
	const n = 30000
	var sum, minY float64
	minY = math.Inf(1)
	topHits := 0 // bursts up in the rare very-top region (rows <= 3)
	for i := 0; i < n; i++ {
		fw := newFirework(w, h, i%2 == 0)
		sum += fw.yExp
		if fw.yExp < minY {
			minY = fw.yExp
		}
		if fw.yExp <= 3 {
			topHits++
		}
	}
	// CAN reach near the top: the ceiling is fwTopMargin(=1), so some shot lands
	// well above the top fanfare (row lay.topFanfareRow).
	if minY > 3 {
		t.Errorf("no burst reached near the top (min yExp %.2f); ceiling should permit row ~%d", minY, fwTopMargin)
	}
	if minY >= float64(lay.topFanfareRow) {
		t.Errorf("min burst row %.2f never cleared the top fanfare row %d", minY, lay.topFanfareRow)
	}
	// STILL clusters centrally: mean near the band midpoint, nowhere near the top.
	mean := sum / n
	mid := (float64(fwTopMargin) + float64(lay.botFanfareRow+3)) / 2
	if math.Abs(mean-mid) > 2.5 {
		t.Errorf("burst rows should cluster near band midpoint %.1f, got mean %.2f (flattened?)", mid, mean)
	}
	if mean <= float64(lay.topFanfareRow) {
		t.Errorf("mean burst row %.2f sits at/above the fanfare %d -- distribution flattened toward the top", mean, lay.topFanfareRow)
	}
	// Very-top bursts are the RARE exception, not the norm.
	if frac := float64(topHits) / n; frac > 0.15 {
		t.Errorf("too many very-top bursts (%.1f%%); tall shots should be the rare exception, not uniform", frac*100)
	}
}

// TestFireworkBurstStaysOnScreen covers part 3's clamp: with the aggressive
// +/-0.9 lean, the predicted burst centre xExp=posX(tExp) must stay a couple
// cells inside the viewport on a NARROW terminal (where long-tExp top shots
// would otherwise drift off-screen and burst half-clipped) -- while a WIDE
// terminal still lets most shots keep the full lean (clamp is a safety net,
// not a general flattening).
func TestFireworkBurstStaysOnScreen(t *testing.T) {
	const margin = 2.0
	const eps = 1e-6
	// Narrow width where the clamp actually binds: the invariant must hold for
	// EVERY sample, including the tallest (largest tExp) shots.
	for _, w := range []int{52, 60} {
		const h = 40
		clamped := false
		for i := 0; i < 20000; i++ {
			fw := newFirework(w, h, i%2 == 0)
			xExp := fw.posX(fw.tExp)
			if xExp < margin-eps || xExp > float64(w-1)-margin+eps {
				t.Fatalf("w=%d: burst centre xExp=%.3f left the viewport [%.1f,%.1f] (half-clipped)",
					w, xExp, margin, float64(w-1)-margin)
			}
			// Did the clamp bite? An unclamped |vx| could be up to 0.9; if the
			// realised drift sits hard against the on-screen edge, it was clamped.
			if fw.tExp > 0 {
				hi := (float64(w-1) - margin - fw.x0) / fw.tExp
				lo := (margin - fw.x0) / fw.tExp
				if math.Abs(fw.vx-hi) < 1e-9 || math.Abs(fw.vx-lo) < 1e-9 {
					clamped = true
				}
			}
		}
		if !clamped {
			t.Errorf("w=%d: expected the on-screen clamp to bind on at least one narrow-width shot", w)
		}
	}
	// Wide terminal: the clamp should NOT generally flatten the lean -- plenty of
	// shots keep a strong horizontal sweep (|vx| well past the old +/-0.3).
	strong := 0
	for i := 0; i < 20000; i++ {
		fw := newFirework(240, 40, i%2 == 0)
		if math.Abs(fw.vx) > 0.5 {
			strong++
		}
	}
	if strong == 0 {
		t.Error("wide terminal: no strong-lean shots survived; clamp is over-flattening the arcs")
	}
}

// TestFireworkBiggerBursts covers part 4: the widened INDEPENDENT ranges let a
// burst be bigger (more particles, wider radius, longer ember life) than the old
// caps allowed, while the small mins still exist (dim pops remain possible).
func TestFireworkBiggerBursts(t *testing.T) {
	const oldCountMax, oldRadiusMax = 34, 1.25
	maxCount, minCount := 0, 1<<30
	maxRadius, minRadius := 0.0, math.Inf(1)
	maxTTL := 0
	for i := 0; i < 30000; i++ {
		fw := newFirework(120, 45, i%2 == 0)
		if fw.count > maxCount {
			maxCount = fw.count
		}
		if fw.count < minCount {
			minCount = fw.count
		}
		if fw.radius > maxRadius {
			maxRadius = fw.radius
		}
		if fw.radius < minRadius {
			minRadius = fw.radius
		}
		fw.explode()
		for _, p := range fw.particles {
			if p.ttl > maxTTL {
				maxTTL = p.ttl
			}
		}
	}
	if maxCount <= oldCountMax {
		t.Errorf("bursts no bigger: max count %d should exceed the old cap %d", maxCount, oldCountMax)
	}
	if maxRadius <= oldRadiusMax {
		t.Errorf("bursts no wider: max radius %.2f should exceed the old cap %.2f", maxRadius, oldRadiusMax)
	}
	if maxTTL <= 18 {
		t.Errorf("embers no longer-lived: max ttl %d should exceed the old cap 18", maxTTL)
	}
	// Small dim pops still exist (mins unchanged).
	if minCount > int(fwCountMin)+2 {
		t.Errorf("small bursts vanished: min count %d, want near fwCountMin %.0f", minCount, fwCountMin)
	}
	if minRadius > fwRadiusMin+0.15 {
		t.Errorf("tight bursts vanished: min radius %.2f, want near fwRadiusMin %.2f", minRadius, fwRadiusMin)
	}
}

// TestMuzzleSmoke covers part 5: newFirework seeds a puff that ages out, and --
// the core requirement -- overlapping puffs from simultaneous launches COMPOUND
// (density adds) rather than last-write-wins.
func TestMuzzleSmoke(t *testing.T) {
	// (a) Seeding + decay: a fresh shell carries smoke that fully expires.
	fw := newFirework(100, 40, true)
	if len(fw.smoke) == 0 {
		t.Fatal("newFirework should seed muzzle smoke")
	}
	for _, p := range fw.smoke {
		if p.ttl < fwSmokeLifeMin || p.ttl > fwSmokeLifeMax {
			t.Errorf("smoke ttl %d outside [%d,%d]", p.ttl, fwSmokeLifeMin, fwSmokeLifeMax)
		}
	}
	for i := 0; i <= fwSmokeLifeMax; i++ {
		fw.step()
	}
	if len(fw.smoke) != 0 {
		t.Errorf("smoke should fully expire after %d frames, still %d left", fwSmokeLifeMax, len(fw.smoke))
	}

	// (b) Compounding: two shells whose smoke shares a cell must sum to strictly
	// more density there than one -- and additively (not max/last-write-wins).
	proto := firework{smoke: []fwParticle{{x: 12, y: 20, ttl: 10, age: 2}}}
	one := smokeDensity([]firework{proto}, 40, 30)
	two := smokeDensity([]firework{proto, proto}, 40, 30)
	if one == nil || two == nil {
		t.Fatal("smokeDensity should return a buffer when smoke is present")
	}
	d1, d2 := one[20][12], two[20][12]
	if !(d2 > d1) {
		t.Errorf("compounding failed: two overlapping puffs (%.3f) must exceed one (%.3f)", d2, d1)
	}
	if math.Abs(d2-2*d1) > 1e-9 {
		t.Errorf("smoke must ADD, not last-write-wins: one=%.3f two=%.3f (want ~2x)", d1, d2)
	}

	// (c) Density -> heavier glyph/brighter gray as puffs pile up.
	light := smokeCell(0.3, true)
	heavy := smokeCell(2.4, true)
	if light.s == heavy.s {
		t.Errorf("smoke glyph should thicken with density: light=%q heavy=%q", light.s, heavy.s)
	}
	// ASCII path stays ASCII (no mojibake under non-emoji terminals).
	for _, c := range []styledCell{smokeCell(0.3, false), smokeCell(2.4, false)} {
		for _, r := range c.s {
			if r > 127 {
				t.Errorf("ascii smoke glyph %q is non-ASCII", c.s)
			}
		}
	}
}

// TestFireworkLifecycle covers stepping/expiry: a firework rises, bursts into
// particles, then all embers (and any flourish) expire and it reports done.
func TestFireworkLifecycle(t *testing.T) {
	fw := newFirework(100, 40, false)

	// The arch reaches its chosen band row exactly at the explosion frame.
	wantY := fw.yExp
	guard := 0
	for fw.stage == fwRising {
		fw.step()
		if guard++; guard > 5000 {
			t.Fatal("firework never exploded")
		}
	}
	if len(fw.particles) == 0 {
		t.Fatal("explosion should create particles")
	}
	if math.Abs(fw.yExp-wantY) > 1.0 {
		t.Errorf("burst row %.2f should match the chosen band row %.2f", fw.yExp, wantY)
	}
	if fw.done() {
		t.Fatal("a just-exploded firework is not done")
	}

	guard = 0
	for !fw.done() {
		fw.step()
		if guard++; guard > 5000 {
			t.Fatal("firework never finished")
		}
	}
	if len(fw.particles) != 0 {
		t.Errorf("a done firework should have no live particles, got %d", len(fw.particles))
	}
}

// TestStepFireworksReaps covers the pruning: spent fireworks are dropped,
// live ones survive.
func TestStepFireworksReaps(t *testing.T) {
	// Burst with no particles and no pending flourish -> reaped in one step.
	if out := stepFireworks([]firework{{stage: fwBurst}}); len(out) != 0 {
		t.Errorf("spent firework should be reaped, got %d", len(out))
	}
	// A fresh rising firework survives a step.
	if out := stepFireworks([]firework{newFirework(100, 40, true)}); len(out) != 1 {
		t.Errorf("rising firework should survive a step, got %d", len(out))
	}
	// A mixed batch drains to empty without panicking.
	batch := make([]firework, 0, 30)
	for i := 0; i < 30; i++ {
		batch = append(batch, newFirework(120, 45, i%2 == 0))
	}
	guard := 0
	for len(batch) > 0 {
		batch = stepFireworks(batch)
		if guard++; guard > 5000 {
			t.Fatal("batch never drained")
		}
	}
}

// TestFireworkGlyphs covers the ascii-vs-unicode glyph gating: ascii terminals
// get pure-ASCII sparks (no mojibake), emoji terminals get the Unicode set.
func TestFireworkGlyphs(t *testing.T) {
	ascii := firework{emoji: false}
	for _, r := range ascii.glyphSet() {
		if r > 127 {
			t.Errorf("ascii glyph set contains non-ASCII rune %q", r)
		}
	}
	for _, br := range []float64{-0.5, 0, 0.3, 0.6, 1, 1.5} {
		if g := ascii.glyph(br); g > 127 {
			t.Errorf("ascii glyph(%.2f)=%q is non-ASCII", br, g)
		}
	}
	uni := firework{emoji: true}
	nonASCII := false
	for _, r := range uni.glyphSet() {
		if r > 127 {
			nonASCII = true
		}
	}
	if !nonASCII {
		t.Error("unicode glyph set should contain non-ASCII sparks")
	}
}

// TestFireworkGlyphFloor covers glyphFloor (jkbp): the rising trail must floor
// at index 1 (░ / ascii ':') so a bare '·'/'.' never punches a whitespace hole
// through the egg text, while plain glyph() (floor 0, delegating to
// glyphFloor) keeps the old unfloored behavior for the burst.
func TestFireworkGlyphFloor(t *testing.T) {
	uni := firework{emoji: true}
	if g := uni.glyphFloor(0, 1); g != '░' {
		t.Errorf("uni.glyphFloor(0, 1) = %q, want '░' (floored, not the bare dot)", g)
	}
	if g := uni.glyphFloor(0, 0); g != '·' {
		t.Errorf("uni.glyphFloor(0, 0) = %q, want '·' (unfloored = old glyph behavior)", g)
	}
	if g := uni.glyphFloor(1, 1); g != '█' {
		t.Errorf("uni.glyphFloor(1, 1) = %q, want '█' (high brightness still tops out)", g)
	}

	ascii := firework{emoji: false}
	if g := ascii.glyphFloor(0, 1); g != ':' {
		t.Errorf("ascii.glyphFloor(0, 1) = %q, want ':' (floored, not the bare dot)", g)
	}

	for _, br := range []float64{0, 0.3, 0.6, 1} {
		if got, want := uni.glyph(br), uni.glyphFloor(br, 0); got != want {
			t.Errorf("uni.glyph(%.2f) = %q, want %q (glyph must delegate to glyphFloor(br, 0))", br, got, want)
		}
	}
}

// TestFireworkDraw covers compositing sparks into the grid, with edge clipping
// (no panic, no overflow) for out-of-bounds particles.
func TestFireworkDraw(t *testing.T) {
	grid := newCellGrid(40, 20)
	rising := firework{stage: fwRising, x0: 20, y0: 19, v0: 2, g: fwGravity, emoji: false}
	rising.draw(grid, 40, 20)
	if grid[19][20].s == " " || grid[19][20].s == "" {
		t.Errorf("a rising firework should plot a head at its launch cell, got %q", grid[19][20].s)
	}

	burst := firework{stage: fwBurst, emoji: true, scheme: 0, particles: []fwParticle{
		{x: 10, y: 10, ttl: 10},
		{x: -5, y: -5, ttl: 10},   // off top-left: must clip
		{x: 500, y: 500, ttl: 10}, // off bottom-right: must clip
	}}
	burst.draw(grid, 40, 20)
	if grid[10][10].s == " " {
		t.Error("a burst particle should plot into the grid")
	}
}

// TestFireworkCap covers the fwCap concurrency backstop: 'f' presses beyond
// the cap (while fwCap are live) are ignored, and no ticker is stacked. Press
// count is comfortably above fwCap (60) so the loop still actually saturates
// the cap regardless of its exact value (3e8b).
func TestFireworkCap(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := New(config.Config{})
	m.active = map[int]bool{}
	m.rebuildItems()
	m.width, m.height = 120, 45
	m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'E'}})

	firstCmd := true
	for i := 0; i < 100; i++ {
		res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
		m = res.(model)
		if !m.showEgg {
			t.Fatal("'f' must never close the egg (modality)")
		}
		if firstCmd {
			if cmd == nil {
				t.Error("the first 'f' should start the decoupled fireworks ticker")
			}
			firstCmd = false
		} else if cmd != nil {
			t.Error("later 'f' presses must not stack a second fireworks ticker")
		}
	}
	if len(m.fireworks) != fwCap {
		t.Errorf("cap: %d live fireworks, want %d (extra presses ignored)", len(m.fireworks), fwCap)
	}

	// Drain via the fireworks tick; it must stop cleanly (no reschedule, no leak).
	guard := 0
	for len(m.fireworks) > 0 {
		res, _ := m.Update(fwTickMsg{})
		m = res.(model)
		if guard++; guard > 5000 {
			t.Fatal("fireworks never drained under the tick")
		}
	}
	res, cmd := m.Update(fwTickMsg{})
	m = res.(model)
	if cmd != nil {
		t.Error("an idle fireworks tick must not reschedule (no busy loop)")
	}
	if m.fwTicking {
		t.Error("fwTicking should be false once no fireworks remain")
	}
}

// TestFireworkKeyLifecycle covers the 'f' handler and ticker discipline: launch,
// no double-ticker, and esc clearing the fireworks + stopping the tick.
func TestFireworkKeyLifecycle(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := New(config.Config{})
	m.active = map[int]bool{}
	m.rebuildItems()
	m.width, m.height = 100, 40
	m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'E'}})

	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m = res.(model)
	if len(m.fireworks) != 1 || !m.fwTicking || cmd == nil {
		t.Fatalf("first 'f': fireworks=%d fwTicking=%v cmd=%v", len(m.fireworks), m.fwTicking, cmd != nil)
	}
	res, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	m = res.(model)
	if len(m.fireworks) != 2 || cmd != nil {
		t.Fatalf("second 'f': fireworks=%d cmd=%v (want 2 and no new ticker)", len(m.fireworks), cmd != nil)
	}

	m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.showEgg {
		t.Error("esc should close the egg")
	}
	if len(m.fireworks) != 0 {
		t.Error("esc should clear in-flight fireworks immediately")
	}
	res, cmd = m.Update(fwTickMsg{})
	m = res.(model)
	if cmd != nil || m.fwTicking {
		t.Error("a fireworks tick after close must stop (no reschedule, fwTicking false)")
	}
}

// TestFwClutchNext covers the intake clutch's hysteresis gate (3e8b): engage
// strictly above fwClutchOnMs, disengage strictly below fwClutchOffMs, and
// HOLD the passed-in state unchanged (both true and false) inside the band so
// it doesn't flap frame-to-frame.
func TestFwClutchNext(t *testing.T) {
	if got := fwClutchNext(false, fwClutchOnMs+0.01); !got {
		t.Errorf("engage: ewma just above fwClutchOnMs should engage, got %v", got)
	}
	if got := fwClutchNext(true, fwClutchOffMs-0.01); got {
		t.Errorf("disengage: ewma just below fwClutchOffMs should disengage, got %v", got)
	}
	mid := (fwClutchOnMs + fwClutchOffMs) / 2
	if got := fwClutchNext(true, mid); !got {
		t.Errorf("hysteresis-hold: engaged=true inside the band should stay true, got %v", got)
	}
	if got := fwClutchNext(false, mid); got {
		t.Errorf("hysteresis-hold: engaged=false inside the band should stay false, got %v", got)
	}
	// Boundary values themselves fall inside the (inclusive) hold band since
	// the gate uses strict >/< comparisons.
	if got := fwClutchNext(true, fwClutchOnMs); !got {
		t.Errorf("hysteresis-hold at the ON boundary: engaged=true should stay true, got %v", got)
	}
	if got := fwClutchNext(false, fwClutchOffMs); got {
		t.Errorf("hysteresis-hold at the OFF boundary: engaged=false should stay false, got %v", got)
	}
}

// TestFwLagNext covers the EWMA fold (3e8b): a zero prior seeds directly to
// the observation, a converging sequence of steady observations moves the
// EWMA toward that steady value by fwLagAlpha each step, and a lag spike
// followed by recovery rises then falls back down.
func TestFwLagNext(t *testing.T) {
	// Seed: zero prior takes the observation as-is, regardless of magnitude.
	if got := fwLagNext(0, 250); got != 250 {
		t.Errorf("seed: fwLagNext(0, 250) = %v, want 250", got)
	}
	if got := fwLagNext(0, 0); got != 0 {
		t.Errorf("seed: fwLagNext(0, 0) = %v, want 0", got)
	}

	// Convergence: starting away from a steady observed interval, repeated
	// folds move monotonically toward it and land within a small tolerance.
	ewma := fwLagNext(0, 100) // seed at 100ms
	const steady = 50.0
	prev := ewma
	for i := 0; i < 30; i++ {
		ewma = fwLagNext(ewma, steady)
		if ewma > prev {
			t.Fatalf("convergence: EWMA should move monotonically toward %v, went %v -> %v", steady, prev, ewma)
		}
		prev = ewma
	}
	if diff := ewma - steady; diff > 0.5 || diff < -0.5 {
		t.Errorf("convergence: EWMA %v did not converge near steady %v", ewma, steady)
	}
	// A single fold step size matches the fwLagAlpha smoothing factor exactly.
	if got := fwLagNext(100, 50); got != 100+fwLagAlpha*(50-100) {
		t.Errorf("fold: fwLagNext(100, 50) = %v, want %v", got, 100+fwLagAlpha*(50-100))
	}

	// Spike then recovery: a steady EWMA jumps on a lag spike, then falls
	// back down across subsequent normal observations.
	ewma = fwLagNext(0, 50) // seed at a healthy 50ms cadence
	spiked := fwLagNext(ewma, 500)
	if spiked <= ewma {
		t.Fatalf("spike: EWMA should rise on a lag spike, %v -> %v", ewma, spiked)
	}
	recovering := spiked
	for i := 0; i < 20; i++ {
		next := fwLagNext(recovering, 50)
		if next > recovering {
			t.Fatalf("recovery: EWMA should fall monotonically back toward 50, went %v -> %v", recovering, next)
		}
		recovering = next
	}
	if recovering >= spiked {
		t.Errorf("recovery: EWMA %v should have dropped well below the post-spike value %v", recovering, spiked)
	}
}

// TestFwLagWarmupIdleReset covers the warmup/idle-reset contract (3e8b) at
// both levels: the pure fwLagNext seed behavior, and a focused Update test
// confirming the fwTickMsg handler actually resets fwLagEWMA/fwClutch/
// lastFwTick when the sky goes idle, and that the immediately-following tick
// only stamps lastFwTick without measuring (guarded by the IsZero check, so
// this holds regardless of real elapsed wall-clock time in the test).
func TestFwLagWarmupIdleReset(t *testing.T) {
	// Pure level: a first observation after a reset (ewma==0) seeds directly
	// rather than smoothing toward 0.
	if got := fwLagNext(0, 37); got != 37 {
		t.Errorf("warmup seed: fwLagNext(0, 37) = %v, want 37", got)
	}

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := New(config.Config{})
	m.active = map[int]bool{}
	m.rebuildItems()
	m.width, m.height = 100, 40
	m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'E'}})

	// Simulate mid-session lag state, then let the sky go idle: a single
	// already-spent firework reaps to empty on the next step (mirrors
	// TestStepFireworksReaps), which should trigger the idle reset.
	m.fwLagEWMA = 123
	m.fwClutch = true
	m.lastFwTick = time.Now().Add(-time.Second)
	m.fwTicking = true
	m.fireworks = []firework{{stage: fwBurst}}

	res, cmd := m.Update(fwTickMsg{})
	m = res.(model)
	if cmd != nil {
		t.Error("idle reset: an idle fireworks tick must not reschedule")
	}
	if m.fwTicking {
		t.Error("idle reset: fwTicking should be false once the sky is empty")
	}
	if m.fwLagEWMA != 0 {
		t.Errorf("idle reset: fwLagEWMA should reset to 0, got %v", m.fwLagEWMA)
	}
	if m.fwClutch {
		t.Error("idle reset: fwClutch should reset to false")
	}
	if !m.lastFwTick.IsZero() {
		t.Error("idle reset: lastFwTick should reset to the zero time")
	}

	// The very next tick, after a fresh launch, must only stamp lastFwTick --
	// not measure -- since lastFwTick is zero coming in.
	m.fireworks = []firework{newFirework(m.width, m.height, m.emoji)}
	m.fwTicking = true
	res, _ = m.Update(fwTickMsg{})
	m = res.(model)
	if m.fwLagEWMA != 0 {
		t.Errorf("warmup: the tick right after an idle reset must not measure, fwLagEWMA = %v, want 0", m.fwLagEWMA)
	}
	if m.lastFwTick.IsZero() {
		t.Error("warmup: the tick right after an idle reset should still stamp lastFwTick")
	}
}

// TestFireworkClutchGating covers the 'f' handler's clutch gate (3e8b): when
// fwClutch is engaged, new launches are refused outright even though the
// slice is well under fwCap (in-flight fireworks are never touched); when
// disengaged, launches proceed normally up to fwCap.
func TestFireworkClutchGating(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := New(config.Config{})
	m.active = map[int]bool{}
	m.rebuildItems()
	m.width, m.height = 120, 45
	m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'E'}})

	m.fwClutch = true
	for i := 0; i < 5; i++ {
		res, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
		m = res.(model)
	}
	if len(m.fireworks) != 0 {
		t.Errorf("clutch engaged: intake should be refused entirely, got %d fireworks", len(m.fireworks))
	}

	m.fwClutch = false
	for i := 0; i < fwCap+10; i++ {
		res, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
		m = res.(model)
	}
	if len(m.fireworks) != fwCap {
		t.Errorf("clutch disengaged: launches should proceed up to fwCap, got %d, want %d", len(m.fireworks), fwCap)
	}
}

// TestEggViewGrid covers full-screen grid composition: exact dimensions, the
// egg block placed at its offset with the fanfare rows blank (43xw) on their
// recorded rows, credits present (sans Fable), and fireworks overlaid without
// overflow.
func TestEggViewGrid(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := New(config.Config{Markers: "emoji"})
	m.active = map[int]bool{}
	m.rebuildItems()
	m.showEgg = true
	m.width, m.height = 100, 40

	// Composition with no fireworks: the block is placed at its offset with the
	// fanfare on its recorded rows, credits intact, exact viewport dimensions.
	view := m.eggView()
	lines := strings.Split(view, "\n")
	if len(lines) != 40 {
		t.Fatalf("eggView should render exactly 40 rows, got %d", len(lines))
	}
	for i, ln := range lines {
		if w := lipgloss.Width(ln); w != 100 {
			t.Fatalf("row %d display width %d != viewport 100 (overflow)", i, w)
		}
	}
	lay := eggLayout(100, 40)
	// 43xw: the fanfare rows are now blank spacers -- sparkles removed, but the
	// row still exists (occupies the grid) so the burst band anchors hold.
	if s := strings.TrimSpace(stripANSI(lines[lay.topFanfareRow])); s != "" {
		t.Errorf("the top fanfare row should be blank (spacer only), got %q", s)
	}
	if s := strings.TrimSpace(stripANSI(lines[lay.botFanfareRow])); s != "" {
		t.Errorf("the bottom fanfare row should be blank (spacer only), got %q", s)
	}
	if strings.TrimSpace(stripANSI(lines[lay.topFanfareRow+lay.eggRows/2])) == "" {
		t.Error("an egg body row should be non-empty")
	}
	plain := stripANSI(view)
	if !strings.Contains(plain, "Michael E. Gruen") || !strings.Contains(plain, "LLM Agent Fleet") {
		t.Error("egg credits should render inside the grid")
	}
	// 43xw: Fable was dropped from the fleet credit line.
	if strings.Contains(plain, "Fable") {
		t.Error("egg credits should no longer mention Fable")
	}

	// With fireworks overlaid, the grid must still be exactly 40x100 (the
	// sparks clip to the viewport; they may draw over text but never overflow).
	for i := 0; i < 20; i++ {
		m.fireworks = append(m.fireworks, newFirework(100, 40, true))
	}
	for i := 0; i < 12; i++ { // let some rise and some burst
		m.fireworks = stepFireworks(m.fireworks)
	}
	fwLines := strings.Split(m.eggView(), "\n")
	if len(fwLines) != 40 {
		t.Fatalf("with fireworks, eggView should still be 40 rows, got %d", len(fwLines))
	}
	for i, ln := range fwLines {
		if w := lipgloss.Width(ln); w != 100 {
			t.Fatalf("with fireworks, row %d width %d != 100 (overflow)", i, w)
		}
	}
}

// TestEggViewNoColor covers NO_COLOR / --no-color degradation: under the Ascii
// color profile the overlay (egg + bursts) emits no ANSI escapes at all.
func TestEggViewNoColor(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	prev := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(prev)
	lipgloss.SetColorProfile(termenv.Ascii)

	m := New(config.Config{Markers: "ascii"})
	m.active = map[int]bool{}
	m.rebuildItems()
	m.showEgg = true
	m.width, m.height = 100, 40
	for i := 0; i < 24; i++ {
		m.fireworks = append(m.fireworks, newFirework(100, 40, false))
	}
	for i := 0; i < 15; i++ {
		m.fireworks = stepFireworks(m.fireworks)
	}
	if view := m.eggView(); strings.ContainsRune(view, '\x1b') {
		t.Error("under the Ascii profile the egg overlay must contain no ANSI escape sequences")
	}
}

// TestFireworkSchemes covers the ~8 distinct colour schemes (monochrome ..
// vivid), so a firework picks a real variety.
func TestFireworkSchemes(t *testing.T) {
	if len(fwSchemes) < 8 {
		t.Errorf("want ~8 firework colour schemes, got %d", len(fwSchemes))
	}
	seen := map[string]bool{}
	for i := range fwSchemes {
		seen[string(fwSchemes[i].colorAt(0.85, 0))] = true
	}
	if len(seen) < 6 {
		t.Errorf("schemes should span varied colours; got %d distinct", len(seen))
	}
}

// TestEggViewTinyNoPanic covers the small/zero-size fallback paths with
// fireworks present: they must never panic or overflow.
func TestEggViewTinyNoPanic(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, d := range [][2]int{{0, 0}, {1, 1}, {10, 5}, {52, 17}, {200, 60}} {
		m := New(config.Config{})
		m.active = map[int]bool{}
		m.rebuildItems()
		m.showEgg = true
		m.width, m.height = d[0], d[1]
		for i := 0; i < 30; i++ {
			m.fireworks = append(m.fireworks, newFirework(d[0], d[1], i%2 == 0))
		}
		for i := 0; i < 40; i++ {
			m.fireworks = stepFireworks(m.fireworks)
		}
		_ = m.eggView() // must not panic
	}
}

// --- p39s: grouped key hints (bottom-bar grid + unified overlay) ---

// TestKeyGroupsAndFullHelp covers the single grouping source: keyMap.groups()
// yields the four approved columns in order, and FullHelp() mirrors them one
// inner slice per column. Expose ends in the contextual C then x (lock always
// last); Copy (c) sits under NewPort (n) in Favorites.
func TestKeyGroupsAndFullHelp(t *testing.T) {
	k := newKeyMap()
	groups := k.groups()

	wantNames := []string{"Expose", "Favorites", "View", "App"}
	if len(groups) != len(wantNames) {
		t.Fatalf("groups() = %d columns, want %d", len(groups), len(wantNames))
	}
	// 3cwx: Favorites carries F (forget, the old "u"); u is undo and lives in
	// App alongside ctrl+r (redo), which groups() includes so the "?" overlay
	// documents it even though barGroups hides it from the bottom bar.
	wantKeys := [][]string{
		{"space", "p", "C", "x"},
		{"f", "F", "n", "c", "l"},
		{"/", "a", "r"},
		{"u", "ctrl+r", "?", "q"},
	}
	full := k.FullHelp()
	if len(full) != len(groups) {
		t.Fatalf("FullHelp() = %d columns, want %d (one per group)", len(full), len(groups))
	}
	for i, g := range groups {
		if g.name != wantNames[i] {
			t.Errorf("group %d name = %q, want %q", i, g.name, wantNames[i])
		}
		if len(g.bindings) != len(wantKeys[i]) {
			t.Fatalf("group %q has %d bindings, want %d", g.name, len(g.bindings), len(wantKeys[i]))
		}
		for j, b := range g.bindings {
			if got := b.Help().Key; got != wantKeys[i][j] {
				t.Errorf("group %q binding %d key = %q, want %q", g.name, j, got, wantKeys[i][j])
			}
		}
		// FullHelp column must be the same bindings as the group.
		if len(full[i]) != len(g.bindings) {
			t.Fatalf("FullHelp col %d len = %d, want %d", i, len(full[i]), len(g.bindings))
		}
		for j, b := range full[i] {
			if got := b.Help().Key; got != wantKeys[i][j] {
				t.Errorf("FullHelp col %d binding %d key = %q, want %q", i, j, got, wantKeys[i][j])
			}
		}
	}
}

// TestBottomBarGridAligned drives the real model at a width just below the
// 04rb fold threshold (~70, the width Favorites' fold needs to fit -- see
// TestBottomBarGridFolds) and asserts the bar renders the four grouped
// columns UNFOLDED, at their exact packed floor width, with a header row and
// aligned gutters: descriptions line up within a column and columns line up
// across rows. With no dangling, Expose is space/p/x (lock last, clean
// dropped) and Favorites is the tallest column (f/u/n/c/l), so the grid is a
// header + 5 rows. (Previously this used width=100, which now has enough
// surplus to fold Favorites/Expose/View -- see TestBottomBarGridFolds for
// that behavior instead.)
func TestBottomBarGridAligned(t *testing.T) {
	m := New(config.Config{})
	const width = 65 // packed floor is 58 wide; Favorites' fold needs >=70
	m.help.Width = width
	m.width = width

	grid := stripANSI(m.renderLegend())
	lines := strings.Split(grid, "\n")
	if len(lines) < 6 {
		t.Fatalf("grid should be a header + up to 5 rows (>=6 lines); got %d:\n%s", len(lines), grid)
	}
	hdr := lines[0]

	// at returns the (row, col) of needle within the data rows (row 0 == the
	// first data line, i.e. lines[1]); fails if it appears in no data row.
	at := func(needle string) (row, col int) {
		for i, ln := range lines[1:] {
			if c := strings.Index(ln, needle); c >= 0 {
				return i, c
			}
		}
		t.Fatalf("no data row contains %q; got:\n%s", needle, grid)
		return -1, -1
	}

	// Header row carries all four section names, in order, on one line.
	prev := -1
	for _, name := range []string{"Expose", "Favorites", "View", "App"} {
		i := strings.Index(hdr, name)
		if i < 0 {
			t.Fatalf("header row missing %q; got %q", name, hdr)
		}
		if i <= prev {
			t.Errorf("header %q out of order (at %d, prev %d): %q", name, i, prev, hdr)
		}
		prev = i
	}

	// Columns line up: each header's start == the start of every cell in its
	// column, wherever that cell falls (Expose's lock is on the 3rd data row,
	// Favorites' label on the 5th).
	col := func(label string, header, needle string) {
		_, c := at(needle)
		if h := strings.Index(hdr, header); c != h {
			t.Errorf("%s misaligned: %q header at %d, cell %q at %d", label, header, h, needle, c)
		}
	}
	// Expose's key gutter is 5 wide (from "space"), so its cells render like
	// "x     lock/unlock"; anchor its column on "space serve" (which starts flush
	// at the column) rather than a padded cell.
	col("Expose/serve", "Expose", "space serve")
	col("Favorites/favorite", "Favorites", "f favorite")
	col("Favorites/label", "Favorites", "l label")
	col("View/filter", "View", "/ filter")
	col("App/help", "App", "? help")
	col("App/quit", "App", "q quit")

	// Copy sits directly under "n add favorite": same column, next row down.
	nRow, nCol := at("n add favorite")
	cRow, cCol := at("c copy URL")
	if cCol != nCol || cRow != nRow+1 {
		t.Errorf("c copy URL should be the row directly under n add favorite; n at (%d,%d), c at (%d,%d)", nRow, nCol, cRow, cCol)
	}

	// Lock is the LAST Expose row, and its key sits flush at the Expose column
	// start. (Match the desc "lock/unlock" since the padded "x     lock/unlock"
	// cell isn't a single-space substring; lockRow indexes lines[1:].)
	lockRow, _ := at("lock/unlock")
	lockLine := lines[lockRow+1]
	exposeCol := strings.Index(hdr, "Expose")
	if exposeCol >= len(lockLine) || lockLine[exposeCol] != 'x' {
		t.Errorf("lock's key should sit flush at the Expose column start (col %d); line: %q", exposeCol, lockLine)
	}
	for li := lockRow + 2; li < len(lines); li++ {
		if ln := lines[li]; len(ln) > exposeCol && ln[exposeCol] != ' ' {
			t.Errorf("Expose column has content below lock (line %d): %q", li, ln)
		}
	}

	// Within the Expose column the key gutter aligns the descriptions: "serve"
	// (after "space ") and "funnel" (after "p     ") start at the same offset.
	_, serveCol := at("serve")
	_, funnelCol := at("funnel")
	if serveCol != funnelCol {
		t.Errorf("Expose gutter misaligned: serve at %d, funnel at %d", serveCol, funnelCol)
	}
}

// TestBottomBarGridFolds covers 04rb's width-driven fold: once the terminal
// has more room than the packed grid needs, a tall group's single body
// sub-column splits into 2 column-major sub-columns instead of the groups
// spreading apart with bigger gutters -- which SHORTENS the bar (its height
// is set by the tallest group's row count), tallest group first, never past
// 2 sub-columns per group, and stops folding as soon as a candidate no
// longer fits.
func TestBottomBarGridFolds(t *testing.T) {
	lineOf := func(lines []string, needle string) int {
		for i, ln := range lines {
			if strings.Contains(ln, needle) {
				return i
			}
		}
		return -1
	}

	m := New(config.Config{})

	// Floor: below the fold threshold (Favorites' fold needs total width
	// >=70; see the 58-wide packed floor in TestBottomBarNarrowFallback), the
	// grid is the exact packed layout -- header + 5 rows (Favorites, the
	// tallest group unfolded, is f/u/n/c/l).
	m.help.Width, m.width = 65, 65
	floor := stripANSI(m.renderLegend())
	floorLines := strings.Split(floor, "\n")
	if len(floorLines) != 6 {
		t.Fatalf("floor (width 65) grid should be header + 5 rows (6 lines); got %d:\n%s", len(floorLines), floor)
	}

	// Wide: 100 cols is enough surplus to fold Favorites (tallest, 5 rows),
	// then Expose and View (tied at 3, Expose first since it's earlier in
	// group order), but not App (2 rows, tried last) -- folding it would push
	// the grid past 100. Folding SHORTENS the bar: Favorites' fold (ceil(5/2)
	// = 3 rows) is now the tallest group, so header+3 = 4 lines, fewer than
	// the floor's 6 -- not just wider-gapped.
	m.help.Width, m.width = 100, 100
	wide := stripANSI(m.renderLegend())
	wideLines := strings.Split(wide, "\n")
	if len(wideLines) >= len(floorLines) {
		t.Errorf("wide (100) grid (%d lines) should be shorter than the floor grid (%d lines) once tall groups fold:\nfloor:\n%s\nwide:\n%s",
			len(wideLines), len(floorLines), floor, wide)
	}

	// Favorites folded: top-heavy column-major split -- f/u/n down the first
	// sub-column, c/l down the second (never a dangling item left stranded
	// atop an empty second sub-column). "c copy URL" now sits beside
	// "f favorite" on the SAME row, not two rows below it as in the floor.
	if r1, r2 := lineOf(wideLines, "f favorite"), lineOf(wideLines, "c copy URL"); r1 < 0 || r1 != r2 {
		t.Errorf("Favorites should fold f favorite/c copy URL onto the same row; f favorite row %d, c copy URL row %d:\n%s", r1, r2, wide)
	}
	if r1, r2 := lineOf(wideLines, "F forget"), lineOf(wideLines, "l label"); r1 < 0 || r1 != r2 {
		t.Errorf("Favorites should fold F forget/l label onto the same row; F forget row %d, l label row %d:\n%s", r1, r2, wide)
	}
	if r := lineOf(wideLines, "n add favorite"); r < 0 {
		t.Errorf("n add favorite missing from wide grid:\n%s", wide)
	} else if strings.Contains(wideLines[r], "l label") {
		t.Errorf("n add favorite's row should have an empty second sub-col (only 5 items, top-heavy 3/2 split): %q", wideLines[r])
	}

	// Expose folded too (tied at 3 with View, but earlier in group order so
	// tried first): space serve | x lock/unlock on one row.
	if r1, r2 := lineOf(wideLines, "space serve"), lineOf(wideLines, "x lock/unlock"); r1 < 0 || r1 != r2 {
		t.Errorf("Expose should fold space serve/x lock/unlock onto the same row; space serve row %d, x lock/unlock row %d:\n%s", r1, r2, wide)
	}

	// App (3 bar bindings since 3cwx -- u undo, ? help, q quit; ctrl+r redo is
	// hidden from the bar) is tried last and doesn't fit the fold at width 100,
	// so it stays a single unfolded column: help and quit on SEPARATE rows.
	if r1, r2 := lineOf(wideLines, "? help"), lineOf(wideLines, "q quit"); r1 < 0 || r2 < 0 || r1 == r2 {
		t.Errorf("App should NOT fold at width 100 (no surplus left after the other 3 groups); ? help row %d, q quit row %d:\n%s", r1, r2, wide)
	}

	// Ceiling: a very wide terminal folds ALL FOUR groups, App included,
	// never past 2 sub-columns per group. Past that point extra width just
	// sits blank on the right -- no re-growing, no gutter-stretching.
	m.help.Width, m.width = 200, 200
	ceiling := stripANSI(m.renderLegend())
	m.help.Width, m.width = 400, 400
	pastCeiling := stripANSI(m.renderLegend())
	if ceiling != pastCeiling {
		t.Errorf("grid should stop changing once every group is folded; width=200 and width=400 rendered differently:\n200:\n%s\n400:\n%s", ceiling, pastCeiling)
	}
	ceilingLines := strings.Split(ceiling, "\n")
	// App's 3 bar bindings fold top-heavy 2/1: "u undo" and "? help" down the
	// first sub-column, "q quit" alone in the second -- so the fold shows up as
	// q quit rising to share u undo's row (before 3cwx, App was 2 bindings and
	// this read "? help | q quit").
	if r1, r2 := lineOf(ceilingLines, "u undo"), lineOf(ceilingLines, "q quit"); r1 < 0 || r1 != r2 {
		t.Errorf("App should fold at the ceiling width (u undo | q quit on one row); u undo row %d, q quit row %d:\n%s", r1, r2, ceiling)
	}
	if r1, r2 := lineOf(ceilingLines, "? help"), lineOf(ceilingLines, "q quit"); r1 < 0 || r1 == r2 {
		t.Errorf("App's folded second sub-col holds only q quit; ? help should be on its own row, not beside q quit; ? help row %d, q quit row %d:\n%s", r1, r2, ceiling)
	}

	// Still no truncation/ellipsis at the ceiling: every hint present. ("p
	// funnel public" isn't checked as a single-space literal here: unlike the
	// wrapped fallback, the grid pads keys to their sub-column's gutter --
	// Expose's folded left sub-col gutter is 5 (from "space"), so "p" renders
	// padded ("p     funnel public") -- checking the description alone
	// sidesteps that padding.)
	for _, want := range []string{
		"space serve", "funnel public", "x lock/unlock",
		"f favorite", "F forget", "n add favorite", "c copy URL", "l label",
		"/ filter", "a switch view", "r refresh",
		"u undo", "? help", "q quit",
	} {
		if !strings.Contains(ceiling, want) {
			t.Errorf("ceiling grid dropped %q; got:\n%s", want, ceiling)
		}
	}

	// ...but redo stays OFF the bar at every width, ceiling included: it's in
	// groups() for the "?" overlay only (3cwx).
	if strings.Contains(ceiling, "ctrl+r") {
		t.Errorf("ctrl+r redo must not appear in the bottom bar, even at the ceiling width; got:\n%s", ceiling)
	}
}

// TestBottomBarGridFoldedSubColAligned covers kata xqdk: a folded group's
// SECOND sub-column must begin at the same display column on every row, not
// hug the previous row's (possibly shorter) sub-col-1 content. Favorites
// folds at width 100 (see TestBottomBarGridFolds) into a top-heavy 3/2 split:
// f/u/n down sub-col 1, c/l down sub-col 2. Sub-col 1's rendered content width
// varies by row -- "f favorite" is 10 wide, "u unfavorite" is 12, "n add
// favorite" is 14 (the widest, setting subWidth[0]) -- which is exactly the
// shape that exposed the bug: sub-col 2 used to start right after each row's
// OWN sub-col-1 content instead of at the fixed subWidth[0] edge, so "c copy
// URL" (behind the short "f favorite") landed left of where "l label" (behind
// the longer "u unfavorite") landed, instead of both landing on the same
// column.
func TestBottomBarGridFoldedSubColAligned(t *testing.T) {
	m := New(config.Config{})
	m.help.Width, m.width = 100, 100

	grid := stripANSI(m.renderLegend())
	lines := strings.Split(grid, "\n")

	col := func(needle string) int {
		for _, ln := range lines {
			if c := strings.Index(ln, needle); c >= 0 {
				return c
			}
		}
		t.Fatalf("grid missing %q:\n%s", needle, grid)
		return -1
	}

	// Sub-col 2's two cells ("c copy URL" on the "f favorite" row, "l label" on
	// the "u unfavorite" row) must start at the SAME column -- the fixed
	// subWidth[0] edge -- regardless of how much shorter sub-col 1's own
	// content is on either row.
	cCol, lCol := col("c copy URL"), col("l label")
	if cCol != lCol {
		t.Errorf("Favorites sub-col 2 misaligned across rows: 'c copy URL' at %d, 'l label' at %d (should match):\n%s", cCol, lCol, grid)
	}

	// A short sub-col-1 cell doesn't shift sub-col 2 left: "n add favorite" is
	// sub-col 1's widest row (14 wide, == subWidth[0]), so sub-col 2's fixed
	// edge must sit exactly one sub-column gap past where that row's content
	// ends -- not past the shorter "f favorite"/"u unfavorite" rows' content.
	nEnd := col("n add favorite") + len("n add favorite")
	if want := nEnd + legendSubColGap; cCol != want {
		t.Errorf("Favorites sub-col 2 should start at %d (widest sub-col-1 row %q ends at %d, + %d-wide gap); got %d:\n%s", want, "n add favorite", nEnd, legendSubColGap, cCol, grid)
	}
}

// TestBottomBarNarrowFallback covers the responsive fallback: below the
// content-derived threshold the bar becomes a wrapped grouped bar that never
// truncates (every key+desc still present) and never overflows the width.
func TestBottomBarNarrowFallback(t *testing.T) {
	// The 4-column grid is 58 cells wide; 50 forces the wrapped fallback.
	const width = 50
	m := New(config.Config{})
	m.help.Width = width
	m.width = width

	bar := stripANSI(m.renderLegend())
	lines := strings.Split(bar, "\n")

	// Not the grid: the four headers are no longer all on the first line.
	if all := strings.Contains(lines[0], "Expose") && strings.Contains(lines[0], "App"); all {
		t.Errorf("narrow width should fall back, not render the single-row grid header; got %q", lines[0])
	}

	// No line overflows the width (no soft-wrap that would break the sizing math).
	for i, ln := range lines {
		if w := lipgloss.Width(ln); w > width {
			t.Errorf("fallback line %d width %d > %d (truncation/overflow): %q", i, w, width, ln)
		}
	}

	// Every hint is still present -- no truncation, no elision. (C is contextual
	// and absent with no dangling.)
	for _, want := range []string{
		"Expose", "Favorites", "View", "App",
		"space serve", "p funnel public", "c copy URL",
		"f favorite", "F forget", "n add favorite", "l label",
		"x lock/unlock", "/ filter", "a switch view", "r refresh",
		"u undo", "? help", "q quit",
	} {
		if !strings.Contains(bar, want) {
			t.Errorf("narrow fallback dropped %q; got:\n%s", want, bar)
		}
	}
	if strings.Contains(bar, "clean") {
		t.Errorf("C clean should be absent with no dangling; got:\n%s", bar)
	}
}

// TestExposeContextualClean covers the contextual "C clean stale" now that
// Protect is folded into Expose: with no dangling the Expose column ends at
// "x lock/unlock" (space/p/x, no clean, no reserved blank slot); when a
// dangling forward exists it gains "C clean stale" -- inserted just ABOVE lock
// so "x lock/unlock" stays the last item in the column in either state.
func TestExposeContextualClean(t *testing.T) {
	m := New(config.Config{})
	m.help.Width = 100
	m.width = 100

	expose := func(groups []keyGroup) keyGroup {
		for _, g := range groups {
			if g.name == "Expose" {
				return g
			}
		}
		t.Fatal("no Expose group")
		return keyGroup{}
	}
	lastKey := func(g keyGroup) string {
		if len(g.bindings) == 0 {
			return ""
		}
		return g.bindings[len(g.bindings)-1].Help().Key
	}

	// No dangling -> Expose is space/p/x (clean dropped), lock last, and the
	// rendered bar omits "clean".
	noClean := expose(m.barGroups(false))
	if got := len(noClean.bindings); got != 3 {
		t.Errorf("Expose should be 3 bindings (space/p/x) with no dangling; got %d", got)
	}
	if k := lastKey(noClean); k != "x" {
		t.Errorf("lock (x) should be the last Expose binding with no dangling; got %q", k)
	}
	if noDangle := stripANSI(m.renderLegend()); strings.Contains(noDangle, "clean") {
		t.Errorf("bar should not show 'clean' with no dangling:\n%s", noDangle)
	}

	// A served-but-not-listening port is dangling -> Expose gains "C clean",
	// still with lock (x) last.
	m.active = map[int]bool{9999: true}
	if !m.hasDangling() {
		t.Fatal("setup: expected a dangling forward")
	}
	withClean := expose(m.barGroups(true))
	if got := len(withClean.bindings); got != 4 {
		t.Errorf("Expose should be 4 bindings (space/p/C/x) with a dangling; got %d", got)
	}
	if k := lastKey(withClean); k != "x" {
		t.Errorf("lock (x) should STILL be the last Expose binding with a dangling; got %q", k)
	}
	if dangle := stripANSI(m.renderLegend()); !strings.Contains(dangle, "clean stale") {
		t.Errorf("bar should show 'C clean stale' with a dangling:\n%s", dangle)
	}
}

// TestLegendSizingNoClip asserts the list-height/bottom-height bookkeeping stays
// consistent: after a WindowSizeMsg the whole View fits within the terminal
// height (nothing clips or overlaps) at wide and narrow widths, with and without
// a dangling forward -- including when a dangling appears AFTER the resize, which
// the worst-case reservation must already cover.
func TestLegendSizingNoClip(t *testing.T) {
	build := func(w, h int, sizeDangling, renderDangling bool) model {
		m := New(config.Config{Ports: map[int]config.PortMeta{}})
		m.allPorts = []portscan.Port{
			{Number: 3000, Process: "node"}, {Number: 8080, Process: "srv"},
			{Number: 9000, Process: "api"}, {Number: 5173, Process: "vite"},
		}
		m.showAllPorts = true
		m.rebuildItems()
		if sizeDangling {
			m.active = map[int]bool{9999: true}
		}
		r, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
		m = r.(model)
		// Optionally flip the dangling state after sizing (no new resize): the
		// reservation reserved the worst case, so this must still not clip.
		if renderDangling {
			m.active = map[int]bool{9999: true}
		} else if !sizeDangling {
			m.active = map[int]bool{}
		}
		return m
	}

	for _, tc := range []struct {
		name                         string
		w, h                         int
		sizeDangling, renderDangling bool
	}{
		{"wide/no-dangling", 100, 24, false, false},
		{"wide/dangling", 100, 24, true, true},
		{"wide/dangling-appears-after-resize", 100, 24, false, true},
		// Below the 58-wide grid threshold, so these exercise the wrapped fallback.
		{"narrow/no-dangling", 50, 24, false, false},
		{"narrow/dangling", 50, 24, true, true},
		{"narrow/dangling-appears-after-resize", 50, 24, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := build(tc.w, tc.h, tc.sizeDangling, tc.renderDangling)
			view := m.View()
			if got := lipgloss.Height(view); got > m.height {
				t.Errorf("View height %d > terminal height %d (clip/overlap):\n%s", got, m.height, stripANSI(view))
			}
			// The bar's last line must be present (not pushed off the bottom).
			plain := stripANSI(view)
			if !strings.Contains(plain, "q quit") {
				t.Errorf("bottom bar clipped: %q not found in View:\n%s", "q quit", plain)
			}
		})
	}
}

// TestLegendReservationDominatesLive pins the invariant TestLegendSizingNoClip
// depends on, across every width rather than a handful of samples: the
// WindowSizeMsg height reservation (ui.go's Update, renderLegendWith(true) --
// worst case, as if a dangling forward existed) must never be SHORTER than
// the live render with no dangling (renderLegendWith(false), which drops "C
// clean stale" and so renders Expose with one fewer binding).
//
// Before 04rb this held trivially: maxRows was always Favorites' fixed 5
// bindings, independent of width or the Clean row. After 04rb, bar height is
// width- and fold-dependent, and folding is keyed off each group's
// UNFOLDED binding count, which differs between the two calls (Expose: 4
// bindings reserved vs 3 live). It still holds today, but only because
// ceil(4/2) == ceil(3/2) == 2 and "clean stale"/"lock/unlock" happen to be
// the same length (11 chars), so Expose's rendered width AND row count come
// out identical either way, and every other group's fold decision (driven by
// the shared total-width budget) is therefore unaffected by cleanEnabled too.
// That's an incidental tie in today's copy, not something the code enforces
// -- a future edit to keyLegendDescs/newKeyMap's Expose text or binding count
// could silently break it and reintroduce the exact clipping bug the
// reservation exists to prevent. Brute-forcing width here (rather than
// TestLegendSizingNoClip's few sampled widths) is what would actually catch
// that regression. (79xb: this brute-force scan is also what caught the
// second-hint "space serve" contextual polish breaking the reservation at
// width 54 -- see the TODO(79xb) on renderLegendWith -- so it stays a
// single-hint scan, matching the shipped single-hint (cleanEnabled) code.)
func TestLegendReservationDominatesLive(t *testing.T) {
	m := New(config.Config{})
	for w := 1; w <= 300; w++ {
		m.help.Width = w
		reserved := strings.Count(m.renderLegendWith(true), "\n") + 1
		live := strings.Count(m.renderLegendWith(false), "\n") + 1
		if reserved < live {
			t.Fatalf("width %d: reserved height %d < live height %d -- the worst-case reservation no longer dominates, WindowSizeMsg would under-reserve and clip the list", w, reserved, live)
		}
	}
}

// TestRenderStatusLineWrapsLongFlash covers 83wv pt2: bubbletea's renderer
// hard-truncates any bottom-bar line wider than the terminal with no
// ellipsis, so the honest (and long) guard-toast strings from e2f44d6 would
// clip mid-word on a normal 80-column terminal unless the status line wraps.
// At a narrow width the full message must still be present -- nothing
// dropped -- just spread across more than one line; at a wide width the same
// text should fit on one line, unchanged.
func TestRenderStatusLineWrapsLongFlash(t *testing.T) {
	// The general reachTailnet guard toast (ui.go ~2045) -- 92 chars (qptn:
	// reworded shorter, dropping "already"/"not tailport"/"serve"), still
	// long enough to clip mid-word on an 80-col terminal without wrapping.
	const longFlash = "on tailnet — app bound wide (0.0.0.0); rebind to localhost (or 127.0.0.1) to make toggleable"

	t.Run("narrow width wraps without dropping text", func(t *testing.T) {
		m := New(config.Config{})
		m.width = 80
		m.flash = longFlash
		m.flashLevel = flashInfo

		out := m.renderStatusLine()
		if h := lipgloss.Height(out); h < 2 {
			t.Fatalf("renderStatusLine height %d at width 80 -- want >=2 (wrapped), got a single line: %q", h, stripANSI(out))
		}
		plain := stripANSI(out)
		// Wrapping inserts newlines (and lipgloss may re-flow whitespace at
		// the break), so compare word-by-word rather than requiring the
		// exact substring.
		joined := strings.Join(strings.Fields(plain), " ")
		wantWords := strings.Join(strings.Fields(longFlash), " ")
		if joined != wantWords {
			t.Errorf("wrapped status line lost or altered text:\n got: %q\nwant: %q", joined, wantWords)
		}
		for _, want := range []string{"app bound wide", "make toggleable"} {
			if !strings.Contains(plain, want) {
				t.Errorf("wrapped status line missing %q:\n%s", want, plain)
			}
		}
		// No line should exceed the requested width (that's the whole point
		// of wrapping instead of truncating).
		for _, ln := range strings.Split(out, "\n") {
			if w := lipgloss.Width(ln); w > 80 {
				t.Errorf("wrapped line exceeds width 80 (got %d): %q", w, stripANSI(ln))
			}
		}
	})

	t.Run("wide width stays a single line", func(t *testing.T) {
		m := New(config.Config{})
		m.width = 200
		m.flash = longFlash
		m.flashLevel = flashInfo

		out := m.renderStatusLine()
		if h := lipgloss.Height(out); h != 1 {
			t.Errorf("renderStatusLine height %d at width 200 -- want 1 (fits on one line)", h)
		}
		if plain := stripANSI(out); !strings.Contains(plain, longFlash) {
			t.Errorf("status line at width 200 = %q, want it to contain the full flash text", plain)
		}
	})

	t.Run("before first WindowSizeMsg falls back to unwrapped render", func(t *testing.T) {
		m := New(config.Config{}) // m.width is the zero value here
		m.flash = longFlash
		m.flashLevel = flashInfo

		out := m.renderStatusLine()
		if plain := stripANSI(out); plain != longFlash {
			t.Errorf("pre-resize status line = %q, want the raw flash text unwrapped", plain)
		}
	})
}

// TestResizeListReservesWrappedFlashHeight covers the other half of 83wv
// pt2: a wrapped multi-line flash must shrink the list's reserved height by
// exactly the extra lines it occupies (so the list never overlaps the
// wrapped toast), and the list must grow back to its original height the
// moment the flash clears -- exercised through the real Update path
// (WindowSizeMsg then a KeyMsg-driven setFlash/clear), not by calling
// resizeList directly, so it also proves every m.flash mutation site wires
// the reservation up.
func TestResizeListReservesWrappedFlashHeight(t *testing.T) {
	const longFlash = "on tailnet — app bound wide (0.0.0.0); rebind to localhost (or 127.0.0.1) to make toggleable"

	m := New(config.Config{Ports: map[int]config.PortMeta{}})
	m.allPorts = []portscan.Port{{Number: 3000, Process: "node"}}
	m.showAllPorts = true
	m.rebuildItems()

	r, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	m = r.(model)
	baseline := m.list.Height()
	if baseline <= 0 {
		t.Fatalf("baseline list height = %d, want > 0", baseline)
	}

	setCmd := m.setFlash(longFlash, flashInfo)
	if setCmd == nil {
		t.Fatal("setFlash returned a nil expiry cmd")
	}
	statusLines := lipgloss.Height(m.renderStatusLine())
	if statusLines < 2 {
		t.Fatalf("expected the flash to wrap to >=2 lines at width 80, got %d", statusLines)
	}
	withFlash := m.list.Height()
	if want := baseline - (statusLines - 1); withFlash != want {
		t.Errorf("list height with wrapped flash = %d, want %d (baseline %d minus %d extra status lines)", withFlash, want, baseline, statusLines-1)
	}
	if withFlash >= baseline {
		t.Errorf("list height %d did not shrink below baseline %d while a wrapped flash is showing", withFlash, baseline)
	}

	// Feed the expiry message directly rather than invoking setCmd (a real
	// tea.Tick that blocks for the flash's multi-second duration) -- deterministic,
	// no real clock, and exercises the exact same flashID-guarded path
	// (m.flashID was bumped by setFlash above, so this id matches).
	r, _ = m.Update(flashExpireMsg{id: m.flashID})
	m = r.(model)
	if m.flash != "" {
		t.Fatalf("flash still set after its expiry message: %q", m.flash)
	}
	if got := m.list.Height(); got != baseline {
		t.Errorf("list height after flash cleared = %d, want back to baseline %d", got, baseline)
	}
}

// TestHelpOverlayGroupedSections covers the "?" overlay reorg: the same four
// sections in the same order as the bar, each with an aligned key gutter, but
// keeping the RICH per-key prose and the surrounding Markers/warnings/config
// prose.
func TestHelpOverlayGroupedSections(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := New(config.Config{})

	// helpContent is the full overlay text; helpView windows it to the terminal
	// height (v10j). With width unset here, the key legend stays a single
	// vertical column so the sections keep their top-to-bottom order.
	help := stripANSI(m.helpContent())

	// Sections appear in the approved order.
	prev := -1
	for _, name := range []string{"Expose", "Favorites", "View", "App"} {
		at := strings.Index(help, name)
		if at < 0 {
			t.Fatalf("overlay missing section %q", name)
		}
		if at <= prev {
			t.Errorf("overlay section %q out of order (at %d, prev %d)", name, at, prev)
		}
		prev = at
	}

	// Rich prose is preserved, not reduced to the terse bar labels.
	for _, want := range []string{
		"Filter by port number",      // '/' rich prose
		"PUBLIC INTERNET",            // 'p' rich prose
		"durable",                    // 'f' rich prose
		"Tear down stale forwards",   // 'C' rich prose
		"Markers",                    // markers section kept
		"drop your live SSH session", // :22 warning kept
		"Settings (favorites, labels, locks) are saved to:", // config-path prose kept
	} {
		if !strings.Contains(help, want) {
			t.Errorf("overlay dropped prose %q", want)
		}
	}
}

// TestHelpOverlaySetupPrerequisites covers kata tapv's help-overlay note: a
// localized "Setup / prerequisites" section explaining tailscale's operator
// requirement, added near Markers -- INTO the same grouped structure
// (p39s) but not folded into KeyLegendGroups (that source is shared
// verbatim with the bottom-bar grid and quickstart's legend, and this isn't
// a keybinding). The fix command is $USER expanded.
func TestHelpOverlaySetupPrerequisites(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := New(config.Config{})
	m.operatorUser = "alice"

	// Assert on helpContent (the full overlay text); helpView windows it to the
	// terminal height (v10j).
	help := stripANSI(m.helpContent())
	if !strings.Contains(help, "Setup / prerequisites") {
		t.Fatalf("help overlay missing 'Setup / prerequisites' section:\n%s", help)
	}
	if !strings.Contains(help, "sudo tailscale set --operator=alice") {
		t.Errorf("helpView's prerequisites section should show the $USER-expanded fix command; got:\n%s", help)
	}
	// It lands ahead of the keybinding groups, near Markers -- not appended
	// after everything else, and not inside the grouped keybinding legend.
	if at, expose := strings.Index(help, "Setup / prerequisites"), strings.Index(help, "Expose"); at < 0 || expose < 0 || at > expose {
		t.Errorf("'Setup / prerequisites' (at %d) should appear before the 'Expose' keybinding group (at %d)", at, expose)
	}
}

// TestHelpOverlayScrolls covers v10j: the "?" overlay is taller than most
// terminals and alt-screen mode clips rather than scrolls, so helpView windows
// helpContent to m.height with a persistent footer, and the scroll keys pan it.
// The whole point is that content clipped off the bottom stays REACHABLE.
func TestHelpOverlayScrolls(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// A short, ordinary-width terminal: the overlay can't fit at once.
	const h = 12
	m := New(config.Config{})
	m = mustUpdate(t, m, tea.WindowSizeMsg{Width: 100, Height: h})
	m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	if !m.showHelp {
		t.Fatal("'?' should open the help overlay")
	}
	if m.helpScroll != 0 {
		t.Fatalf("opening the overlay should reset helpScroll to 0; got %d", m.helpScroll)
	}
	if m.helpMaxScroll() <= 0 {
		t.Fatalf("test premise broken: overlay should overflow height %d (maxScroll=%d)", h, m.helpMaxScroll())
	}

	// The drawn view never exceeds the terminal height, and its last row is
	// always the footer with the close hint -- no matter the scroll offset.
	fits := func(label string) string {
		view := m.helpView()
		if got := lipgloss.Height(view); got != h {
			t.Fatalf("%s: helpView height = %d, want exactly terminal height %d:\n%s", label, got, h, stripANSI(view))
		}
		plain := stripANSI(view)
		if !strings.Contains(plain, "esc") || !strings.Contains(plain, "close") {
			t.Errorf("%s: footer close hint missing from view:\n%s", label, plain)
		}
		return plain
	}

	// At the top: the title shows, the tail (config path) is clipped off, and
	// the footer advertises more below.
	top := fits("top")
	if !strings.Contains(top, "expose local ports across your tailnet") {
		t.Errorf("top of overlay should show the title:\n%s", top)
	}
	if strings.Contains(top, "saved to:") {
		t.Errorf("config path should be clipped below the fold at the top:\n%s", top)
	}
	if !strings.Contains(top, "more below") {
		t.Errorf("footer should advertise content below at the top:\n%s", top)
	}

	// Jump to the end: the previously-clipped tail is now reachable, and the
	// footer flips to "more above".
	m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyEnd})
	if m.helpScroll != m.helpMaxScroll() {
		t.Errorf("End should scroll to the bottom (%d); got %d", m.helpMaxScroll(), m.helpScroll)
	}
	end := fits("end")
	if !strings.Contains(end, "saved to:") {
		t.Errorf("End should reveal the config path clipped at the top:\n%s", end)
	}
	if !strings.Contains(end, "more above") {
		t.Errorf("footer should advertise content above at the end:\n%s", end)
	}

	// Paging past the bottom clamps (no runaway offset), and Home returns to 0.
	m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyPgDown})
	if m.helpScroll != m.helpMaxScroll() {
		t.Errorf("scrolling past the end should clamp to maxScroll %d; got %d", m.helpMaxScroll(), m.helpScroll)
	}
	m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyHome})
	if m.helpScroll != 0 {
		t.Errorf("Home should scroll back to the top; got %d", m.helpScroll)
	}

	// One line down then up returns to the top and clamps (never negative).
	m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if m.helpScroll != 1 {
		t.Errorf("Down should advance one line; got %d", m.helpScroll)
	}
	m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyUp})
	m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyUp})
	if m.helpScroll != 0 {
		t.Errorf("scrolling up past the top should clamp at 0; got %d", m.helpScroll)
	}

	// Closing resets the offset so a reopen starts at the top.
	m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyEnd})
	m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.showHelp {
		t.Fatal("esc should close the overlay")
	}
	if m.helpScroll != 0 {
		t.Errorf("closing should reset helpScroll to 0; got %d", m.helpScroll)
	}
}

// TestOperatorHintBanner covers kata tapv's persistent, actionable hint. A
// serve/funnel failure classified as tsserve.ErrOperatorNotSet raises the
// STICKY banner -- a deliberate exception to the auto-dismissing toast (see
// TestErrorToasts case 6 for the ordinary-error contrast) -- carrying the
// $USER-expanded fix command, and it survives a keypress and a flash-expiry
// tick that would clear an ordinary toast. It clears only on genuine
// resolution: a subsequent successful toggle, or a re-check
// (detectOperatorMsg, as triggered by "r") confirming the operator is now
// set; an INCONCLUSIVE re-check must leave it standing.
func TestOperatorHintBanner(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	base := New(config.Config{})
	base.operatorUser = "alice"
	base.width, base.height = 100, 24

	// (1) A serve failure classified as ErrOperatorNotSet raises the sticky
	// banner, not the auto-dismissing toast.
	m := mustUpdate(t, base, toggleDoneMsg{port: 8080, err: tsserve.ErrOperatorNotSet})
	if !m.operatorNotSet {
		t.Fatal("toggleDoneMsg{err: ErrOperatorNotSet} should set m.operatorNotSet")
	}
	if m.flash != "" {
		t.Errorf("ErrOperatorNotSet should NOT raise the transient toast; flash = %q", m.flash)
	}
	const wantCmd = "sudo tailscale set --operator=alice"
	view := stripANSI(m.View())
	if !strings.Contains(view, wantCmd) {
		t.Errorf("View() missing the $USER-expanded fix command %q; got:\n%s", wantCmd, view)
	}
	if !strings.Contains(view, "press r") {
		t.Errorf("View() should mention pressing r to re-check; got:\n%s", view)
	}

	// (2) It does NOT auto-dismiss: an ordinary keypress -- which clears a
	// transient toast via the tea.KeyMsg case's unconditional m.flash reset
	// -- must leave the sticky banner standing, and so must a flash-expiry
	// tick.
	m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if !m.operatorNotSet {
		t.Error("a keypress must not dismiss the sticky operator banner")
	}
	if !strings.Contains(stripANSI(m.View()), wantCmd) {
		t.Error("the sticky operator banner should still render after a keypress")
	}
	m = mustUpdate(t, m, flashExpireMsg{id: m.flashID})
	if !m.operatorNotSet {
		t.Error("a flashExpireMsg tick must not dismiss the sticky operator banner")
	}

	// (3) Resolution path A: the next successful toggle clears it immediately.
	resolved := mustUpdate(t, m, toggleDoneMsg{port: 8080, err: nil})
	if resolved.operatorNotSet {
		t.Error("a successful toggle should clear the sticky operator banner")
	}
	if strings.Contains(stripANSI(resolved.View()), wantCmd) {
		t.Error("View() should no longer show the operator banner after a successful toggle")
	}

	// (4) Resolution path B: a re-check (as triggered by "r") that
	// CONFIRMS the operator is now set also clears it.
	rechecked := mustUpdate(t, m, detectOperatorMsg{notSet: false, ok: true})
	if rechecked.operatorNotSet {
		t.Error("a confirmed-fine re-check (detectOperatorMsg ok=true, notSet=false) should clear the banner")
	}

	// (5) An INCONCLUSIVE re-check (older tailscale, no `debug prefs`, etc.)
	// must leave the banner exactly as it was -- not guess "fine".
	inconclusive := mustUpdate(t, m, detectOperatorMsg{notSet: false, ok: false})
	if !inconclusive.operatorNotSet {
		t.Error("an inconclusive re-check (ok=false) must not clear the sticky operator banner")
	}

	// (6) "r" itself batches both refresh and detectOperator, so fixing the
	// operator then pressing r re-checks without needing another failed
	// serve attempt first.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if cmd == nil {
		t.Fatal("'r' should return a non-nil batched command")
	}
}

// TestOperatorHintSizingNoClip mirrors TestLegendSizingNoClip for the sticky
// operator banner (kata tapv): WindowSizeMsg must reserve the banner's
// worst-case height UNCONDITIONALLY, exactly like legendLines'
// cleanEnabled=true reservation, because the banner can appear
// asynchronously (a failed toggle's toggleDoneMsg, or the startup
// detectOperatorMsg) with no fresh resize in between -- including when it
// turns on AFTER the last resize, which the worst-case reservation must
// already cover.
func TestOperatorHintSizingNoClip(t *testing.T) {
	build := func(w, h int, bannerAtSize, bannerAtRender bool) model {
		m := New(config.Config{Ports: map[int]config.PortMeta{}})
		m.allPorts = []portscan.Port{
			{Number: 3000, Process: "node"}, {Number: 8080, Process: "srv"},
			{Number: 9000, Process: "api"}, {Number: 5173, Process: "vite"},
		}
		m.showAllPorts = true
		m.rebuildItems()
		m.operatorNotSet = bannerAtSize
		r, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
		m = r.(model)
		m.operatorNotSet = bannerAtRender
		return m
	}

	for _, tc := range []struct {
		name                         string
		w, h                         int
		bannerAtSize, bannerAtRender bool
	}{
		{"wide/no-banner", 100, 24, false, false},
		{"wide/banner", 100, 24, true, true},
		{"wide/banner-appears-after-resize", 100, 24, false, true},
		{"narrow/no-banner", 58, 24, false, false},
		{"narrow/banner", 58, 24, true, true},
		{"narrow/banner-appears-after-resize", 58, 24, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := build(tc.w, tc.h, tc.bannerAtSize, tc.bannerAtRender)
			view := m.View()
			if got := lipgloss.Height(view); got > m.height {
				t.Errorf("View height %d > terminal height %d (clip/overlap):\n%s", got, m.height, stripANSI(view))
			}
			plain := stripANSI(view)
			if !strings.Contains(plain, "q quit") {
				t.Errorf("bottom bar clipped: %q not found in View:\n%s", "q quit", plain)
			}
		})
	}
}

// --- 9gys: multi-column grid layout ---------------------------------------

// TestGridCols pins gridCols' width->column-count table: roughly one column
// per minColWidth cells, clamped to maxCols, never less than 1. See gridCols'
// own doc comment for the boundary caveat TestGridColWidth below covers.
func TestGridCols(t *testing.T) {
	cases := []struct{ width, want int }{
		{60, 1}, {99, 1}, {100, 2}, {149, 2}, {150, 3}, {300, 3}, {10, 1},
	}
	for _, tc := range cases {
		if got := gridCols(tc.width); got != tc.want {
			t.Errorf("gridCols(%d) = %d, want %d", tc.width, got, tc.want)
		}
	}
}

// TestGridColWidth pins the exact per-column-width formula, and separately
// checks the minColWidth floor holds once a width is comfortably inside a
// column tier (not immediately at the tier's own transition point -- see
// gridCols' doc comment: gridCols(100)==2 and gridCols(150)==3 are pinned
// exact test values that put the resulting cell 1-2 cells UNDER minColWidth
// right at those two boundaries, which a stricter gridCols could avoid only
// by changing those two pinned outputs).
func TestGridColWidth(t *testing.T) {
	if got := gridColWidth(150, 3); got != 48 {
		t.Errorf("gridColWidth(150, 3) = %d, want 48 ((150-2*2)/3)", got)
	}
	if got := gridColWidth(80, 1); got != 80 {
		t.Errorf("gridColWidth(80, 1) = %d, want 80 (single column, no gutter)", got)
	}
	// Right at the tier boundary, the naive split can undershoot -- accepted,
	// see gridCols' doc comment.
	if got := gridColWidth(100, gridCols(100)); got != 49 {
		t.Errorf("gridColWidth(100, %d) = %d, want 49 (documented under minColWidth at this exact boundary)", gridCols(100), got)
	}
	// A few columns past the boundary, the floor holds again.
	for _, w := range []int{102, 154, 200, 300} {
		cols := gridCols(w)
		if cw := gridColWidth(w, cols); cols >= 2 && cw < minColWidth {
			t.Errorf("gridColWidth(%d, %d) = %d, want >= minColWidth (%d)", w, cols, cw, minColWidth)
		}
	}
}

// TestGridRows pins gridRows' body-height -> row-count formula: r rows of
// itemHeight with (r-1) spacing gaps fit in h.
func TestGridRows(t *testing.T) {
	cases := []struct{ h, itemHeight, spacing, want int }{
		{30, 2, 1, 10},
		{2, 2, 1, 1},
		{5, 2, 1, 2},
	}
	for _, tc := range cases {
		if got := gridRows(tc.h, tc.itemHeight, tc.spacing); got != tc.want {
			t.Errorf("gridRows(%d, %d, %d) = %d, want %d", tc.h, tc.itemHeight, tc.spacing, got, tc.want)
		}
	}
}

// TestGridColumnMajorPlacement covers gridPlacement, the pure helper
// renderGrid uses to fan a page's items out column-major: a column fills
// top-to-bottom before the next one starts.
func TestGridColumnMajorPlacement(t *testing.T) {
	const rows = 4
	cases := []struct{ k, wantCol, wantRow int }{
		{0, 0, 0}, {1, 0, 1}, {2, 0, 2}, {3, 0, 3},
		{4, 1, 0}, {7, 1, 3},
		{8, 2, 0}, {11, 2, 3},
	}
	for _, tc := range cases {
		col, row := gridPlacement(tc.k, rows)
		if col != tc.wantCol || row != tc.wantRow {
			t.Errorf("gridPlacement(%d, %d) = (%d, %d), want (%d, %d)", tc.k, rows, col, row, tc.wantCol, tc.wantRow)
		}
	}
}

// TestAvailableDescriptionWidthPerColumn covers the fix availableDescriptionWidth
// needed for 9gys: the inline "✓ copied" fit check must budget against the
// per-column width in a multi-column layout, not the full terminal width, or
// copyURL would think a suffix fits when the column it actually renders into
// is much narrower. At a width that stays single-column it's unchanged from
// before 9gys (colWidth == width).
func TestAvailableDescriptionWidthPerColumn(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	pad := descTruncateStyle.GetPaddingLeft() + descTruncateStyle.GetPaddingRight()

	narrow := New(config.Config{})
	narrow.width = 80
	if cols, _, _ := narrow.gridDims(); cols != 1 {
		t.Fatalf("width 80 should stay single-column, got %d cols", cols)
	}
	if got, want := narrow.availableDescriptionWidth(), 80-pad; got != want {
		t.Errorf("width 80 (1 col) availableDescriptionWidth = %d, want %d (== width-pad)", got, want)
	}

	wide := New(config.Config{})
	wide.width = 150
	cols, _, colWidth := wide.gridDims()
	if cols != 3 {
		t.Fatalf("width 150 should choose 3 columns, got %d", cols)
	}
	if got, want := wide.availableDescriptionWidth(), colWidth-pad; got != want {
		t.Errorf("width 150 (3 cols) availableDescriptionWidth = %d, want %d (== colWidth-pad)", got, want)
	}
	if got, narrowGot := wide.availableDescriptionWidth(), narrow.availableDescriptionWidth(); got >= narrowGot {
		t.Errorf("3-column availableDescriptionWidth (%d) should be MUCH less than 1-column (%d)", got, narrowGot)
	}
}

// TestGridNavLeftRight covers the ONE genuinely new nav move the grid needs:
// Left/Right jump exactly one column over (±rows), same row -- intercepted
// before m.list.Update so bubbles/list's own left/right-bound
// PrevPage/NextPage default keys don't also fire. Down still flows through
// to m.list.Update unmodified and must still advance Index() by exactly 1
// (column-major fill means a linear +1 already walks down a column and
// wraps to the next column's top, so Down needs no special-casing -- see
// gridDims' doc comment on why Index()/Select() stay accurate regardless of
// the list's own internal PerPage).
func TestGridNavLeftRight(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := New(config.Config{})
	var ports []portscan.Port
	for i := 0; i < 12; i++ {
		ports = append(ports, portscan.Port{Number: 3000 + i, Process: fmt.Sprintf("p%d", i)})
	}
	m.allPorts = ports
	m.showAllPorts = true
	m.rebuildItems()

	res, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = res.(model)

	cols, rows, _ := m.gridDims()
	if cols != 2 {
		t.Fatalf("width 120 should choose 2 columns, got %d", cols)
	}
	if rows < 2 {
		t.Fatalf("need at least 2 rows per column for this test to be meaningful, got %d rows", rows)
	}

	m.list.Select(0)

	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = res.(model)
	if got := m.list.Index(); got != rows {
		t.Errorf("after Right from index 0, Index() = %d, want %d (rows)", got, rows)
	}

	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = res.(model)
	if got := m.list.Index(); got != 0 {
		t.Errorf("after Left back, Index() = %d, want 0", got)
	}

	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = res.(model)
	if got := m.list.Index(); got != 1 {
		t.Errorf("after Down from index 0, Index() = %d, want 1 (native list handling, untouched by 9gys)", got)
	}
}

// TestRenderGridNoOverflow covers the width-containment requirement: no
// rendered grid line ever exceeds the terminal width, at a width wide enough
// to pick the max column count.
func TestRenderGridNoOverflow(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := New(config.Config{})
	var ports []portscan.Port
	for i := 0; i < 20; i++ {
		ports = append(ports, portscan.Port{Number: 3000 + i, Process: fmt.Sprintf("proc%d", i)})
	}
	m.allPorts = ports
	m.showAllPorts = true
	m.rebuildItems()

	res, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 24})
	m = res.(model)

	for _, ln := range strings.Split(m.renderGrid(), "\n") {
		if w := lipgloss.Width(ln); w > m.width {
			t.Errorf("renderGrid line exceeds terminal width %d (got %d): %q", m.width, w, stripANSI(ln))
		}
	}
}

// TestRenderGridPageIndicator covers the "more below/next page" affordance
// renderGrid restores now that it no longer uses bubbles/list's own
// paginator: a compact "page N/M" line appears exactly when there's more
// than one page, and moving selection to a later page changes it.
func TestRenderGridPageIndicator(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := New(config.Config{})
	var ports []portscan.Port
	for i := 0; i < 40; i++ {
		ports = append(ports, portscan.Port{Number: 3000 + i, Process: fmt.Sprintf("proc%d", i)})
	}
	m.allPorts = ports
	m.showAllPorts = true
	m.rebuildItems()

	res, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 24})
	m = res.(model)

	cols, rows, _ := m.gridDims()
	perPage := cols * rows
	if perPage >= 40 {
		t.Skip("terminal fits all 40 ports on one page; nothing to page through here")
	}

	grid := stripANSI(m.renderGrid())
	if !strings.Contains(grid, "page 1/") {
		t.Errorf("expected a page indicator on a multi-page grid, got:\n%s", grid)
	}

	m.list.Select(len(m.list.VisibleItems()) - 1)
	grid = stripANSI(m.renderGrid())
	if strings.Contains(grid, "page 1/") {
		t.Errorf("selecting the last item should move off page 1, got:\n%s", grid)
	}
}
