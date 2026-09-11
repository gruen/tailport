package ui

import "github.com/charmbracelet/lipgloss"

// Per-route marker styles (kata th05, P2). These pair with the existing
// aggregate-marker styles declared near the top of ui.go (activeStyle,
// warnStyle, publicStyle, publishMarkerStyle, tunnelMarkerStyle) -- reused
// here rather than redefined -- plus two new styles for pairs that don't
// already exist as package-level vars: the tailnet green (pinned to the exact
// {L:#006644, D:42} pair from the design, matching helpTitleStyle rather than
// assuming activeStyle is that same pair) and the muted gray shared by
// localhost/offline.
var (
	// tailnetWideMarkerStyle is the green used for BOTH tailnet marker tiers
	// (◑ wide-bind, ◉ served) -- the design (§1) pins this exact adaptive pair;
	// ◉ additionally bolds via .Bold(true) at the call site so served reads as
	// the "stronger" of the two tiers.
	tailnetWideMarkerStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#006644", Dark: "42"})
	// routeMutedStyle is the muted gray for the localhost and offline markers
	// (○ and ✕) -- least/no exposure, so deliberately unobtrusive.
	routeMutedStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#4b4b4b", Dark: "245"})
)

// marker returns r's per-route marker glyph, styled and padded to a STABLE
// 2-cell width so the marker column never shifts between mono and emoji
// modes (mirrors portItem.markerGlyph()'s mechanics in ui.go). Mono glyphs
// are naturally 1-cell, so a trailing space pads them to 2; emoji glyphs
// (e.g. ✕, ☁️, 🌫️) can themselves render narrower than 2 cells on some
// terminals, so the same trailing-pad loop applies there too.
//
// r.stale or r.foreign is checked FIRST: either overrides the kind-based
// glyph/color with the same amber marker (▲/🌫️) regardless of kind -- stale is
// only ever set on a tailnet or tunnel route, foreign only on a tunnel route,
// but the override lives ahead of the switch so those invariants don't have
// to be re-verified here. The palette doesn't otherwise distinguish "ours but
// not functionally live" from "not ours at all" by glyph -- both read as
// "something here needs your attention" -- the adornment text
// (routeAdornments) carries the distinction (kata aprt).
func (r route) marker(emoji bool) string {
	var m string
	switch {
	case r.stale || r.foreign:
		// Dangling forward (served but nothing listening) or a drifted
		// foreign tunnel. Off the moon ramp, same treatment as ui.go's
		// reachStale.
		if emoji {
			m = "🌫️"
		} else {
			m = warnStyle.Render("▲")
		}
	case r.kind == routeLocalhost:
		if emoji {
			m = "🌕"
		} else {
			m = routeMutedStyle.Render("○")
		}
	case r.kind == routeLAN:
		// No color styling: bare glyph, default foreground.
		if emoji {
			m = "🌔"
		} else {
			m = "◔"
		}
	case r.kind == routeTailnet && r.served:
		if emoji {
			m = "🌒"
		} else {
			m = tailnetWideMarkerStyle.Bold(true).Render("◉")
		}
	case r.kind == routeTailnet && !r.served:
		if emoji {
			m = "🌓"
		} else {
			m = tailnetWideMarkerStyle.Render("◑")
		}
	case r.kind == routeFunnel:
		if emoji {
			m = "🌑"
		} else {
			m = publicStyle.Render("●")
		}
	case r.kind == routePublish:
		if emoji {
			m = "🌐"
		} else {
			m = publishMarkerStyle.Render("◆")
		}
	case r.kind == routeTunnel:
		if emoji {
			m = "☁️"
		} else {
			m = tunnelMarkerStyle.Render("◈")
		}
	case r.kind == routeOffline:
		if emoji {
			m = "✕"
		} else {
			m = routeMutedStyle.Render("✕")
		}
	}
	for lipgloss.Width(m) < 2 {
		m += " "
	}
	return m
}
