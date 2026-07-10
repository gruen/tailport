// Package config loads tailport's YAML config: the list of ports to hide
// from the default (filtered) view.
package config

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	ExcludePorts []int `yaml:"exclude_ports"`
}

var defaultExcludePorts = []int{
	22,    // sshd
	631,   // cups
	5353,  // mdns/avahi
	41641, // tailscale's own wireguard listener
}

func Default() Config {
	return Config{ExcludePorts: append([]int(nil), defaultExcludePorts...)}
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
	return cfg, nil
}

func (c Config) Excludes(port int) bool {
	for _, p := range c.ExcludePorts {
		if p == port {
			return true
		}
	}
	return false
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(Default())
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
