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

// TestHelpViewUsesSharedKeyLegend covers the single-source-of-truth invariant
// (kata x4cg, evolved by p39s): the in-TUI "?" overlay's key sections are
// exactly RenderKeyLegendGroups(KeyLegendGroups(m.emoji)) -- not a hand-copied
// duplicate -- so it and `tailport quickstart` (cmd/tailport, which calls the
// same two functions) can never drift apart. Checked in both marker modes,
// since the space/p/C rows quote the mode-specific exposure glyph.
func TestHelpViewUsesSharedKeyLegend(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	for _, emoji := range []bool{false, true} {
		m := New(config.Config{})
		m.emoji = emoji

		want := RenderKeyLegendGroups(KeyLegendGroups(emoji))
		if got := m.helpView(); !strings.Contains(got, want) {
			t.Errorf("helpView() (emoji=%v) does not contain RenderKeyLegendGroups(KeyLegendGroups(%v)) verbatim.\nwant substring:\n%s\ngot:\n%s", emoji, emoji, want, got)
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

// TestCopyURL covers vnq7's copy action and toast: "c" copies the tailnet URL
// (with a success toast when exposed, an amber caveat when not), and the toast
// clears on the next keypress.
func TestCopyURL(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	newModel := func(active bool) model {
		m := New(config.Config{Ports: map[int]config.PortMeta{8080: {Favorite: true}}})
		m.host = "host"
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

	// Exposed: success toast naming the tailnet URL, not amber, with a cmd.
	m := newModel(true)
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	m = res.(model)
	if cmd == nil {
		t.Error("c should return a copy/flash cmd")
	}
	if !strings.Contains(m.flash, "copied") || !strings.Contains(m.flash, "http://host:8080") {
		t.Errorf("flash = %q, want a copied-URL toast", m.flash)
	}
	if m.flashLevel != flashInfo {
		t.Errorf("exposed copy should be an info toast; level = %v", m.flashLevel)
	}
	// The next keypress dismisses the toast.
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if got := res.(model); got.flash != "" {
		t.Errorf("toast should clear on the next key; got %q", got.flash)
	}

	// Not exposed: still copies, but the toast warns it won't resolve yet.
	m = newModel(false)
	res, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	m = res.(model)
	if m.flashLevel != flashWarn || !strings.Contains(m.flash, "not exposed") {
		t.Errorf("not-exposed copy flash = %q (level=%v), want a warn NOT-exposed caveat", m.flash, m.flashLevel)
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
	// Host on the listening segment (20w6), funnel count labelled "public
	// (funnel)" (67zk), no trailing "— host".
	for _, want := range []string{"5 listening on host", "2 exposed on tailnet", "1 public (funnel)"} {
		if !strings.Contains(got, want) {
			t.Errorf("statusText = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "— host") {
		t.Errorf("statusText should not use the trailing '— host' form; got %q", got)
	}

	// Zero of everything reads cleanly, not "no ports"; no host -> no "on".
	empty := model{host: "", width: 120}
	if got := empty.statusText(); got != "0 listening · 0 exposed on tailnet · 0 public (funnel)" {
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
	// (p39s: "Favorites" now legitimately appears as a key-group column header,
	// so match on "All ports" -- the toggle's distinguishing segment, which the
	// grouped legend never contains.)
	if bottom := m.renderBottom(); strings.Contains(bottom, "All ports") {
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

	// The overlay names where settings save and shows that exact path.
	view := stripANSI(m.helpView())
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

// TestMarkerGlyph covers sqvm: the exposure-state marker resolves to the egg
// lifecycle in emoji mode and the styled ASCII fallback otherwise, with funnel
// outranking a dangling forward, which outranks a healthy serve.
func TestMarkerGlyph(t *testing.T) {
	cases := []struct {
		name                 string
		active, listening    bool
		funnel               int
		wantEmoji, wantASCII string
	}{
		{"idle", false, false, 0, "🥚", "○"},
		{"tailnet", true, true, 0, "🐣", "◉"},
		{"dangling", true, false, 0, "🪹", "▲"},
		{"funnel", true, true, 443, "🐦", "●"},
		{"funnel outranks dangling", true, false, 443, "🐦", "●"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			em := portItem{active: c.active, listening: c.listening, funnelPublic: c.funnel, emoji: true}
			got := em.markerGlyph()
			if !strings.Contains(got, c.wantEmoji) {
				t.Errorf("emoji marker = %q, want to contain %q", got, c.wantEmoji)
			}
			if lipgloss.Width(got) < 2 {
				t.Errorf("emoji marker %q should pad to a 2-cell column, width=%d", got, lipgloss.Width(got))
			}
			as := portItem{active: c.active, listening: c.listening, funnelPublic: c.funnel, emoji: false}
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

// TestDanglingDescription covers km8x: a served-but-not-listening row explains
// itself -- names the loopback target and the un-expose key -- while healthy and
// not-exposed rows keep their plain descriptions.
func TestDanglingDescription(t *testing.T) {
	dangling := portItem{port: portscan.Port{Number: 8025}, active: true, listening: false, host: "host"}
	got := stripANSI(dangling.Description())
	// Names why it looks exposed-yet-empty (tailscale still holds the port) and
	// the key to release it. The loopback fix lives in ? help / README.
	for _, want := range []string{"bound to tailscale", "space", "release"} {
		if !strings.Contains(got, want) {
			t.Errorf("dangling description %q should mention %q", got, want)
		}
	}

	// Healthy serve: the tailnet URL, no scary hint.
	healthy := portItem{port: portscan.Port{Number: 8025}, active: true, listening: true, host: "host"}
	if got := stripANSI(healthy.Description()); got != "http://host:8025" {
		t.Errorf("healthy description = %q, want the tailnet URL", got)
	}

	// Not exposed: unchanged.
	idle := portItem{port: portscan.Port{Number: 8025}}
	if got := idle.Description(); got != "not exposed" {
		t.Errorf("idle description = %q, want %q", got, "not exposed")
	}
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

// TestResolveEmoji covers the markers-mode resolution: emoji/ascii force it,
// auto (or empty) defers to the terminal heuristic (UTF-8 locale + sane TERM).
func TestResolveEmoji(t *testing.T) {
	if !resolveEmoji("emoji") {
		t.Error("markers=emoji should force emoji")
	}
	if resolveEmoji("ascii") {
		t.Error("markers=ascii should force ascii")
	}

	// auto: a UTF-8 locale on a normal terminal -> emoji.
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_CTYPE", "")
	t.Setenv("LANG", "en_US.UTF-8")
	if !resolveEmoji("auto") {
		t.Error("auto with UTF-8 LANG on xterm should resolve emoji")
	}
	if !resolveEmoji("") {
		t.Error("empty (=auto) with UTF-8 should resolve emoji")
	}

	// The bare Linux console can't render emoji, even with a UTF-8 locale.
	t.Setenv("TERM", "linux")
	if resolveEmoji("auto") {
		t.Error("auto on the linux console should resolve ascii")
	}

	// A non-UTF-8 locale -> ascii.
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("LANG", "C")
	if resolveEmoji("auto") {
		t.Error("auto with a non-UTF-8 locale should resolve ascii")
	}
}

// TestNewMarkersOverride covers zn2x's precedence and persistence contract
// at the New()/model level (the CLI-flag validation itself is covered in
// cmd/tailport): the override passed to New wins for rendering (m.emoji),
// but must never leak into cfg.Markers -- and therefore never into what a
// later, unrelated Save() (e.g. from favoriting a port) writes to disk.
func TestNewMarkersOverride(t *testing.T) {
	// Override wins over the persisted config value.
	m := New(config.Config{Ports: map[int]config.PortMeta{}, Markers: "emoji"}, "ascii")
	if m.emoji {
		t.Error("New(cfg{Markers:emoji}, \"ascii\") should resolve ascii (flag beats config)")
	}
	if m.cfg.Markers != "emoji" {
		t.Errorf("New should not mutate cfg.Markers: got %q, want the original %q", m.cfg.Markers, "emoji")
	}

	// No override (variadic omitted) falls back to cfg.Markers, exactly as
	// before zn2x -- the common existing-call-site case stays unaffected.
	m2 := New(config.Config{Ports: map[int]config.PortMeta{}, Markers: "emoji"})
	if !m2.emoji {
		t.Error("New(cfg{Markers:emoji}) with no override should still resolve emoji")
	}

	// An empty override (as when --markers wasn't passed at all) must not
	// clobber a real config value either.
	m3 := New(config.Config{Ports: map[int]config.PortMeta{}, Markers: "emoji"}, "")
	if !m3.emoji {
		t.Error("New(cfg{Markers:emoji}, \"\") should still resolve emoji (empty override = no override)")
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
	if m.emoji {
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
// centre-bottom within +/-15% of width, and every explosion lands inside the
// band (a few rows around the floating fanfare).
func TestNewFireworkGeometry(t *testing.T) {
	const w, h = 100, 40
	lay := eggLayout(w, h)
	bandTop := float64(lay.topFanfareRow - 3)
	bandBot := float64(lay.botFanfareRow + 3)
	maxOff := fwLaunchSpreadPct * float64(w)
	const eps = 1e-6
	for i := 0; i < 20000; i++ {
		fw := newFirework(w, h, i%2 == 0)
		if fw.y0 != float64(h-1) {
			t.Fatalf("launch row %.1f want center-bottom %d", fw.y0, h-1)
		}
		if fw.x0 < float64(w)/2-maxOff-eps || fw.x0 > float64(w)/2+maxOff+eps {
			t.Fatalf("launch col %.3f outside +/-15%% of centre", fw.x0)
		}
		if fw.yExp < bandTop-eps || fw.yExp > bandBot+eps {
			t.Fatalf("explosion row %.3f outside band [%.0f,%.0f]", fw.yExp, bandTop, bandBot)
		}
		if fw.count < 1 || fw.v0 <= 0 || fw.tExp <= 0 {
			t.Fatalf("degenerate firework: count=%d v0=%.3f tExp=%.3f", fw.count, fw.v0, fw.tExp)
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

// TestFireworkCap covers the ~25 concurrency cap: 'f' presses beyond the cap
// (while 25 are live) are ignored, and no ticker is stacked.
func TestFireworkCap(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := New(config.Config{})
	m.active = map[int]bool{}
	m.rebuildItems()
	m.width, m.height = 120, 45
	m = mustUpdate(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'E'}})

	firstCmd := true
	for i := 0; i < 60; i++ {
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

// TestEggViewGrid covers full-screen grid composition: exact dimensions, the
// egg block placed at its offset with the fanfare on its recorded rows, credits
// present, and fireworks overlaid without overflow.
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
	if strings.TrimSpace(stripANSI(lines[lay.topFanfareRow])) == "" {
		t.Error("the top fanfare row should carry sparkles")
	}
	if strings.TrimSpace(stripANSI(lines[lay.botFanfareRow])) == "" {
		t.Error("the bottom fanfare row should carry sparkles")
	}
	if strings.TrimSpace(stripANSI(lines[lay.topFanfareRow+lay.eggRows/2])) == "" {
		t.Error("an egg body row should be non-empty")
	}
	plain := stripANSI(view)
	if !strings.Contains(plain, "Michael E. Gruen") || !strings.Contains(plain, "LLM Agent Fleet") {
		t.Error("egg credits should render inside the grid")
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
// yields the five approved columns in order, and FullHelp() mirrors them one
// inner slice per column.
func TestKeyGroupsAndFullHelp(t *testing.T) {
	k := newKeyMap()
	groups := k.groups()

	wantNames := []string{"Expose", "Favorites", "Protect", "View", "App"}
	if len(groups) != len(wantNames) {
		t.Fatalf("groups() = %d columns, want %d", len(groups), len(wantNames))
	}
	wantKeys := [][]string{
		{"space", "p", "c"},
		{"f", "u", "n", "l"},
		{"x", "C"},
		{"/", "a", "r"},
		{"?", "q"},
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

// TestBottomBarGridAligned drives the real model at a wide width and asserts
// the bar renders the five grouped columns with a header row and aligned
// gutters: descriptions line up within a column and columns line up across
// rows.
func TestBottomBarGridAligned(t *testing.T) {
	m := New(config.Config{})
	m.help.Width = 100
	m.width = 100

	grid := stripANSI(m.renderLegend())
	lines := strings.Split(grid, "\n")
	if len(lines) < 5 {
		t.Fatalf("grid should be a header + up to 4 rows (>=5 lines); got %d:\n%s", len(lines), grid)
	}
	hdr, r1, r2 := lines[0], lines[1], lines[2]

	// Header row carries all five section names, in order, on one line.
	prev := -1
	for _, name := range []string{"Expose", "Favorites", "Protect", "View", "App"} {
		at := strings.Index(hdr, name)
		if at < 0 {
			t.Fatalf("header row missing %q; got %q", name, hdr)
		}
		if at <= prev {
			t.Errorf("header %q out of order (at %d, prev %d): %q", name, at, prev, hdr)
		}
		prev = at
	}

	// Columns line up across rows: each header's start == its cells' start.
	eq := func(label string, a, b int) {
		if a != b {
			t.Errorf("%s misaligned: %d vs %d\nhdr: %q\nr1:  %q\nr2:  %q", label, a, b, hdr, r1, r2)
		}
	}
	eq("Favorites/r1", strings.Index(hdr, "Favorites"), strings.Index(r1, "f favorite"))
	eq("Favorites/r2", strings.Index(hdr, "Favorites"), strings.Index(r2, "u unfavorite"))
	eq("Protect/r1", strings.Index(hdr, "Protect"), strings.Index(r1, "x lock/unlock"))
	eq("View/r1", strings.Index(hdr, "View"), strings.Index(r1, "/ filter"))
	eq("App/r1", strings.Index(hdr, "App"), strings.Index(r1, "? help"))
	eq("App/r2", strings.Index(hdr, "App"), strings.Index(r2, "q quit"))

	// Within the Expose column the key gutter aligns the descriptions: "serve"
	// (after "space ") and "funnel" (after "p     ") start at the same offset.
	eq("Expose gutter", strings.Index(r1, "serve"), strings.Index(r2, "funnel"))
}

// TestBottomBarNarrowFallback covers the responsive fallback: below the
// content-derived threshold the bar becomes a wrapped grouped bar that never
// truncates (every key+desc still present) and never overflows the width.
func TestBottomBarNarrowFallback(t *testing.T) {
	const width = 60
	m := New(config.Config{})
	m.help.Width = width
	m.width = width

	bar := stripANSI(m.renderLegend())
	lines := strings.Split(bar, "\n")

	// Not the grid: the five headers are no longer all on the first line.
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
		"Expose", "Favorites", "Protect", "View", "App",
		"space serve", "p funnel public", "c copy URL",
		"f favorite", "u unfavorite", "n add favorite", "l label",
		"x lock/unlock", "/ filter", "a switch view", "r refresh",
		"? help", "q quit",
	} {
		if !strings.Contains(bar, want) {
			t.Errorf("narrow fallback dropped %q; got:\n%s", want, bar)
		}
	}
	if strings.Contains(bar, "clean") {
		t.Errorf("C clean should be absent with no dangling; got:\n%s", bar)
	}
}

// TestProtectColumnContextualClean covers the contextual "C clean stale": the
// Protect column collapses to just "x lock/unlock" (no reserved blank slot)
// when nothing is dangling, and expands to add "C clean stale" when a dangling
// forward exists.
func TestProtectColumnContextualClean(t *testing.T) {
	m := New(config.Config{})
	m.help.Width = 100
	m.width = 100

	protect := func(groups []keyGroup) keyGroup {
		for _, g := range groups {
			if g.name == "Protect" {
				return g
			}
		}
		t.Fatal("no Protect group")
		return keyGroup{}
	}

	// No dangling -> Protect has exactly the lock binding, and the rendered bar
	// omits "clean".
	if got := len(protect(m.barGroups(false)).bindings); got != 1 {
		t.Errorf("Protect should collapse to 1 binding (x) with no dangling; got %d", got)
	}
	if noDangle := stripANSI(m.renderLegend()); strings.Contains(noDangle, "clean") {
		t.Errorf("bar should not show 'clean' with no dangling:\n%s", noDangle)
	}

	// A served-but-not-listening port is dangling -> Protect gains "C clean".
	m.active = map[int]bool{9999: true}
	if !m.hasDangling() {
		t.Fatal("setup: expected a dangling forward")
	}
	if got := len(protect(m.barGroups(true)).bindings); got != 2 {
		t.Errorf("Protect should expand to 2 bindings (x, C) with a dangling; got %d", got)
	}
	if dangle := stripANSI(m.renderLegend()); !strings.Contains(dangle, "C clean stale") {
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
		{"narrow/no-dangling", 58, 24, false, false},
		{"narrow/dangling", 58, 24, true, true},
		{"narrow/dangling-appears-after-resize", 58, 24, false, true},
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

// TestHelpOverlayGroupedSections covers the "?" overlay reorg: the same five
// sections in the same order as the bar, each with an aligned key gutter, but
// keeping the RICH per-key prose and the surrounding Markers/warnings/config
// prose.
func TestHelpOverlayGroupedSections(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := New(config.Config{})

	help := stripANSI(m.helpView())

	// Sections appear in the approved order.
	prev := -1
	for _, name := range []string{"Expose", "Favorites", "Protect", "View", "App"} {
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
