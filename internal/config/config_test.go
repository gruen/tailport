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

// TestDefaultSeedsLockedPort22 replaces the old TestDefaultIsEmptyRegistry:
// Default() now seeds a single entry, port 22 (SSH) locked, so a fresh
// install never accidentally exposes it via tailscale serve. This is a
// deliberate behavior change (see kata 7f0z) from the prior empty-registry
// default.
func TestDefaultSeedsLockedPort22(t *testing.T) {
	cfg := Default()
	if len(cfg.Ports) != 1 {
		t.Fatalf("expected Default() to have exactly one port entry, got %v", cfg.Ports)
	}
	meta, ok := cfg.Ports[22]
	if !ok {
		t.Fatal("expected Default() to seed a registry entry for port 22")
	}
	if !meta.Locked {
		t.Error("expected Default() port 22 entry to be locked")
	}
	if meta.Favorite || meta.Label != "" {
		t.Errorf("expected Default() port 22 entry to have no favorite/label, got %+v", meta)
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

	// 3: the seeded port 22 (locked) plus the two set above.
	if len(got.Ports) != 3 {
		t.Fatalf("Load() got %d ports, want 3: %v", len(got.Ports), got.Ports)
	}
	if meta := got.Ports[3000]; meta.Label != "dev server" || !meta.Favorite {
		t.Errorf("Load() port 3000 = %+v, want Label=\"dev server\" Favorite=true", meta)
	}
	if meta, ok := got.Ports[9000]; !ok || meta.Favorite {
		t.Errorf("Load() port 9000 = %+v (ok=%v), want present, Favorite=false", meta, ok)
	}
	if meta, ok := got.Ports[22]; !ok || !meta.Locked {
		t.Errorf("Load() port 22 = %+v (ok=%v), want present, Locked=true", meta, ok)
	}
}

// TestMarkersRoundTrip covers the display preference (sqvm): the markers mode
// persists and reloads, and is omitted from the file when empty (auto).
func TestMarkersRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg := Default()
	cfg.Markers = "emoji"
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.Markers != "emoji" {
		t.Errorf("Load() Markers = %q, want %q", got.Markers, "emoji")
	}
}

func TestLoadReturnsDefaultWhenAbsent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	// Matches Default(): a single seeded, locked entry for port 22.
	if len(got.Ports) != 1 {
		t.Errorf("expected registry with only the seeded port 22 when no config file exists, got %v", got.Ports)
	}
	if meta, ok := got.Ports[22]; !ok || !meta.Locked {
		t.Errorf("expected port 22 to be present and locked when no config file exists, got %+v (ok=%v)", meta, ok)
	}
}
