package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gruen/tailport/internal/config"
)

// TestMain isolates config writes for the ENTIRE internal/ui test binary, so no
// test can ever touch the developer's real ~/.config/tailport/config.yaml.
//
// This is the guard for kata 2kak. config.Save falls back to config.Path("")
// -- which resolves to the real ~/.config/tailport/config.yaml when
// XDG_CONFIG_HOME is unset -- whenever a Config's path is unset, i.e. a Config
// LITERAL built directly in a test (as most ui tests do via New(config.Config{
// ...})). A test that then triggers a save (favorite/label/lock keys, a publish
// flow, etc.) without isolating XDG_CONFIG_HOME would silently clobber the real
// config; that once wiped a developer's favorites. Only one of the ui test
// files set XDG per-test, and per-test isolation is easy to forget on the next
// test added. Pointing XDG_CONFIG_HOME at a throwaway dir here makes isolation
// the package default, independent of any individual test remembering it. Tests
// that set their own XDG_CONFIG_HOME via t.Setenv still work and restore to this
// safe default afterward.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tailport-ui-test-xdg-*")
	if err != nil {
		panic("tailport ui test isolation: " + err.Error())
	}
	if err := os.Setenv("XDG_CONFIG_HOME", dir); err != nil {
		panic("tailport ui test isolation: " + err.Error())
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// TestConfigWritesAreIsolated is the canary for kata 2kak: it fails loudly if
// TestMain's isolation ever stops covering config writes -- e.g. someone
// deletes or breaks TestMain, silently re-arming the footgun where a path-unset
// config.Save() (a Config literal built in a test, which is nearly every ui
// test) lands on the developer's real ~/.config/tailport/config.yaml. It
// asserts the effective config path resolves INSIDE the throwaway XDG dir and
// NOT under the real user config dir.
func TestConfigWritesAreIsolated(t *testing.T) {
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		t.Fatal("XDG_CONFIG_HOME is unset -- TestMain isolation is not active; a path-unset config.Save() could clobber the real ~/.config/tailport (kata 2kak)")
	}
	p, err := config.Path("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p, xdg+string(os.PathSeparator)) {
		t.Fatalf("config.Path(%q) = %q resolves outside the isolated XDG dir %q -- a real-config write is possible (kata 2kak)", "", p, xdg)
	}
	if home, herr := os.UserHomeDir(); herr == nil && home != "" {
		real := filepath.Join(home, ".config", "tailport")
		if strings.HasPrefix(p, real+string(os.PathSeparator)) {
			t.Fatalf("config.Path(%q) = %q points at the REAL config dir %q -- isolation broken (kata 2kak)", "", p, real)
		}
	}
}
