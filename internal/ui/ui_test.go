package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"golang.org/x/crypto/bcrypt"

	"github.com/gruen/tailport/internal/caddyedge"
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

// resumePublishDone unpacks the resume cmd a successful purgeDoneMsg returns
// (kata dw57): since the purge success now ALSO fires the fire-and-forget poof
// (batched alongside the resume publish -- see the dw57 seam), cmd() no longer
// yields a bare publishDoneMsg but a tea.BatchMsg carrying both the poof's own
// tick and the resume publish. This finds and returns the publishDoneMsg,
// asserting the batch also carries exactly the poof tick fire-and-forget --
// i.e. that the resume publish is issued UNCHANGED alongside it.
func resumePublishDone(t *testing.T, cmd tea.Cmd) publishDoneMsg {
	t.Helper()
	if cmd == nil {
		t.Fatal("resume cmd is nil, want a batch carrying the resume publish")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("resume cmd = %#v, want a tea.BatchMsg (poof tick + resume publish)", cmd())
	}
	var pubdone publishDoneMsg
	var gotPublish, gotPoofTick bool
	for _, c := range batch {
		switch msg := c().(type) {
		case publishDoneMsg:
			pubdone = msg
			gotPublish = true
		case poofTickMsg:
			gotPoofTick = true
		default:
			t.Fatalf("unexpected msg in resume batch: %#v", msg)
		}
	}
	if !gotPublish {
		t.Fatal("resume batch should contain the resume publish's publishDoneMsg")
	}
	if !gotPoofTick {
		t.Error("resume batch should contain the poof's own fire-and-forget tick")
	}
	return pubdone
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

// TestStatusLineFitsEdgeWarning pins roborev 0k12 #4: the " · edge unreachable"
// health suffix is now sized INTO the width fit (not appended after the variant
// is chosen), so on narrow terminals -- where ordinary status text isn't
// wrapped -- the whole line stays within m.width and the warning is never
// truncated away; it degrades to a shorter "edge down" fragment instead.
func TestStatusLineFitsEdgeWarning(t *testing.T) {
	mk := func(width int) string {
		cfg := config.Config{}
		cfg.Caddy.Domain = "example.com"
		cfg.Caddy.Hostname = "caddy"
		m := New(cfg)
		m.width = width
		m.publishReachable = false // a failed edge poll -> warning suffix present
		return m.statusText()
	}

	// At every width the line stays within m.width AND keeps the warning
	// (possibly shortened) rather than truncating it away.
	for _, w := range []int{120, 60, 40, 30, 24} {
		got := mk(w)
		if lipgloss.Width(got) > w {
			t.Errorf("width %d: status %q width %d overflows", w, got, lipgloss.Width(got))
		}
		if !strings.Contains(got, "edge") {
			t.Errorf("width %d: the edge-health warning was truncated away; got %q", w, got)
		}
	}
	// A comfortable width keeps the full phrasing; a very narrow one uses the
	// shorter fallback rather than dropping the warning.
	if wide := mk(120); !strings.Contains(wide, "edge unreachable") {
		t.Errorf("wide status should carry the full 'edge unreachable'; got %q", wide)
	}
	if narrow := mk(24); !strings.Contains(narrow, "edge down") {
		t.Errorf("narrow status should degrade to 'edge down'; got %q", narrow)
	}
}

// TestStatusLineWarningSurvivesVeryNarrowWidth pins roborev bps9 #2: even
// after 0k12 #4 sized the suffix into the width fit, the compact
// "NL · NT · NP" fallback is chosen unconditionally when nothing wider fits
// -- it is never itself checked against avail -- so on terminals under ~24
// columns, or once the counts go multi-digit, "compact base + edge down" can
// still exceed m.width and clip the warning. The health warning must survive
// at any width the terminal actually gives us, so once even that minimum
// combined form doesn't fit, statusText drops the base and shows the warning
// alone (further truncated if even the bare warning can't fit).
func TestStatusLineWarningSurvivesVeryNarrowWidth(t *testing.T) {
	mk := func(width int) string {
		cfg := config.Config{}
		cfg.Caddy.Domain = "example.com"
		cfg.Caddy.Hostname = "caddy"
		m := New(cfg)
		// Double-digit counts on every segment (bps9's "port counts have
		// multiple digits" case) widen the compact fallback ("12L · 11T ·
		// 10P" -- 15 columns) past what fits alongside "· edge down" (12
		// columns) at widths well above the single-digit case's threshold.
		ports := make([]portscan.Port, 12)
		active := map[int]bool{}
		for i := range ports {
			port := 3000 + i
			ports[i] = portscan.Port{Number: port}
			if i < 11 {
				active[port] = true
			}
		}
		m.allPorts = ports
		m.active = active
		m.funnel = map[int]int{4000: 443, 4001: 443, 4002: 443, 4003: 443, 4004: 443,
			4005: 443, 4006: 443, 4007: 443, 4008: 443, 4009: 443}
		m.width = width
		m.publishReachable = false // failed edge poll -> warning suffix present
		return m.statusText()
	}

	for w := 10; w <= 20; w++ {
		got := mk(w)
		if lipgloss.Width(got) > w {
			t.Errorf("width %d: status %q width %d overflows", w, got, lipgloss.Width(got))
		}
		if !strings.Contains(got, "edge") {
			t.Errorf("width %d: the edge-health warning was clipped; got %q", w, got)
		}
	}

	// Even a width too narrow for the bare "edge down" fallback (9 columns)
	// must not panic and must never exceed the given width -- the fragment
	// is truncated from the right rather than dropped entirely.
	if got := mk(6); lipgloss.Width(got) > 6 {
		t.Errorf("width 6: status %q width %d overflows", got, lipgloss.Width(got))
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
	// fwRand is package-level and auto-seeded per process in production (see
	// its declaration), so an unfixed seed made the clamp-bind assertion below
	// seed-dependent: the clamp is a rare event (empirically ~1-in-6000 shots
	// at w=60), so an unlucky auto-seed could draw 20000 shots without ever
	// tripping it. Pin fwRand to a fixed seed for this test's duration
	// (save/restore) for bit-for-bit repeatability, and also widen the sample
	// size well past what a single lucky seed would need, so the property
	// holds for essentially any seed -- not one cherry-picked to pass.
	prevRand := fwRand
	defer func() { fwRand = prevRand }()
	fwRand = rand.New(rand.NewSource(1))
	// Narrow width where the clamp actually binds: the invariant must hold for
	// EVERY sample, including the tallest (largest tExp) shots.
	const narrowSamples = 200000 // >>20000: makes a zero-clamp draw astronomically unlikely
	for _, w := range []int{52, 60} {
		const h = 40
		clamped := false
		for i := 0; i < narrowSamples; i++ {
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

// TestStepPoofAdvancesFrameAndTTL covers the pure step function: frame counts
// up, ttl counts down, one tick at a time.
func TestStepPoofAdvancesFrameAndTTL(t *testing.T) {
	p := poofState{text: "app.example.com → dev-box:8080", ttl: poofTTL, emoji: false}
	next := stepPoof(p)
	if next.frame != 1 {
		t.Errorf("frame = %d, want 1", next.frame)
	}
	if next.ttl != poofTTL-1 {
		t.Errorf("ttl = %d, want %d", next.ttl, poofTTL-1)
	}
	if next.text != p.text || next.emoji != p.emoji {
		t.Error("stepPoof must not touch text/emoji, only frame/ttl")
	}
}

// TestPoofTickerSelfStopsNoLeak mirrors TestFireworkCap's ticker discipline for
// the poof's sibling ticker (kata dw57): startPoof arms exactly one ticker,
// draining poofTickMsg advances it to completion, and once ttl is exhausted
// m.poof is cleared and poofTicking goes false with NO reschedule -- an idle
// tick afterward is a no-op, never a busy loop.
func TestPoofTickerSelfStopsNoLeak(t *testing.T) {
	m := New(config.Config{})
	m.width, m.height = 100, 40

	cmd := m.startPoof("app.example.com → dev-box:8080")
	if m.poof == nil {
		t.Fatal("startPoof should set m.poof")
	}
	if !m.poofTicking || cmd == nil {
		t.Fatalf("startPoof should start the ticker: poofTicking=%v cmd=%v", m.poofTicking, cmd != nil)
	}

	guard := 0
	for m.poof != nil {
		res, _ := m.Update(poofTickMsg{})
		m = res.(model)
		if guard++; guard > poofTTL+5 {
			t.Fatal("poof never drained under the tick")
		}
	}
	if m.poofTicking {
		t.Error("poofTicking should be false once the poof completes")
	}

	// An idle tick afterward (e.g. a stray already-scheduled tick landing after
	// completion) must not reschedule and must not resurrect m.poof.
	res, cmd := m.Update(poofTickMsg{})
	m = res.(model)
	if cmd != nil {
		t.Error("an idle poof tick must not reschedule (no busy loop)")
	}
	if m.poof != nil || m.poofTicking {
		t.Error("an idle poof tick must not resurrect m.poof or poofTicking")
	}
}

// TestPoofDoesNotStackTickers covers the no-stacking guard: a second trigger
// while a poof is already in flight (e.g. two purges in quick succession)
// replaces the descriptor but must NOT start a second ticker (mirrors the 'f'
// key's fwTicking guard under repeated presses).
func TestPoofDoesNotStackTickers(t *testing.T) {
	m := New(config.Config{})
	m.width, m.height = 100, 40

	first := m.startPoof("a.example.com → dev-box:8080")
	if first == nil || !m.poofTicking {
		t.Fatalf("first trigger should start the ticker: cmd=%v poofTicking=%v", first != nil, m.poofTicking)
	}

	second := m.startPoof("b.example.com → dev-box:9090")
	if second != nil {
		t.Error("a second trigger while already ticking must not start a second ticker")
	}
	if m.poof == nil || m.poof.text != "b.example.com → dev-box:9090" {
		t.Errorf("a second trigger should still replace the descriptor; got %#v", m.poof)
	}
	if m.poof.frame != 0 || m.poof.ttl != poofTTL {
		t.Errorf("a replaced poof should restart at frame 0/full ttl; got frame=%d ttl=%d", m.poof.frame, m.poof.ttl)
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
		{"space", "p", "P", "C", "x"},
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

	// Copy sits directly under "n new favorite": same column, next row down.
	nRow, nCol := at("n new favorite")
	cRow, cCol := at("c copy URL")
	if cCol != nCol || cRow != nRow+1 {
		t.Errorf("c copy URL should be the row directly under n new favorite; n at (%d,%d), c at (%d,%d)", nRow, nCol, cRow, cCol)
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
	if r := lineOf(wideLines, "n new favorite"); r < 0 {
		t.Errorf("n new favorite missing from wide grid:\n%s", wide)
	} else if strings.Contains(wideLines[r], "l label") {
		t.Errorf("n new favorite's row should have an empty second sub-col (only 5 items, top-heavy 3/2 split): %q", wideLines[r])
	}

	// Expose folded too (4 bindings since kata v1z5 added P publish edge, so
	// it's now taller than View's 3 and tried right after Favorites). Its 2/2
	// top-heavy split puts space serve | P publish edge on one row and
	// p funnel public | x lock/unlock on the next.
	if r1, r2 := lineOf(wideLines, "space serve"), lineOf(wideLines, "P publish edge"); r1 < 0 || r1 != r2 {
		t.Errorf("Expose should fold space serve/P publish edge onto the same row; space serve row %d, P publish edge row %d:\n%s", r1, r2, wide)
	}
	if r1, r2 := lineOf(wideLines, "funnel public"), lineOf(wideLines, "x lock/unlock"); r1 < 0 || r1 != r2 {
		t.Errorf("Expose should fold p funnel public/x lock/unlock onto the same row; p funnel public row %d, x lock/unlock row %d:\n%s", r1, r2, wide)
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
		"f favorite", "F forget", "n new favorite", "c copy URL", "l label",
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

	// A short sub-col-1 cell doesn't shift sub-col 2 left: "n new favorite" is
	// sub-col 1's widest row (14 wide, == subWidth[0]), so sub-col 2's fixed
	// edge must sit exactly one sub-column gap past where that row's content
	// ends -- not past the shorter "f favorite"/"u unfavorite" rows' content.
	nEnd := col("n new favorite") + len("n new favorite")
	if want := nEnd + legendSubColGap; cCol != want {
		t.Errorf("Favorites sub-col 2 should start at %d (widest sub-col-1 row %q ends at %d, + %d-wide gap); got %d:\n%s", want, "n new favorite", nEnd, legendSubColGap, cCol, grid)
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
		"f favorite", "F forget", "n new favorite", "l label",
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
// "x lock/unlock" (space/p/P/x, no clean, no reserved blank slot); when a
// dangling forward exists it gains "C clean stale" -- inserted just ABOVE lock
// so "x lock/unlock" stays the last item in the column in either state.
// (kata v1z5 added P publish edge to Expose, after p funnel.)
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

	// No dangling -> Expose is space/p/P/x (clean dropped), lock last, and the
	// rendered bar omits "clean".
	noClean := expose(m.barGroups(false))
	if got := len(noClean.bindings); got != 4 {
		t.Errorf("Expose should be 4 bindings (space/p/P/x) with no dangling; got %d", got)
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
	if got := len(withClean.bindings); got != 5 {
		t.Errorf("Expose should be 5 bindings (space/p/P/C/x) with a dangling; got %d", got)
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
// width- and fold-dependent, and folding is keyed off each group's UNFOLDED
// binding count, which differs between the two cleanEnabled calls (Expose: 5
// bindings reserved-with-clean vs 4 live-no-clean since kata v1z5 added P
// publish edge). That difference USED to net out to an incidental tie, but P
// broke it: at some widths the no-clean render is actually TALLER than the
// with-clean one (folding is non-monotonic -- one more binding can push a
// group over a fold threshold and SHRINK it). So the reservation no longer
// assumes cleanEnabled=true dominates: listBodyHeight reserves the MAX of both
// states (legendReservationLines). This test pins that the reservation the
// code actually uses is never shorter than EITHER live render, brute-forcing
// width rather than sampling a few. (79xb: this brute-force scan is also what
// caught the second-hint "space serve" contextual polish breaking the
// reservation at width 54 -- see the TODO(79xb) on renderLegendWith.)
func TestLegendReservationDominatesLive(t *testing.T) {
	m := New(config.Config{})
	for w := 1; w <= 300; w++ {
		m.help.Width = w
		reserved := m.legendReservationLines()
		for _, cleanEnabled := range []bool{true, false} {
			live := strings.Count(m.renderLegendWith(cleanEnabled), "\n") + 1
			if reserved < live {
				t.Fatalf("width %d: reserved height %d < live height %d (cleanEnabled=%v) -- the reservation no longer dominates, WindowSizeMsg would under-reserve and clip the list", w, reserved, live, cleanEnabled)
			}
		}
	}
}

// TestRenderStatusLineShowsPoof covers dw57's status-slot render: an active
// m.poof renders through renderStatusLine (never a list-row animation, never
// a separate banner), never exceeds m.width (renderPoofLine truncates rather
// than wraps, so there's no horizontal scroll), and self-reserves through the
// SAME live statusLines measurement listBodyHeight already uses for m.flash
// -- so the list body height never drops below 1 while a poof is animating,
// even on a short terminal.
func TestRenderStatusLineShowsPoof(t *testing.T) {
	m := New(config.Config{})
	m.active = map[int]bool{}
	m.width, m.height = 80, 24
	m.rebuildItems()

	longDescriptor := "a-very-long-hostname.internal.example.com → some-machine-label:65535"
	for _, width := range []int{10, 40, 80, 120} {
		m.width = width
		m.poof = &poofState{text: longDescriptor, ttl: poofTTL, emoji: false}
		m.resizeList()

		out := m.renderStatusLine()
		if lipgloss.Height(out) != 1 {
			t.Errorf("width %d: poof status line height = %d, want exactly 1 (no wrap)", width, lipgloss.Height(out))
		}
		if w := lipgloss.Width(out); w > width {
			t.Errorf("width %d: poof status line width = %d, exceeds the terminal", width, w)
		}
		if h := m.listBodyHeight(); h < 1 {
			t.Errorf("width %d: listBodyHeight = %d with an active poof, want >=1 (no clip)", width, h)
		}
	}

	// A short terminal (small m.height) is the tightest case for the "never
	// clips below 1" floor.
	m.width, m.height = 80, 6
	m.poof = &poofState{text: longDescriptor, ttl: poofTTL, emoji: false}
	m.resizeList()
	if h := m.listBodyHeight(); h < 1 {
		t.Errorf("short terminal: listBodyHeight = %d with an active poof, want >=1 (no clip)", h)
	}
}

// TestRenderStatusLinePoofFlashPrecedence pins the documented precedence in
// renderStatusLine (dw57 §3.5: "keep it simple and documented"): when both a
// flash and a poof are active, the flash wins the slot; the poof itself is
// untouched by this (it keeps ticking underneath, just isn't drawn).
func TestRenderStatusLinePoofFlashPrecedence(t *testing.T) {
	// Corrected precedence (roborev 65qc-#1): the poof outranks an INFO flash
	// (so the resumed take-over's "took over" toast, which lands within the
	// poof's life, no longer hides the whole animation), but a WARN/ERROR flash
	// still preempts the poof (the user must see a failure).
	t.Run("poof outranks an INFO flash", func(t *testing.T) {
		m := New(config.Config{})
		m.width = 80
		m.poof = &poofState{text: "app.example.com → host-b:3000", ttl: poofTTL, emoji: false}
		m.flash = "took over app.example.com"
		m.flashLevel = flashInfo

		out := stripANSI(m.renderStatusLine())
		// The poof dissolves characters as it animates, so the exact text isn't
		// stable; the load-bearing check is that the INFO flash did NOT preempt
		// it. ("host-b" — the descriptor tail — is intact at frame 0 too.)
		if strings.Contains(out, "took over") {
			t.Errorf("renderStatusLine = %q, an info flash must not preempt the poof", out)
		}
		if !strings.Contains(out, "host-b") {
			t.Errorf("renderStatusLine = %q, want the poof (dissolving the deleted route) to show", out)
		}
	})

	t.Run("ERROR flash preempts the poof", func(t *testing.T) {
		m := New(config.Config{})
		m.width = 80
		m.poof = &poofState{text: "app.example.com → host-b:3000", ttl: poofTTL, emoji: false}
		m.flash = "purged the old route, but the take-over failed"
		m.flashLevel = flashError

		out := stripANSI(m.renderStatusLine())
		if !strings.Contains(out, "take-over failed") {
			t.Errorf("renderStatusLine = %q, want the error flash to preempt the poof", out)
		}
		if strings.Contains(out, "host-b") {
			t.Errorf("renderStatusLine = %q, the poof must not show while an error flash is active", out)
		}
	})
}

// TestPurgeDescOf pins the descriptor logic (roborev 65qc-#2): a parseable
// backend renders label:port; a non-proxy route falls back to its handler.
func TestPurgeDescOf(t *testing.T) {
	if got := purgeDescOf(caddyedge.PurgeExpect{BackendParseable: true, Label: "host-b", Port: 3000}); got != "host-b:3000" {
		t.Errorf("purgeDescOf(parseable) = %q, want host-b:3000", got)
	}
	if got := purgeDescOf(caddyedge.PurgeExpect{BackendParseable: false, Handler: "static_response"}); got != "static_response" {
		t.Errorf("purgeDescOf(non-proxy) = %q, want static_response", got)
	}
}

// TestPoofShowsDeletedBackendNotTakeover covers roborev 65qc-#2 end-to-end: on a
// purge success the poof must animate the DELETED route's backend (carried on
// purgeDoneMsg.deletedDesc), not this machine's take-over backend.
func TestPoofShowsDeletedBackendNotTakeover(t *testing.T) {
	m := New(config.Config{})
	m.width = 80
	m.fqdn = "dev-box.tailnet.ts.net"
	// Our take-over will republish app.example.com to dev-box:4000...
	m.pendingPublish = pendingPublish{hostname: "app.example.com", label: "dev-box", port: 4000}
	// ...but the route we purged pointed at host-b:3000.
	msg := purgeDoneMsg{
		hostname:    "app.example.com",
		port:        4000,
		deletedDesc: "host-b:3000",
		captured:    caddyedge.Captured{Hostname: "app.example.com", HadID: true},
	}
	updated, _ := m.Update(msg)
	m2 := updated.(model)
	if m2.poof == nil {
		t.Fatal("expected a poof to start on purge success")
	}
	if !strings.Contains(m2.poof.text, "host-b:3000") {
		t.Errorf("poof text = %q, want the DELETED backend host-b:3000", m2.poof.text)
	}
	if strings.Contains(m2.poof.text, "dev-box") {
		t.Errorf("poof text = %q, must NOT show the take-over backend dev-box", m2.poof.text)
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

// ---------------------------------------------------------------------------
// Publish (`P`) path -- kata v1z5 steps 3/4/5. These are the primary
// verification for w7k4: bubbletea Model.Update walks driving the whole dialog
// state machine, the requestPublish guards in order, funnel<->publish mutual
// exclusion in both directions, drift surfaced-not-ranked, the quiet poll
// degrade, and the edge round-trips against an in-process httptest fake (no
// real caddy, no bound port). `caddy` is intentionally NOT installed here, so
// the live tmux-against-real-caddy walk is deliberately out of scope.
// ---------------------------------------------------------------------------

// fakeCaddy is a minimal in-process stand-in for the Caddy admin API: enough of
// GET/POST/PATCH/DELETE on /id/<id> and /config/.../routes to drive the UI's
// publish/unpublish/poll flows. It stores routes by @id and records mutations
// for assertions. It ignores If-Match (never 412) -- the ETag/concurrency paths
// are caddyedge's own tests, not the UI's.
type fakeCaddy struct {
	mu        sync.Mutex
	routes    map[string]caddyedge.Route
	mutations []caddyedge.Route // POST/PATCH bodies, in order
	deletes   []string          // @ids DELETEd, in order
	// failNextPost, when true, makes the NEXT POST commit the route server-side and
	// then drop the connection so the client sees a transport error -- a lost-
	// response step-B commit (kata 7jy2 FIX 1). Consumed (reset) after it fires.
	failNextPost bool
	// failNextRoutesGet, when true, drops the connection on the NEXT GET /routes so
	// the client sees a transport error -- used to fail the restore preflight's
	// VerifyRestore read (roborev nk3b-#1). Consumed after it fires.
	failNextRoutesGet bool
}

func newFakeCaddy() *fakeCaddy {
	return &fakeCaddy{routes: map[string]caddyedge.Route{}}
}

func (fc *fakeCaddy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	w.Header().Set("Etag", `"v1"`)
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/id/"):
		id := strings.TrimPrefix(path, "/id/")
		switch r.Method {
		case http.MethodGet:
			rt, ok := fc.routes[id]
			if !ok {
				http.Error(w, "unknown object", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(rt)
		case http.MethodPatch:
			var rt caddyedge.Route
			_ = json.NewDecoder(r.Body).Decode(&rt)
			fc.routes[id] = rt
			fc.mutations = append(fc.mutations, rt)
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			delete(fc.routes, id)
			fc.deletes = append(fc.deletes, id)
			w.WriteHeader(http.StatusOK)
		}
	case strings.HasSuffix(path, "/routes"):
		switch r.Method {
		case http.MethodGet:
			if fc.failNextRoutesGet {
				fc.failNextRoutesGet = false
				if hj, ok := w.(http.Hijacker); ok {
					if conn, _, err := hj.Hijack(); err == nil {
						_ = conn.Close()
						return
					}
				}
			}
			out := make([]caddyedge.Route, 0, len(fc.routes))
			for _, rt := range fc.routes {
				out = append(out, rt)
			}
			_ = json.NewEncoder(w).Encode(out)
		case http.MethodPost:
			var rt caddyedge.Route
			_ = json.NewDecoder(r.Body).Decode(&rt)
			fc.routes[rt.ID] = rt
			fc.mutations = append(fc.mutations, rt)
			if fc.failNextPost {
				// The write LANDED above; now drop the connection before writing any
				// response so the client sees a transport error (ErrUnreachable) even
				// though the commit succeeded server-side -- a lost step-B response.
				fc.failNextPost = false
				if hj, ok := w.(http.Hijacker); ok {
					if conn, _, err := hj.Hijack(); err == nil {
						_ = conn.Close()
						return
					}
				}
			}
			w.WriteHeader(http.StatusOK)
		}
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// routeDial returns a route's reverse_proxy backend dial ("<label>:<port>").
func routeDial(rt caddyedge.Route) string {
	for _, h := range rt.Handle {
		if h.Handler == "reverse_proxy" && len(h.Upstreams) > 0 {
			return h.Upstreams[0].Dial
		}
	}
	return ""
}

// routeHasAuth reports whether a route carries an authentication handler.
func routeHasAuth(rt caddyedge.Route) bool {
	for _, h := range rt.Handle {
		if h.Handler == "authentication" {
			return true
		}
	}
	return false
}

var enterKey = tea.KeyMsg{Type: tea.KeyEnter}
var escKey = tea.KeyMsg{Type: tea.KeyEsc}

// rkey builds a rune KeyMsg from a string ("P", "y", "n", or typed text).
func rkey(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// newPublishModel builds a model wired for the publish path: publish
// configured (domain/hostname/server_name/admin_port), a known fqdn (so the
// short backend label is "dev-box"), one listening favorite :8080, and -- when
// srv is non-nil -- a caddyClientOverride pointed at that fake edge. serve is
// already on for :8080 by default so publishEnableServe is false (a publish cmd
// can then run without shelling out to tailscale).
func newPublishModel(t *testing.T, srv *httptest.Server) model {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // isolate saveConfig
	cfg := config.Config{Ports: map[int]config.PortMeta{8080: {Favorite: true}}}
	cfg.Caddy.Domain = "example.com"
	cfg.Caddy.Hostname = "caddy"
	cfg.Caddy.ServerName = "tailport"
	cfg.Caddy.AdminPort = 2019
	m := New(cfg)
	m.fqdn = "dev-box.tailnet.ts.net"
	m.allPorts = []portscan.Port{{Number: 8080, Process: "web"}}
	m.active = map[int]bool{8080: true}
	m.showAllPorts = true
	if srv != nil {
		m.caddyClientOverride = &caddyedge.Client{AdminURL: srv.URL, ServerName: "tailport", HTTPClient: srv.Client()}
	}
	m.rebuildItems()
	return m
}

// TestShortLabel pins the backend-label derivation: the FIRST dot-component of
// the fqdn (from Self.DNSName), not the whole name and not HostName.
func TestShortLabel(t *testing.T) {
	for _, c := range []struct{ fqdn, want string }{
		{"dev-box.tailnet.ts.net", "dev-box"},
		{"host", "host"},
		{"", ""},
	} {
		if got := shortLabel(c.fqdn); got != c.want {
			t.Errorf("shortLabel(%q) = %q, want %q", c.fqdn, got, c.want)
		}
	}
}

// TestPublishFlowWalkNoAuth walks P -> host -> auth(n) -> confirm(y) with no
// auth, asserting every transition and that the confirm arms a publish
// (pending set, back to entryNone) without a credential.
func TestPublishFlowWalkNoAuth(t *testing.T) {
	m := newPublishModel(t, nil)

	m = mustUpdate(t, m, rkey("P"))
	if m.mode != entryPublishHost {
		t.Fatalf("after P, mode = %v, want entryPublishHost", m.mode)
	}
	// Prefill precedence: no label, process "web" -> "web.example.com".
	if got := m.publishInput.Value(); got != "web.example.com" {
		t.Errorf("host prefill = %q, want web.example.com", got)
	}

	m = mustUpdate(t, m, enterKey)
	if m.mode != entryPublishAuth {
		t.Fatalf("after host enter, mode = %v, want entryPublishAuth", m.mode)
	}
	if m.publishHostname != "web.example.com" {
		t.Errorf("publishHostname = %q, want web.example.com", m.publishHostname)
	}

	m = mustUpdate(t, m, rkey("n")) // no auth (distinct from esc/abort)
	if m.mode != entryConfirmPublish {
		t.Fatalf("after auth 'n', mode = %v, want entryConfirmPublish", m.mode)
	}
	if m.publishWithAuth {
		t.Error("auth 'n' should leave publishWithAuth false")
	}
	// serve already on for :8080 -> confirm won't also enable serve.
	if m.publishEnableServe {
		t.Error("publishEnableServe should be false when serve is already on")
	}

	res, cmd := m.Update(rkey("y"))
	got := res.(model)
	if got.mode != entryNone {
		t.Errorf("after confirm, mode = %v, want entryNone", got.mode)
	}
	if got.pending != 8080 {
		t.Errorf("after confirm, pending = %d, want 8080", got.pending)
	}
	if cmd == nil {
		t.Error("confirm should return a publish cmd")
	}
	if got.cfg.Caddy.AuthHash != "" {
		t.Error("a no-auth publish must not set an auth hash")
	}
}

// TestPublishConfirmViewEnablesServe covers the confirm line that warns serve
// will also be turned on when it isn't already, and that the exact https URL
// and public-internet warning are shown.
func TestPublishConfirmViewEnablesServe(t *testing.T) {
	m := newPublishModel(t, nil)
	m.active = map[int]bool{} // serve OFF -> confirm should say it'll turn serve on
	m = mustUpdate(t, m, rkey("P"))
	m = mustUpdate(t, m, enterKey) // accept prefill host
	m = mustUpdate(t, m, rkey("n"))
	if !m.publishEnableServe {
		t.Fatal("publishEnableServe should be true when serve is off")
	}
	view := stripANSI(m.renderBottom())
	for _, want := range []string{
		"https://web.example.com",
		"PUBLIC INTERNET",
		"will also turn tailscale serve on for :8080",
		"no basic auth",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("confirm view missing %q; got:\n%s", want, view)
		}
	}
}

// TestPublishFlowWithAuthPersistsHash walks the first authed publish end to
// end: the password is bcrypt-hashed at confirm-time, persisted to config (so a
// later publish reuses it), the plaintext is dropped, and the password step is
// masked.
func TestPublishFlowWithAuthPersistsHash(t *testing.T) {
	m := newPublishModel(t, nil)

	m = mustUpdate(t, m, rkey("P"))
	m = mustUpdate(t, m, enterKey)  // host -> auth
	m = mustUpdate(t, m, rkey("y")) // auth yes -> cred user (no stored cred yet)
	if m.mode != entryPublishCredUser {
		t.Fatalf("auth 'y' with no stored cred -> mode %v, want entryPublishCredUser", m.mode)
	}
	m = mustUpdate(t, m, rkey("admin"))
	m = mustUpdate(t, m, enterKey) // user -> pass
	if m.mode != entryPublishCredPass {
		t.Fatalf("after username, mode = %v, want entryPublishCredPass", m.mode)
	}
	if m.publishCredUser != "admin" {
		t.Errorf("publishCredUser = %q, want admin", m.publishCredUser)
	}
	if m.publishInput.EchoMode != textinput.EchoPassword {
		t.Error("password step should mask input (EchoPassword)")
	}
	m = mustUpdate(t, m, rkey("s3cr3t"))
	m = mustUpdate(t, m, enterKey) // pass -> confirm
	if m.mode != entryConfirmPublish {
		t.Fatalf("after password, mode = %v, want entryConfirmPublish", m.mode)
	}

	res, _ := m.Update(rkey("y"))
	got := res.(model)
	if got.cfg.Caddy.AuthUser != "admin" {
		t.Errorf("persisted AuthUser = %q, want admin", got.cfg.Caddy.AuthUser)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(got.cfg.Caddy.AuthHash), []byte("s3cr3t")); err != nil {
		t.Errorf("stored AuthHash does not verify against the password: %v", err)
	}
	if got.publishCredPass != "" || got.publishCredUser != "" {
		t.Errorf("plaintext credential not cleared after confirm: user=%q pass=%q", got.publishCredUser, got.publishCredPass)
	}
	// Persisted to disk via the explicit saveConfig (remember() would skip).
	loaded, err := config.Load("")
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if loaded.Caddy.AuthHash != got.cfg.Caddy.AuthHash || loaded.Caddy.AuthUser != "admin" {
		t.Errorf("credential not persisted: loaded user=%q hash set=%v", loaded.Caddy.AuthUser, loaded.Caddy.AuthHash != "")
	}
}

// TestPublishSaveFailureAbortsPublish covers roborev 0k12 #3: when persisting a
// newly-gathered shared credential fails at confirm-time, the publish must be
// ABORTED -- otherwise a live authenticated public route would exist whose
// credential is only in memory and lost on restart. On failure the prior
// in-memory credential is restored (not left half-set), the error is surfaced,
// the flow is cleared, and no publish is armed.
func TestPublishSaveFailureAbortsPublish(t *testing.T) {
	fc := newFakeCaddy()
	srv := httptest.NewServer(fc)
	defer srv.Close()
	m := newPublishModel(t, srv)

	// Force the credential Save to fail: point XDG_CONFIG_HOME at a regular
	// file so Save's MkdirAll(<file>/tailport) errors with ENOTDIR.
	badXDG := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(badXDG, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", badXDG)

	// Walk P -> host -> auth(y) -> user -> pass -> confirm, gathering a NEW
	// shared credential (none stored yet, so confirm must Save).
	m = mustUpdate(t, m, rkey("P"))
	m = mustUpdate(t, m, enterKey)  // host -> auth
	m = mustUpdate(t, m, rkey("y")) // auth yes -> cred user
	m = mustUpdate(t, m, rkey("admin"))
	m = mustUpdate(t, m, enterKey) // user -> pass
	m = mustUpdate(t, m, rkey("s3cr3t"))
	m = mustUpdate(t, m, enterKey) // pass -> confirm
	if m.mode != entryConfirmPublish {
		t.Fatalf("pre-confirm mode = %v, want entryConfirmPublish", m.mode)
	}

	res, _ := m.Update(rkey("y")) // confirm 'y' -> save fails -> abort
	got := res.(model)

	// No publish was armed: pending stays 0 (publishCmd is only reached after a
	// successful save), and the fake edge received no route mutation.
	if got.pending != 0 {
		t.Errorf("a failed credential save must NOT arm a publish; pending=%d", got.pending)
	}
	if len(fc.mutations) != 0 {
		t.Errorf("a failed credential save must NOT publish; got %d edge mutations", len(fc.mutations))
	}
	// The prior (empty) credential is restored, not left half-set from the
	// in-flight assignment.
	if got.cfg.Caddy.AuthHash != "" || got.cfg.Caddy.AuthUser != "" {
		t.Errorf("failed save must restore the prior credential; user=%q hashSet=%v", got.cfg.Caddy.AuthUser, got.cfg.Caddy.AuthHash != "")
	}
	// The error is surfaced and the flow is cleared (plaintext dropped).
	if got.flashLevel != flashError || got.flash == "" {
		t.Errorf("failed save must surface an error toast; flash=%q level=%v", got.flash, got.flashLevel)
	}
	if got.mode != entryNone || got.publishCredPass != "" || got.publishCredUser != "" {
		t.Errorf("failed save must clear the flow; mode=%v userSet=%v passSet=%v", got.mode, got.publishCredUser != "", got.publishCredPass != "")
	}
}

// TestPublishAuthReusesStoredCredential covers the "first authed publish only"
// rule: once a shared credential exists, auth 'y' skips the cred steps and
// goes straight to the confirm, reusing the stored hash unchanged.
func TestPublishAuthReusesStoredCredential(t *testing.T) {
	m := newPublishModel(t, nil)
	m.cfg.Caddy.AuthUser = "admin"
	m.cfg.Caddy.AuthHash = "$2a$10$abcdefghijklmnopqrstuv" // opaque stored hash
	before := m.cfg.Caddy.AuthHash

	m = mustUpdate(t, m, rkey("P"))
	m = mustUpdate(t, m, enterKey)  // host -> auth
	m = mustUpdate(t, m, rkey("y")) // auth yes -> should SKIP cred steps
	if m.mode != entryConfirmPublish {
		t.Fatalf("auth 'y' with a stored cred should skip to confirm; mode = %v", m.mode)
	}
	if !m.publishWithAuth {
		t.Error("publishWithAuth should be true")
	}
	res, _ := m.Update(rkey("y"))
	got := res.(model)
	if got.cfg.Caddy.AuthHash != before {
		t.Error("reusing a stored credential must not rewrite the hash")
	}
}

// TestPublishEscClearsPlaintext exercises esc at every publish step, and
// crucially that esc after the password is typed drops the plaintext (both at
// the password step and at the confirm step).
func TestPublishEscClearsPlaintext(t *testing.T) {
	// esc at each step returns to entryNone with a clean flow.
	steps := []struct {
		name string
		walk []tea.KeyMsg // keys to reach the step (before the esc)
	}{
		{"host", []tea.KeyMsg{rkey("P")}},
		{"auth", []tea.KeyMsg{rkey("P"), enterKey}},
		{"credUser", []tea.KeyMsg{rkey("P"), enterKey, rkey("y")}},
		{"credPass", []tea.KeyMsg{rkey("P"), enterKey, rkey("y"), rkey("bob"), enterKey}},
		{"confirm", []tea.KeyMsg{rkey("P"), enterKey, rkey("y"), rkey("bob"), enterKey, rkey("hunter2"), enterKey}},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			m := newPublishModel(t, nil)
			for _, k := range s.walk {
				m = mustUpdate(t, m, k)
			}
			m = mustUpdate(t, m, escKey)
			if m.mode != entryNone {
				t.Errorf("esc at %s -> mode %v, want entryNone", s.name, m.mode)
			}
			if m.publishCredPass != "" || m.publishCredUser != "" {
				t.Errorf("esc at %s left plaintext: user=%q pass=%q", s.name, m.publishCredUser, m.publishCredPass)
			}
			if m.publishHostname != "" || m.publishPort != 0 {
				t.Errorf("esc at %s left flow state: host=%q port=%d", s.name, m.publishHostname, m.publishPort)
			}
			if m.publishInput.EchoMode != textinput.EchoNormal {
				t.Errorf("esc at %s left the input masked", s.name)
			}
		})
	}
}

// TestRequestPublishGuards drives every requestPublish guard, in the order they
// fire, asserting the refusal (or de-escalation) each produces.
func TestRequestPublishGuards(t *testing.T) {
	base := func() model {
		t.Helper()
		m := newPublishModel(t, nil)
		return m
	}

	// :22 hard-block.
	t.Run("ssh hard-block", func(t *testing.T) {
		m := base()
		m.allPorts = append(m.allPorts, portscan.Port{Number: 22})
		cmd := m.requestPublish(22)
		if m.mode != entryNone || m.flashLevel != flashError || !strings.Contains(m.flash, ":22") {
			t.Errorf(":22 publish: mode=%v flash=%q level=%v (want refuse)", m.mode, m.flash, m.flashLevel)
		}
		_ = cmd
	})

	// empty fqdn: no backend label derivable.
	t.Run("empty fqdn", func(t *testing.T) {
		m := base()
		m.fqdn = ""
		m.requestPublish(8080)
		if m.mode != entryNone || m.flashLevel != flashError || !strings.Contains(m.flash, "tailnet name") {
			t.Errorf("empty fqdn: mode=%v flash=%q (want refuse)", m.mode, m.flash)
		}
	})

	// mutual exclusion: a funnelled port is refused, message names the funnel.
	t.Run("funnelled port refused", func(t *testing.T) {
		m := base()
		m.funnel = map[int]int{8080: 443}
		m.requestPublish(8080)
		if m.mode != entryNone || m.flashLevel != flashError || !strings.Contains(m.flash, "funnel") {
			t.Errorf("funnelled: mode=%v flash=%q (want refuse naming the funnel)", m.mode, m.flash)
		}
	})

	// blank caddy.domain: no longer refused -- it opens the inline
	// domain-capture prompt (kata w131), and ONLY after all refuse-guards pass.
	t.Run("blank domain opens capture prompt", func(t *testing.T) {
		m := base()
		m.cfg.Caddy.Domain = ""
		if cmd := m.requestPublish(8080); cmd != nil {
			t.Error("opening the domain-capture prompt should return a nil cmd")
		}
		if m.mode != entryPublishDomain {
			t.Errorf("blank domain: mode=%v flash=%q (want entryPublishDomain capture prompt)", m.mode, m.flash)
		}
		if m.publishPort != 8080 {
			t.Errorf("blank domain: publishPort=%d, want 8080", m.publishPort)
		}
	})

	// unresolvable caddy.hostname: a blank hostname is refused (guard 7).
	t.Run("no hostname configured", func(t *testing.T) {
		m := base()
		m.cfg.Caddy.Hostname = ""
		m.requestPublish(8080)
		if m.mode != entryNone || !strings.Contains(m.flash, "caddy.hostname") {
			t.Errorf("no hostname: mode=%v flash=%q (want refuse naming caddy.hostname)", m.mode, m.flash)
		}
	})

	// FQDN-shaped caddy.hostname: refused with the short-MagicDNS-label
	// guidance (§4d) -- the edge admits only the short name, so an FQDN 403s.
	t.Run("fqdn hostname refused", func(t *testing.T) {
		m := base()
		m.cfg.Caddy.Hostname = "caddy.tailnet.ts.net"
		m.requestPublish(8080)
		if m.mode != entryNone || m.flashLevel != flashError {
			t.Errorf("fqdn hostname: mode=%v level=%v (want refuse)", m.mode, m.flashLevel)
		}
		if !strings.Contains(m.flash, "short MagicDNS label") {
			t.Errorf("fqdn hostname refusal should name the short label; flash=%q", m.flash)
		}
	})

	// The guard REORDER's whole point (ycv1 r1-#10): a locked port with a BLANK
	// domain must be REFUSED for the lock, never prompted for a domain first.
	t.Run("locked port with blank domain refused (not prompted)", func(t *testing.T) {
		m := base()
		m.cfg.Caddy.Domain = ""
		m.cfg.Ports[8080] = config.PortMeta{Favorite: true, Locked: true}
		m.requestPublish(8080)
		if m.mode != entryNone || m.flashLevel != flashError || !strings.Contains(m.flash, "locked") {
			t.Errorf("locked+blank-domain: mode=%v flash=%q (want lock refusal, NOT a domain prompt)", m.mode, m.flash)
		}
	})

	// locked port: publish must not bypass the x lock.
	t.Run("locked port refused", func(t *testing.T) {
		m := base()
		m.cfg.Ports[8080] = config.PortMeta{Favorite: true, Locked: true}
		m.requestPublish(8080)
		if m.mode != entryNone || m.flashLevel != flashError || !strings.Contains(m.flash, "locked") {
			t.Errorf("locked: mode=%v flash=%q (want refuse)", m.mode, m.flash)
		}
	})

	// busy: an in-flight op no-ops.
	t.Run("busy no-op", func(t *testing.T) {
		m := base()
		m.pending = 3000
		if cmd := m.requestPublish(8080); cmd != nil {
			t.Error("busy publish should be a nil no-op")
		}
		if m.mode != entryNone {
			t.Errorf("busy publish should not open a dialog; mode=%v", m.mode)
		}
	})

	// happy path: opens the dialog with the prefilled host.
	t.Run("happy path opens dialog", func(t *testing.T) {
		m := base()
		if cmd := m.requestPublish(8080); cmd != nil {
			t.Error("opening the dialog should return a nil cmd")
		}
		if m.mode != entryPublishHost {
			t.Errorf("happy path: mode=%v, want entryPublishHost", m.mode)
		}
		if m.publishPort != 8080 {
			t.Errorf("publishPort = %d, want 8080", m.publishPort)
		}
	})
}

// TestPublishPrefillPrecedence pins the host prefill: label > process >
// machine short-label, then "."+domain.
func TestPublishPrefillPrecedence(t *testing.T) {
	// user label wins.
	m := newPublishModel(t, nil)
	m.cfg.Ports[8080] = config.PortMeta{Favorite: true, Label: "dashboard"}
	m.requestPublish(8080)
	if got := m.publishInput.Value(); got != "dashboard.example.com" {
		t.Errorf("label prefill = %q, want dashboard.example.com", got)
	}

	// no label, process name.
	m = newPublishModel(t, nil)
	m.requestPublish(8080)
	if got := m.publishInput.Value(); got != "web.example.com" {
		t.Errorf("process prefill = %q, want web.example.com", got)
	}

	// no label, no process -> machine short-label.
	m = newPublishModel(t, nil)
	m.allPorts = []portscan.Port{{Number: 8080}} // no Process
	m.requestPublish(8080)
	if got := m.publishInput.Value(); got != "dev-box.example.com" {
		t.Errorf("short-label prefill = %q, want dev-box.example.com", got)
	}
}

// TestPublishInvalidHostnameRefused: an invalid hostname at the host step stays
// on the step with an error, never advancing to the auth gate.
func TestPublishInvalidHostnameRefused(t *testing.T) {
	m := newPublishModel(t, nil)
	m = mustUpdate(t, m, rkey("P"))
	m.publishInput.SetValue("not a host") // spaces are invalid
	m = mustUpdate(t, m, enterKey)
	if m.mode != entryPublishHost {
		t.Errorf("invalid hostname should stay on the host step; mode=%v", m.mode)
	}
	if m.flashLevel != flashError {
		t.Errorf("invalid hostname should raise an error toast; level=%v flash=%q", m.flashLevel, m.flash)
	}
}

// TestValidPublishDomain pins the base-domain validator (kata w131): it reuses
// caddyedge.ValidHostname (rejecting blank/"*.x") and ADDS a "must have a dot"
// rule so a bare label can't become a bogus "label.foo" publish host, while
// dotted public domains pass.
func TestValidPublishDomain(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"example.com", true},
		{"apps.example.com", true},
		{"a.b.example.com", true},
		{"foo", false},       // bare label -- no dot
		{"localhost", false}, // bare label -- no dot
		{"", false},          // blank
		{"*.x", false},       // wildcard label is not LDH
		{"http://x.com", false},
		{"x.com:8080", false},
		{"has space.com", false},
	} {
		if got := validPublishDomain(c.in); got != c.want {
			t.Errorf("validPublishDomain(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestPublishDomainCaptureHappyPath is the guard-reorder assist's happy path
// (kata w131): a blank caddy.domain opens the inline capture prompt, a valid
// domain is persisted (disk + memory), the flow advances into the SHARED host
// dialog with the prefill rebuilt against the just-saved domain, and the
// parallel sticky setup-banner goes active.
func TestPublishDomainCaptureHappyPath(t *testing.T) {
	m := newPublishModel(t, nil)
	m.cfg.Caddy.Domain = "" // force the capture path

	m = mustUpdate(t, m, rkey("P"))
	if m.mode != entryPublishDomain {
		t.Fatalf("blank domain: mode=%v, want entryPublishDomain", m.mode)
	}
	if m.publishPort != 8080 {
		t.Fatalf("publishPort=%d, want 8080", m.publishPort)
	}

	m = mustUpdate(t, m, rkey("apps.example.com"))
	m = mustUpdate(t, m, enterKey)

	// Persisted in memory...
	if m.cfg.Caddy.Domain != "apps.example.com" {
		t.Errorf("in-memory caddy.domain = %q, want apps.example.com", m.cfg.Caddy.Domain)
	}
	// ...and to disk (SaveCaddyDomain wrote to the isolated XDG config).
	loaded, err := config.Load("")
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if loaded.Caddy.Domain != "apps.example.com" {
		t.Errorf("persisted caddy.domain = %q, want apps.example.com", loaded.Caddy.Domain)
	}
	// Advanced into the shared host dialog, prefill rebuilt on the new domain.
	if m.mode != entryPublishHost {
		t.Fatalf("after domain save, mode=%v, want entryPublishHost", m.mode)
	}
	if got := m.publishInput.Value(); !strings.HasSuffix(got, ".apps.example.com") {
		t.Errorf("host prefill = %q, want suffix .apps.example.com", got)
	}
	// The parallel sticky setup-banner is now active and names the domain.
	if !m.domainSetupPending {
		t.Error("domainSetupPending should be true after a domain capture")
	}
	if hint := m.domainSetupHintText(); !strings.Contains(hint, "*.apps.example.com") {
		t.Errorf("domain-setup banner should name *.apps.example.com; got %q", hint)
	}
}

// TestPublishDomainInvalidStaysOnPrompt: an invalid base domain at the capture
// step stays on the prompt with an error and persists nothing (kata w131).
func TestPublishDomainInvalidStaysOnPrompt(t *testing.T) {
	for _, bad := range []string{"foo", "*.x", ""} {
		t.Run(bad, func(t *testing.T) {
			m := newPublishModel(t, nil)
			m.cfg.Caddy.Domain = ""
			m = mustUpdate(t, m, rkey("P"))
			if bad != "" {
				m.publishInput.SetValue(bad)
			}
			m = mustUpdate(t, m, enterKey)
			if m.mode != entryPublishDomain {
				t.Errorf("invalid domain %q should stay on the capture step; mode=%v", bad, m.mode)
			}
			if m.flashLevel != flashError {
				t.Errorf("invalid domain %q should raise an error toast; level=%v flash=%q", bad, m.flashLevel, m.flash)
			}
			if m.cfg.Caddy.Domain != "" {
				t.Errorf("invalid domain %q must not persist a domain; got %q", bad, m.cfg.Caddy.Domain)
			}
			if m.domainSetupPending {
				t.Errorf("invalid domain %q must not raise the setup banner", bad)
			}
		})
	}
}

// TestPublishDomainEscAborts: esc at the domain-capture step aborts cleanly back
// to entryNone, persisting nothing and raising no banner (kata w131).
func TestPublishDomainEscAborts(t *testing.T) {
	m := newPublishModel(t, nil)
	m.cfg.Caddy.Domain = ""
	m = mustUpdate(t, m, rkey("P"))
	if m.mode != entryPublishDomain {
		t.Fatalf("setup: mode=%v, want entryPublishDomain", m.mode)
	}
	m = mustUpdate(t, m, escKey)
	if m.mode != entryNone {
		t.Errorf("esc at domain step -> mode %v, want entryNone", m.mode)
	}
	if m.publishPort != 0 {
		t.Errorf("esc at domain step left publishPort=%d", m.publishPort)
	}
	if m.domainSetupPending {
		t.Error("esc at domain step must not raise the setup banner")
	}
}

// TestDomainSetupBannerClearsOnPublish: the sticky setup-banner is retired by a
// successful publish and by a successful published-state poll (kata w131) -- a
// real edge is then demonstrably working.
func TestDomainSetupBannerClearsOnPublish(t *testing.T) {
	t.Run("cleared on publishDoneMsg success", func(t *testing.T) {
		m := newPublishModel(t, nil)
		m.domainSetupPending = true
		res, _ := m.Update(publishDoneMsg{port: 8080, err: nil})
		if res.(model).domainSetupPending {
			t.Error("a successful publish should clear domainSetupPending")
		}
	})
	t.Run("cleared on publishPollMsg success", func(t *testing.T) {
		m := newPublishModel(t, nil)
		m.domainSetupPending = true
		res, _ := m.Update(publishPollMsg{gen: 1, published: map[int]publishInfo{}})
		if res.(model).domainSetupPending {
			t.Error("a successful poll should clear domainSetupPending")
		}
	})
}

// TestPublishSuccessClearsPendingPublish (kata vsx4 #1): a successful publish is
// a TERMINAL outcome, so it clears m.pendingPublish -- otherwise the carry from
// this attempt could linger and be misread by some later, unrelated op (the
// vsx4 #1 class of bug).
func TestPublishSuccessClearsPendingPublish(t *testing.T) {
	m := newPublishModel(t, nil)
	m.pendingPublish = pendingPublish{hostname: "app.example.com", label: "dev-box", port: 8080, withAuth: true}
	res, _ := m.Update(publishDoneMsg{port: 8080, err: nil})
	got := res.(model)
	if got.pendingPublish != (pendingPublish{}) {
		t.Errorf("a successful publish should clear pendingPublish; got %+v", got.pendingPublish)
	}
}

// TestTwoConcurrentBanners proves the two sticky banners are genuinely PARALLEL
// (kata w131, ycv1 r3-NEW-1): with BOTH operatorNotSet and domainSetupPending
// true, View renders both lines, and listBodyHeight reserves enough that neither
// the banners nor the bottom bar clip.
func TestTwoConcurrentBanners(t *testing.T) {
	m := New(config.Config{Ports: map[int]config.PortMeta{}})
	m.cfg.Caddy.Domain = "apps.example.com"
	m.operatorUser = "alice"
	m.allPorts = []portscan.Port{{Number: 3000, Process: "node"}, {Number: 8080, Process: "srv"}}
	m.showAllPorts = true
	m.rebuildItems()
	r, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m = r.(model)
	m.operatorNotSet = true
	m.domainSetupPending = true

	view := m.View()
	if got := lipgloss.Height(view); got > m.height {
		t.Errorf("View height %d > terminal height %d (both banners clip/overlap):\n%s", got, m.height, stripANSI(view))
	}
	plain := stripANSI(view)
	if !strings.Contains(plain, "operator not set") {
		t.Errorf("operator banner missing from View:\n%s", plain)
	}
	if !strings.Contains(plain, "domain saved") {
		t.Errorf("domain-setup banner missing from View:\n%s", plain)
	}
	if !strings.Contains(plain, "q quit") {
		t.Errorf("bottom bar clipped with both banners live:\n%s", plain)
	}
}

// TestBannerReservationDominatesBothLive mirrors TestOperatorHintSizingNoClip
// but exercises BOTH sticky banners appearing after the last resize (kata w131),
// and now pins the WRAPPED-height invariant directly (roborev 7dbj finding 3):
// the banners are wrapped to m.width at the render site (renderBanner) instead
// of being hard-truncated, so a long one -- the domain reminder easily runs past
// 80 cols -- can span more than one row. listBodyHeight must reserve that
// measured worst case (bannerReservationLines), through the SAME renderBanner the
// render uses, or a wrapped banner clips the list. Each case asserts the
// reservation is never shorter than the live rendered banner height, that
// listBodyHeight stays >=1, and that View never exceeds the viewport; one case
// forces a LONG domain at a NARROW width so the domain banner genuinely wraps to
// 2+ lines and proves the reservation still covers it.
func TestBannerReservationDominatesBothLive(t *testing.T) {
	build := func(w, h int, domain string, opAtRender, domAtRender bool) model {
		m := New(config.Config{Ports: map[int]config.PortMeta{}})
		m.cfg.Caddy.Domain = domain
		m.operatorUser = "alice"
		m.allPorts = []portscan.Port{
			{Number: 3000, Process: "node"}, {Number: 8080, Process: "srv"},
			{Number: 9000, Process: "api"}, {Number: 5173, Process: "vite"},
		}
		m.showAllPorts = true
		m.rebuildItems()
		// Both banners OFF at resize time, then flipped on AFTER -- the reservation
		// must already cover the worst case.
		r, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
		m = r.(model)
		m.operatorNotSet = opAtRender
		m.domainSetupPending = domAtRender
		return m
	}
	// liveBannerHeight is the height the LIVE (active-flag-gated) banners actually
	// render to, wrapped exactly as renderBottom wraps them -- the height the
	// reservation must dominate.
	liveBannerHeight := func(m model) int {
		h := 0
		if b := m.renderBanner(m.domainSetupHintText()); b != "" {
			h += lipgloss.Height(b)
		}
		if b := m.renderBanner(m.operatorHintText()); b != "" {
			h += lipgloss.Height(b)
		}
		return h
	}
	const longDomain = "staging.internal-tools.us-east-1.platform.example.com"
	for _, tc := range []struct {
		name        string
		w, h        int
		domain      string
		opOn, domOn bool
		wantDomWrap bool // domain banner must wrap to 2+ lines in this case
	}{
		{"wide/both", 100, 24, "apps.example.com", true, true, false},
		{"narrow/both", 58, 24, "apps.example.com", true, true, false},
		{"wide/op-only", 100, 24, "apps.example.com", true, false, false},
		{"wide/dom-only", 100, 24, "apps.example.com", false, true, false},
		{"narrow/dom-only", 58, 24, "apps.example.com", false, true, false},
		// A long domain at a narrow width forces the domain banner across
		// multiple rows -- the reservation must MEASURE that, not assume 1 line.
		{"narrow/long-domain/both", 48, 24, longDomain, true, true, true},
		{"narrow/long-domain/dom-only", 40, 24, longDomain, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := build(tc.w, tc.h, tc.domain, tc.opOn, tc.domOn)

			// The reservation must never fall short of the live wrapped banners --
			// the exact invariant a wrapped (multi-line) banner could otherwise
			// break. bannerReservationLines worst-cases BOTH banners, so it equals
			// the live height when both are on and exceeds it when only one is.
			reserved := m.bannerReservationLines()
			if live := liveBannerHeight(m); reserved < live {
				t.Errorf("banner reservation %d < live banner height %d -- a wrapped banner would clip the list", reserved, live)
			}

			// Prove the intended multi-line wrap is actually being exercised, so
			// this case can't silently degrade into a single-line one.
			if tc.wantDomWrap {
				if dh := lipgloss.Height(m.renderBanner(m.domainSetupHintText())); dh < 2 {
					t.Errorf("domain banner height %d at width %d -- expected a multi-line wrap (>=2) for this case", dh, tc.w)
				}
			}

			// listBodyHeight must stay positive (never negative/zero) even with the
			// larger wrapped reservation subtracted.
			if bh := m.listBodyHeight(); bh < 1 {
				t.Errorf("listBodyHeight %d < 1 -- reservation drove the body negative", bh)
			}

			view := m.View()
			if got := lipgloss.Height(view); got > m.height {
				t.Errorf("View height %d > terminal height %d (clip/overlap):\n%s", got, m.height, stripANSI(view))
			}
			if plain := stripANSI(view); !strings.Contains(plain, "q quit") {
				t.Errorf("bottom bar clipped:\n%s", plain)
			}
		})
	}
}

// TestDeEscalationImmediateUnpublish: P on a port THIS machine already publishes
// unpublishes immediately -- no confirm, no dialog -- and the edge round-trip
// actually deletes the route (re-verifying ownership first).
func TestDeEscalationImmediateUnpublish(t *testing.T) {
	fc := newFakeCaddy()
	rt := caddyedge.BuildRoute("web.example.com", "dev-box", 8080, nil)
	fc.routes[rt.ID] = rt
	srv := httptest.NewServer(fc)
	defer srv.Close()

	m := newPublishModel(t, srv)
	m.published = map[int]publishInfo{8080: {hostname: "web.example.com"}}

	cmd := m.requestPublish(8080)
	if m.mode != entryNone {
		t.Errorf("de-escalation must not open a dialog; mode=%v", m.mode)
	}
	if m.pending != 8080 {
		t.Errorf("de-escalation should set pending; got %d", m.pending)
	}
	if cmd == nil {
		t.Fatal("de-escalation should return an unpublish cmd")
	}
	msg, ok := cmd().(publishDoneMsg)
	if !ok || msg.err != nil {
		t.Fatalf("unpublish cmd = %#v, want a clean publishDoneMsg", msg)
	}
	if _, still := fc.routes[rt.ID]; still {
		t.Error("de-escalation should have DELETEd the published route")
	}
	if len(fc.deletes) != 1 {
		t.Errorf("expected exactly one DELETE; got %d", len(fc.deletes))
	}
}

// TestFunnelRefusesPublished is the OTHER mutual-exclusion direction: the p
// funnel key refuses a port that is currently Caddy-published.
func TestFunnelRefusesPublished(t *testing.T) {
	m := newPublishModel(t, nil)
	m.published = map[int]publishInfo{8080: {hostname: "web.example.com"}}
	cmd := m.requestFunnel(8080)
	if m.mode != entryNone {
		t.Errorf("funnel on a published port must not open a confirm; mode=%v", m.mode)
	}
	if m.flashLevel != flashError || !strings.Contains(m.flash, "published") || !strings.Contains(m.flash, "unpublish") {
		t.Errorf("funnel-on-published flash=%q level=%v, want a refusal naming publish", m.flash, m.flashLevel)
	}
	_ = cmd
}

// TestPublishReachDriftSurfaced covers step 4's reach() rules: a published-only
// port shows the ◆ marker and its https description; a port carrying BOTH
// public paths (external drift) is NOT silently collapsed to one marker -- it
// reuses the ▲ warning affordance and a distinct "funnelled AND published"
// description, with no bespoke drift state.
func TestPublishReachDriftSurfaced(t *testing.T) {
	// Published only -> reachPublish, ◆, https description with an auth note.
	pub := portItem{port: portscan.Port{Number: 8080}, host: "dev-box", publishHostname: "web.example.com", publishAuth: true}
	if got := pub.reach(); got != reachPublish {
		t.Errorf("published-only reach() = %v, want reachPublish", got)
	}
	if got := stripANSI(pub.markerGlyph()); got != "◆" {
		t.Errorf("published marker = %q, want ◆", got)
	}
	if d := pub.plainDescription(); !strings.Contains(d, "published to the internet · https://web.example.com") || !strings.Contains(d, "basic auth") {
		t.Errorf("published description = %q", d)
	}

	// Drift: both funnel AND publish (external mutation) -> reachStale (warning
	// affordance), distinct description, NOT reachPublish/reachFunnel.
	drift := portItem{port: portscan.Port{Number: 8080}, host: "dev-box", fqdn: "dev-box.tailnet.ts.net", active: true, listening: true, funnelPublic: 443, publishHostname: "web.example.com"}
	if got := drift.reach(); got != reachStale {
		t.Errorf("drift reach() = %v, want reachStale (warning affordance, not a silent collapse)", got)
	}
	if got := stripANSI(drift.markerGlyph()); got != "▲" {
		t.Errorf("drift marker = %q, want ▲ (the existing warn affordance)", got)
	}
	if d := drift.plainDescription(); !strings.Contains(d, "funnelled AND published") {
		t.Errorf("drift description = %q, want it to name the dual exposure", d)
	}
}

// TestPublishPollQuietDegrade covers step 5: a failed poll keeps last-known
// state and flips publishReachable false with a persistent status fragment
// (never a toast); a later good poll restores it.
func TestPublishPollQuietDegrade(t *testing.T) {
	cfg := config.Config{}
	cfg.Caddy.Domain = "example.com"
	cfg.Caddy.Hostname = "caddy"
	m := New(cfg)
	m.width = 200
	m.published = map[int]publishInfo{8080: {hostname: "web.example.com"}}
	m.publishReachable = true

	// A poll failure: keep the published map, mark unreachable, NO toast.
	res, _ := m.Update(publishPollMsg{err: fmt.Errorf("dial tcp: refused")})
	got := res.(model)
	if got.publishReachable {
		t.Error("a failed poll should set publishReachable=false")
	}
	if len(got.published) != 1 {
		t.Errorf("a failed poll must keep last-known state; got %d entries", len(got.published))
	}
	if got.flash != "" {
		t.Errorf("a failed poll must NOT raise a toast; flash=%q", got.flash)
	}
	if !strings.Contains(got.statusText(), "edge unreachable") {
		t.Errorf("status line should carry the persistent degrade fragment; got %q", got.statusText())
	}

	// A subsequent good poll restores reachability and replaces the map.
	res, _ = got.Update(publishPollMsg{published: map[int]publishInfo{9090: {hostname: "api.example.com"}}})
	got = res.(model)
	if !got.publishReachable {
		t.Error("a good poll should restore publishReachable=true")
	}
	if _, ok := got.published[9090]; !ok || len(got.published) != 1 {
		t.Errorf("a good poll should replace the published map; got %#v", got.published)
	}
	if strings.Contains(got.statusText(), "edge unreachable") {
		t.Error("status fragment should clear once the edge is reachable again")
	}
}

// TestStatusFragmentGatedOnConfig: the degrade fragment never appears when
// publishing is unconfigured (blank caddy.domain), even if publishReachable is
// false, since the poll is suppressed entirely there.
func TestStatusFragmentGatedOnConfig(t *testing.T) {
	m := New(config.Config{}) // Domain == ""
	m.width = 200
	m.publishReachable = false
	if strings.Contains(m.statusText(), "edge unreachable") {
		t.Errorf("unconfigured publish must not show the degrade fragment; got %q", m.statusText())
	}
}

// TestPollPublishedFiltersByLabel: the poll keeps only routes whose backend
// label matches THIS machine's short label, and is a nil (zero-cost) cmd when
// publishing is unconfigured.
func TestPollPublishedFiltersByLabel(t *testing.T) {
	fc := newFakeCaddy()
	ours := caddyedge.BuildRoute("a.example.com", "dev-box", 8080, nil)
	theirs := caddyedge.BuildRoute("b.example.com", "other-box", 9090, nil)
	fc.routes[ours.ID] = ours
	fc.routes[theirs.ID] = theirs
	srv := httptest.NewServer(fc)
	defer srv.Close()

	m := newPublishModel(t, srv)
	cmd := m.pollPublishedCmd()
	if cmd == nil {
		t.Fatal("a configured edge should return a poll cmd")
	}
	msg, ok := cmd().(publishPollMsg)
	if !ok || msg.err != nil {
		t.Fatalf("poll = %#v, want a clean publishPollMsg", msg)
	}
	if len(msg.published) != 1 {
		t.Fatalf("poll should keep only our machine's routes; got %#v", msg.published)
	}
	if info := msg.published[8080]; info.hostname != "a.example.com" {
		t.Errorf("poll[8080] = %#v, want a.example.com", info)
	}
	if _, ok := msg.published[9090]; ok {
		t.Error("poll must NOT include another machine's route")
	}

	// Unconfigured -> nil cmd, zero cost.
	m.cfg.Caddy.Domain = ""
	if m.pollPublishedCmd() != nil {
		t.Error("a blank caddy.domain should suppress the poll (nil cmd)")
	}
}

// TestPollPublishedGatedOnLabel: no poll is issued until the short backend
// label is known (roborev 0k12 #1a) -- a label-less poll would return an empty
// map that, landing after the fqdn-triggered poll, wipes valid routes. Each
// issued poll also carries a strictly increasing generation stamp (#1b) so
// out-of-order results can be ordered.
func TestPollPublishedGatedOnLabel(t *testing.T) {
	fc := newFakeCaddy()
	srv := httptest.NewServer(fc)
	defer srv.Close()
	m := newPublishModel(t, srv)

	// fqdn not yet resolved -> no short label -> nil cmd, even though the edge
	// is fully configured.
	m.fqdn = ""
	if m.pollPublishedCmd() != nil {
		t.Error("a label-less (fqdn unresolved) poll must be suppressed (nil cmd)")
	}

	// Once the label is known, successive polls carry increasing generations.
	m.fqdn = "dev-box.tailnet.ts.net"
	cmd1 := m.pollPublishedCmd()
	if cmd1 == nil {
		t.Fatal("a configured edge with a known label should return a poll cmd")
	}
	msg1 := cmd1().(publishPollMsg)
	msg2 := m.pollPublishedCmd()().(publishPollMsg)
	if msg1.gen <= 0 || msg2.gen <= msg1.gen {
		t.Errorf("poll generations must strictly increase; got gen1=%d gen2=%d", msg1.gen, msg2.gen)
	}
}

// TestPublishPollOutOfOrderDropsStale drives two versioned poll results in
// REVERSE order (roborev 0k12 #1): a newer poll (gen 2) with populated routes
// applies, and a stale poll (gen 1) carrying an empty map arriving afterward is
// DROPPED so it can't wipe the fresh state -- which would transiently hide
// exposure and bypass the funnel/publish mutual-exclusion guards. The drop also
// covers the err path, so a stale failure can't flip reachability false after a
// newer success.
func TestPublishPollOutOfOrderDropsStale(t *testing.T) {
	cfg := config.Config{}
	cfg.Caddy.Domain = "example.com"
	cfg.Caddy.Hostname = "caddy"
	m := New(cfg)
	m.width = 200

	// The newer poll lands first: populated routes.
	fresh := map[int]publishInfo{8080: {hostname: "web.example.com"}}
	m = mustUpdate(t, m, publishPollMsg{gen: 2, published: fresh})
	if _, ok := m.published[8080]; !ok || len(m.published) != 1 {
		t.Fatalf("newer poll (gen 2) should populate published; got %#v", m.published)
	}

	// The older poll arrives late with an EMPTY map: it must be dropped.
	m = mustUpdate(t, m, publishPollMsg{gen: 1, published: map[int]publishInfo{}})
	if _, ok := m.published[8080]; !ok || len(m.published) != 1 {
		t.Errorf("stale poll (gen 1) must NOT overwrite fresh state; got %#v", m.published)
	}

	// A stale FAILURE likewise can't flip reachability false after the newer
	// success already set it true.
	m.publishReachable = true
	m = mustUpdate(t, m, publishPollMsg{gen: 1, err: fmt.Errorf("dial tcp: refused")})
	if !m.publishReachable {
		t.Error("a stale failure poll must not flip publishReachable=false")
	}

	// A genuinely newer poll (gen 3) still applies normally.
	m = mustUpdate(t, m, publishPollMsg{gen: 3, published: map[int]publishInfo{9090: {hostname: "api.example.com"}}})
	if _, ok := m.published[9090]; !ok || len(m.published) != 1 {
		t.Errorf("newer poll (gen 3) should apply; got %#v", m.published)
	}
}

// TestPollPublishedIgnoresForeignRoute: a FOREIGN route -- one whose @id lacks
// the tailport- prefix, so RouteInfo.Owned is false -- targeting THIS machine's
// backend label:port must NOT be treated as published (roborev 0k12 #2). Left
// unfiltered it would show the port published, block funnel, and on
// de-escalation try to unpublish a synthesized id that doesn't exist.
func TestPollPublishedIgnoresForeignRoute(t *testing.T) {
	fc := newFakeCaddy()
	// An owned tailport route on :8080, plus a foreign (hand-added) route
	// targeting the SAME backend label on :9090.
	ours := caddyedge.BuildRoute("a.example.com", "dev-box", 8080, nil)
	foreign := caddyedge.BuildRoute("b.example.com", "dev-box", 9090, nil)
	foreign.ID = "manual-b.example.com" // not the tailport- prefix -> Owned=false
	fc.routes[ours.ID] = ours
	fc.routes[foreign.ID] = foreign
	srv := httptest.NewServer(fc)
	defer srv.Close()

	m := newPublishModel(t, srv)
	msg := m.pollPublishedCmd()().(publishPollMsg)
	if _, ok := msg.published[9090]; ok {
		t.Error("a foreign (non-owned) route must NOT be treated as published")
	}
	if info := msg.published[8080]; info.hostname != "a.example.com" || len(msg.published) != 1 {
		t.Errorf("only our owned route should be published; got %#v", msg.published)
	}

	// Applying the poll, the foreign :9090 must not block funnel: requestFunnel
	// should proceed to the confirm, not refuse with a "published" message.
	m.published = msg.published
	m.allPorts = append(m.allPorts, portscan.Port{Number: 9090, Process: "svc"})
	m.active = map[int]bool{8080: true, 9090: true}
	m.requestFunnel(9090)
	if strings.Contains(m.flash, "published") {
		t.Errorf("a foreign route must not block funnel on :9090; flash=%q", m.flash)
	}
	if m.mode != entryConfirmFunnel {
		t.Errorf("funnel on a non-published port should reach the confirm; mode=%v", m.mode)
	}
}

// TestPublishCmdRoundTrip: a publish cmd built with auth POSTs a route whose
// backend dials <label>:<port> plainly and carries the auth handler.
func TestPublishCmdRoundTrip(t *testing.T) {
	fc := newFakeCaddy()
	srv := httptest.NewServer(fc)
	defer srv.Close()
	client := &caddyedge.Client{AdminURL: srv.URL, ServerName: "tailport", HTTPClient: srv.Client()}

	auth := &caddyedge.BasicAuth{User: "admin", Hash: "$2a$10$deadbeefdeadbeefdeadbe"}
	cmd := publishCmd(client, "app.example.com", "dev-box", 8080, auth, false) // enableServe false: no tailscale
	msg, ok := cmd().(publishDoneMsg)
	if !ok || msg.err != nil {
		t.Fatalf("publish cmd = %#v, want a clean publishDoneMsg", msg)
	}
	if len(fc.mutations) != 1 {
		t.Fatalf("expected one POST; got %d", len(fc.mutations))
	}
	rt := fc.mutations[0]
	if rt.ID != caddyedge.IDFor("app.example.com") {
		t.Errorf("route @id = %q, want %q", rt.ID, caddyedge.IDFor("app.example.com"))
	}
	if dial := routeDial(rt); dial != "dev-box:8080" {
		t.Errorf("backend dial = %q, want dev-box:8080", dial)
	}
	if !routeHasAuth(rt) {
		t.Error("route should carry the basic-auth handler")
	}
}

// TestUnpublishCmdRoundTrip: an unpublish cmd DELETEs the route after the
// client re-verifies ownership.
func TestUnpublishCmdRoundTrip(t *testing.T) {
	fc := newFakeCaddy()
	rt := caddyedge.BuildRoute("app.example.com", "dev-box", 8080, nil)
	fc.routes[rt.ID] = rt
	srv := httptest.NewServer(fc)
	defer srv.Close()
	client := &caddyedge.Client{AdminURL: srv.URL, ServerName: "tailport", HTTPClient: srv.Client()}

	cmd := unpublishCmd(client, "app.example.com", "dev-box", 8080)
	msg, ok := cmd().(publishDoneMsg)
	if !ok || msg.err != nil {
		t.Fatalf("unpublish cmd = %#v, want a clean publishDoneMsg", msg)
	}
	if !msg.unpublish {
		t.Error("unpublishCmd's publishDoneMsg must set unpublish=true (kata vsx4 #1)")
	}
	if _, still := fc.routes[rt.ID]; still {
		t.Error("unpublish should have deleted the route")
	}
}

// TestUnpublishConflictNeverRepublishes is the vsx4 #1 HIGH regression: an
// unpublish whose Unpublish call returns caddyedge.ErrHostnameConflict (a real
// possibility -- caddyedge.go) must NEVER walk the publish conflict/retry path,
// even with a STALE m.pendingPublish left over from some earlier, unrelated
// publish attempt (m.pendingPublish is never cleared on the non-terminal
// conflict/inspect/purge branches, so a prior attempt's carry can genuinely
// still be sitting there). Before the fix, publishDoneMsg had no way to tell an
// unpublish outcome from a publish one, so this classified the STALE hostname
// and, on Kind==None, re-published -- re-exposing a port the user asked to
// de-escalate. The fix gates the conflict branch on !msg.unpublish, so this
// must fall straight through to a plain error toast: no inspectConflictCmd, no
// publishCmd.
func TestUnpublishConflictNeverRepublishes(t *testing.T) {
	m := newPublishModel(t, nil)
	m.pendingPublish = pendingPublish{hostname: "stale.example.com", label: "dev-box", port: 9999, withAuth: true}
	m.pending = 8080

	res, cmd := m.Update(publishDoneMsg{port: 8080, err: caddyedge.ErrHostnameConflict, unpublish: true})
	got := res.(model)

	if got.pending != 0 {
		t.Errorf("an unpublish outcome should clear pending; got %d", got.pending)
	}
	if got.pendingPublish != (pendingPublish{}) {
		t.Errorf("pendingPublish should be cleared on this terminal outcome; got %+v", got.pendingPublish)
	}
	if got.flashLevel != flashError || got.flash != publishErrText(caddyedge.ErrHostnameConflict) {
		t.Errorf("an unpublish conflict should raise the PLAIN error toast; flash=%q level=%v", got.flash, got.flashLevel)
	}

	if cmd == nil {
		t.Fatal("expected a cmd (toast + refresh + poll)")
	}
	// The classify/re-publish path (the bug) returns inspectConflictCmd BARE --
	// not wrapped in tea.Batch -- so cmd() would be a direct inspectConflictMsg.
	// The fixed, gated path always falls through to the plain-toast return,
	// which batches setErr+refresh+pollPublishedCmd. Assert the shape without
	// invoking the individual sub-cmds (refresh/poll do real local/network
	// I/O that has no place in this unit test).
	switch msg := cmd().(type) {
	case inspectConflictMsg:
		t.Fatalf("unpublish conflict must NOT classify/inspect; got a direct inspectConflictMsg cmd %#v", msg)
	case tea.BatchMsg:
		// expected: the plain-toast branch's batched refresh/toast/poll.
	default:
		t.Fatalf("unexpected cmd result type %T (%#v)", msg, msg)
	}
}

// TestPublishDoneMsgReFetches: publishDoneMsg mirrors toggleDoneMsg -- clear
// pending and re-fetch (never hand-set the published map); an error surfaces a
// mapped toast.
func TestPublishDoneMsgReFetches(t *testing.T) {
	m := newPublishModel(t, nil)
	m.pending = 8080
	res, cmd := m.Update(publishDoneMsg{port: 8080})
	got := res.(model)
	if got.pending != 0 {
		t.Errorf("publishDoneMsg should clear pending; got %d", got.pending)
	}
	if cmd == nil {
		t.Error("publishDoneMsg should return a re-fetch cmd")
	}

	// Error variant maps the sentinel to a friendly toast.
	m.pending = 8080
	res, _ = m.Update(publishDoneMsg{port: 8080, err: caddyedge.ErrUnreachable})
	got = res.(model)
	if got.pending != 0 {
		t.Errorf("errored publishDoneMsg should still clear pending; got %d", got.pending)
	}
	if got.flashLevel != flashError || !strings.Contains(got.flash, "unreachable") {
		t.Errorf("errored publishDoneMsg flash=%q level=%v, want a mapped error toast", got.flash, got.flashLevel)
	}
}

// TestPublishConflictClassificationPerKind drives a hostname conflict end to end
// (publishDoneMsg{ErrHostnameConflict} -> inspectConflictCmd -> inspectConflictMsg)
// and asserts each Kind is handled correctly. Under 6n15 the two PURGEABLE kinds
// (OwnedDiffBackend, ForeignOverlap) now OPEN A CONFIRM LADDER instead of a flat
// refusal; IdHijacked still refuses. Classification itself never mutates — the
// delete only fires on the final confirm.
func TestPublishConflictClassificationPerKind(t *testing.T) {
	const host = "app.example.com"
	id := caddyedge.IDFor(host)

	// drive seeds the fake, runs the conflict classification round trip for an
	// attempted publish of :8080 -> host, and returns the resulting model + fake.
	// It asserts pending discipline and that classification mutated nothing.
	drive := func(t *testing.T, seed func(fc *fakeCaddy)) (model, *fakeCaddy) {
		t.Helper()
		fc := newFakeCaddy()
		seed(fc)
		srv := httptest.NewServer(fc)
		t.Cleanup(srv.Close)

		m := newPublishModel(t, srv)
		// Mirror confirmPublish's secret-free carry + the in-flight marker.
		m.pendingPublish = pendingPublish{hostname: host, label: "dev-box", port: 8080}
		m.pending = 8080

		res, cmd := m.Update(publishDoneMsg{port: 8080, err: caddyedge.ErrHostnameConflict})
		m = res.(model)
		if cmd == nil {
			t.Fatal("a conflicted publishDoneMsg should issue a classification cmd")
		}
		if m.pending != 8080 {
			t.Errorf("pending must stay set across the classification read; got %d", m.pending)
		}
		msg, ok := cmd().(inspectConflictMsg)
		if !ok {
			t.Fatalf("conflict cmd returned %#v, want an inspectConflictMsg", cmd())
		}
		res, _ = m.Update(msg)
		m = res.(model)
		if m.pending != 0 {
			t.Errorf("inspectConflictMsg should clear pending; got %d", m.pending)
		}
		if len(fc.mutations) != 0 || len(fc.deletes) != 0 {
			t.Errorf("classification must not mutate: mutations=%d deletes=%d", len(fc.mutations), len(fc.deletes))
		}
		return m, fc
	}

	t.Run("owned same machine opens the purge-owned confirm naming your backend", func(t *testing.T) {
		m, _ := drive(t, func(fc *fakeCaddy) {
			fc.routes[id] = caddyedge.BuildRoute(host, "dev-box", 3000, nil) // our label, other port
		})
		if m.mode != entryConfirmPurgeOwned {
			t.Fatalf("owned conflict should open entryConfirmPurgeOwned; mode=%v flash=%q", m.mode, m.flash)
		}
		prompt := stripANSI(m.renderBottom())
		if !strings.Contains(prompt, "your dev-box:3000") {
			t.Errorf("same-machine purge prompt = %q, want it to name your dev-box:3000", prompt)
		}
		if strings.Contains(prompt, "ANOTHER machine") {
			t.Errorf("same-machine prompt must not say ANOTHER machine: %q", prompt)
		}
	})

	t.Run("owned another machine opens the purge-owned confirm naming it as another machine's", func(t *testing.T) {
		m, _ := drive(t, func(fc *fakeCaddy) {
			fc.routes[id] = caddyedge.BuildRoute(host, "other-box", 9090, nil) // NOT our label
		})
		if m.mode != entryConfirmPurgeOwned {
			t.Fatalf("owned conflict should open entryConfirmPurgeOwned; mode=%v", m.mode)
		}
		prompt := stripANSI(m.renderBottom())
		if !strings.Contains(prompt, "ANOTHER machine") || !strings.Contains(prompt, "other-box:9090") {
			t.Errorf("cross-machine purge prompt = %q, want it to name another machine's other-box:9090", prompt)
		}
	})

	t.Run("id hijacked stays a refusal (never a purge)", func(t *testing.T) {
		m, _ := drive(t, func(fc *fakeCaddy) {
			rt := caddyedge.BuildRoute(host, "dev-box", 8080, nil)
			rt.Match = []caddyedge.Match{{Host: []string{"evil.example.com"}}} // @id kept, matcher repointed
			fc.routes[id] = rt
		})
		if m.mode != entryNone {
			t.Fatalf("a hijacked @id must NOT open a purge confirm; mode=%v", m.mode)
		}
		if m.flashLevel != flashError || !strings.Contains(m.flash, "re-pointed at evil.example.com") || !strings.Contains(m.flash, "resolve it in Caddy") {
			t.Errorf("hijacked refusal = %q level=%v, want it to name the re-point and send the user to Caddy", m.flash, m.flashLevel)
		}
	})

	t.Run("foreign overlap (non-proxy route) opens the scary first gate", func(t *testing.T) {
		// A foreign static_response with no @id of ours and no reverse_proxy dial:
		// List would drop it, but the confirm must still name it by its handler.
		m, _ := drive(t, func(fc *fakeCaddy) {
			fc.routes["foreign-static"] = caddyedge.Route{
				ID:     "foreign-static",
				Match:  []caddyedge.Match{{Host: []string{host}}},
				Handle: []caddyedge.Handler{{Handler: "static_response"}},
			}
		})
		if m.mode != entryConfirmPurgeForeign {
			t.Fatalf("a foreign overlap should open entryConfirmPurgeForeign; mode=%v", m.mode)
		}
		prompt := stripANSI(m.renderBottom())
		if !strings.Contains(prompt, "did NOT create") || !strings.Contains(prompt, "static_response") {
			t.Errorf("foreign purge prompt = %q, want it to name the foreign static_response route", prompt)
		}
	})
}

// TestPublishConflictForeignDisclosableGate (roborev en3n-#1): a ForeignOverlap
// route tailport can FULLY disclose (Disclosable) opens the scary force-delete
// ladder; one it CANNOT (a hostless OR block / extra matcher) is REFUSED instead,
// so the user never force-deletes a catch-all under a confirm that named one host.
func TestPublishConflictForeignDisclosableGate(t *testing.T) {
	const host = "app.example.com"
	drive := func(disclosable bool) model {
		m := New(config.Config{})
		m.fqdn = "dev-box.tailnet.ts.net"
		m.pendingPublish = pendingPublish{hostname: host, label: "dev-box", port: 8080}
		m.pending = 8080
		info := caddyedge.ConflictInfo{Kind: caddyedge.ForeignOverlap, Hosts: []string{host}, Disclosable: disclosable}
		res, _ := m.Update(inspectConflictMsg{port: 8080, hostname: host, info: info})
		return res.(model)
	}

	t.Run("disclosable opens the scary ladder", func(t *testing.T) {
		if m := drive(true); m.mode != entryConfirmPurgeForeign {
			t.Errorf("a disclosable foreign route should open the scary confirm; mode=%v", m.mode)
		}
	})
	t.Run("non-disclosable is refused, no ladder", func(t *testing.T) {
		m := drive(false)
		if m.mode == entryConfirmPurgeForeign || m.mode == entryConfirmPurgeForeignType {
			t.Errorf("a non-disclosable foreign route must NOT open a force-delete ladder; mode=%v", m.mode)
		}
		if !strings.Contains(m.flash, "matches more than a hostname") {
			t.Errorf("want the refusal naming why; flash=%q", m.flash)
		}
		// The refusal must reconcile serve and warn it's left on (roborev xzns):
		// publishCmd enabled serve for the port before the conflict surfaced.
		if !m.active[8080] {
			t.Error("a conflict refusal must reconcile serve state (m.active[8080]) so space stops it")
		}
		if !strings.Contains(m.flash, "serve left on for :8080") {
			t.Errorf("the refusal must warn serve is left on; flash=%q", m.flash)
		}
	})
}

// TestConflictRefusalReconcilesRowForSpaceStop (roborev 2wts): a conflict refusal
// reconciles serve state so the "space to stop" hint actually stops it. The space
// toggle reads the cached portItem.active, not m.active, so the refusal must
// rebuild the list immediately -- otherwise, with a stale row reading serve=OFF,
// space would turn serve ON before the async refresh lands.
func TestConflictRefusalReconcilesRowForSpaceStop(t *testing.T) {
	cfg := config.Config{Ports: map[int]config.PortMeta{8080: {Favorite: true}}}
	cfg.Caddy.Domain, cfg.Caddy.Hostname, cfg.Caddy.ServerName, cfg.Caddy.AdminPort = "example.com", "caddy", "tailport", 2019
	m := New(cfg)
	m.fqdn = "dev-box.tailnet.ts.net"
	m.allPorts = []portscan.Port{{Number: 8080, Process: "web"}}
	m.active = map[int]bool{8080: false} // STALE: the cached row will show serve OFF
	m.showAllPorts = true
	m.rebuildItems()
	m.pendingPublish = pendingPublish{hostname: "app.example.com", label: "dev-box", port: 8080}
	m.pending = 8080

	// A non-disclosable foreign conflict → refuseConflict reconciles serve.
	info := caddyedge.ConflictInfo{Kind: caddyedge.ForeignOverlap, Hosts: []string{"app.example.com"}, Disclosable: false}
	res, _ := m.Update(inspectConflictMsg{port: 8080, hostname: "app.example.com", info: info})
	m2 := res.(model)

	found := false
	for _, it := range m2.list.Items() {
		if pi, ok := it.(portItem); ok && pi.port.Number == 8080 {
			found = true
			if !pi.active {
				t.Error("after a conflict refusal the cached :8080 row must read serve=ON, so space stops it (roborev 2wts)")
			}
		}
	}
	if !found {
		t.Fatal("port 8080 row not found in the rebuilt list")
	}
}

// TestPublishConflictNoneRetriesOnce: when the classification finds nothing (the
// conflict cleared between Publish's refusal and the read), the model retries the
// plain publish exactly ONCE. A second None gives up with a refusal instead of
// spinning (kata qfbf, bounded retry).
func TestPublishConflictNoneRetriesOnce(t *testing.T) {
	const host = "app.example.com"
	m := newPublishModel(t, nil) // no edge round-trip: we feed the msgs directly
	m.pendingPublish = pendingPublish{hostname: host, label: "dev-box", port: 8080}

	none := inspectConflictMsg{port: 8080, hostname: host, info: caddyedge.ConflictInfo{Kind: caddyedge.None}}

	// First None -> a retry publish cmd; retried flips true; pending re-armed; no toast.
	res, cmd := m.Update(none)
	m = res.(model)
	if !m.pendingPublish.retried {
		t.Error("the first None should mark the retry as fired")
	}
	if m.pending != 8080 {
		t.Errorf("the retry should re-arm pending; got %d", m.pending)
	}
	if cmd == nil {
		t.Fatal("the first None should return a retry publish cmd")
	}
	if m.flash != "" {
		t.Errorf("a retried None should not toast; got %q", m.flash)
	}

	// Second None -> give up (no spin), a plain refusal, no further retry cmd.
	res, _ = m.Update(none)
	m = res.(model)
	if m.flashLevel != flashError || !strings.Contains(m.flash, "cleared then returned") {
		t.Errorf("the second None should give up with a refusal; flash=%q level=%v", m.flash, m.flashLevel)
	}
}

// TestPublishConflictInspectUnreachableFallsBack: if the classification read
// itself fails (edge unreachable), the model surfaces the transport error rather
// than a bogus attribution, and mutates nothing.
func TestPublishConflictInspectUnreachableFallsBack(t *testing.T) {
	m := newPublishModel(t, nil)
	m.pendingPublish = pendingPublish{hostname: "app.example.com", label: "dev-box", port: 8080}

	res, _ := m.Update(inspectConflictMsg{port: 8080, hostname: "app.example.com", err: caddyedge.ErrUnreachable})
	m = res.(model)
	if m.pending != 0 {
		t.Errorf("a failed classification should clear pending; got %d", m.pending)
	}
	if m.flashLevel != flashError || !strings.Contains(m.flash, "unreachable") {
		t.Errorf("classification failure flash=%q level=%v, want the mapped transport error", m.flash, m.flashLevel)
	}
}

// TestPublishErrText maps caddyedge sentinels to friendly text.
func TestPublishErrText(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{caddyedge.ErrUnreachable, "unreachable"},
		{caddyedge.ErrNotFound, "not found"},
		{caddyedge.ErrConcurrentUpdate, "concurrently"},
		{caddyedge.ErrHostnameConflict, "already published"},
	}
	for _, c := range cases {
		if got := publishErrText(c.err); !strings.Contains(got, c.want) {
			t.Errorf("publishErrText(%v) = %q, want it to contain %q", c.err, got, c.want)
		}
	}
}

// TestPublishTickNeverStops: the poll ticker reschedules on every tick,
// configured or not (the poll itself is what's suppressed when unconfigured).
func TestPublishTickNeverStops(t *testing.T) {
	m := newPublishModel(t, nil)
	_, cmd := m.Update(publishTickMsg{})
	if cmd == nil {
		t.Error("publishTickMsg should always reschedule the ticker")
	}
	// Even while an op is in flight, the ticker keeps going (poll is skipped).
	m.pending = 8080
	if _, cmd := m.Update(publishTickMsg{}); cmd == nil {
		t.Error("publishTickMsg should reschedule even while pending")
	}
}

// TestPublishKeyLeavesLabelInputAlone guards the fallthrough: typing in the
// publish host step feeds publishInput, never labelInput.
func TestPublishKeyDoesNotLeakToLabelInput(t *testing.T) {
	m := newPublishModel(t, nil)
	m = mustUpdate(t, m, rkey("P"))
	m.publishInput.SetValue("") // clear the prefill to type fresh
	m = mustUpdate(t, m, rkey("abc"))
	if got := m.publishInput.Value(); got != "abc" {
		t.Errorf("typed text should land in publishInput; got %q", got)
	}
	if m.labelInput.Value() != "" {
		t.Errorf("publish keystrokes leaked into labelInput: %q", m.labelInput.Value())
	}
}

// --- force-purge / take-over confirm ladders + resume (kata 6n15) ------------

// reachConflictLadder drives an attempted publish of :8080 -> host into the
// confirm ladder for a seeded conflict and returns the model (in its confirm
// mode) plus the fake edge, asserting classification mutated nothing.
func reachConflictLadder(t *testing.T, host string, withAuth bool, seed func(fc *fakeCaddy)) (model, *fakeCaddy) {
	t.Helper()
	fc := newFakeCaddy()
	seed(fc)
	srv := httptest.NewServer(fc)
	t.Cleanup(srv.Close)

	m := newPublishModel(t, srv)
	if withAuth {
		m.cfg.Caddy.AuthUser = "admin"
		m.cfg.Caddy.AuthHash = "$2a$10$abcdefghijklmnopqrstuv"
	}
	m.pendingPublish = pendingPublish{hostname: host, label: "dev-box", port: 8080, withAuth: withAuth}
	m.pending = 8080

	res, cmd := m.Update(publishDoneMsg{port: 8080, err: caddyedge.ErrHostnameConflict})
	m = res.(model)
	icm, ok := cmd().(inspectConflictMsg)
	if !ok {
		t.Fatalf("a conflicted publish should classify; got %#v", cmd())
	}
	m = mustUpdate(t, m, icm)
	if len(fc.mutations) != 0 || len(fc.deletes) != 0 {
		t.Fatalf("opening the ladder must not mutate: mutations=%d deletes=%d", len(fc.mutations), len(fc.deletes))
	}
	return m, fc
}

// TestPurgeOwnedTakeoverResumes: an OwnedDiffBackend conflict, confirmed with a
// single y, deletes the owned route and RESUMES the publish (enableServe=false),
// which republishes OUR backend with auth rebuilt from cfg — no plaintext in the
// model — and toasts "took over <host>".
func TestPurgeOwnedTakeoverResumes(t *testing.T) {
	const host = "app.example.com"
	id := caddyedge.IDFor(host)

	m, fc := reachConflictLadder(t, host, true, func(fc *fakeCaddy) {
		fc.routes[id] = caddyedge.BuildRoute(host, "other-box", 9090, nil) // OwnedDiffBackend
	})
	if m.mode != entryConfirmPurgeOwned {
		t.Fatalf("owned conflict should open entryConfirmPurgeOwned; mode=%v", m.mode)
	}

	// One y confirms → the purge fires (mode clears, pending re-armed).
	res, cmd := m.Update(rkey("y"))
	m = res.(model)
	if m.mode != entryNone || m.pending != 8080 {
		t.Fatalf("y should commit the owned purge; mode=%v pending=%d", m.mode, m.pending)
	}
	pdm, ok := cmd().(purgeDoneMsg)
	if !ok || pdm.err != nil {
		t.Fatalf("purge cmd = %#v, want a clean purgeDoneMsg", cmd())
	}
	if !pdm.captured.HadID {
		t.Errorf("an owned route capture should have HadID=true")
	}
	if len(fc.deletes) != 1 || fc.deletes[0] != id {
		t.Errorf("the owned route should be deleted by its @id; deletes=%v", fc.deletes)
	}

	// Feeding purgeDoneMsg resumes the takeover publish (enableServe is baked into
	// publishCmd; here we assert the model carries the takeover flag and no secret).
	res, cmd = m.Update(pdm)
	m = res.(model)
	if m.takeoverHost != host || m.pending != 8080 {
		t.Fatalf("purge success should resume the takeover; takeoverHost=%q pending=%d", m.takeoverHost, m.pending)
	}
	if m.publishCredPass != "" || m.publishCredUser != "" {
		t.Errorf("no plaintext credential may survive into the takeover resume")
	}
	// (kata dw57) The same purge success ALSO starts the poof, fire-and-forget,
	// WITHOUT altering the control flow just asserted above.
	if m.poof == nil {
		t.Fatal("purge success should start the poof")
	}
	if !strings.Contains(m.poof.text, host) {
		t.Errorf("poof descriptor = %q, want it to name %s", m.poof.text, host)
	}
	if !m.poofTicking {
		t.Error("purge success should start the poof ticker")
	}
	pubdone := resumePublishDone(t, cmd)
	if pubdone.err != nil {
		t.Fatalf("resume publish = %#v, want a clean publishDoneMsg", pubdone)
	}
	// The route now points at OUR backend and carries auth rebuilt from cfg.
	rt, present := fc.routes[id]
	if !present {
		t.Fatalf("takeover should have republished our route")
	}
	if got := routeDial(rt); got != "dev-box:8080" {
		t.Errorf("takeover route dial = %q, want dev-box:8080", got)
	}
	if !routeHasAuth(rt) {
		t.Errorf("takeover route should carry auth rebuilt from cfg (withAuth)")
	}

	// The takeover publish's success toasts "took over <host>" and clears the flag.
	res, _ = m.Update(pubdone)
	m = res.(model)
	if m.flashLevel != flashInfo || !strings.Contains(m.flash, "took over "+host) {
		t.Errorf("takeover success toast = %q level=%v, want a plain 'took over %s'", m.flash, m.flashLevel, host)
	}
	if m.takeoverHost != "" {
		t.Errorf("takeoverHost should be cleared after the toast; got %q", m.takeoverHost)
	}
}

// TestPurgeForeignLadderTypedGate: a ForeignOverlap opens the scary TWO-gate
// ladder — a y/n drift warning, then a typed-"purge" commit. Only an exact
// "purge" fires; a wrong word or an empty enter cancels with no mutation.
func TestPurgeForeignLadderTypedGate(t *testing.T) {
	const host = "app.example.com"
	seedForeign := func(fc *fakeCaddy) {
		foreign := caddyedge.BuildRoute(host, "3rd-party", 7000, nil)
		foreign.ID = "foreign-app" // a non-tailport @id → foreign
		fc.routes[foreign.ID] = foreign
	}

	t.Run("y advances to the typed gate; a wrong word cancels with no mutation", func(t *testing.T) {
		m, fc := reachConflictLadder(t, host, false, seedForeign)
		if m.mode != entryConfirmPurgeForeign {
			t.Fatalf("foreign conflict should open entryConfirmPurgeForeign; mode=%v", m.mode)
		}
		m = mustUpdate(t, m, rkey("y"))
		if m.mode != entryConfirmPurgeForeignType {
			t.Fatalf("y on the drift warning should advance to the typed gate; mode=%v", m.mode)
		}
		m = mustUpdate(t, m, rkey("nope")) // wrong word feeds the input
		res, cmd := m.Update(enterKey)
		m = res.(model)
		if m.mode != entryNone {
			t.Errorf("a wrong typed word must cancel; mode=%v", m.mode)
		}
		if cmd != nil {
			if _, isPurge := cmd().(purgeDoneMsg); isPurge {
				t.Error("a wrong typed word must NOT fire a purge")
			}
		}
		if len(fc.deletes) != 0 || len(fc.mutations) != 0 {
			t.Errorf("a cancelled foreign purge must not mutate; deletes=%d mutations=%d", len(fc.deletes), len(fc.mutations))
		}
	})

	t.Run("empty enter cancels the typed gate", func(t *testing.T) {
		m, fc := reachConflictLadder(t, host, false, seedForeign)
		m = mustUpdate(t, m, rkey("y"))
		m = mustUpdate(t, m, enterKey) // empty input, enter
		if m.mode != entryNone {
			t.Errorf("empty enter must cancel the typed gate; mode=%v", m.mode)
		}
		if len(fc.deletes) != 0 {
			t.Errorf("empty enter must not purge; deletes=%d", len(fc.deletes))
		}
	})

	t.Run("exact purge commits, deletes the foreign route, and resumes the takeover", func(t *testing.T) {
		m, fc := reachConflictLadder(t, host, false, seedForeign)
		m = mustUpdate(t, m, rkey("y"))
		m = mustUpdate(t, m, rkey("purge")) // exact word feeds the input
		res, cmd := m.Update(enterKey)
		m = res.(model)
		if m.mode != entryNone || m.pending != 8080 {
			t.Fatalf("exact purge should commit; mode=%v pending=%d", m.mode, m.pending)
		}
		pdm, ok := cmd().(purgeDoneMsg)
		if !ok || pdm.err != nil {
			t.Fatalf("purge cmd = %#v, want a clean purgeDoneMsg", cmd())
		}
		if len(fc.deletes) != 1 || fc.deletes[0] != "foreign-app" {
			t.Errorf("the foreign route should be deleted by its @id; deletes=%v", fc.deletes)
		}
		res, cmd = m.Update(pdm)
		m = res.(model)
		if m.takeoverHost != host {
			t.Fatalf("purge success should resume the takeover; takeoverHost=%q", m.takeoverHost)
		}
		if m.poof == nil || !strings.Contains(m.poof.text, host) {
			t.Errorf("purge success should start the poof naming %s; got %#v", host, m.poof)
		}
		pubdone := resumePublishDone(t, cmd)
		if pubdone.err != nil {
			t.Fatalf("resume publish = %#v, want a clean publishDoneMsg", pubdone)
		}
		if got := routeDial(fc.routes[caddyedge.IDFor(host)]); got != "dev-box:8080" {
			t.Errorf("takeover route dial = %q, want dev-box:8080", got)
		}
	})
}

// TestPurgeCaseInsensitiveTypedWord: the typed gate accepts "PURGE"/" Purge "
// (trimmed, case-insensitive), mirroring the SSH unlock gate exactly.
func TestPurgeCaseInsensitiveTypedWord(t *testing.T) {
	const host = "app.example.com"
	m, fc := reachConflictLadder(t, host, false, func(fc *fakeCaddy) {
		foreign := caddyedge.BuildRoute(host, "3rd-party", 7000, nil)
		foreign.ID = "foreign-app"
		fc.routes[foreign.ID] = foreign
	})
	m = mustUpdate(t, m, rkey("y"))
	m.purgeInput.SetValue("  PuRgE  ") // trimmed + case-folded should still commit
	res, cmd := m.Update(enterKey)
	m = res.(model)
	if m.mode != entryNone || m.pending != 8080 {
		t.Fatalf("a trimmed/case-variant 'purge' should commit; mode=%v pending=%d", m.mode, m.pending)
	}
	if _, ok := cmd().(purgeDoneMsg); !ok {
		t.Fatalf("commit should fire a purge; got %#v", cmd())
	}
	_ = fc
}

// TestPurgeConflictChangedReopensLadder: if the route escalates owned→foreign
// between the owned confirm and the delete, PurgeConflict returns
// ErrConflictChanged, nothing is deleted, and the handler re-classifies and
// re-opens the SCARIER foreign ladder rather than deleting drift under the
// weaker confirm.
func TestPurgeConflictChangedReopensLadder(t *testing.T) {
	const host = "app.example.com"
	id := caddyedge.IDFor(host)

	m, fc := reachConflictLadder(t, host, false, func(fc *fakeCaddy) {
		fc.routes[id] = caddyedge.BuildRoute(host, "other-box", 9090, nil) // owned
	})
	if m.mode != entryConfirmPurgeOwned {
		t.Fatalf("want entryConfirmPurgeOwned; got %v", m.mode)
	}

	// Confirm → purge cmd. Between confirm and execution the route becomes FOREIGN
	// (its @id was replaced by a hand edit).
	res, cmd := m.Update(rkey("y"))
	m = res.(model)
	delete(fc.routes, id)
	foreign := caddyedge.BuildRoute(host, "3rd-party", 7000, nil)
	foreign.ID = "foreign-app"
	fc.routes["foreign-app"] = foreign

	pdm, ok := cmd().(purgeDoneMsg)
	if !ok || !errors.Is(pdm.err, caddyedge.ErrConflictChanged) {
		t.Fatalf("purge should report ErrConflictChanged; got %#v", cmd())
	}
	if len(fc.deletes) != 0 {
		t.Errorf("an escalated route must not be deleted; deletes=%v", fc.deletes)
	}

	// The handler re-classifies → re-opens the scarier foreign ladder.
	res, cmd = m.Update(pdm)
	m = res.(model)
	if m.pending != 8080 {
		t.Errorf("re-classify should re-arm pending; got %d", m.pending)
	}
	icm, ok := cmd().(inspectConflictMsg)
	if !ok {
		t.Fatalf("ErrConflictChanged should re-issue inspectConflictCmd; got %#v", cmd())
	}
	m = mustUpdate(t, m, icm)
	if m.mode != entryConfirmPurgeForeign {
		t.Errorf("a route that became foreign must re-open the scarier foreign ladder; mode=%v", m.mode)
	}
}

// TestPurgeNoConflictRetriesPublish: if the conflict cleared before the delete
// (ErrNoConflict), the handler reuses the qfbf None path — re-classify, which
// finds nothing and retries the plain publish once.
func TestPurgeNoConflictRetriesPublish(t *testing.T) {
	const host = "app.example.com"
	m := newPublishModel(t, nil)
	m.pendingPublish = pendingPublish{hostname: host, label: "dev-box", port: 8080, retried: false}
	m.pending = 8080

	// Feed an ErrNoConflict purgeDoneMsg directly (no edge needed): it re-issues a
	// classification cmd and re-arms pending.
	res, cmd := m.Update(purgeDoneMsg{hostname: host, port: 8080, err: caddyedge.ErrNoConflict})
	m = res.(model)
	if m.pending != 8080 {
		t.Errorf("ErrNoConflict should re-arm pending for the re-classify; got %d", m.pending)
	}
	if cmd == nil {
		t.Fatal("ErrNoConflict should re-issue a classification cmd")
	}

	// A None classification then retries the plain publish once (the qfbf path).
	res, cmd = m.Update(inspectConflictMsg{port: 8080, hostname: host, info: caddyedge.ConflictInfo{Kind: caddyedge.None}})
	m = res.(model)
	if !m.pendingPublish.retried {
		t.Error("the None path should mark the retry as fired")
	}
	if cmd == nil {
		t.Error("the None path should retry the plain publish")
	}
}

// TestPurgeUnreachableToasts: a transport failure on the purge is a toast with
// nothing half-done (no re-classify loop).
func TestPurgeUnreachableToasts(t *testing.T) {
	const host = "app.example.com"
	m := newPublishModel(t, nil)
	m.pendingPublish = pendingPublish{hostname: host, label: "dev-box", port: 8080}
	m.pending = 8080

	res, _ := m.Update(purgeDoneMsg{hostname: host, port: 8080, err: caddyedge.ErrUnreachable})
	m = res.(model)
	if m.pending != 0 {
		t.Errorf("a failed purge should clear pending; got %d", m.pending)
	}
	if m.flashLevel != flashError || !strings.Contains(m.flash, "unreachable") {
		t.Errorf("purge failure flash=%q level=%v, want the mapped transport error", m.flash, m.flashLevel)
	}
	if m.mode != entryNone {
		t.Errorf("a failed purge must not leave a confirm mode open; mode=%v", m.mode)
	}
}

// TestPurgeForeignConfirmDisclosesBlastRadius (roborev hped #3): a foreign
// wildcard / multi-host / catch-all route's confirm names every host pattern it
// carries, so the user isn't deleting unrelated public hostnames blind. The
// disclosure rides BOTH foreign gates (the y/n warning and the typed commit).
func TestPurgeForeignConfirmDisclosesBlastRadius(t *testing.T) {
	const host = "app.example.com"

	t.Run("wildcard is named", func(t *testing.T) {
		m, _ := reachConflictLadder(t, host, false, func(fc *fakeCaddy) {
			fc.routes["foreign-wild"] = caddyedge.Route{
				ID:     "foreign-wild",
				Match:  []caddyedge.Match{{Host: []string{"*.example.com"}}},
				Handle: []caddyedge.Handler{{Handler: "reverse_proxy", Upstreams: []caddyedge.Upstream{{Dial: "10.0.0.5:80"}}}},
			}
		})
		if m.mode != entryConfirmPurgeForeign {
			t.Fatalf("want entryConfirmPurgeForeign; got %v", m.mode)
		}
		if got := stripANSI(m.renderBottom()); !strings.Contains(got, "also serves: *.example.com") {
			t.Errorf("first gate = %q, want it to name the wildcard *.example.com", got)
		}
		// Advance to the typed gate — the disclosure must persist there too.
		m = mustUpdate(t, m, rkey("y"))
		if got := stripANSI(m.renderBottom()); !strings.Contains(got, "*.example.com") {
			t.Errorf("typed gate = %q, want it to still name the wildcard", got)
		}
	})

	t.Run("multi-host names the OTHER hosts", func(t *testing.T) {
		m, _ := reachConflictLadder(t, host, false, func(fc *fakeCaddy) {
			fc.routes["foreign-multi"] = caddyedge.Route{
				ID:     "foreign-multi",
				Match:  []caddyedge.Match{{Host: []string{host, "other.example.com", "third.example.com"}}},
				Handle: []caddyedge.Handler{{Handler: "reverse_proxy", Upstreams: []caddyedge.Upstream{{Dial: "10.0.0.6:80"}}}},
			}
		})
		got := stripANSI(m.renderBottom())
		if !strings.Contains(got, "other.example.com") || !strings.Contains(got, "third.example.com") {
			t.Errorf("multi-host confirm = %q, want it to name other/third.example.com", got)
		}
	})

	t.Run("bare star catch-all is called out prominently", func(t *testing.T) {
		m, _ := reachConflictLadder(t, host, false, func(fc *fakeCaddy) {
			fc.routes["foreign-catchall"] = caddyedge.Route{
				ID:     "foreign-catchall",
				Match:  []caddyedge.Match{{Host: []string{"*"}}},
				Handle: []caddyedge.Handler{{Handler: "reverse_proxy", Upstreams: []caddyedge.Upstream{{Dial: "10.0.0.7:80"}}}},
			}
		})
		if got := stripANSI(m.renderBottom()); !strings.Contains(got, "CATCH-ALL") || !strings.Contains(got, "EVERY hostname") {
			t.Errorf("catch-all confirm = %q, want a prominent CATCH-ALL callout", got)
		}
	})
}

// TestTakeoverResumeFailurePartialToast (roborev hped #5 / ve95 FIX 3): when the
// RESUMED take-over publish fails, the toast must disclose the partial outcome —
// the old route WAS purged — without over-claiming the host is now "unpublished".
// That claim isn't proven (a conflict means another route may hold the name; a
// transport error may have committed server-side), so the toast reports only the
// known purge plus that the take-over's outcome is UNKNOWN, pending the poll.
func TestTakeoverResumeFailurePartialToast(t *testing.T) {
	const host = "app.example.com"
	id := caddyedge.IDFor(host)
	m, _ := reachConflictLadder(t, host, false, func(fc *fakeCaddy) {
		fc.routes[id] = caddyedge.BuildRoute(host, "other-box", 9090, nil) // OwnedDiffBackend
	})
	if m.mode != entryConfirmPurgeOwned {
		t.Fatalf("want entryConfirmPurgeOwned; got %v", m.mode)
	}

	// y commits the purge; run the purge cmd to get the clean purgeDoneMsg.
	res, cmd := m.Update(rkey("y"))
	m = res.(model)
	pdm, ok := cmd().(purgeDoneMsg)
	if !ok || pdm.err != nil {
		t.Fatalf("want a clean purgeDoneMsg; got %#v", cmd())
	}
	// The success handler arms the take-over resume (takeoverHost set).
	res, _ = m.Update(pdm)
	m = res.(model)
	if m.takeoverHost != host {
		t.Fatalf("purge success should arm the take-over; takeoverHost=%q", m.takeoverHost)
	}

	// The resumed publish FAILS (a third machine grabbed it, edge blip, …). Inject
	// the failure directly rather than run the resume cmd.
	res, _ = m.Update(publishDoneMsg{port: 8080, err: caddyedge.ErrUnreachable})
	m = res.(model)
	if m.flashLevel != flashError {
		t.Errorf("a partial take-over failure should be an error toast; level=%v", m.flashLevel)
	}
	if !strings.Contains(m.flash, "purged the old route for "+host) ||
		!strings.Contains(m.flash, "take-over publish failed") ||
		!strings.Contains(m.flash, host+"'s state on the edge is now uncertain") {
		t.Errorf("partial-failure toast = %q, want it to disclose the purge + that %s's state is UNCERTAIN", m.flash, host)
	}
	// It must NOT promise the (owned-only) edge poll will reveal the state
	// (roborev cmr5-#2): a foreign route that claimed the hostname is invisible to
	// pollPublishedCmd, so that promise would be false.
	if strings.Contains(m.flash, "next edge poll") {
		t.Errorf("partial-failure toast must not promise the poll reveals the state; got %q", m.flash)
	}
	// It must NOT over-claim the host is now unpublished (roborev ve95 FIX 3): that
	// isn't proven after a failed take-over publish.
	if strings.Contains(m.flash, "is now unpublished") {
		t.Errorf("partial-failure toast must not over-claim 'unpublished'; got %q", m.flash)
	}
	if m.takeoverHost != "" {
		t.Errorf("takeoverHost must be cleared so a later ordinary error isn't mislabeled; got %q", m.takeoverHost)
	}
}

// TestPurgeCancelFlashesServeLeftOn (roborev hped #7 / OQ-Serve): cancelling a
// purge ladder flashes that serve was left on for the port — publishCmd turned it
// on before the conflicting publish, so backing out must not leave it on silently.
// Covers both the owned ladder and the foreign ladder (at both gates).
func TestPurgeCancelFlashesServeLeftOn(t *testing.T) {
	const host = "app.example.com"
	id := caddyedge.IDFor(host)
	wantFlash := "serve left on for :8080 — space to stop"

	assertServeFlash := func(t *testing.T, m model) {
		t.Helper()
		if m.mode != entryNone {
			t.Errorf("cancel should return to entryNone; mode=%v", m.mode)
		}
		if m.flashLevel != flashWarn || !strings.Contains(m.flash, wantFlash) {
			t.Errorf("cancel flash = %q level=%v, want %q at warn level", m.flash, m.flashLevel, wantFlash)
		}
	}

	t.Run("owned ladder cancel", func(t *testing.T) {
		m, fc := reachConflictLadder(t, host, false, func(fc *fakeCaddy) {
			fc.routes[id] = caddyedge.BuildRoute(host, "other-box", 9090, nil)
		})
		if m.mode != entryConfirmPurgeOwned {
			t.Fatalf("want entryConfirmPurgeOwned; got %v", m.mode)
		}
		m = mustUpdate(t, m, rkey("n")) // any non-y key cancels
		assertServeFlash(t, m)
		if len(fc.deletes) != 0 {
			t.Errorf("a cancelled purge must not delete; deletes=%v", fc.deletes)
		}
	})

	seedForeign := func(fc *fakeCaddy) {
		foreign := caddyedge.BuildRoute(host, "3rd-party", 7000, nil)
		foreign.ID = "foreign-app"
		fc.routes[foreign.ID] = foreign
	}

	t.Run("foreign first gate cancel", func(t *testing.T) {
		m, _ := reachConflictLadder(t, host, false, seedForeign)
		if m.mode != entryConfirmPurgeForeign {
			t.Fatalf("want entryConfirmPurgeForeign; got %v", m.mode)
		}
		m = mustUpdate(t, m, rkey("n"))
		assertServeFlash(t, m)
	})

	t.Run("foreign typed gate esc cancel", func(t *testing.T) {
		m, _ := reachConflictLadder(t, host, false, seedForeign)
		m = mustUpdate(t, m, rkey("y")) // advance to the typed gate
		if m.mode != entryConfirmPurgeForeignType {
			t.Fatalf("want entryConfirmPurgeForeignType; got %v", m.mode)
		}
		m = mustUpdate(t, m, escKey)
		assertServeFlash(t, m)
	})

	t.Run("foreign typed gate wrong word cancel", func(t *testing.T) {
		m, _ := reachConflictLadder(t, host, false, seedForeign)
		m = mustUpdate(t, m, rkey("y"))
		m = mustUpdate(t, m, rkey("nope"))
		m = mustUpdate(t, m, enterKey) // wrong word cancels
		assertServeFlash(t, m)
	})
}

// TestPurgeCancelReconcilesServeState (roborev ve95 FIX 4): publishCmd auto-
// enables serve BEFORE the conflicting publish, but m.active can be STALE at the
// purge confirm (the 15s poll hasn't run since the auto-enable). The cancel toast
// promises "space to stop", and the space toggle decides on/off from
// m.active[port] — so a stale false would make space try to turn serve ON again
// instead of stopping it. Drive the real publish→conflict→cancel sequence with a
// stale (false) serve state and assert the cancel reconciles it to ON, so the
// selected row reflects serve=on and space genuinely stops it.
func TestPurgeCancelReconcilesServeState(t *testing.T) {
	const host = "app.example.com"
	id := caddyedge.IDFor(host)
	m, _ := reachConflictLadder(t, host, false, func(fc *fakeCaddy) {
		fc.routes[id] = caddyedge.BuildRoute(host, "other-box", 9090, nil)
	})
	if m.mode != entryConfirmPurgeOwned {
		t.Fatalf("want entryConfirmPurgeOwned; got %v", m.mode)
	}
	// Simulate the stale window: publishCmd turned serve ON for :8080, but the poll
	// hasn't reflected it yet, so the model still believes serve is off.
	m.active[8080] = false

	m = mustUpdate(t, m, rkey("n")) // any non-y key cancels
	if m.mode != entryNone {
		t.Fatalf("cancel should return to entryNone; mode=%v", m.mode)
	}
	// Reconciled to ON: the space toggle now computes turnOn = !active = false, i.e.
	// it will STOP serve rather than try to enable it again.
	if !m.active[8080] {
		t.Errorf("cancel must reconcile serve state to ON so 'space to stop' is honest; m.active[8080]=%v", m.active[8080])
	}
}

// --- purge-undo (kata ttfh, the OWNED-only restore) --------------------------

const ttfhHost = "app.example.com"

// armedRestoreModelWith drives an OWNED purge → take-over sequence and returns a
// model with m.lastPurge ARMED, so tests can exercise R / the messages / the
// clears from a clean armed state. takeoverErr is the take-over resume's outcome
// (nil = it succeeded → takeoverLive true; non-nil = it failed → armed for retry).
// captured is a plain BuildRoute (no unmodeled field) so the ui-level fakeCaddy —
// which decodes+re-encodes on POST — round-trips it faithfully for a "Restored".
func armedRestoreModelWith(t *testing.T, srv *httptest.Server, takeoverErr error) model {
	t.Helper()
	m := newPublishModel(t, srv)
	m.pendingPublish = pendingPublish{hostname: ttfhHost, label: "dev-box", port: 8080}
	m.pending = 8080
	captured, _ := json.Marshal(caddyedge.BuildRoute(ttfhHost, "other-box", 9090, nil))
	m = mustUpdate(t, m, purgeDoneMsg{
		captured: caddyedge.Captured{Raw: captured, Hostname: ttfhHost, HadID: true},
		hostname: ttfhHost, port: 8080, deletedDesc: "other-box:9090", owned: true,
	})
	if m.pendingArm == nil || m.lastPurge != nil {
		t.Fatalf("owned purge should stash pending-arm and NOT arm yet; pendingArm=%v lastPurge=%v", m.pendingArm, m.lastPurge)
	}
	m = mustUpdate(t, m, publishDoneMsg{port: 8080, err: takeoverErr})
	if m.lastPurge == nil {
		t.Fatal("armedRestoreModelWith: slot should be armed after the take-over publishDoneMsg")
	}
	// A clean armed state for downstream tests: drop the transient poof/flash the
	// take-over left, so the restore prompt / next flash is what shows.
	m.poof, m.poofTicking = nil, false
	m.flash, m.flashLevel = "", flashInfo
	return m
}

func armedRestoreModel(t *testing.T, srv *httptest.Server) model {
	t.Helper()
	return armedRestoreModelWith(t, srv, nil)
}

// TestTtfhArmOnlyForOwnedPurge: the restore slot arms ONLY for an OWNED purge and
// ONLY after the take-over publishDoneMsg (design OQ8/F4) — even if the take-over
// FAILS; a FOREIGN purge or an id-less owned purge never arms.
func TestTtfhArmOnlyForOwnedPurge(t *testing.T) {
	captured, _ := json.Marshal(caddyedge.BuildRoute(ttfhHost, "other-box", 9090, nil))
	seed := func() model {
		m := newPublishModel(t, nil)
		m.pendingPublish = pendingPublish{hostname: ttfhHost, label: "dev-box", port: 8080}
		m.pending = 8080
		return m
	}

	t.Run("owned arms after the take-over publishDoneMsg", func(t *testing.T) {
		m := seed()
		m = mustUpdate(t, m, purgeDoneMsg{captured: caddyedge.Captured{Raw: captured, Hostname: ttfhHost, HadID: true}, hostname: ttfhHost, port: 8080, deletedDesc: "other-box:9090", owned: true})
		if m.pendingArm == nil {
			t.Fatal("owned purge should stash pending-arm")
		}
		if m.lastPurge != nil {
			t.Fatal("must NOT arm until the take-over publishDoneMsg (design F4)")
		}
		m = mustUpdate(t, m, publishDoneMsg{port: 8080})
		if m.lastPurge == nil || !m.lastPurge.takeoverLive {
			t.Fatalf("take-over success should arm a LIVE slot; lastPurge=%v", m.lastPurge)
		}
		if m.pendingArm != nil {
			t.Error("pendingArm should be consumed by the arm")
		}
		if m.lastPurge.deletedDesc != "other-box:9090" || m.lastPurge.ourLabel != "dev-box" || m.lastPurge.ourPort != 8080 {
			t.Errorf("armed slot fields wrong: %+v", m.lastPurge)
		}
	})

	t.Run("owned arms even if the take-over FAILS", func(t *testing.T) {
		m := seed()
		m = mustUpdate(t, m, purgeDoneMsg{captured: caddyedge.Captured{Raw: captured, Hostname: ttfhHost, HadID: true}, hostname: ttfhHost, port: 8080, owned: true})
		m = mustUpdate(t, m, publishDoneMsg{port: 8080, err: caddyedge.ErrHostnameConflict})
		if m.lastPurge == nil {
			t.Fatal("a FAILED take-over must still arm — the purge already happened (design F4)")
		}
		if m.lastPurge.takeoverLive {
			t.Error("a failed take-over must mark takeoverLive=false (armed for retry, not a live take-over)")
		}
	})

	t.Run("foreign purge never arms (OQ8)", func(t *testing.T) {
		m := seed()
		m = mustUpdate(t, m, purgeDoneMsg{captured: caddyedge.Captured{Raw: captured, Hostname: ttfhHost, HadID: true}, hostname: ttfhHost, port: 8080, owned: false})
		if m.pendingArm != nil {
			t.Fatal("a foreign purge must not stash pending-arm (one-way, OQ8)")
		}
		m = mustUpdate(t, m, publishDoneMsg{port: 8080})
		if m.lastPurge != nil {
			t.Fatal("a foreign purge must NEVER arm the restore slot")
		}
	})

	t.Run("owned but id-less (HadID=false) never arms", func(t *testing.T) {
		m := seed()
		m = mustUpdate(t, m, purgeDoneMsg{captured: caddyedge.Captured{Raw: captured, Hostname: ttfhHost, HadID: false}, hostname: ttfhHost, port: 8080, owned: true})
		if m.pendingArm != nil {
			t.Fatal("HadID=false must not arm — nothing to re-POST by @id")
		}
	})
}

// TestTtfhRestoreKeyDrivesABC: while armed, R runs step A (Unpublish our take-over)
// → B (RestoreRoute) → C (VerifyRestore) against a real fake edge, classifying
// Restored, and the message clears the slot.
func TestTtfhRestoreKeyDrivesABC(t *testing.T) {
	fc := newFakeCaddy()
	fc.routes[caddyedge.IDFor(ttfhHost)] = caddyedge.BuildRoute(ttfhHost, "dev-box", 8080, nil) // our live take-over
	srv := httptest.NewServer(fc)
	defer srv.Close()

	m := armedRestoreModel(t, srv)
	res, cmd := m.Update(rkey("R"))
	m = res.(model)
	if !m.restoring || m.pending != 8080 {
		t.Fatalf("R should launch the restore in-flight; restoring=%v pending=%d", m.restoring, m.pending)
	}
	if cmd == nil {
		t.Fatal("R should launch restoreCmd")
	}
	done, ok := cmd().(restoreDoneMsg)
	if !ok {
		t.Fatalf("restore cmd should yield restoreDoneMsg; got %#v", cmd())
	}
	if done.result != restoreRestored {
		t.Errorf("A→B→C should classify Restored; got %v", done.result)
	}
	// A removed our take-over; B re-created the OLD backend.
	rt, ok := fc.routes[caddyedge.IDFor(ttfhHost)]
	if !ok || routeDial(rt) != "other-box:9090" {
		t.Errorf("restore should re-create the old backend other-box:9090; got %+v", rt)
	}
	if len(fc.deletes) != 1 {
		t.Errorf("step A should have deleted exactly our take-over once; deletes=%v", fc.deletes)
	}
	m = mustUpdate(t, m, done)
	if m.lastPurge != nil {
		t.Error("a successful restore should clear the slot")
	}
	if m.restoring || m.pending != 0 {
		t.Errorf("restoreDoneMsg should clear the in-flight guards; restoring=%v pending=%d", m.restoring, m.pending)
	}
	if m.flashLevel != flashInfo || !strings.Contains(m.flash, "restored "+ttfhHost) {
		t.Errorf("flash=%q level=%v, want a plain 'restored %s'", m.flash, m.flashLevel, ttfhHost)
	}
}

// TestTtfhSplitAOutcomes: step A splits on Unpublish's result — ErrNotFound
// proceeds to B (our take-over already gone); ErrHostnameConflict ABORTS with no
// append (our take-over is still live/drifted — appending would dual-expose).
func TestTtfhSplitAOutcomes(t *testing.T) {
	t.Run("A ErrNotFound proceeds to B and restores", func(t *testing.T) {
		fc := newFakeCaddy() // our take-over is NOT present
		srv := httptest.NewServer(fc)
		defer srv.Close()
		m := armedRestoreModel(t, srv)
		_, cmd := m.Update(rkey("R"))
		done := cmd().(restoreDoneMsg)
		if done.result != restoreRestored {
			t.Errorf("A ErrNotFound should proceed to B → Restored; got %v", done.result)
		}
		if rt, ok := fc.routes[caddyedge.IDFor(ttfhHost)]; !ok || routeDial(rt) != "other-box:9090" {
			t.Errorf("B should append the captured route even when A found nothing; got %+v", rt)
		}
	})

	t.Run("A ErrHostnameConflict aborts with NO append", func(t *testing.T) {
		fc := newFakeCaddy()
		// our @id is present but drifted to a DIFFERENT backend → Unpublish refuses.
		fc.routes[caddyedge.IDFor(ttfhHost)] = caddyedge.BuildRoute(ttfhHost, "drifted-box", 1234, nil)
		srv := httptest.NewServer(fc)
		defer srv.Close()
		m := armedRestoreModel(t, srv)
		_, cmd := m.Update(rkey("R"))
		done := cmd().(restoreDoneMsg)
		if done.result != restoreTakeoverChanged {
			t.Errorf("A ErrHostnameConflict should abort with takeoverChanged; got %v", done.result)
		}
		if len(fc.routes) != 1 || routeDial(fc.routes[caddyedge.IDFor(ttfhHost)]) != "drifted-box:1234" {
			t.Errorf("an aborted restore must NOT append (no dual exposure); routes=%v", fc.routes)
		}
		if len(fc.deletes) != 0 {
			t.Errorf("A refused to delete the drifted route; deletes=%v", fc.deletes)
		}
		m = mustUpdate(t, m, done)
		if m.lastPurge != nil {
			t.Error("takeoverChanged is terminal → clear the slot")
		}
		if !strings.Contains(m.flash, "changed under you") {
			t.Errorf("flash=%q, want the take-over-changed message", m.flash)
		}
	})
}

// TestTtfhLostResponseCommitRetryReportsRestored (kata 7jy2 FIX 1): a step-B POST
// that COMMITS server-side but whose response is LOST to a transport error leaves
// the restore armed for retry. Because restoreCmd verifies FIRST, the retry sees
// the captured route already present and reports "restored" — NOT the pre-fix
// "your take-over changed under you" that step A would emit (it would see the
// restored OLD backend under our @id and refuse the Unpublish). This is the
// load-bearing test: without the VerifyRestore-first guard the retry classifies
// restoreTakeoverChanged.
func TestTtfhLostResponseCommitRetryReportsRestored(t *testing.T) {
	fc := newFakeCaddy()
	fc.routes[caddyedge.IDFor(ttfhHost)] = caddyedge.BuildRoute(ttfhHost, "dev-box", 8080, nil) // our live take-over
	fc.failNextPost = true                                                                      // step B commits, then the response is lost
	srv := httptest.NewServer(fc)
	defer srv.Close()

	m := armedRestoreModel(t, srv)

	// First R: A deletes our take-over, B POSTs the captured route (COMMITTED) but
	// the response is lost → ErrUnreachable → retryable, slot stays armed.
	res, cmd := m.Update(rkey("R"))
	m = res.(model)
	done := cmd().(restoreDoneMsg)
	if done.result != restoreUnreachable {
		t.Fatalf("a lost-response commit should be retryable (unreachable); got %v", done.result)
	}
	m = mustUpdate(t, m, done)
	if m.lastPurge == nil {
		t.Fatal("a lost-response commit must KEEP the slot armed for retry")
	}
	// The commit actually landed: the captured OLD backend is now under our @id.
	if rt, ok := fc.routes[caddyedge.IDFor(ttfhHost)]; !ok || routeDial(rt) != "other-box:9090" {
		t.Fatalf("step B should have committed the captured route despite the lost response; got %+v", rt)
	}

	// Retry R: VerifyRestore-first sees the captured route already present →
	// Restored, WITHOUT re-running A (which would refuse and misreport).
	res, cmd = m.Update(rkey("R"))
	m = res.(model)
	if !m.restoring || cmd == nil {
		t.Fatal("retry R should relaunch the restore")
	}
	done = cmd().(restoreDoneMsg)
	if done.result != restoreRestored {
		t.Errorf("retry after a lost-response commit should report Restored (not takeoverChanged); got %v", done.result)
	}
	m = mustUpdate(t, m, done)
	if m.lastPurge != nil {
		t.Error("a restored retry should clear the slot")
	}
	if m.flashLevel != flashInfo || !strings.Contains(m.flash, "restored "+ttfhHost) {
		t.Errorf("flash=%q level=%v, want a plain 'restored %s'", m.flash, m.flashLevel, ttfhHost)
	}
}

// TestTtfhMessagePerState pins each restore outcome's exact message + level and
// whether it clears or KEEPS (retryable) the slot — computed from the FINAL state,
// never a guess.
func TestTtfhMessagePerState(t *testing.T) {
	cases := []struct {
		name        string
		result      restoreResult
		wantSub     string
		wantLevel   flashLevel
		wantCleared bool
	}{
		{"restored", restoreRestored, "restored " + ttfhHost, flashInfo, true},
		{"unclaimed", restoreUnclaimed, "it's now unclaimed", flashWarn, true},
		{"claimed-by-other", restoreClaimedByOther, "another route now claims", flashWarn, true},
		{"content-mismatch", restoreContentMismatch, "its config changed", flashWarn, true},
		{"unverified", restoreUnverified, "couldn't verify the final state", flashWarn, true},
		{"takeover-changed", restoreTakeoverChanged, "changed under you", flashWarn, true},
		{"unreachable", restoreUnreachable, "edge unreachable; press R to retry", flashWarn, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := armedRestoreModel(t, nil)
			m.restoring = true
			m = mustUpdate(t, m, restoreDoneMsg{hostname: ttfhHost, result: c.result})
			if !strings.Contains(m.flash, c.wantSub) {
				t.Errorf("flash=%q, want substring %q", m.flash, c.wantSub)
			}
			if m.flashLevel != c.wantLevel {
				t.Errorf("level=%v, want %v", m.flashLevel, c.wantLevel)
			}
			if m.restoring {
				t.Error("restoreDoneMsg must clear the in-flight flag")
			}
			if c.wantCleared && m.lastPurge != nil {
				t.Errorf("%s should CLEAR the slot", c.name)
			}
			if !c.wantCleared && m.lastPurge == nil {
				t.Errorf("%s (retryable) should KEEP the slot armed", c.name)
			}
		})
	}

	t.Run("error", func(t *testing.T) {
		m := armedRestoreModel(t, nil)
		m = mustUpdate(t, m, restoreDoneMsg{hostname: ttfhHost, result: restoreError, err: caddyedge.ErrConcurrentUpdate})
		if m.flashLevel != flashError || !strings.Contains(m.flash, "couldn't restore "+ttfhHost) {
			t.Errorf("flash=%q level=%v, want an error toast", m.flash, m.flashLevel)
		}
		if m.lastPurge != nil {
			t.Error("restoreError should clear the slot")
		}
	})
}

// TestTtfhRetryableReArms: a retryable (edge-unreachable) outcome KEEPS the slot,
// re-arms the idle timer (bumps the gen), and R relaunches the restore.
func TestTtfhRetryableReArms(t *testing.T) {
	m := armedRestoreModel(t, nil)
	m.restoring = true
	genBefore := m.lastPurgeGen
	res, cmd := m.Update(restoreDoneMsg{hostname: ttfhHost, result: restoreUnreachable})
	m = res.(model)
	if m.lastPurge == nil {
		t.Fatal("unreachable is retryable → the slot must stay armed")
	}
	if m.restoring {
		t.Error("the in-flight flag must clear even on a retryable outcome")
	}
	if m.lastPurgeGen == genBefore {
		t.Error("a retryable outcome should re-arm (bump the idle-timer gen)")
	}
	if cmd == nil {
		t.Error("a retryable outcome should return a (flash + re-arm timer) batch")
	}
	res, cmd2 := m.Update(rkey("R"))
	if cmd2 == nil {
		t.Error("R should relaunch the restore after a retryable outcome")
	}
	if !res.(model).restoring {
		t.Error("R should re-enter the in-flight state")
	}
}

// TestTtfhInFlightGuard: a second R while a restore is in flight is a no-op, so two
// concurrent A/B can never fire (design F5).
func TestTtfhInFlightGuard(t *testing.T) {
	m := armedRestoreModel(t, nil)
	res, cmd1 := m.Update(rkey("R"))
	m = res.(model)
	if !m.restoring || cmd1 == nil {
		t.Fatal("first R should launch the restore")
	}
	res, cmd2 := m.Update(rkey("R"))
	if cmd2 != nil {
		t.Error("a second R while restoring must be a no-op (no concurrent A/B)")
	}
	if !res.(model).restoring {
		t.Error("the second R must not disturb the in-flight state")
	}
}

// TestTtfhRestoreKeyUnarmedIsInert: R does nothing while unarmed — it shadows no
// binding (it is not handled here and falls through to the list).
func TestTtfhRestoreKeyUnarmedIsInert(t *testing.T) {
	m := newPublishModel(t, nil)
	res, _ := m.Update(rkey("R"))
	m2 := res.(model)
	if m2.restoring || m2.lastPurge != nil {
		t.Error("R while unarmed must not start or arm a restore")
	}
	if m2.flash != "" {
		t.Errorf("R while unarmed must not raise a toast; got %q", m2.flash)
	}
}

// TestTtfhClearTriggers exercises every non-timeout clear trigger.
func TestTtfhClearTriggers(t *testing.T) {
	t.Run("de-escalation of our take-over port clears", func(t *testing.T) {
		m := armedRestoreModel(t, nil)
		m = mustUpdate(t, m, publishDoneMsg{port: 8080, unpublish: true})
		if m.lastPurge != nil {
			t.Error("unpublishing our take-over port should clear the restore slot (design F10)")
		}
	})

	t.Run("a new user publish clears", func(t *testing.T) {
		m := armedRestoreModel(t, nil)
		m.publishPort = 8080
		m.publishHostname = "new.example.com"
		m.publishEnableServe = false
		_ = m.confirmPublish()
		if m.lastPurge != nil {
			t.Error("a new user publish should clear the prior restore slot")
		}
	})

	t.Run("a new purge clears", func(t *testing.T) {
		m := armedRestoreModel(t, nil)
		m.purgeHostname = "other.example.com"
		m.purgePort = 8080
		m.purgeExpect = caddyedge.PurgeExpect{Owned: true, ID: "x"}
		_ = m.confirmPurge()
		if m.lastPurge != nil {
			t.Error("a new purge should clear the prior restore slot")
		}
	})

	t.Run("a poll showing a third-party re-take clears", func(t *testing.T) {
		m := armedRestoreModel(t, nil) // takeoverLive=true
		m = mustUpdate(t, m, publishPollMsg{published: map[int]publishInfo{}, gen: 1})
		if m.lastPurge != nil {
			t.Error("a poll showing our take-over gone should clear the slot")
		}
	})

	t.Run("a poll CONFIRMING our take-over does NOT clear", func(t *testing.T) {
		m := armedRestoreModel(t, nil)
		m = mustUpdate(t, m, publishPollMsg{published: map[int]publishInfo{8080: {hostname: ttfhHost}}, gen: 1})
		if m.lastPurge == nil {
			t.Error("a poll confirming our take-over is live must NOT clear the slot")
		}
	})

	t.Run("a failed take-over's retry slot survives a 'no route' poll", func(t *testing.T) {
		m := armedRestoreModelWith(t, nil, caddyedge.ErrHostnameConflict) // takeoverLive=false
		m = mustUpdate(t, m, publishPollMsg{published: map[int]publishInfo{}, gen: 1})
		if m.lastPurge == nil {
			t.Error("a failed take-over (armed for retry) must survive the expected 'no route of ours' poll")
		}
	})

	t.Run("navigation clears", func(t *testing.T) {
		m := armedRestoreModel(t, nil)
		m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyDown})
		if m.lastPurge != nil {
			t.Error("navigation intent should clear the restore affordance")
		}
	})

	// The grid-column left/right case returns EARLY, before the end-of-Update
	// navigation clear, so it must clear the affordance itself (kata 7jy2 FIX 4).
	t.Run("horizontal (left/right) navigation clears", func(t *testing.T) {
		for _, kt := range []tea.KeyType{tea.KeyLeft, tea.KeyRight} {
			m := armedRestoreModel(t, nil)
			m = mustUpdate(t, m, tea.KeyMsg{Type: kt})
			if m.lastPurge != nil {
				t.Errorf("%v navigation should clear the restore affordance", kt)
			}
		}
	})

	// But horizontal nav must NOT clear while a restore is in flight — the slot has
	// to survive to catch its restoreDoneMsg / retry.
	t.Run("horizontal navigation does NOT clear while restoring", func(t *testing.T) {
		m := armedRestoreModel(t, nil)
		m.restoring = true
		m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyRight})
		if m.lastPurge == nil {
			t.Error("horizontal nav must not clear the slot while a restore is in flight (design F11)")
		}
	})

	// Other early-returning selection changes must clear too (roborev nk3b-#3).
	t.Run("entering the filter (/) clears", func(t *testing.T) {
		m := armedRestoreModel(t, nil)
		m = mustUpdate(t, m, rkey("/"))
		if m.lastPurge != nil {
			t.Error("entering the filter changes selection and must clear the restore affordance")
		}
	})
	t.Run("switching all/favorites views (a) clears", func(t *testing.T) {
		m := armedRestoreModel(t, nil)
		m = mustUpdate(t, m, rkey("a"))
		if m.lastPurge != nil {
			t.Error("switching views changes selection and must clear the restore affordance")
		}
	})
}

// TestTtfhPreflightVerifyErrorStaysRetryable (roborev nk3b-#1): if the restore
// preflight VerifyRestore read fails transiently, the flow must NOT fall through
// to step A -- on the retry-after-a-committed-B path that would let Unpublish see
// the restored route, refuse, and misreport restoreTakeoverChanged, permanently
// clearing undo. A preflight transport error is retryable, keeping the slot.
func TestTtfhPreflightVerifyErrorStaysRetryable(t *testing.T) {
	fc := newFakeCaddy()
	srv := httptest.NewServer(fc)
	defer srv.Close()

	m := armedRestoreModel(t, srv)
	// Simulate a committed-but-lost B: the captured OLD backend is already back
	// under our @id (our take-over gone), and the preflight VerifyRestore GET
	// fails transiently.
	fc.mu.Lock()
	fc.routes[caddyedge.IDFor(ttfhHost)] = caddyedge.BuildRoute(ttfhHost, "other-box", 9090, nil)
	fc.failNextRoutesGet = true
	fc.mu.Unlock()

	res, cmd := m.Update(rkey("R"))
	m = res.(model)
	done := cmd().(restoreDoneMsg)
	if done.result != restoreUnreachable {
		t.Fatalf("a preflight verify failure must be retryable (unreachable), not fall through to A; got %v", done.result)
	}
	m = mustUpdate(t, m, done)
	if m.lastPurge == nil {
		t.Error("a retryable preflight failure must KEEP the slot armed for retry")
	}
	if len(fc.deletes) != 0 {
		t.Errorf("the flow must stop at the failed preflight, before step A's delete; deletes=%v", fc.deletes)
	}
}

// TestTtfhIdleTimeout: the ~60s idle timer clears the slot, a stale-gen expiry is
// ignored, and the timer is suspended while a restore is in flight.
func TestTtfhIdleTimeout(t *testing.T) {
	t.Run("matching gen clears", func(t *testing.T) {
		m := armedRestoreModel(t, nil)
		m = mustUpdate(t, m, lastPurgeExpireMsg{gen: m.lastPurgeGen})
		if m.lastPurge != nil {
			t.Error("the idle timeout should clear the slot")
		}
	})
	t.Run("stale gen is ignored", func(t *testing.T) {
		m := armedRestoreModel(t, nil)
		m = mustUpdate(t, m, lastPurgeExpireMsg{gen: m.lastPurgeGen - 1})
		if m.lastPurge == nil {
			t.Error("a superseded (stale-gen) expiry must be ignored")
		}
	})
	t.Run("suspended while restoring", func(t *testing.T) {
		m := armedRestoreModel(t, nil)
		m.restoring = true
		m = mustUpdate(t, m, lastPurgeExpireMsg{gen: m.lastPurgeGen})
		if m.lastPurge == nil {
			t.Error("the idle timer is suspended while a restore is in flight (design F11)")
		}
	})
}

// TestTtfhRestorePromptRenders: the armed slot shows the deleted route's descriptor
// and the R key in the shared status-slot action line, reads "restoring …" in
// flight, and yields to a warn/error flash.
func TestTtfhRestorePromptRenders(t *testing.T) {
	m := armedRestoreModel(t, nil)
	m.width = 80
	got := stripANSI(m.renderStatusLine())
	for _, want := range []string{ttfhHost, "other-box:9090", "press", "R", "restore"} {
		if !strings.Contains(got, want) {
			t.Errorf("restore prompt = %q, want it to contain %q", got, want)
		}
	}
	m.restoring = true
	if got := stripANSI(m.renderStatusLine()); !strings.Contains(got, "restoring "+ttfhHost) {
		t.Errorf("in-flight prompt = %q, want 'restoring …'", got)
	}
	m.restoring = false
	m.flash, m.flashLevel = "boom", flashError
	if got := stripANSI(m.renderStatusLine()); !strings.Contains(got, "boom") {
		t.Errorf("a warn/error flash must outrank the restore prompt; got %q", got)
	}
}

// TestTtfhUndoHelpPointsToRestore: the registry-undo `u` help documents that the
// force-purge restore is a SEPARATE, distinctly-keyed affordance (design OQ3).
func TestTtfhUndoHelpPointsToRestore(t *testing.T) {
	desc := keyLegendDescs(false)["u"]
	if !strings.Contains(desc, "R") || !strings.Contains(strings.ToLower(desc), "restore") {
		t.Errorf("`u` help should point to the separate R restore affordance; got %q", desc)
	}
	if !strings.Contains(desc, "does NOT touch what's exposed") {
		t.Errorf("`u` help should keep its exposure promise; got %q", desc)
	}
}
