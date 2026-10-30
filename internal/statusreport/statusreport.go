// Package statusreport builds tailport's headless `status` report: a
// READ-ONLY snapshot of every port currently exposed via `tailscale serve`
// (tailnet), `tailscale funnel` (public internet), a Caddy-edge publish
// (public internet at a custom hostname, kata v1z5), or a Cloudflare tunnel
// (public internet at a trycloudflare.com or custom hostname, kata nc1j /
// p7c5; reported here since kata afk4).
//
// It is deliberately the SAME source of truth internal/ui's TUI reads from
// (tsserve.Status, portscan.List, tsserve.FQDN, tsserve.PublicURL,
// caddyedge.Client.List for publish state, and cftunnel.Client.Discover/
// Health for tunnel state) so this report can never drift from what the
// interactive list shows for the same node -- see Gather. It never calls any
// mutating function: no tsserve On/Off/FunnelOn/FunnelOff, no caddyedge
// Publish/Unpublish, and no cftunnel Start/Stop.
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
	"github.com/gruen/tailport/internal/cftunnel"
	"github.com/gruen/tailport/internal/config"
	"github.com/gruen/tailport/internal/portscan"
	"github.com/gruen/tailport/internal/tsserve"
)

// Mode names the exposure mechanism for a Row. It was originally documented
// as a closed two-value enum (serve, funnel). ModePublish was a DOCUMENTED
// SCHEMA CHANGE (kata v1z5 step 6) adding Caddy-edge publish; ModeTunnel is
// the same kind of change (kata afk4) adding Cloudflare tunnel exposure.
// Any future addition should be held to the same bar: explicit, never
// silent.
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
	// node (the `d` publish path, kata v1z5; re-lettered from `p` under kata
	// 58ws): reachable by anyone at a custom public hostname (no ts.net, no
	// Funnel slot). INDEPENDENT of ModeFunnel and ModeTunnel -- since kata
	// th05 the UI permits any combination of the three public paths on one
	// port, and Build emits one row per path (see its doc comment). Always
	// rendered distinctly from ModeServe -- see WriteTable.
	ModePublish Mode = "publish"
	// ModeTunnel is public-internet exposure via a Cloudflare tunnel
	// (cloudflared), either quick (`o`: a random *.trycloudflare.com hostname)
	// or named (`O`: a custom hostname bound by config, kata nc1j / p7c5).
	// Independent of ModeFunnel and ModePublish, exactly like ModePublish --
	// one port may carry all three, each with its own row. Always rendered
	// distinctly from ModeServe -- see WriteTable.
	ModeTunnel Mode = "tunnel"
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
	// Mode is "serve", "funnel", "publish", or "tunnel" (see the Mode
	// constants). Funnel, publish, and tunnel all mean PUBLIC INTERNET
	// exposure; serve is tailnet-only.
	Mode Mode `json:"mode"`
	// URL is the address a client would actually use to reach this port: the
	// tailnet http://<host>:<port> URL for serve, the public
	// https://<fqdn>[:port] URL for funnel, the public https://<hostname> URL
	// for publish (no port -- Caddy always terminates on 443), or the public
	// https://<hostname> URL for tunnel. A tunnel row's URL is "" while a
	// quick tunnel hasn't been assigned its hostname yet (cloudflared reports
	// it only after connecting); WriteTable renders a pending note for that.
	URL string `json:"url"`
	// Auth reports whether basic auth is enforced at the Caddy edge for this
	// row. Only ever true for a ModePublish row (auth is a publish-only
	// concept; ModeServe/ModeFunnel/ModeTunnel rows always carry false).
	// Added
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

// Tunnel is the small, statusreport-local shape of one tailport-owned
// Cloudflare tunnel relevant to this machine. Deliberately not
// cftunnel.Running: as with Published, statusreport stays decoupled from the
// owning package's types and carries only what a row needs. Tunnels are a
// SLICE, not a map keyed by port: two owned tunnels can share one local port
// (two tailport instances racing), and each is a distinct public exposure
// that must get its own row rather than silently overwriting the other
// (roborev 5416).
type Tunnel struct {
	// Port is the local TCP port this tunnel exposes.
	Port int
	// Hostname is the tunnel's public hostname -- for a named tunnel the
	// hostname recovered from tailport's sentinel logfile, for a quick tunnel
	// the *.trycloudflare.com host cloudflared reported (empty until it has
	// assigned one).
	Hostname string
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

// tunnelHealthTimeout bounds Gather's per-tunnel metrics scrape -- one shared
// deadline for every owned tunnel, mirroring internal/ui's pollTunnelsCmd
// (3s), so a wedged cloudflared metrics server degrades the report within a
// bounded time instead of hanging `tailport status`.
const tunnelHealthTimeout = 3 * time.Second

// Gather performs the READ-ONLY calls needed to build a status report --
// portscan.List, tsserve.Status, tsserve.FQDN, os.Hostname, (when
// cfg.Caddy.Domain is configured) caddyedge.Client.List, and (when
// cloudflared is installed) cftunnel.Client.Discover/Health -- the exact same
// functions internal/ui's refresh()/fetchFQDN/publish-poll/tunnel-poll read,
// so `tailport status` reports precisely what the TUI would show right now.
// It never calls tsserve.On/Off/FunnelOn/FunnelOff, caddyedge's
// Publish/Unpublish, or cftunnel's Start/Stop.
//
// cfg is needed (unlike the pre-v1z5 signature) because publish state lives
// on a remote Caddy edge, reachable only via cfg.Caddy.*, and tunnel state
// depends on cfg.Cloudflared.Binary; this is also why `tailport status` now
// honors -c/--config (see cmd/tailport's newStatusFlagSet).
func Gather(cfg config.Config) ([]Row, error) {
	return gatherFrom(cfg, gatherSources{
		listPorts:       portscan.List,
		fqdn:            tsserve.FQDN,
		tailscaleStatus: tsserve.Status,
		discoverTunnels: gatherTunnels,
	})
}

// gatherSources is every live, environment-dependent read Gather makes, so
// gatherFrom can be driven entirely by fakes in tests: the port scanner (ss /
// lsof), the tailnet FQDN and serve status (tailscaled), and owned-tunnel
// discovery (cloudflared). Keeping ALL of them injectable is what lets a
// Gather test honour the clean-runner contract in .github/workflows/ci.yml --
// no ss, lsof, tailscaled or cloudflared required (roborev 5419).
type gatherSources struct {
	listPorts       func() ([]portscan.Port, error)
	fqdn            func() (string, error)
	tailscaleStatus func() ([]int, map[int]int, error)
	discoverTunnels func(config.Config) []Tunnel
}

// gatherFrom is Gather with every live, environment-dependent read injected
// through src (see gatherSources), so tests can exercise the tailscaled-down
// path deterministically, on any host. Production calls it only through
// Gather with the real functions.
//
// Tunnel discovery runs INDEPENDENTLY of tailscale (roborev 5416): a
// cloudflared tunnel tailport started is detached and stays public even when
// tailscaled is down, so a tailscale failure must not stop `tailport status`
// from reporting it. In that case gatherFrom still returns the tunnel rows
// alongside the error; runStatus prints them to stdout and the error to
// stderr, preserving the non-zero exit code.
func gatherFrom(cfg config.Config, src gatherSources) ([]Row, error) {
	ports, err := src.listPorts()
	if err != nil {
		return nil, fmt.Errorf("listing local ports: %w", err)
	}
	// Best-effort, mirroring ui.New/fetchFQDN: an unresolved hostname or FQDN
	// degrades the URL rather than failing the whole report.
	host, _ := os.Hostname()
	fqdn, _ := src.fqdn()
	tunnels := src.discoverTunnels(cfg)
	published := gatherPublished(cfg, fqdn)
	active, funnel, err := src.tailscaleStatus()
	if err != nil {
		// Tailscale state is unavailable, but the tunnels (and any published
		// rows the edge still knows about) are real exposures right now.
		// Report those rows and surface the tailscale failure alongside them.
		return Build(ports, nil, nil, published, tunnels, host, fqdn), fmt.Errorf("reading tailscale serve status: %w", err)
	}
	return Build(ports, active, funnel, published, tunnels, host, fqdn), nil
}

// gatherTunnels discovers this machine's tailport-OWNED Cloudflare tunnels, or
// nil when the feature is unavailable -- cloudflared not installed (Detect
// fails, exactly the gate internal/ui uses for its `o`/`O` keys) or a
// Discover error. Either way it degrades quietly to nil rather than failing
// Gather: a status report must still show serve/funnel/publish rows when
// cloudflared is missing or the process table can't be read.
//
// One Tunnel per owned process, not one per local port: two tailport
// instances racing can run two owned tunnels on the same port, and each is a
// distinct public URL (roborev 5416). Sorted by (port, hostname) so the
// report is deterministic regardless of process-table order.
//
// Read-only by construction: only Detect, Discover, and Health -- never
// Start/Stop. Foreign cloudflared processes (Owned false) are skipped: they
// are not a tailport exposure (internal/ui surfaces them as drift; a status
// report of tailport's own exposure has nothing to say about them).
func gatherTunnels(cfg config.Config) []Tunnel {
	client := &cftunnel.Client{Binary: cfg.Cloudflared.Binary}
	if _, err := client.Detect(); err != nil {
		return nil
	}
	running, err := client.Discover()
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), tunnelHealthTimeout)
	defer cancel()
	tunnels := make([]Tunnel, 0, len(running))
	for _, r := range running {
		if !r.Owned {
			continue
		}
		hostname := r.Hostname // named: recovered from the sentinel logfile
		if hostname == "" {
			hostname = client.Health(ctx, r.MetricsPort).Hostname // quick: from /quicktunnel
		}
		tunnels = append(tunnels, Tunnel{Port: r.Port, Hostname: hostname})
	}
	sort.Slice(tunnels, func(i, j int) bool {
		if tunnels[i].Port != tunnels[j].Port {
			return tunnels[i].Port < tunnels[j].Port
		}
		return tunnels[i].Hostname < tunnels[j].Hostname
	})
	return tunnels
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
	// short MagicDNS label) AND that tailport itself owns (r.Owned, i.e. the
	// @id carries the tailport- prefix -- see caddyedge.RouteInfo.Owned).
	// Published state is defined by tailport-owned routes (kata v1z5); a
	// route that merely targets this machine's label:port but was NOT
	// created by tailport (a hand-edited or foreign Caddy config) is not a
	// tailport publish and must not be reported as one -- doing so would be
	// a false publish/drift signal, not a real one.
	published := make(map[int]Published, len(routes))
	for _, r := range routes {
		if r.Label != label || !r.Owned {
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
// Gather so tests can drive it with fake ports/active/funnel/published/tunnel
// data without a live tailscaled, Caddy edge, or cloudflared (per this
// issue's verification bar).
//
// Only ports that are actually exposed -- present in active, funnel,
// published, or tunnels -- are included; a merely-listening, never-served
// port is out of scope for a status report (that's what the TUI's full port
// list is for). The exposed set deliberately includes a public path on its
// own (not gated behind also being in active): a published-but-not-served
// port is a dangling forward -- Caddy is still proxying to it, so a public
// visitor gets a 502 -- and that drift must stay visible, not disappear
// because the local backend went away. The same holds for a tunnelled port
// whose local backend went away.
//
// The three public paths -- funnel, publish, tunnel -- are INDEPENDENT, not
// alternatives: since kata th05 the UI permits any combination of them on one
// port, so Build emits one row per path a port actually carries (in funnel,
// publish, tunnel order). A tunnel is emitted per TUNNEL, not per port: two
// owned tunnels on the same local port (two tailport instances racing) are
// two distinct public URLs and each gets its own row (roborev 5416). Plain
// serve is emitted only when the port carries no public path at all: any
// public exposure outranks serve for display (mirroring portItem.reach() in
// internal/ui), since it is what actually governs reachability. Nothing is
// ranked above anything else; multiple public rows for one port are the
// expected, legitimate shape.
func Build(ports []portscan.Port, active []int, funnel map[int]int, published map[int]Published, tunnels []Tunnel, host, fqdn string) []Row {
	processByPort := make(map[int]string, len(ports))
	for _, p := range ports {
		processByPort[p.Number] = p.Process
	}

	tunnelsByPort := make(map[int][]Tunnel, len(tunnels))
	for _, t := range tunnels {
		tunnelsByPort[t.Port] = append(tunnelsByPort[t.Port], t)
	}

	exposed := make(map[int]bool, len(active)+len(funnel)+len(published)+len(tunnelsByPort))
	for _, p := range active {
		exposed[p] = true
	}
	for p := range funnel {
		exposed[p] = true
	}
	for p := range published {
		exposed[p] = true
	}
	for p := range tunnelsByPort {
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
		portTunnels := tunnelsByPort[n]

		if isFunnel {
			rows = append(rows, Row{Port: n, Process: process, Mode: ModeFunnel, URL: tsserve.PublicURL(fqdn, pubPort)})
		}
		if isPublished {
			rows = append(rows, Row{Port: n, Process: process, Mode: ModePublish, URL: publishURL(pub), Auth: pub.Auth})
		}
		for _, tun := range portTunnels {
			rows = append(rows, Row{Port: n, Process: process, Mode: ModeTunnel, URL: tunnelURL(tun)})
		}
		if !isFunnel && !isPublished && len(portTunnels) == 0 {
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

// tunnelURL builds the public HTTPS URL a Cloudflare tunnel is reachable at,
// or "" while a quick tunnel's hostname is still unknown (cloudflared assigns
// it only after connecting -- see Tunnel.Hostname). Like a publish, a tunnel
// is terminated on 443, so no port ever appears.
func tunnelURL(t Tunnel) string {
	if t.Hostname == "" {
		return ""
	}
	return "https://" + t.Hostname
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

// modeServeText, modeFunnelText, modePublishText, and modeTunnelText are the
// plain-text mode labels. Serve is lowercase and parenthetically scoped;
// funnel, publish, and tunnel are all upper-case and say "public" outright,
// so all four remain visually distinct even with color disabled (--no-color,
// NO_COLOR, or a non-TTY pipe) -- the case difference alone is legible in any
// terminal, and the scoped parents ("public", "edge, public", "cloudflare,
// public") keep the three public labels textually unambiguous from each
// other too.
const (
	modeServeText   = "serve (tailnet)"
	modeFunnelText  = "FUNNEL (public)"
	modePublishText = "PUBLISH (edge, public)"
	modeTunnelText  = "TUNNEL (cloudflare, public)"
)

// pendingHostnameNote is what WriteTable shows in a tunnel row's URL cell
// while the hostname isn't known yet (a quick tunnel that hasn't connected).
// WriteJSON keeps the URL empty in that case -- the machine-readable schema
// stays a plain URL-or-empty, never prose -- so the note is display-only.
const pendingHostnameNote = "(hostname pending)"

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
		fmt.Fprintln(w, "No ports are currently served, funnelled, published, or tunnelled.")
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
		case ModeTunnel:
			mode = modeTunnelText
		}
		process := r.Process
		if process == "" {
			process = "?"
		}
		url := r.URL
		if url == "" && r.Mode == ModeTunnel {
			url = pendingHostnameNote
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", mode, r.Port, process, url)
	}
	tw.Flush()

	out := buf.String()
	// funnelStyle.Render is a no-op (returns its input verbatim) under the
	// Ascii color profile, so this substitution is itself a no-op with color
	// disabled -- see the doc comment above. modePublishText and modeTunnelText
	// get the SAME (funnel-style) treatment: all three are public-internet
	// exposure and should draw the eye the same way; the text itself
	// ("FUNNEL" vs "PUBLISH" vs "TUNNEL") already keeps them distinguishable.
	out = strings.ReplaceAll(out, modeFunnelText, funnelStyle.Render(modeFunnelText))
	out = strings.ReplaceAll(out, modePublishText, funnelStyle.Render(modePublishText))
	out = strings.ReplaceAll(out, modeTunnelText, funnelStyle.Render(modeTunnelText))
	fmt.Fprint(w, out)
}
