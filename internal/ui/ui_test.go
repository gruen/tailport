package ui

import (
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

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

// TestEmptyStateMessage covers the contextual empty-view text: it names the
// current view so a blank list explains itself instead of showing "No items."
func TestEmptyStateMessage(t *testing.T) {
	m := New(config.Config{Ports: map[int]config.PortMeta{}})

	m.showAllPorts = false
	if msg := m.emptyStateMessage(); !strings.Contains(msg, "Favorites") {
		t.Errorf("favorites empty-state should mention Favorites; got %q", msg)
	}
	m.showAllPorts = true
	if msg := m.emptyStateMessage(); !strings.Contains(msg, "listening") {
		t.Errorf("all-ports empty-state should mention listening; got %q", msg)
	}
}
