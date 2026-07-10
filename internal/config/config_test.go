package config

import (
	"path/filepath"
	"testing"
)

func TestPathHonorsXDGConfigHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdgtest")
	got, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/tmp/xdgtest", "tailport", "config.yaml")
	if got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

func TestExcludes(t *testing.T) {
	cfg := Config{ExcludePorts: []int{22, 631}}
	if !cfg.Excludes(22) {
		t.Error("expected 22 to be excluded")
	}
	if cfg.Excludes(8080) {
		t.Error("did not expect 8080 to be excluded")
	}
}

func TestDefaultIncludesSSH(t *testing.T) {
	if !Default().Excludes(22) {
		t.Error("expected default config to exclude ssh (22)")
	}
}
