package ui

// Service block rendering (kata th05, P3a): the PURE renderer that turns one
// service's resolved display state + its routesFor() output into the styled
// lines bubbles ultimately puts on screen -- a header line ("record" line:
// port/name/★/🔒) followed by one line per route (marker/label/url/
// adornments). No bubbles wiring, no model access, no I/O: renderServiceBlock
// takes a blockInput value and returns []string. See
// docs/tmp/th05-multiroute-design-DRAFT.md §2 (row anatomy), §3 (mockups),
// and §4 (selection) for the design this implements.
//
// Column grid (display cells; concatenation order matters -- see each field's
// comment below for its exact width):
//
//	HEADER  <bar><badge><sp><:PORT:6><2sp><name>[<sp>🔒]
//	ROUTE   <bar><2sp indent><pointer><sp><marker:2><sp><label:10><2sp><url><adornments>
//
// The marker glyph (route.marker) already encodes served/stale/kind color and
// is a stable 2 cells wide in both mono and emoji modes -- it is NEVER
// restyled here. Selection (§4) touches only the bar, the pointer, and the
// selected route's url; everything else keeps its own treatment, with one
// deliberate exception the design calls "quiet" routes: the localhost and
// offline route lines render their label (and, when not the selected route,
// their url) muted, regardless of selection.

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// routeSelURLStyle is the SELECTED route's url: bold + underlined + the same
// accent teal as logoStyle/the record bar (design §4, "three redundant
// channels"). Deliberately a fresh var rather than logoStyle.Underline(true)
// -- logoStyle is a shared package var used elsewhere unstyled-underline, and
// mutating a shared *lipgloss.Style-returning call every render would be
// wasteful; this is a one-time package-level definition instead.
var routeSelURLStyle = lipgloss.NewStyle().
	Foreground(lipgloss.AdaptiveColor{Light: "#005f5f", Dark: "51"}).
	Bold(true).
	Underline(true)

// blockInput is the resolved, pure input to renderServiceBlock: everything
// about one service's display already decided by the caller (name
// resolution, which route is selected/just-copied, terminal width) so this
// file stays free of model/registry/bubbles concerns.
type blockInput struct {
	port    int
	name    string // already-resolved display name (label | process | "was <proc>" | "?"); caller resolves
	nameWas bool   // true when name is a muted "was <proc>" form -> render with wasStyle
	pid     int    // listening process's PID; 0 means unknown -> renders nothing

	favorite bool
	locked   bool

	routes []route // from routesFor; always >=1

	current       bool // this is the service the cursor is on -> draw the teal record bar on every line
	selectedRoute int  // index into routes of the selected route (only meaningful when current); -1 otherwise
	copiedRoute   int  // index of a route currently showing "✓ copied", or -1

	emoji bool
	width int // total available width (for URL truncation); <=0 means "don't truncate"
}

// renderServiceBlock returns the styled lines for one service: a header line
// followed by one line per route. The caller stacks blocks (blank line
// between); this function never adds blank lines itself.
func renderServiceBlock(b blockInput) []string {
	lines := make([]string, 0, 1+len(b.routes))
	lines = append(lines, renderServiceHeader(b))
	for i, r := range b.routes {
		lines = append(lines, renderRouteLine(b, r, i))
	}
	return lines
}

// renderServiceHeader builds the HEADER line: <bar><badge><sp><:PORT:6><2sp><name>[<sp>🔒]
func renderServiceHeader(b blockInput) string {
	bar := " "
	if b.current {
		bar = logoStyle.Render("▎")
	}

	badge := " "
	if b.favorite {
		badge = favStyle.Render("★")
	}

	portField := padCells(fmt.Sprintf(":%d", b.port), 6)
	if b.current {
		// Bold + teal-tinted (logoStyle is exactly that pair) when this is
		// the current service.
		portField = logoStyle.Render(portField)
	} else {
		portField = lipgloss.NewStyle().Bold(true).Render(portField)
	}

	name := b.name
	if b.nameWas {
		name = wasStyle.Render(name)
	}

	header := bar + badge + " " + portField + "  " + name
	if b.locked {
		header += " " + lockStyle.Render("🔒")
	}
	if b.pid > 0 {
		header += "    " + routeMutedStyle.Render(fmt.Sprintf("pid:%d", b.pid))
	}
	return header
}

// renderRouteLine builds one ROUTE line for routes[i]:
// <bar><2sp indent><pointer><sp><marker:2><sp><label:10><2sp><url><adornments>
func renderRouteLine(b blockInput, r route, i int) string {
	bar := " "
	if b.current {
		bar = logoStyle.Render("▎")
	}

	selected := b.current && i == b.selectedRoute
	pointer := " "
	if selected {
		pointer = logoStyle.Render("▸")
	}

	// route.marker already returns a styled, stable-2-cell glyph -- never
	// restyled here (safety colors must survive selection untouched).
	marker := r.marker(b.emoji)

	label := padCells(r.label(), 10)
	muted := r.kind == routeLocalhost || r.kind == routeOffline
	if muted {
		// "Quiet" routes (design §6): localhost/offline read muted even when
		// this is the current/selected service -- the label is exempt from
		// the "selection only touches bar/pointer/url" rule.
		label = routeMutedStyle.Render(label)
	}

	prefix := bar + "  " + pointer + " " + marker + " " + label + "  "

	urlText := r.url
	if r.kind == routeOffline {
		// Offline is a pseudo-route with no address; the design's em-dash
		// placeholder stands in for it.
		urlText = "—"
	}

	adorn := routeAdornments(b, r, i)

	if b.width > 0 {
		avail := b.width - lipgloss.Width(prefix) - lipgloss.Width(adorn)
		if avail < 1 {
			avail = 1
		}
		urlText = truncateCells(urlText, avail)
	}

	var styledURL string
	switch {
	case selected:
		styledURL = routeSelURLStyle.Render(urlText)
	case muted:
		// Not selected: routeLocalhost and routeOffline both render muted
		// (offline's "—" included).
		styledURL = routeMutedStyle.Render(urlText)
	default:
		styledURL = urlText
	}

	return prefix + styledURL + adorn
}

// routeAdornments builds the trailing, 2-space-separated adornment sequence
// for routes[i]: auth glyph, then "· stale", then the copied confirmation --
// in that fixed order (design §2). Each present adornment is preceded by its
// own 2 spaces, except copiedSuffix, which already carries its own leading
// "  " (see its doc comment in ui.go) and is appended as-is.
func routeAdornments(b blockInput, r route, i int) string {
	var out strings.Builder
	if r.kind == routePublish && r.auth {
		glyph := authGlyphMono
		if b.emoji {
			glyph = authGlyphEmoji
		}
		out.WriteString("  " + publishMarkerStyle.Render(glyph))
	}
	if r.stale {
		out.WriteString("  " + warnStyle.Render("· stale"))
	}
	if i == b.copiedRoute {
		out.WriteString(activeStyle.Render(copiedSuffix))
	}
	return out.String()
}

// padCells left-justifies s to width display cells with plain spaces,
// measuring with lipgloss.Width rather than len/rune-count so it's correct
// even if s carries multi-cell runes. s wider than width is returned
// unchanged (never truncated here -- only route urls are ever truncated, via
// truncateCells).
func padCells(s string, width int) string {
	w := lipgloss.Width(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

// truncateCells returns s unchanged if it already fits within avail display
// cells, else trims runes off the end and appends a trailing "…" so the
// result (ellipsis included) fits within avail. avail<=0 degrades to just
// "…" -- callers needing "never truncate" should not call this (routerender
// only calls it when b.width>0).
func truncateCells(s string, avail int) string {
	if lipgloss.Width(s) <= avail {
		return s
	}
	runes := []rune(s)
	for len(runes) > 0 && lipgloss.Width(string(runes)+"…") > avail {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}
