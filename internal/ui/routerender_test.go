package ui

import (
	"strings"
	"testing"
)

// blockLines renders b and returns its lines stripped of ANSI styling, so
// tests can assert on the visible structure (columns/labels/urls/markers)
// without re-deriving lipgloss's exact escape sequences -- same approach
// TestRouteMarker takes for route.marker() in routemarker_test.go.
func blockLines(t *testing.T, b blockInput) []string {
	t.Helper()
	rendered := renderServiceBlock(b)
	lines := make([]string, len(rendered))
	for i, l := range rendered {
		lines[i] = stripANSI(l)
	}
	return lines
}

// TestRenderServiceBlockLocalhostOnly covers the quiet, common case (design
// §6): a not-current, localhost-only service is a 2-line block (header +
// one route) with no record bar and no selection pointer anywhere in it.
func TestRenderServiceBlockLocalhostOnly(t *testing.T) {
	b := blockInput{
		port: 5173,
		name: "astro",
		routes: []route{
			{kind: routeLocalhost, url: "http://localhost:5173"},
		},
		current:       false,
		selectedRoute: -1,
		copiedRoute:   -1,
	}

	lines := blockLines(t, b)
	if len(lines) != 2 {
		t.Fatalf("len(lines) = %d, want 2 (header + 1 route); lines=%q", len(lines), lines)
	}

	header := lines[0]
	if !strings.Contains(header, ":5173") {
		t.Errorf("header = %q, want it to contain %q", header, ":5173")
	}
	if !strings.Contains(header, "astro") {
		t.Errorf("header = %q, want it to contain %q", header, "astro")
	}

	route := lines[1]
	if !strings.Contains(route, "○") {
		t.Errorf("route line = %q, want it to contain the localhost marker %q", route, "○")
	}
	if !strings.Contains(route, "localhost") {
		t.Errorf("route line = %q, want it to contain the label %q", route, "localhost")
	}
	if !strings.Contains(route, "http://localhost:5173") {
		t.Errorf("route line = %q, want it to contain the url", route)
	}

	whole := strings.Join(lines, "\n")
	if strings.Contains(whole, "▎") {
		t.Errorf("block = %q, not current -> should not contain the record bar ▎", whole)
	}
	if strings.Contains(whole, "▸") {
		t.Errorf("block = %q, not current -> should not contain the selection pointer ▸", whole)
	}
}

// TestRenderServiceBlockFullyExposed covers a service with all six route
// kinds represented (minus LAN, which routesFor never emits alongside a
// wildcard bind -- see route.go), current with its ts.net route selected: 6
// lines, the record bar on every line, the pointer on the selected route's
// line only, and the auth glyph on the published (caddy) route's line.
func TestRenderServiceBlockFullyExposed(t *testing.T) {
	routes := []route{
		{kind: routeLocalhost, url: "http://localhost:3000"},               // 0
		{kind: routeTailnet, served: true, url: "http://myhost:3000"},      // 1
		{kind: routeFunnel, url: "https://myhost.tail1234.ts.net"},         // 2 - selected
		{kind: routePublish, auth: true, url: "https://app.example.com"},   // 3
		{kind: routeTunnel, url: "https://witty-fox-42.trycloudflare.com"}, // 4
	}
	b := blockInput{
		port:          3000,
		name:          "vite",
		routes:        routes,
		current:       true,
		selectedRoute: 2,
		copiedRoute:   -1,
	}

	lines := blockLines(t, b)
	if len(lines) != 6 {
		t.Fatalf("len(lines) = %d, want 6 (header + 5 routes); lines=%q", len(lines), lines)
	}

	header := lines[0]
	if !strings.Contains(header, "▎") {
		t.Errorf("header = %q, current service -> want it to contain the record bar ▎", header)
	}
	if !strings.Contains(header, ":3000") {
		t.Errorf("header = %q, want it to contain %q", header, ":3000")
	}
	if !strings.Contains(header, "vite") {
		t.Errorf("header = %q, want it to contain %q", header, "vite")
	}

	tsNetLine := lines[1+2] // routes[2]
	if !strings.Contains(tsNetLine, "▸") {
		t.Errorf("ts.net line = %q, selected -> want it to contain the pointer ▸", tsNetLine)
	}
	if !strings.Contains(tsNetLine, "ts.net") {
		t.Errorf("ts.net line = %q, want it to contain the label %q", tsNetLine, "ts.net")
	}

	caddyLine := lines[1+3] // routes[3], publish+auth
	if !strings.Contains(caddyLine, authGlyphMono) {
		t.Errorf("caddy line = %q, want it to contain the auth glyph %q", caddyLine, authGlyphMono)
	}
	if !strings.Contains(caddyLine, "caddy") {
		t.Errorf("caddy line = %q, want it to contain the label %q", caddyLine, "caddy")
	}

	for i, l := range lines {
		if !strings.HasPrefix(l, "▎") {
			t.Errorf("line %d = %q, current service -> every line should start with the record bar ▎", i, l)
		}
	}

	// Only the selected route's line carries the pointer.
	for i, l := range lines {
		wantPointer := i == 1+2
		gotPointer := strings.Contains(l, "▸")
		if gotPointer != wantPointer {
			t.Errorf("line %d = %q, pointer present = %v, want %v", i, l, gotPointer, wantPointer)
		}
	}
}

// TestRenderServiceBlockFavoriteLocked covers the header-only ★/🔒 badges.
func TestRenderServiceBlockFavoriteLocked(t *testing.T) {
	b := blockInput{
		port:     22,
		name:     "sshd",
		favorite: true,
		locked:   true,
		routes: []route{
			{kind: routeLocalhost, url: "ssh localhost"},
		},
		current:       false,
		selectedRoute: -1,
		copiedRoute:   -1,
	}

	lines := blockLines(t, b)
	header := lines[0]
	if !strings.Contains(header, "★") {
		t.Errorf("header = %q, favorite -> want it to contain ★", header)
	}
	if !strings.Contains(header, "🔒") {
		t.Errorf("header = %q, locked -> want it to contain 🔒", header)
	}
}

// TestRenderServiceBlockStaleTailnet covers a dangling-forward tailnet route:
// its marker is the stale ▲ glyph (via route.marker, not restyled here) and
// its line carries the "· stale" adornment.
func TestRenderServiceBlockStaleTailnet(t *testing.T) {
	b := blockInput{
		port: 8025,
		name: "was mailpit",
		routes: []route{
			{kind: routeTailnet, served: true, stale: true, url: "http://myhost:8025"},
		},
		current:       false,
		selectedRoute: -1,
		copiedRoute:   -1,
	}

	lines := blockLines(t, b)
	if len(lines) != 2 {
		t.Fatalf("len(lines) = %d, want 2 (header + 1 route); lines=%q", len(lines), lines)
	}
	route := lines[1]
	if !strings.Contains(route, "▲") {
		t.Errorf("route line = %q, stale -> want it to contain the ▲ marker", route)
	}
	if !strings.Contains(route, "· stale") {
		t.Errorf("route line = %q, stale -> want it to contain the %q adornment", route, "· stale")
	}
}

// TestRenderServiceBlockOffline covers the down-favorite pseudo-route: it
// labels "offline" and its url column shows the "—" placeholder (routesFor
// never gives an offline route a real url).
func TestRenderServiceBlockOffline(t *testing.T) {
	b := blockInput{
		port:     6379,
		name:     "was redis-server",
		nameWas:  true,
		favorite: true,
		routes: []route{
			{kind: routeOffline, url: ""},
		},
		current:       false,
		selectedRoute: -1,
		copiedRoute:   -1,
	}

	lines := blockLines(t, b)
	if len(lines) != 2 {
		t.Fatalf("len(lines) = %d, want 2 (header + 1 route); lines=%q", len(lines), lines)
	}
	route := lines[1]
	if !strings.Contains(route, "offline") {
		t.Errorf("route line = %q, want it to contain the label %q", route, "offline")
	}
	if !strings.Contains(route, "—") {
		t.Errorf("route line = %q, offline -> want it to contain the %q placeholder", route, "—")
	}
}

// TestRenderServiceBlockCopiedRoute covers the transient "✓ copied"
// confirmation: only the route at b.copiedRoute carries it.
func TestRenderServiceBlockCopiedRoute(t *testing.T) {
	routes := []route{
		{kind: routeLocalhost, url: "http://localhost:3000"},
		{kind: routeTailnet, served: true, url: "http://myhost:3000"},
	}
	b := blockInput{
		port:          3000,
		name:          "vite",
		routes:        routes,
		current:       true,
		selectedRoute: 1,
		copiedRoute:   1,
	}

	lines := blockLines(t, b)
	if len(lines) != 3 {
		t.Fatalf("len(lines) = %d, want 3 (header + 2 routes); lines=%q", len(lines), lines)
	}
	if strings.Contains(lines[1], "✓ copied") {
		t.Errorf("localhost line = %q, not the copied route -> should not contain the copied suffix", lines[1])
	}
	if !strings.Contains(lines[2], "✓ copied") {
		t.Errorf("tailnet line = %q, copiedRoute=1 -> want it to contain the copied suffix", lines[2])
	}
}

// TestRenderServiceBlockNonEmptyAndMarkerColumn is a light sanity sweep
// across the cases above: no rendered line is empty, and every route line
// carries a non-blank marker glyph from the route's kind-specific set.
func TestRenderServiceBlockNonEmptyAndMarkerColumn(t *testing.T) {
	blocks := []blockInput{
		{
			port: 5173, name: "astro",
			routes:        []route{{kind: routeLocalhost, url: "http://localhost:5173"}},
			selectedRoute: -1, copiedRoute: -1,
		},
		{
			port: 8025, name: "was mailpit", nameWas: true,
			routes:        []route{{kind: routeTailnet, served: true, stale: true, url: "http://myhost:8025"}},
			selectedRoute: -1, copiedRoute: -1,
		},
		{
			port: 6379, name: "was redis-server", nameWas: true, favorite: true,
			routes:        []route{{kind: routeOffline}},
			selectedRoute: -1, copiedRoute: -1,
		},
	}

	markerGlyphs := []string{"○", "▲", "✕"}
	for bi, b := range blocks {
		lines := blockLines(t, b)
		for li, l := range lines {
			if strings.TrimSpace(l) == "" {
				t.Errorf("block %d line %d is empty", bi, li)
			}
		}
		route := lines[1]
		if !strings.Contains(route, markerGlyphs[bi]) {
			t.Errorf("block %d route line = %q, want it to contain the marker %q", bi, route, markerGlyphs[bi])
		}
	}
}

// TestRenderServiceHeaderPid covers the PID display (kata 6x92): a known PID
// renders as a muted "pid:<N>" after the name, while an unknown PID (0, the
// zero value -- e.g. a process owned by another user, or a down favorite)
// renders nothing at all.
func TestRenderServiceHeaderPid(t *testing.T) {
	base := blockInput{
		port:          8888,
		name:          "labelname",
		routes:        []route{{kind: routeLocalhost, url: "http://localhost:8888"}},
		selectedRoute: -1,
		copiedRoute:   -1,
	}

	withPid := base
	withPid.pid = 9999
	lines := blockLines(t, withPid)
	header := lines[0]
	if !strings.Contains(header, "pid:9999") {
		t.Errorf("header = %q, want it to contain %q", header, "pid:9999")
	}

	noPid := base
	lines = blockLines(t, noPid)
	header = lines[0]
	if strings.Contains(header, "pid:") {
		t.Errorf("header = %q, pid == 0 -> should not contain %q", header, "pid:")
	}
}
