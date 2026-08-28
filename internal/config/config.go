// Package config loads and persists tailport's YAML config: a per-port
// registry of labels and favorites that drives the default (filtered) view.
package config

import (
	"fmt"
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
// The file is written 0600 from creation, never written-then-chmod'd: the
// caddy block can hold a bcrypt auth_hash, and a world-readable hash invites
// offline cracking by other local users. roborev mzvh found that the
// original write-then-chmod sequence left a window (and, if Chmod failed, an
// indefinite window) where a pre-existing 0644 config held the freshly
// written hash before its mode was tightened. Save now writes the new
// content to a 0600 temp file in the SAME directory as path and renames it
// over path -- an atomic same-filesystem swap (mirroring
// selfupdate.ReplaceExecutable) that is secure from the first byte and also
// eliminates any torn-write risk. On any error the temp file is removed and
// the existing config is left byte-for-byte untouched.
//
// If path is itself a symlink (e.g. a user symlinks config.yaml into a
// dotfiles repo), renaming onto path would replace the link with a plain
// file, silently breaking it -- unlike the pre-atomic-write os.WriteFile,
// which wrote through the symlink to its target. roborev 3f5t/870 caught
// this regression. resolveSaveTarget resolves that case: the temp file is
// created in the REAL target's directory and the rename lands on the real
// target path, leaving the symlink itself untouched and now pointing at the
// freshly written file. A non-symlink or not-yet-existing path resolves to
// itself, matching the prior behavior exactly.
//
// c is a value receiver, so applying the caddy defaults here is local to
// this copy and just makes the "saved caddy block always carries visible
// defaults" invariant hold even for a Config literal that never went through
// Default()/Load().
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
	target, err := resolveSaveTarget(path)
	if err != nil {
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
	return writeConfigAtomic(target, data)
}

// writeConfigAtomic writes data to target using the symlink-safe atomic swap
// Save's doc comment describes: a 0600 temp file in target's OWN directory,
// written, explicitly Chmod'd 0600, closed, then os.Rename'd over target -- an
// atomic same-filesystem replace that is secure from the first byte (never
// written-then-chmod'd in place, so an auth_hash is never transiently
// world-readable) and free of torn-write risk. target must ALREADY be the
// resolved save target (see resolveSaveTarget); the parent directory is created
// if needed. On any error the temp file is removed and target is left
// byte-for-byte untouched.
//
// Factored out of Save so Save and SaveCaddyDomain share one audited write
// path (mzvh/3f5t/kg6f all live here) rather than diverging copies.
func writeConfigAtomic(target string, data []byte) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// os.CreateTemp creates the file 0600 already, but the umask can still
	// widen that in principle, so Chmod explicitly rather than relying on
	// the creation mode alone.
	tmp, err := os.CreateTemp(dir, ".config-*.yaml.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	remove := func() { _ = tmp.Close(); _ = os.Remove(tmpName) }

	if _, err := tmp.Write(data); err != nil {
		remove()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		remove()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// SaveCaddyDomain persists ONLY the caddy.domain field to the on-disk config,
// leaving every other byte of the file -- its comments, its formatting, and any
// top-level keys this build's Config struct does not model -- untouched. It is
// the write-back primitive the TUI's mid-flow domain-capture prompt (kata w131)
// calls after the user types a domain during a `P` publish.
//
// Unlike Save, it deliberately does NOT re-encode the in-memory Config. A
// struct re-encode would DROP any unknown top-level key on disk (a field a
// newer/foreign tailport wrote, or a hand `$EDITOR` addition) and would
// stale-overwrite a concurrent edit -- e.g. a second tailport instance changing
// a port label/marker between this Config's Load and this call. That
// last-writer-wins clobber is exactly the config-clobber history the ycv1
// design (OQ3) set out to avoid. Instead this method RE-READS the file's
// current bytes, parses them into a yaml.Node tree, mutates only the
// caddy.domain scalar in that tree (inserting the caddy block and/or the domain
// key if a hand-minimal file lacks them, then re-applying the caddy
// head-comments via applyCaddyComments so a freshly inserted domain still
// documents itself -- matching Save's self-documenting caddy-block invariant),
// and re-marshals. Everything else already on disk therefore survives verbatim.
//
// Before overwriting, it copies the file's CURRENT on-disk bytes to
// <resolved-target>.bak at mode 0600 (via the same atomic 0600 write path as
// the main file): the config can hold a bcrypt auth_hash, and a world-readable
// backup would defeat the 0600 protection the main write maintains. If no config
// file exists yet there is nothing to back up -- it seeds a fresh Default() with
// the domain set and writes that, with no .bak. The overwrite reuses the
// symlink-safe atomic temp-file+rename (writeConfigAtomic), so on any failure
// the existing config is left byte-for-byte intact (the temp is removed).
//
// The target and its .bak resolve exactly as Save's write does: the Config's
// resolved path (c.path) if set, else Path(""), run through resolveSaveTarget so
// a symlinked config is written THROUGH to its real target and the .bak lands
// next to that real target.
//
// It persists to DISK ONLY. The caller must update its own in-memory
// cfg.Caddy.Domain after a successful return -- this method intentionally does
// not read the file's (possibly newer) values for the OTHER caddy/port fields
// back into the struct, so it cannot and must not mutate the caller's struct
// view of fields it did not touch. No domain validation happens here: this is a
// pure persistence primitive, and the caller (w131) validates the domain string
// before calling.
func (c Config) SaveCaddyDomain(domain string) error {
	path := c.path
	if path == "" {
		var err error
		path, err = Path("")
		if err != nil {
			return err
		}
	}
	target, err := resolveSaveTarget(path)
	if err != nil {
		return err
	}

	current, readErr := os.ReadFile(target)
	if os.IsNotExist(readErr) {
		// Nothing on disk to merge or back up: seed a fresh default with the
		// domain set, matching what a first Save would write (defaults +
		// caddy comments), and skip the .bak.
		cfg := Default()
		cfg.Caddy.Domain = domain
		var root yaml.Node
		if err := root.Encode(cfg); err != nil {
			return err
		}
		applyCaddyComments(&root)
		data, err := yaml.Marshal(&root)
		if err != nil {
			return err
		}
		return writeConfigAtomic(target, data)
	}
	if readErr != nil {
		return readErr
	}

	// Merge path: parse the current file into a Node tree and set only
	// caddy.domain, so comments and unknown/foreign keys are preserved.
	var root yaml.Node
	if err := yaml.Unmarshal(current, &root); err != nil {
		return err
	}
	setCaddyDomainNode(&root, domain)
	data, err := yaml.Marshal(&root)
	if err != nil {
		return err
	}

	// Back up the CURRENT on-disk bytes (0600) BEFORE overwriting, so a
	// failure or a bad merge is recoverable and the backup never leaks a hash.
	if err := writeConfigAtomic(target+".bak", current); err != nil {
		return err
	}
	return writeConfigAtomic(target, data)
}

// setCaddyDomainNode mutates a parsed config Node tree in place so that
// caddy.domain equals domain, touching nothing else. doc is what
// yaml.Unmarshal produced (a DocumentNode, or a bare node for an empty file).
// If the caddy mapping and/or the domain key are absent (a hand-minimal file),
// they are inserted so the value lands. It then re-applies the caddy
// head-comments (applyCaddyComments) so a freshly inserted domain key still
// carries its explanatory comment, exactly as Save keeps the caddy block
// self-documenting on every write.
func setCaddyDomainNode(doc *yaml.Node, domain string) {
	root := documentRootMapping(doc)
	caddy := mappingValueNode(root, "caddy")
	if caddy == nil {
		caddy = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "caddy"},
			caddy,
		)
	}
	if v := mappingValueNode(caddy, "domain"); v != nil {
		// Update in place; reset Style so a real domain renders plain
		// (domain: apps.example.com) the way an encoded struct would, rather
		// than inheriting a quoted style from a prior `domain: ""`.
		v.Kind = yaml.ScalarNode
		v.Tag = "!!str"
		v.Value = domain
		v.Style = 0
	} else {
		caddy.Content = append(caddy.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "domain"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: domain},
		)
	}
	applyCaddyComments(root)
}

// documentRootMapping returns the mapping node setCaddyDomainNode should mutate:
// the content of a DocumentNode (the normal yaml.Unmarshal shape), a bare
// MappingNode as-is, or -- for an empty/whitespace-only file that parsed to a
// null/zero node -- a freshly initialized empty mapping installed as the tree's
// content so the caddy block has somewhere to land.
func documentRootMapping(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
			m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			doc.Content = []*yaml.Node{m}
			return m
		}
		return doc.Content[0]
	}
	if doc.Kind != yaml.MappingNode {
		doc.Kind = yaml.MappingNode
		doc.Tag = "!!map"
		doc.Content = nil
	}
	return doc
}

// maxSymlinkHops bounds the manual chain-walk in resolveSaveTarget's
// dangling-symlink fallback, matching typical kernel symlink-loop limits. It
// exists so a genuine loop (a -> b -> a) errors out instead of looping
// forever or (worse) getting silently truncated at an arbitrary intermediate
// link.
const maxSymlinkHops = 255

// resolveSaveTarget returns the path Save should actually write to. For a
// plain file or a not-yet-existing path, that's path itself -- preserving
// Save's pre-3f5t behavior exactly. If path is a symlink, it resolves to the
// link's real target so the atomic rename lands there instead of replacing
// the symlink with a regular file (see the symlink paragraph on Save's doc
// comment).
func resolveSaveTarget(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return path, nil
		}
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}
	if target, err := filepath.EvalSymlinks(path); err == nil {
		return target, nil
	}
	// EvalSymlinks failed -- most likely a dangling symlink chain (the
	// eventual target doesn't exist yet, e.g. a fresh dotfiles checkout) or
	// a symlink loop. Walk the chain by hand, one hop at a time, stopping at
	// the first component that either doesn't exist (that's the intended
	// save target -- and, for a multi-hop chain like
	// config.yaml -> second-link -> missing.yaml, that's missing.yaml, not
	// second-link) or isn't a symlink (a real file/dir to write through).
	// roborev kg6f: a single-hop fallback here renamed over an intermediate
	// symlink instead of resolving through it, and replaced a symlink loop
	// with a plain file instead of erroring.
	current := path
	for hop := 0; hop < maxSymlinkHops; hop++ {
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				return current, nil
			}
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return current, nil
		}
		link, err := os.Readlink(current)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(link) {
			link = filepath.Join(filepath.Dir(current), link)
		}
		current = link
	}
	return "", fmt.Errorf("resolveSaveTarget: too many levels of symbolic links: %s", path)
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
