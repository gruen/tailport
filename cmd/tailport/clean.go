package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/gruen/tailport/internal/config"
	"github.com/gruen/tailport/internal/portscan"
	"github.com/gruen/tailport/internal/stale"
	"github.com/gruen/tailport/internal/statusreport"
	"github.com/gruen/tailport/internal/tsserve"
)

// The data sources and the one mutation `tailport clean` uses (kata 9p43),
// package-level vars purely so tests can substitute fakes without a live
// tailscaled, ss/lsof, Caddy edge or cloudflared -- the same injectable-var
// pattern as statusGather. Production code never reassigns them.
var (
	cleanListPorts   = portscan.List
	cleanServeStatus = tsserve.Status
	cleanPublicPorts = statusreport.PublicPorts
	cleanUnserve     = tsserve.Off
)

func printCleanUsage(w io.Writer) {
	fmt.Fprintln(w, "tailport clean -- turn off DANGLING tailscale serve forwards: served, with")
	fmt.Fprintln(w, "nothing listening locally. Such a forward still holds its port on the")
	fmt.Fprintln(w, "tailnet IPs, so an app binding the wildcard address there fails with")
	fmt.Fprintln(w, "EADDRINUSE. No grace period (running it is deliberate); never touches :22,")
	fmt.Fprintln(w, "locked ports, or ports with funnel, a Caddy publish or a Cloudflare tunnel.")
	fmt.Fprintln(w, "Prints one line per port; exits non-zero if tailscale status can't be read")
	fmt.Fprintln(w, "or any unserve fails.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  tailport clean [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "      --dry-run            list what would be cleared; change nothing")
	fmt.Fprintln(w, "  -c, --config <path>      config file path (default: $XDG_CONFIG_HOME/tailport/config.yaml, else ~/.config/tailport/config.yaml)")
}

// runClean implements `tailport clean [--dry-run]` (kata 9p43): the one-shot,
// TUI-free counterpart of the TUI's grace-period auto-clear, usable as e.g. a
// `predev` step. It applies the SAME eligibility rule (internal/stale) but no
// grace, and it deliberately ignores auto_clear_stale (global and per-port):
// that setting governs only what tailport does UNPROMPTED in the TUI, whereas
// running this command is itself the instruction. Locks, :22 and public
// exposure still protect a port.
func runClean(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tailport clean", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	dryRun := fs.Bool("dry-run", false, "list what would be cleared; change nothing")
	noColor := fs.Bool("no-color", false, "disable ANSI color output (also honors NO_COLOR)")
	configPath := fs.String("config", "", "config file path")
	fs.StringVar(configPath, "c", "", "shorthand for --config")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printCleanUsage(stdout)
			return 0
		}
		fmt.Fprintln(stderr, "tailport clean:", err)
		printCleanUsage(stderr)
		return 2
	}
	applyNoColor(*noColor)

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "tailport clean: resolving config:", err)
		return 1
	}
	ports, err := cleanListPorts()
	if err != nil {
		fmt.Fprintln(stderr, "tailport clean: listing local ports:", err)
		return 1
	}
	active, funnel, err := cleanServeStatus()
	if err != nil {
		fmt.Fprintln(stderr, "tailport clean: reading tailscale serve status:", err)
		return 1
	}
	served := make(map[int]bool, len(active))
	for _, p := range active {
		served[p] = true
	}
	listening := make(map[int]bool, len(ports))
	for _, p := range ports {
		listening[p.Number] = true
	}
	targets := stale.Clearable(cfg, served, listening, funnel, cleanPublicPorts(cfg))
	if len(targets) == 0 {
		fmt.Fprintln(stdout, "nothing to clean")
		return 0
	}
	code := 0
	for _, p := range targets {
		if *dryRun {
			fmt.Fprintf(stdout, "would clear :%d\n", p)
			continue
		}
		if err := cleanUnserve(p); err != nil {
			fmt.Fprintf(stderr, "tailport clean: could not clear :%d: %v\n", p, err)
			code = 1
			continue
		}
		fmt.Fprintf(stdout, "cleared :%d\n", p)
	}
	return code
}
