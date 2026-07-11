package ui

import (
	"reflect"
	"testing"

	"github.com/gruen/tailport/internal/portscan"
)

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
