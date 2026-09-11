package main

import (
	"os"
	"testing"
)

// TestMain isolates config writes for the ENTIRE cmd/tailport test binary as
// defense in depth for kata 2kak. These tests exercise run(), which loads and
// (via the TUI's state) can save the config; config.Save falls back to
// config.Path("") -- the real ~/.config/tailport/config.yaml when
// XDG_CONFIG_HOME is unset -- for a Config with no resolved path. The tests pass
// an explicit -c/configPath, but pinning XDG_CONFIG_HOME to a throwaway dir here
// guarantees no test can reach the real config even if a future one forgets.
// Per-test t.Setenv overrides still win and restore to this safe default.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tailport-cmd-test-xdg-*")
	if err != nil {
		panic("tailport cmd test isolation: " + err.Error())
	}
	if err := os.Setenv("XDG_CONFIG_HOME", dir); err != nil {
		panic("tailport cmd test isolation: " + err.Error())
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
