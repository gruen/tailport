package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Kata p2r5 (was 9p43 auto_clear_stale): serve.auto_pause and ports.<n>.auto_pause.

func TestParseAutoPause(t *testing.T) {
	for _, tc := range []struct {
		in      string
		d       time.Duration
		set, ok bool
	}{
		{"", 0, false, true},
		{"  ", 0, false, true},
		{"5s", 5 * time.Second, true, true},
		{"2m", 2 * time.Minute, true, true},
		{"0", 0, true, true},
		{"0s", 0, true, true},
		{"off", 0, true, true},
		{"OFF", 0, true, true},
		{"soon", 0, true, false},
		{"-5s", 0, true, false},
		{"30", 0, true, false}, // a bare non-zero number is not a duration
	} {
		d, set, ok := ParseAutoPause(tc.in)
		if d != tc.d || set != tc.set || ok != tc.ok {
			t.Errorf("ParseAutoPause(%q) = %v,%v,%v want %v,%v,%v", tc.in, d, set, ok, tc.d, tc.set, tc.ok)
		}
	}
}

func TestPauseGraceResolution(t *testing.T) {
	cfg := Config{
		Serve: ServeConfig{AutoPause: "1m"},
		Ports: map[int]PortMeta{1: {AutoPause: "5s"}, 2: {AutoPause: "off"}, 3: {AutoPause: "bogus"}, 4: {}},
	}
	for port, want := range map[int]time.Duration{1: 5 * time.Second, 2: 0, 3: time.Minute, 4: time.Minute, 99: time.Minute} {
		if got := cfg.PauseGrace(port); got != want {
			t.Errorf("grace(:%d) = %v, want %v", port, got, want)
		}
	}
	if got := (Config{}).PauseGrace(1); got != 5*time.Second {
		t.Errorf("unset global = %v, want the 5s default", got)
	}
	if got := (Config{Serve: ServeConfig{AutoPause: "off"}}).PauseGrace(1); got != 0 {
		t.Errorf("global off = %v, want 0", got)
	}
	if got := (Config{Serve: ServeConfig{AutoPause: "junk"}}).PauseGrace(1); got != 5*time.Second {
		t.Errorf("invalid global = %v, want the default", got)
	}
}

func TestPauseWarnings(t *testing.T) {
	cfg := Config{
		Serve: ServeConfig{AutoPause: "soon"},
		Ports: map[int]PortMeta{9: {AutoPause: "x"}, 3: {AutoPause: "y"}, 5: {AutoPause: "off"}},
	}
	w := cfg.PauseWarnings()
	if len(w) != 3 || !strings.HasPrefix(w[0], `serve.auto_pause "soon"`) ||
		!strings.HasPrefix(w[1], `ports.3.auto_pause "y"`) || !strings.HasPrefix(w[2], `ports.9.auto_pause "x"`) {
		t.Fatalf("warnings = %q", w)
	}
	if got := (Config{}).PauseWarnings(); len(got) != 0 {
		t.Fatalf("clean config warned: %q", got)
	}
}

// YAML forms users will type: unquoted off / 0 / 2m must all load as strings,
// a missing serve block gets the default, and Save -> Load is lossless.
func TestServeConfigLoadAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := "serve:\n  auto_pause: 0\nports:\n  1:\n    auto_pause: off\n  2:\n    auto_pause: 2m\n  3: {}\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Serve.AutoPause != "0" || cfg.Ports[1].AutoPause != "off" || cfg.Ports[2].AutoPause != "2m" {
		t.Fatalf("loaded %+v / %+v", cfg.Serve, cfg.Ports)
	}
	if cfg.PauseGrace(3) != 0 || cfg.PauseGrace(2) != 2*time.Minute || cfg.PauseGrace(1) != 0 {
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
	if cfg.Serve.AutoPause != "5s" {
		t.Fatalf("missing block should default to 5s, got %q", cfg.Serve.AutoPause)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	out := string(b)
	for _, want := range []string{"serve:", "auto_pause: 5s", "# Pause a dangling forward", "ports.<n>.auto_pause", "EADDRINUSE"} {
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
