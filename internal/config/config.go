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

// Config is the persisted per-port registry, plus display preferences.
type Config struct {
	Ports map[int]PortMeta `yaml:"ports"`
	// Markers selects the port exposure-state glyphs: "auto" (default; emoji
	// on a UTF-8-capable terminal, else ASCII), "emoji" (force the egg
	// lifecycle 🥚/🐣/🐦/🪹), or "ascii" (force ○/●/◉/▲). Empty means auto.
	Markers string `yaml:"markers,omitempty"`

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
	return Config{Ports: map[int]PortMeta{22: {Locked: true}}}
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
func (c Config) Save() error {
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
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
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
