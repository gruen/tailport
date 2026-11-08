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
	got := Clearable(cfg, served, listening, funnel, public)
	if want := []int{6000, 7000}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Clearable = %v, want %v", got, want)
	}
}
