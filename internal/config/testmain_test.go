package config

import (
	"os"
	"testing"
)

// TestMain isolates config writes for the ENTIRE internal/config test binary.
// Originally this was the sole defense for kata 2kak: Save fell back to
// Path("") -- the real ~/.config/tailport/config.yaml when XDG_CONFIG_HOME is
// unset -- for a Config whose path is unset, so a stray Save() in a test
// could clobber the real config. kata 34km made that fallback impossible
// outright: Save, SaveCaddyDomain, and SaveCaddyHostname now refuse an
// unset-path Config with ErrNoPath (see saveTarget in config.go and
// TestSaveRefusesUnsetPath), so that guard is now the PRIMARY defense. This
// TestMain isolation stays as defense IN DEPTH -- e.g. against a future
// regression of the guard, or any other code path that resolves Path("")
// directly. This package's tests already isolate themselves per-test (every
// Path/Save/Load case sets its own XDG_CONFIG_HOME or a temp path, or roots
// its Config's path explicitly), and no test depends on XDG being unset;
// setting a throwaway XDG_CONFIG_HOME here just makes that guarantee the
// package default rather than a per-test discipline. Tests that set their own
// XDG_CONFIG_HOME via t.Setenv still win and restore to this safe default
// afterward.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tailport-config-test-xdg-*")
	if err != nil {
		panic("tailport config test isolation: " + err.Error())
	}
	if err := os.Setenv("XDG_CONFIG_HOME", dir); err != nil {
		panic("tailport config test isolation: " + err.Error())
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
