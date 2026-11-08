package main

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/gruen/tailport/internal/config"
	"github.com/gruen/tailport/internal/portscan"
	"github.com/gruen/tailport/internal/tsserve"
)

// Kata 9p43: `tailport clean`. Every source and the unserve are faked.

type cleanFakes struct {
	listening []int
	served    []int
	funnel    map[int]int
	public    map[int]bool
	publicErr error
	statusErr error
	failOn    map[int]bool
	unserved  []int
	remap     map[int]bool // served ports that are NOT plain 1:1 serves
	scans     int
	// listenLate: ports whose app starts after the first scan.
	listenLate map[int]bool
	// scanErrOn: the Nth scan (1-based) fails; 0 = never.
	scanErrOn int
	order     []string
	statuses  int
	// listenAfterStatus: ports whose app starts WHILE the Nth serve-status
	// request (startOnStatus, 1-based) is in flight -- visible to every scan
	// after that request began.
	listenAfterStatus map[int]bool
	startOnStatus     int
}

func (f *cleanFakes) install(t *testing.T) {
	t.Helper()
	oL, oS, oP, oU := cleanListPorts, cleanServeStatus, cleanPublicPorts, cleanUnserve
	t.Cleanup(func() { cleanListPorts, cleanServeStatus, cleanPublicPorts, cleanUnserve = oL, oS, oP, oU })
	cleanListPorts = func() ([]portscan.Port, error) {
		f.scans++
		f.order = append(f.order, "scan")
		if f.scanErrOn == f.scans {
			return nil, errors.New("ss exploded")
		}
		var ps []portscan.Port
		for _, p := range f.listening {
			ps = append(ps, portscan.Port{Number: p})
		}
		if f.scans > 1 {
			for p := range f.listenLate {
				ps = append(ps, portscan.Port{Number: p})
			}
		}
		return ps, nil
	}
	cleanServeStatus = func() (tsserve.Detail, error) {
		f.statuses++
		f.order = append(f.order, "status")
		if f.startOnStatus != 0 && f.statuses == f.startOnStatus {
			for p := range f.listenAfterStatus {
				f.listening = append(f.listening, p)
			}
		}
		if f.statusErr != nil {
			return tsserve.Detail{}, f.statusErr
		}
		one := map[int]bool{}
		for _, p := range f.served {
			if !f.remap[p] {
				one[p] = true
			}
		}
		return tsserve.Detail{Active: f.served, Funnel: f.funnel, OneToOne: one}, nil
	}
	cleanPublicPorts = func(config.Config) (map[int]bool, error) {
		f.order = append(f.order, "public")
		return f.public, f.publicErr
	}
	cleanUnserve = func(p int) error {
		if f.failOn[p] {
			return errors.New("boom")
		}
		f.unserved = append(f.unserved, p)
		return nil
	}
}

func runCleanWith(t *testing.T, f *cleanFakes, args ...string) (code int, out, errOut string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // default config: :22 locked
	f.install(t)
	var so, se bytes.Buffer
	code = runClean(args, &so, &se)
	return code, so.String(), se.String()
}

func TestCleanDryRunListsWithoutUnserving(t *testing.T) {
	f := &cleanFakes{served: []int{9000, 8080}}
	code, out, _ := runCleanWith(t, f, "--dry-run")
	if code != 0 || out != "would clear :8080\nwould clear :9000\n" || len(f.unserved) != 0 {
		t.Fatalf("code=%d out=%q unserved=%v", code, out, f.unserved)
	}
}

func TestCleanUnservesViaInjectedFunction(t *testing.T) {
	f := &cleanFakes{served: []int{9000, 8080}}
	code, out, _ := runCleanWith(t, f)
	if code != 0 || out != "cleared :8080\ncleared :9000\n" || !reflect.DeepEqual(f.unserved, []int{8080, 9000}) {
		t.Fatalf("code=%d out=%q unserved=%v", code, out, f.unserved)
	}
}

func TestCleanExclusionsAndNothingToClean(t *testing.T) {
	f := &cleanFakes{
		served:    []int{22, 3000, 4000, 5000, 6000, 7000},
		listening: []int{7000}, // listening (e.g. No127): not dangling
		funnel:    map[int]int{4000: 443},
		public:    map[int]bool{5000: true},
	}
	// 22 is locked by the default config; lock 3000 through a config file.
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	cfgPath := dir + "/c.yaml"
	if err := (config.Config{Ports: map[int]config.PortMeta{3000: {Locked: true}, 6000: {AutoClearStale: "off"}}}).WithPath(cfgPath).Save(); err != nil {
		t.Fatal(err)
	}
	f.install(t)
	var so, se bytes.Buffer
	code := runClean([]string{"-c", cfgPath}, &so, &se)
	// Only :6000 qualifies; its per-port auto_clear_stale: off governs the TUI
	// auto-clear only, an explicit clean still clears it.
	if code != 0 || so.String() != "cleared :6000\n" || !reflect.DeepEqual(f.unserved, []int{6000}) {
		t.Fatalf("code=%d out=%q unserved=%v", code, so.String(), f.unserved)
	}

	f2 := &cleanFakes{served: []int{22}, listening: nil}
	code, out, _ := runCleanWith(t, f2)
	if code != 0 || out != "nothing to clean\n" {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestCleanExitCodes(t *testing.T) {
	f := &cleanFakes{statusErr: errors.New("tailscale: not found")}
	code, out, errOut := runCleanWith(t, f)
	if code != 1 || out != "" || !strings.Contains(errOut, "reading tailscale serve status") || len(f.unserved) != 0 {
		t.Fatalf("status failure: code=%d out=%q err=%q", code, out, errOut)
	}

	f = &cleanFakes{served: []int{8080, 9000}, failOn: map[int]bool{8080: true}}
	code, out, errOut = runCleanWith(t, f)
	if code != 1 || out != "cleared :9000\n" || !strings.Contains(errOut, "could not clear :8080") {
		t.Fatalf("unserve failure: code=%d out=%q err=%q", code, out, errOut)
	}

	if code, _, _ := runCleanWith(t, &cleanFakes{}, "--bogus"); code != 2 {
		t.Fatalf("bad flag exit = %d, want 2", code)
	}
}

func TestCleanIsListedInUsageAndDispatch(t *testing.T) {
	var b bytes.Buffer
	printUsage(&b, "")
	if !strings.Contains(b.String(), "clean        turn off dangling serve forwards (--dry-run to preview)") {
		t.Errorf("--help should list clean:\n%s", b.String())
	}
	f := &cleanFakes{served: []int{8080}}
	f.install(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var so, se bytes.Buffer
	if code := run([]string{"clean", "--dry-run"}, &so, &se); code != 0 || so.String() != "would clear :8080\n" {
		t.Fatalf("dispatch: code=%d out=%q err=%q", code, so.String(), se.String())
	}
}

// The README's `tailport clean` claims, each pinned by a test above: one line
// per port, --dry-run, ignores auto_clear_stale (TestCleanExclusionsAndNothingToClean),
// non-zero on failure (TestCleanExitCodes).
func TestCleanReadmeClaims(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"`tailport clean [--dry-run]`", "`would clear :P`", "ignores\n  `auto_clear_stale`", "`tailport clean 8080` exits 2", "(then nothing is cleared)", "skipped :P (no longer dangling)"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("README should contain %q", want)
		}
	}
}

// Finding 8: a remapped serve (tailnet 15173 -> local 5173) is never touched,
// whether or not either port is listening.
func TestCleanNeverTouchesRemappedServe(t *testing.T) {
	f := &cleanFakes{served: []int{15173}, remap: map[int]bool{15173: true}}
	if code, out, _ := runCleanWith(t, f); code != 0 || out != "nothing to clean\n" || len(f.unserved) != 0 {
		t.Fatalf("remap, nothing listening: code=%d out=%q unserved=%v", code, out, f.unserved)
	}
	f = &cleanFakes{served: []int{15173}, remap: map[int]bool{15173: true}, listening: []int{5173}}
	if code, out, _ := runCleanWith(t, f); code != 0 || out != "nothing to clean\n" || len(f.unserved) != 0 {
		t.Fatalf("remap, target up: code=%d out=%q unserved=%v", code, out, f.unserved)
	}
}

// Finding 1: unreadable public state clears nothing and exits non-zero.
func TestCleanUnknownPublicStateClearsNothing(t *testing.T) {
	f := &cleanFakes{served: []int{8080}, publicErr: errors.New("cannot read publish state from the Caddy edge: refused")}
	code, out, errOut := runCleanWith(t, f)
	if code != 1 || out != "" || len(f.unserved) != 0 || !strings.Contains(errOut, "Caddy edge") || !strings.Contains(errOut, "nothing was cleared") {
		t.Fatalf("code=%d out=%q err=%q unserved=%v", code, out, errOut, f.unserved)
	}
	f = &cleanFakes{served: []int{8080}, publicErr: errors.New("x")}
	if code, _, _ := runCleanWith(t, f, "--dry-run"); code != 1 {
		t.Fatalf("dry-run must fail too, code=%d", code)
	}
}

// Finding 3: public state is read before the listener scan, and each port is
// re-checked against a fresh scan right before it is unserved.
func TestCleanRechecksEachPortBeforeUnserving(t *testing.T) {
	f := &cleanFakes{served: []int{8080, 9000}, listenLate: map[int]bool{8080: true}}
	code, out, _ := runCleanWith(t, f)
	if code != 0 || out != "skipped :8080 (no longer dangling)\ncleared :9000\n" || !reflect.DeepEqual(f.unserved, []int{9000}) {
		t.Fatalf("code=%d out=%q unserved=%v", code, out, f.unserved)
	}
	if len(f.order) < 3 || f.order[0] != "public" || f.order[1] != "status" || f.order[2] != "scan" {
		t.Fatalf("public state, then serve status, then the listener scan: %v", f.order)
	}
}

// roborev 6192: the per-port recheck must scan listeners AFTER reading serve
// status, so an app that starts while that (slower) status request is in
// flight is still seen and its forward kept. With the old scan-then-status
// order the recheck's scan ran first, missed the app, and the forward was cut.
func TestCleanRecheckScansAfterServeStatus(t *testing.T) {
	f := &cleanFakes{served: []int{8080}, listenAfterStatus: map[int]bool{8080: true}, startOnStatus: 2}
	code, out, _ := runCleanWith(t, f)
	if code != 0 || out != "skipped :8080 (no longer dangling)\n" || len(f.unserved) != 0 {
		t.Fatalf("an app starting during the recheck's status request must be kept: code=%d out=%q unserved=%v", code, out, f.unserved)
	}
}

func TestCleanAbortsOnRescanError(t *testing.T) {
	f := &cleanFakes{served: []int{8080, 9000}, scanErrOn: 2}
	code, out, errOut := runCleanWith(t, f)
	if code != 1 || out != "" || len(f.unserved) != 0 || !strings.Contains(errOut, "aborting") {
		t.Fatalf("code=%d out=%q err=%q unserved=%v", code, out, errOut, f.unserved)
	}
	f = &cleanFakes{served: []int{8080}, scanErrOn: 1}
	if code, _, _ := runCleanWith(t, f); code != 1 || len(f.unserved) != 0 {
		t.Fatalf("first-scan failure: code=%d unserved=%v", code, f.unserved)
	}
}

// Finding 6: a stray positional is rejected before anything is gathered or
// mutated.
func TestCleanRejectsPositionalArgs(t *testing.T) {
	for _, args := range [][]string{{"8080", "--dry-run"}, {"--dry-run", "8080"}, {"all"}} {
		f := &cleanFakes{served: []int{8080}}
		code, out, errOut := runCleanWith(t, f, args...)
		if code != 2 || out != "" || !strings.Contains(errOut, "unexpected argument") || len(f.unserved) != 0 || f.scans != 0 || len(f.order) != 0 {
			t.Errorf("%v: code=%d out=%q err=%q unserved=%v scans=%d", args, code, out, errOut, f.unserved, f.scans)
		}
	}
}
