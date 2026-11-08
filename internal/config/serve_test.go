package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Kata 9p43: serve.auto_clear_stale and ports.<n>.auto_clear_stale.

func TestParseAutoClear(t *testing.T) {
	for _, tc := range []struct {
		in      string
		d       time.Duration
		set, ok bool
	}{
		{"", 0, false, true},
		{"  ", 0, false, true},
		{"30s", 30 * time.Second, true, true},
		{"2m", 2 * time.Minute, true, true},
		{"0", 0, true, true},
		{"0s", 0, true, true},
		{"off", 0, true, true},
		{"OFF", 0, true, true},
		{"soon", 0, true, false},
		{"-5s", 0, true, false},
		{"30", 0, true, false}, // a bare non-zero number is not a duration
	} {
		d, set, ok := ParseAutoClear(tc.in)
		if d != tc.d || set != tc.set || ok != tc.ok {
			t.Errorf("ParseAutoClear(%q) = %v,%v,%v want %v,%v,%v", tc.in, d, set, ok, tc.d, tc.set, tc.ok)
		}
	}
}

func TestAutoClearGraceResolution(t *testing.T) {
	cfg := Config{
		Serve: ServeConfig{AutoClearStale: "1m"},
		Ports: map[int]PortMeta{1: {AutoClearStale: "5s"}, 2: {AutoClearStale: "off"}, 3: {AutoClearStale: "bogus"}, 4: {}},
	}
	for port, want := range map[int]time.Duration{1: 5 * time.Second, 2: 0, 3: time.Minute, 4: time.Minute, 99: time.Minute} {
		if got := cfg.AutoClearGrace(port); got != want {
			t.Errorf("grace(:%d) = %v, want %v", port, got, want)
		}
	}
	if got := (Config{}).AutoClearGrace(1); got != 30*time.Second {
		t.Errorf("unset global = %v, want the 30s default", got)
	}
	if got := (Config{Serve: ServeConfig{AutoClearStale: "off"}}).AutoClearGrace(1); got != 0 {
		t.Errorf("global off = %v, want 0", got)
	}
	if got := (Config{Serve: ServeConfig{AutoClearStale: "junk"}}).AutoClearGrace(1); got != 30*time.Second {
		t.Errorf("invalid global = %v, want the default", got)
	}
}

func TestStaleWarnings(t *testing.T) {
	cfg := Config{
		Serve: ServeConfig{AutoClearStale: "soon"},
		Ports: map[int]PortMeta{9: {AutoClearStale: "x"}, 3: {AutoClearStale: "y"}, 5: {AutoClearStale: "off"}},
	}
	w := cfg.StaleWarnings()
	if len(w) != 3 || !strings.HasPrefix(w[0], `serve.auto_clear_stale "soon"`) ||
		!strings.HasPrefix(w[1], `ports.3.auto_clear_stale "y"`) || !strings.HasPrefix(w[2], `ports.9.auto_clear_stale "x"`) {
		t.Fatalf("warnings = %q", w)
	}
	if got := (Config{}).StaleWarnings(); len(got) != 0 {
		t.Fatalf("clean config warned: %q", got)
	}
}

// YAML forms users will type: unquoted off / 0 / 2m must all load as strings,
// a missing serve block gets the default, and Save -> Load is lossless.
func TestServeConfigLoadAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := "serve:\n  auto_clear_stale: 0\nports:\n  1:\n    auto_clear_stale: off\n  2:\n    auto_clear_stale: 2m\n  3: {}\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Serve.AutoClearStale != "0" || cfg.Ports[1].AutoClearStale != "off" || cfg.Ports[2].AutoClearStale != "2m" {
		t.Fatalf("loaded %+v / %+v", cfg.Serve, cfg.Ports)
	}
	if cfg.AutoClearGrace(3) != 0 || cfg.AutoClearGrace(2) != 2*time.Minute || cfg.AutoClearGrace(1) != 0 {
		t.Fatal("grace resolution off the loaded file is wrong")
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Serve, again.Serve) || !reflect.DeepEqual(cfg.Ports, again.Ports) {
		t.Fatalf("round trip lost data: %+v %+v vs %+v %+v", cfg.Serve, cfg.Ports, again.Serve, again.Ports)
	}
}

func TestServeBlockDefaultAndComment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("ports: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path) // a pre-9p43 file: no serve: block
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Serve.AutoClearStale != "30s" {
		t.Fatalf("missing block should default to 30s, got %q", cfg.Serve.AutoClearStale)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	out := string(b)
	for _, want := range []string{"serve:", "auto_clear_stale: 30s", "# Turn off a dangling forward", "ports.<n>.auto_clear_stale", "EADDRINUSE"} {
		if !strings.Contains(out, want) {
			t.Errorf("saved config missing %q:\n%s", want, out)
		}
	}
	// Comments survive a second save.
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(path)
	if string(b2) != out {
		t.Errorf("second save changed the file:\n%s\n---\n%s", out, b2)
	}
}
