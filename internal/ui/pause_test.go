package ui

import (
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
	"github.com/gruen/tailport/internal/tsserve"
)

// Kata p2r5: pause / resume. Helpers (acModel, acRefresh, acIdent, ...) live in
// autopause_test.go. No test here executes a returned cmd that could reach
// tailscaled: toggles are checked via m.pending and fed back as done messages.

var otherIdent = procid.Identity{Exe: "/usr/bin/python3", Cwd: "/srv/b"}

// pmModel is acModel with a private state dir and a paused entry for :8080.
func pmModel(t *testing.T, cfg config.Config) (model, *acClock) {
	t.Helper()
	acReset(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	m, clk := acModel(cfg)
	p, err := pausedserve.Path()
	if err != nil {
		t.Fatal(err)
	}
	m.pausedPath = p
	m.host = "box"
	return m, clk
}

func pmPause(m *model, clk *acClock, port int) {
	m.paused[port] = pausedserve.Entry{Port: port, Exe: acIdent.Exe, Cwd: acIdent.Cwd, PausedAt: clk.now()}
}

// ── identity capture and the pause itself ───────────────────────────────────

func TestIdentityIsCapturedFromListenerAndNeverPausedWithout(t *testing.T) {
	m, clk := pmModel(t, config.Config{})
	m.lastIdent = map[int]procid.Identity{} // un-seed: capture it for real
	m, _ = acRefresh(t, m, []int{8080}, []int{8080}, nil)
	if got := m.lastIdent[8080]; got != acIdent {
		t.Fatalf("identity not captured from the refresh: %+v", got)
	}
	// The app dies: the captured identity is what gets paused.
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	clk.advance(5 * time.Second)
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	if m.cleaning != 1 || m.pausing[8080].Exe != acIdent.Exe || m.pausing[8080].Cwd != acIdent.Cwd {
		t.Fatalf("expected a pause carrying the captured identity: cleaning=%d pausing=%+v", m.cleaning, m.pausing)
	}

	// A served port whose listener was never identified is never paused.
	m2, clk2 := pmModel(t, config.Config{})
	m2, _ = acRefresh(t, m2, []int{8888}, nil, nil)
	clk2.advance(time.Hour)
	m2, _ = acRefresh(t, m2, []int{8888}, nil, nil)
	if m2.cleaning != 0 || len(m2.danglingSince) != 0 {
		t.Fatalf("no identity must mean never paused: cleaning=%d", m2.cleaning)
	}

	// An unreadable listener (another user's process) erases the identity.
	m3, _ := pmModel(t, config.Config{})
	acListener[8080] = procid.Identity{}
	m3, _ = acRefresh(t, m3, []int{8080}, []int{8080}, nil)
	if m3.lastIdent[8080].Known() {
		t.Fatal("an unreadable listener must clear the remembered identity")
	}
}

func TestPauseAfterGraceRecordsAndPersists(t *testing.T) {
	m, clk := pmModel(t, config.Config{})
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	clk.advance(5 * time.Second)
	m, _ = acRefresh(t, m, []int{8080}, nil, nil)
	if m.cleaning != 1 {
		t.Fatal("expected a pause after the 5s grace")
	}
	res, _ := m.Update(cleanupDoneMsg{auto: true, cleaned: []int{8080}})
	m = res.(model)
	e, ok := m.paused[8080]
	if !ok || e.Exe != acIdent.Exe || e.Cwd != acIdent.Cwd || !e.PausedAt.Equal(clk.now()) {
		t.Fatalf("paused entry = %+v ok=%v", e, ok)
	}
	if want := "paused :8080 (app stopped) — resumes when python3 in /srv/a returns · t to forget"; m.flash != want {
		t.Fatalf("toast = %q", m.flash)
	}
	onDisk, err := pausedserve.Load(m.pausedPath, clk.now())
	if err != nil || onDisk[8080].Cwd != "/srv/a" {
		t.Fatalf("not persisted: %+v %v", onDisk, err)
	}
	if _, still := m.lastIdent[8080]; still {
		t.Error("a paused port no longer has a live identity")
	}
}

func TestPausedPortStaysVisibleWithPausedRoute(t *testing.T) {
	m, clk := pmModel(t, config.Config{})
	pmPause(&m, clk, 8080)
	m, _ = acRefresh(t, m, nil, nil, nil) // nothing served, nothing listening
	m.width, m.height = 120, 40
	m.reconcileViewport()
	var found bool
	for _, it := range m.list.VisibleItems() {
		pi := it.(portItem)
		if pi.port.Number != 8080 {
			continue
		}
		found = true
		rs := pi.routes()
		if len(rs) != 1 || rs[0].kind != routePaused || rs[0].label() != "tailnet" || rs[0].url != "" {
			t.Fatalf("routes = %+v", rs)
		}
	}
	if !found {
		t.Fatal("a paused port must stay in the list with nothing listening (Favorites view)")
	}
	view := stripANSI(m.View())
	if !strings.Contains(view, "paused — resumes when python3 in /srv/a returns") {
		t.Errorf("row should say it is paused and name the program:\n%s", view)
	}
	// Also in the All-ports view.
	m.showAllPorts = true
	m.rebuildItems()
	if !strings.Contains(stripANSI(m.View()), "paused — resumes when") {
		t.Error("paused row missing from All ports view")
	}
}

// ── resume ──────────────────────────────────────────────────────────────────

func TestResumeOnMatchAndLoopback(t *testing.T) {
	m, clk := pmModel(t, config.Config{})
	pmPause(&m, clk, 8080)
	m, _ = acRefresh(t, m, nil, []int{8080}, nil) // same program, loopback bind
	if m.pending != 8080 || !m.resuming[8080] {
		t.Fatalf("expected a re-serve in flight: pending=%d resuming=%v", m.pending, m.resuming)
	}
	// The serve landed.
	res, _ := m.Update(toggleDoneMsg{port: 8080, kind: routeTailnet, turnOn: true})
	m = res.(model)
	if _, ok := m.paused[8080]; ok {
		t.Fatal("the paused entry must be dropped after the resume")
	}
	if m.flash != "resumed :8080 (same app is back)" {
		t.Fatalf("toast = %q", m.flash)
	}
	onDisk, _ := pausedserve.Load(m.pausedPath, clk.now())
	if len(onDisk) != 0 {
		t.Fatalf("file should be empty after resume: %+v", onDisk)
	}
}

func TestNoServeOnWildcardReturnEntryDropped(t *testing.T) {
	m, clk := pmModel(t, config.Config{})
	pmPause(&m, clk, 8080)
	acBind[8080] = acBindSpec{scope: portscan.ScopeWildcard}
	m, _ = acRefresh(t, m, nil, []int{8080}, nil)
	if m.pending != 0 || len(m.resuming) != 0 {
		t.Fatalf("a wildcard-bound return must NOT be served: pending=%d", m.pending)
	}
	if _, ok := m.paused[8080]; ok {
		t.Fatal("the entry must be dropped")
	}
	if !strings.Contains(m.flash, ":8080 is back (same app) and already reachable on the tailnet") {
		t.Fatalf("toast = %q", m.flash)
	}
	onDisk, _ := pausedserve.Load(m.pausedPath, clk.now())
	if len(onDisk) != 0 {
		t.Fatalf("drop must persist: %+v", onDisk)
	}
}

func TestNoResumeOnMismatchOrUnknownRowMarked(t *testing.T) {
	for name, id := range map[string]procid.Identity{
		"different cwd": otherIdent,
		"different exe": {Exe: "/usr/bin/node", Cwd: "/srv/a"},
		"unreadable":    {},
	} {
		m, clk := pmModel(t, config.Config{})
		pmPause(&m, clk, 8080)
		acListener[8080] = id
		m, _ = acRefresh(t, m, nil, []int{8080}, nil)
		if m.pending != 0 || len(m.resuming) != 0 {
			t.Errorf("%s: must never auto-share a different program", name)
		}
		if _, ok := m.paused[8080]; !ok {
			t.Errorf("%s: the entry must be kept", name)
		}
		var item portItem
		for _, it := range m.list.VisibleItems() {
			if pi := it.(portItem); pi.port.Number == 8080 {
				item = pi
			}
		}
		if !item.paused || !item.pausedOther {
			t.Errorf("%s: row should be marked 'a different program': %+v", name, item)
		}
		m.width, m.height = 140, 40
		m.reconcileViewport()
		if v := stripANSI(m.View()); !strings.Contains(v, "a different program is listening (was python3 in /srv/a)") {
			t.Errorf("%s: row text missing:\n%s", name, v)
		}
	}
}

// ── t on a paused row ───────────────────────────────────────────────────────

func selectPort(t *testing.T, m *model, port int) {
	t.Helper()
	for i, it := range m.list.VisibleItems() {
		if it.(portItem).port.Number == port {
			m.list.Select(i)
			m.syncSelKey()
			return
		}
	}
	t.Fatalf("port %d not visible", port)
}

func TestTOnPausedRowForgets(t *testing.T) {
	m, clk := pmModel(t, config.Config{})
	pmPause(&m, clk, 8080)
	m, _ = acRefresh(t, m, nil, nil, nil)
	selectPort(t, &m, 8080)
	res, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	m = res.(model)
	if _, ok := m.paused[8080]; ok || m.pending != 0 {
		t.Fatalf("t must forget, not serve: paused=%v pending=%d", m.paused, m.pending)
	}
	if !strings.Contains(m.flash, "forgot paused :8080") {
		t.Fatalf("toast = %q", m.flash)
	}
	if onDisk, _ := pausedserve.Load(m.pausedPath, clk.now()); len(onDisk) != 0 {
		t.Fatalf("forget must persist: %+v", onDisk)
	}
}

func TestTOnMismatchedRowServesTheNewProgram(t *testing.T) {
	m, clk := pmModel(t, config.Config{})
	m.cfg = rooted(t, m.cfg)
	pmPause(&m, clk, 8080)
	acListener[8080] = otherIdent
	m, _ = acRefresh(t, m, nil, []int{8080}, nil)
	selectPort(t, &m, 8080)
	res, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	m = res.(model)
	if m.pending != 8080 {
		t.Fatalf("t on a mismatched row must serve the listening program: pending=%d flash=%q", m.pending, m.flash)
	}
	if _, ok := m.paused[8080]; ok {
		t.Fatal("serving supersedes the paused memory")
	}
}

// ── persistence, expiry, corruption ─────────────────────────────────────────

func TestPausedSurvivesSimulatedRestart(t *testing.T) {
	m, clk := pmModel(t, config.Config{})
	pmPause(&m, clk, 8080)
	if err := m.savePaused(); err != nil {
		t.Fatal(err)
	}
	m2 := New(config.Config{}) // a relaunched tailport, same state dir
	if e, ok := m2.paused[8080]; !ok || e.Cwd != acIdent.Cwd {
		t.Fatalf("New() did not load the paused file: %+v", m2.paused)
	}
	m2.now = clk.now
	m2.cfAvailable = false
	m2, _ = acRefresh(t, m2, nil, []int{8080}, nil)
	if m2.pending != 8080 {
		t.Fatal("a relaunched tailport must resume the same app")
	}
}

func TestPausedExpiresAfterSevenDays(t *testing.T) {
	m, clk := pmModel(t, config.Config{})
	m.paused[8080] = pausedserve.Entry{Port: 8080, Exe: acIdent.Exe, Cwd: acIdent.Cwd, PausedAt: clk.now().Add(-7*24*time.Hour - time.Minute)}
	m.paused[9090] = pausedserve.Entry{Port: 9090, Exe: acIdent.Exe, Cwd: acIdent.Cwd, PausedAt: clk.now().Add(-6 * 24 * time.Hour)}
	m, _ = acRefresh(t, m, nil, []int{8080}, nil) // even with the app back, an expired entry is gone
	if _, ok := m.paused[8080]; ok || m.pending != 0 {
		t.Fatalf("expired entry must be dropped, not resumed: %+v pending=%d", m.paused, m.pending)
	}
	if _, ok := m.paused[9090]; !ok {
		t.Fatal("a 6-day-old entry must survive")
	}
	// And on load.
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	p, _ := pausedserve.Path()
	_ = pausedserve.Save(p, map[int]pausedserve.Entry{1: {Port: 1, Exe: "/a", Cwd: "/c", PausedAt: time.Now().Add(-8 * 24 * time.Hour)}})
	if got := New(config.Config{}); len(got.paused) != 0 {
		t.Fatalf("expired entries must not load: %+v", got.paused)
	}
}

func TestCorruptPausedFileWarnsOnceAndStartsEmpty(t *testing.T) {
	acReset(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p, _ := pausedserve.Path()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(config.Config{})
	m.cfAvailable = false
	if len(m.paused) != 0 || m.pausedLoadErr == nil {
		t.Fatalf("corrupt file: paused=%v err=%v", m.paused, m.pausedLoadErr)
	}
	m, _ = acRefresh(t, m, nil, nil, nil)
	if !strings.Contains(m.flash, "corrupt") || m.flashLevel != flashWarn {
		t.Fatalf("expected one corrupt-file warning, got %q (%v)", m.flash, m.flashLevel)
	}
	m.flash = ""
	m, _ = acRefresh(t, m, nil, nil, nil)
	if m.flash != "" {
		t.Fatal("the warning must be one-shot")
	}
}

func TestPausedFileNotWrittenWhenNothingChanged(t *testing.T) {
	m, clk := pmModel(t, config.Config{})
	for i := 0; i < 3; i++ {
		m, _ = acRefresh(t, m, []int{8080}, []int{8080}, nil)
		clk.advance(time.Second)
	}
	if _, err := os.Stat(m.pausedPath); !os.IsNotExist(err) {
		t.Fatalf("no change, so no file: err=%v", err)
	}
	pmPause(&m, clk, 9090)
	if err := m.savePaused(); err != nil {
		t.Fatal(err)
	}
	fi1, _ := os.Stat(m.pausedPath)
	clk.advance(time.Second)
	m, _ = acRefresh(t, m, nil, nil, nil) // steady state with a paused entry
	fi2, _ := os.Stat(m.pausedPath)
	if !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Fatal("a refresh that changed nothing must not rewrite the file")
	}
}

// ── exclusions, generation guards ───────────────────────────────────────────

func TestPauseKeepsEveryNineP43Exclusion(t *testing.T) {
	cfg := config.Config{Ports: map[int]config.PortMeta{22: {}, 3000: {Locked: true}}}
	m, clk := pmModel(t, cfg)
	m.lastIdent[22] = acIdent
	m.published = map[int]publishInfo{4000: {hostname: "x.example.com"}}
	m.tunnels = map[int]tunnelInfo{5000: {}}
	acRemap[15173] = true
	served := []int{22, 3000, 4000, 5000, 6000, 7000, 15173}
	m, _ = acRefresh(t, m, served, nil, map[int]int{6000: 443})
	clk.advance(time.Hour)
	m, _ = acRefresh(t, m, served, nil, map[int]int{6000: 443})
	if m.cleaning != 1 || len(m.pausing) != 1 || m.pausing[7000].Port != 7000 {
		t.Fatalf("only :7000 may be paused: cleaning=%d pausing=%+v", m.cleaning, m.pausing)
	}
}

func TestStaleGenerationRefreshNeverPausesOrResumes(t *testing.T) {
	m, clk := pmModel(t, config.Config{})
	pmPause(&m, clk, 8080)
	m.refreshFloor = 10
	clk.advance(time.Hour)
	// Would resume (same app, loopback) and would pause :9090 (due), but gen is stale.
	res, cmd := m.Update(refreshMsg{
		ports:  []portscan.Port{{Number: 8080, BindScope: portscan.ScopeLoopback}},
		active: map[int]bool{9090: true}, funnel: map[int]int{}, oneToOne: map[int]bool{9090: true},
		idents: map[int]procid.Identity{8080: acIdent}, gen: 3,
	})
	m = res.(model)
	if cmd != nil || m.pending != 0 || m.cleaning != 0 || len(m.resuming) != 0 || len(m.danglingSince) != 0 {
		t.Fatalf("a dropped stale refresh must do nothing: pending=%d cleaning=%d since=%v", m.pending, m.cleaning, m.danglingSince)
	}
	if _, ok := m.paused[8080]; !ok {
		t.Fatal("stale refresh must not drop the paused entry")
	}
}

func TestResumeWaitsWhileBlocked(t *testing.T) {
	m, clk := pmModel(t, config.Config{})
	pmPause(&m, clk, 8080)
	m.mode = entryConfirmFunnel
	m, _ = acRefresh(t, m, nil, []int{8080}, nil)
	if m.pending != 0 {
		t.Fatal("no auto re-serve while a dialog is open")
	}
	m.mode = entryNone
	m, _ = acRefresh(t, m, nil, []int{8080}, nil)
	if m.pending != 8080 {
		t.Fatal("resumes on the next refresh once unblocked")
	}
}

// The refresh cmd asks for identities of served 1:1 listeners and of paused
// ports that are listening -- and only those.
func TestRefreshReadsIdentitiesOnlyWhereNeeded(t *testing.T) {
	var asked []int
	oldID, oldScan, oldStatus := identify, scanListeners, serveStatus
	t.Cleanup(func() { identify, scanListeners, serveStatus = oldID, oldScan, oldStatus })
	identify = func(pid int) procid.Identity { asked = append(asked, pid); return acIdent }
	scanListeners = func() ([]portscan.Port, error) {
		return []portscan.Port{{Number: 1, Pid: 101}, {Number: 2, Pid: 102}, {Number: 3, Pid: 103}, {Number: 4}}, nil
	}
	serveStatus = func() (tsserve.Detail, error) {
		return tsserve.Detail{Active: []int{1, 2}, OneToOne: map[int]bool{1: true}}, nil
	}
	msg := refresh(3, 4).(refreshMsg)
	if len(asked) != 2 || msg.idents[1].Exe == "" || msg.idents[3].Exe == "" {
		t.Fatalf("asked=%v idents=%v (want :1 served 1:1 and :3 paused; not remap :2, not pid-less :4)", asked, msg.idents)
	}
}
