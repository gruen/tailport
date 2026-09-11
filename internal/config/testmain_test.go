package config

import (
	"os"
	"testing"
)

// TestMain isolates config writes for the ENTIRE internal/config test binary as
// defense in depth for kata 2kak. Save falls back to Path("") -- the real
// ~/.config/tailport/config.yaml when XDG_CONFIG_HOME is unset -- for a Config
// whose path is unset, so a stray Save() in a test could clobber the real
// config. This package's tests already isolate themselves per-test (every
// Path/Save/Load case sets its own XDG_CONFIG_HOME or a temp path), and no test
// depends on XDG being unset; setting a throwaway XDG_CONFIG_HOME here just makes
// that guarantee the package default rather than a per-test discipline. Tests
// that set their own XDG_CONFIG_HOME via t.Setenv still win and restore to this
// safe default afterward.
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
