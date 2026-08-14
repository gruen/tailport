package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathHonorsXDGConfigHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdgtest")
	got, err := Path("")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/tmp/xdgtest", "tailport", "config.yaml")
	if got != want {
		t.Errorf("Path(\"\") = %q, want %q", got, want)
	}
}

// TestPathOverrideWinsOverXDG covers y4gt's precedence rule: an explicit
// override (as -c/--config passes) always wins, even when XDG_CONFIG_HOME is
// also set.
func TestPathOverrideWinsOverXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdgtest")
	got, err := Path("/tmp/explicit-override.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if want := "/tmp/explicit-override.yaml"; got != want {
		t.Errorf("Path(override) = %q, want %q", got, want)
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
	cfg.Ports[3000] = PortMeta{Label: "dev server", Favorite: true, LastProcess: "vite"}
	cfg.Ports[9000] = PortMeta{Favorite: false}

	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	got, err := Load("")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// 3: the seeded port 22 (locked) plus the two set above.
	if len(got.Ports) != 3 {
		t.Fatalf("Load() got %d ports, want 3: %v", len(got.Ports), got.Ports)
	}
	if meta := got.Ports[3000]; meta.Label != "dev server" || !meta.Favorite || meta.LastProcess != "vite" {
		t.Errorf("Load() port 3000 = %+v, want Label=\"dev server\" Favorite=true LastProcess=vite", meta)
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
	got, err := Load("")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.Markers != "emoji" {
		t.Errorf("Load() Markers = %q, want %q", got.Markers, "emoji")
	}
}

func TestLoadReturnsDefaultWhenAbsent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	got, err := Load("")
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

// TestOverridePathIsolatesFromXDGDefault covers y4gt's acceptance bar
// directly: an explicit override path seeds, loads, and saves at that exact
// file, and never touches the XDG-resolved default -- even when a mutation
// (Save, via the round-tripped Config) happens afterward.
func TestOverridePathIsolatesFromXDGDefault(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	overridePath := filepath.Join(t.TempDir(), "profile-a.yaml")

	// WriteDefault seeds only the override path.
	if err := WriteDefault(overridePath); err != nil {
		t.Fatalf("WriteDefault(override) error: %v", err)
	}
	if _, err := os.Stat(overridePath); err != nil {
		t.Fatalf("expected override path to be seeded: %v", err)
	}
	xdgDefault, err := Path("")
	if err != nil {
		t.Fatalf("Path(\"\") error: %v", err)
	}
	if _, err := os.Stat(xdgDefault); !os.IsNotExist(err) {
		t.Fatalf("WriteDefault(override) must not touch the XDG default; stat err = %v", err)
	}

	// Load reads back from the override path and binds ResolvedPath to it.
	cfg, err := Load(overridePath)
	if err != nil {
		t.Fatalf("Load(override) error: %v", err)
	}
	if cfg.ResolvedPath() != overridePath {
		t.Errorf("cfg.ResolvedPath() = %q, want %q", cfg.ResolvedPath(), overridePath)
	}

	// A mutation + Save persists back to the override path only.
	cfg.Ports[3000] = PortMeta{Label: "profile-a"}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	reloaded, err := Load(overridePath)
	if err != nil {
		t.Fatalf("Load(override) after save error: %v", err)
	}
	if reloaded.Ports[3000].Label != "profile-a" {
		t.Errorf("expected the mutation to persist at the override path, got %+v", reloaded.Ports[3000])
	}
	if _, err := os.Stat(xdgDefault); !os.IsNotExist(err) {
		t.Fatalf("Save() after Load(override) must not create the XDG default; stat err = %v", err)
	}
}

// TestFreshSeedIncludesCaddyBlockAndComments covers v1z5/hhha: a newly
// seeded config file always carries the caddy: block, its default values,
// and the seeded explanatory comments -- not just when the user touches
// caddy settings.
func TestFreshSeedIncludesCaddyBlockAndComments(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if err := WriteDefault(""); err != nil {
		t.Fatalf("WriteDefault() error: %v", err)
	}
	path, err := Path("")
	if err != nil {
		t.Fatalf("Path(\"\") error: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading seeded config: %v", err)
	}
	text := string(raw)

	if !strings.Contains(text, "caddy:") {
		t.Errorf("expected seeded config to contain a caddy: block, got:\n%s", text)
	}
	for _, want := range []string{
		`hostname: caddy`,
		`domain: ""`,
		`server_name: tailport`,
		`admin_port: 2019`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("expected seeded config to contain %q, got:\n%s", want, text)
		}
	}
	for _, want := range []string{
		"Tailnet name of the Caddy edge node",
		"Public base domain used to build publish hostnames",
		"Name of the shared Caddy JSON HTTP server under apps.http.servers",
		"Port of the Caddy admin API on the edge",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("expected seeded config to contain comment %q, got:\n%s", want, text)
		}
	}

	// The in-struct defaults (from Load, which a caller uses right after
	// WriteDefault) must match what's on disk.
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	want := CaddyConfig{Hostname: "caddy", ServerName: "tailport", AdminPort: 2019}
	if cfg.Caddy != want {
		t.Errorf("Load().Caddy = %+v, want %+v", cfg.Caddy, want)
	}
}

// TestCaddyRoundTrip covers v1z5/hhha: caddy settings persist across a
// Save/Load cycle like any other config field.
func TestCaddyRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg := Default()
	cfg.Caddy.Domain = "example.com"
	cfg.Caddy.AuthUser = "mg"
	cfg.Caddy.AuthHash = "$2a$10$examplebcrypthashvalueexamplebcrypthash"
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	got, err := Load("")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.Caddy.Domain != "example.com" {
		t.Errorf("Load().Caddy.Domain = %q, want %q", got.Caddy.Domain, "example.com")
	}
	if got.Caddy.AuthUser != "mg" {
		t.Errorf("Load().Caddy.AuthUser = %q, want %q", got.Caddy.AuthUser, "mg")
	}
	if got.Caddy.AuthHash != cfg.Caddy.AuthHash {
		t.Errorf("Load().Caddy.AuthHash = %q, want %q", got.Caddy.AuthHash, cfg.Caddy.AuthHash)
	}
	// Untouched fields still carry their defaults.
	if got.Caddy.Hostname != "caddy" || got.Caddy.ServerName != "tailport" || got.Caddy.AdminPort != 2019 {
		t.Errorf("Load().Caddy = %+v, want defaults for hostname/server_name/admin_port", got.Caddy)
	}
}

// TestCaddyCommentsSurviveUnrelatedFieldSave is the dedicated
// comment-preservation test v1z5/hhha specifically demands: the seeded
// caddy: comments (and values) must survive a Save that changes a field
// that has nothing to do with caddy -- proving Save's encode-then-reapply
// mechanism (see the comment on Save) isn't just a fresh-seed artifact.
func TestCaddyCommentsSurviveUnrelatedFieldSave(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if err := WriteDefault(""); err != nil {
		t.Fatalf("WriteDefault() error: %v", err)
	}
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// An unrelated mutation: register a new port. Nothing about the caddy
	// block changes.
	cfg.Ports[3000] = PortMeta{Label: "x"}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	path, err := Path("")
	if err != nil {
		t.Fatalf("Path(\"\") error: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading saved config: %v", err)
	}
	text := string(raw)

	for _, want := range []string{
		"Public base domain used to build publish hostnames",
		"Name of the shared Caddy JSON HTTP server under apps.http.servers",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("expected saved config to still contain comment %q after an unrelated save, got:\n%s", want, text)
		}
	}

	got, err := Load("")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.Caddy.Hostname != "caddy" || got.Caddy.ServerName != "tailport" || got.Caddy.AdminPort != 2019 {
		t.Errorf("Load().Caddy = %+v, want defaults intact after unrelated save", got.Caddy)
	}
	if meta, ok := got.Ports[3000]; !ok || meta.Label != "x" {
		t.Errorf("Load().Ports[3000] = %+v (ok=%v), want Label=\"x\" present", meta, ok)
	}
}

// TestSaveWritesOwnerOnlyMode covers roborev 4ejm finding #2: the config can
// hold a bcrypt auth_hash, so Save must not leave it group/world-readable.
// Both a freshly-created file AND a pre-existing 0644 file must end up 0600.
func TestSaveWritesOwnerOnlyMode(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, err := Path("")
	if err != nil {
		t.Fatalf("Path(\"\") error: %v", err)
	}

	// Fresh create via Save.
	if err := Default().Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat after create: %v", err)
	} else if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("freshly saved config mode = %o, want 600", got)
	}

	// A pre-existing world-readable file must be tightened on the next Save.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod 0644: %v", err)
	}
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	cfg.Ports[3000] = PortMeta{Label: "x"}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat after resave: %v", err)
	} else if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("resaved config mode = %o, want 600 (existing 0644 not tightened)", got)
	}
}

// TestSaveAppliesCaddyDefaultsForLiteral covers roborev 4ejm finding #3: a
// Config literal built directly (never through Default()/Load(), which apply
// the defaults) must still write visible caddy defaults, not empty/zero values.
func TestSaveAppliesCaddyDefaultsForLiteral(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// A bare literal: no caddy fields set, no Default()/Load() in the path.
	cfg := Config{Ports: map[int]PortMeta{8080: {Favorite: true}}}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	got, err := Load("")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.Caddy.Hostname != "caddy" || got.Caddy.ServerName != "tailport" || got.Caddy.AdminPort != 2019 {
		t.Errorf("literal Save() then Load().Caddy = %+v, want visible defaults (caddy/tailport/2019)", got.Caddy)
	}

	path, err := Path("")
	if err != nil {
		t.Fatalf("Path(\"\") error: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading saved config: %v", err)
	}
	// The file itself must show the defaults, not hostname:"" / admin_port:0.
	for _, want := range []string{"hostname: caddy", "server_name: tailport", "admin_port: 2019"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("expected saved literal config to contain %q, got:\n%s", want, raw)
		}
	}
}
