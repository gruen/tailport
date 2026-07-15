package ui

import (
	"bytes"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/gruen/tailport/internal/config"
)

// truecolorFgRE matches a 24-bit truecolor SGR foreground sequence
// (38;2;R;G;B) anywhere in a rendered string, capturing the three channels.
var truecolorFgRE = regexp.MustCompile(`38;2;(\d+);(\d+);(\d+)`)

// parseTruecolorFg extracts the first 38;2;R;G;B truecolor foreground
// sequence from a rendered string, if any.
func parseTruecolorFg(s string) (r, g, b int, ok bool) {
	m := truecolorFgRE.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, 0, false
	}
	r, _ = strconv.Atoi(m[1])
	g, _ = strconv.Atoi(m[2])
	b, _ = strconv.Atoi(m[3])
	return r, g, b, true
}

func absDiff(a, b int) int {
	if a < b {
		return b - a
	}
	return a - b
}

// ansiHexByIndex maps the ANSI-256 palette indices used as this package's
// must-fix AdaptiveColor.Dark values (see ui.go's style-color comment block,
// kata n7gc) to their RGB hex equivalents. These are extracted directly from
// this repository's pinned dependency (github.com/muesli/termenv v0.16.0,
// ansicolors.go's unexported ansiHex table) rather than typed from memory --
// see the kata n7gc close message for the extraction -- so
// TestAdaptiveColorContrast can compute a real WCAG contrast ratio for the
// Dark side without depending on any particular terminal's color-profile
// detection at test time.
var ansiHexByIndex = map[string]string{
	"42":  "#00d787",
	"51":  "#00ffff",
	"81":  "#5fd7ff",
	"201": "#ff00ff",
	"214": "#ffaf00",
	"220": "#ffd700",
	"245": "#8a8a8a",
	"252": "#d0d0d0",
	"255": "#eeeeee",
}

// srgbToLinear, relativeLuminance, and contrastRatio implement the WCAG 2.x
// contrast formula (https://www.w3.org/TR/WCAG21/#dfn-contrast-ratio):
// per-channel relative luminance under the sRGB gamma curve, combined via
// ITU-R BT.709 weights, then (L1+0.05)/(L2+0.05) with L1 the lighter of the
// two colors.
func srgbToLinear(c float64) float64 {
	if c <= 0.03928 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

func hexToRGB(hex string) (r, g, b int, err error) {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) != 6 {
		return 0, 0, 0, fmt.Errorf("not a 6-digit hex color: %q", hex)
	}
	rr, err := strconv.ParseInt(hex[0:2], 16, 0)
	if err != nil {
		return 0, 0, 0, err
	}
	gg, err := strconv.ParseInt(hex[2:4], 16, 0)
	if err != nil {
		return 0, 0, 0, err
	}
	bb, err := strconv.ParseInt(hex[4:6], 16, 0)
	if err != nil {
		return 0, 0, 0, err
	}
	return int(rr), int(gg), int(bb), nil
}

func relativeLuminance(hex string) (float64, error) {
	r, g, b, err := hexToRGB(hex)
	if err != nil {
		return 0, err
	}
	return 0.2126*srgbToLinear(float64(r)/255) +
		0.7152*srgbToLinear(float64(g)/255) +
		0.0722*srgbToLinear(float64(b)/255), nil
}

func contrastRatio(hex1, hex2 string) (float64, error) {
	l1, err := relativeLuminance(hex1)
	if err != nil {
		return 0, err
	}
	l2, err := relativeLuminance(hex2)
	if err != nil {
		return 0, err
	}
	if l1 < l2 {
		l1, l2 = l2, l1
	}
	return (l1 + 0.05) / (l2 + 0.05), nil
}

// mustFixStyle names one of n7gc's must-fix AdaptiveColor styles, the exact
// ANSI-256 index it used before n7gc (asserted unchanged as Dark -- the
// no-regression bar), and the minimum WCAG contrast its Light variant must
// clear against white.
type mustFixStyle struct {
	name     string
	style    lipgloss.Style
	origDark string
	lightBar float64
}

// mustFixStyleTable is every style the n7gc audit flagged. Bar choice: 4.5:1
// (WCAG AA "normal text") for anything carrying information a user must read
// correctly -- which includes AGENTS.md's safety-critical markers (warnStyle,
// publicStyle) as well as ordinary body/label/title text, since none of
// those are purely decorative. 3.0:1 (WCAG AA "large text"/non-text) for the
// two purely decorative accents (favStyle's ★, viewInactiveStyle's muted chip
// label). Every value actually chosen in ui.go clears 4.5:1 regardless of
// which bar applies here -- see ui.go's style-color comment.
func mustFixStyleTable() []mustFixStyle {
	return []mustFixStyle{
		{"warnStyle", warnStyle, "214", 4.5},     // safety-critical
		{"publicStyle", publicStyle, "201", 4.5}, // safety-critical
		{"helpTextStyle", helpTextStyle, "252", 4.5},
		{"logoStyle", logoStyle, "51", 4.5},
		{"helpKeyStyle", helpKeyStyle, "81", 4.5},
		{"favStyle", favStyle, "220", 3.0},
		{"activeStyle", activeStyle, "42", 4.5},
		{"helpTitleStyle", helpTitleStyle, "42", 4.5},
		{"wasStyle", wasStyle, "245", 4.5},
		{"viewInactiveStyle", viewInactiveStyle, "245", 3.0},
	}
}

// TestAdaptiveColorContrast is n7gc's core "legible on light" guarantee,
// encoded as a bar rather than an eyeball check: every must-fix style's
// Light variant clears its assigned WCAG contrast ratio against white, and
// (as a sanity check that nothing regressed) its Dark variant still clears
// 4.5:1 against black -- true of every one of these pre-existing production
// colors already, see the kata n7gc audit.
func TestAdaptiveColorContrast(t *testing.T) {
	const darkBar = 4.5
	for _, tc := range mustFixStyleTable() {
		t.Run(tc.name, func(t *testing.T) {
			fg, ok := tc.style.GetForeground().(lipgloss.AdaptiveColor)
			if !ok {
				t.Fatalf("%s.GetForeground() is a %T, not a lipgloss.AdaptiveColor -- did someone revert it to a plain Color?", tc.name, tc.style.GetForeground())
			}
			if fg.Dark != tc.origDark {
				t.Errorf("%s.Dark = %q, want the original pre-n7gc value %q (no-regression)", tc.name, fg.Dark, tc.origDark)
			}
			if !strings.HasPrefix(fg.Light, "#") {
				t.Fatalf("%s.Light = %q, want a #rrggbb hex string", tc.name, fg.Light)
			}

			lightRatio, err := contrastRatio(fg.Light, "#ffffff")
			if err != nil {
				t.Fatalf("computing Light contrast: %v", err)
			}
			if lightRatio < tc.lightBar {
				t.Errorf("%s Light %s vs white = %.2f:1, want >= %.1f:1", tc.name, fg.Light, lightRatio, tc.lightBar)
			}
			t.Logf("%s: Light %s vs #ffffff = %.2f:1 (bar %.1f:1)", tc.name, fg.Light, lightRatio, tc.lightBar)

			darkHex, ok := ansiHexByIndex[fg.Dark]
			if !ok {
				t.Fatalf("no known hex for ANSI-256 index %q -- add it to ansiHexByIndex", fg.Dark)
			}
			darkRatio, err := contrastRatio(darkHex, "#000000")
			if err != nil {
				t.Fatalf("computing Dark contrast: %v", err)
			}
			if darkRatio < darkBar {
				t.Errorf("%s Dark %s (ANSI %s) vs black = %.2f:1, want >= %.1f:1", tc.name, darkHex, fg.Dark, darkRatio, darkBar)
			}
			t.Logf("%s: Dark %s (ANSI %s) vs #000000 = %.2f:1", tc.name, darkHex, fg.Dark, darkRatio)
		})
	}
}

// TestNoDarkRegression proves n7gc's hard no-regression requirement at the
// rendered-bytes level, not just by comparing the stored Dark string: with
// lipgloss forced to a dark background, every must-fix AdaptiveColor style
// must emit the exact same ANSI escape sequence a plain
// lipgloss.NewStyle().Foreground(lipgloss.Color(origDark)) (with the same
// Bold/Italic modifiers) emitted before n7gc touched this file -- i.e. an
// existing dark-terminal user's output is byte-for-byte unchanged.
func TestNoDarkRegression(t *testing.T) {
	origProfile := lipgloss.ColorProfile()
	origDark := lipgloss.HasDarkBackground()
	t.Cleanup(func() {
		lipgloss.SetColorProfile(origProfile)
		lipgloss.SetHasDarkBackground(origDark)
	})

	// Forced so the comparison is deterministic regardless of whether go
	// test's stdout looks like a terminal at all.
	lipgloss.SetColorProfile(termenv.TrueColor)
	lipgloss.SetHasDarkBackground(true)

	for _, tc := range mustFixStyleTable() {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.style.Render("x")
			ref := lipgloss.NewStyle().
				Foreground(lipgloss.Color(tc.origDark)).
				Bold(tc.style.GetBold()).
				Italic(tc.style.GetItalic())
			want := ref.Render("x")
			if got != want {
				t.Errorf("%s under forced dark background = %q, want byte-identical to the pre-n7gc plain style %q", tc.name, got, want)
			}
		})
	}
}

// TestAdaptiveStylesResolveLightDifferently is n7gc's "resolution" bar:
// forcing lipgloss.SetHasDarkBackground(false) vs (true) actually changes
// what each must-fix style renders, and the emitted color matches the
// intended Light/Dark code specifically -- not just that the two outputs
// differ for some unrelated reason.
func TestAdaptiveStylesResolveLightDifferently(t *testing.T) {
	origProfile := lipgloss.ColorProfile()
	origDark := lipgloss.HasDarkBackground()
	t.Cleanup(func() {
		lipgloss.SetColorProfile(origProfile)
		lipgloss.SetHasDarkBackground(origDark)
	})
	lipgloss.SetColorProfile(termenv.TrueColor)

	for _, tc := range mustFixStyleTable() {
		t.Run(tc.name, func(t *testing.T) {
			fg := tc.style.GetForeground().(lipgloss.AdaptiveColor)

			lipgloss.SetHasDarkBackground(false)
			light := tc.style.Render("x")

			lipgloss.SetHasDarkBackground(true)
			dark := tc.style.Render("x")

			if light == dark {
				t.Fatalf("%s rendered identically under light and dark background: %q", tc.name, light)
			}

			wantR, wantG, wantB, err := hexToRGB(fg.Light)
			if err != nil {
				t.Fatalf("parsing Light hex: %v", err)
			}
			gotR, gotG, gotB, ok := parseTruecolorFg(light)
			if !ok {
				t.Errorf("%s light render = %q, want a 38;2;R;G;B truecolor foreground sequence", tc.name, light)
			} else if absDiff(gotR, wantR) > 1 || absDiff(gotG, wantG) > 1 || absDiff(gotB, wantB) > 1 {
				// termenv's TrueColor path round-trips hex colors through an
				// internal color-space conversion (colorful), which can be off
				// by 1 per channel versus the literal hex -- e.g. #004a7f (0,
				// 74, 127) renders as 0;73;127. A tolerance of 1 absorbs that
				// rounding while still catching a genuinely wrong color.
				t.Errorf("%s light render RGB = (%d,%d,%d), want approximately (%d,%d,%d) for Light=%s", tc.name, gotR, gotG, gotB, wantR, wantG, wantB, fg.Light)
			}

			wantDarkSeq := fmt.Sprintf("38;5;%s", fg.Dark)
			if !strings.Contains(dark, wantDarkSeq) {
				t.Errorf("%s dark render = %q, want it to contain ANSI-256 sequence %q (Dark=%s)", tc.name, dark, wantDarkSeq, fg.Dark)
			}
		})
	}
}

// TestHelpViewAndLegendReflectTheme exercises n7gc's forced-theme resolution
// through the actual render paths a user sees, not just isolated
// style.Render() calls: the "?" overlay (helpView) and the bottom-bar
// legend (renderLegend -- p39s's renderLegendGrid/renderGroupedBar). Both
// must visibly differ between a forced light and forced dark background,
// and the light-mode help overlay/dark-mode bottom bar must carry the
// specific colors this issue chose.
func TestHelpViewAndLegendReflectTheme(t *testing.T) {
	origProfile := lipgloss.ColorProfile()
	origDark := lipgloss.HasDarkBackground()
	t.Cleanup(func() {
		lipgloss.SetColorProfile(origProfile)
		lipgloss.SetHasDarkBackground(origDark)
	})
	lipgloss.SetColorProfile(termenv.TrueColor)

	m := New(config.Config{Ports: map[int]config.PortMeta{}})

	// Assert on helpContent (the full overlay text); helpView windows it to the
	// terminal height (v10j), which would clip the styled body text this checks.
	lipgloss.SetHasDarkBackground(false)
	lightHelp := m.helpContent()
	lightBar := m.renderLegend()

	lipgloss.SetHasDarkBackground(true)
	darkHelp := m.helpContent()
	darkBar := m.renderLegend()

	if lightHelp == darkHelp {
		t.Error("helpContent() rendered identically under light and dark background")
	}
	if lightBar == darkBar {
		t.Error("renderLegend() (bottom bar) rendered identically under light and dark background")
	}

	// helpTextStyle's Light hex (#303030 = 48,48,48) should show up in the
	// light-mode help overlay body text.
	if !strings.Contains(lightHelp, "38;2;48;48;48") {
		t.Error("helpContent() under a forced light background doesn't contain helpTextStyle's Light truecolor sequence (38;2;48;48;48)")
	}
	// helpTitleStyle's Dark ANSI-256 index (42) should show up in the
	// dark-mode bottom bar (renderLegendGrid's group-name header).
	if !strings.Contains(darkBar, "38;5;42") {
		t.Error("renderLegend() (bottom bar) under a forced dark background doesn't contain helpTitleStyle's Dark ANSI-256 sequence (38;5;42)")
	}
}

// TestBottomBarHintStylesContrast covers the bottom bar's hint shading (04rb,
// walking back part of c5n8's "brighter monochrome key hints"). The bar's key
// AND desc now share barHintColor -- the EXACT muted grey
// (#A49FA5 light / #777777 dark) bubbles/list's DefaultDelegate already uses
// for this app's own plain reachability row description (e.g. "localhost
// only"/"offline") -- with the key kept
// BOLD so it still anchors the row without a brighter hue. That grey is
// deliberately below the app's 4.5:1 must-fix WCAG bar (this is secondary,
// not must-read, text -- same tier as the list's idle description), so this
// test targets the LOWER floor that grey actually clears rather than
// asserting the old 4.5:1 bar it was never going to meet. Asserted at the
// style level (wiring + contrast) and the rendered-bytes level (the dark bar
// carries barHintColor's truecolor sequence, not bubbles/help's faint
// defaults or c5n8's old bright key sequence).
func TestBottomBarHintStylesContrast(t *testing.T) {
	// Wiring: the help model the TUI actually renders uses tailport's muted
	// bar styles, not bubbles/help's faint defaults -- and key/desc share the
	// same color, with only the key bold.
	m := New(config.Config{})
	if got, want := m.help.Styles.ShortKey.GetForeground(), barKeyStyle.GetForeground(); got != want {
		t.Errorf("m.help.Styles.ShortKey foreground = %v, want barKeyStyle's %v (bar still using bubbles' faint default?)", got, want)
	}
	if !m.help.Styles.ShortKey.GetBold() {
		t.Error("m.help.Styles.ShortKey should be bold (barKeyStyle)")
	}
	if got, want := m.help.Styles.ShortDesc.GetForeground(), barDescStyle.GetForeground(); got != want {
		t.Errorf("m.help.Styles.ShortDesc foreground = %v, want barDescStyle's %v", got, want)
	}
	if m.help.Styles.ShortDesc.GetBold() {
		t.Error("m.help.Styles.ShortDesc should NOT be bold -- only the key anchors the row")
	}
	if got, want := barKeyStyle.GetForeground(), barDescStyle.GetForeground(); got != want {
		t.Errorf("barKeyStyle/barDescStyle foreground mismatch: key %v, desc %v (both should be barHintColor)", got, want)
	}

	// Contrast: barHintColor is the SAME grey as bubbles/list's NormalDesc
	// (list.NewDefaultItemStyles), an already-accepted app-wide muted level --
	// computed directly rather than eyeballed: Light #A49FA5 vs white ~=
	// 2.60:1, Dark #777777 vs black ~= 4.69:1. mutedFloor is pinned at 2.5,
	// just below the weaker (Light) side, so this still catches an accidental
	// slide toward invisibility without re-imposing the 4.5:1 must-fix bar
	// this intentionally-secondary grey doesn't (and isn't meant to) clear.
	const mutedFloor = 2.5
	fg, ok := barKeyStyle.GetForeground().(lipgloss.AdaptiveColor)
	if !ok {
		t.Fatalf("barKeyStyle foreground is %T, not AdaptiveColor", barKeyStyle.GetForeground())
	}
	if fg != barHintColor {
		t.Errorf("barKeyStyle foreground = %v, want barHintColor %v", fg, barHintColor)
	}
	if r, err := contrastRatio(fg.Light, "#ffffff"); err != nil {
		t.Fatal(err)
	} else if r < mutedFloor {
		t.Errorf("barHintColor Light %s vs white = %.2f:1, want >= %.1f:1", fg.Light, r, mutedFloor)
	}
	// barHintColor.Dark is already a truecolor hex (#777777), unlike most of
	// this package's other AdaptiveColors, which pin an ANSI-256 index for
	// dark-terminal byte-identity (see ansiHexByIndex) -- it needs no
	// index->hex lookup here.
	if r, err := contrastRatio(fg.Dark, "#000000"); err != nil {
		t.Fatal(err)
	} else if r < mutedFloor {
		t.Errorf("barHintColor Dark %s vs black = %.2f:1, want >= %.1f:1", fg.Dark, r, mutedFloor)
	}

	// Rendered-bytes: the dark bottom bar carries barHintColor's truecolor
	// sequence (38;2;119;119;119, i.e. #777777) and none of bubbles/help's
	// faint defaults or c5n8's old bright key sequence (38;5;255).
	origProfile := lipgloss.ColorProfile()
	origDark := lipgloss.HasDarkBackground()
	t.Cleanup(func() {
		lipgloss.SetColorProfile(origProfile)
		lipgloss.SetHasDarkBackground(origDark)
	})
	lipgloss.SetColorProfile(termenv.TrueColor)
	lipgloss.SetHasDarkBackground(true)

	m.help.Width = 100
	m.width = 100
	darkBar := m.renderLegend()
	if !strings.Contains(darkBar, "38;2;119;119;119") {
		t.Errorf("dark bottom bar missing barHintColor's truecolor sequence (38;2;119;119;119, #777777):\n%q", darkBar)
	}
	for _, stale := range []string{"38;5;255", "38;2;98;98;98", "38;2;74;74;74"} { // c5n8's bright key + bubbles' faint defaults
		if strings.Contains(darkBar, stale) {
			t.Errorf("dark bottom bar still contains a stale/faint sequence %q", stale)
		}
	}
}

// TestResolveThemeForcesBackground covers n7gc's "light"/"dark" half of
// resolveTheme: each forces lipgloss's background notion outright,
// case-insensitively and trimmed (mirrors resolveMarkerEmoji's own handling
// of "emoji"/"ascii").
func TestResolveThemeForcesBackground(t *testing.T) {
	origDark := lipgloss.HasDarkBackground()
	t.Cleanup(func() { lipgloss.SetHasDarkBackground(origDark) })

	resolveTheme("light")
	if lipgloss.HasDarkBackground() {
		t.Error(`resolveTheme("light") should force HasDarkBackground() == false`)
	}
	resolveTheme("dark")
	if !lipgloss.HasDarkBackground() {
		t.Error(`resolveTheme("dark") should force HasDarkBackground() == true`)
	}
	resolveTheme("DARK")
	if !lipgloss.HasDarkBackground() {
		t.Error(`resolveTheme("DARK") should force HasDarkBackground() == true (case-insensitive)`)
	}
	resolveTheme(" light ")
	if lipgloss.HasDarkBackground() {
		t.Error(`resolveTheme(" light ") should force HasDarkBackground() == false (trimmed)`)
	}
}

// TestResolveThemeAutoLeavesDetectionAlone covers the "auto (or an
// unrecognized value) leaves lipgloss's own detection alone" half of
// resolveTheme -- the mechanism that, combined with termenv's own
// already-dark fallback for the undetectable case (see
// TestTermenvFallsBackToDarkWhenUndetectable below), gives n7gc's
// no-regression guarantee with no extra fallback code in this package.
func TestResolveThemeAutoLeavesDetectionAlone(t *testing.T) {
	origDark := lipgloss.HasDarkBackground()
	t.Cleanup(func() { lipgloss.SetHasDarkBackground(origDark) })

	lipgloss.SetHasDarkBackground(false)
	for _, mode := range []string{"auto", "", "bogus", "  AUTO  "} {
		resolveTheme(mode)
		if lipgloss.HasDarkBackground() {
			t.Errorf("resolveTheme(%q) should leave HasDarkBackground() untouched (still false), got true", mode)
		}
	}

	lipgloss.SetHasDarkBackground(true)
	for _, mode := range []string{"auto", "", "bogus"} {
		resolveTheme(mode)
		if !lipgloss.HasDarkBackground() {
			t.Errorf("resolveTheme(%q) should leave HasDarkBackground() untouched (still true), got false", mode)
		}
	}
}

// TestTermenvFallsBackToDarkWhenUndetectable locks in the dependency
// behavior resolveTheme's "auto" path relies on for n7gc's no-regression
// requirement: when termenv can't query a background color at all (e.g. the
// output isn't a real terminal -- here, a bytes.Buffer, which has no file
// descriptor), it reports the background as dark, not light. If a future
// termenv upgrade changes this fallback, this test -- not a silent
// light-on-white regression for existing users -- is what should catch it.
func TestTermenvFallsBackToDarkWhenUndetectable(t *testing.T) {
	var buf bytes.Buffer
	out := termenv.NewOutput(&buf)
	if !out.HasDarkBackground() {
		t.Error("termenv.Output.HasDarkBackground() on an undetectable (non-tty) output = false, want true (dark fallback)")
	}
}

// TestApplyTheme covers the exported entry point cmd/tailport/main.go calls
// (ApplyTheme) end to end: it's a thin wrapper around resolveTheme, so this
// just confirms the wrapping doesn't lose the mode string.
func TestApplyTheme(t *testing.T) {
	origDark := lipgloss.HasDarkBackground()
	t.Cleanup(func() { lipgloss.SetHasDarkBackground(origDark) })

	ApplyTheme("light")
	if lipgloss.HasDarkBackground() {
		t.Error(`ApplyTheme("light") should force HasDarkBackground() == false`)
	}
	ApplyTheme("dark")
	if !lipgloss.HasDarkBackground() {
		t.Error(`ApplyTheme("dark") should force HasDarkBackground() == true`)
	}
}

// TestVersionStyleContrast holds 0qy8's new versionStyle to the same bar as
// the rest of the app's informational text. The version is secondary, but
// it's a string users read character-by-character to answer "which build am
// I on?" -- a misread digit is a wrong answer, so it gets the 4.5:1 "normal
// text" bar rather than the decorative 3.0:1 one. It reuses the exact
// Light/Dark pair already accepted for wasStyle/viewInactiveStyle, so this
// is really a pin against someone later swapping in an unvetted grey.
func TestVersionStyleContrast(t *testing.T) {
	const bar = 4.5
	fg, ok := versionStyle.GetForeground().(lipgloss.AdaptiveColor)
	if !ok {
		t.Fatalf("versionStyle.GetForeground() is a %T, not a lipgloss.AdaptiveColor", versionStyle.GetForeground())
	}

	lightRatio, err := contrastRatio(fg.Light, "#ffffff")
	if err != nil {
		t.Fatalf("computing Light contrast: %v", err)
	}
	if lightRatio < bar {
		t.Errorf("versionStyle Light %s vs white = %.2f:1, want >= %.1f:1", fg.Light, lightRatio, bar)
	}

	darkHex, ok := ansiHexByIndex[fg.Dark]
	if !ok {
		t.Fatalf("no known hex for ANSI-256 index %q -- add it to ansiHexByIndex", fg.Dark)
	}
	darkRatio, err := contrastRatio(darkHex, "#000000")
	if err != nil {
		t.Fatalf("computing Dark contrast: %v", err)
	}
	if darkRatio < bar {
		t.Errorf("versionStyle Dark %s (ANSI %s) vs black = %.2f:1, want >= %.1f:1", darkHex, fg.Dark, darkRatio, bar)
	}
	t.Logf("versionStyle: Light %s vs white = %.2f:1, Dark %s vs black = %.2f:1 (bar %.1f:1)", fg.Light, lightRatio, darkHex, darkRatio, bar)
}
