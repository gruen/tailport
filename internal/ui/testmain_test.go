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
// This was originally the sole guard for kata 2kak: config.Save fell back to
// config.Path("") -- which resolves to the real ~/.config/tailport/config.yaml
// when XDG_CONFIG_HOME is unset -- whenever a Config's path was unset, i.e. a
// Config LITERAL built directly in a test (as most ui tests do via New(config.
// Config{...})). A test that then triggered a save (favorite/label/lock keys, a
// publish flow, etc.) without isolating XDG_CONFIG_HOME would silently clobber
// the real config; that once wiped a developer's favorites. kata 34km closed
// that hole at the source: config.Save/SaveCaddyDomain/SaveCaddyHostname now
// refuse an unset-path Config outright with config.ErrNoPath (see the config
// package's saveTarget and TestSaveRefusesUnsetPath) instead of silently
// falling back to Path(""), so a stray unrooted Config literal in a test can no
// longer reach the real config at all -- that guard is now the PRIMARY
// defense. This TestMain isolation stays as defense IN DEPTH (e.g. against a
// future regression of that guard, or any direct config.Path("") read), and
// remains useful on its own terms: it also isolates tests that DO give their
// Config a path (via the ui test helper `rooted`, or config.Load) from ever
// resolving to the real XDG location if they forget their own t.Setenv.
// Pointing XDG_CONFIG_HOME at a throwaway dir here makes that the package
// default, independent of any individual test remembering it. Tests that set
// their own XDG_CONFIG_HOME via t.Setenv still work and restore to this safe
// default afterward.
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
// deletes or breaks TestMain. Since kata 34km, a path-unset config.Save() (a
// Config literal built directly, without going through the ui test helper
// `rooted` or config.Load) can no longer reach ANY file at all -- it returns
// config.ErrNoPath, defense primary. This test guards the layer below that
// guard: for a Config that DOES have a path (as `rooted` and config.Load
// give it), the path must still resolve inside the throwaway XDG dir, not
// the developer's real ~/.config/tailport/config.yaml, so isolation stays
// defense in depth even if the primary guard ever regressed.
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
