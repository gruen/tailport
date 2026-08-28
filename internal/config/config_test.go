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

// TestSaveWritesOwnerOnlyMode covers roborev 4ejm finding #2 (mode) and
// mzvh finding (atomicity): the config can hold a bcrypt auth_hash, so Save
// must never leave it group/world-readable -- not even transiently. Both a
// freshly-created file AND a pre-existing 0644 file must end up 0600, the
// resave must not corrupt content (proving the temp-file+rename swap is
// sound), and no ".config-*.yaml.tmp" scratch file may survive a
// successful Save (mzvh: Save writes via a same-directory temp file that's
// renamed over the target, never written-then-chmod'd in place).
func TestSaveWritesOwnerOnlyMode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
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
	assertNoLeftoverTempFiles(t, filepath.Dir(path))

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
	assertNoLeftoverTempFiles(t, filepath.Dir(path))

	// The atomic rename must not have corrupted content: the port set right
	// before the resave must round-trip intact.
	got, err := Load("")
	if err != nil {
		t.Fatalf("Load() after resave error: %v", err)
	}
	if meta, ok := got.Ports[3000]; !ok || meta.Label != "x" {
		t.Errorf("Load().Ports[3000] after resave = %+v (ok=%v), want Label=\"x\" present (atomic rename must preserve content)", meta, ok)
	}
	if _, ok := got.Ports[22]; !ok {
		t.Errorf("Load().Ports[22] missing after resave, want the seeded default (Default()'s locked SSH port) preserved")
	}
}

// assertNoLeftoverTempFiles fails the test if any Save-created scratch file
// (the ".config-*.yaml.tmp" pattern os.CreateTemp is given in Save) is still
// present in dir. A successful Save renames its temp file over the target,
// so nothing matching that pattern should ever survive.
func assertNoLeftoverTempFiles(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".config-*.yaml.tmp"))
	if err != nil {
		t.Fatalf("glob for leftover temp files: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("leftover temp file(s) after Save: %v", matches)
	}
}

// TestSaveWritesThroughSymlink covers roborev 3f5t/870: the atomic
// temp-file+rename Save (mzvh) introduced a regression where, if the
// resolved config path is itself a symlink (e.g. a user's dotfiles repo
// symlinks config.yaml into place), the rename would replace the symlink
// with a plain file -- silently breaking the link, unlike the pre-atomic
// os.WriteFile which wrote through it. Save must instead write through the
// link to its real target, leaving the symlink itself intact.
func TestSaveWritesThroughSymlink(t *testing.T) {
	dirA := t.TempDir()
	realPath := filepath.Join(dirA, "real.yaml")
	if err := os.WriteFile(realPath, []byte("stale: marker\n"), 0o644); err != nil {
		t.Fatalf("seeding real file: %v", err)
	}

	dirB := t.TempDir()
	linkPath := filepath.Join(dirB, "config.yaml")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	cfg := Default()
	cfg.path = linkPath
	cfg.Ports[4242] = PortMeta{Label: "through-symlink"}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	// The config path must still be a symlink, pointing at the same target.
	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("Lstat(linkPath): %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected %s to still be a symlink after Save, got mode %v", linkPath, info.Mode())
	}
	if got, err := os.Readlink(linkPath); err != nil {
		t.Fatalf("Readlink(linkPath): %v", err)
	} else if got != realPath {
		t.Errorf("Readlink(linkPath) = %q, want %q (symlink target must be unchanged)", got, realPath)
	}

	// The real target must hold the new content, at mode 0600.
	realInfo, err := os.Stat(realPath)
	if err != nil {
		t.Fatalf("Stat(realPath): %v", err)
	}
	if got := realInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("real file mode = %o, want 600", got)
	}
	raw, err := os.ReadFile(realPath)
	if err != nil {
		t.Fatalf("ReadFile(realPath): %v", err)
	}
	text := string(raw)
	if strings.Contains(text, "stale: marker") {
		t.Errorf("expected real file content to be replaced by Save, still contains stale marker:\n%s", text)
	}
	if !strings.Contains(text, "through-symlink") {
		t.Errorf("expected real file to contain the new content, got:\n%s", text)
	}
}

// TestSaveWritesThroughMultiHopDanglingSymlinkChain covers roborev kg6f/920:
// resolveSaveTarget's dangling-symlink fallback used to do a single
// os.Readlink hop, so a multi-hop dangling chain (cfg -> link2 ->
// missing.yaml, where missing.yaml doesn't exist yet) renamed over link2 --
// destroying the intermediate symlink -- instead of resolving through to
// missing.yaml. Save must write to missing.yaml (creating it) and leave both
// cfg and link2 intact as symlinks.
func TestSaveWritesThroughMultiHopDanglingSymlinkChain(t *testing.T) {
	dir := t.TempDir()
	missingPath := filepath.Join(dir, "missing.yaml")
	link2Path := filepath.Join(dir, "link2")
	cfgPath := filepath.Join(dir, "config.yaml")

	if err := os.Symlink(missingPath, link2Path); err != nil {
		t.Fatalf("Symlink(link2 -> missing): %v", err)
	}
	if err := os.Symlink(link2Path, cfgPath); err != nil {
		t.Fatalf("Symlink(cfg -> link2): %v", err)
	}

	cfg := Default()
	cfg.path = cfgPath
	cfg.Ports[4242] = PortMeta{Label: "multi-hop-dangling"}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	// Both links in the chain must still be symlinks, pointing at their
	// original (unchanged) targets.
	cfgInfo, err := os.Lstat(cfgPath)
	if err != nil {
		t.Fatalf("Lstat(cfgPath): %v", err)
	}
	if cfgInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected %s to still be a symlink after Save, got mode %v", cfgPath, cfgInfo.Mode())
	}
	if got, err := os.Readlink(cfgPath); err != nil {
		t.Fatalf("Readlink(cfgPath): %v", err)
	} else if got != link2Path {
		t.Errorf("Readlink(cfgPath) = %q, want %q (cfg symlink must be unchanged)", got, link2Path)
	}

	link2Info, err := os.Lstat(link2Path)
	if err != nil {
		t.Fatalf("Lstat(link2Path): %v", err)
	}
	if link2Info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected %s to still be a symlink after Save, got mode %v", link2Path, link2Info.Mode())
	}
	if got, err := os.Readlink(link2Path); err != nil {
		t.Fatalf("Readlink(link2Path): %v", err)
	} else if got != missingPath {
		t.Errorf("Readlink(link2Path) = %q, want %q (link2 symlink must be unchanged)", got, missingPath)
	}

	// The final target must now exist, holding the new content.
	raw, err := os.ReadFile(missingPath)
	if err != nil {
		t.Fatalf("ReadFile(missingPath): %v", err)
	}
	if !strings.Contains(string(raw), "multi-hop-dangling") {
		t.Errorf("expected missing.yaml to contain the new content, got:\n%s", raw)
	}
}

// TestSaveErrorsOnSymlinkLoop covers roborev kg6f/920: a symlink loop
// (a -> b -> a) must not be silently replaced with a plain file by Save's
// dangling-symlink fallback -- it must return an error instead.
func TestSaveErrorsOnSymlinkLoop(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a")
	bPath := filepath.Join(dir, "b")

	if err := os.Symlink(bPath, aPath); err != nil {
		t.Fatalf("Symlink(a -> b): %v", err)
	}
	if err := os.Symlink(aPath, bPath); err != nil {
		t.Fatalf("Symlink(b -> a): %v", err)
	}

	cfg := Default()
	cfg.path = aPath
	if err := cfg.Save(); err == nil {
		t.Fatal("Save() error = nil, want error for symlink loop")
	}

	// Neither link may have been replaced.
	for _, p := range []string{aPath, bPath} {
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("Lstat(%s): %v", p, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("expected %s to still be a symlink after failed Save, got mode %v", p, info.Mode())
		}
	}
}

// caddyDomainWritebackFixture is a realistic, hand-tuned config file used by
// the SaveCaddyDomain tests: it carries a ports block, a foreign top-level key
// (future_field) this build's Config struct does NOT model -- with both a head
// comment and an inline comment -- a full caddy block including an auth_hash,
// and a markers preference. SaveCaddyDomain must change ONLY caddy.domain and
// leave everything else (foreign key, comments, other fields) intact; a
// struct-re-encode "simplification" would drop future_field and its comments,
// failing the merge test loudly.
const caddyDomainWritebackFixture = `# tailport config (hand-tuned by a human)
ports:
    22:
        locked: true
    3000:
        label: dev server
        favorite: true
        last_process: vite
# future_field is a top-level key this build's Config struct does not model
# (a newer tailport wrote it, or a human added it in $EDITOR). It MUST survive
# a caddy.domain write-back -- a struct re-encode would silently drop it.
future_field: 42 # keep this inline comment too
caddy:
    hostname: caddy
    domain: old.example.com
    server_name: tailport
    admin_port: 2019
    auth_user: mg
    auth_hash: $2a$10$abcdefghijklmnopqrstuvABCDEFghijklmnopqrstuvwxyz012
markers: emoji
`

// TestSaveCaddyDomainMergePreservesForeignContent is THE test proving
// SaveCaddyDomain re-reads the file into a yaml.Node tree and merges, rather
// than re-encoding the in-memory Config (which would drop unknown keys and
// stale-overwrite concurrent edits -- the config-clobber history ycv1/OQ3
// avoids). Starting from a file with ports, a foreign top-level key + comments,
// a full caddy block, and a markers pref, a SaveCaddyDomain changes only
// caddy.domain: the foreign key, its comments, the other caddy fields, the
// ports, and the markers pref all survive. If someone later "simplifies" this
// back to a struct re-encode, future_field (and its comments) vanish and this
// fails.
func TestSaveCaddyDomainMergePreservesForeignContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(caddyDomainWritebackFixture), 0o644); err != nil {
		t.Fatalf("seeding fixture: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(fixture) error: %v", err)
	}
	if err := cfg.SaveCaddyDomain("apps.example.com"); err != nil {
		t.Fatalf("SaveCaddyDomain() error: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading merged config: %v", err)
	}
	text := string(raw)

	// The domain changed to the new value; the old value is gone.
	if !strings.Contains(text, "domain: apps.example.com") {
		t.Errorf("expected merged config to contain the new domain, got:\n%s", text)
	}
	if strings.Contains(text, "old.example.com") {
		t.Errorf("expected the old domain to be gone from the merged config, got:\n%s", text)
	}

	// The foreign top-level key SURVIVES -- the load-bearing proof of a
	// Node-tree merge (a struct re-encode would drop it entirely).
	if !strings.Contains(text, "future_field: 42") {
		t.Errorf("expected the unknown top-level key future_field to survive the merge, got:\n%s", text)
	}
	// Its comments (a non-tailport-managed head comment + an inline comment)
	// survive too -- only a Node re-read preserves these.
	for _, want := range []string{
		"future_field is a top-level key this build's Config struct does not model",
		"keep this inline comment too",
		"tailport config (hand-tuned by a human)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("expected foreign comment %q to survive the merge, got:\n%s", want, text)
		}
	}
	// The self-documenting caddy comments are present (applyCaddyComments is
	// re-applied on the merged tree, matching Save's invariant).
	for _, want := range []string{
		"Public base domain used to build publish hostnames",
		"Name of the shared Caddy JSON HTTP server under apps.http.servers",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("expected caddy comment %q present after merge, got:\n%s", want, text)
		}
	}

	// Everything else round-trips unchanged through Load.
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() after merge error: %v", err)
	}
	if got.Caddy.Domain != "apps.example.com" {
		t.Errorf("Load().Caddy.Domain = %q, want %q", got.Caddy.Domain, "apps.example.com")
	}
	if got.Caddy.Hostname != "caddy" || got.Caddy.ServerName != "tailport" || got.Caddy.AdminPort != 2019 {
		t.Errorf("Load().Caddy = %+v, want hostname/server_name/admin_port untouched", got.Caddy)
	}
	if got.Caddy.AuthUser != "mg" || got.Caddy.AuthHash != "$2a$10$abcdefghijklmnopqrstuvABCDEFghijklmnopqrstuvwxyz012" {
		t.Errorf("Load().Caddy auth = user %q hash %q, want the fixture's auth untouched", got.Caddy.AuthUser, got.Caddy.AuthHash)
	}
	if got.Markers != "emoji" {
		t.Errorf("Load().Markers = %q, want %q (unrelated field must survive)", got.Markers, "emoji")
	}
	if meta, ok := got.Ports[3000]; !ok || meta.Label != "dev server" || !meta.Favorite || meta.LastProcess != "vite" {
		t.Errorf("Load().Ports[3000] = %+v (ok=%v), want the fixture's port entry untouched", meta, ok)
	}
	if meta, ok := got.Ports[22]; !ok || !meta.Locked {
		t.Errorf("Load().Ports[22] = %+v (ok=%v), want locked SSH port untouched", meta, ok)
	}
}

// TestSaveCaddyDomainWritesBackup covers the .bak requirement: before
// overwriting, SaveCaddyDomain copies the file's CURRENT on-disk bytes to
// <path>.bak, which must exist, be mode 0600 (the config can hold a bcrypt
// auth_hash -- a world-readable backup would defeat the main file's 0600
// protection), and contain the pre-write bytes exactly.
func TestSaveCaddyDomainWritesBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(caddyDomainWritebackFixture), 0o600); err != nil {
		t.Fatalf("seeding fixture: %v", err)
	}

	cfg := Config{path: path}
	if err := cfg.SaveCaddyDomain("apps.example.com"); err != nil {
		t.Fatalf("SaveCaddyDomain() error: %v", err)
	}

	bakPath := path + ".bak"
	info, err := os.Stat(bakPath)
	if err != nil {
		t.Fatalf("expected %s to exist: %v", bakPath, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf(".bak mode = %o, want 600 (may hold an auth_hash)", got)
	}
	bak, err := os.ReadFile(bakPath)
	if err != nil {
		t.Fatalf("reading .bak: %v", err)
	}
	if string(bak) != caddyDomainWritebackFixture {
		t.Errorf(".bak contents = %q, want the exact pre-write bytes", string(bak))
	}
	// No Save-style scratch temp file may survive the write.
	assertNoLeftoverTempFiles(t, dir)
}

// TestSaveCaddyDomainLoadRoundTrip covers the basic contract: after
// SaveCaddyDomain, a fresh Load yields the new domain.
func TestSaveCaddyDomainLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(caddyDomainWritebackFixture), 0o600); err != nil {
		t.Fatalf("seeding fixture: %v", err)
	}

	cfg := Config{path: path}
	if err := cfg.SaveCaddyDomain("newdomain.example.org"); err != nil {
		t.Fatalf("SaveCaddyDomain() error: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.Caddy.Domain != "newdomain.example.org" {
		t.Errorf("Load().Caddy.Domain = %q, want %q", got.Caddy.Domain, "newdomain.example.org")
	}
}

// TestSaveCaddyDomainCreatesFileWhenAbsent covers the no-file-yet branch: with
// nothing on disk to back up, SaveCaddyDomain seeds a fresh Default() with the
// domain set (defaults + caddy comments), writes it 0600, creates any missing
// parent directory, and drops NO .bak.
func TestSaveCaddyDomainCreatesFileWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	// A parent dir that does not exist yet, to exercise the mkdir path.
	path := filepath.Join(dir, "sub", "config.yaml")

	cfg := Config{path: path}
	if err := cfg.SaveCaddyDomain("fresh.example.com"); err != nil {
		t.Fatalf("SaveCaddyDomain() error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected fresh config to be created: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("fresh config mode = %o, want 600", got)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("expected NO .bak when seeding a fresh file; stat err = %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.Caddy.Domain != "fresh.example.com" {
		t.Errorf("Load().Caddy.Domain = %q, want %q", got.Caddy.Domain, "fresh.example.com")
	}
	// Fresh seed carries the Default() caddy defaults and the locked SSH port.
	if got.Caddy.Hostname != "caddy" || got.Caddy.ServerName != "tailport" || got.Caddy.AdminPort != 2019 {
		t.Errorf("Load().Caddy = %+v, want defaults on a fresh seed", got.Caddy)
	}
	if meta, ok := got.Ports[22]; !ok || !meta.Locked {
		t.Errorf("Load().Ports[22] = %+v (ok=%v), want Default()'s locked SSH port", meta, ok)
	}
	// The fresh file is self-documenting (caddy comments present).
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fresh config: %v", err)
	}
	if !strings.Contains(string(raw), "Public base domain used to build publish hostnames") {
		t.Errorf("expected fresh seed to carry caddy comments, got:\n%s", raw)
	}
}

// TestSaveCaddyDomainInsertsMissingCaddyBlock covers a hand-minimal file that
// has no caddy block at all: SaveCaddyDomain must INSERT caddy.domain (and
// attach its explanatory comment) so the value lands, while preserving the
// file's other content.
func TestSaveCaddyDomainInsertsMissingCaddyBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	minimal := "ports:\n    8080:\n        favorite: true\nfuture_field: 7\n"
	if err := os.WriteFile(path, []byte(minimal), 0o600); err != nil {
		t.Fatalf("seeding minimal fixture: %v", err)
	}

	cfg := Config{path: path}
	if err := cfg.SaveCaddyDomain("apps.example.com"); err != nil {
		t.Fatalf("SaveCaddyDomain() error: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "domain: apps.example.com") {
		t.Errorf("expected inserted caddy.domain, got:\n%s", text)
	}
	if !strings.Contains(text, "Public base domain used to build publish hostnames") {
		t.Errorf("expected the inserted domain key to carry its comment, got:\n%s", text)
	}
	if !strings.Contains(text, "future_field: 7") {
		t.Errorf("expected the minimal file's other content to survive, got:\n%s", text)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.Caddy.Domain != "apps.example.com" {
		t.Errorf("Load().Caddy.Domain = %q, want %q", got.Caddy.Domain, "apps.example.com")
	}
	if meta, ok := got.Ports[8080]; !ok || !meta.Favorite {
		t.Errorf("Load().Ports[8080] = %+v (ok=%v), want the minimal file's port preserved", meta, ok)
	}
}

// TestSaveCaddyDomainWritesThroughSymlink mirrors TestSaveWritesThroughSymlink
// for the write-back path: when the resolved config path is a symlink (a
// dotfiles-repo setup), SaveCaddyDomain writes THROUGH to the real target
// (leaving the symlink itself intact), and its .bak lands next to the real
// target -- not next to the link.
func TestSaveCaddyDomainWritesThroughSymlink(t *testing.T) {
	dirA := t.TempDir()
	realPath := filepath.Join(dirA, "real.yaml")

	// Seed the real target with a valid tailport config (defaults + comments)
	// carrying an old domain.
	seed := Default()
	seed.path = realPath
	seed.Caddy.Domain = "old.example.com"
	if err := seed.Save(); err != nil {
		t.Fatalf("seeding real target: %v", err)
	}
	preBytes, err := os.ReadFile(realPath)
	if err != nil {
		t.Fatalf("reading seeded real target: %v", err)
	}

	dirB := t.TempDir()
	linkPath := filepath.Join(dirB, "config.yaml")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	cfg := Config{path: linkPath}
	if err := cfg.SaveCaddyDomain("apps.example.com"); err != nil {
		t.Fatalf("SaveCaddyDomain() error: %v", err)
	}

	// The link must still be a symlink pointing at the same real target.
	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("Lstat(linkPath): %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected %s to still be a symlink after SaveCaddyDomain, got mode %v", linkPath, info.Mode())
	}
	if got, err := os.Readlink(linkPath); err != nil {
		t.Fatalf("Readlink(linkPath): %v", err)
	} else if got != realPath {
		t.Errorf("Readlink(linkPath) = %q, want %q", got, realPath)
	}

	// The real target holds the new domain, at 0600.
	realInfo, err := os.Stat(realPath)
	if err != nil {
		t.Fatalf("Stat(realPath): %v", err)
	}
	if got := realInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("real target mode = %o, want 600", got)
	}
	realRaw, err := os.ReadFile(realPath)
	if err != nil {
		t.Fatalf("ReadFile(realPath): %v", err)
	}
	if !strings.Contains(string(realRaw), "domain: apps.example.com") {
		t.Errorf("expected real target to carry the new domain, got:\n%s", realRaw)
	}

	// The .bak lands next to the REAL target (with the pre-write bytes, 0600),
	// NOT next to the link.
	realBak := realPath + ".bak"
	bakInfo, err := os.Stat(realBak)
	if err != nil {
		t.Fatalf("expected .bak next to real target: %v", err)
	}
	if got := bakInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("real-target .bak mode = %o, want 600", got)
	}
	if bak, err := os.ReadFile(realBak); err != nil {
		t.Fatalf("reading real-target .bak: %v", err)
	} else if string(bak) != string(preBytes) {
		t.Errorf(".bak contents differ from the pre-write real-target bytes")
	}
	if _, err := os.Stat(linkPath + ".bak"); !os.IsNotExist(err) {
		t.Errorf("expected NO .bak next to the symlink; stat err = %v", err)
	}
}

// TestSaveCaddyDomainMainFileStays0600 covers the mode requirement on the main
// file for the merge path: a pre-existing world-readable (0644) config must be
// tightened to 0600 by the atomic overwrite, and its .bak written 0600 too.
func TestSaveCaddyDomainMainFileStays0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(caddyDomainWritebackFixture), 0o644); err != nil {
		t.Fatalf("seeding 0644 fixture: %v", err)
	}

	cfg := Config{path: path}
	if err := cfg.SaveCaddyDomain("apps.example.com"); err != nil {
		t.Fatalf("SaveCaddyDomain() error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat main file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("main config mode = %o, want 600 (pre-existing 0644 must be tightened)", got)
	}
	bakInfo, err := os.Stat(path + ".bak")
	if err != nil {
		t.Fatalf("stat .bak: %v", err)
	}
	if got := bakInfo.Mode().Perm(); got != 0o600 {
		t.Errorf(".bak mode = %o, want 600", got)
	}
	assertNoLeftoverTempFiles(t, dir)
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
