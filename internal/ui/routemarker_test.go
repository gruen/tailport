package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestRouteMarker checks route.marker() against the design's per-route
// glyph table (docs/tmp/th05-multiroute-design-DRAFT.md §1): the right glyph
// shows up (post stripANSI, since color/bold are lipgloss styling we don't
// re-derive here) and the rendered marker is always exactly 2 cells wide, in
// both mono and emoji modes, so the marker column never shifts between the
// two.
func TestRouteMarker(t *testing.T) {
	tests := []struct {
		name       string
		r          route
		monoGlyph  string
		emojiGlyph string
	}{
		{
			name:       "localhost",
			r:          route{kind: routeLocalhost},
			monoGlyph:  "○",
			emojiGlyph: "🌕",
		},
		{
			name:       "LAN",
			r:          route{kind: routeLAN},
			monoGlyph:  "◔",
			emojiGlyph: "🌔",
		},
		{
			name:       "tailnet wide bind (not served)",
			r:          route{kind: routeTailnet, served: false},
			monoGlyph:  "◑",
			emojiGlyph: "🌓",
		},
		{
			name:       "tailnet served",
			r:          route{kind: routeTailnet, served: true},
			monoGlyph:  "◉",
			emojiGlyph: "🌒",
		},
		{
			name:       "funnel",
			r:          route{kind: routeFunnel},
			monoGlyph:  "●",
			emojiGlyph: "🌑",
		},
		{
			name:       "publish",
			r:          route{kind: routePublish},
			monoGlyph:  "◆",
			emojiGlyph: "🌐",
		},
		{
			name:       "tunnel",
			r:          route{kind: routeTunnel},
			monoGlyph:  "◈",
			emojiGlyph: "☁️",
		},
		{
			name:       "offline",
			r:          route{kind: routeOffline},
			monoGlyph:  "✕",
			emojiGlyph: "✕",
		},
		{
			name:       "stale tailnet (served but nothing listening)",
			r:          route{kind: routeTailnet, served: true, stale: true},
			monoGlyph:  "▲",
			emojiGlyph: "🌫️",
		},
		{
			name:       "stale tailnet (wide bind flavor, still forced stale)",
			r:          route{kind: routeTailnet, served: false, stale: true},
			monoGlyph:  "▲",
			emojiGlyph: "🌫️",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/mono", func(t *testing.T) {
			got := tt.r.marker(false)
			plain := stripANSI(got)
			if !strings.Contains(plain, tt.monoGlyph) {
				t.Errorf("marker(false) = %q (plain %q), want it to contain %q", got, plain, tt.monoGlyph)
			}
			if w := lipgloss.Width(got); w != 2 {
				t.Errorf("lipgloss.Width(marker(false)) = %d, want 2 (marker=%q)", w, got)
			}
		})
		t.Run(tt.name+"/emoji", func(t *testing.T) {
			got := tt.r.marker(true)
			plain := stripANSI(got)
			if !strings.Contains(plain, tt.emojiGlyph) {
				t.Errorf("marker(true) = %q (plain %q), want it to contain %q", got, plain, tt.emojiGlyph)
			}
			if w := lipgloss.Width(got); w != 2 {
				t.Errorf("lipgloss.Width(marker(true)) = %d, want 2 (marker=%q)", w, got)
			}
		})
	}
}

// TestRouteMarkerStaleOverridesKind pins down that r.stale forces the
// ▲/🌫️ marker regardless of the route's kind or served flag -- stale is only
// ever set on a tailnet route in practice (route.go), but marker() checks it
// first rather than assuming that invariant holds.
func TestRouteMarkerStaleOverridesKind(t *testing.T) {
	served := route{kind: routeTailnet, served: true, stale: true}
	wide := route{kind: routeTailnet, served: false, stale: true}

	for _, r := range []route{served, wide} {
		mono := stripANSI(r.marker(false))
		if !strings.Contains(mono, "▲") {
			t.Errorf("stale route marker(false) = %q, want it to contain ▲", mono)
		}
		if strings.Contains(mono, "◉") || strings.Contains(mono, "◑") {
			t.Errorf("stale route marker(false) = %q, should not contain the non-stale tailnet glyph", mono)
		}

		emoji := stripANSI(r.marker(true))
		if !strings.Contains(emoji, "🌫️") {
			t.Errorf("stale route marker(true) = %q, want it to contain 🌫️", emoji)
		}
		if strings.Contains(emoji, "🌒") || strings.Contains(emoji, "🌓") {
			t.Errorf("stale route marker(true) = %q, should not contain the non-stale tailnet glyph", emoji)
		}
	}
}
