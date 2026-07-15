package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/gruen/tailport/internal/statusreport"
	"github.com/gruen/tailport/internal/tsserve"
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

// TestResolveVersion covers the `go install` version fix. `go install` applies
// no -ldflags, so main.version stays "dev" -- but Go embeds the module version
// in the binary, and that's the real answer. The risk being tested is the other
// direction: Go synthesizes Main.Version for builds with NO tag behind them
// too, and reporting one of those as a release would make a random working tree
// look like the shipped artifact.
func TestResolveVersion(t *testing.T) {
	bi := func(v string) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Version: v}}
	}
	for _, tc := range []struct {
		name    string
		stamped string
		bi      *debug.BuildInfo
		ok      bool
		want    string
	}{
		// The stamp is authoritative wherever it exists: releases, AUR, brew.
		{"ldflags stamp wins", "0.1.4", bi("v0.1.3"), true, "0.1.4"},
		{"stamp wins even over no build info", "0.1.4", nil, false, "0.1.4"},

		// The fix: `go install ...@latest` of a real tag.
		{"go install of a tag", devVersion, bi("v0.1.4"), true, "0.1.4"},
		{"go install of a prerelease tag", devVersion, bi("v0.2.0-rc1"), true, "0.2.0-rc1"},

		// Everything with no tag behind it must stay "dev".
		{"local go build (pseudo-version + dirty)", devVersion, bi("v0.1.4-0.20260713001843-f1c0508a5634+dirty"), true, devVersion},
		{"go install of a bare commit (pseudo-version)", devVersion, bi("v0.1.4-0.20260713001843-f1c0508a5634"), true, devVersion},
		{"devel placeholder", devVersion, bi("(devel)"), true, devVersion},
		{"empty module version", devVersion, bi(""), true, devVersion},
		{"build metadata", devVersion, bi("v0.1.4+incompatible"), true, devVersion},
		{"no build info at all", devVersion, nil, false, devVersion},
		{"not a semver", devVersion, bi("banana"), true, devVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveVersion(tc.stamped, tc.bi, tc.ok); got != tc.want {
				t.Errorf("resolveVersion(%q, %+v, %v) = %q, want %q", tc.stamped, tc.bi, tc.ok, got, tc.want)
			}
		})
	}
}

// TestResolveVersionReturnsBareSemver pins the convention both version paths
// must share: -ldflags stamps a BARE semver (build.yml passes
// ${GITHUB_REF_NAME#v}), so the module-info fallback has to strip the "v" too.
// If it didn't, `go install` builds would print "tailport v0.1.4" while release
// builds print "tailport 0.1.4", and the TUI header would read "vv0.1.4".
func TestResolveVersionReturnsBareSemver(t *testing.T) {
	got := resolveVersion(devVersion, &debug.BuildInfo{Main: debug.Module{Version: "v0.1.4"}}, true)
	if strings.HasPrefix(got, "v") {
		t.Errorf("resolveVersion returned %q, want a bare semver with no leading 'v'", got)
	}
	if got != "0.1.4" {
		t.Errorf("resolveVersion = %q, want %q", got, "0.1.4")
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
		for _, want := range []string{"tailport --", "Usage:", "--config", "--no-color", "--markers", "--theme", "quickstart", "status", "update", "Config path:"} {
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

// TestRunQuickstart covers kata x4cg's acceptance bar (evolved by tapv):
// `tailport quickstart` prints its onboarding text (what tailport does, a
// prerequisites note on tailscale's operator requirement, its safety
// model, the resolved config path, the full keybinding legend) to stdout,
// touches stderr not at all, and exits 0 -- no TUI, no side effects (it
// must not create a config file that didn't already exist: quickstart only
// reads).
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
		// Prerequisites: tailscale's own operator requirement (kata tapv).
		"Prerequisites:", "operator", "sudo tailscale set --operator=",
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
// requirement (kata x4cg, evolved by p39s) directly: `tailport quickstart`'s
// printed keybinding legend is byte-identical to
// ui.RenderKeyLegendGroups(ui.KeyLegendGroups(...)), the exact same grouped call
// the in-TUI "?" overlay (internal/ui.helpView) makes. Run with both --markers
// ascii and --markers emoji so the glyph-dependent rows (space/p/C) are checked
// in both marker modes, not just the default.
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

		want := ui.RenderKeyLegendGroups(ui.KeyLegendGroups(tc.emoji))
		if !strings.Contains(out.String(), want) {
			t.Errorf("run([quickstart --markers %s]) stdout does not contain the shared grouped legend verbatim.\nwant substring:\n%s\ngot:\n%s", tc.markers, want, out.String())
		}
	}
}

// TestRunQuickstartPrerequisitesMatchesOverlay is the prerequisites-note
// counterpart to TestRunQuickstartLegendMatchesOverlay (kata tapv):
// `tailport quickstart`'s printed prerequisites section is (line-for-line,
// modulo quickstart's two-space indent) the exact same
// ui.OperatorSetupText the in-TUI "?" overlay's "Setup / prerequisites"
// section renders, so the two can never drift apart. tsserve.CurrentUsername
// is called from the test itself (not hardcoded) since both quickstart and
// the test resolve it the same way on whatever machine runs this.
func TestRunQuickstartPrerequisitesMatchesOverlay(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	var out, errOut bytes.Buffer
	code := run([]string{"quickstart"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("run([quickstart]) code = %d, want 0; stderr:\n%s", code, errOut.String())
	}

	want := ui.OperatorSetupText(tsserve.CurrentUsername())
	got := out.String()
	for _, line := range strings.Split(want, "\n") {
		if !strings.Contains(got, "  "+line) {
			t.Errorf("run([quickstart]) stdout missing prerequisites line %q (from the shared ui.OperatorSetupText); got:\n%s", line, got)
		}
	}
}

// TestValidateMarkers covers zn2x's validation directly: only "" (not
// passed), auto, emoji, and ascii (case-insensitively, trimmed) are
// accepted; anything else is a clean error, never a panic.
func TestValidateMarkers(t *testing.T) {
	// "  " (whitespace-only) is deliberately in the accepted group: it trims
	// to "", and both validateMarkers and internal/ui.resolveMarkerEmoji treat
	// "" the same way ("not meaningfully set" -> mono default, qwcw), so the
	// two stay consistent about what counts as unset.
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

// TestValidateTheme covers n7gc's validation directly: only "" (not
// passed), auto, light, and dark (case-insensitively, trimmed) are
// accepted; anything else is a clean error, never a panic. Mirrors
// TestValidateMarkers exactly.
func TestValidateTheme(t *testing.T) {
	for _, v := range []string{"", "  ", "auto", "AUTO", " light ", "LIGHT", "dark", "Dark"} {
		if err := validateTheme(v); err != nil {
			t.Errorf("validateTheme(%q) = %v, want nil", v, err)
		}
	}
	for _, v := range []string{"bogus", "lightish", "DARK!"} {
		if err := validateTheme(v); err == nil {
			t.Errorf("validateTheme(%q) = nil, want an error", v)
		}
	}
}

// TestRunBadThemeFlag covers n7gc's CLI-level acceptance bar: an invalid
// --theme value fails cleanly through run() -- a one-line stderr message,
// non-zero exit, and (implicitly, since this test would panic-fail
// otherwise) no stack trace -- and importantly returns before ever touching
// config, lipgloss's background override, or the TUI. Mirrors
// TestRunBadMarkersFlag exactly.
func TestRunBadThemeFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"--theme", "bogus"}, &out, &errOut)
	if code == 0 {
		t.Errorf("run([--theme bogus]) code = 0, want non-zero")
	}
	if out.Len() != 0 {
		t.Errorf("run([--theme bogus]) stdout = %q, want empty", out.String())
	}
	got := errOut.String()
	if !strings.Contains(got, "invalid --theme") || !strings.Contains(got, "bogus") {
		t.Errorf("run([--theme bogus]) stderr = %q, want a clean invalid-theme message", got)
	}
	if strings.Count(got, "\n") > 1 {
		t.Errorf("run([--theme bogus]) stderr should be a single line, got %d lines:\n%s", strings.Count(got, "\n"), got)
	}
}

// TestRunBadThemeFlagQuickstart covers the same bar for `tailport
// quickstart --theme bogus`, since runQuickstart validates --theme
// independently of run() (quickstart never reaches the default TUI path).
func TestRunBadThemeFlagQuickstart(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"quickstart", "--theme", "bogus"}, &out, &errOut)
	if code == 0 {
		t.Errorf("run([quickstart --theme bogus]) code = 0, want non-zero")
	}
	if out.Len() != 0 {
		t.Errorf("run([quickstart --theme bogus]) stdout = %q, want empty", out.String())
	}
	if !strings.Contains(errOut.String(), "invalid --theme") {
		t.Errorf("run([quickstart --theme bogus]) stderr = %q, want a clean invalid-theme message", errOut.String())
	}
}

// TestResolveThemeMode covers n7gc's precedence contract directly: an
// explicit --theme flag value always wins; otherwise the persisted
// cfg.Theme; otherwise "" (auto). This is the exact function run() and
// runQuickstart() both call before ui.ApplyTheme, so exercising it here
// verifies real precedence, not a reimplementation of it.
func TestResolveThemeMode(t *testing.T) {
	cases := []struct {
		name            string
		flagVal, cfgVal string
		want            string
	}{
		{"flag wins over config", "light", "dark", "light"},
		{"flag wins with no config", "dark", "", "dark"},
		{"falls back to config when flag unset", "", "light", "light"},
		{"both unset -> auto (empty)", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveThemeMode(tc.flagVal, tc.cfgVal); got != tc.want {
				t.Errorf("resolveThemeMode(%q, %q) = %q, want %q", tc.flagVal, tc.cfgVal, got, tc.want)
			}
		})
	}
}

// TestRunGoodThemeFlagAcceptedByParsing confirms --theme light/dark/auto
// parse cleanly into cf.theme (the flag itself is accepted, distinct from
// validateTheme's acceptance of the same strings tested above) without
// going through run()'s full default path, which would launch the
// interactive TUI (tea.NewProgram(...).Run()) and hang under `go test`.
func TestRunGoodThemeFlagAcceptedByParsing(t *testing.T) {
	for _, v := range []string{"auto", "light", "dark", ""} {
		var out, errOut bytes.Buffer
		cf, code, handled := parseFlags([]string{"--theme", v}, &out, &errOut)
		if handled {
			t.Fatalf("parseFlags([--theme %q]) unexpectedly handled (code=%d, stderr=%q)", v, code, errOut.String())
		}
		if cf.theme != v {
			t.Errorf("parseFlags([--theme %q]).theme = %q, want %q", v, cf.theme, v)
		}
		if err := validateTheme(cf.theme); err != nil {
			t.Errorf("validateTheme(%q) (from --theme) = %v, want nil", v, err)
		}
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

// stubStatusGather swaps statusGather for a fixed fake result and restores
// the original (statusreport.Gather, which shells to live tailscaled/ss/
// lsof) via t.Cleanup. This is the seam that lets TestRunStatus* exercise
// runStatus's flag parsing, JSON/table output, and exit codes without a live
// tailscaled -- see statusGather's doc comment in main.go.
func stubStatusGather(t *testing.T, rows []statusreport.Row, err error) {
	t.Helper()
	orig := statusGather
	t.Cleanup(func() { statusGather = orig })
	statusGather = func() ([]statusreport.Row, error) { return rows, err }
}

// fakeStatusRows is a fixed two-row fixture shared by the TestRunStatus*
// cases below: one tailnet-served port, one funnelled (public) port.
func fakeStatusRows() []statusreport.Row {
	return []statusreport.Row{
		{Port: 3000, Process: "node", Mode: statusreport.ModeServe, URL: "http://host-a:3000"},
		{Port: 8080, Process: "python3", Mode: statusreport.ModeFunnel, URL: "https://host-a.tailnet.ts.net:8443"},
	}
}

// TestRunStatusTable covers m7jc's default (human-readable) output: both an
// exposed serve port and an exposed funnel port appear, the funnel row is
// visually distinct (upper-case "FUNNEL (public)", never confusable with
// "serve" even with color disabled by the test environment), exit is 0, and
// stderr stays empty.
func TestRunStatusTable(t *testing.T) {
	stubStatusGather(t, fakeStatusRows(), nil)

	var out, errOut bytes.Buffer
	code := run([]string{"status"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("run([status]) code = %d, want 0", code)
	}
	if errOut.Len() != 0 {
		t.Errorf("run([status]) stderr = %q, want empty", errOut.String())
	}
	got := out.String()
	for _, want := range []string{"MODE", "PORT", "PROCESS", "URL", "3000", "node", "http://host-a:3000", "8080", "python3", "FUNNEL (public)", "https://host-a.tailnet.ts.net:8443", "serve (tailnet)"} {
		if !strings.Contains(got, want) {
			t.Errorf("run([status]) stdout missing %q; got:\n%s", want, got)
		}
	}
}

// TestRunStatusJSON covers --json: valid, parseable JSON matching
// statusreport's stable Document schema, exit 0, empty stderr.
func TestRunStatusJSON(t *testing.T) {
	stubStatusGather(t, fakeStatusRows(), nil)

	var out, errOut bytes.Buffer
	code := run([]string{"status", "--json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("run([status --json]) code = %d, want 0", code)
	}
	if errOut.Len() != 0 {
		t.Errorf("run([status --json]) stderr = %q, want empty", errOut.String())
	}

	var doc statusreport.Document
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("run([status --json]) stdout did not parse as JSON: %v; got:\n%s", err, out.String())
	}
	if len(doc.Ports) != 2 {
		t.Fatalf("doc.Ports has %d entries, want 2; got %+v", len(doc.Ports), doc.Ports)
	}
	want := fakeStatusRows()
	for i, row := range doc.Ports {
		if row != want[i] {
			t.Errorf("doc.Ports[%d] = %+v, want %+v", i, row, want[i])
		}
	}
}

// TestRunStatusEmptyJSONIsEmptyArrayNotNull covers WriteJSON's documented
// contract at the CLI level: no exposed ports still emits a "ports" array,
// [], never a JSON null, so a consumer never needs a null check.
func TestRunStatusEmptyJSONIsEmptyArrayNotNull(t *testing.T) {
	stubStatusGather(t, nil, nil)

	var out, errOut bytes.Buffer
	code := run([]string{"status", "--json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("run([status --json]) code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), `"ports": []`) {
		t.Errorf("run([status --json]) stdout = %q, want a \"ports\": [] array", out.String())
	}
}

// TestRunStatusEmptyTable covers the human-readable empty case: a plain
// sentence, not an empty/blank table.
func TestRunStatusEmptyTable(t *testing.T) {
	stubStatusGather(t, nil, nil)

	var out, errOut bytes.Buffer
	code := run([]string{"status"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("run([status]) code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "No ports are currently served or funnelled") {
		t.Errorf("run([status]) stdout = %q, want a no-ports-served message", out.String())
	}
}

// TestRunStatusGatherError covers a failed read (e.g. tailscaled
// unreachable): a clean one-line-ish stderr message, exit 1, and -- since
// this is a READ-ONLY report -- nothing on stdout.
func TestRunStatusGatherError(t *testing.T) {
	stubStatusGather(t, nil, errors.New("reading tailscale serve status: exit status 1"))

	var out, errOut bytes.Buffer
	code := run([]string{"status"}, &out, &errOut)
	if code != 1 {
		t.Errorf("run([status]) code = %d, want 1", code)
	}
	if out.Len() != 0 {
		t.Errorf("run([status]) stdout = %q, want empty", out.String())
	}
	if !strings.Contains(errOut.String(), "reading tailscale serve status") {
		t.Errorf("run([status]) stderr = %q, want it to surface the gather error", errOut.String())
	}
}

// TestRunStatusHelp covers -h/--help for the status subcommand: usage on
// stdout, exit 0, empty stderr -- and never touches statusGather (a bogus
// gather that would panic on GatherError proves --help returns before any
// data read).
func TestRunStatusHelp(t *testing.T) {
	orig := statusGather
	t.Cleanup(func() { statusGather = orig })
	statusGather = func() ([]statusreport.Row, error) {
		t.Fatal("statusGather should not be called for --help")
		return nil, nil
	}

	for _, args := range [][]string{{"status", "-h"}, {"status", "--help"}} {
		var out, errOut bytes.Buffer
		code := run(args, &out, &errOut)
		if code != 0 {
			t.Errorf("run(%v) code = %d, want 0", args, code)
		}
		if errOut.Len() != 0 {
			t.Errorf("run(%v) stderr = %q, want empty", args, errOut.String())
		}
		got := out.String()
		for _, want := range []string{"tailport status", "Usage:", "--json", "--no-color", "READ-ONLY"} {
			if !strings.Contains(got, want) {
				t.Errorf("run(%v) stdout missing %q; got:\n%s", args, want, got)
			}
		}
	}
}

// TestRunStatusBadFlag covers an unknown flag: usage/error to stderr, empty
// stdout, exit 2 -- mirroring TestRunUnknownFlag's contract for the
// top-level flag set.
func TestRunStatusBadFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"status", "--bogus"}, &out, &errOut)
	if code != 2 {
		t.Errorf("run([status --bogus]) code = %d, want 2", code)
	}
	if out.Len() != 0 {
		t.Errorf("run([status --bogus]) stdout = %q, want empty", out.String())
	}
	got := errOut.String()
	if !strings.Contains(got, "bogus") || !strings.Contains(got, "Usage:") {
		t.Errorf("run([status --bogus]) stderr = %q, want it to mention the bad flag and usage", got)
	}
}
