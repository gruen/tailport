package ui

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/gruen/tailport/internal/config"
	"github.com/gruen/tailport/internal/portscan"
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
// port. A labeled-but-not-favorited port appears only in All ports.
func TestRebuildItemsViews(t *testing.T) {
	cfg := config.Config{Ports: map[int]config.PortMeta{
		3000: {Favorite: true},
		8080: {Favorite: true, Label: "web"},
		4000: {Favorite: true}, // favorite but not listening -> synthetic entry
		5000: {Label: "api"},   // labeled, not favorited -> All ports only
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
	if got, want := portNumbers(m), []int{3000, 8080, 9000}; !reflect.DeepEqual(got, want) {
		t.Errorf("all ports view = %v, want %v", got, want)
	}
}

// TestRequestToggle covers the toggle gate (weyy): the lock guard blocks
// turning a locked port on, port :22 defers to a y/n confirm in BOTH
// directions (off is what drops SSH), and every other port toggles now.
func TestRequestToggle(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // isolate any config.Save

	// Locked :22 turn-on: lock guard fires -- no confirm, no toggle.
	m := New(config.Config{Ports: map[int]config.PortMeta{22: {Locked: true}}})
	if cmd := m.requestToggle(22, true); cmd != nil {
		t.Error("locked :22 turn-on should not return a toggle cmd")
	}
	if m.err == nil {
		t.Error("locked :22 turn-on should set an error")
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
	if help := m.helpView(); !strings.Contains(help, "Filter by port number") {
		t.Errorf("help overlay should describe '/' filter; got %q", help)
	}
}

// TestStatusText covers k4ph's multi-state breakdown: listening (portscan
// total), exposed on tailnet (served count), and public (funnel count), with
// in-flight operation messages taking precedence and narrow terminals
// degrading gracefully.
func TestStatusText(t *testing.T) {
	base := model{
		host:     "host",
		width:    120,
		allPorts: []portscan.Port{{Number: 22}, {Number: 3000}, {Number: 8080}, {Number: 9000}, {Number: 5000}},
		active:   map[int]bool{8080: true, 3000: true},
		funnel:   map[int]int{9000: 443},
	}

	got := base.statusText()
	// The host attaches to the listening segment (20w6), not a trailing "— host".
	for _, want := range []string{"5 listening on host", "2 exposed on tailnet", "1 public"} {
		if !strings.Contains(got, want) {
			t.Errorf("statusText = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "— host") {
		t.Errorf("statusText should not use the trailing '— host' form; got %q", got)
	}

	// Zero of everything reads cleanly, not "no ports"; no host -> no "on".
	empty := model{host: "", width: 120}
	if got := empty.statusText(); got != "0 listening · 0 exposed on tailnet · 0 public" {
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

	// :22 hard-block: no funnel cmd, error set, no confirm opened.
	m := New(config.Config{Ports: map[int]config.PortMeta{}})
	if cmd := m.requestFunnel(22); cmd != nil {
		t.Error("funnel :22 should be refused (nil cmd)")
	}
	if m.err == nil {
		t.Error("funnel :22 should set an error")
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

	// All three ingress ports taken: a 4th funnel is refused.
	m = New(config.Config{Ports: map[int]config.PortMeta{}})
	m.funnel = map[int]int{3000: 443, 3001: 8443, 3002: 10000}
	if cmd := m.requestFunnel(9999); cmd != nil {
		t.Error("a 4th funnel should be refused (nil cmd)")
	}
	if m.err == nil || m.mode != entryNone {
		t.Errorf("4th funnel should error without a confirm; err=%v mode=%v", m.err, m.mode)
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
// ◉ marker and a public HTTPS URL in the description, both overriding the
// tailnet-serve presentation even when the port is also served.
func TestFunnelItemRender(t *testing.T) {
	it := portItem{
		port:         portscan.Port{Number: 3000, Process: "node"},
		active:       true, // also served on the tailnet ...
		listening:    true,
		host:         "host",
		fqdn:         "host.example.ts.net",
		funnelPublic: 8443, // ... but funnel outranks it
	}
	if got := it.Title(); !strings.Contains(got, "◉") {
		t.Errorf("funnelled Title should carry the ◉ marker; got %q", got)
	}
	desc := it.Description()
	if !strings.Contains(desc, "https://host.example.ts.net:8443") {
		t.Errorf("funnelled Description should show the public URL; got %q", desc)
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
	if bottom := m.renderBottom(); strings.Contains(bottom, "Favorites") || strings.Contains(bottom, "All ports") {
		t.Errorf("bottom bar should not contain the view toggle; got %q", bottom)
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
