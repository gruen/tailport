// Package statusreport builds tailport's headless `status` report: a
// READ-ONLY snapshot of every port currently exposed via `tailscale serve`
// (tailnet) or `tailscale funnel` (public internet).
//
// It is deliberately the SAME source of truth internal/ui's TUI reads from
// (tsserve.Status, portscan.List, tsserve.FQDN, tsserve.PublicURL) so this
// report can never drift from what the interactive list shows for the same
// node -- see Gather. It never calls any of tsserve's mutating functions
// (On/Off/FunnelOn/FunnelOff).
package statusreport

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/charmbracelet/lipgloss"

	"github.com/gruen/tailport/internal/portscan"
	"github.com/gruen/tailport/internal/tsserve"
)

// Mode names the exposure mechanism for a Row. These two values are the
// entire stable enum; new values, if ever added, would be a documented
// schema change, not a silent addition.
type Mode string

const (
	// ModeServe is tailnet-only exposure via `tailscale serve`: reachable by
	// other devices on the tailnet, never the public internet.
	ModeServe Mode = "serve"
	// ModeFunnel is public-internet exposure via `tailscale funnel`: reachable
	// by anyone, gated behind an explicit opt-in everywhere else in tailport.
	// Always rendered distinctly from ModeServe -- see WriteTable.
	ModeFunnel Mode = "funnel"
)

// Row is one exposed port in the report. Field names and types are the
// stable JSON schema `tailport status --json` promises; treat any change to
// an existing field's name, type, or meaning as a breaking change.
type Row struct {
	// Port is the local TCP port number (the same number tailnet serve is
	// mapped 1:1 to; for funnel it's the local target, not the public ingress
	// port -- see URL for the address that's actually reachable).
	Port int `json:"port"`
	// Process is the name of the local process bound to Port, or "" if
	// nothing is currently listening there. An exposed-but-empty Process is a
	// "dangling forward": tailscale is still holding the port open, but a
	// peer hitting URL would get connection refused until something binds it
	// locally again.
	Process string `json:"process"`
	// Mode is "serve" or "funnel" (see the Mode constants). Funnel means
	// PUBLIC INTERNET exposure.
	Mode Mode `json:"mode"`
	// URL is the address a client would actually use to reach this port: the
	// tailnet http://<host>:<port> URL for serve, or the public
	// https://<fqdn>[:port] URL for funnel.
	URL string `json:"url"`
}

// Document is the top-level JSON object `tailport status --json` writes.
// Wrapping the row list in an object (rather than emitting a bare array)
// leaves room to add sibling fields later without breaking existing
// consumers that only look at "ports".
type Document struct {
	Ports []Row `json:"ports"`
}

// Gather performs the READ-ONLY calls needed to build a status report --
// portscan.List, tsserve.Status, tsserve.FQDN, os.Hostname -- the exact same
// functions internal/ui's refresh()/fetchFQDN read on every TUI refresh, so
// `tailport status` reports precisely what the TUI would show right now. It
// never calls tsserve.On/Off/FunnelOn/FunnelOff.
func Gather() ([]Row, error) {
	ports, err := portscan.List()
	if err != nil {
		return nil, fmt.Errorf("listing local ports: %w", err)
	}
	active, funnel, err := tsserve.Status()
	if err != nil {
		return nil, fmt.Errorf("reading tailscale serve status: %w", err)
	}
	// Best-effort, mirroring ui.New/fetchFQDN: an unresolved hostname or FQDN
	// degrades the URL rather than failing the whole report.
	host, _ := os.Hostname()
	fqdn, _ := tsserve.FQDN()
	return Build(ports, active, funnel, host, fqdn), nil
}

// Build assembles the report rows from already-fetched data. Split out from
// Gather so tests can drive it with fake ports/active/funnel data without a
// live tailscaled (per this issue's verification bar).
//
// Only ports that are actually exposed -- present in active or funnel --
// are included; a merely-listening, never-served port is out of scope for a
// status report (that's what the TUI's full port list is for). funnel
// outranks active for the same port, mirroring portItem.markerGlyph in
// internal/ui: a funnelled port is reachable from the public internet
// regardless of its tailnet-serve state.
func Build(ports []portscan.Port, active []int, funnel map[int]int, host, fqdn string) []Row {
	processByPort := make(map[int]string, len(ports))
	for _, p := range ports {
		processByPort[p.Number] = p.Process
	}

	exposed := make(map[int]bool, len(active)+len(funnel))
	for _, p := range active {
		exposed[p] = true
	}
	for p := range funnel {
		exposed[p] = true
	}

	numbers := make([]int, 0, len(exposed))
	for p := range exposed {
		numbers = append(numbers, p)
	}
	sort.Ints(numbers)

	rows := make([]Row, 0, len(numbers))
	for _, n := range numbers {
		row := Row{Port: n, Process: processByPort[n]}
		if pub, ok := funnel[n]; ok {
			row.Mode = ModeFunnel
			row.URL = tsserve.PublicURL(fqdn, pub)
		} else {
			row.Mode = ModeServe
			row.URL = fmt.Sprintf("http://%s:%d", host, n)
		}
		rows = append(rows, row)
	}
	return rows
}

// WriteJSON writes rows as the stable Document schema (see Document/Row's
// doc comments), always emitting a "ports" array -- [] rather than null when
// rows is empty -- so consumers never need a null check.
func WriteJSON(w io.Writer, rows []Row) error {
	if rows == nil {
		rows = []Row{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(Document{Ports: rows})
}

// funnelStyle renders the funnel mode marker distinctly from tailnet serve
// (safety: a public-internet exposure should never blend in). It's a
// package-level lipgloss.NewStyle() with no custom renderer, so it renders
// through the same shared default renderer main.applyNoColor mutates --
// --no-color/NO_COLOR silently downgrades it to plain text, matching every
// other style in this codebase (see internal/ui's package-level styles and
// main_test.go's TestApplyNoColorForcesAsciiProfile).
var funnelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("201")).Bold(true)

// modeServeText and modeFunnelText are the plain-text mode labels. Serve is
// lowercase and parenthetically scoped; funnel is upper-case and says
// "public" outright, so the two remain visually distinct even with color
// disabled (--no-color, NO_COLOR, or a non-TTY pipe) -- the case difference
// alone is legible in any terminal.
const (
	modeServeText  = "serve (tailnet)"
	modeFunnelText = "FUNNEL (public)"
)

// WriteTable writes rows as a human-readable, column-aligned table. Mode
// leads (leftmost column) so a public funnel is the first thing a reader's
// eye lands on, per this issue's "funnels visually distinct" safety
// requirement.
//
// Color is applied AFTER text/tabwriter has already computed column widths
// and flushed plain text: tabwriter counts bytes, not display width, so
// coloring a cell before flushing would let invisible ANSI escapes skew
// alignment of every column to its right. Post-processing a finished,
// correctly-aligned table is simpler than teaching tabwriter about ANSI.
func WriteTable(w io.Writer, rows []Row) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "No ports currently exposed via tailscale serve or funnel.")
		return
	}

	var buf strings.Builder
	tw := tabwriter.NewWriter(&buf, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "MODE\tPORT\tPROCESS\tURL")
	for _, r := range rows {
		mode := modeServeText
		if r.Mode == ModeFunnel {
			mode = modeFunnelText
		}
		process := r.Process
		if process == "" {
			process = "?"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", mode, r.Port, process, r.URL)
	}
	tw.Flush()

	out := buf.String()
	// funnelStyle.Render is a no-op (returns its input verbatim) under the
	// Ascii color profile, so this substitution is itself a no-op with color
	// disabled -- see the doc comment above.
	out = strings.ReplaceAll(out, modeFunnelText, funnelStyle.Render(modeFunnelText))
	fmt.Fprint(w, out)
}
