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
	"github.com/gruen/tailport/internal/pausedserve"
	"github.com/gruen/tailport/internal/portscan"
	"github.com/gruen/tailport/internal/procid"
)

// Kata p2r5 (reworking 9p43): grace-period auto-PAUSE of dangling serve forwards. Every test
// drives the model through Update with an injected clock; none touches a real
// tailscaled (serveOff is faked wherever a cmd is executed).

type acClock struct{ t time.Time }

func (c *acClock) now() time.Time          { return c.t }
func (c *acClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// acRemap lists served ports acRefresh reports as NOT plain 1:1 serves.
var acRemap = map[int]bool{}

// acIdent is the default program behind a test port; acSeeded are the ports
// acModel pre-seeds with it (as if tailport had seen the app listening), so a
// test can start from "served, app just died". Ports outside it have no
// known identity.
var acIdent = procid.Identity{Exe: "/usr/bin/python3", Cwd: "/srv/a"}
var acSeeded = []int{1000, 2000, 3000, 4000, 5000, 6000, 7000, 8080, 9090, 15173}

// acBind / acListener override a test listener's bind and identity.
type acBindSpec struct {
	scope portscan.BindScope
	no127 bool
}

var (
	acBind     = map[int]acBindSpec{}
	acListener = map[int]procid.Identity{}
)

func acReset(t *testing.T) {
	acBind, acListener, acRemap = map[int]acBindSpec{}, map[int]procid.Identity{}, map[int]bool{}
	t.Cleanup(func() { acBind, acListener, acRemap = map[int]acBindSpec{}, map[int]procid.Identity{}, map[int]bool{} })
}

func acModel(cfg config.Config) (model, *acClock) {
	clk := &acClock{t: time.Now()}
	m := New(cfg)
	m.now = clk.now
	// Hermetic: no cloudflared poll is required (its presence on the dev box
	// must not gate these tests); public-state gating has its own tests.
	m.cfAvailable = false
	for _, p := range acSeeded {
		m.lastIdent[p] = acIdent
	}
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
	idents := map[int]procid.Identity{}
	for _, p := range listen {
		pt := portscan.Port{Number: p, BindScope: portscan.ScopeLoopback, Loopback: true}
		if b, ok := acBind[p]; ok {
			pt.BindScope, pt.No127 = b.scope, b.no127
		}
		ports = append(ports, pt)
		if id, ok := acListener[p]; ok {
			idents[p] = id
		} else {
			idents[p] = acIdent
		}
	}
	if funnel == nil {
		funnel = map[int]int{}
	}
	m.refreshGen++
	one := map[int]bool{}
	for p := range am {
		if !acRemap[p] {
			one[p] = true
		}
	}
	res, cmd := m.Update(refreshMsg{ports: ports, active: am, funnel: funnel, oneToOne: one, idents: idents, gen: m.refreshGen})
	return res.(model), cmd
}

func TestAutoPauseGraceTiming(t *testing.T) {
	m, clk := acModel(config.Config{})
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	if m.cleaning != 0 {
		t.Fatal("must not clear on first sight")
	}
	clk.advance(config.DefaultAutoPause - time.Second)
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	if m.cleaning != 0 {
		t.Fatal("must not pause before the 5s grace")
	}
	clk.advance(time.Second) // exactly 5s of continuous dangling
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	if m.cleaning != 1 {
		t.Fatalf("must pause at 5s; cleaning=%d", m.cleaning)
	}
}

func TestAutoPauseTimerResetsWhenListeningReturns(t *testing.T) {
	m, clk := acModel(config.Config{})
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	clk.advance(3 * time.Second)
	m, _ = acRefresh(t, m, []int{8080}, []int{8080}, nil) // app came back
	if _, ok := m.danglingSince[8080]; ok {
		t.Fatal("listening again must drop the timer")
	}
	clk.advance(1 * time.Second)
	m, _ = acRefresh(t, m, []int{8080}, nil, nil) // dangling again: fresh timer
	clk.advance(3 * time.Second)                  // 7s since the first sight, 3s since the restart
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	if m.cleaning != 0 {
		t.Fatal("grace must count only CONTINUOUS dangling")
	}
	clk.advance(2 * time.Second)
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	if m.cleaning != 1 {
		t.Fatal("5s after the restart it must pause")
	}
}

func TestAutoPauseTimerDropsWhenServeGoesAway(t *testing.T) {
	m, clk := acModel(config.Config{})
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	clk.advance(3 * time.Second)
	m, _ = acRefresh(t, m, nil, nil, nil)
	if len(m.danglingSince) != 0 {
		t.Fatalf("timer should go when the serve does: %v", m.danglingSince)
	}
}

func TestAutoPausePerPortOverrides(t *testing.T) {
	cfg := config.Config{Ports: map[int]config.PortMeta{
		1000: {AutoPause: "2s"},  // shorter than the global
		2000: {AutoPause: "off"}, // never, while the global is on
		3000: {},                 // inherits 5s
	}}
	m, clk := acModel(cfg)
	m, _ = acRefresh(t, m, []int{1000, 2000, 3000}, nil, nil)
	clk.advance(2 * time.Second)
	m, _ = acRefresh(t, m, []int{1000, 2000, 3000}, nil, nil)
	if m.cleaning != 1 {
		t.Fatalf("only :1000 is due at 2s; cleaning=%d", m.cleaning)
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

func TestAutoPauseGlobalOffPerPortOn(t *testing.T) {
	cfg := config.Config{
		Serve: config.ServeConfig{AutoPause: "off"},
		Ports: map[int]config.PortMeta{1000: {AutoPause: "10s"}},
	}
	m, clk := acModel(cfg)
	m, _ = acRefresh(t, m, []int{1000, 2000}, nil, nil)
	clk.advance(time.Hour)
	m, _ = acRefresh(t, m, []int{1000, 2000}, nil, nil)
	if m.cleaning != 1 {
		t.Fatalf("global off: only the per-port 10s port may clear; cleaning=%d", m.cleaning)
	}
}

func TestAutoPauseExclusions(t *testing.T) {
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

func TestAutoPauseNo127StaleIsNeverCleared(t *testing.T) {
	m, clk := acModel(config.Config{})
	// Served and something IS listening (e.g. bound ::1 only): not dangling.
	m, _ = acRefresh(t, m, []int{8080}, []int{8080}, nil)
	clk.advance(time.Hour)
	m, _ = acRefresh(t, m, []int{8080}, []int{8080}, nil)
	if m.cleaning != 0 || len(m.danglingSince) != 0 {
		t.Fatalf("a listening port must never be tracked or cleared: cleaning=%d since=%v", m.cleaning, m.danglingSince)
	}
}

func TestAutoPauseGenerationRefreshDoesNotAdvanceTimers(t *testing.T) {
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

func TestAutoPauseStatusErrorResetsTimers(t *testing.T) {
	m, clk := acModel(config.Config{})
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	clk.advance(4 * time.Second)
	m.refreshGen++
	res, _ := m.Update(refreshMsg{statusErr: errors.New("tailscale down"), gen: m.refreshGen})
	m = res.(model)
	if len(m.danglingSince) != 0 {
		t.Fatal("unknown serve state must not count as continuous dangling")
	}
}

func TestAutoPauseToastText(t *testing.T) {
	e := pausedserve.Entry{Port: 8080, Exe: "/usr/bin/python3.12", Cwd: "/srv/a"}
	want := "paused :8080 (app stopped) — resumes when python3.12 in /srv/a returns · t to forget"
	if got := pauseToast([]pausedserve.Entry{e}); got != want {
		t.Errorf("toast = %q, want %q", got, want)
	}
	if got, w := pauseToast([]pausedserve.Entry{e, {Port: 9000}}), "paused :8080, :9000 (apps stopped) — resume when the same apps return · t to forget"; got != w {
		t.Errorf("plural toast = %q, want %q", got, w)
	}
	m, _ := acModel(config.Config{})
	m.pausing[8080] = e
	res, _ := m.Update(cleanupDoneMsg{auto: true, cleaned: []int{8080}})
	if got := res.(model).flash; got != want {
		t.Errorf("flash after auto-pause = %q", got)
	}
	// A manual C batch neither toasts a pause nor records paused memory.
	m2, _ := acModel(config.Config{})
	res, _ = m2.Update(cleanupDoneMsg{cleaned: []int{8080}})
	if got := res.(model); got.flash != "" || len(got.paused) != 0 {
		t.Errorf("manual clean must not pause: flash=%q paused=%v", got.flash, got.paused)
	}
}

func TestAutoPauseFailureRestartsGraceAndToastsError(t *testing.T) {
	m, clk := acModel(config.Config{})
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	clk.advance(5 * time.Second)
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

func TestAutoPauseRespectsInFlightGuards(t *testing.T) {
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
func TestAutoPauseCmdUnservesViaSharedTeardown(t *testing.T) {
	var got []int
	old := serveOff
	serveOff = func(p int) error { got = append(got, p); return nil }
	t.Cleanup(func() { serveOff = old })

	m, clk := acModel(config.Config{})
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	clk.advance(5 * time.Second)
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

func TestInvalidAutoPauseValueWarnsOnce(t *testing.T) {
	m, _ := acModel(config.Config{Serve: config.ServeConfig{AutoPause: "soon"}})
	m, _ = acRefresh(t, m, nil, nil, nil)
	if !strings.Contains(m.flash, `serve.auto_pause "soon"`) || m.flashLevel != flashWarn {
		t.Fatalf("expected a warn toast naming the bad value, got %q (%v)", m.flash, m.flashLevel)
	}
	m.flash = ""
	m, _ = acRefresh(t, m, nil, nil, nil)
	if m.flash != "" {
		t.Fatal("the warning must be one-shot")
	}
}

// Docs pins (AGENTS.md "Docs stay honest"): each claim below is made in the
// README / the C and t help text and fails here if the behavior or prose drifts.
func TestAutoPauseDocsMatchBehavior(t *testing.T) {
	if config.DefaultAutoPause != 5*time.Second {
		t.Fatalf("docs promise a 5s default, code has %v", config.DefaultAutoPause)
	}
	if pausedserve.MaxAge != 7*24*time.Hour {
		t.Fatalf("docs promise a 7 day expiry, code has %v", pausedserve.MaxAge)
	}
	help := keyLegendDescs(false)["C"]
	for _, want := range []string{"PERMANENTLY", "5s", "auto_pause", "tailport clean", ":22", "locked", "remap", "resumes", "different program"} {
		if !strings.Contains(help, want) {
			t.Errorf("C help should mention %q: %q", want, help)
		}
	}
	if th := keyLegendDescs(false)["t"]; !strings.Contains(th, "PAUSED row") || !strings.Contains(th, "forgets") {
		t.Errorf("t help should explain paused rows: %q", th)
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := string(b)
	for _, want := range []string{
		"serve.auto_pause",
		"ports.<n>.auto_pause",
		"`tailport clean [--dry-run]`",
		"paused :P\n(app stopped) — resumes when <program> in <directory> returns · t to forget",
		"`resumed :P (same app is\n  back)`",
		"address already in use",
		"`0` / `off`",
		"a remap such as tailnet `15173` →\n  local `5173`",
		"`go run`\nbuilds to a fresh temporary executable",
		"paused-serves.json",
		"expires 7 days after pausing",
		"◌ paused",
		"💤 paused",
		"does not\n  touch the TUI's remembered paused serves",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("README is missing %q", want)
		}
	}
}

// Finding 8: a remapped serve is never auto-cleared, with either port up or not.
func TestAutoPauseNeverClearsRemappedServe(t *testing.T) {
	acRemap = map[int]bool{15173: true}
	t.Cleanup(func() { acRemap = map[int]bool{} })
	for _, listen := range [][]int{nil, {5173}} {
		m, clk := acModel(config.Config{})
		m, _ = acRefresh(t, m, []int{15173}, listen, nil)
		clk.advance(time.Hour)
		m, _ = acRefresh(t, m, []int{15173}, listen, nil)
		if m.cleaning != 0 || len(m.danglingSince) != 0 {
			t.Fatalf("listen=%v: remap must never be tracked or cleared (cleaning=%d)", listen, m.cleaning)
		}
	}
	// The same port as a plain 1:1 serve IS eligible.
	acRemap = map[int]bool{}
	m, clk := acModel(config.Config{})
	m, _ = acRefresh(t, m, []int{15173}, nil, nil)
	clk.advance(5 * time.Second)
	m, _ = acRefresh(t, m, []int{15173}, nil, nil)
	if m.cleaning != 1 {
		t.Fatal("a plain dangling 1:1 serve must be cleared")
	}
}

// Finding 1: unknown public state is not "no public routes".
func TestAutoPauseWaitsForRequiredPublicState(t *testing.T) {
	cfg := config.Config{}
	cfg.Caddy.Domain = "example.com"
	m, clk := acModel(cfg)
	m.cfAvailable = true
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	clk.advance(time.Hour)
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	if m.cleaning != 0 {
		t.Fatal("nothing may clear before the edge and tunnel polls have succeeded")
	}
	m.publishOK = true
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	if m.cleaning != 0 {
		t.Fatal("still waiting on the tunnel poll")
	}
	m.tunnelsOK = true
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	if m.cleaning != 1 {
		t.Fatal("with every required source known it clears (grace long elapsed)")
	}
	// Pauses again while a source errors.
	m.cleaning = 0
	m.publishOK = false
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	if m.cleaning != 0 {
		t.Fatal("must pause while the edge poll is failing")
	}
	// Not required: no caddy.domain, no cloudflared.
	m2, clk2 := acModel(config.Config{})
	m2, _ = acRefresh(t, m2, []int{8080}, nil, nil)
	clk2.advance(time.Hour)
	m2, _ = acRefresh(t, m2, []int{8080}, nil, nil)
	if m2.cleaning != 1 {
		t.Fatal("unconfigured sources must not block auto-clear")
	}
}

func TestPollMessagesMaintainPublicStateFlags(t *testing.T) {
	m, _ := acModel(config.Config{})
	res, _ := m.Update(publishPollMsg{published: map[int]publishInfo{}, gen: 1})
	m = res.(model)
	if !m.publishOK {
		t.Fatal("successful edge poll should mark publishOK")
	}
	res, _ = m.Update(publishPollMsg{err: errors.New("down"), gen: 2})
	m = res.(model)
	if m.publishOK {
		t.Fatal("failed edge poll should clear publishOK")
	}
	res, _ = m.Update(tunnelPollMsg{tunnels: map[int]tunnelInfo{}, gen: 1})
	m = res.(model)
	if !m.tunnelsOK {
		t.Fatal("successful tunnel poll should mark tunnelsOK")
	}
	res, _ = m.Update(tunnelPollMsg{err: errors.New("down"), gen: 2})
	if res.(model).tunnelsOK {
		t.Fatal("failed tunnel poll should clear tunnelsOK")
	}
}

// Finding 2: no auto-clear while any modal flow or in-flight op owns the state.
func TestAutoPauseBlockedByOpenFlowsAndInFlightOps(t *testing.T) {
	for name, set := range map[string]func(*model){
		"publish confirm": func(m *model) { m.mode = entryConfirmPublish },
		"publish host":    func(m *model) { m.mode = entryPublishHost },
		"funnel confirm":  func(m *model) { m.mode = entryConfirmFunnel },
		"tunnel confirm":  func(m *model) { m.mode = entryConfirmTunnelQuick },
		"C confirm":       func(m *model) { m.mode = entryConfirmClean },
		":22 confirm":     func(m *model) { m.mode = entryConfirm22 },
		"pending op":      func(m *model) { m.pending = 1234 },
		"tunnel stopping": func(m *model) { m.tunnelStopping = map[int]bool{1: true} },
		"tunnel starting": func(m *model) { m.tunnelSpinnerPort = 1 },
	} {
		m, clk := acModel(config.Config{})
		m, _ = acRefresh(t, m, []int{8080}, nil, nil)
		set(&m)
		clk.advance(time.Hour)
		m, _ = acRefresh(t, m, []int{8080}, nil, nil)
		if m.cleaning != 0 {
			t.Errorf("%s: auto-clear must wait", name)
		}
	}
}

// Finding 2: confirmPublish re-checks the CURRENT serve state, not the flag
// saved when the dialog opened, and refuses while a teardown is in flight.
func TestConfirmPublishRechecksServeState(t *testing.T) {
	cfg := config.Config{}
	cfg.Caddy.Domain = "example.com"
	m, _ := acModel(cfg)
	m.fqdn = "box.tail.ts.net"
	m.active = map[int]bool{8080: true}
	m.publishPort, m.publishHostname = 8080, "app.example.com"
	m.publishEnableServe = false // dialog opened while served
	delete(m.active, 8080)       // ...then the serve went away before "y"
	cmd := m.confirmPublish()
	if cmd == nil || m.pending != 8080 {
		t.Fatalf("publish should proceed (pending=%d)", m.pending)
	}
	// With a teardown in flight it refuses instead of racing it.
	m2, _ := acModel(cfg)
	m2.fqdn = "box.tail.ts.net"
	m2.active = map[int]bool{8080: true}
	m2.publishPort, m2.publishHostname = 8080, "app.example.com"
	m2.cleaning = 1
	m2.confirmPublish()
	if m2.pending != 0 || !strings.Contains(m2.flash, "pausing a stale forward") {
		t.Fatalf("must refuse during a teardown: pending=%d flash=%q", m2.pending, m2.flash)
	}
}

// Finding 4: forgetting a favorite keeps a port's auto_pause override.
func TestForgetKeepsAutoPauseOverride(t *testing.T) {
	cfg := rooted(t, config.Config{Ports: map[int]config.PortMeta{8080: {Favorite: true, AutoPause: "off"}}})
	m, clk := acModel(cfg)
	m.list.SetSize(80, 20)
	m, _ = acRefresh(t, m, []int{8080}, []int{8080}, nil)
	m.list.Select(0)
	res, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'F'}})
	m = res.(model)
	meta, ok := m.cfg.Ports[8080]
	if !ok || meta.Favorite || meta.AutoPause != "off" {
		t.Fatalf("override must survive forget: %+v ok=%v", meta, ok)
	}
	// And auto-clear stays off for the port.
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	clk.advance(time.Hour)
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	if m.cleaning != 0 {
		t.Fatal("auto-clear must stay off after forget")
	}
}

// Finding 5: a listener-scan failure resets the grace.
func TestAutoPauseScanErrorResetsTimers(t *testing.T) {
	m, clk := acModel(config.Config{})
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	clk.advance(4 * time.Second)
	m.refreshGen++
	res, _ := m.Update(refreshMsg{err: errors.New("ss failed"), auto: true, gen: m.refreshGen})
	m = res.(model)
	if len(m.danglingSince) != 0 {
		t.Fatal("scan error must drop the timers")
	}
	clk.advance(2 * time.Second) // past the ORIGINAL deadline
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	if m.cleaning != 0 {
		t.Fatal("must not clear at the original deadline after a scan error")
	}
	clk.advance(5 * time.Second)
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	if m.cleaning != 1 {
		t.Fatal("a fresh 5s of continuous dangling pauses")
	}
}

// Finding 7: a partial batch yields ONE error-severity notification naming both.
func TestPartialBatchSingleCombinedNotification(t *testing.T) {
	m, _ := acModel(config.Config{})
	m.pausing[8080] = pausedserve.Entry{Port: 8080, Exe: "/usr/bin/python3", Cwd: "/srv/a"}
	res, _ := m.Update(cleanupDoneMsg{auto: true, cleaned: []int{8080}, failed: []int{9000}, err: errors.New("could not clean :9000")})
	m = res.(model)
	want := "paused :8080 (app stopped) — resumes when python3 in /srv/a returns · t to forget; could not clean :9000"
	if m.flash != want || m.flashLevel != flashError {
		t.Fatalf("flash=%q level=%v", m.flash, m.flashLevel)
	}
	res, _ = m.Update(cleanupDoneMsg{cleaned: []int{1}, failed: []int{2}, err: errors.New("could not clean :2")})
	m = res.(model)
	if m.flash != "cleared :1; could not clean :2" || m.flashLevel != flashError {
		t.Fatalf("manual partial: flash=%q level=%v", m.flash, m.flashLevel)
	}
}
