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
)

// Kata 9p43: `tailport clean`. Every source and the unserve are faked.

type cleanFakes struct {
	listening []int
	served    []int
	funnel    map[int]int
	public    map[int]bool
	statusErr error
	failOn    map[int]bool
	unserved  []int
}

func (f *cleanFakes) install(t *testing.T) {
	t.Helper()
	oL, oS, oP, oU := cleanListPorts, cleanServeStatus, cleanPublicPorts, cleanUnserve
	t.Cleanup(func() { cleanListPorts, cleanServeStatus, cleanPublicPorts, cleanUnserve = oL, oS, oP, oU })
	cleanListPorts = func() ([]portscan.Port, error) {
		var ps []portscan.Port
		for _, p := range f.listening {
			ps = append(ps, portscan.Port{Number: p})
		}
		return ps, nil
	}
	cleanServeStatus = func() ([]int, map[int]int, error) {
		if f.statusErr != nil {
			return nil, nil, f.statusErr
		}
		return f.served, f.funnel, nil
	}
	cleanPublicPorts = func(config.Config) map[int]bool { return f.public }
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
	for _, want := range []string{"`tailport clean [--dry-run]`", "`would clear :P`", "ignores\n  `auto_clear_stale`", "Exits\n  non-zero"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("README should contain %q", want)
		}
	}
}
