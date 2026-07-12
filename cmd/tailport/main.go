// Command tailport is a TUI for toggling tailscale serve (tailnet-only,
// plain HTTP) on and off per locally listening port.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/gruen/tailport/internal/config"
	"github.com/gruen/tailport/internal/statusreport"
	"github.com/gruen/tailport/internal/ui"
)

// version is the build version, injected at release time via the linker:
// -ldflags "-X main.version=<pkgver>" (see .github/workflows/build.yml and the
// AUR PKGBUILDs). It stays "dev" for plain `go build`/`go run` so an
// unversioned local build is self-evident.
var version = "dev"

// versionLine is the string printed by --version. Kept tiny and pure so it can
// be asserted in a test without standing up the whole TUI.
func versionLine() string {
	return "tailport " + version
}

// cliFlags holds tailport's global flags: the set shared by the default
// (TUI) path and, eventually, every subcommand, so --config/--no-color/etc.
// behave identically everywhere they're accepted.
type cliFlags struct {
	showVersion bool
	configPath  string // -c/--config override; "" means "resolve normally" (y4gt)
	noColor     bool   // --no-color (63c6); NO_COLOR env is handled separately, see applyNoColor
	markers     string // --markers value, unvalidated; "" means "not passed" (zn2x)
	theme       string // --theme value, unvalidated; "" means "not passed" (n7gc)
}

// newFlagSet builds tailport's shared flag.FlagSet: the same flags, in the
// same shape, for the default TUI path and every future subcommand. It uses
// ContinueOnError and discards the flag package's own error/usage output so
// parseFlags has full control over where usage text goes and what exit code
// follows (5dgj) -- stdlib's ExitOnError default conflates "-h" and "bad
// flag" onto the same stream/exit-code pair, which isn't what we want here.
func newFlagSet(name string) (*flag.FlagSet, *cliFlags) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {} // parseFlags prints usage itself; see below.

	cf := &cliFlags{}
	fs.BoolVar(&cf.showVersion, "version", false, "print version and exit")
	fs.BoolVar(&cf.showVersion, "v", false, "print version and exit (shorthand)")
	fs.StringVar(&cf.configPath, "config", "", "config file path (default: $XDG_CONFIG_HOME/tailport/config.yaml, else ~/.config/tailport/config.yaml)")
	fs.StringVar(&cf.configPath, "c", "", "shorthand for --config")
	fs.BoolVar(&cf.noColor, "no-color", false, "disable ANSI color output (also honors NO_COLOR)")
	fs.StringVar(&cf.markers, "markers", "", "exposure-glyph style override: auto, emoji, or ascii")
	fs.StringVar(&cf.theme, "theme", "", "color-scheme override: auto, light, or dark")
	return fs, cf
}

// parseFlags parses args with tailport's shared flag set and applies 5dgj's
// usage/exit-code contract: -h/--help prints the custom usage to stdout and
// the caller should exit 0; any other parse error (bad/unknown flag, a flag
// missing its value, ...) prints the error plus usage to stderr and the
// caller should exit 2. handled reports whether one of those two terminal
// cases fired, in which case code is the exit code to use and cf should be
// ignored.
func parseFlags(args []string, stdout, stderr io.Writer) (cf *cliFlags, code int, handled bool) {
	fs, cf := newFlagSet("tailport")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(stdout, cf.configPath)
			return cf, 0, true
		}
		fmt.Fprintln(stderr, "tailport:", err)
		printUsage(stderr, cf.configPath)
		return cf, 2, true
	}
	return cf, 0, false
}

// printUsage writes tailport's help text: a one-line description, the usage
// synopsis, the flag list, the subcommands (quickstart/status/update -- most
// still extension points, see runSubcommand), and the resolved config path
// (reusing config.Path's own resolution, honoring configOverride if one is
// already known). It deliberately excludes the in-TUI keybinding legend --
// that belongs to `tailport quickstart` (kata x4cg), not --help.
func printUsage(w io.Writer, configOverride string) {
	fmt.Fprintln(w, "tailport -- expose locally listening ports across your tailnet via")
	fmt.Fprintln(w, "`tailscale serve` (tailnet-only, the default) or, as a deliberate")
	fmt.Fprintln(w, "per-port opt-in, publicly via `tailscale funnel`.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  tailport [flags]              launch the interactive TUI")
	fmt.Fprintln(w, "  tailport <command> [flags]    run a subcommand")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  -v, --version         print version and exit")
	fmt.Fprintln(w, "  -c, --config <path>   config file path (default: $XDG_CONFIG_HOME/tailport/config.yaml, else ~/.config/tailport/config.yaml)")
	fmt.Fprintln(w, "      --no-color        disable ANSI color output (also honors NO_COLOR)")
	fmt.Fprintln(w, "      --markers <mode>  exposure-glyph style: auto, emoji, or ascii (default: auto)")
	fmt.Fprintln(w, "      --theme <mode>    color scheme: auto, light, or dark (default: auto)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  quickstart   non-interactive onboarding and keybinding legend")
	fmt.Fprintln(w, "  status       headless, read-only report of current port exposure")
	fmt.Fprintln(w, "  update       self-update tailport to the latest release")
	fmt.Fprintln(w)
	path, err := config.Path(configOverride)
	if err != nil {
		fmt.Fprintf(w, "Config path: <unresolved: %v>\n", err)
		return
	}
	fmt.Fprintln(w, "Config path:", path)
}

// validMarkersModes are the only values --markers (zn2x) accepts, matching
// what internal/ui.resolveEmoji understands for cfg.Markers: "auto" (or
// empty) defers to terminal auto-detection, "emoji"/"ascii" force a set.
var validMarkersModes = map[string]bool{"": true, "auto": true, "emoji": true, "ascii": true}

// validateMarkers rejects any --markers value other than "" (not passed),
// "auto", "emoji", or "ascii" (case-insensitive, trimmed), returning a clean
// error for the caller to report on one stderr line -- never a panic/stack
// trace.
func validateMarkers(v string) error {
	if validMarkersModes[strings.ToLower(strings.TrimSpace(v))] {
		return nil
	}
	return fmt.Errorf("invalid --markers %q: want one of auto, emoji, ascii", v)
}

// validThemeModes are the only values --theme/theme (kata n7gc) accept,
// matching what internal/ui.resolveTheme understands: "auto" (or empty)
// defers to lipgloss/termenv's own background detection, "light"/"dark"
// force it. Mirrors validMarkersModes exactly.
var validThemeModes = map[string]bool{"": true, "auto": true, "light": true, "dark": true}

// validateTheme rejects any --theme value other than "" (not passed),
// "auto", "light", or "dark" (case-insensitive, trimmed), returning a clean
// error for the caller to report on one stderr line -- never a panic/stack
// trace. Mirrors validateMarkers exactly.
func validateTheme(v string) error {
	if validThemeModes[strings.ToLower(strings.TrimSpace(v))] {
		return nil
	}
	return fmt.Errorf("invalid --theme %q: want one of auto, light, dark", v)
}

// resolveThemeMode applies n7gc's theme precedence: an explicit --theme flag
// value wins outright; otherwise the persisted cfg.Theme; otherwise "" (which
// internal/ui.ApplyTheme/resolveTheme treats as "auto" -- leave lipgloss's
// own detection alone). Mirrors the inline markersMode precedence used for
// --markers (see runQuickstart, and ui.New's doc comment) but factored into
// a named, directly-testable function: unlike markers, the resolved theme
// isn't threaded through ui.New's return value -- it's applied via a
// package-level side effect (ui.ApplyTheme, called once at startup below),
// so the precedence itself needs a seam a test can call without also
// launching the TUI.
func resolveThemeMode(flagVal, cfgVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return cfgVal
}

// applyNoColor forces lipgloss/termenv to the Ascii (no-color) profile when
// flagSet is true or NO_COLOR (https://no-color.org) is set to a non-empty
// value (63c6). It mutates lipgloss's shared default renderer, which every
// package-level style in internal/ui (and any future non-interactive output
// built the same way) renders through, so this single call covers both the
// TUI and any headless subcommand output. Safe to call more than once.
func applyNoColor(flagSet bool) {
	if flagSet || os.Getenv("NO_COLOR") != "" {
		lipgloss.SetColorProfile(termenv.Ascii)
	}
}

// runSubcommand handles a recognized os.Args[1] that doesn't look like a
// flag (see run). quickstart (kata x4cg), status (kata m7jc), and update
// (kata prh1) all have real handlers -- see runQuickstart/runStatus/runUpdate.
// Anything else is an unknown subcommand: a clear error plus usage, exit 2.
func runSubcommand(args []string, stdout, stderr io.Writer) int {
	switch args[0] {
	case "quickstart":
		return runQuickstart(args[1:], stdout, stderr)
	case "status":
		return runStatus(args[1:], stdout, stderr)
	case "update":
		// Built-in self-updater (kata prh1). runUpdate lives in update.go and
		// parses its own flags (--check/-y/--yes/-f/--force); args[1:] is
		// everything after the subcommand name.
		return runUpdate(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "tailport: unknown subcommand %q\n", args[0])
		printUsage(stderr, "")
		return 2
	}
}

// runQuickstart implements `tailport quickstart` (kata x4cg): non-interactive
// onboarding printed straight to stdout, then exit -- no TUI is launched. It
// parses the same shared flags every subcommand gets (parseFlags: -c/--config,
// --no-color, --markers, -v/--version), so e.g. `tailport quickstart -c
// other.yaml` or `tailport quickstart --markers ascii` behave exactly like
// the equivalent flags do everywhere else.
func runQuickstart(args []string, stdout, stderr io.Writer) int {
	cf, code, handled := parseFlags(args, stdout, stderr)
	if handled {
		return code
	}
	applyNoColor(cf.noColor)

	if cf.showVersion {
		fmt.Fprintln(stdout, versionLine())
		return 0
	}
	if err := validateMarkers(cf.markers); err != nil {
		fmt.Fprintln(stderr, "tailport:", err)
		return 2
	}
	if err := validateTheme(cf.theme); err != nil {
		fmt.Fprintln(stderr, "tailport:", err)
		return 2
	}

	// config.Load only reads -- unlike the default TUI path, quickstart never
	// calls config.WriteDefault, so merely running `tailport quickstart` has
	// no side effect on a machine that's never run tailport before. Load
	// still resolves and returns the exact path (honoring -c/--config and
	// XDG_CONFIG_HOME) and, if a config already exists, its persisted
	// Markers preference -- both needed so this output matches what the TUI
	// would actually show.
	cfg, err := config.Load(cf.configPath)
	if err != nil {
		fmt.Fprintln(stderr, "tailport: resolving config:", err)
		return 1
	}

	// Marker-glyph precedence mirrors ui.New: this run's --markers flag wins,
	// else the persisted cfg.Markers, else auto-detect (ui.ResolveEmoji).
	markersMode := cf.markers
	if markersMode == "" {
		markersMode = cfg.Markers
	}

	// Theme precedence mirrors markers above (n7gc): flag > cfg.Theme > auto.
	// Applied here too (not just the default TUI path in run()) since
	// quickstart's legend is rendered through the exact same colored styles
	// (ui.RenderKeyLegendGroups -> helpKeyStyle/helpTextStyle/etc.) the "?"
	// overlay uses, so a light-terminal user piping quickstart's output
	// should see the same legible colors --theme light gives the TUI.
	ui.ApplyTheme(resolveThemeMode(cf.theme, cfg.Theme))

	fmt.Fprint(stdout, quickstartText(cfg.ResolvedPath(), ui.ResolveEmoji(markersMode)))
	return 0
}

// quickstartText builds `tailport quickstart`'s entire stdout output: a short
// paragraph on what tailport does, its safety model (serve is tailnet-only
// and the only automatic path; funnel is public and opt-in ONLY via the `p`
// key behind a strong confirm; :22 is hard-blocked from funnel -- see
// AGENTS.md's "Design constraints" section, which this wording tracks
// closely), the resolved config path, and the full keybinding legend.
//
// SINGLE SOURCE OF TRUTH: the grouped legend comes from ui.KeyLegendGroups,
// rendered by ui.RenderKeyLegendGroups -- the exact same function calls
// internal/ui.helpView uses for the in-TUI "?" overlay, and the same
// keyMap.groups() grouping the bottom-bar grid uses. Nothing here hand-copies a
// key, a section, or a description, so quickstart's legend cannot drift from
// what "?" shows; a future edit has only one place to change.
//
// Kept as a pure string builder (like versionLine) rather than writing
// straight to an io.Writer, so it's testable without stdout/exit-code
// plumbing.
func quickstartText(configPath string, emoji bool) string {
	var b strings.Builder

	fmt.Fprintln(&b, "tailport exposes your machine's locally listening TCP ports across your")
	fmt.Fprintln(&b, "tailnet. It discovers what's listening with `ss` (Linux) / `lsof` (macOS)")
	fmt.Fprintln(&b, "and toggles `tailscale serve` on and off per port -- from an interactive")
	fmt.Fprintln(&b, "list (run `tailport` with no arguments) or headlessly via its subcommands.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Safety model:")
	fmt.Fprintln(&b, "  Tailnet-first. `tailscale serve` (tailnet-only exposure) is the default")
	fmt.Fprintln(&b, "  path for every port you expose: plain HTTP, reachable only by your other")
	fmt.Fprintln(&b, "  tailnet devices at http://<host>:<port>, always a 1:1 port mapping (same")
	fmt.Fprintln(&b, "  port in and out).")
	fmt.Fprintln(&b, "  `tailscale funnel` (public internet exposure) IS supported, but only as")
	fmt.Fprintln(&b, "  a deliberate, per-service opt-in via the `p` key behind a strong y/n")
	fmt.Fprintln(&b, "  confirm that names the port and shows the resulting public URL. tailport")
	fmt.Fprintln(&b, "  never funnels implicitly, in bulk, or without that confirm.")
	fmt.Fprintln(&b, "  `:22` (SSH) is hard-blocked from funnel.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Config path:", configPath)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Keybinding legend (interactive TUI; run `tailport` with no arguments):")
	b.WriteString(ui.RenderKeyLegendGroups(ui.KeyLegendGroups(emoji)))

	return b.String()
}

// statusGather is the data source runStatus reads from. It's a package-level
// var -- not a hardcoded statusreport.Gather() call -- purely so tests can
// substitute a fake without touching live tailscaled/portscan; see
// main_test.go's TestRunStatus*. Production code never reassigns it.
var statusGather = statusreport.Gather

// newStatusFlagSet builds the flag.FlagSet for `tailport status`: --json
// plus --no-color, reusing 5dgj's ContinueOnError/io.Discard-output/silent-
// Usage pattern (see newFlagSet) so parse errors and -h/--help are handled
// the same way as the top-level flags. Deliberately narrower than the
// shared cliFlags set: --version/--config/--markers don't mean anything for
// a one-shot, config-free status report, so they're left undefined here
// rather than silently accepted and ignored.
func newStatusFlagSet() (fs *flag.FlagSet, jsonOut, noColor *bool) {
	fs = flag.NewFlagSet("tailport status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	jsonOut = fs.Bool("json", false, "emit machine-readable JSON (stable schema) instead of a table")
	noColor = fs.Bool("no-color", false, "disable ANSI color output (also honors NO_COLOR)")
	return fs, jsonOut, noColor
}

// printStatusUsage writes `tailport status`'s help text.
func printStatusUsage(w io.Writer) {
	fmt.Fprintln(w, "tailport status -- headless, READ-ONLY report of ports currently exposed")
	fmt.Fprintln(w, "via tailscale serve (tailnet) or funnel (public internet). Never launches")
	fmt.Fprintln(w, "the TUI and never mutates serve/funnel state.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  tailport status [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "      --json        emit machine-readable JSON (stable schema) instead of a table")
	fmt.Fprintln(w, "      --no-color    disable ANSI color output (also honors NO_COLOR)")
}

// runStatus implements `tailport status [--json]`: kata m7jc, tailport's
// first non-interactive mode. It is strictly READ-ONLY (it never calls
// tsserve.On/Off/FunnelOn/FunnelOff) and reuses the exact same status
// functions the TUI's own refresh reads, via statusGather -- see
// internal/statusreport's package doc for why that matters (drift
// prevention between the TUI and this report).
func runStatus(args []string, stdout, stderr io.Writer) int {
	fs, jsonOut, noColor := newStatusFlagSet()
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printStatusUsage(stdout)
			return 0
		}
		fmt.Fprintln(stderr, "tailport status:", err)
		printStatusUsage(stderr)
		return 2
	}
	applyNoColor(*noColor)

	rows, err := statusGather()
	if err != nil {
		fmt.Fprintln(stderr, "tailport status:", err)
		return 1
	}

	if *jsonOut {
		if err := statusreport.WriteJSON(stdout, rows); err != nil {
			fmt.Fprintln(stderr, "tailport status:", err)
			return 1
		}
		return 0
	}
	statusreport.WriteTable(stdout, rows)
	return 0
}

// run implements tailport's entire CLI behavior, returning the process exit
// code rather than calling os.Exit directly so it -- and every branch below
// -- is unit-testable against plain io.Writers.
//
// Dispatch scaffold: os.Args[1] is inspected BEFORE flag parsing. If it's
// present and doesn't start with "-", it's treated as a subcommand name
// (runSubcommand); otherwise args are parsed as tailport's global flags and
// the default TUI path runs. A future subcommand's handler should call
// parseFlags(args[1:], stdout, stderr) itself to get the same --config/
// --no-color/--markers/--version handling as the TUI path, then add its own
// flags to a FlagSet built the same way as newFlagSet if it needs more.
func run(args []string, stdout, stderr io.Writer) int {
	// NO_COLOR (https://no-color.org) must be honored regardless of which
	// path runs below; --no-color itself only exists once flags are parsed,
	// which happens differently per path (or not at all yet, for the
	// still-unimplemented subcommands), so check the env var up front.
	applyNoColor(false)

	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return runSubcommand(args, stdout, stderr)
	}

	cf, code, handled := parseFlags(args, stdout, stderr)
	if handled {
		return code
	}
	applyNoColor(cf.noColor)

	if cf.showVersion {
		fmt.Fprintln(stdout, versionLine())
		return 0
	}

	if err := validateMarkers(cf.markers); err != nil {
		fmt.Fprintln(stderr, "tailport:", err)
		return 2
	}
	if err := validateTheme(cf.theme); err != nil {
		fmt.Fprintln(stderr, "tailport:", err)
		return 2
	}

	if err := config.WriteDefault(cf.configPath); err != nil {
		fmt.Fprintln(stderr, "tailport: writing default config:", err)
		return 1
	}
	cfg, err := config.Load(cf.configPath)
	if err != nil {
		fmt.Fprintln(stderr, "tailport: loading config:", err)
		return 1
	}

	// Theme override hook (n7gc): applied once, here, before the TUI's first
	// render -- flag > cfg.Theme > auto (resolveThemeMode). "auto" (or an
	// unrecognized cfg.Theme) leaves lipgloss's own background detection, and
	// its dark fallback, alone; see ui.ApplyTheme/resolveTheme's doc comment
	// for why that fallback already satisfies "undetectable -> treat as dark"
	// with no extra code here. Unlike cf.markers (below), this is a
	// process-global lipgloss side effect, not per-model state, so it's
	// applied directly rather than threaded through ui.Run/ui.New.
	ui.ApplyTheme(resolveThemeMode(cf.theme, cfg.Theme))

	// cf.markers is passed through as a separate, run-only override (zn2x)
	// rather than written into cfg.Markers here: cfg is the exact value the
	// TUI keeps as its live state and later Save()s back to disk on any
	// unrelated mutation (favorite/label/lock/etc.), so mutating cfg.Markers
	// in place would leak this run's --markers flag into the persisted
	// config on the next unrelated save. ui.Run/ui.New apply the override to
	// rendering only. See internal/ui/ui.go's New doc comment for the full
	// reasoning.
	if err := ui.Run(cfg, cf.markers); err != nil {
		fmt.Fprintln(stderr, "tailport:", err)
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
