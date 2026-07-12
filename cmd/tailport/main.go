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
// flag (see run). quickstart/status/update are reserved names, each with
// its own kata issue; until those land this just says so plainly instead of
// silently falling through to the TUI or pretending to work. Whichever
// issue lands first should replace its case with a real handler -- see the
// dispatch scaffold note in run() for how little that diff should need to
// touch here.
func runSubcommand(args []string, stdout, stderr io.Writer) int {
	switch args[0] {
	case "quickstart":
		// case "quickstart": … implemented under kata x4cg (non-interactive
		// onboarding + the keybinding legend, printed to stdout).
		fmt.Fprintln(stderr, "tailport: quickstart is not implemented yet (kata x4cg)")
		return 1
	case "status":
		// case "status": … implemented under kata m7jc (headless read-only
		// exposure report, optionally --json).
		fmt.Fprintln(stderr, "tailport: status is not implemented yet (kata m7jc)")
		return 1
	case "update":
		// case "update": … implemented under kata prh1 (built-in self-updater).
		fmt.Fprintln(stderr, "tailport: update is not implemented yet (kata prh1)")
		return 1
	default:
		fmt.Fprintf(stderr, "tailport: unknown subcommand %q\n", args[0])
		printUsage(stderr, "")
		return 2
	}
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

	if err := config.WriteDefault(cf.configPath); err != nil {
		fmt.Fprintln(stderr, "tailport: writing default config:", err)
		return 1
	}
	cfg, err := config.Load(cf.configPath)
	if err != nil {
		fmt.Fprintln(stderr, "tailport: loading config:", err)
		return 1
	}

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
