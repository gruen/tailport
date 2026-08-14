// Package statusreport builds tailport's headless `status` report: a
// READ-ONLY snapshot of every port currently exposed via `tailscale serve`
// (tailnet), `tailscale funnel` (public internet), or a Caddy-edge publish
// (public internet at a custom hostname, kata v1z5).
//
// It is deliberately the SAME source of truth internal/ui's TUI reads from
// (tsserve.Status, portscan.List, tsserve.FQDN, tsserve.PublicURL, and now
// caddyedge.Client.List for publish state) so this report can never drift
// from what the interactive list shows for the same node -- see Gather. It
// never calls any of tsserve's or caddyedge's mutating functions
// (On/Off/FunnelOn/FunnelOff/Publish/Unpublish).
package statusreport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/gruen/tailport/internal/caddyedge"
	"github.com/gruen/tailport/internal/config"
	"github.com/gruen/tailport/internal/portscan"
	"github.com/gruen/tailport/internal/tsserve"
)

// Mode names the exposure mechanism for a Row. It was originally documented
// as a closed two-value enum (serve, funnel); ModePublish below is a
// DOCUMENTED SCHEMA CHANGE (kata v1z5 step 6) adding a third value for
// Caddy-edge publish exposure -- the exact kind of change the original
// comment said would need to be explicit, not silent. Any future addition
// should be held to the same bar.
type Mode string

const (
	// ModeServe is tailnet-only exposure via `tailscale serve`: reachable by
	// other devices on the tailnet, never the public internet.
	ModeServe Mode = "serve"
	// ModeFunnel is public-internet exposure via `tailscale funnel`: reachable
	// by anyone, gated behind an explicit opt-in everywhere else in tailport.
	// Always rendered distinctly from ModeServe -- see WriteTable.
	ModeFunnel Mode = "funnel"
	// ModePublish is public-internet exposure via a user-controlled Caddy edge
	// node (the `P` publish path, kata v1z5): reachable by anyone at a custom
	// public hostname (no ts.net, no Funnel slot). For a tailport-driven port,
	// ModeFunnel and ModePublish are mutually exclusive by construction (the
	// UI refuses to create one where the other already exists) -- see Build's
	// doc comment for what happens when they're BOTH observed anyway (a sign
	// of exposure created outside tailport). Always rendered distinctly from
	// ModeServe -- see WriteTable.
	ModePublish Mode = "publish"
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
	// Mode is "serve", "funnel", or "publish" (see the Mode constants).
	// Funnel and publish both mean PUBLIC INTERNET exposure; serve is
	// tailnet-only.
	Mode Mode `json:"mode"`
	// URL is the address a client would actually use to reach this port: the
	// tailnet http://<host>:<port> URL for serve, the public
	// https://<fqdn>[:port] URL for funnel, or the public
	// https://<hostname> URL for publish (no port -- Caddy always terminates
	// on 443).
	URL string `json:"url"`
	// Auth reports whether basic auth is enforced at the Caddy edge for this
	// row. Only ever true for a ModePublish row (auth is a publish-only
	// concept; ModeServe/ModeFunnel rows always carry false). Added
	// alongside ModePublish (kata v1z5 step 6) as an ADDITIVE field --
	// json:"auth,omitempty" means it's simply absent from a serve/funnel
	// row's JSON, so existing consumers parsing only "port"/"process"/
	// "mode"/"url" are unaffected.
	Auth bool `json:"auth,omitempty"`
}

// Published is the small, statusreport-local shape of one Caddy-edge publish
// route relevant to this machine, keyed by local port in Build's published
// parameter. Deliberately not caddyedge.RouteInfo: mirroring how funnel is
// passed as a plain map[int]int rather than a tsserve type, statusreport
// stays decoupled from caddyedge's wire schema and client details, taking
// only the two fields Build actually needs to render a row.
type Published struct {
	// Hostname is the public hostname this port is published at
	// (rendered as https://<Hostname>, no port).
	Hostname string
	// Auth reports whether the Caddy route enforces basic auth.
	Auth bool
}

// Document is the top-level JSON object `tailport status --json` writes.
// Wrapping the row list in an object (rather than emitting a bare array)
// leaves room to add sibling fields later without breaking existing
// consumers that only look at "ports".
type Document struct {
	Ports []Row `json:"ports"`
}

// caddyListTimeout bounds Gather's best-effort caddyedge.Client.List call, so
// an unreachable edge degrades the whole report within a bounded time rather
// than hanging `tailport status` indefinitely. Matches caddyedge's own
// defaultTimeout for a single admin-API round trip.
const caddyListTimeout = 5 * time.Second

// Gather performs the READ-ONLY calls needed to build a status report --
// portscan.List, tsserve.Status, tsserve.FQDN, os.Hostname, and (when
// cfg.Caddy.Domain is configured) caddyedge.Client.List -- the exact same
// functions internal/ui's refresh()/fetchFQDN/publish-poll read, so
// `tailport status` reports precisely what the TUI would show right now. It
// never calls tsserve.On/Off/FunnelOn/FunnelOff or caddyedge's
// Publish/Unpublish.
//
// cfg is needed (unlike the pre-v1z5 signature) because publish state lives
// on a remote Caddy edge, reachable only via cfg.Caddy.*; this is also why
// `tailport status` now honors -c/--config (see cmd/tailport's
// newStatusFlagSet).
func Gather(cfg config.Config) ([]Row, error) {
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
	published := gatherPublished(cfg, fqdn)
	return Build(ports, active, funnel, published, host, fqdn), nil
}

// gatherPublished fetches this machine's Caddy-edge publish routes, or nil
// when publish is unconfigured (cfg.Caddy.Domain == "") or the machine's
// short label can't be resolved (fqdn == "", e.g. tailscaled unreachable) --
// either way, zero cost / zero risk, per this issue's spec. A List failure
// (edge unreachable, admin API error) degrades quietly to nil rather than
// failing Gather: a status report must still show serve/funnel rows when the
// edge can't be reached.
func gatherPublished(cfg config.Config, fqdn string) map[int]Published {
	if cfg.Caddy.Domain == "" {
		return nil
	}
	label := shortLabel(fqdn)
	if label == "" {
		return nil
	}

	client := &caddyedge.Client{
		AdminURL:   fmt.Sprintf("http://%s:%d", cfg.Caddy.Hostname, cfg.Caddy.AdminPort),
		ServerName: cfg.Caddy.ServerName,
	}
	ctx, cancel := context.WithTimeout(context.Background(), caddyListTimeout)
	defer cancel()
	routes, err := client.List(ctx)
	if err != nil {
		return nil
	}

	// Filter to routes whose backend is THIS machine (Label matches the
	// short MagicDNS label) -- other tailport computers publishing through
	// the same shared Caddy edge write routes too, and those aren't ours to
	// report. Deliberately not filtered on RouteInfo.Owned: a route matching
	// this machine's backend that ISN'T tailport-owned (a foreign/manual
	// Caddy edit) is exactly the kind of external-mutation drift a status
	// report should surface, not hide.
	published := make(map[int]Published, len(routes))
	for _, r := range routes {
		if r.Label != label {
			continue
		}
		published[r.Port] = Published{Hostname: r.Hostname, Auth: r.Auth}
	}
	return published
}

// shortLabel derives the short MagicDNS label from a full FQDN (its first
// `.`-component, e.g. "host" from "host.tailnet.ts.net") -- the same
// derivation the publish path uses to address a backend by label (see
// caddyedge's package doc and v1z5's Architecture section: Self.DNSName's
// first `.`-component, not Self.HostName, which can diverge via
// sanitization or a dedup suffix). Returns "" for an empty fqdn.
func shortLabel(fqdn string) string {
	if fqdn == "" {
		return ""
	}
	if i := strings.Index(fqdn, "."); i >= 0 {
		return fqdn[:i]
	}
	return fqdn
}

// Build assembles the report rows from already-fetched data. Split out from
// Gather so tests can drive it with fake ports/active/funnel/published data
// without a live tailscaled or Caddy edge (per this issue's verification
// bar).
//
// Only ports that are actually exposed -- present in active, funnel, or
// published -- are included; a merely-listening, never-served port is out
// of scope for a status report (that's what the TUI's full port list is
// for). The exposed set deliberately includes published on its own (not
// gated behind also being in active): a published-but-not-served port is a
// dangling forward -- Caddy is still proxying to it, so a public visitor
// gets a 502 -- and that drift must stay visible, not disappear because the
// local backend went away.
//
// Mode selection is funnel XOR publish for a tailport-driven port (mutually
// exclusive by construction -- the UI refuses to create one where the other
// already exists), and either one outranks plain serve for the same port
// (mirroring portItem.markerGlyph in internal/ui: any public exposure is
// what actually governs reachability, regardless of tailnet-serve state).
// The one exception: if a port is observed carrying BOTH a funnel AND a
// Caddy-owned publish route -- only possible via exposure created outside
// tailport, since tailport's own guards prevent it -- Build emits BOTH rows
// for that port rather than picking a winner. The duplicate exposure is
// itself the explicit signal; no new schema field encodes it.
func Build(ports []portscan.Port, active []int, funnel map[int]int, published map[int]Published, host, fqdn string) []Row {
	processByPort := make(map[int]string, len(ports))
	for _, p := range ports {
		processByPort[p.Number] = p.Process
	}

	exposed := make(map[int]bool, len(active)+len(funnel)+len(published))
	for _, p := range active {
		exposed[p] = true
	}
	for p := range funnel {
		exposed[p] = true
	}
	for p := range published {
		exposed[p] = true
	}

	numbers := make([]int, 0, len(exposed))
	for p := range exposed {
		numbers = append(numbers, p)
	}
	sort.Ints(numbers)

	rows := make([]Row, 0, len(numbers))
	for _, n := range numbers {
		process := processByPort[n]
		pubPort, isFunnel := funnel[n]
		pub, isPublished := published[n]

		switch {
		case isFunnel && isPublished:
			// External-mutation drift: both exposures observed on one port.
			// Emit both rows rather than ranking one over the other.
			rows = append(rows, Row{Port: n, Process: process, Mode: ModeFunnel, URL: tsserve.PublicURL(fqdn, pubPort)})
			rows = append(rows, Row{Port: n, Process: process, Mode: ModePublish, URL: publishURL(pub), Auth: pub.Auth})
		case isFunnel:
			rows = append(rows, Row{Port: n, Process: process, Mode: ModeFunnel, URL: tsserve.PublicURL(fqdn, pubPort)})
		case isPublished:
			rows = append(rows, Row{Port: n, Process: process, Mode: ModePublish, URL: publishURL(pub), Auth: pub.Auth})
		default:
			rows = append(rows, Row{Port: n, Process: process, Mode: ModeServe, URL: fmt.Sprintf("http://%s:%d", host, n)})
		}
	}
	return rows
}

// publishURL builds the public HTTPS URL a publish route is reachable at.
// Unlike tsserve.PublicURL (which sometimes shows a non-443 funnel ingress
// port), a Caddy-edge publish is always terminated on 443 -- "no port in
// the URL" is the whole point of v1z5's custom-hostname design -- so no port
// ever appears.
func publishURL(p Published) string {
	return "https://" + p.Hostname
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

// modeServeText, modeFunnelText, and modePublishText are the plain-text mode
// labels. Serve is lowercase and parenthetically scoped; funnel and publish
// are both upper-case and say "public" outright, so all three remain
// visually distinct even with color disabled (--no-color, NO_COLOR, or a
// non-TTY pipe) -- the case difference alone is legible in any terminal, and
// "FUNNEL" vs "PUBLISH" are textually unambiguous from each other too.
const (
	modeServeText   = "serve (tailnet)"
	modeFunnelText  = "FUNNEL (public)"
	modePublishText = "PUBLISH (edge, public)"
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
		fmt.Fprintln(w, "No ports are currently served, funnelled, or published.")
		return
	}

	var buf strings.Builder
	tw := tabwriter.NewWriter(&buf, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "MODE\tPORT\tPROCESS\tURL")
	for _, r := range rows {
		mode := modeServeText
		switch r.Mode {
		case ModeFunnel:
			mode = modeFunnelText
		case ModePublish:
			mode = modePublishText
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
	// disabled -- see the doc comment above. modePublishText gets the SAME
	// (funnel-style) treatment: both are public-internet exposure and should
	// draw the eye the same way; the text itself ("FUNNEL" vs "PUBLISH")
	// already keeps them distinguishable.
	out = strings.ReplaceAll(out, modeFunnelText, funnelStyle.Render(modeFunnelText))
	out = strings.ReplaceAll(out, modePublishText, funnelStyle.Render(modePublishText))
	fmt.Fprint(w, out)
}
