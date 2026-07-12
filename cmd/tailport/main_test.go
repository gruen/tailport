package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/gruen/tailport/internal/ui"
)

// TestVersionLine covers the --version output (jtpx). The default build stamps
// "dev"; a release stamps the pkgver via -ldflags -X main.version, and whatever
// that value is must appear verbatim in the line so `tailport --version`
// reports the packaged version.
func TestVersionLine(t *testing.T) {
	if got := versionLine(); got != "tailport "+version {
		t.Errorf("versionLine() = %q, want %q", got, "tailport "+version)
	}
	if !strings.HasPrefix(versionLine(), "tailport ") {
		t.Errorf("versionLine() = %q, want it to start with 'tailport '", versionLine())
	}

	// Simulate a release-time injection: whatever main.version is set to must
	// surface in the printed line.
	orig := version
	t.Cleanup(func() { version = orig })
	version = "0.1.1"
	if got := versionLine(); got != "tailport 0.1.1" {
		t.Errorf("with injected version, versionLine() = %q, want %q", got, "tailport 0.1.1")
	}
}

// TestRunVersionFlag covers --version/-v via the run() entry point (rather
// than just versionLine() in isolation): it should print to stdout and exit
// 0, without touching stderr or config.
func TestRunVersionFlag(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"-v"}} {
		var out, errOut bytes.Buffer
		code := run(args, &out, &errOut)
		if code != 0 {
			t.Errorf("run(%v) code = %d, want 0", args, code)
		}
		if got := out.String(); got != versionLine()+"\n" {
			t.Errorf("run(%v) stdout = %q, want %q", args, got, versionLine()+"\n")
		}
		if errOut.Len() != 0 {
			t.Errorf("run(%v) stderr = %q, want empty", args, errOut.String())
		}
	}
}

// TestRunHelpFlag covers 5dgj's acceptance bar: -h and --help print
// tailport's custom usage to stdout and the caller should exit 0 (never
// stdlib's bare flag listing, never stderr, never exit 2).
func TestRunHelpFlag(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--help"}} {
		var out, errOut bytes.Buffer
		code := run(args, &out, &errOut)
		if code != 0 {
			t.Errorf("run(%v) code = %d, want 0", args, code)
		}
		if errOut.Len() != 0 {
			t.Errorf("run(%v) stderr = %q, want empty", args, errOut.String())
		}
		got := out.String()
		for _, want := range []string{"tailport --", "Usage:", "--config", "--no-color", "--markers", "quickstart", "status", "update", "Config path:"} {
			if !strings.Contains(got, want) {
				t.Errorf("run(%v) stdout missing %q; got:\n%s", args, want, got)
			}
		}
		// The keybinding legend belongs to `quickstart`, not --help (5dgj).
		if strings.Contains(got, "space") || strings.Contains(got, "Toggle tailscale serve") {
			t.Errorf("run(%v) stdout should not include the TUI keybinding legend; got:\n%s", args, got)
		}
	}
}

// TestRunUnknownFlag covers 5dgj's other half: a bad flag prints usage to
// stderr (not stdout) and the caller should exit 2.
func TestRunUnknownFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"--bogus"}, &out, &errOut)
	if code != 2 {
		t.Errorf("run([--bogus]) code = %d, want 2", code)
	}
	if out.Len() != 0 {
		t.Errorf("run([--bogus]) stdout = %q, want empty", out.String())
	}
	got := errOut.String()
	if !strings.Contains(got, "bogus") {
		t.Errorf("run([--bogus]) stderr should mention the bad flag; got:\n%s", got)
	}
	if !strings.Contains(got, "Usage:") {
		t.Errorf("run([--bogus]) stderr should include usage; got:\n%s", got)
	}
}

// TestRunUnknownSubcommand covers the dispatch scaffold's default case: an
// unrecognized first argument (not a flag) prints "unknown subcommand" plus
// usage to stderr and exits 2.
func TestRunUnknownSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"frobnicate"}, &out, &errOut)
	if code != 2 {
		t.Errorf("run([frobnicate]) code = %d, want 2", code)
	}
	if out.Len() != 0 {
		t.Errorf("run([frobnicate]) stdout = %q, want empty", out.String())
	}
	got := errOut.String()
	if !strings.Contains(got, `unknown subcommand "frobnicate"`) {
		t.Errorf("run([frobnicate]) stderr = %q, want it to name the bad subcommand", got)
	}
	if !strings.Contains(got, "Usage:") {
		t.Errorf("run([frobnicate]) stderr should include usage; got:\n%s", got)
	}
}

// TestRunReservedSubcommands covers the two reserved-but-not-yet-implemented
// subcommand names left in the dispatch scaffold (quickstart, kata x4cg, is
// implemented -- see TestRunQuickstart below): each is recognized (distinct
// from an unknown subcommand -- no "unknown subcommand" message, no usage
// dump) but reports plainly that it isn't implemented yet, on stderr, with a
// non-zero exit.
func TestRunReservedSubcommands(t *testing.T) {
	for _, name := range []string{"status", "update"} {
		var out, errOut bytes.Buffer
		code := run([]string{name}, &out, &errOut)
		if code == 0 {
			t.Errorf("run([%s]) code = 0, want non-zero (not implemented)", name)
		}
		if out.Len() != 0 {
			t.Errorf("run([%s]) stdout = %q, want empty", name, out.String())
		}
		got := errOut.String()
		if !strings.Contains(got, name) || !strings.Contains(got, "not implemented") {
			t.Errorf("run([%s]) stderr = %q, want it to name %q and say not implemented", name, got, name)
		}
		if strings.Contains(got, "unknown subcommand") {
			t.Errorf("run([%s]) should be a recognized (reserved) name, not an unknown subcommand; got:\n%s", name, got)
		}
	}
}

// TestRunQuickstart covers kata x4cg's acceptance bar: `tailport quickstart`
// prints its onboarding text (what tailport does, its safety model, the
// resolved config path, the full keybinding legend) to stdout, touches
// stderr not at all, and exits 0 -- no TUI, no side effects (it must not
// create a config file that didn't already exist: quickstart only reads).
func TestRunQuickstart(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	var out, errOut bytes.Buffer
	code := run([]string{"quickstart", "--markers", "ascii"}, &out, &errOut)
	if code != 0 {
		t.Errorf("run([quickstart]) code = %d, want 0; stderr:\n%s", code, errOut.String())
	}
	if errOut.Len() != 0 {
		t.Errorf("run([quickstart]) stderr = %q, want empty", errOut.String())
	}

	got := out.String()
	for _, want := range []string{
		// What tailport does.
		"tailport exposes", "tailscale serve",
		// Safety model wording (AGENTS.md's design constraints).
		"tailnet-only", "tailscale funnel", "public", "deliberate",
		"p` key", "y/n confirm", ":22", "hard-blocked",
		// Resolved config path.
		"Config path:", filepath.Join(xdg, "tailport", "config.yaml"),
		// The keybinding legend (spot-check a few rows).
		"space", "Toggle tailscale serve", "Quit.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("run([quickstart]) stdout missing %q; got:\n%s", want, got)
		}
	}

	if _, err := os.Stat(filepath.Join(xdg, "tailport", "config.yaml")); !os.IsNotExist(err) {
		t.Errorf("run([quickstart]) must not create a config file as a side effect; os.Stat err = %v", err)
	}
}

// TestRunQuickstartLegendMatchesOverlay proves the SINGLE SOURCE OF TRUTH
// requirement for kata x4cg directly: `tailport quickstart`'s printed
// keybinding legend is byte-identical to ui.RenderKeyLegend(ui.KeyLegendRows(...)),
// the exact same call the in-TUI "?" overlay (internal/ui.helpView) makes.
// Run with both --markers ascii and --markers emoji so the glyph-dependent
// rows (space/p/C) are checked in both marker modes, not just the default.
func TestRunQuickstartLegendMatchesOverlay(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	for _, tc := range []struct {
		markers string
		emoji   bool
	}{
		{"ascii", false},
		{"emoji", true},
	} {
		var out, errOut bytes.Buffer
		code := run([]string{"quickstart", "--markers", tc.markers}, &out, &errOut)
		if code != 0 {
			t.Fatalf("run([quickstart --markers %s]) code = %d, want 0; stderr:\n%s", tc.markers, code, errOut.String())
		}

		want := ui.RenderKeyLegend(ui.KeyLegendRows(tc.emoji))
		if !strings.Contains(out.String(), want) {
			t.Errorf("run([quickstart --markers %s]) stdout does not contain the shared legend verbatim.\nwant substring:\n%s\ngot:\n%s", tc.markers, want, out.String())
		}
	}
}

// TestValidateMarkers covers zn2x's validation directly: only "" (not
// passed), auto, emoji, and ascii (case-insensitively, trimmed) are
// accepted; anything else is a clean error, never a panic.
func TestValidateMarkers(t *testing.T) {
	// "  " (whitespace-only) is deliberately in the accepted group: it trims
	// to "", and both validateMarkers and internal/ui.resolveEmoji treat ""
	// the same way ("not meaningfully set" -> auto-detect), so the two stay
	// consistent about what counts as "auto".
	for _, v := range []string{"", "  ", "auto", "AUTO", " ascii ", "ASCII", "emoji", "Emoji"} {
		if err := validateMarkers(v); err != nil {
			t.Errorf("validateMarkers(%q) = %v, want nil", v, err)
		}
	}
	for _, v := range []string{"bogus", "emojis", "ASCII!"} {
		if err := validateMarkers(v); err == nil {
			t.Errorf("validateMarkers(%q) = nil, want an error", v)
		}
	}
}

// TestRunBadMarkersFlag covers zn2x's CLI-level acceptance bar: an invalid
// --markers value fails cleanly through run() -- a one-line stderr message,
// non-zero exit, and (implicitly, since this test would panic-fail
// otherwise) no stack trace -- and importantly returns before ever touching
// config or the TUI.
func TestRunBadMarkersFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"--markers", "bogus"}, &out, &errOut)
	if code == 0 {
		t.Errorf("run([--markers bogus]) code = 0, want non-zero")
	}
	if out.Len() != 0 {
		t.Errorf("run([--markers bogus]) stdout = %q, want empty", out.String())
	}
	got := errOut.String()
	if !strings.Contains(got, "invalid --markers") || !strings.Contains(got, "bogus") {
		t.Errorf("run([--markers bogus]) stderr = %q, want a clean invalid-markers message", got)
	}
	if strings.Count(got, "\n") > 1 {
		t.Errorf("run([--markers bogus]) stderr should be a single line, got %d lines:\n%s", strings.Count(got, "\n"), got)
	}
}

// TestApplyNoColorForcesAsciiProfile covers 63c6's mechanism directly:
// applyNoColor(true) (as when --no-color is passed, or NO_COLOR is set)
// forces lipgloss's shared *default* renderer to the Ascii profile. Every
// package-level style in internal/ui -- and any future non-interactive
// output built the same way (lipgloss.NewStyle(), no custom renderer) --
// renders through that same shared renderer, so this one call covers both
// the TUI and headless output alike; see internal/ui/ui.go's package-level
// style vars for the pattern this relies on.
//
// Note: this only proves the mechanism (the right API, the right constant);
// it can't prove an on/off transition here since go test's stdout isn't a
// real terminal, so termenv's own auto-detection may already resolve to
// Ascii regardless. The on/off differential is verified against the actual
// compiled binary in a real pty (see the kata close message for this issue).
//
// This also mutates process-global lipgloss state with no way to unset it
// (SetColorProfile has no inverse), so later tests in this file must not
// assume color is otherwise "on".
func TestApplyNoColorForcesAsciiProfile(t *testing.T) {
	applyNoColor(true)
	if got := lipgloss.ColorProfile(); got != termenv.Ascii {
		t.Errorf("after applyNoColor(true), lipgloss.ColorProfile() = %v, want termenv.Ascii", got)
	}
	styled := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true).Render("x")
	if strings.Contains(styled, "\x1b[") {
		t.Errorf("styled render under the forced no-color profile = %q, want no ANSI escapes", styled)
	}
}
