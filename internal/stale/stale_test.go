package stale

import (
	"reflect"
	"testing"

	"github.com/gruen/tailport/internal/config"
)

func TestClearable(t *testing.T) {
	cfg := config.Config{Ports: map[int]config.PortMeta{3000: {Locked: true}}}
	served := map[int]bool{22: true, 3000: true, 4000: true, 5000: true, 6000: true, 7000: true, 8000: true, 9000: false}
	listening := map[int]bool{8000: true} // 8000: served and listening (No127 included)
	funnel := map[int]int{4000: 443}
	public := map[int]bool{5000: true}
	one := map[int]bool{}
	for p := range served {
		one[p] = true
	}
	got := Clearable(cfg, served, listening, funnel, public, one)
	if want := []int{6000, 7000}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Clearable = %v, want %v", got, want)
	}
}

// Finding 8: only a plain 1:1 serve is eligible; a remap never is, whether the
// served port, the target, both or neither is listening.
func TestClearableRequiresOneToOne(t *testing.T) {
	cfg := config.Config{}
	served := map[int]bool{15173: true, 7000: true}
	oneToOne := map[int]bool{7000: true} // 15173 is a remap to 5173
	for _, listening := range []map[int]bool{{}, {5173: true}, {15173: true}, {15173: true, 5173: true}} {
		got := Clearable(cfg, served, listening, nil, nil, oneToOne)
		if want := []int{7000}; !reflect.DeepEqual(got, want) {
			t.Errorf("listening=%v: Clearable = %v, want %v", listening, got, want)
		}
	}
	if got := Clearable(cfg, served, nil, nil, nil, nil); len(got) != 0 {
		t.Errorf("unknown shape must never be eligible, got %v", got)
	}
}
