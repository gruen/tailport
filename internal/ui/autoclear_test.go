package ui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gruen/tailport/internal/config"
	"github.com/gruen/tailport/internal/portscan"
)

// Kata 9p43: grace-period auto-clear of dangling serve forwards. Every test
// drives the model through Update with an injected clock; none touches a real
// tailscaled (serveOff is faked wherever a cmd is executed).

type acClock struct{ t time.Time }

func (c *acClock) now() time.Time          { return c.t }
func (c *acClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func acModel(cfg config.Config) (model, *acClock) {
	clk := &acClock{t: time.Unix(1_700_000_000, 0)}
	m := New(cfg)
	m.now = clk.now
	return m, clk
}

// acRefresh applies one fresh refresh: served = active, listening = listen.
func acRefresh(t *testing.T, m model, active, listen []int, funnel map[int]int) (model, tea.Cmd) {
	t.Helper()
	am := map[int]bool{}
	for _, p := range active {
		am[p] = true
	}
	var ports []portscan.Port
	for _, p := range listen {
		ports = append(ports, portscan.Port{Number: p})
	}
	if funnel == nil {
		funnel = map[int]int{}
	}
	m.refreshGen++
	res, cmd := m.Update(refreshMsg{ports: ports, active: am, funnel: funnel, gen: m.refreshGen})
	return res.(model), cmd
}

func TestAutoClearGraceTiming(t *testing.T) {
	m, clk := acModel(config.Config{})
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	if m.cleaning != 0 {
		t.Fatal("must not clear on first sight")
	}
	clk.advance(config.DefaultAutoClearStale - time.Second)
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	if m.cleaning != 0 {
		t.Fatal("must not clear before the 30s grace")
	}
	clk.advance(time.Second) // exactly 30s of continuous dangling
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	if m.cleaning != 1 {
		t.Fatalf("must clear at 30s; cleaning=%d", m.cleaning)
	}
}

func TestAutoClearTimerResetsWhenListeningReturns(t *testing.T) {
	m, clk := acModel(config.Config{})
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	clk.advance(25 * time.Second)
	m, _ = acRefresh(t, m, []int{8080}, []int{8080}, nil) // app came back
	if _, ok := m.danglingSince[8080]; ok {
		t.Fatal("listening again must drop the timer")
	}
	clk.advance(5 * time.Second)
	m, _ = acRefresh(t, m, []int{8080}, nil, nil) // dangling again: fresh timer
	clk.advance(25 * time.Second)                 // 55s since the first sight, 25s since the restart
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	if m.cleaning != 0 {
		t.Fatal("grace must count only CONTINUOUS dangling")
	}
	clk.advance(5 * time.Second)
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	if m.cleaning != 1 {
		t.Fatal("30s after the restart it must clear")
	}
}

func TestAutoClearTimerDropsWhenServeGoesAway(t *testing.T) {
	m, clk := acModel(config.Config{})
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	clk.advance(20 * time.Second)
	m, _ = acRefresh(t, m, nil, nil, nil)
	if len(m.danglingSince) != 0 {
		t.Fatalf("timer should go when the serve does: %v", m.danglingSince)
	}
}

func TestAutoClearPerPortOverrides(t *testing.T) {
	cfg := config.Config{Ports: map[int]config.PortMeta{
		1000: {AutoClearStale: "5s"},  // shorter than the global
		2000: {AutoClearStale: "off"}, // never, while the global is on
		3000: {},                      // inherits 30s
	}}
	m, clk := acModel(cfg)
	m, _ = acRefresh(t, m, []int{1000, 2000, 3000}, nil, nil)
	clk.advance(5 * time.Second)
	m, _ = acRefresh(t, m, []int{1000, 2000, 3000}, nil, nil)
	if m.cleaning != 1 {
		t.Fatalf("only :1000 is due at 5s; cleaning=%d", m.cleaning)
	}
	// Applying the result frees the in-flight guard; 2000 stays forever.
	res, _ := m.Update(cleanupDoneMsg{auto: true, cleaned: []int{1000}})
	m = res.(model)
	clk.advance(time.Hour)
	m, _ = acRefresh(t, m, []int{2000, 3000}, nil, nil)
	if m.cleaning != 1 {
		t.Fatalf("after an hour only :3000 is due (2000 is off); cleaning=%d", m.cleaning)
	}
}

func TestAutoClearGlobalOffPerPortOn(t *testing.T) {
	cfg := config.Config{
		Serve: config.ServeConfig{AutoClearStale: "off"},
		Ports: map[int]config.PortMeta{1000: {AutoClearStale: "10s"}},
	}
	m, clk := acModel(cfg)
	m, _ = acRefresh(t, m, []int{1000, 2000}, nil, nil)
	clk.advance(time.Hour)
	m, _ = acRefresh(t, m, []int{1000, 2000}, nil, nil)
	if m.cleaning != 1 {
		t.Fatalf("global off: only the per-port 10s port may clear; cleaning=%d", m.cleaning)
	}
}

func TestAutoClearExclusions(t *testing.T) {
	cfg := config.Config{Ports: map[int]config.PortMeta{22: {}, 3000: {Locked: true}}}
	m, clk := acModel(cfg)
	m.published = map[int]publishInfo{4000: {hostname: "x.example.com"}}
	m.tunnels = map[int]tunnelInfo{5000: {}}
	served := []int{22, 3000, 4000, 5000, 6000, 7000}
	m, _ = acRefresh(t, m, served, nil, map[int]int{6000: 443})
	clk.advance(time.Hour)
	m, _ = acRefresh(t, m, served, nil, map[int]int{6000: 443})
	if m.cleaning != 1 {
		t.Fatalf("only :7000 (no lock/funnel/publish/tunnel, not :22) is eligible; cleaning=%d", m.cleaning)
	}
	if got := m.clearableStale(); len(got) != 1 || got[0] != 7000 {
		t.Fatalf("clearableStale = %v, want [7000]", got)
	}
	// C keeps its unfiltered semantics.
	if got := m.danglingPorts(); len(got) != len(served) {
		t.Fatalf("danglingPorts must stay unfiltered, got %v", got)
	}
}

func TestAutoClearNo127StaleIsNeverCleared(t *testing.T) {
	m, clk := acModel(config.Config{})
	// Served and something IS listening (e.g. bound ::1 only): not dangling.
	m, _ = acRefresh(t, m, []int{8080}, []int{8080}, nil)
	clk.advance(time.Hour)
	m, _ = acRefresh(t, m, []int{8080}, []int{8080}, nil)
	if m.cleaning != 0 || len(m.danglingSince) != 0 {
		t.Fatalf("a listening port must never be tracked or cleared: cleaning=%d since=%v", m.cleaning, m.danglingSince)
	}
}

func TestAutoClearStaleGenerationRefreshDoesNotAdvanceTimers(t *testing.T) {
	m, clk := acModel(config.Config{})
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	m.supersedeRefreshes()
	stale := m.refreshFloor - 1
	clk.advance(time.Hour)
	res, cmd := m.Update(refreshMsg{active: map[int]bool{8080: true}, funnel: map[int]int{}, gen: stale})
	m = res.(model)
	if cmd != nil || m.cleaning != 0 {
		t.Fatal("a dropped stale refresh must not clear anything")
	}
	// ...and must not START a timer either.
	m2, clk2 := acModel(config.Config{})
	m2.refreshFloor = 5
	res, _ = m2.Update(refreshMsg{active: map[int]bool{9090: true}, funnel: map[int]int{}, gen: 4})
	m2 = res.(model)
	clk2.advance(time.Hour)
	if len(m2.danglingSince) != 0 {
		t.Fatalf("a stale refresh started a timer: %v", m2.danglingSince)
	}
}

func TestAutoClearStatusErrorResetsTimers(t *testing.T) {
	m, clk := acModel(config.Config{})
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	clk.advance(29 * time.Second)
	m.refreshGen++
	res, _ := m.Update(refreshMsg{statusErr: errors.New("tailscale down"), gen: m.refreshGen})
	m = res.(model)
	if len(m.danglingSince) != 0 {
		t.Fatal("unknown serve state must not count as continuous dangling")
	}
}

func TestAutoClearToastText(t *testing.T) {
	cfg := config.Config{Ports: map[int]config.PortMeta{8080: {AutoClearStale: "2m"}}}
	m, _ := acModel(cfg)
	m2, _ := acModel(config.Config{})
	if got, want := m2.autoClearToast([]int{8080}), "cleared stale forward :8080 (nothing listened for 30s) — t to re-serve"; got != want {
		t.Errorf("toast = %q, want %q", got, want)
	}
	if got, want := m.autoClearToast([]int{8080}), "cleared stale forward :8080 (nothing listened for 2m) — t to re-serve"; got != want {
		t.Errorf("toast = %q, want %q", got, want)
	}
	res, _ := m2.Update(cleanupDoneMsg{auto: true, cleaned: []int{8080}})
	if got := res.(model).flash; got != "cleared stale forward :8080 (nothing listened for 30s) — t to re-serve" {
		t.Errorf("flash after auto-clear = %q", got)
	}
	// A manual C batch does not use the auto toast.
	res, _ = m2.Update(cleanupDoneMsg{cleaned: []int{8080}})
	if got := res.(model).flash; got != "" {
		t.Errorf("manual clean should not raise the auto toast, got %q", got)
	}
}

func TestAutoClearFailureRestartsGraceAndToastsError(t *testing.T) {
	m, clk := acModel(config.Config{})
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	clk.advance(30 * time.Second)
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	res, _ := m.Update(cleanupDoneMsg{auto: true, failed: []int{8080}, err: errors.New("could not clean :8080")})
	m = res.(model)
	if m.cleaning != 0 || m.flashLevel != flashError {
		t.Fatalf("failure should free the guard and raise an error toast: cleaning=%d level=%v", m.cleaning, m.flashLevel)
	}
	clk.advance(time.Second)
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	if m.cleaning != 0 {
		t.Fatal("a failed teardown must wait out a fresh grace, not retry on the next refresh")
	}
}

func TestAutoClearRespectsInFlightGuards(t *testing.T) {
	m, clk := acModel(config.Config{})
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	clk.advance(time.Minute)
	m.pending = 9999 // a toggle is in flight
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	if m.cleaning != 0 {
		t.Fatal("must not auto-clear while a toggle is pending")
	}
}

// The cmd the refresh returns really tears the port down through the shared
// cleanupPorts machinery (serveOff), tagged auto.
func TestAutoClearCmdUnservesViaSharedTeardown(t *testing.T) {
	var got []int
	old := serveOff
	serveOff = func(p int) error { got = append(got, p); return nil }
	t.Cleanup(func() { serveOff = old })

	m, clk := acModel(config.Config{})
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	clk.advance(30 * time.Second)
	_, cmd := acRefresh(t, m, []int{8080}, nil, nil)

	msgs := make(chan tea.Msg, 8)
	var run func(c tea.Cmd)
	run = func(c tea.Cmd) {
		if c == nil {
			return
		}
		go func() {
			msg := c()
			if b, ok := msg.(tea.BatchMsg); ok {
				for _, sub := range b {
					run(sub)
				}
				return
			}
			msgs <- msg
		}()
	}
	run(cmd)
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-msgs:
			if d, ok := msg.(cleanupDoneMsg); ok {
				if !d.auto || len(d.cleaned) != 1 || d.cleaned[0] != 8080 || len(got) != 1 || got[0] != 8080 {
					t.Fatalf("done=%+v unserved=%v", d, got)
				}
				return
			}
		case <-deadline:
			t.Fatal("no cleanupDoneMsg from the auto-clear cmd")
		}
	}
}

func TestInvalidAutoClearValueWarnsOnce(t *testing.T) {
	m, _ := acModel(config.Config{Serve: config.ServeConfig{AutoClearStale: "soon"}})
	m, _ = acRefresh(t, m, nil, nil, nil)
	if !strings.Contains(m.flash, `serve.auto_clear_stale "soon"`) || m.flashLevel != flashWarn {
		t.Fatalf("expected a warn toast naming the bad value, got %q (%v)", m.flash, m.flashLevel)
	}
	m.flash = ""
	m, _ = acRefresh(t, m, nil, nil, nil)
	if m.flash != "" {
		t.Fatal("the warning must be one-shot")
	}
}

// Docs pins (AGENTS.md "Docs stay honest"): each claim below is made in the
// README / the C help text and fails here if the behavior or the prose drifts.
func TestAutoClearDocsMatchBehavior(t *testing.T) {
	if config.DefaultAutoClearStale != 30*time.Second {
		t.Fatalf("docs promise a 30s default, code has %v", config.DefaultAutoClearStale)
	}
	help := keyLegendDescs(false)["C"]
	for _, want := range []string{"30s", "auto_clear_stale", "tailport clean", ":22", "locked"} {
		if !strings.Contains(help, want) {
			t.Errorf("C help should mention %q: %q", want, help)
		}
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := string(b)
	for _, want := range []string{
		"serve.auto_clear_stale",
		"ports.<n>.auto_clear_stale",
		"`tailport clean [--dry-run]`",
		"cleared stale forward :P (nothing listened for\n30s) — t to re-serve",
		"address already in use",
		"`0` / `off`",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("README is missing %q", want)
		}
	}
}
