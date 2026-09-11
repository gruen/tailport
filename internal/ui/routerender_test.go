package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
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

// TestRenderServiceHeaderOverflowTruncates covers the z6yf bug: nothing in
// renderServiceHeader was ever measured against b.width (only route URLs
// were, via truncateCells), so a long user LABEL on a narrow terminal
// produced a header wider than the terminal -- which the terminal itself
// then soft-wraps. renderList slices by LOGICAL lines and View sizes its gap
// from lipgloss.Height(body) (a logical-newline count), so that extra visual
// row went uncounted and the bottom bar/scroll indicator drifted. The fix
// must (1) never let the header exceed b.width, (2) always keep :PORT, and
// (3) prefer dropping the trailing pid:NNNN before ever truncating the name.
func TestRenderServiceHeaderOverflowTruncates(t *testing.T) {
	longName := strings.Repeat("a-very-long-user-label", 3) // 66 chars

	// At a moderately narrow width, dropping the pid alone frees enough room
	// -- the name itself is left untouched.
	b := blockInput{
		port: 8080, name: longName, pid: 12345, width: 80,
		routes:        []route{{kind: routeLocalhost, url: "http://localhost:8080"}},
		selectedRoute: -1, copiedRoute: -1,
	}
	header := renderServiceHeader(b)
	if w := lipgloss.Width(header); w > b.width {
		t.Fatalf("header width %d exceeds b.width %d: %q", w, b.width, stripANSI(header))
	}
	plain := stripANSI(header)
	if !strings.Contains(plain, ":8080") {
		t.Errorf("header = %q, want it to still contain %q", plain, ":8080")
	}
	if !strings.Contains(plain, longName) {
		t.Errorf("header = %q, want the full name kept once dropping the pid frees enough room", plain)
	}
	if strings.Contains(plain, "pid:") {
		t.Errorf("header = %q, want the pid dropped once it no longer fits", plain)
	}

	// At a much narrower width, dropping the pid isn't enough either -- the
	// name itself must be truncated with an ellipsis, but :PORT survives.
	narrow := b
	narrow.width = 40
	header = renderServiceHeader(narrow)
	plain = stripANSI(header)
	if w := lipgloss.Width(header); w > narrow.width {
		t.Fatalf("narrow header width %d exceeds width %d: %q", w, narrow.width, plain)
	}
	if !strings.Contains(plain, ":8080") {
		t.Errorf("narrow header = %q, want it to still contain %q", plain, ":8080")
	}
	if strings.Contains(plain, "pid:") {
		t.Errorf("narrow header = %q, want the pid dropped", plain)
	}
	if strings.Contains(plain, longName) {
		t.Errorf("narrow header = %q, want the long name truncated, not kept in full", plain)
	}
	if !strings.Contains(plain, "…") {
		t.Errorf("narrow header = %q, want a truncated name to end in an ellipsis", plain)
	}

	// A locked record's 🔒 badge must survive truncation too (design: never
	// drop it, only the name/pid).
	locked := narrow
	locked.locked = true
	header = renderServiceHeader(locked)
	plain = stripANSI(header)
	if w := lipgloss.Width(header); w > locked.width {
		t.Errorf("locked narrow header width %d exceeds width %d: %q", w, locked.width, plain)
	}
	if !strings.Contains(plain, "🔒") {
		t.Errorf("locked narrow header = %q, want the lock badge kept", plain)
	}

	// Sweep a range of realistic widths: the header must never exceed the
	// terminal width, whatever the name length.
	for _, w := range []int{24, 30, 40, 60, 80, 120} {
		sweep := b
		sweep.width = w
		if got := lipgloss.Width(renderServiceHeader(sweep)); got > w {
			t.Errorf("width %d: header width %d overflows", w, got)
		}
	}
}

// TestRenderServiceBlockDimmed covers z6yf's restored dimming: the
// single-column renderer had no concept of portItem.dimmed at all (the field
// was only ever read by the retired grid's list delegate, which View() no
// longer calls), so a non-favorite match pulled into the Favorites view by an
// active "/" filter (4ye6) rendered exactly like a real favorite. Dimming
// must change the STYLING of the header name and any NON-quiet route (the
// tailnet route here) without changing the VISIBLE TEXT anywhere (stripped of
// ANSI, every line is identical). The localhost route is a "quiet" route
// (design §6) that's ALREADY routeMutedStyle regardless of dimming, so its
// line is expected to render byte-identical either way -- there's no lower
// style to drop to.
func TestRenderServiceBlockDimmed(t *testing.T) {
	// Forced so the ANSI comparisons below are deterministic regardless of
	// whether go test's stdout looks like a terminal at all -- routeMutedStyle
	// sets only a Foreground color (no Bold/Italic), which degrades to plain
	// text under the no-color/Ascii profile termenv falls back to for a
	// non-tty, making the dimmed and plain renders look byte-identical.
	origProfile := lipgloss.ColorProfile()
	t.Cleanup(func() { lipgloss.SetColorProfile(origProfile) })
	lipgloss.SetColorProfile(termenv.TrueColor)

	routes := []route{
		{kind: routeLocalhost, url: "http://localhost:8080"}, // quiet -- always muted
		{kind: routeTailnet, served: true, url: "http://host:8080"},
	}
	dimmed := blockInput{
		port: 8080, name: "webapp", dimmed: true,
		routes:        routes,
		selectedRoute: -1, copiedRoute: -1,
	}
	plainInput := dimmed
	plainInput.dimmed = false

	dimmedLines := renderServiceBlock(dimmed)
	plainLines := renderServiceBlock(plainInput)
	if len(dimmedLines) != 3 || len(plainLines) != 3 {
		t.Fatalf("len(dimmedLines)=%d len(plainLines)=%d, want 3 (header + 2 routes)", len(dimmedLines), len(plainLines))
	}

	// Header (name) and the tailnet route (not otherwise muted) must change
	// styling when dimmed, but never their visible text.
	for _, i := range []int{0, 2} {
		if dimmedLines[i] == plainLines[i] {
			t.Errorf("line %d identical dimmed vs not dimmed -- want dimming to change the styling: %q", i, stripANSI(dimmedLines[i]))
		}
		if stripANSI(dimmedLines[i]) != stripANSI(plainLines[i]) {
			t.Errorf("line %d visible text changed by dimming: dimmed=%q plain=%q", i, stripANSI(dimmedLines[i]), stripANSI(plainLines[i]))
		}
	}

	// The quiet localhost route is already muted regardless of dimming, so
	// its line is unaffected either way.
	if dimmedLines[1] != plainLines[1] {
		t.Errorf("quiet localhost line changed by dimming, want it unaffected: dimmed=%q plain=%q", stripANSI(dimmedLines[1]), stripANSI(plainLines[1]))
	}
}
