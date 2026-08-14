// Package config loads and persists tailport's YAML config: a per-port
// registry of labels and favorites that drives the default (filtered) view.
package config

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// PortMeta holds user-set metadata for a single port: a custom label,
// favorite status, and/or lock state. An entry existing in the registry
// (regardless of its field values) means the port is "known" and should
// stay visible in the default view even when nothing's currently
// listening on it.
type PortMeta struct {
	Label    string `yaml:"label,omitempty"`
	Favorite bool   `yaml:"favorite,omitempty"`
	// Locked blocks the port from being exposed via tailscale serve
	// (toggled on) until explicitly unlocked. It never blocks toggling
	// off, labeling, or favoriting.
	Locked bool `yaml:"locked,omitempty"`
	// LastProcess is the most recent process name seen listening on this port.
	// It's remembered so a favorite that goes down can still show what used to
	// run there ("was mailpit") instead of an anonymous "?".
	LastProcess string `yaml:"last_process,omitempty"`
}

// CaddyConfig holds the settings for publishing a tailnet-served port to a
// custom public hostname through a user-controlled Caddy edge node (the `P`
// key, kata v1z5). Unlike PortMeta entries, this block is ALWAYS present in
// the saved config -- see applyDefaults and the comment on Save -- so a new
// user sees the available knobs (and what they mean) without reading docs.
type CaddyConfig struct {
	// Hostname is the tailnet name of the Caddy edge node (short MagicDNS
	// label or FQDN); tailport reaches its admin API here. Defaults to
	// "caddy".
	Hostname string `yaml:"hostname"`
	// Domain is the public base domain used to build publish hostnames.
	// Blank (the default) means publishing is unconfigured: no safe
	// universal value can be inferred, and a blank domain suppresses edge
	// polling until the user sets one.
	Domain string `yaml:"domain"`
	// ServerName is the name of the shared Caddy JSON HTTP server under
	// apps.http.servers that tailport manages. Defaults to "tailport".
	ServerName string `yaml:"server_name"`
	// AdminPort is the port of the Caddy admin API on the edge node
	// (reachable tailnet-only). Defaults to 2019.
	AdminPort int `yaml:"admin_port"`
	// AuthUser, when set, is the basic-auth username enforced at the Caddy
	// edge for published hostnames. Empty by default (no auth).
	AuthUser string `yaml:"auth_user,omitempty"`
	// AuthHash, when set, is the basic-auth password hash enforced at the
	// Caddy edge. This MUST be a bcrypt MCF-formatted hash (e.g. as
	// produced by `caddy hash-password`) -- NEVER a plaintext password.
	// Empty by default.
	AuthHash string `yaml:"auth_hash,omitempty"`
}

// applyDefaults fills any zero-value field that has a sensible default,
// leaving Domain blank (see the Domain doc comment -- no safe universal
// value exists for it) and AuthUser/AuthHash blank (no auth by default).
func (c *CaddyConfig) applyDefaults() {
	if c.Hostname == "" {
		c.Hostname = "caddy"
	}
	if c.ServerName == "" {
		c.ServerName = "tailport"
	}
	if c.AdminPort == 0 {
		c.AdminPort = 2019
	}
}

// Config is the persisted per-port registry, plus display preferences.
type Config struct {
	Ports map[int]PortMeta `yaml:"ports"`
	// Caddy holds the publish-via-Caddy-edge settings (kata v1z5). Always
	// present in the saved file with defaults + explanatory comments --
	// see CaddyConfig and applyCaddyComments.
	Caddy CaddyConfig `yaml:"caddy"`
	// Markers selects the EXPOSURE-marker glyph style (qwcw): the port-state
	// moon-phase ramp 🌕 localhost/🌔 local network/🌒 on tailnet (served or
	// bound wide)/🌑 funnelled to the internet (plus the off-ramp 🌫️ stale/
	// ✕ offline), or its mono fallback ○/◔/◉/●/▲/✕. Empty (the default, ""
	// -- not set) means MONO/ASCII; "auto" opts into detecting a
	// UTF-8-capable terminal and picking emoji there; "emoji"/"ascii" force
	// a set outright regardless of the terminal. This governs the exposure
	// markers ONLY -- the hidden Easter-egg overlay and its fireworks always
	// auto-detect their own glyph style independently of this setting.
	Markers string `yaml:"markers,omitempty"`

	// Theme selects the color-scheme mode (kata n7gc): "auto" (default;
	// detect the terminal's background via lipgloss/termenv, falling back to
	// dark if it can't be detected), "light" (force light-background
	// colors), or "dark" (force dark-background colors -- tailport's
	// original, pre-n7gc look). Empty means auto. See internal/ui's
	// ApplyTheme for how this is applied.
	Theme string `yaml:"theme,omitempty"`

	// path is the file this Config was resolved against by Load/WriteDefault
	// (see Path), and what Save writes back to. Unexported so it never
	// round-trips into the YAML file itself. Zero value ("") means "not yet
	// resolved" -- Save falls back to Path("") in that case, which preserves
	// old behavior for callers (mainly tests) that build a Config literal
	// directly instead of going through Load.
	path string
}

// ResolvedPath returns the file this Config is bound to: what Load resolved
// it from, or what WriteDefault seeded it at. Empty if the Config was never
// routed through either (e.g. a literal built directly by a test).
func (c Config) ResolvedPath() string { return c.path }

// Default returns a registry seeded with port 22 (SSH) locked, so a
// fresh install doesn't accidentally expose it via tailscale serve
// before the user has looked at the tool. All other ports start
// unregistered.
func Default() Config {
	cfg := Config{Ports: map[int]PortMeta{22: {Locked: true}}}
	cfg.Caddy.applyDefaults()
	return cfg
}

// Path returns the config file location. override, when non-empty, is an
// explicit `-c`/`--config <path>` value and wins outright. Otherwise the
// location honors XDG_CONFIG_HOME, falling back to ~/.config/tailport. The
// full precedence is: override > $XDG_CONFIG_HOME > ~/.config.
func Path(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "tailport", "config.yaml"), nil
}

// Load reads the config file, returning defaults if it doesn't exist yet.
// override behaves as in Path: a non-empty value pins the file read (and any
// later Save on the returned Config) to that exact path.
func Load(override string) (Config, error) {
	path, err := Path(override)
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		cfg := Default()
		cfg.path = path
		return cfg, nil
	}
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.Ports == nil {
		cfg.Ports = map[int]PortMeta{}
	}
	// A pre-existing config file from before v1z5 (or one with no caddy:
	// block for any other reason) still ends up with the defaults in the
	// in-memory struct here, and the block appears -- with its comments --
	// on the next Save.
	cfg.Caddy.applyDefaults()
	cfg.path = path
	return cfg, nil
}

// Save writes the config to disk, creating the parent directory if needed.
// It writes to the path this Config was resolved against by Load or
// WriteDefault (ResolvedPath); if the Config was never routed through
// either (path is unset -- e.g. a literal built directly by a test), it
// falls back to Path(""), matching the pre-override default. Called
// immediately after any registry mutation (label set, favorite toggled,
// port remembered) so changes survive restarts without requiring a clean
// exit.
//
// Save deliberately does NOT do plain yaml.Marshal(c): that discards
// comments, and the caddy: block's explanatory comments are a UX
// requirement (kata v1z5) -- a new user must understand
// hostname/domain/server_name/admin_port without reading docs. Instead it
// encodes the struct into a fresh yaml.Node tree (which carries the
// current field values, generated fresh every call) and then re-applies
// the caddy comments onto that tree (applyCaddyComments) before
// marshaling. Because both steps run unconditionally on every Save,
// values and comments both survive every save -- including one that only
// changed an unrelated field (e.g. a port label) -- without needing to
// retain a parsed Node tree across calls.
//
// The file is written 0600 (and any pre-existing config tightened to it):
// the caddy block can hold a bcrypt auth_hash, and a world-readable hash
// invites offline cracking by other local users. c is a value receiver, so
// applying the caddy defaults here is local to this copy and just makes the
// "saved caddy block always carries visible defaults" invariant hold even
// for a Config literal that never went through Default()/Load().
func (c Config) Save() error {
	c.Caddy.applyDefaults()
	path := c.path
	if path == "" {
		var err error
		path, err = Path("")
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var root yaml.Node
	if err := root.Encode(c); err != nil {
		return err
	}
	applyCaddyComments(&root)
	data, err := yaml.Marshal(&root)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	// WriteFile only sets the mode when it creates the file; an existing
	// config written before the 0600 default would keep its old (possibly
	// 0644) mode, so tighten it explicitly now that it can hold a credential.
	return os.Chmod(path, 0o600)
}

// applyCaddyComments sets the explanatory head comments on the caddy:
// block's keys, matching mg's plan-review decision on the exact text.
// These strings are constants tailport owns (not user data), so Save
// re-applies them on every write, which is what makes them durable across
// saves that touch fields outside the caddy block.
func applyCaddyComments(root *yaml.Node) {
	caddy := mappingValueNode(root, "caddy")
	if caddy == nil {
		return
	}
	setKeyHeadComment(caddy, "hostname",
		"Tailnet name of the Caddy edge node (short MagicDNS label or FQDN);\ntailport reaches its admin API here.")
	setKeyHeadComment(caddy, "domain",
		"\nPublic base domain used to build publish hostnames. Point its DNS\n"+
			"(typically a wildcard) at the public Caddy edge before publishing.")
	setKeyHeadComment(caddy, "server_name",
		"\nName of the shared Caddy JSON HTTP server under apps.http.servers.\n"+
			"Every tailport computer publishing through this same Caddy edge must\n"+
			"use the same value; this does not identify the source computer.")
	setKeyHeadComment(caddy, "admin_port",
		"\nPort of the Caddy admin API on the edge (reachable tailnet-only).")
}

// mappingValueNode returns the value node for key within mapping node m, or
// nil if m isn't a mapping node or doesn't contain key.
func mappingValueNode(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// setKeyHeadComment sets the HeadComment on key's key-node within mapping
// node m. In yaml.v3, a head comment on a mapping pair attaches to the key
// node, not the value node.
func setKeyHeadComment(m *yaml.Node, key, comment string) {
	if m == nil || m.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i].HeadComment = comment
			return
		}
	}
}

// WriteDefault writes the default config to disk if no config exists yet,
// at the location override resolves to (see Path).
func WriteDefault(override string) error {
	path, err := Path(override)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return nil // already exists, don't clobber
	}
	d := Default()
	d.path = path
	return d.Save()
}
