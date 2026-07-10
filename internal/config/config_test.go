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

func TestDefaultIsEmptyRegistry(t *testing.T) {
	cfg := Default()
	if len(cfg.Ports) != 0 {
		t.Errorf("expected Default() to have no port entries, got %v", cfg.Ports)
	}
}

func TestPortGainsEntryWhenFavorited(t *testing.T) {
	cfg := Default()
	cfg.Ports[8080] = PortMeta{Favorite: true}
	meta, ok := cfg.Ports[8080]
	if !ok {
		t.Fatal("expected port 8080 to have a registry entry")
	}
	if !meta.Favorite {
		t.Error("expected port 8080 to be marked favorite")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg := Default()
	cfg.Ports[3000] = PortMeta{Label: "dev server", Favorite: true}
	cfg.Ports[9000] = PortMeta{Favorite: false}

	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if len(got.Ports) != 2 {
		t.Fatalf("Load() got %d ports, want 2: %v", len(got.Ports), got.Ports)
	}
	if meta := got.Ports[3000]; meta.Label != "dev server" || !meta.Favorite {
		t.Errorf("Load() port 3000 = %+v, want Label=\"dev server\" Favorite=true", meta)
	}
	if meta, ok := got.Ports[9000]; !ok || meta.Favorite {
		t.Errorf("Load() port 9000 = %+v (ok=%v), want present, Favorite=false", meta, ok)
	}
}

func TestLoadReturnsDefaultWhenAbsent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if len(got.Ports) != 0 {
		t.Errorf("expected empty registry when no config file exists, got %v", got.Ports)
	}
}
