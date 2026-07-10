// Package config loads and persists tailport's YAML config: a per-port
// registry of labels and favorites that drives the default (filtered) view.
package config

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// PortMeta holds user-set metadata for a single port: a custom label
// and/or favorite status. An entry existing in the registry (regardless
// of its field values) means the port is "known" and should stay visible
// in the default view even when nothing's currently listening on it.
type PortMeta struct {
	Label    string `yaml:"label,omitempty"`
	Favorite bool   `yaml:"favorite,omitempty"`
}

// Config is the persisted per-port registry.
type Config struct {
	Ports map[int]PortMeta `yaml:"ports"`
}

// Default returns an empty registry: no ports are pre-known.
func Default() Config {
	return Config{Ports: map[int]PortMeta{}}
}

// Path returns the config file location, honoring XDG_CONFIG_HOME.
func Path() (string, error) {
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
func Load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Default(), nil
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
	return cfg, nil
}

// Save writes the config to disk at Path(), creating the parent directory
// if needed. Called immediately after any registry mutation (label set,
// favorite toggled, port remembered) so changes survive restarts without
// requiring a clean exit.
func (c Config) Save() error {
	path, err := Path()
	if err != nil {
		return err
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

// WriteDefault writes the default config to disk if no config exists yet.
func WriteDefault() error {
	path, err := Path()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return nil // already exists, don't clobber
	}
	return Default().Save()
}
