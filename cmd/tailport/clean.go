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
	cleanServeStatus = tsserve.StatusDetail
	cleanPublicPorts = statusreport.PublicPorts
	cleanUnserve     = tsserve.Off
)

func printCleanUsage(w io.Writer) {
	fmt.Fprintln(w, "tailport clean -- turn off DANGLING tailscale serve forwards: served, with")
	fmt.Fprintln(w, "nothing listening locally. Such a forward still holds its port on the")
	fmt.Fprintln(w, "tailnet IPs, so an app binding the wildcard address there fails with")
	fmt.Fprintln(w, "EADDRINUSE. No grace period (running it is deliberate); never touches :22,")
	fmt.Fprintln(w, "locked ports, or ports with funnel, a Caddy publish or a Cloudflare tunnel.")
	fmt.Fprintln(w, "Only a plain 1:1 serve (127.0.0.1:P -> P) qualifies; a remap is left alone.")
	fmt.Fprintln(w, "This is a permanent clear: it ignores auto_pause (the TUI's pause/resume")
	fmt.Fprintln(w, "setting) and does not touch the TUI's remembered paused serves, which are")
	fmt.Fprintln(w, "not tailscale state (a paused port has no forward to clear).")
	fmt.Fprintln(w, "Prints one line per port. Exits non-zero, clearing nothing, if tailscale")
	fmt.Fprintln(w, "status or publish/tunnel state can't be read up front. A failure DURING")
	fmt.Fprintln(w, "cleanup (an unserve or a re-scan) also exits non-zero, but forwards already")
	fmt.Fprintln(w, "cleared by then stay cleared; the printed lines say which.")
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
// grace, and it deliberately ignores auto_pause (global and per-port):
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
	if fs.NArg() > 0 {
		// A stray positional (`clean 8080 --dry-run` stops flag parsing at
		// 8080) must never silently turn into "clear everything".
		fmt.Fprintf(stderr, "tailport clean: unexpected argument %q\n", fs.Arg(0))
		printCleanUsage(stderr)
		return 2
	}
	applyNoColor(*noColor)

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "tailport clean: resolving config:", err)
		return 1
	}
	// The slow public-state read goes FIRST, so the listener scan below is the
	// freshest fact before any mutation. Unreadable public state clears nothing:
	// unknown is not empty.
	public, err := cleanPublicPorts(cfg)
	if err != nil {
		fmt.Fprintln(stderr, "tailport clean:", err)
		fmt.Fprintln(stderr, "tailport clean: nothing was cleared")
		return 1
	}
	targets, err := cleanEligible(cfg, public)
	if err != nil {
		fmt.Fprintln(stderr, "tailport clean:", err)
		return 1
	}
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
		// TOCTOU: re-check this port against a fresh listener scan and serve
		// status immediately before unserving it, so an app that started
		// since the first scan is never cut off. A scan failure aborts.
		now, err := cleanEligible(cfg, public)
		if err != nil {
			fmt.Fprintln(stderr, "tailport clean: aborting:", err)
			return 1
		}
		if !containsPort(now, p) {
			fmt.Fprintf(stdout, "skipped :%d (no longer dangling)\n", p)
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

// cleanEligible reads serve status and scans listeners afresh and applies the
// shared eligibility rule (internal/stale) with the given public-port set. The
// listener scan runs LAST (roborev 6192): it is the signal that must be
// freshest at the moment a forward is removed, so an app that starts while the
// slower serve-status request is in flight is still seen and kept.
func cleanEligible(cfg config.Config, public map[int]bool) ([]int, error) {
	detail, err := cleanServeStatus()
	if err != nil {
		return nil, fmt.Errorf("reading tailscale serve status: %w", err)
	}
	ports, err := cleanListPorts()
	if err != nil {
		return nil, fmt.Errorf("listing local ports: %w", err)
	}
	served := make(map[int]bool, len(detail.Active))
	for _, p := range detail.Active {
		served[p] = true
	}
	// Unattributed tailnet-IP rows on a served port are tailscaled's own proxy,
	// not an app (kata nvv6): resolve them so a dangling forward stays one.
	ports = portscan.ResolveTailnetRows(ports, served, true)
	listening := make(map[int]bool, len(ports))
	for _, p := range ports {
		listening[p.Number] = true
	}
	return stale.Clearable(cfg, served, listening, detail.Funnel, public, detail.OneToOne), nil
}

func containsPort(ports []int, p int) bool {
	for _, q := range ports {
		if q == p {
			return true
		}
	}
	return false
}
