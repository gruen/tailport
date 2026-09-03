// Package ui implements tailport's Bubble Tea TUI: a list of locally
// listening ports, toggled on/off tailnet-wide via tailscale serve.
package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/crypto/bcrypt"

	"github.com/gruen/tailport/internal/caddyedge"
	"github.com/gruen/tailport/internal/clip"
	"github.com/gruen/tailport/internal/config"
	"github.com/gruen/tailport/internal/portscan"
	"github.com/gruen/tailport/internal/tsserve"
)

// Style colors (kata n7gc): the must-fix set below use
// lipgloss.AdaptiveColor{Light, Dark} rather than a single fixed
// lipgloss.Color, so the TUI stays legible on both dark and light terminal
// backgrounds -- the app was originally designed white-on-black only. Each
// Dark value is the exact original ANSI-256 index string this app shipped
// with pre-n7gc (never a hex re-encoding of it), so an existing dark-terminal
// user sees byte-identical rendered output, not just "a similar shade" --
// see TestNoDarkRegression in theme_test.go, which asserts this directly
// against a forced dark background. Each Light value is a hand-picked truecolor hex
// chosen to clear a WCAG contrast bar against white -- see
// TestAdaptiveColorContrast, which computes the actual ratio rather than
// eyeballing it: >=4.5:1 (WCAG AA "normal text") for anything that carries
// information a user must read correctly, which here is every must-fix style
// including both AGENTS.md's safety-critical markers (publicStyle's
// public-funnel indicator, warnStyle's caution/dangling indicator) and
// ordinary body/label/title text; >=3:1 (WCAG AA "large text"/non-text) would
// suffice for purely decorative accents (e.g. favStyle's ★, viewInactiveStyle's
// muted chip label), but every value chosen here clears 4.5:1 anyway, so the
// lower bar is documented intent, not a color that's actually that close to
// the line.
//
// lockStyle, errStyle, helpStyle, and viewActiveStyle are deliberately left
// as plain lipgloss.Color: the audit (kata n7gc) found their existing
// contrast already fine on both backgrounds (viewActiveStyle paints its own
// Background(), so it never depends on the terminal's at all). The bubbles
// list.DefaultDelegate and help.Model widgets already use AdaptiveColor
// internally and are untouched here.
var (
	// activeStyle marks the ◉ tailnet-served row and doubles as the info
	// flash/toast color -- >=4.5:1 bar (informational text).
	activeStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#006644", Dark: "42"}).Bold(true)
	// warnStyle marks the ▲ dangling-forward row, the warn flash, and the
	// :22/funnel caution text. Safety-critical (AGENTS.md tailnet-vs-public):
	// the Light variant is a strong, high-contrast amber/brown, not a token
	// nudge -- >=4.5:1 bar, same as publicStyle below.
	warnStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#8a4500", Dark: "214"}).Bold(true)
	// favStyle marks the ★ favorite indicator -- decorative/accent, >=3:1
	// bar would suffice, but the chosen Light value clears >=4.5:1.
	favStyle  = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#8b6500", Dark: "220"}).Bold(true)
	lockStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Bold(true)
	errStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	helpStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	// wasStyle renders a remembered-but-gone process name ("was mailpit") as a
	// muted italic, so it reads as a memory rather than a live label -- still
	// >=4.5:1 so "muted" doesn't slide into "illegible".
	wasStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#4b4b4b", Dark: "245"}).Italic(true)
	// publicStyle marks a port funnelled to the public internet -- deliberately
	// a hot magenta ● (ASCII mode), distinct from the green ◉ tailnet-serve and
	// amber ▲ dangling markers, so "this is on the public internet" reads at a
	// glance. Safety-critical (AGENTS.md tailnet-vs-public): the Light variant
	// is a strong, high-contrast magenta (>=4.5:1), not a token nudge -- this
	// marker must be unambiguous on both backgrounds.
	publicStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#8b008b", Dark: "201"}).Bold(true)

	helpTitleStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#006644", Dark: "42"}).Bold(true)
	helpKeyStyle   = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#004a7f", Dark: "81"}).Bold(true)
	helpTextStyle  = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#303030", Dark: "252"})

	// barHintColor (04rb) is the muted grey shared by both the bottom-bar key
	// and description. It is the EXACT same hex pair bubbles/list's
	// DefaultDelegate uses for its own idle-row description foreground
	// (list.NewDefaultItemStyles().NormalDesc, charmbracelet/bubbles
	// list/defaultitem.go) -- the grey behind this app's plain reachability
	// text (Description(), see portItem -- e.g. "localhost only"/"offline")
	// -- so the bar's hints read at the same muted level as the rest of the
	// app's secondary text instead of standing out as a brighter accent.
	// Duplicated here as a literal (rather than
	// derived via a type assertion on bubbles' returned style at init time)
	// so a future bubbles upgrade can't panic tailport's startup over a
	// cosmetic color; TestBottomBarHintStylesContrast pins the value.
	barHintColor = lipgloss.AdaptiveColor{Light: "#A49FA5", Dark: "#777777"}

	// barKeyStyle colors the KEY of each bottom-bar hint (the "space" in "space
	// serve"); barDescStyle colors its description. Both replace bubbles/help's
	// built-in ShortKey/ShortDesc defaults and share barHintColor -- the same
	// muted grey as the list's idle description -- so the bar doesn't outshine
	// the rest of the app's secondary text. Keys stay BOLD so they still anchor
	// the row without needing a brighter hue; descriptions are plain weight.
	// Both are wired onto m.help.Styles in newModel. Group headers keep their
	// own accent (helpTitleStyle, green) -- the bar's only non-muted color.
	// This walks back part of kata c5n8's "brighter monochrome key hints" --
	// see TestBottomBarHintStylesContrast for the (lower, but still
	// app-wide-accepted) contrast floor this now targets.
	barKeyStyle  = lipgloss.NewStyle().Foreground(barHintColor).Bold(true)
	barDescStyle = lipgloss.NewStyle().Foreground(barHintColor)

	// logoStyle draws the persistent cyan "tailport" wordmark pinned to the
	// top-left of every view (list and empty-state alike); see renderHeader.
	logoStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#005f5f", Dark: "51"}).Bold(true)

	// versionStyle draws the build version next to the wordmark (0qy8). It's
	// deliberately the app's muted secondary grey rather than another accent:
	// the version is reference metadata you look up, not something competing
	// with the cyan wordmark for attention. Same pair as viewInactiveStyle /
	// wasStyle, so it clears the app-wide 4.5:1 contrast bar on both
	// backgrounds (see TestVersionStyleContrast).
	versionStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#6a6a6a", Dark: "245"})

	// The two segments of the Favorites|All-ports view indicator: the active
	// view is a filled green chip, the inactive one is dim.
	viewActiveStyle = lipgloss.NewStyle().Background(lipgloss.Color("42")).Foreground(lipgloss.Color("233")).Bold(true)
	// viewInactiveStyle labels the inactive view chip -- decorative/accent,
	// >=3:1 bar would suffice, but the chosen Light value clears >=4.5:1.
	viewInactiveStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#6a6a6a", Dark: "245"})
)

// keyMap describes every keybinding the TUI responds to, for the bubbles/help
// legend. It satisfies help.KeyMap. Most bindings carry static help text;
// ShowAll's Desc is refreshed on each render to reflect the current view
// (favorites vs all ports; see model.renderLegend).
type keyMap struct {
	Toggle key.Binding
	Funnel key.Binding
	// Publish exposes a port to the public internet through a user-controlled
	// Caddy edge (the `p` key, kata v1z5; swapped from `P` under vzj4). It is a
	// SECOND public path, sibling
	// to Funnel and mutually exclusive with it per port -- never ranked above.
	Publish  key.Binding
	Filter   key.Binding
	NewPort  key.Binding
	Label    key.Binding
	Favorite key.Binding
	// Forget clears ★ -- what "u" did before 3cwx moved that key to Undo.
	Forget key.Binding
	Lock   key.Binding
	// Undo/Redo step through registry edits (favorite/forget/label/lock/add).
	// Redo is deliberately absent from the bottom bar -- see barGroups.
	Undo    key.Binding
	Redo    key.Binding
	ShowAll key.Binding
	Copy    key.Binding
	Clean   key.Binding
	Refresh key.Binding
	Help    key.Binding
	Quit    key.Binding
}

// keyGroup is one like-for-like column of the keybinding legend (kata p39s): a
// display name and the bindings under it, in order. keyMap.groups() is the
// SINGLE grouping source -- the bottom-bar grid (renderLegend), the "?" overlay
// and `tailport quickstart` (both via KeyLegendGroups), and FullHelp() all
// derive from it, so the four columns can never drift apart.
type keyGroup struct {
	name     string
	bindings []key.Binding
}

// groups returns the approved like-for-like grouping: Serve Toggles, Favorites,
// View, App -- in display order, one group per bottom-bar column and one
// "?"-overlay section. (p39s introduced this grouping with a separate Protect
// column; folded into Serve Toggles here -- lock/unlock and the contextual
// clean-stale are exposure guards, so they live at the end of the group with x
// lock/unlock always the last item. Clean is contextual: barGroups drops it
// unless a dangling forward exists, so ordering it before Lock keeps Lock last
// in every state. Copy moved out to sit under "n new favorite" in Favorites.)
func (k keyMap) groups() []keyGroup {
	return []keyGroup{
		{"Serve Toggles", []key.Binding{k.Toggle, k.Funnel, k.Publish, k.Clean, k.Lock}},
		{"Favorites", []key.Binding{k.Favorite, k.Forget, k.NewPort, k.Copy, k.Label}},
		{"View", []key.Binding{k.Filter, k.ShowAll, k.Refresh}},
		// Undo/Redo sit in App, not Favorites: they step through every registry
		// edit, including the lock changes that live in the Serve Toggles column, so
		// filing them under Favorites would understate their reach.
		{"App", []key.Binding{k.Undo, k.Redo, k.Help, k.Quit}},
	}
}

// ShortHelp flattens groups() in grouped order; FullHelp returns one inner
// slice per column (per group) -- the columnar vehicle bubbles/help expects and
// the shape the bottom-bar grid mirrors. Neither distinguishes a "short" vs
// "full" mode or truncates: the legend relies on width-based layout (see
// renderLegend) instead of an ellipsis.
func (k keyMap) ShortHelp() []key.Binding {
	var out []key.Binding
	for _, g := range k.groups() {
		out = append(out, g.bindings...)
	}
	return out
}

func (k keyMap) FullHelp() [][]key.Binding {
	groups := k.groups()
	cols := make([][]key.Binding, len(groups))
	for i, g := range groups {
		cols[i] = g.bindings
	}
	return cols
}

func newKeyMap() keyMap {
	return keyMap{
		// "on tailscale" in the bar's Serve Toggles column: it names what space
		// does -- serve the port on the tailnet -- as one of three parallel serve
		// toggles (p on caddy, P on ts.net). The "?" overlay keeps the fuller
		// "toggle serve on/off" prose (keyLegendDescs).
		Toggle: key.NewBinding(key.WithKeys(" "), key.WithHelp("space", "on tailscale")),
		// p/P swapped (vzj4): capital guards the more-permanent exposure, so
		// funnel (tailnet-only cert, easy to drop) takes the shifted key and
		// publish (custom domain via Caddy edge) takes the bare key.
		Funnel: key.NewBinding(key.WithKeys("P"), key.WithHelp("P", "on ts.net (public)")),
		// "on caddy (public)": the second public path (kata v1z5), a Caddy-edge
		// publish sibling to funnel, in the Serve Toggles group.
		Publish: key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "on caddy (public)")),
		// Filter is display-only (legend + help): the actual "/" handling lives
		// in bubbles/list. Listed here so the feature is discoverable.
		Filter: key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
		// "new favorite", matching the binding's own name (NewPort) and ykgj's
		// "n (new port)" -- "add" was the drift (2pz4). Deliberately scoped to
		// the bar hint: the n input prompt and the "?" overlay still say "add".
		NewPort:  key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "new favorite")),
		Label:    key.NewBinding(key.WithKeys("l"), key.WithHelp("l", "label")),
		Favorite: key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "favorite")),
		// 3cwx: "u" used to unfavorite; it's Undo now, and the unfavorite
		// action moved to shift-F under the clearer name "forget". The pair
		// is deliberately f/F -- same letter, shifted, opposite effect.
		Forget:  key.NewBinding(key.WithKeys("F"), key.WithHelp("F", "forget")),
		Lock:    key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "lock/unlock")),
		Undo:    key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "undo")),
		Redo:    key.NewBinding(key.WithKeys("ctrl+r"), key.WithHelp("ctrl+r", "redo")),
		ShowAll: key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "filtered")),
		Copy:    key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy URL")),
		// Clean moved to shift-C when "c" was reassigned to copy (vnq7); it's
		// contextual (only enabled when dangling forwards exist), so demoting
		// it to a shifted key is fine.
		Clean:   key.NewBinding(key.WithKeys("C"), key.WithHelp("C", "clean stale")),
		Refresh: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		Help:    key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:    key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

type portItem struct {
	port      portscan.Port
	active    bool
	listening bool
	host      string
	fqdn      string
	// funnelPublic is the public ingress port (443/8443/10000) this port is
	// funnelled on, or 0 if it isn't funnelled. A funnelled port is exposed to
	// the public internet, which outranks its tailnet-serve state in the UI.
	funnelPublic int
	// publishHostname is the public custom hostname this port is published at
	// through the Caddy edge (kata v1z5), or "" if it isn't published. Set from
	// the live edge poll (m.published), never persisted per-port. publishAuth
	// records whether that route carries basic auth. Publish is a SIBLING of
	// funnel, not ranked against it: tailport enforces mutual exclusion, so a
	// tailport-driven port is funnelled XOR published, never both -- the only
	// way to see both is external mutation, surfaced as explicit drift (reach).
	publishHostname string
	publishAuth     bool
	// dimmed de-emphasises this row: set on non-favorite ports pulled into the
	// Favorites view by an active "/" filter (4ye6), so real favorites still
	// stand out among the wider search results. See portDelegate.Render.
	dimmed bool
	meta   config.PortMeta
	// emoji selects the moon-phase reach-ramp marker set
	// (🌕/🌔/🌒/🌑/🌫️/✕) over the ASCII fallback (○/◔/◉/●/▲/✕) -- on tailnet
	// and served share 🌒/◉ (qptn). Resolved once for the model and copied
	// onto each item.
	emoji bool
	// justCopied marks the port most recently copied via "c" while its
	// description was the bare tailnet URL (state C: reachServed), set in
	// rebuildItems from m.copiedPort (py5b). Description() appends the
	// styled "✓ copied" suffix when it's set; it fades on its own via
	// copiedExpireMsg/copiedID (mirroring flashExpireMsg/flashID) rather
	// than being cleared by selection changes.
	justCopied bool
}

// portDelegate is the list's item renderer: the stock DefaultDelegate, except
// a portItem flagged dimmed is drawn with the delegate's built-in dimmed
// styles so filter matches that aren't favorites recede in the Favorites view.
type portDelegate struct {
	list.DefaultDelegate
}

func newPortDelegate() portDelegate {
	return portDelegate{DefaultDelegate: list.NewDefaultDelegate()}
}

func (d portDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	if it, ok := item.(portItem); ok && it.dimmed {
		// Render this one item with dimmed styles by copying the delegate (a
		// value) and swapping its normal styles for the dimmed ones. The copy
		// keeps all the default layout/selection logic intact.
		dd := d.DefaultDelegate
		dd.Styles.NormalTitle = dd.Styles.DimmedTitle
		dd.Styles.NormalDesc = dd.Styles.DimmedDesc
		dd.Render(w, m, index, item)
		return
	}
	d.DefaultDelegate.Render(w, m, index, item)
}

// markerGlyph is the port's reachability marker: a moon-phase "fill ramp"
// (1exs, direction fixed by e1wv) tracking i.reach()'s 7-state classification
// (79xb) from least exposed to most -- both channels go open/light at
// localhost and filled/dark at the public internet, so the emoji ramp reads
// consistently with the mono ramp: 🌕/○ localhost only, 🌔/◔ local network
// only, 🌒/◉ on tailnet (served or bound wide), 🌑/● funnelled to the public
// internet. The two BROKEN states sit OFF the ramp as plain glyphs, not
// moons: 🌫️/▲ a stale dangling forward, ✕/✕ a favorite whose process is
// down. It switches on the SAME i.reach() resolver Description() uses, so
// the glyph and the row's text can never disagree about a port's state.
// Emoji markers are padded to a stable 2-cell width so the :port column
// stays aligned even if a terminal renders a given emoji (or the naturally
// 1-cell ✕) narrow.
func (i portItem) markerGlyph() string {
	var m string
	switch i.reach() {
	case reachFunnel:
		// Reachable from the open internet -- outranks every other state.
		// Public is safety-critical (AGENTS.md): keep the hot-magenta ●.
		if i.emoji {
			m = "🌑"
		} else {
			m = publicStyle.Render("●")
		}
	case reachPublish:
		// Published to the public internet via the Caddy edge (kata v1z5). A
		// DISTINCT public marker from funnel's ●/🌑 per the safety-marker
		// mandate (the two public paths must be tellable apart at a glance),
		// same publicStyle magenta since both mean "reachable by anyone".
		if i.emoji {
			m = "🌐"
		} else {
			m = publicStyle.Render("◆")
		}
	case reachServed, reachTailnet:
		// Served AND already-tailnet-reachable-by-IP (wildcard/tailnet bind)
		// share this glyph (qptn): both answer at the SAME http://host:port,
		// so they're the same reachability tier from a peer's perspective.
		// The bind-scope distinction (served vs. bound wide) now lives on
		// the title's bindPrefix() instead of a separate marker tier.
		if i.emoji {
			m = "🌒"
		} else {
			m = activeStyle.Render("◉")
		}
	case reachStale:
		// Dangling forward: served, but nothing is bound locally, so a tailnet
		// peer hitting the URL gets connection refused. Off the moon ramp --
		// this reads as "something's wrong", not just "less reachable".
		if i.emoji {
			m = "🌫️"
		} else {
			m = warnStyle.Render("▲")
		}
	case reachLAN:
		if i.emoji {
			m = "🌔"
		} else {
			m = "◔"
		}
	case reachOffline:
		// A favorite whose process is down -- off the moon ramp, same as
		// reachStale, but styled distinctly so the two broken states don't
		// read as the same problem: wasStyle is the exact "remembered but
		// gone" muted treatment the row's own "was mailpit" label already
		// uses for this precise situation (a down favorite).
		if i.emoji {
			m = "✕"
		} else {
			m = wasStyle.Render("✕")
		}
	default: // reachLocalhost
		if i.emoji {
			m = "🌕"
		} else {
			m = "○"
		}
	}
	if i.emoji {
		for lipgloss.Width(m) < 2 {
			m += " "
		}
	}
	return m
}

// bindPrefix is the netstat-style host prefix shown left of ":PORT" on the row
// title, the differentiator now that a bound-wide tailnet port and a served
// port share the green ◉ glyph and the same URL (qptn): "*" for a wildcard
// bind (0.0.0.0/::), the LAN IP for a specific LAN bind, and "" (quiet) for a
// loopback/served port. So "*:3000" (bound wide) vs ":3000" (served) read alike
// but stay distinguishable, and a LAN row's "<ip>:3000" self-explains why it is
// NOT on the tailnet.
func (i portItem) bindPrefix() string {
	switch i.port.BindScope {
	case portscan.ScopeWildcard:
		return "*"
	case portscan.ScopeLAN:
		return i.port.BindHost // e.g. "192.168.1.5"; may be "" in the odd unclassified case
	default: // loopback / tailnet-ip / unknown -> quiet
		return ""
	}
}

func (i portItem) Title() string {
	marker := i.markerGlyph()
	lock := ""
	if i.meta.Locked {
		lock = " " + lockStyle.Render("🔒")
	}
	star := ""
	if i.meta.Favorite {
		star = favStyle.Render("★") + " "
	}
	// Name precedence: an explicit user label wins; else the live process name
	// while something's listening; else the remembered last process ("was
	// mailpit", italic) so a down favorite still says what used to run there;
	// else "?".
	name := i.meta.Label
	switch {
	case name != "":
	case i.port.Process != "":
		name = i.port.Process
	case i.meta.LastProcess != "":
		name = wasStyle.Render("was " + i.meta.LastProcess)
	default:
		name = "?"
	}
	return fmt.Sprintf("%s%s %s:%d  %s%s", marker, lock, i.bindPrefix(), i.port.Number, star, name)
}

// reachState is the honest 7-state reachability lexicon (79xb): who can
// ACTUALLY reach this port, as distinct from whether tailport has served it.
// `tailscale serve` is a separate app-layer reverse proxy that only matters
// for a loopback-bound app -- a wildcard/tailnet-IP bind (e.g. sshd on :22)
// is already tailnet-reachable at the IP layer with or without serve. This
// single resolver backs both Description (the row text) and the Part-3
// serve-guard, so the two can never disagree about a port's state.
type reachState int

const (
	reachLocalhost reachState = iota // A: loopback bind, unserved -- this machine only
	reachTailnet                     // B: wildcard/tailnet-IP bind, unserved -- already on tailnet
	reachLAN                         // B': specific LAN-IP bind, unserved -- LAN only, NOT tailnet
	reachServed                      // C: served AND something is listening
	reachFunnel                      // D: funnelled to the public internet -- outranks everything
	reachPublish                     // D': published to the public internet via the Caddy edge -- SIBLING of reachFunnel, not ranked (mutual exclusion means a tailport port is in exactly one)
	reachStale                       // E: served but nothing listening -- a dangling forward
	reachOffline                     // F: not served, not listening (e.g. a down favorite)
)

// reach resolves a portItem's reachState. Precedence (top to bottom): the two
// public paths (funnel D and publish D') come first -- either makes the port
// public regardless of its tailnet serve status. They are SIBLINGS, not a
// ranked pair (kata v1z5): tailport enforces funnel/publish mutual exclusion,
// so a tailport-driven port is in exactly one and reach() never has to pick a
// winner. The one case where BOTH are observed on a port is external mutation
// (a foreign Funnel or Caddy edit); rather than silently collapse to one
// marker, reach() surfaces it with the existing warning/stale affordance (▲)
// and a distinct "funnelled AND published" description (see plainDescription)
// -- no bespoke drift state, glyph, or field. Below the public tier: a served
// port is either healthy (C, listening) or a stale dangling forward (E, not
// listening); an unserved port's reachability comes straight from its widest
// bind scope (portscan.BindScope); anything neither served nor listening is
// simply offline.
func (i portItem) reach() reachState {
	published := i.publishHostname != ""
	switch {
	case i.funnelPublic != 0 && published:
		// External drift: both public paths on one port. Reuse the warning/stale
		// affordance rather than picking a winner (plainDescription names it).
		return reachStale
	case i.funnelPublic != 0:
		return reachFunnel
	case published:
		return reachPublish
	case i.active && !i.listening:
		return reachStale
	case i.active && i.listening:
		return reachServed
	case i.listening: // !active && listening
		switch i.port.BindScope {
		case portscan.ScopeWildcard, portscan.ScopeTailnet:
			return reachTailnet
		case portscan.ScopeLAN:
			return reachLAN
		default: // ScopeLoopback or ScopeUnknown
			return reachLocalhost
		}
	default: // !active && !listening
		return reachOffline
	}
}

// inlineCopyState reports whether a `c` copy on this row confirms INLINE
// (append a transient "✓ copied" to the row's description) rather than via the
// bottom-bar toast (vqa3). True for the healthy copyable states whose row
// text already states what was copied -- including reachPublish (d80p): `c`
// now copies the exact "https://<publishHostname>" the row shows, so
// shown==copied and it joins the inline group. False for funnel (shown
// PUBLIC funnel URL ≠ copied TAILNET URL — a genuine shown≠copied mismatch),
// stale (dangling — the copied URL resolves to nothing), and offline (nothing
// live to copy), which keep the one disambiguating toast.
func (i portItem) inlineCopyState() bool {
	switch i.reach() {
	case reachLocalhost, reachLAN, reachTailnet, reachServed, reachPublish:
		return true
	default: // reachFunnel, reachStale, reachOffline
		return false
	}
}

// plainDescription is the UNSTYLED row text for the current reach state -- what
// inlineCopyFits measures before choosing inline-✓ vs toast, and the base that
// styledDescription() and the inline "✓ copied" suffix build on (vqa3). Kept in
// lockstep with the states in reach().
func (i portItem) plainDescription() string {
	switch i.reach() {
	case reachFunnel:
		return tsserve.PublicURL(i.fqdn, i.funnelPublic) + " · on the internet"
	case reachPublish:
		d := "https://" + i.publishHostname + " · published to the internet"
		if i.publishAuth {
			// A glyph marks a basic-auth-protected route rather than spelling out
			// "basic auth"; emoji-gated like the exposure markers (markerGlyph).
			if i.emoji {
				d += " " + authGlyphEmoji
			} else {
				d += " " + authGlyphMono
			}
		}
		return d
	case reachStale:
		// Drift: a port carrying BOTH public paths (external mutation only) is
		// routed here to reuse the ▲ warning affordance; name it explicitly
		// rather than pretending it's an ordinary dangling forward.
		if i.funnelPublic != 0 && i.publishHostname != "" {
			return "funnelled AND published — remove one"
		}
		return "bound to tailnet, but stale — space to unbind"
	case reachServed:
		return i.servedDescPlain()
	case reachTailnet:
		// :22 (SSH) isn't HTTP, so it keeps its own line rather than a
		// served-style URL. A leading guard like this makes room for future
		// non-HTTP special-cases without disturbing the common path below.
		if i.port.Number == 22 {
			return "on tailnet · reachable via SSH"
		}
		// qptn: a wildcard-bound port is already reachable at the SAME
		// http://host:port a served port answers at (post-83wv, `c` copies
		// this exact URL for both) -- so its description now reads
		// IDENTICALLY to reachServed's. The bind-scope distinction moves to
		// the title's bindPrefix() instead (netstat-style "*:port").
		return i.servedDescPlain()
	case reachLAN:
		return "local network only"
	case reachOffline:
		return "offline"
	default: // reachLocalhost
		return "localhost only"
	}
}

// styledDescription wraps plainDescription with the per-state emphasis the row
// carries today: publicStyle for a funnelled (public) row, warnStyle for a
// stale dangling forward. The healthy states stay unstyled.
func (i portItem) styledDescription() string {
	switch i.reach() {
	case reachFunnel, reachPublish:
		return publicStyle.Render(i.plainDescription())
	case reachStale:
		return warnStyle.Render(i.plainDescription())
	default:
		return i.plainDescription()
	}
}

func (i portItem) Description() string {
	desc := i.styledDescription()
	if i.justCopied {
		// Pre-styled bold-green suffix (py5b), now on EVERY inline-copy state
		// (vqa3), not just served. justCopied is set by copyURL ONLY for
		// inlineCopyState() rows, so funnel/stale/offline never reach here with
		// it set. The delegate's rune highlighter is ANSI-unaware, but
		// filterNoHighlight (see New) strips the per-char match highlight from
		// every row, so embedding raw ANSI here is safe.
		desc += activeStyle.Render(copiedSuffix)
	}
	return desc
}

// servedDescPlain returns the UNSTYLED state-C description text
// ("http://host:port · on tailnet") -- the row text Description() renders for
// reachServed, and the exact string whose URL copyURL copies. Shared by
// Description() and copyURL's inlineCopyFits width check (py5b) so the two
// can never drift out of sync about what the row actually shows.
func (i portItem) servedDescPlain() string {
	return fmt.Sprintf("http://%s:%d · on tailnet", i.host, i.port.Number)
}

func (i portItem) FilterValue() string {
	// Number, live process, user label, AND the remembered process name -- so a
	// down favorite showing "was mailpit" is still found by filtering "mail",
	// which is exactly when you're hunting for a service that's gone.
	return fmt.Sprintf("%d %s %s %s", i.port.Number, i.port.Process, i.meta.Label, i.meta.LastProcess)
}

type refreshMsg struct {
	ports  []portscan.Port
	active map[int]bool
	funnel map[int]int
	// auto marks a periodic-poll refresh (e40f), whose errors fade silently
	// instead of raising a red toast every interval.
	auto bool
	err  error
}

// fqdnMsg carries the node's MagicDNS name, fetched once at startup and cached
// (it's static for the session), keeping the heaviest call out of the poll.
type fqdnMsg struct{ fqdn string }

// detectOperatorMsg carries the result of tsserve.DetectOperatorNotSet's
// best-effort, read-only proactive check (kata tapv). ok is false when the
// check was inconclusive (e.g. an older tailscale without `debug prefs`) --
// the Update handler then leaves m.operatorNotSet exactly as it was, rather
// than treating "couldn't tell" as "it's fine".
type detectOperatorMsg struct {
	notSet bool
	ok     bool
}

// refreshTickMsg fires on the periodic auto-refresh timer (e40f).
type refreshTickMsg struct{}

// eggTickMsg advances the hidden Easter-egg animation (28mv).
type eggTickMsg struct{}

// fwTickMsg advances the hidden fireworks animation (5x1e). It runs on its OWN
// faster cadence (fwInterval) so smooth arcs don't force the slower egg spin
// (eggInterval) to speed up too -- the two tickers are deliberately decoupled.
type fwTickMsg struct{}

// poofTickMsg advances the "poof" dissipation animation for a just-purged edge
// route (kata dw57). It is a SIBLING of fwTickMsg, NOT a reuse of it: the
// fwTickMsg handler hard-stops whenever !m.showEgg (see that case below), but
// a poof legitimately fires with the egg overlay closed (it's triggered by a
// force-purge/take-over, nothing to do with the Easter egg) -- relaxing the
// egg guard instead would entangle the fireworks lag-clutch for no reason
// (design doc §3.5). So the poof gets its own message, its own ticker
// (poofTick, same fwInterval cadence), and its own no-leak guard (poofTicking,
// mirroring fwTicking) rather than hooking into the fireworks'.
type poofTickMsg struct{}

// poofState is the in-flight "poof" dissolve for a just-purged route's
// descriptor (kata dw57) -- purely cosmetic, decoupled from the purge/undo
// control flow. text is the descriptor being dissolved (e.g.
// "<host> → <label>:<port>"); frame counts ticks elapsed and ttl counts ticks
// remaining (poofTickMsg's handler clears m.poof once ttl reaches 0); emoji is
// captured at trigger time (mirrors *firework.emoji, set once at newFirework)
// so the glyph vocabulary stays consistent for the animation's whole ~500ms
// life even if terminal capability detection were re-run mid-flight. Rendering
// is derived from frame/ttl at render time (renderPoofLine) rather than baked
// in here, mirroring how stepFireworks advances physics separately from the
// grid-time draw.
type poofState struct {
	text  string
	frame int
	ttl   int
	emoji bool
}

// poofTTL is the poof's total lifetime in ticks at fwInterval (50ms) -- 10
// frames = ~500ms, inside the design's ~6-12 frame target (§3.5).
const poofTTL = 10

// lastPurgeState is the single-slot, ephemeral, session-memory record armed after
// an OWNED force-purge take-over (kata ttfh; design §3.6). It is NOT a persistent
// undoStack entry and NOT redoable: the local undoStack holds registry deltas
// (persisted, wouldUnlockSSH-guarded) whose `u` help promises undo "does NOT touch
// what's exposed" -- a remote mutation to shared edge state has the wrong lifetime
// and would falsify that promise, so the restore affordance is a DISTINCT key (R)
// off this one slot. captured is the byte-faithful deleted route to re-POST (undo
// step B); hostname/ourLabel/ourPort drive step A (Unpublish our take-over) + B/C;
// deletedDesc names the purged backend shown in the restore prompt; takeoverLive
// records whether the in-transaction take-over publish actually succeeded, gating
// the poll-based third-party-re-take clear (a FAILED take-over is armed for retry,
// so its "no route of ours" poll must NOT be read as a re-take).
type lastPurgeState struct {
	captured     json.RawMessage
	hostname     string
	ourLabel     string
	ourPort      int
	deletedDesc  string
	takeoverLive bool
}

// lastPurgeTTL is the restore affordance's idle lifetime (design OQ1: ~60s +
// competing-action clears). The timer is generation-stamped (a stale expiry is
// ignored), suspended while a restore is in flight, and re-armed on a retryable
// (edge-unreachable) restore outcome.
const lastPurgeTTL = 60 * time.Second

type toggleDoneMsg struct {
	port int
	err  error
}

// publishInfo is the live per-port publish state the edge poll returns: the
// public hostname a local port is published at, and whether that route carries
// basic auth. Keyed by local port in m.published. Never persisted -- Caddy is
// the source of truth (kata v1z5).
type publishInfo struct {
	hostname string
	auth     bool
}

// publishDoneMsg reports a completed publish/unpublish edge op (kata v1z5). Its
// handler MIRRORS toggleDoneMsg: it clears m.pending and, rather than hand-set
// the published map from the op's outcome, always re-fetches (refresh +
// pollPublishedCmd) so the UI reflects Caddy's actual state, not an optimistic
// guess. unpublish distinguishes which op produced this outcome (kata vsx4 #1):
// unpublishCmd sets it true, publishCmd leaves it false (its zero value). This
// matters because Unpublish can also return caddyedge.ErrHostnameConflict
// (caddyedge.go) -- without the flag an unpublish outcome would be
// indistinguishable from a publish one and could walk the ErrHostnameConflict
// branch below, classifying the STALE m.pendingPublish from a prior publish and
// potentially RE-PUBLISHING during what the user asked to be a de-escalation.
// The handler gates that branch on !unpublish so an unpublish conflict always
// falls through to a plain error toast.
type publishDoneMsg struct {
	port      int
	err       error
	unpublish bool
}

// inspectConflictMsg carries the read-only classification of a hostname conflict
// (kata qfbf): when a publish returns caddyedge.ErrHostnameConflict, the handler
// issues inspectConflictCmd, which calls caddyedge.InspectConflict and returns
// this so the model can render a SPECIFIC refusal naming the current holder
// (owned-other-backend / another machine / hijacked-@id / foreign route), or —
// on Kind==None — retry the plain publish once. port/hostname identify the
// attempted publish; info is the classification; err is any read failure.
type inspectConflictMsg struct {
	port     int
	hostname string
	info     caddyedge.ConflictInfo
	err      error
}

// purgeDoneMsg reports a completed force-purge / take-over delete (kata 6n15).
// On success captured carries the deleted route's exact bytes (meaningful for an
// OWNED purge only — the ttfh undo seam) and the handler resumes the takeover
// publish; err carries a caddyedge sentinel (ErrConflictChanged / ErrNoConflict /
// ErrUnreachable / …) the handler maps to a re-classify or a toast. port/hostname
// identify the attempted takeover so the resume can re-issue it.
type purgeDoneMsg struct {
	captured caddyedge.Captured
	hostname string
	port     int
	err      error
	// deletedDesc names the DELETED route's backend (from the confirmed
	// identity), so the poof animates the route that was purged -- e.g.
	// "host-b:3000" -- not this machine's take-over backend (roborev 65qc-#2).
	// Empty for a route with no nameable backend (its hostname alone is shown).
	deletedDesc string
	// owned carries the confirmed identity's Owned bit (kata ttfh): the undo is
	// OWNED-only (design OQ8), and by purgeDoneMsg time m.purgeExpect has been
	// zeroed by clearPurgeFlow, so the owned-ness the arm needs rides the message.
	owned bool
}

// restoreResult classifies the outcome of a purge-undo (kata ttfh; design §3.6),
// so the flash is computed from the FINAL observed edge state (VerifyRestore) or
// the split step-A/B outcome -- never from a guess about which step failed.
type restoreResult int

const (
	restoreRestored        restoreResult = iota // C: Restored
	restoreUnclaimed                            // C: Unclaimed
	restoreClaimedByOther                       // C: ClaimedByOther, or B refused (name re-claimed)
	restoreContentMismatch                      // C: ContentMismatch
	restoreUnverified                           // B succeeded but C could not be read
	restoreTakeoverChanged                      // A: our take-over is still live (drifted) -- do NOT append
	restoreUnreachable                          // A or B: edge unreachable -- retryable, keep the slot
	restoreError                                // A or B: any other error -- toast, clear the slot
)

// restoreDoneMsg reports a completed purge-undo (kata ttfh). result classifies the
// outcome (from VerifyRestore's final read or the split A/B handling); hostname
// names the route for the message; err carries the underlying transport/edge error
// for the restoreError toast only.
type restoreDoneMsg struct {
	hostname string
	result   restoreResult
	err      error
}

// lastPurgeExpireMsg fires the restore affordance's ~60s idle timeout (kata ttfh;
// design OQ1). gen matches it to the arming/re-arming that scheduled it, so a
// superseded timer (the slot cleared, restored, or re-armed) is ignored -- exactly
// the flashExpireMsg/flashID discipline.
type lastPurgeExpireMsg struct{ gen int }

// pendingPublish is the carry described on model.pendingPublish: the minimal
// parameters needed to re-issue an in-flight publish after the flow state is
// cleared, holding no plaintext secret.
type pendingPublish struct {
	hostname string
	label    string
	port     int
	withAuth bool // auth was requested; rebuild from cfg.Caddy.AuthUser/AuthHash
	retried  bool // a Kind==None retry has already fired (bounds it to once)
}

// publishPollMsg carries one edge-poll result (kata v1z5 step 5). On err the
// poll degrades quietly: last-known m.published is kept and publishReachable
// goes false. On success published replaces m.published wholesale. gen is the
// poll's version stamp (roborev 0k12 #1): the handler ignores any result older
// than the newest already applied, so out-of-order completions can't clobber
// newer state.
type publishPollMsg struct {
	published map[int]publishInfo
	err       error
	gen       int
}

// publishTickMsg fires the separate 15s published-state poll timer. The ticker
// never stops (it reschedules unconditionally); the poll it triggers is a nil
// cmd when publishing is unconfigured, so an unconfigured edge costs nothing.
type publishTickMsg struct{}

type cleanupDoneMsg struct{ err error }

// flashExpireMsg clears the transient toast if it's still the one that
// scheduled this expiry (matched by id), so a newer toast isn't cut short.
type flashExpireMsg struct{ id int }

// copiedExpireMsg clears the inline row "✓ copied" annotation (m.copiedPort)
// if it's still the one that scheduled this expiry (matched by id), mirroring
// flashExpireMsg/flashID (py5b): copying a second port before the first
// annotation fades bumps copiedID, so the first port's stale timer is ignored
// rather than clearing the newer annotation out from under it.
type copiedExpireMsg struct{ id int }

// flashLevel is the severity of a toast, driving its colour: info (green),
// warn (amber), error (red). It unifies what used to be a bare success toast
// plus a separate persistent m.err red line (q89g).
type flashLevel int

const (
	flashInfo flashLevel = iota
	flashWarn
	flashError
)

// entryMode tracks which (if any) text-input flow is currently active.
// Both "n" (add/toggle an arbitrary port) and "l" (label the selected
// port) reuse the same open-textinput interaction pattern, but need
// distinct submit behavior, hence the enum instead of a single bool.
type entryMode int

const (
	entryNone entryMode = iota
	entryAddPort
	entryLabel
	entryConfirmClean
	// entryConfirm22 gates a toggle of port :22 (SSH) behind an explicit
	// y/n prompt: turning serve on/off for :22 can drop the operator's live
	// SSH session, so :22 -- and only :22 -- always confirms first.
	entryConfirm22
	// entryConfirmFunnel gates turning funnel ON for a port behind a strong
	// y/n prompt that names the port and shows the resulting public HTTPS URL
	// with an explicit "anyone on the internet" warning. Turning funnel OFF
	// (de-escalation back to tailnet-served) is not gated.
	entryConfirmFunnel
	// entryConfirmUnlockSSH gates UNLOCKING port :22 behind a type-"ssh"
	// text confirm (ah23): the lock is the primary guard on SSH access, so a
	// stray "x" must not remove it. Only unlocking is gated -- locking :22 and
	// any non-:22 lock toggle stay a single instant keypress.
	entryConfirmUnlockSSH
	// The publish (`p`) flow (kata v1z5; swapped from `P` under vzj4) is a
	// small state machine of its own,
	// all handled in updatePublishEntry. It gathers a public hostname, an
	// optional shared basic-auth credential (first authed publish only), then a
	// funnel-grade confirm before touching the Caddy edge. The hostname and
	// domain steps are reached ONLY on FRESH setup -- the SAME blank-caddy.domain
	// condition -- and run in that order: hostname first (kata ztzg; it's needed
	// to reach the admin API at all), THEN domain (kata w131; unchanged, captured
	// inline instead of refusing). Once both are saved, they feed the same host
	// step, exactly as the domain step alone did before ztzg:
	//   [entryPublishHostname -> entryPublishDomain ->] entryPublishHost -> entryPublishAuth ->
	//   [entryPublishCredUser -> entryPublishCredPass ->] entryConfirmPublish
	entryPublishHostname // text: the edge's tailnet (MagicDNS) hostname, prefilled from caddy.hostname, captured when caddy.domain is blank (ztzg)
	entryPublishDomain   // text: the public base domain, captured when caddy.domain is blank (w131)
	entryPublishHost     // text: the editable label only; ".<domain>" is a locked suffix (single label)
	entryPublishAuth     // 3-way y/n/esc: require basic auth? ("no auth" != "abort")
	entryPublishCredUser // text: shared basic-auth username (first authed publish)
	entryPublishCredPass // text (masked): shared basic-auth password (first authed publish)
	entryConfirmPublish  // funnel-grade y/n naming the exact https://<hostname>
	// The force-purge / take-over confirm ladders (kata 6n15; design §3.4). A
	// publish that collided with an existing edge route can, for the two purgeable
	// conflict kinds, be escalated to "delete that route and take the hostname
	// over". An OWNED conflict (our own @id, another local port or another
	// machine) is a NORMAL y/n; a FOREIGN route (drift AGENTS.md never silently
	// overrides) is a SCARY two-gate: a y/n drift warning, then a typed-"purge"
	// commit modeled exactly on the entryConfirmUnlockSSH gate. IdHijacked STAYS a
	// refusal (never a blind delete of drift).
	entryConfirmPurgeOwned       // y/n: force-purge an owned conflicting route, then take over
	entryConfirmPurgeForeign     // y/n: scary drift warning before purging a foreign route
	entryConfirmPurgeForeignType // typed-"purge" commit gate for a foreign route
)

type model struct {
	list list.Model
	// delegate is the SAME portDelegate instance handed to list.New (list.Model
	// exposes no public delegate getter) -- kept here too so renderGrid can
	// reuse it directly to render each grid cell (identical
	// selection/dimmed/ANSI styling to the old single-column list.View()
	// path) and so gridDims can read its Height()/Spacing() for row math.
	delegate portDelegate
	help     help.Model
	keys     keyMap
	cfg      config.Config
	host     string
	// undoStack/redoStack step through registry edits (3cwx). Session-only:
	// deliberately NOT persisted, so undo can never reach back past a restart
	// into edits the user has long since forgotten making.
	//
	// They hold per-PORT deltas rather than whole-config snapshots, which is
	// load-bearing: the registry is also written by bookkeeping the user never
	// asked for (remember() on serve, rememberProcesses() on every background
	// refresh). Restoring a whole-config snapshot would silently revert those
	// too, so an undo of "favorite :8080" could erase an unrelated port's
	// remembered process name. A delta touches only its own port.
	undoStack []registryEdit
	redoStack []registryEdit
	// version is the build version to show beside the wordmark (0qy8),
	// threaded in from main's -ldflags-injected `version` via Run -- the ui
	// package has no build stamp of its own. Empty means "unknown": New's
	// dozens of test call sites leave it unset and renderHeader then draws
	// the bare wordmark, so no test has to know a version string.
	version string
	// fqdn is this node's MagicDNS name (host.tailnet.ts.net), refreshed
	// alongside serve/funnel status and used to build public funnel URLs.
	fqdn string
	// showAllPorts selects the list view: false = Favorites (only ports
	// marked meta.Favorite), true = All ports (every currently-listening
	// port). Toggled by "a".
	showAllPorts bool
	// filtering mirrors "a '/' filter is active" (Filtering or FilterApplied).
	// It's the scope signal rebuildItems reads: while filtering, the list
	// searches ALL listening ports regardless of showAllPorts (4ye6). Kept in
	// sync with the list's own filter state as it toggles on/off.
	filtering bool
	// showHelp gates the full-screen "?" help overlay (see helpView). While
	// it's open the overlay replaces the whole view and swallows every key
	// except the ?/esc/q that dismiss it (and the scroll keys below).
	showHelp bool
	// helpScroll is the top-line offset of the "?" overlay's scrollable body
	// (v10j). The overlay's content is taller than most terminals, and
	// alt-screen mode clips rather than scrolls, so helpView slices the
	// content to the viewport and up/down/pgup/pgdn/home/end pan it. Reset to
	// 0 each time the overlay opens; clamped to [0, helpMaxScroll()] on every
	// adjustment and at render.
	helpScroll int
	// showEgg gates the hidden "E" Easter-egg overlay (eggView); eggFrame is
	// its animation counter, advanced by an eggTickMsg that STOPS rescheduling
	// the moment showEgg goes false (no leaked ticker). Like showHelp it's
	// fully modal and always exitable via esc/q/E.
	showEgg  bool
	eggFrame int
	// fireworks holds the in-flight ASCII fireworks launched by the hidden 'f'
	// key WITHIN the egg overlay (5x1e -- a secret-within-the-secret, never in
	// the legend/help). Each 'f' press launches one instantly; the slice is
	// capped at fwCap. fwTicking guards the decoupled fireworks ticker so it's
	// scheduled at most once (never stacked) and stops when no fireworks remain
	// or the overlay closes. Kept adjacent to showEgg/eggFrame on purpose.
	fireworks []firework
	fwTicking bool
	// poof holds the in-flight "poof" dissolve animation for a just-purged edge
	// route (kata dw57) -- nil when idle. It is triggered fire-and-forget from
	// the purgeDoneMsg success handler and rendered as ONE transient line
	// through the status slot (renderStatusLine), never a list-row animation or
	// a sticky banner (design §3.5). poofTicking guards its SIBLING ticker
	// (poofTick/poofTickMsg) exactly like fwTicking guards fwTick: scheduled at
	// most once, and stopped the instant the poof completes. Every mutation of
	// m.poof (set OR clear) calls resizeList, mirroring m.flash's discipline, so
	// the live-measured status-slot reservation (statusLines in
	// listBodyHeight) always tracks it and the list never clips.
	poof        *poofState
	poofTicking bool
	// (3e8b) adaptive intake clutch: lastFwTick is the wall-clock time of the
	// previous fwTickMsg (zero when idle/warming up); fwLagEWMA is the EWMA
	// (ms) of the OBSERVED inter-tick interval, our proxy for event-loop /
	// terminal-write backpressure; fwClutch is the hysteresis-gated verdict
	// that throttles NEW 'f' launches (never kills in-flight fireworks) when
	// the EWMA says we're falling behind. See fwClutchNext/fwLagNext.
	lastFwTick time.Time
	fwLagEWMA  float64
	fwClutch   bool
	// width and height are the terminal dimensions from the last
	// WindowSizeMsg. height pins the bottom bar (status, shortcuts) to the
	// last rows of the viewport regardless of how short the body is; width
	// right-justifies the view toggle in the top header (see renderHeader).
	width  int
	height int

	allPorts []portscan.Port
	active   map[int]bool
	// funnel maps a local port to the public ingress port it's funnelled on
	// (443/8443/10000). Reconciled from tsserve.FunnelStatus on refresh.
	funnel map[int]int
	// published maps a local port to its live Caddy-edge publish state (kata
	// v1z5), keyed by RouteInfo.Port and filtered to routes whose backend Label
	// is THIS machine's short MagicDNS label. Read live from the edge on a
	// separate 15s poll (pollPublishedCmd) -- never persisted per-port, Caddy is
	// the source of truth. Nil/empty when publish is unconfigured.
	published map[int]publishInfo
	// publishReachable tracks the edge poll's health. It starts optimistically
	// true (so the status line doesn't cry "edge unreachable" before the first
	// poll finishes) and flips false on a poll failure; a later successful poll
	// flips it back. On failure the LAST-KNOWN published map is kept (quiet
	// degrade) and a small persistent " · edge unreachable" fragment is shown --
	// never a toast (kata v1z5 step 5).
	publishReachable bool
	// publishPollGen / publishPollApplied VERSION the async edge polls (roborev
	// 0k12 #1): pollPublishedCmd stamps each issued poll with an incrementing
	// publishPollGen, and the publishPollMsg handler DROPS any result whose gen
	// is older than the newest already applied (publishPollApplied). Polls are
	// remote round-trips that can complete out of order -- without this, a slow
	// stale poll (e.g. an older periodic read landing after the post-publish
	// refresh) would clobber newer state, transiently hiding exposure and
	// bypassing the funnel/publish mutual-exclusion guards.
	publishPollGen     int
	publishPollApplied int
	// caddyClientOverride, when non-nil, replaces the client built from
	// cfg.Caddy for every edge call. Production leaves it nil (caddyClient builds
	// AdminURL/ServerName from config); tests inject a client pointed at an
	// httptest.Server so the whole publish/unpublish/poll flow runs against
	// in-process fakes, never a real caddy or a bound port.
	caddyClientOverride *caddyedge.Client
	// pendingPublish carries the parameters of the in-flight publish so the
	// conflict path can act after clearPublishFlow has zeroed the flow (kata
	// qfbf; design §3.0). It is set at confirm-time and consulted only on a
	// publishDoneMsg ErrHostnameConflict — to name the attempted hostname when
	// classifying, and to re-issue the publish ONCE on a Kind==None (the conflict
	// cleared). The plaintext password never lives here (withAuth records only
	// whether auth was requested; auth is rebuilt from cfg.Caddy.AuthUser/AuthHash,
	// already bcrypt-hashed). retried bounds the Kind==None retry to a single
	// attempt so a cleared-then-returned conflict can never spin. This carry is
	// also the seam kata 6n15's force-purge takeover resumes the publish through.
	pendingPublish pendingPublish

	mode       entryMode
	portInput  textinput.Model
	labelInput textinput.Model
	labelPort  int // port being labeled while mode == entryLabel
	// sshInput is the type-"ssh" text field for the entryConfirmUnlockSSH
	// gate; sshUnlockPort is the port being unlocked (always 22, stored for
	// symmetry). See the "x" handler.
	sshInput      textinput.Model
	sshUnlockPort int

	pending       int   // port currently being toggled; 0 = none
	cleaning      int   // number of dangling forwards being torn down; 0 = none
	cleanTargets  []int // ports the entryConfirmClean prompt is asking about
	confirmPort   int   // port the entryConfirm22 prompt is asking about (always 22)
	confirmTurnOn bool  // direction of the pending entryConfirm22 toggle
	// entryConfirmFunnel state: the local port being funnelled, the public
	// ingress port it will use, and the toggle direction.
	funnelPort   int
	funnelPublic int
	funnelTurnOn bool
	// Publish (`p`) flow state (kata v1z5; swapped from `P` under vzj4), carried
	// across the dialog steps and
	// cleared by clearPublishFlow on esc/abort/confirm. publishInput is the ONE
	// shared textinput reused for the host / cred-user / cred-pass steps (its
	// EchoMode is flipped to EchoPassword only for the password step).
	// publishCredUser/publishCredPass hold the plaintext credential gathered on
	// a first authed publish ONLY until confirm-time, where the password is
	// bcrypt-hashed and the plaintext dropped -- clearPublishFlow zeroes both so
	// an aborted flow never leaves a plaintext password in memory.
	publishInput       textinput.Model
	publishPort        int    // local port being published
	publishHostname    string // the public hostname entered (held into the confirm)
	publishWithAuth    bool   // basic auth requested for this publish
	publishCredUser    string // plaintext username (first authed publish only)
	publishCredPass    string // plaintext password (first authed publish only; hashed + dropped at confirm)
	publishEnableServe bool   // this confirm will also turn tailscale serve on
	// Force-purge / take-over state (kata 6n15), carried from the
	// inspectConflictMsg classification through the confirm ladder into the purge
	// cmd, and cleared by clearPurgeFlow. purgeInfo drives the confirm text (naming
	// the backend / a cross-machine route); purgeExpect is the re-verify identity
	// PurgeConflict pins so the approved route can't be swapped out from under the
	// confirm. purgeInput is the typed-"purge" gate for a foreign route (its own
	// field, kept out of the publish flow's input-reset logic). takeoverHost is set
	// on a successful purge so the resumed takeover publish can toast "took over
	// <host>"; the secret-free resume params ride m.pendingPublish.
	purgeHostname string
	purgePort     int
	purgeInfo     caddyedge.ConflictInfo
	purgeExpect   caddyedge.PurgeExpect
	purgeInput    textinput.Model
	takeoverHost  string
	// Purge-undo state (kata ttfh; design §3.6). lastPurge is the single armed
	// restore slot (nil = nothing to restore); pendingArm stashes the arm info at
	// purgeDoneMsg-success so the take-over's own publishDoneMsg ARMS lastPurge
	// from it (even if the take-over failed) without letting itself clear it;
	// restoring is the in-flight guard (a second R while a restore is running is a
	// no-op); lastPurgeGen generation-stamps the ~60s idle timer so a stale expiry
	// (or one suspended by an in-flight restore) is ignored. All OWNED-only (OQ8).
	lastPurge    *lastPurgeState
	pendingArm   *lastPurgeState
	restoring    bool
	lastPurgeGen int
	// flash is the single transient notification shown in the bottom bar --
	// copy confirmations, refusals, and errors alike (q89g). flashLevel tints
	// it (info green / warn amber / error red). It clears on the next keypress
	// or when a matching flashExpireMsg (tagged with flashID) fires after a
	// short delay, so nothing -- including errors -- sticks around stale.
	flash      string
	flashLevel flashLevel
	flashID    int
	// copiedPort is the port number showing the inline "✓ copied" row
	// annotation (py5b), or 0 for none -- set by copyURL's state-C fast path
	// instead of the toast, and read back in rebuildItems to flag that one
	// port's item justCopied. copiedID is copiedPort's flashID-style guard:
	// bumped on every inline copy so a matching copiedExpireMsg clears it,
	// while a stale one (superseded by a newer copy) is ignored -- see
	// copiedExpireMsg.
	copiedPort int
	copiedID   int
	// operatorNotSet is the STICKY counterpart to flash (kata tapv): a
	// deliberate exception to the auto-dismiss toast, because tailscale's
	// operator requirement is required-setup guidance, not a fleeting
	// error. Unlike flash it survives keypresses and does NOT time out; it
	// clears only when the underlying issue is actually resolved -- a
	// successful serve/funnel toggle, or a re-check (proactively at
	// startup, or on "r") confirming the operator is now set. See
	// operatorHintText, and the detectOperatorMsg/toggleDoneMsg handlers.
	operatorNotSet bool
	// domainSetupPending is a SECOND sticky banner, PARALLEL to and independent
	// of operatorNotSet (kata w131, ycv1 r3-NEW-1): it is raised when the `p`
	// flow (swapped from `P` under vzj4) captures a blank caddy.domain inline
	// (entryPublishDomain) and reminds
	// the user that saving the config FIELD is not the same as doing the edge
	// SETUP -- they still owe the *.<domain> wildcard DNS pointed at the edge and
	// a deployed edge. It is deliberately NOT a reuse of operatorNotSet: that one
	// is a single slot with its own unrelated clear-triggers, and the two
	// conditions (operator unset; domain-saved-needs-DNS) are orthogonal and can
	// be true SIMULTANEOUSLY, which one slot can't render. Like operatorNotSet it
	// is sticky -- it survives keypresses and does NOT auto-dismiss (a toast
	// would vanish while the user types the hostname in the very next dialog) --
	// and it clears when a real DNS/edge is demonstrably working: a successful
	// published-state poll or a successful publish. See domainSetupHintText and
	// the publishPollMsg/publishDoneMsg handlers.
	domainSetupPending bool
	// operatorUser is the OS username used to build the sticky hint's exact,
	// copy-pasteable fix command ($USER EXPANDED, via tsserve.CurrentUsername
	// resolved once in New) -- falls back to a "<you>" placeholder at render
	// time if it couldn't be determined.
	operatorUser string
	// configPath is the resolved absolute path where preferences (the port
	// registry: favorites, labels, locks) are persisted, captured once at
	// New() from config.Path(cfg.ResolvedPath()) so the help overlay can
	// state it exactly -- honoring an explicit -c/--config override or
	// XDG_CONFIG_HOME rather than guessing (gahj, y4gt). Empty if
	// config.Path() errored, in which case helpView describes the rule instead.
	configPath string
	// emoji selects the Easter-egg overlay's UNICODE glyph set (egg art, the
	// fireworks' ·░▒▓█ shading ramp, muzzle smoke) over its ASCII fallback.
	// It is resolved once at New() from ONLY the terminal's capabilities
	// (emojiCapable()) -- deliberately independent of cfg.Markers/--markers
	// (qwcw): the egg is a hidden, undocumented feature, so its glyph style
	// always auto-detects and is never governed by the exposure-marker flag.
	// See markerEmoji for the exposure markers' own (now decoupled) glyph
	// choice.
	emoji bool
	// markerEmoji selects the port-state EXPOSURE markers' moon-phase glyph
	// ramp (🌕🌔🌒🌑🌫️✕) over the mono ASCII/Unicode-symbol fallback
	// (○◔◉●▲✕), resolved once at New() from resolveMarkerEmoji(markersMode)
	// -- i.e. cfg.Markers, overridden by --markers for this run only (zn2x).
	// Unlike emoji above, an unset/unrecognized mode resolves to MONO (qwcw):
	// the exposure markers default to ascii/mono, only opting into
	// emoji/detection via an explicit "emoji"/"auto" mode. Copied onto each
	// portItem in rebuildItems so markerGlyph()/Title() pick the same set.
	markerEmoji bool
}

// filterNoHighlight ranks items with the list's default fuzzy filter but clears
// the matched-rune indices, so the delegate's ANSI-unaware highlighter never
// runs over our styled titles (ykxh). Filtering/ranking behaviour is identical
// to the default; only the per-character match highlight is dropped.
func filterNoHighlight(term string, targets []string) []list.Rank {
	ranks := list.DefaultFilter(term, targets)
	for i := range ranks {
		ranks[i].MatchedIndexes = nil
	}
	return ranks
}

// resolveMarkerEmoji picks the EXPOSURE-marker glyph set (the port-state moon
// ramp 🌕🌔🌒🌑🌫️✕ vs its mono fallback ○◔◉●▲✕) from the configured
// --markers/cfg.Markers mode (qwcw, splitting this from the egg/fireworks'
// own always-auto-detecting resolution -- see the model.emoji field doc):
// "emoji"/"ascii" force it; "auto" is an explicit opt-in to the terminal's
// apparent UTF-8 capability; anything else -- crucially including "" (unset)
// -- is the NEW default and resolves to MONO. This deliberately splits ""
// from "auto" (identical before qwcw): unset now means mono, "auto" means
// detect.
func resolveMarkerEmoji(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "emoji":
		return true
	case "ascii":
		return false
	case "auto":
		return emojiCapable() // explicit opt-in to detection
	default:
		return false // ""/unset/unknown -> MONO (new default)
	}
}

// emojiCapable is a best-effort heuristic for "this terminal can render emoji":
// a UTF-8 effective locale (LC_ALL, else LC_CTYPE, else LANG) and a TERM that
// isn't the bare Linux console or a dumb terminal. It never guarantees glyph
// coverage -- that's why "ascii" (and "emoji") can force the choice.
func emojiCapable() bool {
	switch os.Getenv("TERM") {
	case "", "dumb", "linux":
		return false
	}
	loc := os.Getenv("LC_ALL")
	if loc == "" {
		loc = os.Getenv("LC_CTYPE")
	}
	if loc == "" {
		loc = os.Getenv("LANG")
	}
	loc = strings.ToLower(loc)
	return strings.Contains(loc, "utf-8") || strings.Contains(loc, "utf8")
}

// ResolveMarkerEmoji exports resolveMarkerEmoji's exposure-marker glyph
// resolution for callers outside this package. `tailport quickstart` (kata
// x4cg, updated by qwcw) uses it so its printed legend picks the same glyph
// set (see keyLegendDescs) the "?" overlay would for the same markers mode,
// not just the same key text. The egg/fireworks glyph choice has no exported
// resolver -- it's always emojiCapable(), independent of markers mode.
func ResolveMarkerEmoji(mode string) bool {
	return resolveMarkerEmoji(mode)
}

// resolveTheme applies the "theme" manual override (kata n7gc) to lipgloss's
// shared default renderer: "light"/"dark" force lipgloss's notion of the
// terminal background for the rest of the process, so every package-level
// AdaptiveColor style in this file -- and the bubbles list/help widgets'
// own AdaptiveColor values -- render through that same forced choice.
// Anything else ("auto", "", or an unrecognized value) leaves lipgloss's own
// auto-detection alone -- unlike resolveMarkerEmoji (qwcw), theme mode does
// NOT split "" from "auto"; both still mean "detect" here.
//
// No extra fallback code is needed here for "undetectable -> treat as dark"
// (the no-regression requirement for existing dark-terminal users):
// termenv's own HasDarkBackground already resolves that way on its own --
// when it can't query a background color at all (no TTY, unsupported
// terminal) it falls back to NoColor, which converts to RGB black, whose
// lightness is 0 (<0.5), so termenv itself reports "dark" with no help from
// this package. See TestResolveThemeAutoLeavesDetectionAlone.
func resolveTheme(mode string) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "light":
		lipgloss.SetHasDarkBackground(false)
	case "dark":
		lipgloss.SetHasDarkBackground(true)
	}
}

// ApplyTheme exports resolveTheme for cmd/tailport/main.go: call it once at
// startup, before the first render, after main.go has already resolved
// --theme/config precedence (flag > cfg.Theme > auto; see resolveThemeMode
// in main.go and applyTheme's call sites in run/runQuickstart).
//
// It's a standalone function rather than threaded through New's
// markersOverride-style variadic parameter because, unlike the marker-glyph
// choice, "theme" is not per-model state: it's a one-time side effect on
// lipgloss's shared package-level renderer that every package-level style
// (and the bubbles widgets) already reads from on every render, so it only
// needs to be applied once, early -- it doesn't need to be carried on the
// model, and doing so would risk a second call site (e.g. a future
// non-model render path) forgetting to apply it.
func ApplyTheme(mode string) {
	resolveTheme(mode)
}

// New builds the initial model from a loaded Config. markersOverride is an
// optional, run-only "--markers" value (zn2x): the caller (main.go, after
// its own validation) passes at most one string. When it's non-empty it
// wins for THIS session's EXPOSURE-marker glyph choice (m.markerEmoji,
// resolved once below via resolveMarkerEmoji), but it deliberately never
// touches cfg.Markers itself -- cfg is stored as-is into m.cfg, which is
// what any later Save() (triggered by an unrelated mutation:
// favorite/label/lock/etc.) writes back to disk. Mutating cfg.Markers here
// would leak the run-only override into the persisted config on the next
// unrelated save, which is exactly what "applies to the current run only;
// never rewrites config" rules out.
//
// markersOverride never affects m.emoji (qwcw): the egg/fireworks glyph
// choice is always emojiCapable(), decoupled from --markers/cfg.Markers.
//
// It's variadic rather than a plain second parameter so every existing
// New(cfg) call site (there are dozens across ui_test.go) keeps compiling
// unchanged; only ui.Run passes an override today.
func New(cfg config.Config, markersOverride ...string) model {
	host, _ := os.Hostname()

	del := newPortDelegate()
	l := list.New(nil, del, 0, 0)
	// The "tailport" wordmark now lives in View()'s persistent header
	// (renderHeader), drawn above both the list and the empty state, so the
	// list's own built-in title is turned off to avoid rendering it twice.
	l.SetShowTitle(false)
	l.SetShowHelp(false)
	// Filtering stays enabled, but its input is rendered by the app in a
	// dedicated row under the header (see View/renderFilterRow) rather than in
	// the list's title area, which would stack awkwardly beneath the header.
	l.SetShowFilter(false)
	l.FilterInput.Prompt = "filter: "
	// Rank with the default fuzzy filter, but drop the per-character match
	// highlighting: our row titles embed ANSI (coloured marker, gold ★, italic
	// "was …"), and the delegate's highlighter (lipgloss.StyleRunes) is not
	// ANSI-aware -- it splices styling into the middle of those escape sequences
	// and prints garbage like "[1;38;5;220m" (ykxh). Nil MatchedIndexes -> the
	// highlighter is a no-op and the coloured title renders intact; ranking is
	// unchanged. (The old highlight was misaligned anyway, since it indexed the
	// FilterValue string, not the styled Title.)
	l.Filter = filterNoHighlight

	ti := textinput.New()
	ti.Placeholder = "port"
	ti.CharLimit = 5
	ti.Width = 10
	ti.Validate = func(s string) error {
		for _, r := range s {
			if r < '0' || r > '9' {
				return fmt.Errorf("digits only")
			}
		}
		return nil
	}

	li := textinput.New()
	li.Placeholder = "label"
	li.CharLimit = 40
	li.Width = 30

	si := textinput.New()
	si.Placeholder = "ssh"
	si.CharLimit = 8
	si.Width = 10

	// One shared textinput for the publish flow's three text steps (host,
	// cred-user, cred-pass). Wide enough for a nested public hostname; its
	// EchoMode is flipped to EchoPassword for the masked password step and back
	// to EchoNormal otherwise (see updatePublishEntry / clearPublishFlow).
	pi := textinput.New()
	pi.Placeholder = "host.example.com"
	pi.CharLimit = 253 // max DNS name length
	pi.Width = 40

	// The typed-"purge" gate for a foreign force-purge (kata 6n15), modeled on the
	// type-"ssh" unlock gate (si above).
	pui := textinput.New()
	pui.Placeholder = "purge"
	pui.CharLimit = 8
	pui.Width = 10

	if cfg.Ports == nil {
		cfg.Ports = map[int]config.PortMeta{}
	}

	h := help.New()
	// Swap bubbles/help's faint default key/desc colors for tailport's own
	// muted-grey pair (barHintColor, 04rb) so the bottom-bar hints read at the
	// same secondary level as the rest of the app instead of standing out (see
	// barKeyStyle / barDescStyle). renderLegendGrid / renderGroupedBar pull
	// ShortKey/ShortDesc straight off m.help.Styles, so this is the single
	// wiring point; the header row keeps its green helpTitleStyle.
	h.Styles.ShortKey = barKeyStyle
	h.Styles.ShortDesc = barDescStyle

	// Resolve the config path once here (best-effort) so the help overlay can
	// show exactly where settings live, -c/--config and XDG overrides all. If
	// cfg came from config.Load, ResolvedPath() already pins the exact file;
	// otherwise (e.g. a literal built directly by a test) this falls back to
	// normal XDG/~/.config resolution. On error we leave it empty and
	// helpView falls back to describing the rule.
	configPath, _ := config.Path(cfg.ResolvedPath())

	// Run-only markers override (zn2x): flag > cfg.Markers > mono default
	// (qwcw -- "auto" is required to opt into terminal detection; unset no
	// longer implies it). Only the resolved markerEmoji bool below is
	// affected; cfg itself (and thus what a later Save() persists) is
	// untouched.
	markersMode := cfg.Markers
	if len(markersOverride) > 0 && markersOverride[0] != "" {
		markersMode = markersOverride[0]
	}

	return model{
		list: l, delegate: del, help: h, keys: newKeyMap(), cfg: cfg, host: host, active: map[int]bool{},
		portInput: ti, labelInput: li, sshInput: si, publishInput: pi, purgeInput: pui, configPath: configPath,
		// Optimistic until the first edge poll actually fails (see the field
		// doc), so a configured-but-not-yet-polled edge doesn't flash
		// "unreachable" on startup.
		publishReachable: true,
		// emoji (egg/fireworks) always auto-detects, independent of markersMode.
		emoji: emojiCapable(),
		// markerEmoji (exposure markers) obeys --markers/cfg.Markers, defaulting
		// to mono when unset (qwcw).
		markerEmoji:  resolveMarkerEmoji(markersMode),
		operatorUser: tsserve.CurrentUsername(),
	}
}

// Run launches the interactive TUI. markersOverride is an optional, run-only
// "--markers" value (zn2x); see New for why it's kept separate from
// cfg.Markers rather than overwriting it. version is main's
// -ldflags-injected build version, rendered beside the wordmark (0qy8) --
// passed in rather than read from a package global for the same reason
// selfupdate.NewUpdater takes it: only main owns the build stamp.
func Run(cfg config.Config, markersOverride, version string) error {
	m := New(cfg, markersOverride)
	m.version = version
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

// refreshInterval is how often the TUI re-reads serve/funnel state so changes
// made outside the app (e.g. `tailscale serve` on the CLI) surface on their own
// (e40f). The poll is cheap after the Status dedupe + FQDN cache: ss + one
// serve-status call.
const refreshInterval = 3 * time.Second

func (m model) Init() tea.Cmd {
	// FQDN is fetched once (it's static for the session; see fetchFQDN); the
	// periodic tick then only re-reads the cheap serve/funnel state.
	// detectOperator is the best-effort proactive check (kata tapv): it runs
	// once here so the sticky hint can appear before the user's first
	// space-press, without waiting on a failed toggle.
	//
	// tea.SetWindowTitle emits OSC 2 through Bubble Tea's own renderer, so
	// (unlike clip.go's OSC 52 clipboard write) there's no manual /dev/tty
	// dance needed here. We never restore the previous title on exit --
	// terminals and tmux reset the pane title on the next shell prompt
	// anyway, and it's not a convention other TUIs bother with either.
	title := "tailport"
	if m.host != "" {
		title = "tailport — " + m.host
	}
	// The published-state poll (kata v1z5 step 5) is a SEPARATE ticker from the
	// serve/funnel refresh: a remote round-trip to the Caddy edge, so it runs on
	// its own slower 15s cadence and is a no-op (nil cmd) when unconfigured. The
	// Init poll races fqdn resolution -- it may not yet know our short label --
	// so fqdnMsg re-polls once the label is known.
	return tea.Batch(refresh, fetchFQDN, detectOperator, refreshTick(), m.pollPublishedCmd(), publishTick(), tea.SetWindowTitle(title))
}

// refreshTick schedules the next auto-refresh.
func refreshTick() tea.Cmd {
	return tea.Tick(refreshInterval, func(time.Time) tea.Msg { return refreshTickMsg{} })
}

const (
	eggURL        = "https://michaelgruen.com/"
	eggDomain     = "michaelgruen.com"
	eggRepoURL    = "https://github.com/gruen/tailport"
	eggRepoDomain = "github.com/gruen/tailport"
	// Both links are hardcoded (from the origin remote) -- the app never shells
	// out to git; it only shells to tailscale/ss/lsof (zero extra deps).
	eggInterval = 100 * time.Millisecond // ~10fps: responsive over SSH, no flood
	// fwInterval is the fireworks cadence, DECOUPLED from eggInterval so arcs
	// animate smoothly (~20fps) without also speeding the egg's shimmer/spin.
	fwInterval = 50 * time.Millisecond
	// fwCap bounds simultaneous in-flight fireworks; 'f' presses beyond it are
	// ignored (perf-safe under mashing). This is now a SAFETY BACKSTOP (never
	// unbounded) -- the adaptive intake clutch (fwClutch, below) is the
	// effective limiter under load; 60 is just a taste choice for the
	// unthrottled ceiling (3e8b).
	fwCap = 60
	// fwClutchOnMs/fwClutchOffMs bound the hysteresis band the intake clutch
	// gates on: engage (refuse new launches) once the observed inter-tick EWMA
	// climbs above fwClutchOnMs (~1.6x fwInterval), release once it recovers
	// below fwClutchOffMs (~1.2x fwInterval). The gap between the two keeps the
	// clutch from flapping frame-to-frame near the threshold. fwLagAlpha is the
	// EWMA smoothing factor applied in fwLagNext. (3e8b)
	fwClutchOnMs  = 80.0
	fwClutchOffMs = 62.0
	fwLagAlpha    = 0.3
)

// eggTick schedules the next Easter-egg animation frame. It is only ever
// rescheduled while showEgg is true (see the eggTickMsg handler), so closing
// the overlay stops the ticker -- no leaked goroutine, no busy loop.
func eggTick() tea.Cmd {
	return tea.Tick(eggInterval, func(time.Time) tea.Msg { return eggTickMsg{} })
}

// fwTick schedules the next fireworks frame. Like eggTick it is only rescheduled
// while there is work to do (showEgg AND at least one live firework -- see the
// fwTickMsg handler), so an idle overlay and a closed overlay both stop it.
func fwTick() tea.Cmd {
	return tea.Tick(fwInterval, func(time.Time) tea.Msg { return fwTickMsg{} })
}

// poofTick schedules the next poof-dissolve frame, on the SAME cadence as
// fwTick (fwInterval) but as an entirely separate ticker (kata dw57) -- see
// poofTickMsg's doc for why it can't reuse fwTick/fwTickMsg. Rescheduled only
// while the poof is still alive; see the poofTickMsg handler.
func poofTick() tea.Cmd {
	return tea.Tick(fwInterval, func(time.Time) tea.Msg { return poofTickMsg{} })
}

// startPoof begins (or restarts) the "poof" dissolve for a just-purged route's
// descriptor (kata dw57). It is fire-and-forget by design: every call site
// folds the returned cmd into whatever tea.Batch it already returns and never
// branches on it -- the poof never alters control flow. A poof already in
// flight is simply replaced with the fresh descriptor at frame 0 (a rapid
// second purge doesn't stack animations), but poofTicking (mirroring
// fwTicking) ensures AT MOST ONE ticker is ever scheduled: if one is already
// running, startPoof returns nil and the existing ticker picks up the new
// state on its next tick. Calls resizeList so the status-slot reservation
// (renderStatusLine via listBodyHeight's live statusLines measurement) tracks
// the new poof immediately, exactly like every m.flash mutation site does.
func (m *model) startPoof(text string) tea.Cmd {
	m.poof = &poofState{text: text, ttl: poofTTL, emoji: m.emoji}
	m.resizeList()
	if m.poofTicking {
		return nil
	}
	m.poofTicking = true
	return poofTick()
}

// fwClutchNext is the adaptive intake clutch's hysteresis gate (3e8b): engage
// (refuse new 'f' launches) once ewma climbs above fwClutchOnMs, release once
// it drops below fwClutchOffMs, and hold the current state inside the band so
// it doesn't flap frame-to-frame near the threshold. Pure and clock-free so it
// unit-tests with synthetic EWMA sequences.
func fwClutchNext(engaged bool, ewma float64) bool {
	if ewma > fwClutchOnMs {
		return true
	}
	if ewma < fwClutchOffMs {
		return false
	}
	return engaged
}

// fwLagNext folds one observed inter-tick interval (ms) into the running EWMA
// used by the intake clutch (3e8b). A zero prior -- fresh state, or just after
// an idle reset -- SEEDS the EWMA to obs directly rather than smoothing toward
// 0, so a single observation after a gap isn't misread as a lag spike. Pure
// and clock-free: callers own the only time.Now() read.
func fwLagNext(ewma, obs float64) float64 {
	if ewma == 0 {
		return obs
	}
	return ewma + fwLagAlpha*(obs-ewma)
}

func refresh() tea.Msg {
	ports, err := portscan.List()
	if err != nil {
		return refreshMsg{err: err}
	}
	// One serve-status fetch reconciles both serve and funnel (e40f dedupe).
	activeList, funnel, err := tsserve.Status()
	if err != nil {
		return refreshMsg{err: err}
	}
	active := make(map[int]bool, len(activeList))
	for _, p := range activeList {
		active[p] = true
	}
	return refreshMsg{ports: ports, active: active, funnel: funnel}
}

// autoRefresh is the periodic-poll variant: same read as refresh, but its
// result is flagged so a transient poll failure fades silently instead of
// nagging with a red toast every interval.
func autoRefresh() tea.Msg {
	msg, _ := refresh().(refreshMsg)
	msg.auto = true
	return msg
}

// fetchFQDN reads the node's MagicDNS name once; it's static for the session,
// so it's kept out of the periodic poll (the heaviest call -- it walks the
// netmap). Best-effort: an empty result just degrades public URLs to hostless.
func fetchFQDN() tea.Msg {
	fqdn, _ := tsserve.FQDN()
	return fqdnMsg{fqdn: fqdn}
}

// detectOperator runs the proactive, read-only operator check (kata tapv):
// batched into Init so the sticky hint can appear before the user's first
// space-press, and re-run on a manual "r" refresh so fixing the operator
// (then pressing r) clears the banner without needing another failed
// attempt first.
func detectOperator() tea.Msg {
	notSet, ok := tsserve.DetectOperatorNotSet()
	return detectOperatorMsg{notSet: notSet, ok: ok}
}

func toggle(port int, turnOn bool) tea.Cmd {
	return func() tea.Msg {
		var err error
		if turnOn {
			err = tsserve.On(port)
		} else {
			err = tsserve.Off(port)
		}
		return toggleDoneMsg{port: port, err: err}
	}
}

// funnelCmd turns the public funnel for localPort on or off. Turning on
// exposes it to the internet at publicPort; turning off drops the public
// ingress and restores tailnet serve (see tsserve.FunnelOff). It reuses
// toggleDoneMsg, so completion clears m.pending and triggers a refresh just
// like a serve toggle.
func funnelCmd(localPort, publicPort int, turnOn bool) tea.Cmd {
	return func() tea.Msg {
		var err error
		if turnOn {
			err = tsserve.FunnelOn(localPort, publicPort)
		} else {
			err = tsserve.FunnelOff(localPort, publicPort)
		}
		return toggleDoneMsg{port: localPort, err: err}
	}
}

// publishPollInterval is the published-state poll cadence (kata v1z5 step 5).
// Deliberately slower than refreshInterval: each poll is a remote round-trip to
// the Caddy edge over the tailnet, not a cheap local `ss`/serve-status read.
const publishPollInterval = 15 * time.Second

// publishTick schedules the next published-state poll. Unlike the egg/fireworks
// tickers it NEVER stops (the handler reschedules unconditionally); when
// publishing is unconfigured the poll it fires is simply a nil cmd, so the only
// cost is the timer itself.
func publishTick() tea.Cmd {
	return tea.Tick(publishPollInterval, func(time.Time) tea.Msg { return publishTickMsg{} })
}

// shortLabel derives this machine's short MagicDNS label -- the first
// `.`-component of its FQDN (from Self.DNSName). This is the backend label the
// Caddy edge dials (`<label>:<port>`), and the key the poll filters live routes
// by. Returns "" for an empty fqdn (tailscale down / not yet resolved), which
// the callers guard against.
func shortLabel(fqdn string) string {
	return strings.SplitN(fqdn, ".", 2)[0]
}

// caddyClient builds the edge client from cfg.Caddy (AdminURL from
// hostname:admin_port, ServerName from server_name), or returns the test
// override when one is injected. The nil HTTPClient means caddyedge's own 5s
// default timeout bounds each call.
func (m model) caddyClient() *caddyedge.Client {
	if m.caddyClientOverride != nil {
		return m.caddyClientOverride
	}
	return &caddyedge.Client{
		AdminURL:   "http://" + m.cfg.Caddy.Hostname + ":" + strconv.Itoa(m.cfg.Caddy.AdminPort),
		ServerName: m.cfg.Caddy.ServerName,
	}
}

// pollPublishedCmd is the published-state poll (kata v1z5 step 5): a remote
// List() against the Caddy edge, filtered to routes whose backend label is THIS
// machine's short label and keyed by local port. It returns a nil cmd -- zero
// cost -- when publishing is unconfigured (blank caddy.domain), matching the
// config semantics. A failure degrades quietly (publishPollMsg carries the err;
// the handler keeps last-known state and sets publishReachable=false), never a
// toast.
func (m *model) pollPublishedCmd() tea.Cmd {
	if m.cfg.Caddy.Domain == "" {
		return nil
	}
	// Don't poll until we know our short backend label (roborev 0k12 #1a): the
	// Init poll races fqdn resolution, and a label-less poll would return an
	// empty map that -- landing after the fqdn-triggered poll -- overwrites
	// valid routes with nothing. fqdnMsg re-polls once the label is known.
	label := shortLabel(m.fqdn)
	if label == "" {
		return nil
	}
	client := m.caddyClient()
	// VERSION this poll so an out-of-order completion can't clobber newer state
	// (roborev 0k12 #1b). The bump persists: pollPublishedCmd has a pointer
	// receiver and every caller returns the mutated model.
	m.publishPollGen++
	gen := m.publishPollGen
	return func() tea.Msg {
		infos, err := client.List(context.Background())
		if err != nil {
			return publishPollMsg{err: err, gen: gen}
		}
		published := make(map[int]publishInfo, len(infos))
		for _, r := range infos {
			// Filter to OUR machine's routes AND only routes tailport owns
			// (roborev 0k12 #2): multiple tailport computers share one Caddy
			// server, distinguished by backend label + port; and a FOREIGN
			// route targeting this same backend (a manual Caddy edit, not an
			// @id-tagged tailport route) must not be mistaken for published --
			// it would block funnel and, on de-escalation, try to unpublish a
			// synthesized tailport-<host> id that doesn't exist.
			//
			// DELIBERATE v1 LIMITATION (roborev bps9 #1, mg's call): a foreign
			// route -- one that matches this backend's label:port but isn't
			// tailport's own @id-tagged route -- is filtered out here, not
			// merely unpublishable. That means it never lands in m.published,
			// so it can't participate in drift detection either: tailport
			// surfaces funnel<->tailport-publish dual exposure (both sides
			// under tailport's control), but a manual/foreign Caddy route is
			// not tracked at all, because "published" is defined solely as
			// "has a tailport-owned @id route" -- Caddy's foreign routes are
			// simply out of tailport's view in v1. roborev 874 flagged this as
			// a gap (funnelling an already-foreign-published port raises no
			// warning); mg decided to keep the owned-only filter rather than
			// add foreign-route surfacing / an ownership flag / a
			// funnel-block-on-foreign, so this comment documents that as an
			// intentional scope boundary, not an oversight.
			if label == "" || r.Label != label || !r.Owned {
				continue
			}
			published[r.Port] = publishInfo{hostname: r.Hostname, auth: r.Auth}
		}
		return publishPollMsg{published: published, gen: gen}
	}
}

// publishCmd auto-enables serve (when needed) and THEN publishes, sequentially,
// inside ONE tea.Cmd (kata v1z5 step 3). The order is load-bearing: serve must
// be on before the public route is created, so a serve-enable failure
// short-circuits and NO public route is ever created for a backend that isn't
// actually serving. This is deliberately not two concurrently-batched cmds.
func publishCmd(client *caddyedge.Client, hostname, label string, port int, auth *caddyedge.BasicAuth, enableServe bool) tea.Cmd {
	return func() tea.Msg {
		if enableServe {
			if err := tsserve.On(port); err != nil {
				return publishDoneMsg{port: port, err: err}
			}
		}
		return publishDoneMsg{port: port, err: client.Publish(context.Background(), hostname, label, port, auth)}
	}
}

// unpublishCmd removes a published route. caddyedge.Unpublish re-verifies live
// ownership (still tailport-owned, still pointing at this label:port) before
// deleting, and it NEVER touches serve state (kata v1z5): de-escalating the
// public route leaves the tailnet serve mapping exactly as it was.
func unpublishCmd(client *caddyedge.Client, hostname, label string, port int) tea.Cmd {
	return func() tea.Msg {
		return publishDoneMsg{port: port, err: client.Unpublish(context.Background(), hostname, label, port), unpublish: true}
	}
}

// inspectConflictCmd classifies a hostname conflict read-only (kata qfbf): it
// calls caddyedge.InspectConflict and returns an inspectConflictMsg so the model
// can render a specific refusal (or, on Kind==None, retry once). Nothing is
// mutated — this is a pure classification read. wantLabel/wantPort (kata vsx4
// #2) name the backend we're trying to publish, so InspectConflict can notice
// the live route already matches it and return None instead of a stale refusal.
func inspectConflictCmd(client *caddyedge.Client, port int, hostname, wantLabel string, wantPort int) tea.Cmd {
	return func() tea.Msg {
		info, err := client.InspectConflict(context.Background(), hostname, wantLabel, wantPort)
		return inspectConflictMsg{port: port, hostname: hostname, info: info, err: err}
	}
}

// purgeCmd force-deletes the conflicting route holding hostname and reports the
// captured bytes (kata 6n15). PurgeConflict re-verifies the live route against
// expect just before deleting, so a route approved under one confirm can't be
// silently deleted after it changed. It is reached ONLY behind the escalated
// confirm ladders; it never enables serve (that already happened) and never
// touches funnel state.
func purgeCmd(client *caddyedge.Client, hostname string, port int, expect caddyedge.PurgeExpect) tea.Cmd {
	return func() tea.Msg {
		captured, err := client.PurgeConflict(context.Background(), hostname, expect)
		return purgeDoneMsg{captured: captured, hostname: hostname, port: port, err: err, deletedDesc: purgeDescOf(expect), owned: expect.Owned}
	}
}

// purgeDescOf names the backend of the route a purge deleted, from the confirmed
// identity: its reverse_proxy dial (label:port) when parseable, else its handler
// name (a non-proxy foreign route), else "" (hostname alone). Used so the poof
// animates the DELETED route, not the take-over backend (roborev 65qc-#2).
func purgeDescOf(e caddyedge.PurgeExpect) string {
	if e.BackendParseable {
		return fmt.Sprintf("%s:%d", e.Label, e.Port)
	}
	return e.Handler
}

// restoreCmd runs the purge-undo (kata ttfh; design §3.6) as ONE command, in the
// load-bearing order A-before-B-before-C, classifying its result from the FINAL
// observed edge state rather than a guess about which step failed. It runs a
// VerifyRestore FIRST (kata 7jy2 FIX 1) so a lost-response step-B commit that a
// retry re-attempts is recognized as already-restored instead of being misread
// as a drifted take-over:
//
//   - (A) Unpublish our take-over. A clean delete or ErrNotFound ⇒ our route is
//     gone, proceed to B. ErrHostnameConflict ⇒ our take-over is STILL LIVE
//     (matcher/backend drifted, it refused to delete) ⇒ do NOT append (that
//     dual-exposes) → restoreTakeoverChanged. ErrUnreachable ⇒ retryable.
//   - (B) RestoreRoute the captured bytes (scan-refuse-append under one array
//     If-Match). ErrRestoreNameClaimed ⇒ the name was re-claimed → restoreClaimedByOther.
//     ErrUnreachable ⇒ retryable.
//   - (C) VerifyRestore — the full-array, content-verifying final read — maps to
//     the message. A C read failure after B landed is restoreUnverified (we did
//     append but can't confirm), never a dishonest "couldn't restore".
func restoreCmd(client *caddyedge.Client, hostname, label string, port int, captured json.RawMessage) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		// Idempotency guard (kata 7jy2 FIX 1): step B's POST can COMMIT server-side
		// and then have its response lost to a transport error, which today looks
		// retryable. On the user's retry, step A (Unpublish) would see the now-
		// restored route (our captured OLD backend under the same @id), refuse the
		// delete with ErrHostnameConflict, and misreport "your take-over changed
		// under you" — never noticing the restore already succeeded. So verify
		// FIRST: if the captured route is already present and SEMANTICALLY EQUAL,
		// the earlier B committed, so report success without re-running A/B. This is
		// uniform and idempotent: on the FIRST attempt our take-over (a different
		// backend under the same @id) is present, so VerifyRestore returns a
		// non-Restored state and the flow proceeds A→B→C normally; only after a
		// lost-response commit is it Restored. A preflight READ error, however, does
		// NOT fall through to A (roborev nk3b-#1): it stops here (retryable on a
		// transport error), because falling through would let A misreport a
		// committed-but-lost restore as a changed take-over.
		state, verr := client.VerifyRestore(ctx, hostname, captured)
		if verr != nil {
			// The preflight couldn't read the edge. Do NOT fall through to A
			// (roborev nk3b-#1): a transient verify failure on the retry-after-a-
			// committed-B path would let Unpublish see the already-restored route,
			// refuse with ErrHostnameConflict, and misreport restoreTakeoverChanged
			// — permanently clearing undo. A transport error stays retryable (keep
			// the slot); anything else surfaces as an error.
			if errors.Is(verr, caddyedge.ErrUnreachable) {
				return restoreDoneMsg{hostname: hostname, result: restoreUnreachable}
			}
			return restoreDoneMsg{hostname: hostname, result: restoreError, err: verr}
		}
		if state == caddyedge.Restored {
			return restoreDoneMsg{hostname: hostname, result: restoreRestored}
		}
		// A non-Restored, non-error state (on the first attempt, our take-over is
		// present as a different backend under the same @id) — proceed A→B→C.
		// (A) remove our take-over.
		switch err := client.Unpublish(ctx, hostname, label, port); {
		case err == nil, errors.Is(err, caddyedge.ErrNotFound):
			// take-over gone (or it never landed): safe to restore.
		case errors.Is(err, caddyedge.ErrHostnameConflict):
			return restoreDoneMsg{hostname: hostname, result: restoreTakeoverChanged}
		case errors.Is(err, caddyedge.ErrUnreachable):
			return restoreDoneMsg{hostname: hostname, result: restoreUnreachable}
		default:
			return restoreDoneMsg{hostname: hostname, result: restoreError, err: err}
		}
		// (B) re-create the captured route.
		switch err := client.RestoreRoute(ctx, hostname, captured); {
		case err == nil:
			// appended; verify the final state below.
		case errors.Is(err, caddyedge.ErrRestoreNameClaimed):
			return restoreDoneMsg{hostname: hostname, result: restoreClaimedByOther}
		case errors.Is(err, caddyedge.ErrUnreachable):
			return restoreDoneMsg{hostname: hostname, result: restoreUnreachable}
		default:
			return restoreDoneMsg{hostname: hostname, result: restoreError, err: err}
		}
		// (C) classify the final observed state and compute the message from it.
		state, err := client.VerifyRestore(ctx, hostname, captured)
		if err != nil {
			// B landed but we can't confirm — honest "restored but unverified",
			// never a dishonest "couldn't". C is deliberately not re-armed for
			// retry: a re-press would re-run B against our own just-restored route.
			return restoreDoneMsg{hostname: hostname, result: restoreUnverified}
		}
		switch state {
		case caddyedge.Restored:
			return restoreDoneMsg{hostname: hostname, result: restoreRestored}
		case caddyedge.RestoreUnclaimed:
			return restoreDoneMsg{hostname: hostname, result: restoreUnclaimed}
		case caddyedge.RestoreContentMismatch:
			return restoreDoneMsg{hostname: hostname, result: restoreContentMismatch}
		default: // RestoreClaimedByOther
			return restoreDoneMsg{hostname: hostname, result: restoreClaimedByOther}
		}
	}
}

// conflictRefusalText formats the one-line refusal for a classified hostname
// conflict (kata qfbf; design §3.0/§3.4), naming the current holder so the user
// knows exactly what to do. ourLabel is this machine's short backend label,
// the discriminator (design r2-m2) between our own publish and another
// machine's. Kind==None is handled by the caller (a bounded retry), not here.
func conflictRefusalText(info caddyedge.ConflictInfo, hostname, ourLabel string) string {
	switch info.Kind {
	case caddyedge.OwnedDiffBackend:
		if info.BackendParseable && info.Label == ourLabel {
			// Same machine, different local port: essentially already-published,
			// but from another of this machine's ports — name it.
			return fmt.Sprintf(":%d is already published to %s via your %s:%d — unpublish it first",
				info.Port, hostname, info.Label, info.Port)
		}
		// Another machine's/user's owned route. Do NOT auto-take-over (that's kata
		// 6n15); name that it belongs elsewhere.
		return fmt.Sprintf("%s is published by another machine (%s) — take it over is coming, for now remove it there",
			hostname, backendDesc(info))
	case caddyedge.IdHijacked:
		to := info.HijackedTo
		if to == "" {
			to = "a different route"
		}
		return fmt.Sprintf("your tailport route for %s was re-pointed at %s — resolve it in Caddy", hostname, to)
	case caddyedge.ForeignOverlap:
		return fmt.Sprintf("%s is held by a route tailport didn't create (%s) — resolve it in Caddy",
			hostname, backendDesc(info))
	default:
		// Defensive: an unexpected Kind (incl. None, which the caller handles)
		// still yields a plain, non-spinning refusal rather than a blank toast.
		return fmt.Sprintf("%s is already claimed on the edge — resolve it in Caddy", hostname)
	}
}

// backendDesc names a conflict holder's backend for a refusal: its reverse_proxy
// dial (label:port) when parseable, else its first handler name (static_response,
// file_server, …) for a non-proxy route, else a bare fallback.
func backendDesc(info caddyedge.ConflictInfo) string {
	if info.BackendParseable {
		return fmt.Sprintf("%s:%d", info.Label, info.Port)
	}
	if info.Handler != "" {
		return info.Handler
	}
	return "an unrecognized route"
}

// publishErrText maps a caddyedge failure onto a friendly one-line toast,
// keying off the package's sentinel errors with errors.Is.
func publishErrText(err error) string {
	switch {
	case errors.Is(err, caddyedge.ErrUnreachable):
		return "caddy edge unreachable — check caddy.hostname and that the edge is up"
	case errors.Is(err, caddyedge.ErrNotFound):
		return "route not found on the edge (already removed?)"
	case errors.Is(err, caddyedge.ErrConcurrentUpdate):
		return "caddy config changed concurrently — try again"
	default:
		// ErrHostnameConflict and any wrapped Caddy body already name the
		// conflicting backend / carry Caddy's own message.
		return err.Error()
	}
}

// copiedSuffix is the plain (unstyled) text of the inline copy confirmation
// (py5b), appended -- pre-styled bold-green via activeStyle -- to a state-C
// row's description when its portItem.justCopied is set. Kept as one
// constant so the width-fit check (inlineCopyFits) and the styled render
// (portItem.Description) can never drift out of sync about its width.
const copiedSuffix = "  ✓ copied"

// authGlyph{Emoji,Mono} mark a basic-auth-protected published row in place of
// the words "basic auth". Emoji-gated like the exposure markers (markerGlyph):
// a person for emoji terminals, a plain "@" (login-ish) for the mono fallback.
const (
	authGlyphEmoji = "👤"
	authGlyphMono  = "@"
)

// descTruncateStyle mirrors the style bubbles/list's DefaultDelegate.Render
// uses to compute its available text width (vendored
// github.com/charmbracelet/bubbles/list@v1.0.0, defaultitem.go: textwidth =
// list width - NormalTitle's left+right padding, applied to BOTH title and
// description). portDelegate never overrides Styles.NormalTitle (only swaps
// NormalTitle/NormalDesc for a dimmed row on a throwaway copy inside
// Render), so a freshly resolved list.NewDefaultDelegate()'s style is always
// the one actually in effect -- resolved once here rather than reconstructed
// on every call.
var descTruncateStyle = list.NewDefaultDelegate().Styles.NormalTitle

// Grid layout constants (9gys): minColWidth is the narrowest a single
// column's cell is ever allowed to be, maxCols caps how many side-by-side
// columns a very wide terminal ever grows to, and colGutter is the blank gap
// between adjacent columns. See gridCols/gridColWidth/gridRows.
const (
	minColWidth = 50
	maxCols     = 3
	colGutter   = 2
)

// gridCols returns how many side-by-side columns fit a terminal of the given
// width: roughly one per minColWidth cells, clamped to maxCols and never
// less than 1 (so a very narrow terminal still gets a single, ordinary
// column). Below minColWidth it's always 1.
//
// NOTE (boundary caveat): this is the naive width/minColWidth floor-division
// split the kata spec pins exact test values against (gridCols(100)==2,
// gridCols(150)==3, ...). Right AT those transition widths the resulting
// gridColWidth dips a couple of cells below minColWidth (e.g.
// gridColWidth(100, 2) == 49, gridColWidth(150, 3) == 48) before recovering
// a few columns later (>=102 and >=154 respectively) -- see
// TestGridColWidth. A stricter gridCols that floors on colWidth>=minColWidth
// at every width would change gridCols(100) to 1 and gridCols(150) to 2,
// contradicting the pinned test table, so this deliberately keeps the exact
// formula given rather than "fixing" it unilaterally; the undershoot is at
// most 2 cells and self-heals a few columns later.
func gridCols(width int) int {
	if width < minColWidth {
		return 1
	}
	c := width / minColWidth
	if c > maxCols {
		c = maxCols
	}
	if c < 1 {
		c = 1
	}
	return c
}

// gridColWidth is the per-column cell width given the terminal width and
// column count (colGutter-wide gutters between columns, none at the outer
// edges).
func gridColWidth(width, cols int) int {
	if cols < 1 {
		cols = 1
	}
	return (width - colGutter*(cols-1)) / cols
}

// gridRows is how many item rows fit a body of height h, given the
// delegate's per-item height and the spacing between items: r rows of
// itemHeight with (r-1) spacing gaps between them fit in h.
func gridRows(h, itemHeight, spacing int) int {
	unit := itemHeight + spacing
	if unit < 1 {
		unit = 1
	}
	r := (h + spacing) / unit
	if r < 1 {
		r = 1
	}
	return r
}

// gridPlacement maps a window-relative item index k (0-based) to its
// column-major (col, row) position in a grid with the given row count: a
// column fills top-to-bottom before the next column starts (like a
// newspaper), so the k-th item lands at column k/rows, row k%rows.
func gridPlacement(k, rows int) (col, row int) {
	if rows < 1 {
		rows = 1
	}
	return k / rows, k % rows
}

// gridDims computes the current grid layout from the model's terminal width
// and available body height: cols (gridCols), rows (gridRows, from the same
// body height resizeList gives m.list -- see listBodyHeight), and colWidth
// (gridColWidth). renderGrid, availableDescriptionWidth, and the Left/Right
// grid-nav keys all derive from this single computation so they can never
// disagree about the current layout.
func (m model) gridDims() (cols, rows, colWidth int) {
	cols = gridCols(m.width)
	colWidth = gridColWidth(m.width, cols)
	rows = gridRows(m.listBodyHeight(), m.delegate.Height(), m.delegate.Spacing())
	return cols, rows, colWidth
}

// availableDescriptionWidth returns the width (in cells) the list delegate
// truncates a row's title/description to, given the model's current PER-
// COLUMN width (9gys: multi-column layouts render each cell at colWidth, not
// the full terminal width) -- the same budget bubbles/list enforces at
// render time, so inlineCopyFits can decide whether the "✓ copied" suffix
// will actually be visible before copyURL appends it. At a single-column
// width this is identical to the pre-9gys width-based budget, since
// gridColWidth(width, 1) == width.
func (m *model) availableDescriptionWidth() int {
	_, _, colWidth := m.gridDims()
	return colWidth - descTruncateStyle.GetPaddingLeft() - descTruncateStyle.GetPaddingRight()
}

// inlineCopyFits reports whether appending copiedSuffix to a description of
// descWidth (its PLAIN, unstyled rendered width) would still fit within
// availWidth, the delegate's available title/description budget
// (availableDescriptionWidth). Pure and side-effect free so it's directly
// unit-testable: bubbles/list truncates descriptions END-first, so on a
// narrow terminal / long URL the appended suffix would be the FIRST thing
// clipped -- silently dropping the confirmation -- unless copyURL checks
// this first and falls back to the toast.
func inlineCopyFits(descWidth, availWidth int) bool {
	return descWidth+lipgloss.Width(copiedSuffix) <= availWidth
}

// httpURL builds an http:// URL for host:port, bracketing an IPv6 literal host
// (a bare "fe80::1" would otherwise make the trailing :port ambiguous).
func httpURL(host string, port int) string {
	if strings.Contains(host, ":") { // IPv6 literal needs brackets
		return fmt.Sprintf("http://[%s]:%d", host, port)
	}
	return fmt.Sprintf("http://%s:%d", host, port)
}

// copyTargetURL returns the clipboard URL for sel, chosen to actually resolve
// for the port's reach state: a tailnet-reachable port (reachTailnet) and every
// served/funnelled/stale (active) port copy the tailnet host URL; a LAN-only
// bind copies its real http://<lan-ip>:PORT; a localhost-only port or an
// offline favorite copies http://localhost:PORT instead of a dead tailnet URL.
// (Funnelled ports deliberately keep the tailnet form -- the toast names the
// public-vs-tailnet mismatch.) reachPublish is the one exception (d80p): it
// copies the exact public "https://<publishHostname>" the row shows, not the
// tailnet form, since that's the URL the port is actually reachable at from
// the public internet.
func (m *model) copyTargetURL(sel portItem) string {
	tailnetURL := fmt.Sprintf("http://%s:%d", m.host, sel.port.Number)
	switch sel.reach() {
	case reachLAN:
		if sel.port.BindHost == "" { // no real LAN address to offer; stay honest
			return tailnetURL
		}
		return httpURL(sel.port.BindHost, sel.port.Number)
	case reachLocalhost, reachOffline:
		return fmt.Sprintf("http://localhost:%d", sel.port.Number)
	case reachPublish:
		return "https://" + sel.publishHostname
	default: // reachTailnet, reachServed, reachFunnel, reachStale
		return tailnetURL
	}
}

// copyURL copies the selected port's URL to the clipboard and confirms the
// copy. The copied URL is reach-aware (copyTargetURL): the tailnet host form
// (http://<host>:<port>) for a tailnet-reachable, served, funnelled, or stale
// port; the real http://<lan-ip>:<port> for a LAN-only bind; and
// http://localhost:<port> for a localhost-only port or an offline favorite --
// never a dead tailnet URL for a port that can't actually be reached that way.
// A funnelled port still copies the tailnet form on purpose (the toast names
// the public-vs-tailnet mismatch). A published port copies its exact public
// "https://<publishHostname>" (d80p) -- shown==copied, unlike funnel. The
// inline "✓ copied" confirmation is now UNIVERSAL (vqa3) across every
// inlineCopyState() row -- reachLocalhost, reachLAN, reachTailnet,
// reachServed, reachPublish -- because each row's description already states
// exactly what got copied, so -- provided the annotation fits the terminal
// width (inlineCopyFits) -- the confirmation goes inline as a transient
// "✓ copied" on the row instead of the bottom-bar toast (py5b). The two
// remaining principled exceptions keep the toast: funnel (row shows the
// PUBLIC url but c copies the TAILNET url -- shown≠copied) and stale
// (dangling -- the copied URL resolves to nothing); offline has nothing live
// to copy either. A too-narrow row for an inline state also falls back to
// the toast.
func (m *model) copyURL(sel portItem) tea.Cmd {
	url := m.copyTargetURL(sel)

	if sel.inlineCopyState() && inlineCopyFits(lipgloss.Width(sel.plainDescription()), m.availableDescriptionWidth()) {
		m.copiedID++
		id := m.copiedID
		m.copiedPort = sel.port.Number
		// Clear any lingering toast so the two confirmation channels never
		// show at once (the KeyMsg handler already does this on every
		// keypress before dispatch, but copyURL is the one place that
		// decides inline-vs-toast, so it's made explicit here too).
		m.flash = ""
		m.flashLevel = flashInfo
		m.resizeList() // the toast may have been wrapped multi-line; give the list its rows back
		return tea.Batch(
			copyCmd(url),
			m.rebuildItems(), // immediate render of the new annotation
			tea.Tick(3*time.Second, func(time.Time) tea.Msg { return copiedExpireMsg{id: id} }),
		)
	}

	var flash tea.Cmd
	switch sel.reach() {
	case reachTailnet:
		// Wildcard/tailnet-IP bind: the copied http://<host>:<port> ALREADY
		// resolves across the tailnet (the app is bound 0.0.0.0:PORT), so this
		// is honest -- NOT "localhost only", and NOT "press space" (serving is
		// a no-op here, matching the space guard's "already on tailnet").
		flash = m.setFlash(fmt.Sprintf("copied — :%d is reachable on your tailnet at this URL", sel.port.Number), flashInfo)
	case reachLAN:
		// Bound to a specific LAN IP, not the tailnet: mirrors the space guard's
		// reachLAN message (ui.go ~2019) so c and space agree. Name the copied
		// URL so it's clear it's the real LAN address, not a dead tailnet one.
		flash = m.setFlash(fmt.Sprintf("copied %s — LAN only; serve can't reach this bind", url), flashWarn)
	case reachFunnel:
		// The row shows the PUBLIC funnel URL but c copies the TAILNET URL by
		// design; a bare inline ✓ would imply the public URL was copied. Funnel
		// is the one principled exception to universal inline (vqa3): a toast
		// that names what was actually copied.
		flash = m.setFlash(fmt.Sprintf("copied %s — the tailnet url (row shows the public funnel url)", url), flashInfo)
	default:
		// reachLocalhost / reachOffline: genuinely localhost-only (or a down
		// favorite) -- "press space to serve it" is TRUE here. reachServed /
		// reachPublish (the inline path didn't fit) / reachFunnel / reachStale
		// are all `active`, so keep the plain "copied ✓ url" confirmation.
		if sel.active {
			flash = m.setFlash("copied ✓  "+url, flashInfo)
		} else {
			flash = m.setFlash(fmt.Sprintf("copied %s — localhost only; press space to serve it", url), flashWarn)
		}
	}
	return tea.Batch(copyCmd(url), flash)
}

// copyCmd performs the clipboard write off the render path (it may shell out to
// a local helper). clip.Copy is best-effort and can't be confirmed, so it
// returns no message -- the toast set by copyURL is the user's feedback.
func copyCmd(url string) tea.Cmd {
	return func() tea.Msg {
		clip.Copy(url)
		return nil
	}
}

// eggCopy copies one of the Easter-egg links via the same vetted clip/OSC 52
// path as copyURL, and toasts -- without closing the overlay (28mv/2b4r).
func (m *model) eggCopy(url, domain string) tea.Cmd {
	return tea.Batch(copyCmd(url), m.setFlash("copied "+domain, flashInfo))
}

// setFlash shows a transient toast at the given severity and returns the
// command that expires it after a short delay -- longer for warn/error so
// they're readable, shorter for info. flashID tags the expiry so a newer toast
// supersedes rather than being cut short by an older timer. CRITICAL: callers
// MUST propagate the returned cmd (return/Batch it) or the toast never
// auto-expires.
func (m *model) setFlash(text string, level flashLevel) tea.Cmd {
	m.flashID++
	m.flash = text
	m.flashLevel = level
	// A long toast can wrap across multiple lines at the current width
	// (83wv pt2); re-reserve the list's height now so it never overlaps.
	m.resizeList()
	id := m.flashID
	d := 3 * time.Second
	if level != flashInfo {
		d = 5 * time.Second
	}
	return tea.Tick(d, func(time.Time) tea.Msg { return flashExpireMsg{id: id} })
}

// setErr is a convenience for the common "raise an auto-dismissing error
// toast" case. Returns the expiry cmd for the caller to propagate.
func (m *model) setErr(text string) tea.Cmd {
	return m.setFlash(text, flashError)
}

// saveConfig persists the registry and, on failure, raises an error toast,
// returning its expiry cmd (nil on success) for the caller to propagate.
func (m *model) saveConfig() tea.Cmd {
	if err := m.cfg.Save(); err != nil {
		return m.setErr(err.Error())
	}
	return nil
}

// rememberProcesses records the currently-listening process name of each
// FAVORITE port into its registry entry (meta.LastProcess), so a favorite that
// later goes down can still show "was <name>" rather than an anonymous "?".
// Only favorites persist in the view when down, so only they need it; scoping
// here also keeps the write rare. Returns true if any entry changed, so the
// caller can persist just once and not re-write config on a steady state.
func (m *model) rememberProcesses() bool {
	changed := false
	for _, p := range m.allPorts {
		if p.Process == "" {
			continue
		}
		meta, ok := m.cfg.Ports[p.Number]
		if !ok || !meta.Favorite {
			continue
		}
		if meta.LastProcess != p.Process {
			meta.LastProcess = p.Process
			m.cfg.Ports[p.Number] = meta
			changed = true
		}
	}
	return changed
}

// cleanupDangling turns off every serve mapping in ports, one at a time.
// It reports the ports it could not tear down (if any) as a single error.
func cleanupDangling(ports []int) tea.Cmd {
	return func() tea.Msg {
		var failed []string
		for _, p := range ports {
			if err := tsserve.Off(p); err != nil {
				failed = append(failed, strconv.Itoa(p))
			}
		}
		if len(failed) > 0 {
			return cleanupDoneMsg{err: fmt.Errorf("could not clean :%s", strings.Join(failed, ", :"))}
		}
		return cleanupDoneMsg{}
	}
}

// danglingPorts returns the sorted set of ports that are exposed via
// tailscale serve but have no local process listening (exposed &&
// !listening). These are the "connection refused" forwards.
func (m model) danglingPorts() []int {
	listening := make(map[int]bool, len(m.allPorts))
	for _, p := range m.allPorts {
		listening[p.Number] = true
	}
	var ports []int
	for p, active := range m.active {
		if active && !listening[p] {
			ports = append(ports, p)
		}
	}
	sort.Ints(ports)
	return ports
}

// hasDangling reports whether any dangling forward exists.
func (m model) hasDangling() bool {
	return len(m.danglingPorts()) > 0
}

// undoStackLimit caps how many registry edits are held. Deep enough that a
// real editing session never hits it, bounded so a long-lived session can't
// grow the stacks without limit.
const undoStackLimit = 50

// portState is one port's registry entry, plus whether it existed at all --
// "no entry" and "an entry with every field zero" are different states
// (rebuildItems and the Favorites view both distinguish a known port from an
// unknown one), so a bare PortMeta can't represent the difference.
type portState struct {
	meta    config.PortMeta
	present bool
}

// registryEdit is one undoable change to one port's registry entry: what it
// was, what it became, and a human phrase for the toast ("favorite :8080").
type registryEdit struct {
	port   int
	before portState
	after  portState
	desc   string
}

// portState reads the current registry state of port.
func (m model) portState(port int) portState {
	meta, ok := m.cfg.Ports[port]
	return portState{meta: meta, present: ok}
}

// applyPortState writes s back over port's registry entry, deleting the entry
// when s recorded "no entry at all".
func (m *model) applyPortState(port int, s portState) {
	if !s.present {
		delete(m.cfg.Ports, port)
		return
	}
	if m.cfg.Ports == nil {
		m.cfg.Ports = map[int]config.PortMeta{}
	}
	m.cfg.Ports[port] = s.meta
}

// pushUndo records a deliberate registry edit, to be called with the port's
// BEFORE state captured and the edit already applied. New edits clear the redo
// stack -- the standard rule, and the honest one: once the registry has moved
// on, the redo entries describe a history that no longer happened.
//
// Only user-initiated edits go through here. remember()/rememberProcesses()
// deliberately do NOT: they're bookkeeping the user never asked for, and
// letting a background refresh push undo entries would make "u" step through
// changes nobody made.
func (m *model) pushUndo(port int, before portState, desc string) {
	m.undoStack = append(m.undoStack, registryEdit{
		port:   port,
		before: before,
		after:  m.portState(port),
		desc:   desc,
	})
	if len(m.undoStack) > undoStackLimit {
		m.undoStack = m.undoStack[len(m.undoStack)-undoStackLimit:]
	}
	m.redoStack = nil
}

// wouldUnlockSSH reports whether moving port to s would strip the lock from
// :22 -- the one registry change that is gated behind a typed confirm (ah23,
// the entryConfirmUnlockSSH flow), because :22's lock guards SSH access.
//
// undo/redo must not become a back door around that gate: a user who locked
// :22 this session could otherwise unlock it with a single unprompted "u".
// The invariant is that undo/redo may never land the registry in a state the
// user couldn't have reached by pressing keys directly without a confirm.
func (m model) wouldUnlockSSH(port int, s portState) bool {
	if port != 22 {
		return false
	}
	return m.portState(port).meta.Locked && !(s.present && s.meta.Locked)
}

// historyDirection selects which way stepHistory walks.
type historyDirection int

const (
	undoDirection historyDirection = iota
	redoDirection
)

// stepHistory pops one registry edit off the undo (or redo) stack, applies the
// state for that direction, and pushes the edit onto the opposite stack. Both
// directions are the same move -- restore one port to a recorded state -- so
// they share an implementation rather than drifting apart.
func (m model) stepHistory(dir historyDirection) (tea.Model, tea.Cmd) {
	from, to := &m.undoStack, &m.redoStack
	verb, none := "undo", "nothing to undo"
	if dir == redoDirection {
		from, to = &m.redoStack, &m.undoStack
		verb, none = "redo", "nothing to redo"
	}
	if len(*from) == 0 {
		return m, m.setFlash(none, flashInfo)
	}

	edit := (*from)[len(*from)-1]
	target := edit.before
	if dir == redoDirection {
		target = edit.after
	}

	// The one state undo/redo may not reach on its own (see wouldUnlockSSH):
	// refuse and leave both stacks untouched, so the edit stays available once
	// the user unlocks :22 deliberately. Refusing rather than re-prompting
	// keeps the confirm flow in one place -- x -- instead of growing a second
	// entry point through the history stack.
	if m.wouldUnlockSSH(edit.port, target) {
		return m, m.setFlash(fmt.Sprintf("%s would unlock :22 — use x to unlock SSH deliberately", verb), flashWarn)
	}

	*from = (*from)[:len(*from)-1]
	m.applyPortState(edit.port, target)
	*to = append(*to, edit)

	return m, tea.Batch(
		m.saveConfig(),
		m.rebuildItems(),
		m.setFlash(fmt.Sprintf("%s: %s", verb, edit.desc), flashInfo),
	)
}

// remember ensures port has a registry entry (creating a bare one if
// needed) and persists it. Called whenever a port is toggled on, so it
// stays visible in the default view even after being toggled back off.
func (m *model) remember(port int) tea.Cmd {
	if m.cfg.Ports == nil {
		m.cfg.Ports = map[int]config.PortMeta{}
	}
	if _, ok := m.cfg.Ports[port]; ok {
		return nil
	}
	m.cfg.Ports[port] = config.PortMeta{}
	return m.saveConfig()
}

// favorite registers port with Favorite=true, preserving any existing label
// or lock, and persists it. Backs the "n" add-port flow (ykgj): the port
// sticks in the Favorites view even before its service is running, ready to be
// served with space once it is.
func (m *model) favorite(port int) tea.Cmd {
	if m.cfg.Ports == nil {
		m.cfg.Ports = map[int]config.PortMeta{}
	}
	meta := m.cfg.Ports[port]
	meta.Favorite = true
	m.cfg.Ports[port] = meta
	return m.saveConfig()
}

// requestToggle begins toggling a port on/off from either entry point (the
// space handler or the "n" add-port submit). It enforces the lock guard
// (turning a locked port on is refused) and interposes the :22 SSH confirm:
// for port 22 -- in either direction, since turning serve off is what kicks
// you off SSH -- it opens an entryConfirm22 prompt and returns a nil cmd,
// deferring the actual toggle to the y/n handler. Every other port toggles
// immediately. The :22 confirm is independent of the lock mechanism.
func (m *model) requestToggle(port int, turnOn bool) tea.Cmd {
	if turnOn && m.cfg.Ports[port].Locked {
		return m.setErr(fmt.Sprintf("port :%d is locked -- press x to unlock", port))
	}
	if port == 22 {
		m.confirmPort = port
		m.confirmTurnOn = turnOn
		m.mode = entryConfirm22
		return nil
	}
	return m.beginToggle(port, turnOn)
}

// beginToggle records the pending toggle and returns the command that runs
// it. Callers must have already cleared the lock and :22 guards (see
// requestToggle and the entryConfirm22 handler).
func (m *model) beginToggle(port int, turnOn bool) tea.Cmd {
	var saveCmd tea.Cmd
	if turnOn {
		saveCmd = m.remember(port)
	}
	m.pending = port
	return tea.Batch(saveCmd, toggle(port, turnOn))
}

// requestFunnel begins toggling the public funnel for a port (the "P" key,
// swapped from "p" under vzj4).
// Turning ON is the escalation to the public internet, so it's hard-blocked
// for :22 (SSH), refused when all three ingress ports are taken, and
// otherwise deferred to a strong y/n confirm (entryConfirmFunnel). Turning
// OFF is de-escalation back to tailnet-served and runs immediately. Returns a
// nil cmd whenever it defers or refuses.
func (m *model) requestFunnel(port int) tea.Cmd {
	if pub, on := m.funnel[port]; on {
		// Already public -> drop back to tailnet-served. No confirm: this
		// reduces exposure.
		return m.beginFunnel(port, pub, false)
	}
	// Mutual-exclusion mirror guard (kata v1z5): funnel refuses a port that is
	// currently Caddy-published. Funnel and publish are two INDEPENDENT public
	// paths, never layered or ranked -- so escalating a published port to also
	// carry a funnel is refused, with the same no-ranking treatment publish
	// gives a funnelled port. The user removes the other exposure first.
	if info, ok := m.published[port]; ok {
		return m.setErr(fmt.Sprintf("port :%d is published to the internet (https://%s) — unpublish it first (p) before funnelling", port, info.hostname))
	}
	if port == 22 {
		return m.setErr("refusing to funnel :22 (SSH) to the public internet")
	}
	pub, ok := m.nextFunnelPort()
	if !ok {
		return m.setErr("all funnel ingress ports (443, 8443, 10000) are in use -- tailscale allows at most three")
	}
	m.funnelPort = port
	m.funnelPublic = pub
	m.funnelTurnOn = true
	m.mode = entryConfirmFunnel
	return nil
}

// beginFunnel records the pending funnel op and returns the command that runs
// it. Turning on remembers the port (so it stays visible once toggled back
// down), mirroring beginToggle.
func (m *model) beginFunnel(localPort, publicPort int, turnOn bool) tea.Cmd {
	var saveCmd tea.Cmd
	if turnOn {
		saveCmd = m.remember(localPort)
	}
	m.pending = localPort
	return tea.Batch(saveCmd, funnelCmd(localPort, publicPort, turnOn))
}

// selectedProcess returns the process name discovered for a local port (from
// the last portscan), or "" if the port isn't currently listening. Used to
// sharpen the funnel confirm's HTTP heuristic.
func (m model) selectedProcess(port int) string {
	for _, p := range m.allPorts {
		if p.Number == port {
			return p.Process
		}
	}
	return ""
}

// nextFunnelPort returns the lowest public ingress port not already in use by
// another funnel, in tsserve.FunnelPorts order (443 -> 8443 -> 10000). The
// bool is false when all three are taken (tailscale's per-node funnel limit).
func (m model) nextFunnelPort() (int, bool) {
	used := make(map[int]bool, len(m.funnel))
	for _, pub := range m.funnel {
		used[pub] = true
	}
	for _, p := range tsserve.FunnelPorts {
		if !used[p] {
			return p, true
		}
	}
	return 0, false
}

// looksHTTP is a best-effort guess at whether a port speaks HTTP, used only
// to decide whether the funnel confirm shows an extra caution line: funnel
// proxies HTTP(S) to the local target, so a non-HTTP service won't work.
// Deliberately conservative -- an unknown port gets the caution, not silence.
func looksHTTP(port int, process string) bool {
	switch port {
	case 80, 443, 3000, 3001, 4000, 4200, 5000, 5173, 8000, 8080, 8443, 8888, 9000:
		return true
	}
	p := strings.ToLower(process)
	for _, h := range []string{"http", "node", "next", "vite", "nginx", "caddy", "gunicorn", "uvicorn", "flask", "rails", "puma", "deno", "bun"} {
		if strings.Contains(p, h) {
			return true
		}
	}
	return false
}

// requestPublish is the `p` key's up-front gate (swapped from `P` under vzj4;
// kata v1z5 step 3), mirroring
// requestFunnel. It runs the local guards IN ORDER before opening the publish
// dialog (or, for de-escalation, before the immediate unpublish). Beyond these
// local guards, caddyedge.Publish itself enforces route ownership (a hostname
// owned by a different backend/port, or a foreign route, is a conflict with no
// mutation) -- that can't be pre-checked here since Caddy alone is the source of
// truth, so it surfaces later as a publishDoneMsg error. Returns a nil cmd
// whenever it defers to the dialog.
func (m *model) requestPublish(port int) tea.Cmd {
	// 1. busy: a toggle/funnel/publish is already in flight.
	if m.pending != 0 {
		return nil
	}
	// 2. :22 (SSH) is hard-blocked from the public internet, same as funnel.
	if port == 22 {
		return m.setErr("refusing to publish :22 (SSH) to the public internet")
	}
	// 3. empty fqdn: we derive the backend dial label from this machine's
	// MagicDNS name, so without it we can't build a route. Refuse.
	if m.fqdn == "" {
		return m.setErr("cannot determine this machine's tailnet name — is tailscale up?")
	}
	// 4. mutual exclusion: a funnelled port must lose the funnel first (no
	// ranking -- publish does not outrank funnel; they're independent paths).
	if pub, on := m.funnel[port]; on {
		return m.setErr(fmt.Sprintf("port :%d is funnelled (public %d) — remove the funnel first (P) before publishing", port, pub))
	}
	// 5. already published by tailport on THIS exact port -> de-escalation:
	// unpublish immediately, no confirm (reducing exposure is never gated).
	if info, ok := m.published[port]; ok {
		m.pending = port
		return unpublishCmd(m.caddyClient(), info.hostname, shortLabel(m.fqdn), port)
	}
	// 6. locked port: publish must not bypass the `x` lock any more than serve
	// or funnel do. Resolved BEFORE any domain handling (kata w131, ycv1 r1-#10)
	// so a locked-port (or otherwise un-publishable) user is refused OUTRIGHT --
	// never prompted for a domain and then refused.
	if m.cfg.Ports[port].Locked {
		return m.setErr(fmt.Sprintf("port :%d is locked — press x to unlock", port))
	}
	// All refuse-guards passed. Record the port ONCE, up front, so both the
	// hostname/domain-capture steps and the shared host-dialog continuation can
	// read it.
	m.publishPort = port

	// 7. FRESH setup (blank caddy.domain): capture the hostname FIRST (kata
	// ztzg), then the domain (kata w131). This runs BEFORE any hostname-validity
	// refusal on purpose: the prompt is prefilled with the current caddy.hostname
	// and validates the typed value (caddyedge.ValidLabel), so it IS the fix for
	// a blank or FQDN-shaped stored hostname -- refusing first would shadow the
	// very prompt meant to correct it, leaving hand-editing as the only recovery
	// for exactly those configs (roborev 6tas). Resolved LAST among the guards so
	// an un-publishable port (locked, :22, funnelled, …) was already refused
	// above rather than prompted then refused. Accepting the prefilled default is
	// a same-value no-op. Configured users (domain set) fall through untouched.
	if m.cfg.Caddy.Domain == "" {
		m.publishInput.Reset()
		m.publishInput.EchoMode = textinput.EchoNormal
		m.publishInput.Width = 40            // a normal padded field (the host step sets 0)
		m.publishInput.CharLimit = 63        // a single DNS label max, like the host step
		m.publishInput.Placeholder = "caddy" // a valid short label, for the rare case the user clears the field (roborev 452s)
		// Prefill the CURRENT caddy.hostname, falling back to the "caddy" default
		// when it's blank -- so the field holds a VALID, SUBMITTABLE value (Enter
		// accepts it and advances) rather than an empty field behind a
		// display-only placeholder, which validation rejects and, with the error
		// hidden mid-modal (0jjk), would look like Enter did nothing (roborev). A
		// blank stored hostname is unusual (config load defaults it to "caddy")
		// but reachable, so make it behave like the real default.
		prefill := m.cfg.Caddy.Hostname
		if prefill == "" {
			prefill = "caddy"
		}
		m.publishInput.SetValue(prefill)
		m.publishInput.CursorEnd()
		m.publishInput.Focus()
		m.mode = entryPublishHostname
		return nil
	}

	// 8. CONFIGURED (caddy.domain set): open the host dialog directly, with NO
	// hostname prompt -- so an invalid stored hostname can't be corrected inline
	// here and must be REFUSED. We dial the edge admin API by caddy.hostname, and
	// the edge derives its admin origin from the SHORT MagicDNS label only (ycv1
	// §4d): a blank hostname (it defaults to "caddy" at config load, so this is
	// unusual) leaves nothing to dial; an FQDN-shaped one (contains a dot) would
	// silently 403 at the edge. A configured user who hand-broke caddy.hostname
	// is pointed at the docs (the ztzg prompt is fresh-setup only).
	if m.cfg.Caddy.Hostname == "" {
		where := m.configPath
		if where == "" {
			where = "your tailport config"
		}
		return m.setErr(fmt.Sprintf("publish is unconfigured: set caddy.hostname in %s (see docs/caddy-edge.md)", where))
	}
	if strings.Contains(m.cfg.Caddy.Hostname, ".") {
		return m.setErr(fmt.Sprintf("caddy.hostname %q looks like an FQDN — use the short MagicDNS label (the edge admits only its short name; an FQDN silently 403s). See docs/caddy-edge.md", m.cfg.Caddy.Hostname))
	}
	return m.enterPublishHostDialog()
}

// enterPublishHostDialog opens the entryPublishHost step for m.publishPort. Only
// the editable LABEL goes in the input; ".<domain>" is a LOCKED suffix rendered
// after it (View) and re-appended on submit, so the field reads as one
// <label>.<domain> with the cursor sitting before the first dot ("expand out"
// from there). Prefill (the label) precedence: the port's user label, else its
// live process name, else this machine's short label. Single-label only for now
// -- a typed "." is refused and a dotted label is rejected on submit;
// ValidHostname checks the full host. This is the SHARED continuation (kata
// w131, ycv1 r2-#5) called from BOTH requestPublish (caddy.domain already set)
// and the entryPublishDomain enter-handler (caddy.domain just saved) -- both set
// m.publishPort first, which this reads.
func (m *model) enterPublishHostDialog() tea.Cmd {
	port := m.publishPort
	prefix := m.cfg.Ports[port].Label
	if prefix == "" {
		prefix = m.selectedProcess(port)
	}
	if prefix == "" {
		prefix = shortLabel(m.fqdn)
	}
	m.publishInput.EchoMode = textinput.EchoNormal
	// Width 0 = no field padding, so the locked ".<domain>" suffix (rendered in
	// View) sits flush against the label instead of after ~40 blank columns.
	// (Restored to 40 on the domain/cred steps that share this input.)
	m.publishInput.Width = 0
	// A single DNS label maxes at 63 chars -- bound the buffer to that (the full
	// hostname's 253 limit is for the domain step). This also caps how far an
	// over-long label can run now that Width 0 does no horizontal clipping.
	m.publishInput.CharLimit = 63
	// Label only; the ".<domain>" suffix is locked (rendered in View, re-appended
	// on submit). Cursor at the label's end sits right before that first dot.
	m.publishInput.SetValue(prefix)
	m.publishInput.CursorEnd()
	m.publishInput.Focus()
	m.mode = entryPublishHost
	return nil
}

// validPublishDomain reports whether s is usable as the public BASE domain that
// publish hostnames are built under (label + "." + domain). It reuses
// caddyedge.ValidHostname (already rejecting blank, whitespace, schemes, ports,
// paths, bad labels, and "*.x") and ADDS a "must contain at least one dot" rule:
// a bare label like "localhost" or "foo" can't be a real public base domain and
// would build a bogus "label.foo" publish host, so it's rejected here even
// though it's a syntactically valid single-label hostname. "example.com" and
// "apps.example.com" pass. (kata w131.)
func validPublishDomain(s string) bool {
	return caddyedge.ValidHostname(s) && strings.Contains(s, ".")
}

// clearPublishFlow resets every publish-flow field to its zero value and
// returns the input to entryNone. CRITICAL: it zeroes publishCredUser and
// publishCredPass, so an aborted (or completed) flow never leaves the plaintext
// basic-auth password sitting in the model. Called on esc at every step, on the
// 3-way auth gate's esc, and at confirm-time once the password has been hashed.
func (m *model) clearPublishFlow() {
	m.mode = entryNone
	m.publishInput.Reset()
	m.publishInput.EchoMode = textinput.EchoNormal
	m.publishPort = 0
	m.publishHostname = ""
	m.publishWithAuth = false
	m.publishCredUser = ""
	m.publishCredPass = ""
	m.publishEnableServe = false
}

// updatePublishEntry drives the publish dialog's state machine (kata v1z5 step
// 3). It owns every key for the publish modes -- including feeding the text
// steps into publishInput -- so keystrokes never leak into labelInput via the
// shared entry-mode fallthrough. esc aborts (clearing the plaintext credential)
// at every step.
func (m *model) updatePublishEntry(msg tea.KeyMsg) tea.Cmd {
	switch m.mode {
	case entryPublishHostname:
		// Hostname-capture assist (kata ztzg): reached only on FRESH setup, the
		// SAME blank-caddy.domain condition that opens the domain-capture step
		// below -- hostname runs FIRST because it's needed to reach the edge's
		// admin API at all. The input is prefilled with the CURRENT
		// caddy.hostname (default "caddy"), so hitting enter unedited is a
		// same-value no-op. esc aborts; enter validates a single MagicDNS label
		// (caddyedge.ValidLabel -- blank or dotted/FQDN-shaped both fail, which
		// is exactly the silent-403 bug class this prompt exists to prevent),
		// persists caddy.hostname ONLY when the value actually CHANGED (skipping
		// a needless disk write + .bak otherwise), then feeds the (unchanged)
		// domain-capture step.
		switch msg.String() {
		case "esc":
			m.clearPublishFlow()
			return nil
		case "enter":
			hostname := strings.TrimSpace(m.publishInput.Value())
			if !caddyedge.ValidLabel(hostname) {
				return m.setErr(fmt.Sprintf("invalid tailnet hostname %q — enter the edge's short MagicDNS label (letters, digits, hyphens; no dots)", hostname))
			}
			if hostname != m.cfg.Caddy.Hostname {
				if err := m.cfg.SaveCaddyHostname(hostname); err != nil {
					m.clearPublishFlow()
					return m.setErr("could not save caddy.hostname: " + err.Error())
				}
				// SaveCaddyHostname persists to DISK ONLY -- mirror it in memory.
				m.cfg.Caddy.Hostname = hostname
			}
			// Feed the (unchanged) domain-capture step, matching requestPublish's
			// own setup of it exactly.
			m.publishInput.Reset()
			m.publishInput.EchoMode = textinput.EchoNormal
			m.publishInput.Width = 40      // a normal padded field (the host step sets 0)
			m.publishInput.CharLimit = 253 // full-hostname limit (the host step sets 63)
			m.publishInput.Placeholder = "example.com"
			m.publishInput.Focus()
			m.mode = entryPublishDomain
			return nil
		}
		var cmd tea.Cmd
		m.publishInput, cmd = m.publishInput.Update(msg)
		return cmd

	case entryPublishDomain:
		// Domain-capture assist (kata w131, ycv1 §4b): reached only when
		// caddy.domain was blank, so the base domain is gathered inline instead
		// of refusing the publish. esc aborts; enter validates the base domain,
		// persists ONLY caddy.domain (targeted merge + .bak, OQ3) and then feeds
		// the shared host-dialog continuation. Persisting BEFORE continuing is
		// deliberate: we never open the host step against a domain that isn't on
		// disk -- a save failure ABORTS rather than publishing an unpersisted
		// domain.
		switch msg.String() {
		case "esc":
			m.clearPublishFlow()
			return nil
		case "enter":
			domain := strings.TrimSpace(m.publishInput.Value())
			if !validPublishDomain(domain) {
				return m.setErr(fmt.Sprintf("invalid base domain %q — enter a dotted public domain like example.com or apps.example.com", domain))
			}
			if err := m.cfg.SaveCaddyDomain(domain); err != nil {
				m.clearPublishFlow()
				return m.setErr("could not save caddy.domain: " + err.Error())
			}
			// SaveCaddyDomain persists to DISK ONLY -- mirror it in memory.
			m.cfg.Caddy.Domain = domain
			// Raise the parallel sticky setup-banner (ycv1 r3-NEW-1): the field
			// is saved, but the user still owes *.<domain> DNS + a deployed edge.
			m.domainSetupPending = true
			return m.enterPublishHostDialog()
		}
		var cmd tea.Cmd
		m.publishInput, cmd = m.publishInput.Update(msg)
		return cmd

	case entryPublishHost:
		switch msg.String() {
		case "esc":
			m.clearPublishFlow()
			return nil
		case ".":
			// The ".<domain>" suffix is locked and lives OUTSIDE the buffer
			// (rendered in View), so a typed "." would only start an unsupported
			// nested label. Refuse it silently, like any rejected keystroke.
			return nil
		case "enter":
			// The buffer holds the label only; re-append the locked domain suffix.
			label := strings.TrimSpace(m.publishInput.Value())
			if label == "" {
				return m.setErr("enter a hostname label before the domain")
			}
			if strings.Contains(label, ".") {
				// Guards a dotted prefill or a pasted label (the "." keystroke is
				// already blocked above): single <label>.<domain> only for now.
				return m.setErr("one label only — nested subdomains aren't supported yet")
			}
			host := label + "." + m.cfg.Caddy.Domain
			if !caddyedge.ValidHostname(host) {
				return m.setErr(fmt.Sprintf("invalid public hostname: %q", host))
			}
			m.publishHostname = host
			m.mode = entryPublishAuth
			return nil
		}
		var cmd tea.Cmd
		m.publishInput, cmd = m.publishInput.Update(msg)
		return cmd

	case entryPublishAuth:
		// 3-way gate (a documented deviation from the 2-way y/n confirms): y
		// requires auth, n publishes with NO auth, esc aborts -- so "no auth" is
		// distinct from "cancel". Other keys are ignored (stay on the gate).
		switch msg.String() {
		case "y", "Y":
			m.publishWithAuth = true
			if m.cfg.Caddy.AuthHash == "" {
				// First authed publish: gather the single shared credential.
				m.publishInput.Reset()
				m.publishInput.EchoMode = textinput.EchoNormal
				m.publishInput.Width = 40      // padded field (the host step sets 0)
				m.publishInput.CharLimit = 253 // full limit (the host step sets 63)
				m.publishInput.Placeholder = "username"
				m.publishInput.Focus()
				m.mode = entryPublishCredUser
				return nil
			}
			// A shared credential already exists -> reuse it, skip the cred steps.
			return m.enterConfirmPublish()
		case "n", "N":
			m.publishWithAuth = false
			return m.enterConfirmPublish()
		case "esc":
			m.clearPublishFlow()
			return nil
		default:
			return nil
		}

	case entryPublishCredUser:
		switch msg.String() {
		case "esc":
			m.clearPublishFlow()
			return nil
		case "enter":
			user := strings.TrimSpace(m.publishInput.Value())
			if user == "" {
				return m.setErr("username required (or esc to cancel)")
			}
			m.publishCredUser = user
			m.publishInput.Reset()
			m.publishInput.EchoMode = textinput.EchoPassword // mask the password
			m.publishInput.Width = 40                        // padded field (the host step sets 0)
			m.publishInput.CharLimit = 253                   // full limit (the host step sets 63)
			m.publishInput.Placeholder = "password"
			m.publishInput.Focus()
			m.mode = entryPublishCredPass
			return nil
		}
		var cmd tea.Cmd
		m.publishInput, cmd = m.publishInput.Update(msg)
		return cmd

	case entryPublishCredPass:
		switch msg.String() {
		case "esc":
			m.clearPublishFlow()
			return nil
		case "enter":
			// Don't TrimSpace a password -- spaces can be significant.
			pass := m.publishInput.Value()
			if pass == "" {
				return m.setErr("password required (or esc to cancel)")
			}
			m.publishCredPass = pass
			m.publishInput.Reset()
			m.publishInput.EchoMode = textinput.EchoNormal
			m.publishInput.Placeholder = "host.example.com"
			return m.enterConfirmPublish()
		}
		var cmd tea.Cmd
		m.publishInput, cmd = m.publishInput.Update(msg)
		return cmd

	case entryConfirmPublish:
		// Funnel-grade y/n: y confirms, every other key cancels.
		switch msg.String() {
		case "y", "Y":
			return m.confirmPublish()
		default:
			m.clearPublishFlow()
			return nil
		}
	}
	return nil
}

// enterConfirmPublish sets up the funnel-grade confirm step, recording whether
// the confirm will also turn serve on (serve isn't already active for the port)
// so the confirm can say so.
func (m *model) enterConfirmPublish() tea.Cmd {
	m.publishEnableServe = !m.active[m.publishPort]
	m.mode = entryConfirmPublish
	return nil
}

// confirmPublish is the entryConfirmPublish "yes" path. On a first authed
// publish it bcrypt-hashes the gathered password AT CONFIRM-TIME (sync, one-off)
// and persists the shared credential SYNCHRONOUSLY -- a save failure ABORTS the
// publish (roborev 0k12 #3) rather than leaving a live authenticated route whose
// credential exists only in memory. remember() can't do this write (it's a no-op
// for an already-known port and would skip the hash), hence the explicit Save.
// Only after a successful save does it hand off to the single sequential publish
// cmd (serve-then-publish). The plaintext password is dropped by clearPublishFlow
// before the op runs.
func (m *model) confirmPublish() tea.Cmd {
	// A NEW user-initiated publish clears any armed restore affordance (kata ttfh;
	// design §3.6). The in-transaction take-over resume publishes via publishCmd
	// directly (never through here), so it is inherently exempt from this trigger.
	m.clearLastPurge()
	port := m.publishPort
	hostname := m.publishHostname
	enableServe := m.publishEnableServe

	// Guard 3 re-check FIRST: never build a route dialling ":port" with no
	// label -- and never persist a credential for a publish we'd then abort.
	if m.fqdn == "" {
		m.clearPublishFlow()
		return m.setErr("cannot determine this machine's tailnet name — is tailscale up?")
	}

	var auth *caddyedge.BasicAuth
	if m.publishWithAuth {
		if m.publishCredPass != "" {
			// First authed publish gathered a new shared credential: hash it now.
			hash, err := bcrypt.GenerateFromPassword([]byte(m.publishCredPass), bcrypt.DefaultCost)
			if err != nil {
				m.clearPublishFlow()
				return m.setErr("could not hash password: " + err.Error())
			}
			// Persist BEFORE publishing (roborev 0k12 #3): if the save fails,
			// restore the previous in-memory credential so the model isn't left
			// half-set, surface the error, and do NOT publish -- so a lost
			// on-disk credential can never accompany a live public route.
			prevUser, prevHash := m.cfg.Caddy.AuthUser, m.cfg.Caddy.AuthHash
			m.cfg.Caddy.AuthUser = m.publishCredUser
			m.cfg.Caddy.AuthHash = string(hash)
			if err := m.cfg.Save(); err != nil {
				m.cfg.Caddy.AuthUser, m.cfg.Caddy.AuthHash = prevUser, prevHash
				m.clearPublishFlow()
				return m.setErr(err.Error())
			}
		}
		auth = &caddyedge.BasicAuth{User: m.cfg.Caddy.AuthUser, Hash: m.cfg.Caddy.AuthHash}
	}

	label := shortLabel(m.fqdn)
	client := m.caddyClient()

	// Carry the (secret-free) publish parameters so the conflict path can name
	// the attempted hostname and, on a cleared conflict (Kind==None), re-issue
	// the publish once (kata qfbf). retried starts false: this is a fresh attempt.
	m.pendingPublish = pendingPublish{hostname: hostname, label: label, port: port, withAuth: auth != nil}

	// Drop the flow state (esp. the plaintext password) BEFORE the op runs.
	m.clearPublishFlow()
	m.pending = port
	return publishCmd(client, hostname, label, port, auth, enableServe)
}

// clearPurgeFlow resets the force-purge / take-over confirm state (kata 6n15) to
// zero and returns the input to entryNone. It does NOT clear m.pendingPublish:
// on CONFIRM that carry resumes the takeover; on CANCEL it is harmless (stale
// only until the next confirmPublish, which overwrites it, and it is consulted
// only on a fresh publishDoneMsg conflict). Called on cancel at every gate and
// at confirm-time.
func (m *model) clearPurgeFlow() {
	m.mode = entryNone
	m.purgeHostname = ""
	m.purgePort = 0
	m.purgeInfo = caddyedge.ConflictInfo{}
	m.purgeExpect = caddyedge.PurgeExpect{}
	m.purgeInput.Reset()
}

// scheduleLastPurgeExpiry bumps the restore-affordance generation and schedules
// the ~60s idle timeout for it (kata ttfh; design OQ1/F11). Bumping the gen voids
// any previously scheduled expiry, so an arm, a re-arm, or a clear each supersede
// the last -- the flashExpireMsg/flashID pattern applied to the restore slot.
func (m *model) scheduleLastPurgeExpiry() tea.Cmd {
	m.lastPurgeGen++
	gen := m.lastPurgeGen
	return tea.Tick(lastPurgeTTL, func(time.Time) tea.Msg { return lastPurgeExpireMsg{gen: gen} })
}

// clearLastPurge drops the armed restore slot and its pre-arm stash and voids any
// pending idle timer (by bumping the gen). It is the single point every clear
// trigger routes through (kata ttfh; design §3.6): a successful restore, a new
// non-take-over publish/purge, a poll or C-read showing a third-party re-take,
// de-escalation of our take-over, navigation, or the ~60s timeout.
func (m *model) clearLastPurge() {
	m.lastPurge = nil
	m.pendingArm = nil
	m.restoring = false
	m.lastPurgeGen++ // void any in-flight expiry timer
	m.resizeList()   // give the status slot back the row the prompt held
}

// clearRestoreOnNav drops the restore affordance on selection-changing navigation
// intent (kata ttfh; design §3.6). The end-of-Update navigation clear covers keys
// that fall through to m.list.Update, but the grid-column case "left"/"right"
// returns EARLY — before that clear — so it (and any other early-returning
// navigation case) must route through this helper, else the affordance would stay
// armed after the selection moved (kata 7jy2 FIX 4). Suppressed while a restore is
// in flight: the slot must survive to catch its restoreDoneMsg / retry.
func (m *model) clearRestoreOnNav() {
	if m.lastPurge != nil && !m.restoring {
		m.clearLastPurge()
	}
}

// beginRestore launches the purge-undo A→B→C command (kata ttfh; design §3.6). It
// sets the in-flight guards (restoring + m.pending on our port) so a second R is a
// no-op and no other remote op races it, marks the take-over no-longer-live (step
// A is tearing it down, so a poll must not read the coming "no route of ours" as a
// third-party re-take), and SUSPENDS the idle timer by bumping the gen (re-armed
// only on a retryable outcome). The caller guards that lastPurge is armed and not
// already restoring.
func (m *model) beginRestore() tea.Cmd {
	lp := m.lastPurge
	m.restoring = true
	m.pending = lp.ourPort
	lp.takeoverLive = false // our take-over is being removed as part of the undo
	m.lastPurgeGen++        // suspend the idle timer while the restore is in flight
	return restoreCmd(m.caddyClient(), lp.hostname, lp.ourLabel, lp.ourPort, lp.captured)
}

// cancelPurgeFlow aborts a purge ladder (esc / any non-commit key at any gate)
// and flashes that serve was left on for the port (OQ-Serve; roborev hped #7).
// publishCmd turned serve ON before the publish that hit this conflict, so by the
// time a purge confirm shows serve is already active for the port; backing out
// here would otherwise leave it on silently — a footgun. The port is captured
// BEFORE clearPurgeFlow zeroes it.
//
// It also reconciles the serve state so "space to stop" is honest (roborev ve95
// FIX 4). The space toggle decides on/off from m.active[port], but that map can be
// STALE here — the 15s poll hasn't necessarily run since publishCmd auto-enabled
// serve — so a lingering false would make the very next space try to turn serve ON
// AGAIN instead of stopping it. Serve being ON is a known fact by this point (a
// serve-enable failure short-circuits publishCmd before any conflict), so hand-set
// m.active[port]=true now for an immediate, honest toggle, AND issue a refresh so
// the row reconciles to tailscale's live state — the same reconcile the toggle
// path relies on.
func (m *model) cancelPurgeFlow() tea.Cmd {
	port := m.purgePort
	m.clearPurgeFlow()
	var rebuild tea.Cmd
	if port != 0 {
		m.active[port] = true
		// Reflect serve=on in the cached row now so "space to stop" works before
		// the async refresh lands (roborev 2wts — the space toggle reads the
		// cached portItem.active, not m.active).
		rebuild = m.rebuildItems()
	}
	return tea.Batch(
		m.setFlash(fmt.Sprintf("serve left on for :%d — space to stop", port), flashWarn),
		rebuild, refresh,
	)
}

// refuseConflict builds the terminal toast for a conflict the user can't resolve
// through tailport (a hijacked @id, a non-disclosable foreign route, a
// cleared-then-returned conflict, or a failed classification). publishCmd enabled
// serve for the port BEFORE the conflict surfaced, so -- like cancelPurgeFlow --
// this reconciles m.active[port] and appends "serve left on … space to stop" to
// the refusal, so a single toast carries both the reason and the honest serve
// state rather than leaving serve on silently (roborev xzns). refresh + the
// published-state poll ride along.
func (m *model) refuseConflict(port int, msg string) tea.Cmd {
	full := msg
	var rebuild tea.Cmd
	if port != 0 {
		m.active[port] = true
		// Reflect serve=on in the CACHED row now (roborev 2wts): the space toggle
		// reads the selected portItem.active, not m.active, so without an immediate
		// rebuild pressing space before the async refresh lands would still see the
		// row as inactive and turn serve ON instead of stopping it.
		rebuild = m.rebuildItems()
		full = fmt.Sprintf("%s (serve left on for :%d — space to stop)", msg, port)
	}
	return tea.Batch(m.setErr(full), rebuild, refresh, m.pollPublishedCmd())
}

// purgeBlastRadiusLines renders the extra exposure a foreign purge would remove
// (roborev hped #3): a foreign route may match a bare "*" catch-all, a "*.suffix"
// wildcard, or several hostnames, but the confirm names only the requested host —
// so without this the user could delete unrelated public hostnames blind. It
// names every OTHER host pattern the route carries, and calls out a bare "*"
// catch-all prominently. Returns nil when the route serves only the requested
// hostname (nothing extra to disclose). Styled with warnStyle since the blast
// radius is a safety warning.
func (m *model) purgeBlastRadiusLines() []string {
	req := strings.ToLower(strings.TrimSpace(m.purgeHostname))
	var extras []string
	catchAll := false
	seen := map[string]bool{}
	for _, h := range m.purgeInfo.Hosts {
		hc := strings.ToLower(strings.TrimSpace(h))
		if hc == "*" {
			catchAll = true // named by the prominent catch-all line, not "also serves"
			continue
		}
		if hc == "" || hc == req || seen[hc] {
			continue
		}
		seen[hc] = true
		extras = append(extras, h)
	}
	var lines []string
	if catchAll {
		lines = append(lines, warnStyle.Render("   ⚠ CATCH-ALL (*): this route serves EVERY hostname on this edge"))
	}
	if len(extras) > 0 {
		lines = append(lines, warnStyle.Render("   this route also serves: "+strings.Join(extras, ", ")))
	}
	return lines
}

// updatePurgeEntry drives the two force-purge confirm ladders (kata 6n15; design
// §3.4), owning every key for the purge modes so keystrokes never leak into
// another input. OwnedDiffBackend is a single normal y/n; ForeignOverlap is the
// scary two-gate (a y/n drift warning, then a typed-"purge" commit modeled
// EXACTLY on entryConfirmUnlockSSH — only an exact, trimmed, case-insensitive
// "purge" commits; anything else, including empty/enter, cancels with no
// mutation).
func (m *model) updatePurgeEntry(msg tea.KeyMsg) tea.Cmd {
	switch m.mode {
	case entryConfirmPurgeOwned:
		// Normal y/n: y force-purges and takes over, every other key cancels.
		switch msg.String() {
		case "y", "Y":
			return m.confirmPurge()
		default:
			return m.cancelPurgeFlow()
		}
	case entryConfirmPurgeForeign:
		// Scary first gate: y advances to the typed-word commit, every other key
		// cancels with no mutation.
		switch msg.String() {
		case "y", "Y":
			m.purgeInput.Reset()
			m.purgeInput.Focus()
			m.mode = entryConfirmPurgeForeignType
			return nil
		default:
			return m.cancelPurgeFlow()
		}
	case entryConfirmPurgeForeignType:
		// Typed-word commit (mirror entryConfirmUnlockSSH): commit only on enter
		// with an exact "purge"; esc cancels; anything else feeds the input.
		switch msg.String() {
		case "enter":
			typed := strings.ToLower(strings.TrimSpace(m.purgeInput.Value()))
			if typed != "purge" {
				return m.cancelPurgeFlow() // wrong word (incl. empty): cancel, no mutation
			}
			return m.confirmPurge()
		case "esc":
			return m.cancelPurgeFlow()
		default:
			var cmd tea.Cmd
			m.purgeInput, cmd = m.purgeInput.Update(msg)
			return cmd
		}
	}
	return nil
}

// confirmPurge fires the force-purge on the final gate of either ladder: it
// launches purgeCmd with the re-verify identity (m.purgeExpect) and marks the
// port in-flight. m.pendingPublish (already set at confirmPublish-time, untouched
// through the conflict path) carries the secret-free params the purgeDoneMsg
// success handler resumes the takeover with.
func (m *model) confirmPurge() tea.Cmd {
	// A NEW purge clears any armed restore affordance from a PRIOR take-over (kata
	// ttfh; design §3.6). This transaction re-arms only later, at its own take-over
	// publishDoneMsg, so there is nothing of THIS transaction's to wipe here.
	m.clearLastPurge()
	hostname := m.purgeHostname
	port := m.purgePort
	expect := m.purgeExpect
	client := m.caddyClient()
	m.clearPurgeFlow()
	m.pending = port
	return purgeCmd(client, hostname, port, expect)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.help.Width = msg.Width
		m.width = msg.Width
		m.height = msg.Height
		m.resizeList()
		return m, nil

	case refreshMsg:
		if msg.err != nil {
			if msg.auto {
				return m, nil // periodic poll: fail silently, don't nag
			}
			return m, m.setErr(msg.err.Error())
		}
		m.allPorts = msg.ports
		m.active = msg.active
		m.funnel = msg.funnel
		// Remember the live process names of favorites BEFORE rebuilding, so a
		// favorite that later goes down can show "was <name>". Persist only when
		// something actually changed, so a steady state never re-writes config.
		changed := m.rememberProcesses()
		cmd := m.rebuildItems()
		if changed {
			return m, tea.Batch(cmd, m.saveConfig())
		}
		return m, cmd

	case fqdnMsg:
		if msg.fqdn != "" {
			hadFQDN := m.fqdn != ""
			m.fqdn = msg.fqdn
			// The Init poll fired before fqdn resolved, so it couldn't know our
			// short label. Now that we do, re-poll once so published markers
			// appear promptly instead of waiting a full 15s tick.
			if !hadFQDN {
				return m, m.pollPublishedCmd()
			}
		}
		return m, nil

	case detectOperatorMsg:
		// Only a CONCLUSIVE read (ok) ever changes the sticky hint: it can set
		// it (proactive detection at startup) or clear it (a re-check on "r"
		// confirming the user's `sudo tailscale set --operator=...` worked).
		// An inconclusive read leaves whatever catch-on-first-failure already
		// established alone.
		if msg.ok {
			m.operatorNotSet = msg.notSet
		}
		return m, nil

	case refreshTickMsg:
		// Always reschedule; poll only when idle so an auto-refresh never
		// stomps an in-flight toggle/cleanup. State reconciliation reuses the
		// same path as manual 'r', so it preserves selection.
		if m.pending != 0 || m.cleaning != 0 {
			return m, refreshTick()
		}
		return m, tea.Batch(autoRefresh, refreshTick())

	case eggTickMsg:
		// Advance the animation only while the overlay is open; once closed we
		// stop rescheduling so the ticker dies (no leak).
		if !m.showEgg {
			return m, nil
		}
		m.eggFrame++
		return m, eggTick()

	case fwTickMsg:
		// Step the fireworks on their own cadence. Stop (drop the ticker) when
		// the overlay closed or no fireworks remain, mirroring eggTick's
		// no-leak discipline. Exactly one fwTicker is ever live (see the 'f'
		// handler's fwTicking guard), so this never busy-loops or stacks.
		if !m.showEgg {
			m.fwTicking = false
			m.fireworks = nil
			// (3e8b) reset the clutch's lag state so a gap between egg
			// sessions is never later read as a giant lag spike.
			m.lastFwTick, m.fwLagEWMA, m.fwClutch = time.Time{}, 0, false
			return m, nil
		}
		// (3e8b) sensor: fold the observed inter-tick interval into the lag
		// EWMA and re-derive the clutch verdict. The first tick after warmup
		// only stamps lastFwTick (nothing to measure yet); the EWMA seeds on
		// the second. time.Now() is the only wall-clock read -- everything
		// else (fwLagNext/fwClutchNext) is pure.
		now := time.Now()
		if !m.lastFwTick.IsZero() {
			obs := float64(now.Sub(m.lastFwTick).Milliseconds())
			m.fwLagEWMA = fwLagNext(m.fwLagEWMA, obs)
			m.fwClutch = fwClutchNext(m.fwClutch, m.fwLagEWMA)
		}
		m.lastFwTick = now
		m.fireworks = stepFireworks(m.fireworks)
		if len(m.fireworks) == 0 {
			m.fwTicking = false
			// (3e8b) idle reset: same rationale as the overlay-closed path
			// above -- don't let a quiet sky masquerade as future lag.
			m.lastFwTick, m.fwLagEWMA, m.fwClutch = time.Time{}, 0, false
			return m, nil
		}
		return m, fwTick()

	case poofTickMsg:
		// Step the poof on its own cadence, independent of showEgg/fwTicking
		// (kata dw57 -- see poofTickMsg's doc: it must NOT hook into the
		// fireworks handler above). Mirrors fwTick's no-leak discipline
		// exactly: stop (poofTicking=false) the moment the poof completes --
		// or if it's already gone (defensive: cleared elsewhere between
		// scheduling and firing) -- otherwise reschedule. startPoof's guard
		// ensures exactly one poofTicker is ever live, so this never
		// busy-loops or stacks under repeated triggers.
		if m.poof == nil {
			m.poofTicking = false
			return m, nil
		}
		next := stepPoof(*m.poof)
		if next.ttl <= 0 {
			m.poof = nil
			m.poofTicking = false
			m.resizeList() // give the status slot back the row the poof held
			return m, nil
		}
		m.poof = &next
		return m, poofTick()

	case toggleDoneMsg:
		m.pending = 0
		if msg.err != nil {
			if errors.Is(msg.err, tsserve.ErrOperatorNotSet) {
				// Deliberate exception to the auto-dismiss toast (q89g): this is
				// required-setup guidance, not a fleeting error, so it gets the
				// STICKY banner (operatorHintText) instead of a transient toast
				// that would just flash the same information and vanish.
				m.operatorNotSet = true
				return m, refresh
			}
			// The error toast auto-dismisses (q89g); still refresh to reconcile
			// state -- the toast survives a refresh now that notifications no
			// longer live in a field the refresh clears.
			return m, tea.Batch(m.setErr(msg.err.Error()), refresh)
		}
		// A successful serve/funnel toggle proves the operator issue, if any,
		// is resolved -- clear the sticky banner without waiting for a "r"
		// re-check.
		m.operatorNotSet = false
		return m, refresh

	case publishDoneMsg:
		// Mirrors toggleDoneMsg (kata v1z5): clear pending, and rather than
		// hand-set the published map from this op's outcome, always re-fetch
		// (refresh reconciles serve state; pollPublishedCmd re-reads the live
		// routes) so the UI reflects Caddy's actual state.
		m.pending = 0
		// Consume any pending "took over <host>" flag now (kata 6n15): a takeover
		// publish that FAILS clears it too, so a later ordinary publish can't emit a
		// stale "took over" toast. Only a success below uses it.
		tookOver := m.takeoverHost
		m.takeoverHost = ""
		// Capture BEFORE the terminal-success clear below (mirroring tookOver):
		// the hostname THIS publish targeted, confirmPublish's pendingPublish
		// carry (kata qfbf) -- used only for the plain publish-success toast's
		// "press p to unpublish" teaching clause (71ga).
		publishedHost := m.pendingPublish.hostname
		// ── ttfh stage 2: ARM the restore slot. m.pendingArm is set ONLY just before
		// the take-over resume publish (purgeDoneMsg success, OWNED), so its presence
		// marks THIS publishDoneMsg as that resume. Arm whether the take-over
		// succeeded or FAILED (design F4 — the purge already happened, so restore must
		// be offered); takeoverLive records which, gating the poll-based re-take clear.
		// Because the slot is armed HERE (not at purge time) and no generic "publish
		// clears the slot" trigger exists, this take-over publish is inherently exempt
		// from clearing the affordance it just armed.
		var armTimer tea.Cmd
		if m.pendingArm != nil {
			m.pendingArm.takeoverLive = msg.err == nil
			m.lastPurge = m.pendingArm
			m.pendingArm = nil
			m.resizeList()                         // reserve the status-slot row the prompt will use
			armTimer = m.scheduleLastPurgeExpiry() // start the ~60s idle timeout
		}
		// De-escalation clear (design F10): the user unpublished our take-over's port
		// (a distinct unpublishCmd → unpublish:true), so the restore affordance no
		// longer describes live state — drop it. The restore's OWN step A goes through
		// restoreCmd (a restoreDoneMsg), never here, so this can't fire on an undo.
		if msg.unpublish && m.lastPurge != nil && msg.port == m.lastPurge.ourPort {
			m.clearLastPurge()
		}
		if msg.err != nil {
			if tookOver != "" {
				// This is the RESUMED take-over publish (kata 6n15) and it FAILED. The
				// destructive half already happened — the old route was purged — so a
				// bare publish-error toast would hide that (roborev hped #5). But do NOT
				// over-claim the host is now "unpublished" (roborev ve95 FIX 3): that
				// isn't proven — an ErrHostnameConflict means another route may already
				// hold the name, and a transport error may have committed the publish
				// server-side before the response was lost. Report only the KNOWN fact
				// (the purge happened) and that the take-over's outcome is UNCERTAIN;
				// the poll below reflects whatever the edge actually holds. Terminal:
				// clear the carry and do NOT re-classify/loop; restoring the capture is
				// ttfh's job (its seam on the success path is untouched).
				m.pendingPublish = pendingPublish{}
				// armTimer (ttfh) starts the ~60s idle timeout for the restore slot
				// armed above: the take-over FAILED but the purge succeeded, so restore
				// is still offered (design F4) and its timer must run.
				return m, tea.Batch(
					m.setErr(fmt.Sprintf("purged the old route for %s, but the take-over publish failed: %s — %s's state on the edge is now uncertain; resolve it in Caddy",
						tookOver, publishErrText(msg.err), tookOver)),
					armTimer, refresh, m.pollPublishedCmd())
			}
			if errors.Is(msg.err, tsserve.ErrOperatorNotSet) {
				// The auto-enable-serve step hit tailscale's operator gate --
				// same sticky guidance banner a serve toggle would raise.
				m.operatorNotSet = true
				return m, tea.Batch(refresh, m.pollPublishedCmd())
			}
			if !msg.unpublish && errors.Is(msg.err, caddyedge.ErrHostnameConflict) {
				// The publish collided with a hostname the shared edge already
				// holds (kata qfbf). Don't dump Caddy's raw message: classify it
				// read-only (naming the holder) and refuse specifically, via a
				// second read (inspectConflictCmd -> inspectConflictMsg). Keep
				// m.pending set to the port so the in-flight discipline holds
				// across the classification read -- no concurrent op sneaks in and
				// the status line still reads busy -- until inspectConflictMsg
				// clears it. Fall back to the bare toast only if the attempted
				// hostname was somehow lost (confirmPublish always sets it).
				//
				// Gated on !msg.unpublish (kata vsx4 #1): Unpublish can ALSO return
				// ErrHostnameConflict, and m.pendingPublish is never cleared on this
				// path (only on a terminal outcome below), so without the gate an
				// unpublish's conflict would classify a STALE prior publish's
				// hostname and, on Kind==None, RE-PUBLISH -- re-exposing a port the
				// user asked to de-escalate. An unpublish conflict always falls
				// through to the plain error toast instead.
				if m.pendingPublish.hostname != "" {
					m.pending = msg.port
					return m, inspectConflictCmd(m.caddyClient(), msg.port, m.pendingPublish.hostname, shortLabel(m.fqdn), msg.port)
				}
				return m, tea.Batch(m.setErr(publishErrText(msg.err)), refresh, m.pollPublishedCmd())
			}
			// Terminal, non-conflict outcome (kata vsx4 #1): clear the carry so a
			// stale pendingPublish from THIS attempt can't be read by some later,
			// unrelated op.
			m.pendingPublish = pendingPublish{}
			return m, tea.Batch(m.setErr(publishErrText(msg.err)), refresh, m.pollPublishedCmd())
		}
		// A publish that auto-enabled serve proves the operator is set.
		m.operatorNotSet = false
		// A successful publish means the edge accepted the route -- the domain
		// setup (DNS + a deployed edge) is demonstrably working, so retire the
		// sticky setup reminder (kata w131).
		m.domainSetupPending = false
		// Terminal success (kata vsx4 #1): clear the carry -- see the note on the
		// non-conflict error branch above.
		m.pendingPublish = pendingPublish{}
		if tookOver != "" {
			// This publish resumed a force-purge take-over (kata 6n15): a plain
			// success toast naming the host we took over. armTimer (ttfh) starts the
			// ~60s idle timeout for the restore slot armed above (nil for a foreign
			// take-over, which arms nothing). "press p to unpublish" (71ga) teaches
			// the de-escalation path same as the plain publish toast below.
			return m, tea.Batch(m.setFlash(fmt.Sprintf("took over %s — press p to unpublish", tookOver), flashInfo), armTimer, refresh, m.pollPublishedCmd())
		}
		if !msg.unpublish {
			// A plain publish success (71ga): teach the de-escalation path --
			// requestPublish's own key, pressed again on an already-published
			// port, unpublishes immediately (2643, unchanged behavior). Scoped to
			// !msg.unpublish only: an unpublish success stays silent, as before.
			return m, tea.Batch(m.setFlash(fmt.Sprintf("published %s — press p to unpublish", publishedHost), flashInfo), refresh, m.pollPublishedCmd())
		}
		return m, tea.Batch(refresh, m.pollPublishedCmd())

	case inspectConflictMsg:
		// The read-only classification of a hostname conflict is back (kata qfbf).
		// Clear the in-flight marker the publishDoneMsg conflict path kept set.
		// NOTHING is mutated on any branch here -- this pillar only refuses.
		m.pending = 0
		if msg.err != nil {
			// The classification read itself failed (edge unreachable, etc.). We
			// can't name the holder, so surface the transport error; nothing was
			// mutated. Serve is on from the publish attempt (roborev xzns).
			return m, m.refuseConflict(msg.port, publishErrText(msg.err))
		}
		if msg.info.Kind == caddyedge.None {
			// The conflict cleared between Publish's refusal and this read. Retry
			// the plain publish ONCE (bounded -- never spin): a second None after a
			// retry gives up with a plain refusal instead of looping.
			if !m.pendingPublish.retried && m.pendingPublish.hostname == msg.hostname {
				m.pendingPublish.retried = true
				var auth *caddyedge.BasicAuth
				if m.pendingPublish.withAuth {
					auth = &caddyedge.BasicAuth{User: m.cfg.Caddy.AuthUser, Hash: m.cfg.Caddy.AuthHash}
				}
				m.pending = msg.port
				// Serve is already on from the first attempt (publishCmd enables it
				// before Publish), so don't re-run it here: enableServe=false.
				return m, publishCmd(m.caddyClient(), msg.hostname, m.pendingPublish.label, msg.port, auth, false)
			}
			return m, m.refuseConflict(msg.port, fmt.Sprintf("%s: the conflict cleared then returned — try again", msg.hostname))
		}

		// ── force-purge + take-over (kata 6n15; design §3.4) ──────────────────
		// The conflict is fully classified in msg.info. For the two PURGEABLE
		// kinds, replace qfbf's flat refusal with an escalated confirm ladder that,
		// on confirmation, deletes the offending route and RESUMES the publish
		// (m.pendingPublish carries the secret-free params). IdHijacked and any
		// other kind stay a refusal — a re-pointed @id is drift, never a blind
		// delete.
		switch msg.info.Kind {
		case caddyedge.OwnedDiffBackend:
			// A tailport-owned route (this machine's other port, or another
			// machine's) — a NORMAL y/n whose text names the backend.
			m.purgeHostname, m.purgePort = msg.hostname, msg.port
			m.purgeInfo = msg.info
			m.purgeExpect = caddyedge.ExpectFromConflict(msg.info)
			m.mode = entryConfirmPurgeOwned
			return m, nil
		case caddyedge.ForeignOverlap:
			if !msg.info.Disclosable {
				// The foreign route matches on MORE than a hostname (a hostless OR
				// block, an extra path/method matcher, or a bare catch-all), so its
				// Hosts list is NOT its full blast radius. Offering a force-delete
				// would let the user delete a route matching traffic the confirm
				// never named (roborev en3n-#1). Refuse and send them to Caddy.
				return m, m.refuseConflict(msg.port, fmt.Sprintf("%s is held by a route that matches more than a hostname — resolve it in Caddy", msg.hostname))
			}
			// A route tailport did NOT create (drift) — the SCARY first gate: a
			// y/n drift warning, which on y advances to the typed-"purge" commit.
			m.purgeHostname, m.purgePort = msg.hostname, msg.port
			m.purgeInfo = msg.info
			m.purgeExpect = caddyedge.ExpectFromConflict(msg.info)
			m.mode = entryConfirmPurgeForeign
			return m, nil
		default:
			// IdHijacked (and any defensive fallthrough): refuse with attribution.
			refusal := conflictRefusalText(msg.info, msg.hostname, shortLabel(m.fqdn))
			return m, m.refuseConflict(msg.port, refusal)
		}

	case purgeDoneMsg:
		// A force-purge / take-over delete finished (kata 6n15). Clear the in-flight
		// marker the confirmPurge launch set.
		m.pending = 0
		if msg.err != nil {
			switch {
			case errors.Is(msg.err, caddyedge.ErrConflictChanged):
				// The route changed between classify and purge (possibly an
				// owned→foreign escalation, or an owned backend swap). NOTHING was
				// deleted. Re-classify and re-open the correct — possibly scarier —
				// ladder: a route approved as owned must never be deleted under only
				// the normal confirm once it became foreign.
				m.pending = msg.port
				return m, inspectConflictCmd(m.caddyClient(), msg.port, msg.hostname, shortLabel(m.fqdn), msg.port)
			case errors.Is(msg.err, caddyedge.ErrNoConflict):
				// The conflict cleared before we deleted anything. Reuse the qfbf None
				// path: re-classify, which returns None and retries the plain publish
				// once (bounded). retried starts false on this fresh pendingPublish.
				m.pendingPublish.retried = false
				m.pending = msg.port
				return m, inspectConflictCmd(m.caddyClient(), msg.port, msg.hostname, shortLabel(m.fqdn), msg.port)
			default:
				// ErrUnreachable / ErrConcurrentUpdate / any other: a toast, nothing
				// half-done. The (possibly) deleted route, if any, is reflected by the
				// re-fetch; on these errors PurgeConflict deletes nothing.
				return m, tea.Batch(m.setErr(publishErrText(msg.err)), refresh, m.pollPublishedCmd())
			}
		}

		// The purge succeeded: the offending route is gone. RESUME THE TAKEOVER —
		// republish OUR route, rebuilding auth from cfg (no plaintext survives; the
		// serve is already on, so enableServe=false). takeoverHost lets the takeover
		// publish's success toast say "took over <host>".
		//
		// (kata dw57) Trigger the "poof" dissolve for the just-purged route's
		// descriptor HERE, on the success path. This is fire-and-forget and
		// decoupled: startPoof only sets m.poof + (maybe) starts the sibling
		// poof ticker, it never branches on outcome, and the cmd it returns
		// just rides in the same tea.Batch as the resume publish below --
		// nothing about the takeover resume changes because of it.
		poofText := msg.hostname
		if msg.deletedDesc != "" {
			poofText = fmt.Sprintf("%s → %s", msg.hostname, msg.deletedDesc)
		}
		poofCmd := m.startPoof(poofText)
		// ── SEAM (kata ttfh): the OWNED-only undo arms in TWO stages (design F4).
		//    Here (stage 1) we STASH the pre-arm info and schedule the ~60s idle
		//    timer, but do NOT arm the slot yet: the arm must happen AFTER the
		//    take-over publishDoneMsg (arming even if it FAILS — the purge already
		//    succeeded, so restore must be offered), and staging it there also
		//    exempts the in-transaction take-over publish from clearing the slot it
		//    just armed. Undo is OWNED-only (OQ8): a foreign force-purge is one-way,
		//    so gate on msg.owned && msg.captured.HadID (m.purgeExpect is already
		//    zeroed by clearPurgeFlow, so owned-ness rides the message). The take-over
		//    resume below is UNCHANGED — the stash just rides on the model, and the
		//    ~60s idle timer is scheduled at stage 2 (so it never lands in this
		//    resume batch, which other flows unpack as exactly poof-tick + publish).
		if msg.owned && msg.captured.HadID {
			m.pendingArm = &lastPurgeState{
				captured:    msg.captured.Raw,
				hostname:    msg.hostname,
				ourLabel:    shortLabel(m.fqdn),
				ourPort:     msg.port,
				deletedDesc: msg.deletedDesc,
			}
		}
		var auth *caddyedge.BasicAuth
		if m.pendingPublish.withAuth {
			auth = &caddyedge.BasicAuth{User: m.cfg.Caddy.AuthUser, Hash: m.cfg.Caddy.AuthHash}
		}
		m.takeoverHost = msg.hostname
		m.pending = msg.port
		return m, tea.Batch(poofCmd, publishCmd(m.caddyClient(), msg.hostname, m.pendingPublish.label, msg.port, auth, false))

	case restoreDoneMsg:
		// A purge-undo (kata ttfh; design §3.6) finished. Clear the in-flight
		// guards; the message is computed from msg.result — the FINAL observed edge
		// state (VerifyRestore) or the split A/B outcome — never a guess. All modes
		// except the retryable "edge unreachable" CLEAR the slot; only that one keeps
		// it armed and re-arms the idle timer (design F11). The success message NEVER
		// claims positional/routing fidelity — a restored route re-appends at the
		// array end (an accepted loss, roborev-k7br/he97).
		m.pending = 0
		m.restoring = false
		switch msg.result {
		case restoreRestored:
			m.clearLastPurge()
			return m, tea.Batch(m.setFlash(fmt.Sprintf("restored %s", msg.hostname), flashInfo), refresh, m.pollPublishedCmd())
		case restoreUnclaimed:
			m.clearLastPurge()
			return m, tea.Batch(m.setFlash(fmt.Sprintf("couldn't restore %s — it's now unclaimed", msg.hostname), flashWarn), refresh, m.pollPublishedCmd())
		case restoreClaimedByOther:
			m.clearLastPurge()
			return m, tea.Batch(m.setFlash(fmt.Sprintf("another route now claims %s — resolve the drift in Caddy", msg.hostname), flashWarn), refresh, m.pollPublishedCmd())
		case restoreContentMismatch:
			m.clearLastPurge()
			return m, tea.Batch(m.setFlash(fmt.Sprintf("restored a route for %s but its config changed", msg.hostname), flashWarn), refresh, m.pollPublishedCmd())
		case restoreUnverified:
			m.clearLastPurge()
			return m, tea.Batch(m.setFlash(fmt.Sprintf("restored a route for %s but couldn't verify the final state", msg.hostname), flashWarn), refresh, m.pollPublishedCmd())
		case restoreTakeoverChanged:
			m.clearLastPurge()
			return m, tea.Batch(m.setFlash(fmt.Sprintf("your take-over of %s changed under you — resolve in Caddy; not restoring", msg.hostname), flashWarn), refresh, m.pollPublishedCmd())
		case restoreUnreachable:
			// Retryable (design F11): KEEP the slot armed and re-arm the idle timer.
			// No poll here — a poll could read the take-over teardown as a re-take and
			// clear the slot we're deliberately keeping for the retry.
			var reArm tea.Cmd
			if m.lastPurge != nil {
				reArm = m.scheduleLastPurgeExpiry()
			}
			return m, tea.Batch(m.setFlash(fmt.Sprintf("couldn't restore %s — edge unreachable; press R to retry", msg.hostname), flashWarn), reArm)
		default: // restoreError
			m.clearLastPurge()
			return m, tea.Batch(m.setErr(fmt.Sprintf("couldn't restore %s: %s", msg.hostname, publishErrText(msg.err))), refresh, m.pollPublishedCmd())
		}

	case lastPurgeExpireMsg:
		// The restore affordance's ~60s idle timeout (design OQ1). Honor it only if
		// it matches the current generation (not superseded by a clear/re-arm) and no
		// restore is in flight (the timer is suspended while restoring — design F11).
		if msg.gen == m.lastPurgeGen && m.lastPurge != nil && !m.restoring {
			m.clearLastPurge()
		}
		return m, nil

	case publishPollMsg:
		// Drop a stale result (roborev 0k12 #1): polls are remote round-trips
		// that can finish out of order, so ignore anything older than the
		// newest already applied -- otherwise an older periodic poll could
		// clobber the post-publish refresh (hiding exposure, bypassing the
		// mutual-exclusion guards) or a startup label-less poll could wipe the
		// fqdn-triggered one. The guard covers the err path too, so a stale
		// failure can't flip publishReachable=false after a newer success.
		if msg.gen < m.publishPollApplied {
			return m, nil
		}
		m.publishPollApplied = msg.gen
		if msg.err != nil {
			// Quiet degrade (kata v1z5 step 5): keep the last-known published
			// map, just mark the edge unreachable. No toast -- a persistent
			// " · edge unreachable" status-line fragment carries it instead.
			m.publishReachable = false
			return m, nil
		}
		m.published = msg.published
		m.publishReachable = true
		// A successful poll reached the edge admin API -- a real edge is now
		// working, so the domain-setup reminder has done its job (kata w131).
		m.domainSetupPending = false
		// Third-party re-take clear (kata ttfh; design §3.6/§5): if our take-over
		// was live and the poll now shows our port no longer publishing the armed
		// hostname, someone re-took (or removed) it — the restore affordance no
		// longer describes reality, so drop it. Gated on takeoverLive so a FAILED
		// take-over's armed-for-retry slot (whose expected state is "no route of
		// ours") is never wrongly cleared by this same signal.
		if m.lastPurge != nil && m.lastPurge.takeoverLive {
			if info, ok := m.published[m.lastPurge.ourPort]; !ok || info.hostname != m.lastPurge.hostname {
				m.clearLastPurge()
			}
		}
		return m, m.rebuildItems()

	case publishTickMsg:
		// The published-state ticker NEVER stops: reschedule unconditionally.
		// Poll only when idle (an in-flight op will re-poll on its
		// publishDoneMsg) and only when configured (pollPublishedCmd is nil for
		// a blank caddy.domain).
		if m.pending != 0 || m.cleaning != 0 {
			return m, publishTick()
		}
		return m, tea.Batch(m.pollPublishedCmd(), publishTick())

	case cleanupDoneMsg:
		m.cleaning = 0
		if msg.err != nil {
			return m, tea.Batch(m.setErr(msg.err.Error()), refresh)
		}
		return m, refresh

	case flashExpireMsg:
		// Only clear if no newer toast has replaced this one (see flashID).
		if msg.id == m.flashID {
			m.flash = ""
			m.flashLevel = flashInfo
			m.resizeList() // give the list back any rows a wrapped toast held
		}
		return m, nil

	case copiedExpireMsg:
		// Only clear if no newer inline copy has replaced this one (see
		// copiedID) -- copying a different port moves the annotation via a
		// fresh copiedID, so this stale timer is a no-op.
		if msg.id == m.copiedID {
			m.copiedPort = 0
			return m, m.rebuildItems()
		}
		return m, nil

	case tea.KeyMsg:
		// Any keypress dismisses a lingering toast (a fresh one is set below if
		// this key produces it), so notifications never stick around stale.
		m.flash = ""
		m.flashLevel = flashInfo
		// Give the list back any rows a wrapped toast held; a fresh flash set
		// below (via setFlash) re-reserves on top of this.
		m.resizeList()
		// The Easter-egg overlay (28mv) is modal like help: esc/q/E close it,
		// "c" copies the site link (with a toast, staying open), ctrl+c quits
		// the app entirely, and every other key is swallowed. It never traps.
		if m.showEgg {
			switch msg.String() {
			case "esc", "q", "E":
				m.showEgg = false // the eggTickMsg handler then stops the ticker
				m.fireworks = nil // stop drawing at once; fwTick self-stops next
				return m, nil
			case "ctrl+c":
				return m, tea.Quit
			case "c":
				return m, m.eggCopy(eggURL, eggDomain)
			case "g":
				return m, m.eggCopy(eggRepoURL, eggRepoDomain)
			case "f":
				// Secret-within-the-secret (5x1e): launch ONE firework
				// instantly. Beyond the cap, ignore the press. Start the
				// decoupled fireworks ticker only if it isn't already running
				// (fwTicking) so ticks never stack under mashing.
				// (3e8b) intake is also gated on the adaptive clutch: when
				// fwClutch is engaged (observed tick lag says we're falling
				// behind), refuse new launches even under the cap -- in-flight
				// fireworks are never killed to recover, only throttled at the
				// intake.
				if len(m.fireworks) < fwCap && !m.fwClutch {
					m.fireworks = append(m.fireworks, newFirework(m.width, m.height, m.emoji))
				}
				if !m.fwTicking && len(m.fireworks) > 0 {
					m.fwTicking = true
					return m, fwTick()
				}
				return m, nil
			}
			return m, nil
		}
		// The help overlay is modal: while it's open, ?/esc/q close it, the
		// scroll keys pan its (viewport-clipped) body, and every other key is
		// swallowed (so nothing happens "behind" it).
		if m.showHelp {
			switch msg.String() {
			case "?", "esc", "q", "ctrl+c":
				m.showHelp = false
				m.helpScroll = 0
			case "up", "k":
				m.helpScroll--
			case "down", "j":
				m.helpScroll++
			case "pgup", "b":
				m.helpScroll -= m.helpPageStep()
			case "pgdown", " ", "f":
				m.helpScroll += m.helpPageStep()
			case "home", "g":
				m.helpScroll = 0
			case "end", "G":
				m.helpScroll = m.helpMaxScroll()
			}
			// Clamp after every adjustment: content height depends on width and
			// marker mode, either of which can change between presses.
			if max := m.helpMaxScroll(); m.helpScroll > max {
				m.helpScroll = max
			}
			if m.helpScroll < 0 {
				m.helpScroll = 0
			}
			return m, nil
		}
		if m.mode != entryNone {
			// The clean-confirm prompt is a y/n gate, not a text input, so
			// it's handled before the esc/enter switch below (and before the
			// keys ever reach a textinput). "y"/"Y" is the ONLY affirmative;
			// every other key -- including esc, n, and enter -- cancels.
			if m.mode == entryConfirmClean {
				switch msg.String() {
				case "y", "Y":
					targets := m.cleanTargets
					m.mode = entryNone
					m.cleanTargets = nil
					m.cleaning = len(targets)
					return m, cleanupDangling(targets)
				default:
					m.mode = entryNone
					m.cleanTargets = nil
					return m, nil
				}
			}
			// The :22 SSH confirm is the same y/n gate: "y"/"Y" proceeds with
			// the deferred toggle, every other key cancels with no serve call.
			if m.mode == entryConfirm22 {
				switch msg.String() {
				case "y", "Y":
					port := m.confirmPort
					turnOn := m.confirmTurnOn
					m.mode = entryNone
					m.confirmPort = 0
					if m.pending != 0 {
						return m, nil
					}
					return m, m.beginToggle(port, turnOn)
				default:
					m.mode = entryNone
					m.confirmPort = 0
					return m, nil
				}
			}
			// The funnel confirm is the same y/n gate, guarding the escalation
			// to the PUBLIC INTERNET: "y"/"Y" proceeds with the funnel, every
			// other key cancels with no funnel call.
			if m.mode == entryConfirmFunnel {
				switch msg.String() {
				case "y", "Y":
					port := m.funnelPort
					pub := m.funnelPublic
					m.mode = entryNone
					m.funnelPort = 0
					m.funnelPublic = 0
					if m.pending != 0 {
						return m, nil
					}
					return m, m.beginFunnel(port, pub, true)
				default:
					m.mode = entryNone
					m.funnelPort = 0
					m.funnelPublic = 0
					return m, nil
				}
			}
			// The publish (`p`) flow (kata v1z5; swapped from `P` under vzj4) is
			// a self-contained state
			// machine handled here, BEFORE the generic esc/enter switch and the
			// textinput fallthrough below -- so its keystrokes reach publishInput
			// and never leak into labelInput.
			switch m.mode {
			case entryPublishHostname, entryPublishDomain, entryPublishHost, entryPublishAuth,
				entryPublishCredUser, entryPublishCredPass, entryConfirmPublish:
				return m, m.updatePublishEntry(msg)
			case entryConfirmPurgeOwned, entryConfirmPurgeForeign, entryConfirmPurgeForeignType:
				// The force-purge / take-over ladders (kata 6n15) are their own
				// self-contained state machine, dispatched here for the same reason
				// as the publish flow: keystrokes reach purgeInput, never labelInput.
				return m, m.updatePurgeEntry(msg)
			}
			switch msg.String() {
			case "esc":
				m.mode = entryNone
				m.portInput.Reset()
				m.labelInput.Reset()
				m.labelPort = 0
				m.sshInput.Reset()
				m.sshUnlockPort = 0
				return m, nil
			case "enter":
				switch m.mode {
				case entryAddPort:
					port, err := strconv.Atoi(m.portInput.Value())
					m.mode = entryNone
					m.portInput.Reset()
					if err != nil || port < 1 || port > 65535 {
						return m, m.setErr("invalid port")
					}
					// Re-adding an already-favorited port is a no-op that would
					// look like nothing happened, so surface an info toast
					// instead of silently re-saving (7ac3).
					if m.cfg.Ports[port].Favorite {
						return m, m.setFlash(fmt.Sprintf(":%d already favorited — no change", port), flashInfo)
					}
					// "n" registers + favorites the port; it does NOT serve
					// (ykgj). Exposing is always space. A not-yet-running
					// favorite then shows in the Favorites view as a synthetic
					// entry, ready to serve once its service is up -- so an
					// added port sticks instead of vanishing. No lock/:22 guard
					// is needed here since nothing is exposed. New adds are
					// silent (dup-only feedback).
					before := m.portState(port)
					cmd := m.favorite(port)
					m.pushUndo(port, before, fmt.Sprintf("add :%d", port))
					return m, tea.Batch(cmd, m.rebuildItems())
				case entryLabel:
					label := strings.TrimSpace(m.labelInput.Value())
					port := m.labelPort
					m.mode = entryNone
					m.labelInput.Reset()
					m.labelPort = 0
					_, existed := m.cfg.Ports[port]
					if label != "" || existed {
						if m.cfg.Ports == nil {
							m.cfg.Ports = map[int]config.PortMeta{}
						}
						before := m.portState(port)
						meta := m.cfg.Ports[port]
						// Committing the same label is a no-op; recording it
						// would put a "u" on the stack that visibly does
						// nothing (vgn5 prefills the input, so enter-with-no-
						// edits is the common path, not a rare one).
						if meta.Label == label {
							return m, m.rebuildItems()
						}
						meta.Label = label
						m.cfg.Ports[port] = meta
						m.pushUndo(port, before, fmt.Sprintf("label :%d", port))
						return m, tea.Batch(m.saveConfig(), m.rebuildItems())
					}
					return m, m.rebuildItems()
				case entryConfirmUnlockSSH:
					// Only an exact "ssh" (trimmed, case-insensitive) unlocks;
					// anything else -- including empty -- cancels without
					// touching the lock. This edits config only, no tailscale.
					typed := strings.ToLower(strings.TrimSpace(m.sshInput.Value()))
					port := m.sshUnlockPort
					m.mode = entryNone
					m.sshInput.Reset()
					m.sshUnlockPort = 0
					if typed != "ssh" {
						return m, nil // cancelled: :22 stays locked
					}
					before := m.portState(port)
					meta := m.cfg.Ports[port]
					meta.Locked = false
					m.cfg.Ports[port] = meta
					// Undoable like any other lock edit: undo RE-locks :22,
					// which is the safe direction and needs no confirm. It's
					// the reverse (an undo that unlocks) that wouldUnlockSSH
					// refuses.
					m.pushUndo(port, before, fmt.Sprintf("unlock :%d", port))
					return m, tea.Batch(m.saveConfig(), m.rebuildItems())
				}
			}
			var cmd tea.Cmd
			switch m.mode {
			case entryAddPort:
				m.portInput, cmd = m.portInput.Update(msg)
			case entryConfirmUnlockSSH:
				m.sshInput, cmd = m.sshInput.Update(msg)
			default:
				m.labelInput, cmd = m.labelInput.Update(msg)
			}
			return m, cmd
		}

		if m.list.FilterState() == list.Filtering {
			// While typing a filter, every key belongs to bubbles/list's filter
			// input -- forward it. If this key cancelled the filter (esc), drop
			// the widened scope and restore the current view.
			var cmd tea.Cmd
			m.list, cmd = m.list.Update(msg)
			if m.list.FilterState() == list.Unfiltered {
				m.filtering = false
				m.rebuildItems() // Unfiltered -> nil cmd
			}
			return m, cmd
		}

		// Contextual restore key (kata ttfh; design OQ3): R restores a just-purged
		// OWNED route, but ONLY while the slot is armed. It is a DISTINCT key from the
		// registry-undo `u` (which shadows nothing here and keeps its "does NOT touch
		// what's exposed" promise literally true). When UNARMED it is not handled here
		// at all, so it falls through to the list like any other unbound key and
		// shadows no binding. The in-flight guard (restoring / m.pending) makes a
		// second R a no-op so two concurrent A/B can never fire (design F5).
		if msg.String() == "R" && m.lastPurge != nil {
			if m.restoring || m.pending != 0 {
				return m, nil
			}
			return m, m.beginRestore()
		}

		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "left", "right":
			// Grid column jump (9gys): the one genuinely NEW nav move the
			// column-major grid needs -- move one column over, same row --
			// intercepted here, BEFORE the fallthrough to m.list.Update,
			// so bubbles/list's own left/right-bound PrevPage/NextPage
			// default keys never also fire and double-move the selection.
			// Deliberately ARROW KEYS ONLY: "h"/"l" are left bound to the
			// list's native PrevPage/NextPage (h) and this app's own "l"
			// label shortcut (l, handled below) respectively, so hijacking
			// either would either collide with "l" or need a second
			// carve-out -- not worth it for a key with an arrow equivalent.
			// Up/Down/PgUp/PgDn/Home/End/j/k deliberately are NOT
			// intercepted: they already fall through to m.list.Update below
			// and land on the right Index() there -- CursorUp/CursorDown
			// move it by exactly ±1 and GoToStart/GoToEnd snap to 0/len-1
			// regardless of the list's own internal PerPage, because
			// Index()/Select() are self-consistent by construction
			// (Page*PerPage+cursor round-trips any index); see gridDims'
			// doc comment. PgUp/PgDn still jump by the list's own
			// single-column PerPage rather than the grid's cols*rows page
			// size -- a known, accepted imprecision (not a correctness bug:
			// it's still a monotonic, in-bounds jump) rather than fight
			// bubbles/list's paginator to make it exact.
			//
			// This case returns EARLY (below), before the end-of-Update
			// navigation clear, so clear the restore affordance here too
			// (kata 7jy2 FIX 4) — horizontal nav is selection-changing
			// navigation intent like the vertical keys that fall through.
			m.clearRestoreOnNav()
			items := m.list.VisibleItems()
			if len(items) == 0 {
				return m, nil
			}
			_, rows, _ := m.gridDims()
			if rows < 1 {
				rows = 1
			}
			sel := m.list.Index()
			target := sel - rows
			if msg.String() == "right" {
				target = sel + rows
			}
			if target < 0 {
				target = 0
			}
			if last := len(items) - 1; target > last {
				target = last
			}
			m.list.Select(target)
			return m, nil
		case "/":
			// Widen scope to ALL listening ports BEFORE handing "/" to the list
			// (4ye6): the list snapshots its current items when filtering
			// begins, so the items must already be the full set. Rebuilding
			// while still Unfiltered avoids bubbles/list's async re-filter path.
			m.clearRestoreOnNav() // entering the filter changes scope + selection (roborev nk3b-#3)
			m.filtering = true
			m.rebuildItems() // Unfiltered list -> nil cmd
			var cmd tea.Cmd
			m.list, cmd = m.list.Update(msg)
			return m, cmd
		case "?":
			// This is reached only when not filtering (filtering does the
			// break above), so "?" opens help normally but is typed into the
			// "/" filter query while a filter is active.
			m.showHelp = true
			m.helpScroll = 0
			return m, nil
		case "E":
			// Hidden Easter egg (28mv): deliberately NOT in the legend or help.
			// Opens the animated overlay and starts its ticker.
			m.showEgg = true
			m.eggFrame = 0
			return m, eggTick()
		case "a":
			// Anchor the cursor to the port under it, not the row index:
			// capture the selected port number, switch views, then re-select
			// that port if it survived into the new view (else the nearest
			// remaining port -- see selectPort).
			m.clearRestoreOnNav() // switching views changes the selection (roborev nk3b-#3)
			var cur int
			if sel, ok := m.list.SelectedItem().(portItem); ok {
				cur = sel.port.Number
			}
			m.showAllPorts = !m.showAllPorts
			cmd := m.rebuildItems()
			if cur != 0 {
				m.selectPort(cur)
			}
			return m, cmd
		case "r":
			// Also re-run the proactive operator check (kata tapv): pressing r
			// after fixing it (`sudo tailscale set --operator=...`) clears the
			// sticky banner even without another serve attempt.
			return m, tea.Batch(refresh, detectOperator)
		case "c":
			// Copy the selected port's TAILNET URL (http://<host>:<port>) to the
			// clipboard, even when it isn't currently exposed -- the toast then
			// says so. Always the tailnet URL, never the public/funnel one.
			sel, ok := m.list.SelectedItem().(portItem)
			if !ok {
				return m, nil
			}
			return m, m.copyURL(sel)
		case "C":
			// Batch-tear-down of dangling forwards, behind a y/n confirm (moved
			// from "c" to shift-C when "c" became copy; see vnq7). No-op while a
			// toggle or cleanup is already in flight, and a silent no-op when
			// there's nothing to clean (an error toast would read as a failure).
			if m.pending != 0 || m.cleaning != 0 {
				return m, nil
			}
			targets := m.danglingPorts()
			if len(targets) == 0 {
				return m, nil
			}
			m.cleanTargets = targets
			m.mode = entryConfirmClean
			return m, nil
		case "n":
			m.mode = entryAddPort
			m.portInput.Focus()
			return m, nil
		case "l":
			sel, ok := m.list.SelectedItem().(portItem)
			if !ok {
				return m, nil
			}
			m.mode = entryLabel
			m.labelPort = sel.port.Number
			// Prefill precedence (vgn5): the current label if set (so adjusting
			// starts from existing text, not the process name), else the
			// process name for a listening port, else empty. It's an editable
			// value, so enter with no edits commits the prefill.
			prefill := sel.meta.Label
			if prefill == "" {
				prefill = sel.port.Process
			}
			m.labelInput.SetValue(prefill)
			m.labelInput.CursorEnd()
			m.labelInput.Focus()
			return m, nil
		case "f":
			sel, ok := m.list.SelectedItem().(portItem)
			if !ok {
				return m, nil
			}
			if m.cfg.Ports == nil {
				m.cfg.Ports = map[int]config.PortMeta{}
			}
			before := m.portState(sel.port.Number)
			meta := m.cfg.Ports[sel.port.Number]
			meta.Favorite = true
			m.cfg.Ports[sel.port.Number] = meta
			m.pushUndo(sel.port.Number, before, fmt.Sprintf("favorite :%d", sel.port.Number))
			return m, tea.Batch(m.saveConfig(), m.rebuildItems())
		case "F":
			// 3cwx: this is the old "u" behavior, verbatim -- clear ★, and drop
			// the registry entry entirely if nothing else was worth keeping.
			sel, ok := m.list.SelectedItem().(portItem)
			if !ok {
				return m, nil
			}
			if meta, ok := m.cfg.Ports[sel.port.Number]; ok {
				before := m.portState(sel.port.Number)
				meta.Favorite = false
				if meta.Label == "" && !meta.Locked {
					delete(m.cfg.Ports, sel.port.Number)
				} else {
					m.cfg.Ports[sel.port.Number] = meta
				}
				m.pushUndo(sel.port.Number, before, fmt.Sprintf("forget :%d", sel.port.Number))
				return m, tea.Batch(m.saveConfig(), m.rebuildItems())
			}
			return m, nil
		case "u":
			return m.stepHistory(undoDirection)
		case "ctrl+r":
			return m.stepHistory(redoDirection)
		case "x":
			sel, ok := m.list.SelectedItem().(portItem)
			if !ok {
				return m, nil
			}
			if m.cfg.Ports == nil {
				m.cfg.Ports = map[int]config.PortMeta{}
			}
			// Unlike "F", unlocking never deletes the registry entry: once
			// a port has been locked (or explicitly unlocked), it stays
			// "known" and visible in the default view, same as a port
			// that's been toggled on at least once (see remember).
			before := m.portState(sel.port.Number)
			meta := m.cfg.Ports[sel.port.Number]
			// Unlocking :22 removes the guard on SSH access, so it's gated
			// behind a type-"ssh" confirm (ah23) rather than flipped here.
			// Locking :22, and any non-:22 lock toggle, stay instant.
			if sel.port.Number == 22 && meta.Locked {
				m.sshUnlockPort = 22
				m.sshInput.Reset()
				m.sshInput.Focus()
				m.mode = entryConfirmUnlockSSH
				return m, nil
			}
			meta.Locked = !meta.Locked
			m.cfg.Ports[sel.port.Number] = meta
			verb := "unlock"
			if meta.Locked {
				verb = "lock"
			}
			m.pushUndo(sel.port.Number, before, fmt.Sprintf("%s :%d", verb, sel.port.Number))
			return m, tea.Batch(m.saveConfig(), m.rebuildItems())
		case " ":
			if m.pending != 0 {
				return m, nil // a toggle is already in flight
			}
			sel, ok := m.list.SelectedItem().(portItem)
			if !ok {
				return m, nil
			}
			// 79xb pt3 footgun guard: `tailscale serve` always proxies
			// tailnet -> 127.0.0.1:PORT, so turning it ON is only ever
			// meaningful for a loopback-bound app (state A). A B port
			// (wildcard/tailnet-IP) is already tailnet-reachable -- serving
			// it is redundant and can collide with the existing bind. A B'
			// port (specific LAN IP) isn't on loopback at all -- serving it
			// would proxy to nothing, a BROKEN dangling forward. Block
			// serve-ON for both, with an info (not error) toast explaining
			// why; every other direction (A serve-ON, C serve-OFF, E
			// unbind) is unchanged.
			if !sel.active {
				switch sel.reach() {
				case reachTailnet:
					if sel.port.Number == 22 {
						// :22 is the operator's own live SSH port (they may be
						// connected over it right now). "rebind to localhost" is
						// nonsense for sshd (you don't HTTP-serve SSH) and would
						// lock them out -- so a dedicated, non-actionable line.
						return m, m.setFlash("on tailnet as SSH — this is how you're connected; nothing for tailport to serve", flashInfo)
					}
					// Wildcard bind owned by the APP (0.0.0.0), not a tailport
					// serve, so there's no mapping to toggle. Honest about WHY,
					// and actionable: rebinding to loopback -> reach()==reachLocalhost,
					// which the guard does NOT block -> space then serves it.
					return m, m.setFlash("on tailnet — app bound wide (0.0.0.0); rebind to localhost (or 127.0.0.1) to make toggleable", flashInfo)
				case reachLAN:
					return m, m.setFlash("on your LAN only; serve can't reach this bind", flashInfo)
				}
			}
			// requestToggle applies the lock guard and, for :22 only, the SSH
			// y/n confirm before any serve call.
			return m, m.requestToggle(sel.port.Number, !sel.active)
		case "P": // funnel key (swapped from "p", vzj4)
			if m.pending != 0 {
				return m, nil // a toggle/funnel is already in flight
			}
			sel, ok := m.list.SelectedItem().(portItem)
			if !ok {
				return m, nil
			}
			// requestFunnel hard-blocks :22, refuses when all ingress ports are
			// taken, and defers a turn-on to the strong public-internet confirm.
			return m, m.requestFunnel(sel.port.Number)
		case "p": // publish key (swapped from "P", vzj4)
			if m.pending != 0 {
				return m, nil // a toggle/funnel/publish is already in flight
			}
			sel, ok := m.list.SelectedItem().(portItem)
			if !ok {
				return m, nil
			}
			// requestPublish runs the publish guards in order (busy, :22, empty
			// fqdn, funnel mutual-exclusion, de-escalation, config, lock) and
			// otherwise opens the publish dialog (kata v1z5).
			return m, m.requestPublish(sel.port.Number)
		}
	}

	// Navigation intent clears the restore affordance (kata ttfh; design §3.6).
	// Only keys NOT handled by any case above reach here -- list navigation (arrows,
	// j/k, home/end, pgup/pgdn) and the like -- so a user who has moved on drops the
	// transient "press R to restore" prompt. The early-returning grid-column
	// left/right case clears via the same helper before its return (kata 7jy2 FIX 4).
	m.clearRestoreOnNav()

	// Anything not handled above goes to the list. In FilterApplied state this
	// is where esc clears the filter (ClearFilter), so if the filter just
	// cleared, drop the widened scope and restore the current view.
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	if m.filtering && m.list.FilterState() == list.Unfiltered {
		m.filtering = false
		m.rebuildItems() // Unfiltered -> nil cmd
	}
	return m, cmd
}

// rebuildItems recomputes the visible list for the current view. In the
// All ports view (showAllPorts) it shows every currently-listening port,
// full stop. In the Favorites view it shows only ports marked
// meta.Favorite, merging in synthetic entries (empty Process) for favorite
// ports that aren't currently listening locally so a favorite never
// silently disappears just because its process is down.
//
// While a "/" filter is active (4ye6) the scope widens to ALL listening
// ports regardless of view, so the filter searches everything; when the
// origin view is Favorites, non-favorite matches are flagged dimmed so real
// favorites still stand out among the pulled-in results.
// It returns the list's SetItems command, which is non-nil (a re-filter
// request) only when rebuilding while a filter is active; callers made from
// Update must propagate it so the filtered view doesn't blank out.
func (m *model) rebuildItems() tea.Cmd {
	portsByNumber := make(map[int]portscan.Port, len(m.allPorts))
	for _, p := range m.allPorts {
		portsByNumber[p.Number] = p
	}

	if m.showAllPorts || m.filtering {
		// Non-favorite matches recede only when filtering FROM the Favorites
		// view; the All ports view (and a plain All-ports filter) dims nothing.
		dimNonFav := m.filtering && !m.showAllPorts
		// All ports = every listening port UNION all favorites, so a favorite
		// stays visible here even when its process is down (qqkx), shown as a
		// synthetic not-listening entry like the Favorites view already does.
		seen := make(map[int]bool, len(portsByNumber))
		numbers := make([]int, 0, len(portsByNumber))
		for n := range portsByNumber {
			numbers = append(numbers, n)
			seen[n] = true
		}
		for n, meta := range m.cfg.Ports {
			if meta.Favorite && !seen[n] {
				numbers = append(numbers, n)
				seen[n] = true
			}
		}
		sort.Ints(numbers)
		items := make([]list.Item, 0, len(numbers))
		for _, n := range numbers {
			// ok is the listening bool: present in portsByNumber iff a local
			// process is bound; a non-listening favorite gets a synthetic port.
			p, ok := portsByNumber[n]
			if !ok {
				p = portscan.Port{Number: n}
			}
			meta := m.cfg.Ports[n]
			pub := m.published[n]
			items = append(items, portItem{port: p, active: m.active[n], listening: ok, host: m.host, fqdn: m.fqdn, funnelPublic: m.funnel[n], publishHostname: pub.hostname, publishAuth: pub.auth, dimmed: dimNonFav && !meta.Favorite, meta: meta, emoji: m.markerEmoji, justCopied: m.copiedPort == n})
		}
		return m.setItems(items)
	}

	// Favorites view: exactly the ports flagged meta.Favorite. Unfavoriting
	// a port (which clears the flag) makes it drop out here immediately.
	numbers := make([]int, 0, len(m.cfg.Ports))
	for n, meta := range m.cfg.Ports {
		if meta.Favorite {
			numbers = append(numbers, n)
		}
	}
	sort.Ints(numbers)

	items := make([]list.Item, 0, len(numbers))
	for _, n := range numbers {
		p, ok := portsByNumber[n]
		if !ok {
			p = portscan.Port{Number: n}
		}
		// ok is exactly the listening bool: the port is present in
		// portsByNumber iff a local process is bound to it.
		pub := m.published[n]
		items = append(items, portItem{port: p, active: m.active[n], listening: ok, host: m.host, fqdn: m.fqdn, funnelPublic: m.funnel[n], publishHostname: pub.hostname, publishAuth: pub.auth, meta: m.cfg.Ports[n], emoji: m.markerEmoji, justCopied: m.copiedPort == n})
	}
	return m.setItems(items)
}

// setItems replaces the list's items and clamps the selection if it's now
// out of range. list.Model.SetItems does not do this itself: its cursor
// index is left pointing past the end whenever the item count shrinks
// (e.g. switching from the All ports view back to Favorites, or a refresh
// that drops a since-unregistered port), which makes SelectedItem() return
// nil and silently turns every selection-based key (space, l, f, u) into a
// no-op. Returns SetItems' cmd (a re-filter request while a filter is active).
func (m *model) setItems(items []list.Item) tea.Cmd {
	cmd := m.list.SetItems(items)
	if idx := m.list.Index(); len(items) > 0 && (idx < 0 || idx >= len(items)) {
		m.list.Select(len(items) - 1)
	}
	return cmd
}

// selectPort moves the cursor to the row for the given port number,
// anchoring selection to the port rather than the row index across a view
// switch. If that port isn't present in the current list, it falls back to
// the nearest remaining port (see selectIndexForPort). No-op on an empty
// list.
func (m *model) selectPort(number int) {
	items := m.list.Items()
	if len(items) == 0 {
		return
	}
	numbers := make([]int, len(items))
	for i, it := range items {
		numbers[i] = it.(portItem).port.Number
	}
	m.list.Select(selectIndexForPort(numbers, number))
}

// selectIndexForPort returns the index in numbers (sorted ascending) to
// select when anchoring the cursor to target. If target is present, its
// index is returned. Otherwise the nearest next-lowest port is chosen (the
// row just before target's insertion point), falling back to the first row
// when target is below every remaining port. Example: numbers=[3000,8080],
// target 9000 -> index 1 (:8080); target 22 -> index 0 (:3000).
func selectIndexForPort(numbers []int, target int) int {
	i := sort.SearchInts(numbers, target)
	if i < len(numbers) && numbers[i] == target {
		return i
	}
	if i > 0 {
		return i - 1
	}
	return 0
}

// legendColGap is the blank gutter between adjacent columns in the bottom-bar
// grid, and between groups in the wrapped fallback.
const legendColGap = 2

// renderLegend renders the bottom-bar keybinding legend for the current width
// and dangling state. See renderLegendWith.
func (m model) renderLegend() string {
	return m.renderLegendWith(m.hasDangling())
}

// renderLegendWith renders the grouped keybinding legend (kata p39s). When the
// aligned column grid fits the width it renders that -- folding tall groups
// into 2 sub-columns to spend any surplus width (04rb, see renderLegendGrid);
// otherwise it falls back to a wrapped grouped bar. Neither ever truncates or
// ellipsizes a hint -- narrow terminals wrap, they never clip. The width
// source is m.help.Width (set from each WindowSizeMsg); width <= 0 means
// unbounded, so the full (unfolded) grid renders.
//
// cleanEnabled controls the contextual "C clean stale" hint (shown only when
// dangling forwards exist). WindowSizeMsg passes true to reserve the worst-case
// height; the live render passes m.hasDangling().
//
// TODO(79xb): the issue's secondary polish asks for a SECOND contextual hint
// here -- grey/hide "space on tailscale" when the selected port is already
// B/B' tailnet/LAN reachable (space would just no-op it with an info toast;
// see the pt3 guard in the space key handler). Prototyped as a second
// spaceEnabled bool threaded through here/barGroups exactly like
// cleanEnabled, but MEASURED to break TestLegendReservationDominatesLive's
// brute-force width scan: at width 54, hiding "space" while "clean" stays
// shown renders the Serve Toggles column at 6 lines against a 4-line worst-case
// reservation (both hints assumed shown) -- i.e. hiding one hint does NOT
// always make the bar shorter, because it can shift which fold split the
// shared-width-budget search (renderLegendGrid) picks for other groups too.
// That's the same "incidental tie, not enforced" fragility the comment below
// already flags for the existing single-hint case, now demonstrated to break
// with a second independent hint. Deferred per the issue ("do not block on
// this"); the handler guard (space key case, ~1673) is the shipped
// must-have and needs no bar change to be correct.
func (m model) renderLegendWith(cleanEnabled bool) string {
	groups := m.barGroups(cleanEnabled)
	grid, gridWidth := renderLegendGrid(m.help.Styles, groups, m.help.Width)
	if m.help.Width > 0 && m.help.Width < gridWidth {
		return renderGroupedBar(m.help.Styles, groups, m.help.Width)
	}
	return grid
}

// barGroups adapts keyMap.groups() for the bottom bar: it relabels "a" to
// "switch view" (its keymap help is "filtered"; the active view is shown by the
// header indicator, renderViewIndicator) and drops the contextual "C clean
// stale" unless cleanEnabled. The Serve Toggles column therefore ends at "x
// lock/unlock" (no reserved blank slot) when nothing is dangling, and gains
// "C clean stale" just above lock when a dangling forward exists.
func (m model) barGroups(cleanEnabled bool) []keyGroup {
	keys := m.keys
	keys.ShowAll.SetHelp("a", "switch view")
	keys.Clean.SetEnabled(cleanEnabled)
	// Redo is supported but stays OFF the bottom bar (3cwx, owner's call): it's
	// the rarer half of the pair and the bar is already dense. It remains in
	// groups(), so the "?" overlay and `tailport quickstart` still document it
	// -- hidden from the bar is not the same as undiscoverable.
	keys.Redo.SetEnabled(false)
	src := keys.groups()
	out := make([]keyGroup, 0, len(src))
	for _, g := range src {
		bindings := make([]key.Binding, 0, len(g.bindings))
		for _, b := range g.bindings {
			if b.Enabled() {
				bindings = append(bindings, b)
			}
		}
		if len(bindings) > 0 {
			out = append(out, keyGroup{g.name, bindings})
		}
	}
	return out
}

// legendSubColGap (04rb) is the gutter between a folded group's two
// sub-columns -- kept the same tight width as legendColGap so a folded group
// reads consistently with the inter-group gutter, per the issue's locked
// "no gutter-stretching" decision.
const legendSubColGap = legendColGap

// legendCell is one "key desc" hint, the smallest unit renderLegendGrid lays
// out.
type legendCell struct{ key, desc string }

// legendGroupLayout is one group's column-grid rendering plan: either a
// single body sub-column (unfolded) or, when folded, two column-major
// sub-columns (04rb). subcols/keyGutter/subWidth are parallel per-sub-column
// slices (len 1 or 2); width is the group's total column width (including any
// inter-sub-col gutter), used for the header cell and the inter-group gutter;
// rows is the number of body rows this group occupies, which is what makes
// folding shorten the bar -- the grid's overall height is the max rows across
// every group.
type legendGroupLayout struct {
	name      string
	subcols   [][]legendCell
	keyGutter []int
	subWidth  []int
	width     int
	rows      int
}

// legendSubColMetrics returns a sub-column's key gutter (widest key) and
// content width (keyGutter + 1 + widest desc) for the given cells.
func legendSubColMetrics(cells []legendCell) (keyGutter, width int) {
	for _, c := range cells {
		if w := lipgloss.Width(c.key); w > keyGutter {
			keyGutter = w
		}
	}
	for _, c := range cells {
		if w := keyGutter + 1 + lipgloss.Width(c.desc); w > width {
			width = w
		}
	}
	return keyGutter, width
}

// legendUnfoldedLayout lays out a group as a single body sub-column -- today's
// exact packed shape (the fold floor).
func legendUnfoldedLayout(name string, cells []legendCell) legendGroupLayout {
	keyGutter, contentWidth := legendSubColMetrics(cells)
	groupWidth := contentWidth
	if w := lipgloss.Width(name); w > groupWidth {
		groupWidth = w
	}
	return legendGroupLayout{
		name:      name,
		subcols:   [][]legendCell{cells},
		keyGutter: []int{keyGutter},
		subWidth:  []int{contentWidth},
		width:     groupWidth,
		rows:      len(cells),
	}
}

// legendFoldedLayout lays out a group as 2 column-major sub-columns (04rb,
// max 2 -- never 3+, a locked decision): the first sub-column is top-heavy,
// taking ceil(n/2) cells so it never leaves a dangling single item stranded
// atop an otherwise-empty second sub-column (e.g. Favorites' f/u/n over
// c/l). Rows within a sub-column keep read order; the two sub-columns render
// side by side on the SAME row indices (sub-column 2's first cell sits beside
// sub-column 1's first cell, not shifted down).
// legendFoldedLayout requires len(cells) >= 2 (its only caller, renderLegendGrid,
// never folds a group with fewer bindings than that -- see its "nothing to
// gain" guard), so the ceil(n/2) split below always leaves right non-empty.
func legendFoldedLayout(name string, cells []legendCell) legendGroupLayout {
	split := (len(cells) + 1) / 2 // ceil(n/2)
	left, right := cells[:split], cells[split:]
	kg1, w1 := legendSubColMetrics(left)
	kg2, w2 := legendSubColMetrics(right)
	contentWidth := w1 + legendSubColGap + w2
	groupWidth := contentWidth
	if w := lipgloss.Width(name); w > groupWidth {
		groupWidth = w
	}
	rows := len(left) // top-heavy split: left is always >= right in length
	return legendGroupLayout{
		name:      name,
		subcols:   [][]legendCell{left, right},
		keyGutter: []int{kg1, kg2},
		subWidth:  []int{w1, w2},
		width:     groupWidth,
		rows:      rows,
	}
}

// renderLegendGrid lays the groups out as an aligned column grid: a styled
// header row over, per group, a left-aligned key gutter + description. Column
// widths are computed from content (header vs the widest "key desc"), and every
// cell is padded so the columns line up. It returns the rendered grid and its
// total display width, which renderLegendWith uses as the responsive threshold.
//
// (04rb) Bar height is set by the tallest group (today, Favorites' 5
// bindings). When width leaves surplus room past the packed (unfolded) grid,
// that surplus is spent folding a group's single body sub-column into 2
// column-major sub-columns instead -- which SHORTENS the bar, rather than
// spreading the groups apart with bigger gutters (explicitly rejected: it
// just looks empty). The algorithm is greedy and tallest-group-first: sort
// groups by (unfolded) row count descending, then try folding each in turn,
// keeping a fold only if the grid still fits width and stopping at the first
// fold that doesn't -- no backtracking to try a smaller group afterward. A
// group with fewer than 2 bindings is never folded (splitting a single row
// can't shorten anything). The floor is today's exact packed layout (no
// folds fit, or width <= 0/unbounded); the ceiling is all four groups folded,
// with any leftover width past that left as trailing space on the right --
// per the issue's locked decision, no justification/stretching to fill it.
func renderLegendGrid(styles help.Styles, groups []keyGroup, width int) (string, int) {
	n := len(groups)
	if n == 0 {
		return "", 0
	}

	layouts := make([]legendGroupLayout, n)
	for i, g := range groups {
		cells := make([]legendCell, len(g.bindings))
		for j, b := range g.bindings {
			h := b.Help()
			cells[j] = legendCell{h.Key, h.Desc}
		}
		layouts[i] = legendUnfoldedLayout(g.name, cells)
	}

	totalWidth := func() int {
		total := 0
		for i, l := range layouts {
			total += l.width
			if i < n-1 {
				total += legendColGap
			}
		}
		return total
	}

	if width > 0 {
		order := make([]int, n)
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(a, b int) bool {
			// layouts are still all-unfolded here, so .rows == len(bindings) --
			// reuse it rather than re-deriving the same count from groups.
			return layouts[order[a]].rows > layouts[order[b]].rows
		})
		for _, idx := range order {
			if layouts[idx].rows < 2 {
				continue
			}
			cells := layouts[idx].subcols[0]
			prev := layouts[idx]
			layouts[idx] = legendFoldedLayout(groups[idx].name, cells)
			if totalWidth() <= width {
				continue // keep the fold, move to the next-tallest candidate
			}
			layouts[idx] = prev // didn't fit -- revert and stop entirely
			break
		}
	}

	return renderLegendRows(styles, layouts), totalWidth()
}

// renderLegendRows renders a computed set of group layouts (renderLegendGrid)
// into the final header + data-row grid string: the header row carries each
// group's name padded to its total column width, and each data row walks
// every group's sub-column(s) left to right, padding short/empty cells so
// every column -- and, within a folded group, every sub-column -- lines up
// across rows.
func renderLegendRows(styles help.Styles, layouts []legendGroupLayout) string {
	n := len(layouts)
	maxRows := 0
	for _, l := range layouts {
		if l.rows > maxRows {
			maxRows = l.rows
		}
	}

	gap := strings.Repeat(" ", legendColGap)
	subGap := strings.Repeat(" ", legendSubColGap)
	var lines []string

	// Header row: the group names, each padded to its total column width.
	var hb strings.Builder
	for c, l := range layouts {
		hb.WriteString(helpTitleStyle.Render(l.name))
		hb.WriteString(strings.Repeat(" ", l.width-lipgloss.Width(l.name)))
		if c < n-1 {
			hb.WriteString(gap)
		}
	}
	lines = append(lines, strings.TrimRight(hb.String(), " "))

	// Data rows: each group's sub-column(s), key gutter + description, padded
	// to align both across groups and, within a folded group, across its two
	// sub-columns.
	for r := 0; r < maxRows; r++ {
		var rb strings.Builder
		for c, l := range layouts {
			used := 0
			for sc, cells := range l.subcols {
				if sc > 0 {
					rb.WriteString(subGap)
					used += legendSubColGap
				}
				if r < len(cells) {
					cl := cells[r]
					rb.WriteString(styles.ShortKey.Inline(true).Render(cl.key))
					rb.WriteString(strings.Repeat(" ", l.keyGutter[sc]-lipgloss.Width(cl.key)))
					rb.WriteString(" ")
					rb.WriteString(styles.ShortDesc.Inline(true).Render(cl.desc))
					content := l.keyGutter[sc] + 1 + lipgloss.Width(cl.desc)
					rb.WriteString(strings.Repeat(" ", l.subWidth[sc]-content)) // (xqdk) pad desc out to the sub-column's full width, symmetric with the empty branch below
					used += l.subWidth[sc]
				} else {
					rb.WriteString(strings.Repeat(" ", l.subWidth[sc]))
					used += l.subWidth[sc]
				}
			}
			rb.WriteString(strings.Repeat(" ", l.width-used))
			if c < n-1 {
				rb.WriteString(gap)
			}
		}
		lines = append(lines, strings.TrimRight(rb.String(), " "))
	}
	return strings.Join(lines, "\n")
}

// renderGroupedBar is the narrow-terminal fallback: it flows every group's
// "key desc" hints inline, prefixed by the styled group name, and greedily wraps
// onto new lines so nothing overflows the width -- and nothing is ever
// truncated or elided (every key and description is still present, matching the
// old wrapBindings guarantee, just grouped). width <= 0 means unbounded.
func renderGroupedBar(styles help.Styles, groups []keyGroup, width int) string {
	type piece struct {
		text   string
		w      int
		header bool
	}
	var pieces []piece
	for _, g := range groups {
		pieces = append(pieces, piece{helpTitleStyle.Render(g.name), lipgloss.Width(g.name), true})
		for _, b := range g.bindings {
			h := b.Help()
			txt := styles.ShortKey.Inline(true).Render(h.Key) + " " + styles.ShortDesc.Inline(true).Render(h.Desc)
			pieces = append(pieces, piece{txt, lipgloss.Width(h.Key) + 1 + lipgloss.Width(h.Desc), false})
		}
	}

	var lines []string
	var cur strings.Builder
	curW := 0
	prevHeader := false
	for _, p := range pieces {
		sep, sepW := "", 0
		if curW > 0 {
			switch {
			case p.header:
				sep, sepW = "   ", 3 // wider break before a new group
			case prevHeader:
				sep, sepW = " ", 1 // header to its first hint
			default:
				sep, sepW = " · ", 3 // between hints in a group
			}
		}
		if width > 0 && curW > 0 && curW+sepW+p.w > width {
			lines = append(lines, cur.String())
			cur.Reset()
			curW = 0
			sep, sepW = "", 0
		}
		cur.WriteString(sep)
		curW += sepW
		cur.WriteString(p.text)
		curW += p.w
		prevHeader = p.header
	}
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
	}
	return strings.Join(lines, "\n")
}

// --- hidden Easter egg (28mv) ---
//
// All egg rendering goes through eggView -> Bubble Tea's View() in the
// alt-screen: styled lipgloss text and Unicode only, bounded to m.width/
// m.height. No raw tty writes, no cursor-control "glitch" -- the cursed
// aesthetic is simulated with colour + combining marks, so it can never
// desync the pty (critical: the app is often run over SSH).

var (
	eggGold        = []string{"220", "214", "178", "226", "184"}
	eggSparkColors = []string{"196", "202", "226", "46", "51", "201", "213", "129"} // KEEP vivid: also feeds fw scheme #7 (full rainbow) below -- do not mutate for 43xw
	// eggMutedColors is a SEPARATE near-monochrome grey ramp (43xw) for the
	// title + credits: a soft grey progression with one faintly-tinted step,
	// cycled per-frame like eggSparkColors was, just desaturated. Kept apart
	// from eggSparkColors so the fireworks stay vivid.
	eggMutedColors = []string{"245", "247", "250", "252", "103"}
	eggZalgoMarks  = []rune{'́', '҉', '̴', '͓', 'ͯ'}
)

// styledCell is one column of the egg-overlay compositing grid: a single
// display-width string (a rune, optionally trailed by zero-width combining
// marks) plus its colour. Composing RUNES first and styling ONLY on join is
// the crux of the fireworks feature (5x1e): you cannot index a firework glyph
// into an already-ANSI-styled string, so the whole overlay -- egg body,
// floating fanfare, credits, and the fireworks on top -- is laid into a
// width x height grid of these and rendered in one pass (same technique as the
// old eggSpin). An empty color means "write s bare" (no SGR), which is also how
// NO_COLOR / the Ascii profile degrades: the profile drops the escapes.
type styledCell struct {
	s     string
	color lipgloss.Color
	bold  bool
}

// blankCell is a bare space -- the grid's default fill.
func blankCell() styledCell { return styledCell{s: " "} }

// blankRow returns w blank cells -- used for the fanfare spacer rows (43xw):
// the rainbow sparkles were removed, but each row still needs exactly w cells
// so it occupies one full grid row (see eggView's fanfare rows).
func blankRow(w int) []styledCell {
	row := make([]styledCell, w)
	for i := range row {
		row[i] = blankCell()
	}
	return row
}

// render turns one cell into terminal output: styled when it carries a colour,
// bare otherwise. Under the Ascii colour profile (NO_COLOR / --no-color) the
// style renders without escapes, so bursts degrade to monochrome for free.
func (c styledCell) render() string {
	if c.color == "" {
		return c.s
	}
	st := lipgloss.NewStyle().Foreground(c.color)
	if c.bold {
		st = st.Bold(true)
	}
	return st.Render(c.s)
}

// newCellGrid allocates a rows x cols grid pre-filled with blank cells.
func newCellGrid(cols, rows int) [][]styledCell {
	g := make([][]styledCell, rows)
	for y := range g {
		row := make([]styledCell, cols)
		for x := range row {
			row[x] = blankCell()
		}
		g[y] = row
	}
	return g
}

// cellsToStrings joins each grid row into a styled string (bare cells stay
// bare so the result is byte-identical to composing inline).
func cellsToStrings(grid [][]styledCell) []string {
	out := make([]string, len(grid))
	for y, row := range grid {
		var b strings.Builder
		for _, c := range row {
			b.WriteString(c.render())
		}
		out[y] = b.String()
	}
	return out
}

// centerCells pads a cell row to exactly width, centred (extra column to the
// right, matching lipgloss's Center). A row already at/over width is returned
// truncated to width so it can never overflow the viewport.
func centerCells(cells []styledCell, width int) []styledCell {
	if len(cells) >= width {
		return cells[:width]
	}
	pad := width - len(cells)
	left := pad / 2
	out := make([]styledCell, width)
	for x := range out {
		out[x] = blankCell()
	}
	copy(out[left:], cells)
	return out
}

// plainCells turns a plain string into cells of one colour, folding each
// zero-width combining mark onto the preceding cell so widths stay 1:1 (this
// is what lets the eggZalgo title survive the grid intact).
func plainCells(s string, color lipgloss.Color, bold bool) []styledCell {
	var out []styledCell
	for _, r := range s {
		if unicodeCombining(r) && len(out) > 0 {
			out[len(out)-1].s += string(r)
			continue
		}
		out = append(out, styledCell{s: string(r), color: color, bold: bold})
	}
	return out
}

// unicodeCombining reports whether r is a zero-width combining mark used by
// eggZalgo (a tiny, closed set -- no need to pull in unicode tables).
func unicodeCombining(r rune) bool {
	for _, m := range eggZalgoMarks {
		if r == m {
			return true
		}
	}
	return r >= 0x0300 && r <= 0x036F // combining diacritical marks block
}

// eggHalves computes the per-row half-width of the egg silhouette for a
// nominal height h and half-width a. The profile is a CAPPED SUPERELLIPSE
// skewed so the widest point sits below centre (frag): a superellipse (n>2)
// gives fuller shoulders and rounder caps than an ellipse, the skew makes it a
// classic egg (narrow rounded top, broad rounded bottom), and trimming the
// vertical parameterisation away from the poles (cap) keeps the end rows a few
// chars wide -- rounded caps, never a single-char spike.
func eggHalves(h int, a float64) []int {
	const (
		n      = 2.4  // superellipse exponent (>2: fuller shoulders / rounder caps)
		skew   = 0.42 // pushes the widest row below centre
		capTop = 0.22 // trims the TOP pole hard: the top row starts a few cols
		//               wide and steps in one col per side -> a rounded dome,
		//               not the old pointy single-step cap.
		capBot = 0.05 // trims the bottom pole only lightly, keeping the tighter
		//               rounded point that reads as the broad base of an egg.
		bulge = 1.2 // fattens the belly by ~2 cols; sin() is 0 at the poles so
		//             the top/bottom caps are shaped by cap*/skew, not this term.
	)
	out := make([]int, h)
	for y := 0; y < h; y++ {
		t := float64(y) / float64(h-1)
		// Asymmetric cap: trim the top pole more than the bottom so the crown
		// rounds off while the base stays egg-broad (2b4r follow-up).
		u := 2*t - 1
		if u < 0 {
			u *= (1 - capTop)
		} else {
			u *= (1 - capBot)
		}
		base := math.Pow(math.Max(0, 1-math.Pow(math.Abs(u), n)), 1/n)
		half := int(math.Round(a*base*(1+skew*u) + bulge*math.Sin(math.Pi*t)))
		if half < 0 {
			half = 0
		}
		out[y] = half
	}
	// Shave one block off each side of the very top row for a smaller, rounder
	// crown than the dome's own first step would give.
	if h > 1 && out[0] > 1 {
		out[0]--
	}
	return out
}

// eggSpin renders a BIG, borderless, egg-shaped golden shimmer (amac/frag): no
// outline glyphs -- the shimmer mass IS the silhouette, from eggHalves. Because
// the skew makes some rows wider than the nominal half-width a, the field is
// padded on maxHalf (not a) so nothing overflows or panics on a negative pad.
// The shimmer band (░▒▓█) and gold shade cycle with the frame, so the solid
// mass still spins/breathes. Deterministic per (frame, maxCols, maxRows);
// dimensions clamp DOWN to the budget.
func eggSpin(frame, maxCols, maxRows int) []string {
	return cellsToStrings(eggSpinCells(frame, maxCols, maxRows))
}

// eggSpinCells is eggSpin's compositing core: it returns the shimmer as a grid
// of styledCells (rune + gold colour) instead of pre-styled strings, so the
// egg body can be laid into the fireworks grid and re-styled on join.
func eggSpinCells(frame, maxCols, maxRows int) [][]styledCell {
	h := 15 // nominal height
	if h > maxRows {
		h = maxRows
	}
	if h < 7 {
		h = 7
	}
	a := 8.0 // nominal half-width
	halves := eggHalves(h, a)
	maxHalf := maxInts(halves)

	// Scale a down so the widest row's field (2*maxHalf+1) fits maxCols.
	if maxHalf >= 1 && 2*maxHalf+1 > maxCols {
		target := (maxCols - 1) / 2
		if target < 1 {
			target = 1
		}
		a *= float64(target) / float64(maxHalf)
		if a < 2 {
			a = 2
		}
		halves = eggHalves(h, a)
		maxHalf = maxInts(halves)
	}
	if maxHalf < 1 {
		maxHalf = 1
	}
	if 2*maxHalf+1 > maxCols { // final rounding safety
		maxHalf = (maxCols - 1) / 2
		if maxHalf < 1 {
			maxHalf = 1
		}
	}

	w := 2*maxHalf + 1
	cx := maxHalf

	out := make([][]styledCell, h)
	for y := 0; y < h; y++ {
		half := halves[y]
		if half > maxHalf {
			half = maxHalf
		}
		row := make([]styledCell, w)
		for x := 0; x < w; x++ {
			if x < cx-half || x > cx+half {
				row[x] = blankCell()
				continue
			}
			// Normalise the cell's horizontal position to THIS row's width so
			// the shading hugs the silhouette (pinches at the narrow caps,
			// spreads across the belly) rather than washing straight down.
			rel := 0.0
			if half > 0 {
				rel = float64(x-cx) / float64(half)
			}
			ch, col := eggCell(x, y, h, rel, float64(frame))
			row[x] = styledCell{s: string(ch), color: col, bold: true}
		}
		out[y] = row
	}
	return out
}

// eggChars is the density ramp from shadow (sparse) to lit (solid); brightness
// picks the glyph, gold hue reinforces it (eggRampColor).
var eggChars = []rune{'░', '▒', '▓', '█'}

// eggCell returns the glyph and colour for one cell of the egg, at a
// normalised horizontal position rel in [-1,1] (left edge .. right edge of the
// row), for row y of rows total. The look composes three parts:
//
//   - Body: a broad gold gradient whose midtones carry well to the right (a
//     "chrome" body), lit from just left of centre.
//   - Glint: a specular highlight that EASES back and forth over a small
//     left-of-centre arc (the frames-32..36 motion picked from the study), so
//     it breathes in place rather than orbiting. Both body and glint use the
//     same shared cosine breath (ease-in-out: zero velocity at each extreme).
//   - Shimmer: a facing-weighted micro-sparkle -- a subtle metallic flake plus
//     rare bright "pops" -- densest over the belly (where the shell faces the
//     viewer) and fading to nothing at the silhouette, so the jewelling sits on
//     the 3D curve instead of sprinkling flat.
func eggCell(x, y, rows int, rel, frame float64) (rune, lipgloss.Color) {
	if rel < -1 {
		rel = -1
	} else if rel > 1 {
		rel = 1
	}
	const (
		omega    = 0.28    // ~2.2s per full breath at 100ms/frame
		fillAmp  = 0.40    // body midtone strength (carries right)
		fillAz   = -0.2    // body lit just left of centre
		ambient  = 0.16    // shadow-side floor
		gMid     = -0.9215 // midpoint of the frame 32..36 glint arc
		gAmp     = 0.2725  // half its span
		glintAmp = 0.62    // specular strength
		shimAmp  = 0.28    // subtle flake brightness
		popThr   = 0.66    // subtle: rare bright pops
	)
	breath := math.Cos(frame * omega)
	lon := math.Asin(rel) // -pi/2 .. pi/2 across the row
	fill := fillAmp * math.Max(0, math.Cos(lon-fillAz))
	g := gMid + gAmp*breath
	glint := math.Exp(-math.Pow((lon-g)/0.33, 2))
	br := eggClamp01(ambient + fill + glintAmp*glint)

	// Facing-weighted micro-shimmer: flake lifts the body a touch, a rare pop
	// flashes a near-white diamond. Both scale with how much the cell faces the
	// viewer, so they crowd the belly and vanish at the rim.
	nz := eggFacing(rel, y, rows)
	ph := eggCellPhase(x, y)
	br = eggClamp01(br + eggTwinkle(frame, ph, 0.7, 3)*nz*shimAmp)
	if eggTwinkle(frame, ph+2.0, 0.5, 12)*nz > popThr {
		return '█', lipgloss.Color("#fffbec") // diamond pop
	}

	idx := int(br*float64(len(eggChars)-1) + 0.5)
	if idx < 0 {
		idx = 0
	} else if idx > len(eggChars)-1 {
		idx = len(eggChars) - 1
	}
	return eggChars[idx], eggRampColor(br)
}

func eggClamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

// eggFacing approximates how much a cell's surface faces the viewer -- 1 at the
// belly centre, easing to 0 at the silhouette -- from its horizontal (rel) and
// vertical (row y of rows) position on the egg. Used to weight the shimmer onto
// the 3D curve.
func eggFacing(rel float64, y, rows int) float64 {
	nx := rel
	ny := 0.0
	if rows > 1 {
		ny = (0.5 - float64(y)/float64(rows-1)) * 2 * 0.85
	}
	v := 1 - nx*nx - ny*ny*0.7
	if v < 0 {
		return 0
	}
	return math.Sqrt(v)
}

// eggTwinkle is a sharp positive lobe: sin(...) rectified and raised to a power,
// so higher sharp -> rarer, briefer flashes. Drives the shimmer flake and pops.
func eggTwinkle(frame, phase, speed, sharp float64) float64 {
	s := math.Sin(frame*speed + phase)
	if s < 0 {
		return 0
	}
	return math.Pow(s, sharp)
}

// eggCellPhase is a deterministic per-cell phase in [0,2π) so each cell twinkles
// on its own offset (no visible seams or waves), stable across frames.
func eggCellPhase(x, y int) float64 {
	s := uint32(x)*374761393 + uint32(y)*668265263
	s = (s ^ (s >> 13)) * 1274126177
	return float64(s%6283) / 1000
}

// eggRampColor maps brightness in [0,1] to a gold gradient: deep bronze in
// shadow, through mid gold, to a near-white highlight. Truecolor hex; lipgloss
// degrades it to the nearest 256-colour on terminals without 24-bit support.
func eggRampColor(br float64) lipgloss.Color {
	dark := [3]float64{92, 63, 4}       // #5c3f04 bronze
	mid := [3]float64{217, 165, 32}     // #d9a520 gold
	bright := [3]float64{255, 244, 194} // #fff4c2 highlight
	var c [3]float64
	if br < 0.5 {
		t := br * 2
		for i := 0; i < 3; i++ {
			c[i] = dark[i] + (mid[i]-dark[i])*t
		}
	} else {
		t := (br - 0.5) * 2
		for i := 0; i < 3; i++ {
			c[i] = mid[i] + (bright[i]-mid[i])*t
		}
	}
	return lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", int(c[0]+0.5), int(c[1]+0.5), int(c[2]+0.5)))
}

func maxInts(v []int) int {
	m := 0
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}

// eggZalgo sprinkles zero-width combining marks over s for a light glitch
// accent. Combining marks don't change display width, so this can't overflow.
func eggZalgo(s string, frame int) string {
	var b strings.Builder
	for i, r := range []rune(s) {
		b.WriteRune(r)
		if (i+frame)%5 == 2 {
			b.WriteRune(eggZalgoMarks[(i+frame)%len(eggZalgoMarks)])
		}
	}
	return b.String()
}

// --- Hidden fireworks (5x1e) --------------------------------------------------
//
// A secret WITHIN the secret: inside the 'E' egg overlay, each 'f' press
// launches one ASCII firework INSTANTLY (no fuse -- firing time is just when you
// press). All the randomness lives in each firework's characteristics, sampled
// on a BELL curve (central bias) via bellRange. Fireworks live in absolute
// viewport coordinates but explode relative to the FLOATING rainbow fanfare
// rows (the block is vertically centred), so newFirework and eggView both derive
// geometry from eggLayout, the single source of truth.

// eggLayoutT is the egg overlay's computed geometry for a viewport: the centred
// block's placement plus the ABSOLUTE rows of the top/bottom fanfare (which
// float as the block is vertically centred). ok=false is the tiny-terminal
// fallback gate (matches eggView).
type eggLayoutT struct {
	ok            bool
	sw            int // inner content width (viewport - 4)
	eggCols       int // egg body width budget
	eggRows       int // egg body height == number of body rows
	blockH        int // total block height: fanfare+egg+fanfare+title+6 credits
	topPad        int // rows above the block from vertical centring
	topFanfareRow int // absolute row of the top rainbow fanfare
	botFanfareRow int // absolute row of the bottom rainbow fanfare
}

func eggLayout(w, h int) eggLayoutT {
	var l eggLayoutT
	if w < 52 || h < 17 { // must match eggView's fallback gate
		return l
	}
	l.ok = true
	l.sw = w - 4
	l.eggRows = h - 10
	if l.eggRows > 15 {
		l.eggRows = 15
	}
	l.eggCols = l.sw
	if l.eggCols > 21 {
		l.eggCols = 21
	}
	// 1 top fanfare + eggRows egg body + 1 bottom fanfare + 1 title + 6 credits.
	l.blockH = l.eggRows + 9
	l.topPad = (h - l.blockH) / 2
	if l.topPad < 0 {
		l.topPad = 0
	}
	l.topFanfareRow = l.topPad
	l.botFanfareRow = l.topPad + l.eggRows + 1
	return l
}

// Firework tuning. Units are grid cells per fireworks frame (fwInterval).
const (
	fwAspect          = 1.9   // stretch X: cells are ~2:1, so round bursts need wider X spread
	fwGravity         = 0.06  // launch gravity (rows/frame^2): gentle rise to apex
	fwEmberGravity    = 0.035 // downward pull on burst embers -> they arch and fall
	fwTrailLen        = 4     // rising-trail samples (bright head + fading tail)
	fwCountMin        = 10.0  // burst particle count (bell)
	fwCountMax        = 52.0
	fwRadiusMin       = 0.45 // burst expansion speed (bell) -> radius
	fwRadiusMax       = 2.1
	fwBurstLifeMin    = 8.0 // per-particle life in frames (bell)
	fwBurstLifeMax    = 24.0
	fwFlourishChance  = 0.4  // fraction of fireworks that get a secondary crackle
	fwLaunchSpreadPct = 0.08 // horizontal launch offset: +/-8% of viewport width
	fwTopMargin       = 1    // burst-band ceiling: bursts can reach the viewport top (row 1)

	// Muzzle smoke: a few gray particles puffed at the launch point that drift
	// up and fade. Overlapping puffs from simultaneous launches COMPOUND into a
	// denser plume (see smokeDensity) rather than last-write-wins.
	fwSmokeMin     = 4  // particles per puff (inclusive lo)
	fwSmokeMax     = 7  // particles per puff (inclusive hi)
	fwSmokeLifeMin = 6  // puff-particle life in frames (inclusive lo)
	fwSmokeLifeMax = 12 // puff-particle life in frames (inclusive hi)
)

// fwRand is the sole source of randomness for the fireworks code below
// (bellUnit, newFirework, explode, addFlourish). It's a package-level
// *rand.Rand rather than the bare package-level rand.* funcs so tests can
// swap in a fixed seed for determinism; production seeds it once, here, from
// the global auto-seeded rand, so real fireworks stay random every run.
var fwRand = rand.New(rand.NewSource(rand.Int63()))

type fwStage int

const (
	fwRising fwStage = iota // climbing the arch, drawing a trail
	fwBurst                 // exploded: expanding, then falling/fading embers
)

// fwParticle is one burst spark in absolute grid coords (+vy is downward).
type fwParticle struct {
	x, y    float64
	vx, vy  float64
	age     int
	ttl     int
	crackle bool // secondary-flourish spark: renders near-white regardless of scheme
}

// firework is one in-flight shell: a rising arch that bursts into particles.
type firework struct {
	// Launch + trajectory (absolute viewport coords; +y downward).
	x0, y0 float64
	v0     float64 // upward launch speed
	g      float64 // launch gravity
	vx     float64 // horizontal drift -> the arch lean
	tExp   float64 // frames from launch to explosion
	t      float64 // elapsed frames

	// Explosion.
	yExp, xExp float64 // burst centre (yExp is the chosen band row)
	stage      fwStage
	burstAge   int
	count      int
	radius     float64
	scheme     int
	particles  []fwParticle

	// Optional secondary crackle.
	flourish   bool
	flourishAt int
	flourished bool

	// Muzzle smoke: a gray puff at the launch point, aged every frame
	// INDEPENDENT of stage (keeps drifting/fading while the shell climbs).
	// Accumulated across shells in smokeDensity so simultaneous launches
	// compound into a denser plume.
	smoke []fwParticle

	emoji bool // glyph set captured at spawn (ascii-safe)
}

// bellUnit returns a value in [-1,1] with a central bias -- the mean of three
// uniforms (a bell/triangular shape). Bounded by construction, so unlike a
// clamped NormFloat64 there is no tail to trim: "most values cluster central".
func bellUnit() float64 {
	return (fwRand.Float64()+fwRand.Float64()+fwRand.Float64())/3*2 - 1
}

// bellRange maps bellUnit onto [lo,hi], central-biased toward the midpoint.
func bellRange(lo, hi float64) float64 {
	return lo + (bellUnit()+1)/2*(hi-lo)
}

// newFirework spawns one firework for the current viewport, INSTANTLY (fired at
// t=0 from centre-bottom). Every characteristic is bell-sampled within bounds.
func newFirework(w, h int, emoji bool) firework {
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	lay := eggLayout(w, h)

	// Launch: centre-bottom, horizontal offset bell within +/-8% of width.
	x0 := float64(w)/2 + bellRange(-fwLaunchSpreadPct*float64(w), fwLaunchSpreadPct*float64(w))
	y0 := float64(h - 1)

	// Explosion band: from the viewport top (fwTopMargin) down to a few rows
	// below the bottom fanfare -- keyed off the FLOATING fanfare rows. yExp is
	// still bell-sampled (central bias), so most bursts cluster near the egg and
	// reaching the very top is the rare, exciting exception. Degenerate (tiny)
	// layouts fall back to the mid-viewport so nothing panics.
	bandTop := float64(fwTopMargin)
	bandBot := float64(lay.botFanfareRow + 3)
	if !lay.ok {
		bandTop = float64(h) * 0.25
		bandBot = float64(h) * 0.6
	}
	if bandTop < 1 {
		bandTop = 1
	}
	if bandBot > float64(h-2) {
		bandBot = float64(h - 2)
	}
	if bandBot < bandTop {
		bandBot = bandTop
	}
	// Central bias -> most bursts land near the egg's middle.
	yExp := bellRange(bandTop, bandBot)

	// Explosion phase relative to apex (bell): <1 before apex, ~1 at apex,
	// >1 slightly past it (on the way down).
	alpha := bellRange(0.68, 1.18)
	denom := alpha - 0.5*alpha*alpha
	if denom < 0.15 {
		denom = 0.15
	}
	rise := y0 - yExp
	if rise < 1 {
		rise = 1
	}
	// Solve launch speed so the arch reaches yExp exactly at t=tExp with the
	// requested apex phase: y(t) = y0 - v0*t + g*t^2/2, apex at t=v0/g.
	reach := rise / denom // = v0^2 / g
	v0 := math.Sqrt(fwGravity * reach)
	tExp := alpha * (v0 / fwGravity)

	// Horizontal lean: an aggressive sweep (~31 deg off vertical) from the tight
	// base. But the tallest shots have a long tExp, so an unclamped +/-0.9 drift
	// would carry the burst centre x0+vx*tExp off-screen and burst half-clipped.
	// Clamp vx so the predicted centre stays a couple cells inside the viewport;
	// on a wide terminal hi is typically >0.9, so most shots keep the full lean --
	// the clamp is a safety net (narrow terminals / tallest shots), not a general
	// flattening of tall arcs.
	vx := bellRange(-0.9, 0.9)
	margin := 2.0
	if tExp > 0 {
		lo := (margin - x0) / tExp
		hi := (float64(w-1) - margin - x0) / tExp
		if vx < lo {
			vx = lo
		}
		if vx > hi {
			vx = hi
		}
	}

	// Muzzle smoke: a few gray particles at the launch point with small
	// upward+outward velocity and a short life. Origins are all within +/-8% of
	// centre, so simultaneous puffs overlap heavily and compound (smokeDensity).
	nSmoke := fwSmokeMin + fwRand.Intn(fwSmokeMax-fwSmokeMin+1)
	smoke := make([]fwParticle, 0, nSmoke)
	for i := 0; i < nSmoke; i++ {
		smoke = append(smoke, fwParticle{
			x:   x0 + bellRange(-0.75, 0.75),
			y:   y0,
			vx:  bellRange(-0.2, 0.2),
			vy:  bellRange(-0.35, -0.1),
			ttl: fwSmokeLifeMin + fwRand.Intn(fwSmokeLifeMax-fwSmokeLifeMin+1),
		})
	}

	return firework{
		x0:         x0,
		y0:         y0,
		v0:         v0,
		g:          fwGravity,
		vx:         vx,
		tExp:       tExp,
		yExp:       yExp,
		stage:      fwRising,
		count:      int(bellRange(fwCountMin, fwCountMax)),
		radius:     bellRange(fwRadiusMin, fwRadiusMax),
		scheme:     fwRand.Intn(len(fwSchemes)),
		flourish:   fwRand.Float64() < fwFlourishChance,
		flourishAt: 3 + fwRand.Intn(4),
		smoke:      smoke,
		emoji:      emoji,
	}
}

func (f *firework) posY(t float64) float64 { return f.y0 - f.v0*t + 0.5*f.g*t*t }
func (f *firework) posX(t float64) float64 { return f.x0 + f.vx*t }

// step advances one firework by one frame: climb then explode, then age the
// embers (with gravity) and, once, add a flourish crackle.
func (f *firework) step() {
	f.t++
	// Muzzle smoke ages every frame, INDEPENDENT of stage, so a shell's puff
	// keeps drifting/fading while it climbs (and outlives a short shot's burst).
	if len(f.smoke) > 0 {
		alive := f.smoke[:0]
		for _, p := range f.smoke {
			p.x += p.vx
			p.y += p.vy
			p.age++
			if p.age < p.ttl {
				alive = append(alive, p)
			}
		}
		f.smoke = alive
	}
	switch f.stage {
	case fwRising:
		if f.t >= f.tExp {
			f.explode()
		}
	case fwBurst:
		f.burstAge++
		alive := f.particles[:0]
		for _, p := range f.particles {
			p.vy += fwEmberGravity
			p.x += p.vx
			p.y += p.vy
			p.age++
			if p.age < p.ttl {
				alive = append(alive, p)
			}
		}
		f.particles = alive
		if f.flourish && !f.flourished && f.burstAge >= f.flourishAt {
			f.addFlourish()
			f.flourished = true
		}
	}
}

// done reports a spent firework: burst, all embers expired, any flourish already
// emitted (so we don't reap it before the secondary crackle fires), AND its
// muzzle smoke fully faded. The smoke clause is load-bearing: a minimal-rise
// shot explodes at tExp ~4 frames while smoke ttl runs to ~12, so without it a
// short shot could be reaped mid-puff (a dead heat) -- the extra check is cheap
// and removes that fragile timing coupling entirely.
func (f *firework) done() bool {
	return f.stage == fwBurst && len(f.particles) == 0 && len(f.smoke) == 0 && (!f.flourish || f.flourished)
}

// explode converts the rising shell into a radial burst at the arch's current
// point (== yExp by construction).
func (f *firework) explode() {
	f.stage = fwBurst
	f.burstAge = 0
	f.xExp = f.posX(f.tExp)
	f.yExp = f.posY(f.tExp)
	n := f.count
	if n < 1 {
		n = 1
	}
	for i := 0; i < n; i++ {
		ang := 2*math.Pi*float64(i)/float64(n) + (fwRand.Float64()-0.5)*0.6
		speed := f.radius * (0.45 + 0.55*fwRand.Float64())
		f.particles = append(f.particles, fwParticle{
			x:   f.xExp,
			y:   f.yExp,
			vx:  math.Cos(ang) * speed * fwAspect,
			vy:  math.Sin(ang) * speed,
			ttl: int(bellRange(fwBurstLifeMin, fwBurstLifeMax)),
		})
	}
}

// addFlourish emits a brief bright secondary crackle from the burst centre.
func (f *firework) addFlourish() {
	n := 6 + fwRand.Intn(6)
	for i := 0; i < n; i++ {
		ang := 2*math.Pi*float64(i)/float64(n) + fwRand.Float64()
		speed := f.radius * 1.4 * (0.5 + 0.6*fwRand.Float64())
		f.particles = append(f.particles, fwParticle{
			x:       f.xExp,
			y:       f.yExp,
			vx:      math.Cos(ang) * speed * fwAspect,
			vy:      math.Sin(ang) * speed,
			ttl:     4 + fwRand.Intn(4),
			crackle: true,
		})
	}
}

// stepFireworks advances every firework and reaps the spent ones (filter in
// place, no per-frame allocation).
func stepFireworks(fws []firework) []firework {
	kept := fws[:0]
	for i := range fws {
		fw := fws[i]
		fw.step()
		if !fw.done() {
			kept = append(kept, fw)
		}
	}
	return kept
}

// stepPoof advances the poof dissolve by one frame -- pure, like
// stepFireworks: it only advances the counters (frame up, ttl down). The
// actual dissolve glyphs are derived at render time (renderPoofLine) from
// frame/ttl rather than baked into the model here, so there's a single source
// of truth for "how dissolved is it right now."
func stepPoof(p poofState) poofState {
	p.frame++
	p.ttl--
	return p
}

// renderPoofLine renders one frame of the poof's dissolve (kata dw57): the
// descriptor's characters fade via the SAME glyph vocabulary + colour ramp the
// hidden fireworks reuse -- fwGlyphsUnicode/ASCII gated on emoji, and
// eggRampColor for the dim-out -- reusing the fireworks AESTHETIC only (never
// its ticker or *firework methods; the poof is a sibling animation, design
// §3.5). Each rune dissolves on its own small deterministic offset
// (poofCharPhase) so the descriptor crumbles unevenly rather than fading as
// one flat block; a rune whose own progress has run out renders as a blank
// space. width, when > 0, truncates the descriptor BEFORE rendering so the
// line can never exceed the terminal width -- no wrap, no horizontal scroll,
// mirroring how renderStatusLine bounds m.flash to m.width.
func renderPoofLine(p poofState, width int) string {
	text := p.text
	if width > 0 {
		if r := []rune(text); len(r) > width {
			text = string(r[:width])
		}
	}

	total := p.frame + p.ttl
	if total <= 0 {
		total = 1
	}
	progress := float64(p.frame) / float64(total)

	set := fwGlyphsASCII
	if p.emoji {
		set = fwGlyphsUnicode
	}

	runes := []rune(text)
	out := make([]string, len(runes))
	for i, r := range runes {
		if r == ' ' {
			out[i] = " "
			continue
		}
		// Stagger this rune's own fade around the shared progress so
		// neighbouring characters don't all wink out on the same frame.
		jitter := (poofCharPhase(i) - 0.5) * 0.5
		br := eggClamp01(1 - progress - jitter)
		switch {
		case br <= 0:
			out[i] = " "
		case br >= 0.8:
			// Still bright enough to read as itself -- the descriptor gets a
			// beat to register before it starts crumbling into glyphs.
			out[i] = lipgloss.NewStyle().Foreground(eggRampColor(br)).Render(string(r))
		default:
			idx := int(br / 0.8 * float64(len(set)-1))
			if idx < 0 {
				idx = 0
			} else if idx > len(set)-1 {
				idx = len(set) - 1
			}
			out[i] = lipgloss.NewStyle().Foreground(eggRampColor(br)).Render(string(set[idx]))
		}
	}
	return strings.Join(out, "")
}

// poofCharPhase is a deterministic per-index offset in [0,1) -- mirrors
// eggCellPhase's role for the egg shimmer -- so each character of the poof
// dissolves on its own stagger instead of every character changing glyph on
// the exact same frame.
func poofCharPhase(i int) float64 {
	s := uint32(i)*2654435761 + 1
	s = (s ^ (s >> 15)) * 0x85ebca6b
	s = s ^ (s >> 13)
	return float64(s%1000) / 1000
}

var (
	fwGlyphsUnicode = []rune{'·', '░', '▒', '▓', '█'} // dot · light/medium/dark shade · full block
	fwGlyphsASCII   = []rune{'.', ':', '+', '#', '@'}

	// Option A (sparkle -> solid block): delicate sparkle embers with a solid
	// core. Kept as a documented alternative. We chose B (shade gradient)
	// because the ░▒▓█ density falloff reads as a glowing BLOOM rather than a
	// hard pixel, and the block elements are the most universally-supported and
	// uniformly single-width glyphs (no rendering gamble, no alignment risk).
	// fwGlyphsUnicode = []rune{'·', '✦', '❋', '▓', '█'}
	// fwGlyphsASCII   = []rune{'.', '+', '*', '#', '@'}
)

// glyphSet gates the glyph vocabulary on emoji capability (5x1e is stricter than
// the egg's sparkles, which assumed UTF-8): ascii terminals get pure-ASCII sparks.
func (f *firework) glyphSet() []rune {
	if f.emoji {
		return fwGlyphsUnicode
	}
	return fwGlyphsASCII
}

// glyphFloor is glyph() with a minimum index, so callers that must never render
// the faintest glyph (e.g. the rising trail, whose lone '·' over the egg text
// punches a whitespace hole -- jkbp) can floor at 1 (░ / ':').
func (f *firework) glyphFloor(br float64, floor int) rune {
	set := f.glyphSet()
	i := int(br * float64(len(set)))
	if i < floor {
		i = floor
	}
	if i > len(set)-1 {
		i = len(set) - 1
	}
	return set[i]
}

// glyph picks a spark by brightness (dim -> bright) from the active set.
func (f *firework) glyph(br float64) rune { return f.glyphFloor(br, 0) }

// draw renders a firework into the grid at its current frame. Fireworks are
// drawn last (in eggView), so they sit ON TOP of the egg/fanfare/credits.
func (f *firework) draw(grid [][]styledCell, w, h int) {
	switch f.stage {
	case fwRising:
		// A comet: muted head plus a few analytic samples behind it, fading.
		// (a2vq) Real fireworks ascend mostly dark -- the bright moment is the
		// burst -- so the head is no longer pinned to br=1.0. Instead it's a
		// faint ember (br~0.25) for most of the climb, with a brief soft
		// warm-gold "gunpowder" glow (br~0.45) over the bottom ~15% of the
		// flight that decays into the ember as the shell rises. The trail
		// keeps its old relative comet fade (0.24/step), just measured off
		// the muted head instead of a full-bright one.
		frac := 1.0 // guard f.tExp<=0: treat as "past ignition" -> launch=0
		if f.tExp > 0 {
			frac = f.t / f.tExp
		}
		launch := math.Max(0, 1-frac/0.15) // 1 at ignition, ->0 by ~15% up
		const ember = 0.25                 // faint coast brightness
		headBr := ember + launch*(0.45-ember)
		// (w9sh) Pin the HEAD glyph to the block (last in the active set)
		// regardless of brightness, so the comet has a coherent solid head
		// instead of a single faint dot.
		// (zm95) Color: fwScheme.colorAt ignores br entirely for kind-2
		// (vivid) schemes -- it returns a fixed full-bright ANSI-palette
		// color -- so f.color(br, 0) rendered vivid-scheme ascents as
		// full-bright solid streaks once w9sh swapped the dot for a block.
		// eggRampColor(br) is br-driven for every scheme (dims to bronze
		// #5c3f04 at low br), so use it here to restore a genuinely dim
		// ember ascent across ALL schemes; the burst still uses f.color
		// (the scheme's own bright palette) and is untouched.
		// Trail glyph: keep the head (k==0) as the solid block (w9sh), but
		// let trailing samples ramp through f.glyph(br) (█▓▒░) so the comet
		// visibly fades/recedes behind the head instead of being a uniform
		// streak of blocks.
		set := f.glyphSet()
		block := set[len(set)-1]
		for k := 0; k < fwTrailLen; k++ {
			tt := f.t - float64(k)
			if tt < 0 {
				break
			}
			br := headBr - 0.24*float64(k)
			if br < 0 {
				br = 0
			}
			col := eggRampColor(br)
			if launch > 0 {
				// Some schemes use ANSI-index palettes (e.g. "196",
				// eggSparkColors) that don't RGB-blend cleanly, so rather
				// than crossfade numerically we just render the brief
				// launch window in a fixed soft warm-gold ("gunpowder").
				// It hands straight to the ember ramp once launch hits
				// 0, and pairs with the existing gray muzzle-smoke puff
				// (fwSmoke*) at the launch point.
				col = lipgloss.Color("#ffb454")
			}
			glyph := block
			if k > 0 {
				// Floor at index 1 (░/':') -- never the bare '·', which would
				// punch a whitespace hole through the egg text (jkbp).
				glyph = f.glyphFloor(br, 1)
			}
			f.plot(grid, w, h, f.posX(tt), f.posY(tt), glyph, col)
		}
	case fwBurst:
		set := f.glyphSet()
		for i, p := range f.particles {
			br := 1 - float64(p.age)/float64(p.ttl)
			if br < 0 {
				br = 0
			}
			if p.crackle { // bright near-white regardless of scheme
				g := set[len(set)-1]
				if br < 0.5 {
					g = set[len(set)-2]
				}
				f.plot(grid, w, h, p.x, p.y, g, lipgloss.Color("#fffbec"))
				continue
			}
			f.plot(grid, w, h, p.x, p.y, f.glyph(br), f.color(br, i))
		}
	}
}

// plot writes one styled spark into the grid, rounding to the nearest cell and
// clipping to the viewport (so bursts near the edges never panic or overflow).
func (f *firework) plot(grid [][]styledCell, w, h int, x, y float64, glyph rune, color lipgloss.Color) {
	gx := int(math.Round(x))
	gy := int(math.Round(y))
	if gx < 0 || gx >= w || gy < 0 || gy >= h {
		return
	}
	grid[gy][gx] = styledCell{s: string(glyph), color: color, bold: true}
}

// color resolves a spark's colour from this firework's scheme.
func (f *firework) color(br float64, idx int) lipgloss.Color {
	return fwSchemes[f.scheme].colorAt(br, idx)
}

// fwScheme is one of ~8 firework colour "types", from single-hue monochrome to
// multi-hue vivid. kind 0 reuses the egg's gold ramp; 1 is a monochrome
// brightness ramp; 2 is a vivid ANSI palette sampled per particle.
type fwScheme struct {
	kind    int
	base    [3]float64
	palette []string
}

var fwSchemes = []fwScheme{
	{kind: 0},                                                    // gold (ties to the egg; eggRampColor)
	{kind: 1, base: [3]float64{235, 45, 45}},                     // red monochrome
	{kind: 1, base: [3]float64{50, 210, 70}},                     // green monochrome
	{kind: 1, base: [3]float64{70, 130, 245}},                    // blue monochrome
	{kind: 1, base: [3]float64{205, 70, 215}},                    // magenta monochrome
	{kind: 2, palette: []string{"196", "202", "214", "226"}},     // warm vivid (red -> gold)
	{kind: 2, palette: []string{"51", "45", "39", "201", "129"}}, // cool vivid (cyan -> violet)
	{kind: 2, palette: eggSparkColors},                           // full rainbow (reuse egg palette)
}

func (s fwScheme) colorAt(br float64, idx int) lipgloss.Color {
	switch s.kind {
	case 0:
		return eggRampColor(br) // reuse the egg's gold gradient
	case 2:
		if len(s.palette) == 0 {
			return eggRampColor(br)
		}
		return lipgloss.Color(s.palette[((idx%len(s.palette))+len(s.palette))%len(s.palette)])
	default:
		return fwMonoColor(s.base, br)
	}
}

// fwMonoColor is a single-hue brightness ramp (dark -> base -> white), the
// monochrome end of the scheme range. Truecolor hex; lipgloss degrades it to
// 256/ANSI, and to nothing under NO_COLOR / --no-color (the Ascii profile).
func fwMonoColor(base [3]float64, br float64) lipgloss.Color {
	br = eggClamp01(br)
	dark := [3]float64{base[0] * 0.28, base[1] * 0.28, base[2] * 0.28}
	white := [3]float64{255, 255, 255}
	var c [3]float64
	if br < 0.6 {
		t := br / 0.6
		for i := 0; i < 3; i++ {
			c[i] = dark[i] + (base[i]-dark[i])*t
		}
	} else {
		t := (br - 0.6) / 0.4
		for i := 0; i < 3; i++ {
			c[i] = base[i] + (white[i]-base[i])*t
		}
	}
	return lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", int(c[0]+0.5), int(c[1]+0.5), int(c[2]+0.5)))
}

// smokeDensity accumulates every live shell's muzzle smoke into a w x h buffer:
// each particle adds its remaining-life fraction into its rounded cell, summed
// ACROSS all fireworks. That summation is the compounding requirement -- two
// puffs overlapping a cell make it denser than one (NOT last-write-wins), so
// simultaneous launches (all within +/-8% of centre) build a thicker plume.
// Returns nil when no smoke is on-screen. Bounds-clipped like plot, so the
// caller's grid stays exactly w x h.
func smokeDensity(fws []firework, w, h int) [][]float64 {
	if w <= 0 || h <= 0 {
		return nil
	}
	var dens [][]float64
	for i := range fws {
		for _, p := range fws[i].smoke {
			if p.ttl <= 0 {
				continue
			}
			frac := 1 - float64(p.age)/float64(p.ttl)
			if frac <= 0 {
				continue
			}
			gx := int(math.Round(p.x))
			gy := int(math.Round(p.y))
			if gx < 0 || gx >= w || gy < 0 || gy >= h {
				continue
			}
			if dens == nil {
				dens = make([][]float64, h)
				for y := range dens {
					dens[y] = make([]float64, w)
				}
			}
			dens[gy][gx] += frac
		}
	}
	return dens
}

// smokeCell maps an accumulated density to one gray smoke cell: a heavier glyph
// and brighter gray where puffs compound. Glyphs are gated on emoji like the
// sparks (shade blocks with UTF-8, an ascii ramp otherwise, so no mojibake), and
// the gray hex degrades to nothing under NO_COLOR / the Ascii profile -- the
// glyph alone still reads.
func smokeCell(d float64, emoji bool) styledCell {
	glyphs := []rune{'.', ':', '#'}
	if emoji {
		glyphs = []rune{'░', '▒', '▓'}
	}
	i := 0
	switch {
	case d >= 1.8:
		i = 2
	case d >= 0.8:
		i = 1
	}
	// Density -> gray brightness: dim for a lone speck, brighter where puffs pile
	// up, clamped so a heavy overlap stays a soft gray (not a glaring white).
	lvl := 0.32 + 0.42*d
	if lvl > 0.74 {
		lvl = 0.74
	}
	g := int(lvl * 255)
	return styledCell{s: string(glyphs[i]), color: lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", g, g, g))}
}

// eggView renders the full-screen Easter egg for the current frame. It composes
// a width x height grid of styledCells -- the centred egg block laid at its
// computed offset, then any fireworks (5x1e) drawn on top -- and styles each
// cell only on join. Composing RUNES first is what lets fireworks overlay the
// egg without indexing into ANSI-styled strings. Tiny terminals get a bounded
// fallback rather than any risk of overflow.
func (m model) eggView() string {
	w, h := m.width, m.height
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	f := m.eggFrame

	// The full art needs room for a ~7+ row egg plus ~9 rows of fanfare
	// spacers/title/credits/hint (eggLayout's gate: w >= 52, h >= 17). Below
	// it, a bounded fallback and no fireworks (nowhere safe to place them).
	lay := eggLayout(w, h)
	if !lay.ok {
		msg := lipgloss.NewStyle().Foreground(lipgloss.Color(eggGold[f%len(eggGold)])).Bold(true).
			Render("🥚 enlarge the terminal — esc: back")
		return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, msg)
	}

	sw := lay.sw

	// Compose the egg block as ROWS OF CELLS (never pre-styled strings) so the
	// fireworks share the grid. Row order fixes the fanfare rows the burst band
	// keys off (top fanfare, eggRows body, bottom fanfare, title, 6 credits).
	// The fanfare rows are BLANK spacers (43xw): rainbow sparkles were removed,
	// but each row still occupies exactly one grid row so topFanfareRow/
	// botFanfareRow keep anchoring the fireworks burst band (mm5g) unchanged.
	var block [][]styledCell
	block = append(block, centerCells(blankRow(sw), sw)) // top fanfare (blank spacer, burst-band anchor)
	for _, row := range eggSpinCells(f, lay.eggCols, lay.eggRows) {
		block = append(block, centerCells(row, sw)) // egg body
	}
	block = append(block, centerCells(blankRow(sw), sw)) // bottom fanfare (blank spacer, burst-band anchor)

	// Title + credits are muted near-monochrome grey (43xw): eggMutedColors is
	// a SEPARATE palette from eggSparkColors, which must stay vivid for fw
	// scheme #7 (full rainbow).
	titleColor := lipgloss.Color(eggMutedColors[f%len(eggMutedColors)])
	block = append(block, centerCells(plainCells("✦ "+eggZalgo("tailport", f)+" ✦", titleColor, true), sw))

	credits := []string{
		"Michael E. Gruen",
		"· The LLM Agent Fleet ·",
		"Claude Opus 4.8 · Sonnet 5 · Haiku 4.5",
		eggURL,
		eggRepoURL,
		"c: copy site · g: copy repo · esc / q: back",
	}
	for i, s := range credits {
		color := lipgloss.Color(eggMutedColors[(f/2+i)%len(eggMutedColors)])
		if i == len(credits)-1 {
			color = lipgloss.Color("241") // the muted hint line
		}
		block = append(block, centerCells(plainCells(s, color, false), sw))
	}

	// Lay the centred block into a full-screen grid at its computed offset.
	grid := newCellGrid(w, h)
	const leftPad = 2 // (w - sw) / 2 with sw = w - 4
	for i, row := range block {
		gy := lay.topPad + i
		if gy < 0 || gy >= h {
			continue
		}
		for j, c := range row {
			if gx := leftPad + j; gx >= 0 && gx < w {
				grid[gy][gx] = c
			}
		}
	}

	// The transient copy toast sits just below the block (never over the
	// fanfare rows the fireworks band was computed against).
	if m.flash != "" {
		if fy := lay.topPad + lay.blockH; fy >= 0 && fy < h {
			for j, c := range centerCells(plainCells(m.flash, lipgloss.Color("42"), true), sw) {
				if gx := leftPad + j; gx >= 0 && gx < w {
					grid[fy][gx] = c
				}
			}
		}
	}

	// Muzzle smoke UNDER the sparks: accumulate every live shell's smoke into a
	// density buffer (overlapping puffs compound), then paint each non-empty cell
	// as a gray plume. Bounds-clipped in smokeDensity, so the grid stays w x h.
	if dens := smokeDensity(m.fireworks, w, h); dens != nil {
		for y := range dens {
			for x, d := range dens[y] {
				if d > 0 {
					grid[y][x] = smokeCell(d, m.emoji)
				}
			}
		}
	}

	// Fireworks last -> ON TOP of the egg/fanfare/credits/smoke.
	for i := range m.fireworks {
		m.fireworks[i].draw(grid, w, h)
	}

	return strings.Join(cellsToStrings(grid), "\n")
}

// KeyLegendRow is one row of tailport's full keybinding legend: a key label
// and its (possibly multi-line, "\n"-joined) description.
type KeyLegendRow struct {
	Key  string
	Desc string
}

// KeyLegendGroup is one like-for-like section of the full keybinding legend
// (kata p39s): a section name and its rows, in display order. It is the grouped
// SINGLE SOURCE OF TRUTH for the "?" overlay (helpView) and `tailport
// quickstart` (kata x4cg), which both render off KeyLegendGroups so they can
// never drift -- and its section names/order/membership come from the very same
// keyMap.groups() the bottom-bar grid uses, so the overlay and the bar can't
// drift either.
type KeyLegendGroup struct {
	Name string
	Rows []KeyLegendRow
}

// keyLegendDescs maps a binding's display key to its rich "?"/quickstart prose
// (deliberately fuller than the terse bottom-bar labels). emoji picks which
// exposure glyph (🌒/🌑/🌫️ vs ◉/●/▲) is quoted inline in the space/p/C rows,
// matching whichever marker set the caller is using (see resolveMarkerEmoji;
// callers pass m.markerEmoji, not m.emoji -- this is exposure-marker prose,
// not egg/fireworks).
func keyLegendDescs(emoji bool) map[string]string {
	served, funneled, published, dangling := "◉", "●", "◆", "▲"
	if emoji {
		served, funneled, published, dangling = "🌒", "🌑", "🌐", "🌫️"
	}
	return map[string]string{
		"space": "Toggle tailscale serve for the selected port on/off. Once a port\nis served (" + served + ") its tailnet URL is shown beneath it. Only offered\nfor a loopback-bound port -- one already reachable on the tailnet\nneeds no serving, so space is a no-op there.",
		// p/P swapped (vzj4): funnel now lives under "P", publish under "p".
		"P":      "Funnel the selected port to the PUBLIC INTERNET via tailscale\nfunnel (" + funneled + "), behind a strong y/n confirm. Funnel is HTTPS-only and\ncan use just three public ingress ports — 443, 8443, 10000\n(auto-assigned, max three at once) — so the public port won't match\nthe local one. :22 (SSH) is refused. Press P again to drop the port\nback to tailnet-served.",
		"p":      "Publish the selected port to a custom public hostname (" + published + ") through\nyour own Caddy edge over the tailnet (kata v1z5), behind a strong\ny/n confirm naming the exact https://<hostname>. A SECOND public path,\nindependent of and mutually exclusive with funnel — a port can carry\none or the other, never both. Optional basic auth at the edge; :22\nrefused; auto-enables serve first. First publish prompts for\ncaddy.hostname/domain if unset (see docs/caddy-edge.md). Press p\nagain to unpublish.",
		"c":      "Copy the selected port's URL to the clipboard, via OSC 52 so it\nworks even over SSH (needs a terminal that supports it; tmux: set -g\nset-clipboard on). It copies the URL for the port's current exposure: a\nPUBLISHED port's public https://<hostname>, a LAN bind's\nhttp://<lan-ip>:<port>, a localhost-only or offline port's\nhttp://localhost:<port>, otherwise the tailnet http://<host>:<port>\n(served, tailnet, funnel). The copy is confirmed inline with a ✓, or by\na toast that names the exact URL copied.",
		"f":      "Favorite the selected port (marks it ★). Favorites are a durable\nshortlist — one of the two `a` views — that survives restarts and\nstays visible even when the process isn't running.",
		"F":      "Forget the selected port: clears ★ and drops it out of the\nFavorites view. Shift-F, so a stray f-key press can't undo your\nshortlist. (This was \"u\" before; u is undo now.)",
		"u":      "Undo the last registry edit — favorite, forget, label, lock or\nadd. Stepping back through them one at a time; " + strconv.Itoa(undoStackLimit) + " deep, this session\nonly. It does NOT touch what's exposed: serve and funnel have\ntheir own keys and confirms, and undo never flips them. (To restore a\nforce-purged route is a SEPARATE affordance on its own key — R,\nshown in the status line right after the purge — not this.)",
		"ctrl+r": "Redo the last undone registry edit. Any new edit clears the redo\nstack, so you can't redo onto a changed registry.",
		"n":      "Add a port by number to Favorites (★), even one not currently\nlistening. It doesn't serve — it just registers and sticks in the\nFavorites view; press space there to serve it once its service is up.",
		"l":      "Set a text label for the selected port.",
		"x":      "Lock / unlock the selected port (🔒). A locked port can't be\ntoggled on until you unlock it — a guard against exposing something\nby accident. Port :22 is locked by default; unlocking it requires\ntyping \"ssh\" to confirm (it guards your SSH access).",
		"C":      "Tear down stale forwards — ports still served by tailscale with\nnothing listening locally (shown " + dangling + "). Offered only when some exist.",
		"/":      "Filter by port number, process, or label (fuzzy). Searches ALL\nlistening ports regardless of view, so it works even from an empty\nFavorites screen; non-favorite matches show dimmed in the Favorites\nview. esc clears the filter.",
		"a":      "Switch between the two list views: Favorites (only ★ ports) and\nAll ports (every port listening locally, plus your favorites even\nwhen their process is down).",
		"r":      "Refresh the port list and serve status.",
		"?":      "Toggle this help. esc or q also close it.",
		"q":      "Quit.",
	}
}

// KeyLegendGroups returns the full keybinding legend grouped into the same four
// sections, in the same order, as the bottom-bar grid -- Serve Toggles, Favorites,
// View, App -- each row carrying the RICH prose (keyLegendDescs), not the terse
// bar label. The sections and their membership are taken from keyMap.groups(),
// the one grouping source, so the "?" overlay and the bar cannot diverge.
func KeyLegendGroups(emoji bool) []KeyLegendGroup {
	descs := keyLegendDescs(emoji)
	src := newKeyMap().groups()
	out := make([]KeyLegendGroup, 0, len(src))
	for _, g := range src {
		rows := make([]KeyLegendRow, 0, len(g.bindings))
		for _, b := range g.bindings {
			k := b.Help().Key
			rows = append(rows, KeyLegendRow{Key: k, Desc: descs[k]})
		}
		out = append(out, KeyLegendGroup{Name: g.name, Rows: rows})
	}
	return out
}

// KeyLegendRows returns the full legend as a flat row list (grouped order,
// sections dropped), derived from KeyLegendGroups. Kept for callers that want
// the flat form; the "?" overlay and quickstart use the grouped renderer.
func KeyLegendRows(emoji bool) []KeyLegendRow {
	var out []KeyLegendRow
	for _, g := range KeyLegendGroups(emoji) {
		out = append(out, g.Rows...)
	}
	return out
}

// RenderKeyLegend formats rows as tailport's standard legend block: a bold,
// left-padded key column followed by its description, with any continuation
// lines indented to align beneath the description column.
func RenderKeyLegend(rows []KeyLegendRow) string {
	var b strings.Builder
	for _, r := range rows {
		lines := strings.Split(r.Desc, "\n")
		b.WriteString("  " + helpKeyStyle.Render(fmt.Sprintf("%-6s", r.Key)) + "  " + helpTextStyle.Render(lines[0]) + "\n")
		for _, extra := range lines[1:] {
			b.WriteString("          " + helpTextStyle.Render(extra) + "\n")
		}
	}
	return b.String()
}

// RenderKeyLegendGroups renders the grouped legend for the "?" overlay and
// `tailport quickstart`: each section led by its styled name (helpTitleStyle),
// then its rows via RenderKeyLegend, with a blank line between sections. Shared
// verbatim by both so they can never render different text.
func RenderKeyLegendGroups(groups []KeyLegendGroup) string {
	var b strings.Builder
	for i, g := range groups {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(helpTitleStyle.Render(g.Name) + "\n")
		b.WriteString(RenderKeyLegend(g.Rows))
	}
	return b.String()
}

// helpView renders the full-screen "?" overlay: a short intro to what
// tailport is, then a real explanation of every key (not the terse legend).
// It replaces the whole View while m.showHelp is set.
// configSaveLines describes where preferences are persisted, for the help
// overlay (gahj). Given the resolved path it returns a headline plus the exact
// location, abbreviating $HOME to ~ when the path is under it (cosmetic). When
// path is empty (config.Path() errored) it falls back to stating the rule so
// the overlay never shows nothing.
func configSaveLines(path string) []string {
	const head = "Settings (favorites, labels, locks) are saved to:"
	if path == "" {
		return []string{
			head,
			"  $XDG_CONFIG_HOME/tailport/config.yaml, or",
			"  ~/.config/tailport/config.yaml",
		}
	}
	loc := path
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if prefix := home + string(os.PathSeparator); strings.HasPrefix(loc, prefix) {
			loc = "~" + string(os.PathSeparator) + loc[len(prefix):]
		}
	}
	return []string{head, "  " + loc}
}

// markerLegend describes the exposure-state glyph column: the 7-state
// moon-phase reach ramp (1exs/79xb), using whichever marker set (emoji or
// ASCII) is active, plus the always-present lock/favorite. Wrapped onto three
// lines -- the ramp states, the two off-ramp broken states, then the
// decorations -- so it stays readable rather than one very long line.
func (m model) markerLegend() string {
	if m.markerEmoji {
		return "🌕 localhost   🌔 local network   🌒 on tailnet (served or bound wide)\n" +
			"🌑 internet (funnel)   🌐 internet (published)   🌫️ stale   ✕ offline\n" +
			"🔒 locked   ★ favorite   " + authGlyphEmoji + " basic auth (published)"
	}
	return "○ localhost   ◔ local network   ◉ on tailnet (served or bound wide)\n" +
		"● internet (funnel)   ◆ internet (published)   ▲ stale   ✕ offline\n" +
		"🔒 locked   ★ favorite   " + authGlyphMono + " basic auth (published)"
}

// OperatorSetupText is the prerequisites prose shared by the "?" overlay's
// "Setup / prerequisites" section (see helpView) and `tailport quickstart`
// (kata tapv), so the two can't drift apart -- mirroring how
// KeyLegendGroups/RenderKeyLegendGroups already share the keybinding legend
// between them. operatorUser is expected $USER EXPANDED (see
// tsserve.CurrentUsername) so the fix command is directly copy-pasteable; a
// "<you>" placeholder is substituted if it's empty (couldn't be determined).
func OperatorSetupText(operatorUser string) string {
	you := operatorUser
	if you == "" {
		you = "<you>"
	}
	return "tailscale itself requires an operator to be set before a non-root\n" +
		"user can run `tailscale serve`/`funnel` -- without it you'll see\n" +
		"\"Access denied\" the first time you press space. Run this once:\n" +
		"  sudo tailscale set --operator=" + you + "\n" +
		"(or run tailport itself with sudo). If it's not set yet, pressing\n" +
		"space shows a persistent on-screen reminder with this exact command;\n" +
		"press r afterward to re-check and clear it."
}

// operatorSetupText binds OperatorSetupText to this model's resolved
// operator username, for helpView.
func (m model) operatorSetupText() string {
	return OperatorSetupText(m.operatorUser)
}

// helpContent builds the FULL "?" overlay text (title, intro, markers, setup,
// the grouped keybinding legend, warnings, and where config saves) as one
// string, with NO viewport clipping and NO close/scroll footer -- helpView
// owns those. It's taller than most terminals; helpView slices it to m.height.
func (m model) helpContent() string {
	var b strings.Builder

	b.WriteString(helpTitleStyle.Render("tailport — expose local ports across your tailnet"))
	b.WriteString("\n\n")
	b.WriteString(helpTextStyle.Render(
		"tailport lists the TCP ports listening on this machine. A served port\n" +
			"is reachable by your other tailnet devices over plain HTTP at\n" +
			"http://<host>:<port> — tailnet-only, a 1:1 port mapping (same port in\n" +
			"and out). `tailscale serve` only matters for a LOOPBACK-bound app: a\n" +
			"wildcard-bound port (0.0.0.0) is already reachable on the tailnet\n" +
			"without serving — see the marker legend and each row's description.\n" +
			"A port can also be exposed to the PUBLIC internet two independent,\n" +
			"mutually-exclusive ways (opt-in, see below): `P` funnels it via\n" +
			"tailscale, and `p` publishes it at a custom hostname through your own\n" +
			"Caddy edge (first publish prompts for caddy.hostname/domain if\n" +
			"unset — see docs/caddy-edge.md)."))
	b.WriteString("\n\n")
	b.WriteString(helpTitleStyle.Render("Markers"))
	b.WriteString("\n")
	b.WriteString(helpTextStyle.Render(m.markerLegend()))
	b.WriteString("\n\n")
	// "Setup / prerequisites" (kata tapv): a localized new section, styled the
	// same way as Markers just above, deliberately kept OUT of
	// RenderKeyLegendGroups/KeyLegendGroups -- that structure is sourced from
	// keyMap.groups() and shared verbatim with the bottom-bar grid and
	// `tailport quickstart`'s legend, and this isn't a keybinding.
	b.WriteString(helpTitleStyle.Render("Setup / prerequisites"))
	b.WriteString("\n")
	b.WriteString(helpTextStyle.Render(m.operatorSetupText()))
	b.WriteString("\n\n")
	// Keys, grouped into the same four sections/order as the bottom-bar grid
	// (Serve Toggles, Favorites, View, App) -- each section's own header stands in for
	// the old flat "Keys" title -- but keeping the rich per-key prose.
	// Laid out side by side when the terminal is wide enough (v10j) to shorten
	// the overlay; falls back to a single column otherwise.
	b.WriteString(m.renderKeyLegendColumns())

	b.WriteString("\n")
	b.WriteString(warnStyle.Render(
		"Toggling port :22 (SSH) asks for a y/n confirmation first, in both\n" +
			"directions — turning serve off for :22 can drop your live SSH session."))
	b.WriteString("\n\n")
	// Dangling-forward glyph, same resolution as keyLegendDescs' inline "C" row
	// glyph, quoted again here since this paragraph sits outside that legend.
	dangling := "▲"
	if m.markerEmoji {
		dangling = "🌫️"
	}
	b.WriteString(helpTextStyle.Render(
		"A port marked " + dangling + " (its row reads \"bound to tailnet, but stale …\")\n" +
			"is a dangling forward: served, but no local process holds it. If your\n" +
			"app won't start with \"address already in use\", it's binding\n" +
			"0.0.0.0:<port>, which collides with tailscale's serve listener on that\n" +
			"port — bind it to 127.0.0.1:<port> instead (what serve proxies to, and\n" +
			"off your LAN). Or unbind it: space on the row, or C to clear all stale\n" +
			"forwards."))
	b.WriteString("\n\n")
	for _, line := range configSaveLines(m.configPath) {
		b.WriteString(helpTextStyle.Render(line) + "\n")
	}

	return b.String()
}

// renderKeyLegendColumns renders the grouped keybinding legend for the "?"
// overlay (v10j). On a wide-enough terminal it splits the groups into two
// side-by-side columns to roughly halve the section's height; on a narrow
// terminal (or before the first WindowSizeMsg) it falls back to the single
// vertical column and lets helpView's scroll handle the height. Content is
// unchanged either way -- same groups, same rich per-key prose -- so the
// shared-with-quickstart legend text never diverges; only the layout differs.
func (m model) renderKeyLegendColumns() string {
	groups := KeyLegendGroups(m.markerEmoji)
	single := RenderKeyLegendGroups(groups)
	if m.width <= 0 || len(groups) < 2 {
		return single
	}
	const gutter = 4
	gap := strings.Repeat(" ", gutter)
	best, bestH := single, lipgloss.Height(single)
	// Try every in-order split point (groups stay in reading order, first half
	// over the left column) and keep the shortest layout that fits the width.
	for k := 1; k < len(groups); k++ {
		left := RenderKeyLegendGroups(groups[:k])
		right := RenderKeyLegendGroups(groups[k:])
		if lipgloss.Width(left)+gutter+lipgloss.Width(right) > m.width {
			continue
		}
		joined := lipgloss.JoinHorizontal(lipgloss.Top, left, gap, right)
		if h := lipgloss.Height(joined); h < bestH {
			best, bestH = joined, h
		}
	}
	return best
}

// helpContentLines splits helpContent into display lines, dropping the trailing
// blank so it doesn't inflate the scroll range by a row.
func (m model) helpContentLines() []string {
	return strings.Split(strings.TrimRight(m.helpContent(), "\n"), "\n")
}

// helpBodyRows is how many rows of the overlay body are visible: the whole
// viewport minus the one persistent footer row. Guards m.height == 0 (pre
// first WindowSizeMsg) to at least one row.
func (m model) helpBodyRows() int {
	if rows := m.height - 1; rows >= 1 {
		return rows
	}
	return 1
}

// helpMaxScroll is the largest valid helpScroll offset: content height minus
// the visible body height, floored at 0 (content shorter than the viewport
// never scrolls).
func (m model) helpMaxScroll() int {
	if max := len(m.helpContentLines()) - m.helpBodyRows(); max > 0 {
		return max
	}
	return 0
}

// helpPageStep is the pgup/pgdn jump: a viewport of body rows less one line of
// overlap for continuity, at least one.
func (m model) helpPageStep() int {
	if step := m.helpBodyRows() - 1; step >= 1 {
		return step
	}
	return 1
}

// helpView is the "?" overlay as actually drawn: helpContent sliced to a
// scrolled window of m.height, with a persistent footer showing the close
// hint and the scroll position. It replaces the whole View while showHelp is
// set. Alt-screen mode clips overflow instead of scrolling natively, so this
// in-app windowing is what makes the (taller-than-terminal) overlay reachable.
func (m model) helpView() string {
	lines := m.helpContentLines()
	bodyRows := m.helpBodyRows()
	max := m.helpMaxScroll()
	off := m.helpScroll
	if off > max {
		off = max
	}
	if off < 0 {
		off = 0
	}
	end := off + bodyRows
	if end > len(lines) {
		end = len(lines)
	}
	visible := lines[off:end]
	// Pad so the footer pins to the last row even when the content (or its
	// tail) is shorter than the viewport -- mirrors View's bottom-bar gap.
	for len(visible) < bodyRows {
		visible = append(visible, "")
	}
	return strings.Join(visible, "\n") + "\n" + m.helpFooter(off, max)
}

// helpFooter is the overlay's always-visible bottom row: the close/scroll hint
// plus a position indicator so the user knows there's more above or below.
func (m model) helpFooter(off, max int) string {
	pos := "all shown"
	switch {
	case max == 0:
		// content fits; leave "all shown"
	case off <= 0:
		pos = "more below ▼"
	case off >= max:
		pos = "▲ more above"
	default:
		pos = "▲ more · more ▼"
	}
	return helpStyle.Render("↑/↓ scroll · ? esc q close   " + pos)
}

// emptyStateMessage explains the current (empty) view: why it's empty and
// what to press to get somewhere. It's the friendly stand-in for the list's
// bare "No items." -- most important on a fresh install, where the default
// Favorites view is empty until the user favorites something.
func (m model) emptyStateMessage() string {
	// Lead with what the tool is and the commands that power it -- an empty
	// view is often a user's first screen, so it should explain tailport
	// before it explains why this particular list is empty.
	//
	// Each line is assembled from single-line styled spans and joined with
	// "\n" OUTSIDE any Render call: passing a multi-line string to Render
	// would block-pad the shorter lines with background spaces.
	k := helpKeyStyle.Render
	t := helpTextStyle.Render

	lines := []string{
		t("tailport exposes your machine's listening TCP ports to your tailnet."),
		t("It discovers them with ") + k("ss") + t(" (Linux) / ") + k("lsof") + t(" (macOS), and turns"),
		t("each one on or off with ") + k("tailscale serve --http=<port>") + t(" -- tailnet-only,"),
		t("plain HTTP, same port in and out. Press ") + k("P") + t(" to funnel one publicly."),
		"",
	}
	if m.showAllPorts {
		lines = append(lines,
			t("Nothing is listening on this machine right now. Start a local"),
			t("server, then press ")+k("r")+t(" to refresh. Press ")+k("?")+t(" for help."),
		)
		return helpTitleStyle.Render("All ports") + "\n\n" + strings.Join(lines, "\n")
	}
	lines = append(lines,
		t("This is your Favorites view, and you haven't favorited anything yet."),
		t("Favorites (")+favStyle.Render("★")+t(") are a shortlist that persists across restarts,"),
		t("even when the process isn't running. Press ")+k("a")+t(" for All ports, then ")+k("f"),
		t("to favorite one. Press ")+k("?")+t(" for help."),
	)
	// No "Favorites" heading here (2fgk): the persistent header already names
	// the view, and the body line above already says "your Favorites view", so
	// a green heading too would just be redundant. The symmetric "All ports"
	// heading is deliberately kept.
	return strings.Join(lines, "\n")
}

// renderEmptyState renders the empty-view explanation in place of the list
// body when the current view has no items.
func (m model) renderEmptyState() string {
	return m.emptyStateMessage()
}

// renderHeader draws the persistent top header: the cyan "tailport" wordmark
// pinned top-left, the build version in muted grey just after it (0qy8), and
// the Favorites|All-ports toggle right-aligned on the same row, spanning the
// terminal width. View() draws it above both the list body and the empty
// state, so the logo never disappears when a view is empty or when switching
// views. Before the first WindowSizeMsg (width == 0) the two fall back to a
// single-space separation.
func (m model) renderHeader() string {
	logo := logoStyle.Render("tailport")
	if v := displayVersion(m.version); v != "" {
		logo += " " + versionStyle.Render(v)
	}
	toggle := m.renderViewIndicator()
	gap := m.width - lipgloss.Width(logo) - lipgloss.Width(toggle)
	if gap < 1 {
		gap = 1 // too narrow to justify; keep at least a space (may wrap)
	}
	return logo + strings.Repeat(" ", gap) + toggle
}

// displayVersion formats a build version for the header. Release builds stamp
// a bare semver ("0.1.4" -- build.yml passes -X main.version=${GITHUB_REF_NAME#v},
// which strips the tag's leading v), and the header wants it tag-shaped, so
// the v goes back on: "v0.1.4". Two values are passed through verbatim
// instead: "" (unknown -- caller draws no version at all) and the default
// "dev" of an unstamped local build, which is not a semver and would read as
// a nonsense "vdev". Pure and total so it can be table-tested without a model.
func displayVersion(v string) string {
	switch {
	case v == "" || v == "dev":
		return v
	case strings.HasPrefix(v, "v"):
		return v // already tag-shaped; don't double the prefix
	default:
		return "v" + v
	}
}

// headerSpacerLines is the blank row View draws between the header and the
// body, so the wordmark isn't crowded against the first port row (0qy8). It's
// a named constant because two places must agree on it: View, which emits it,
// and listBodyHeight, which reserves it -- if the reservation missed it the
// grid would size one row too tall and the bottom bar would be pushed off.
const headerSpacerLines = 1

// renderViewIndicator renders the Favorites | All-ports segmented control,
// with the active view as a filled chip so it's unmistakable which one "a"
// is currently showing.
func (m model) renderViewIndicator() string {
	fav, all := " Favorites ", " All ports "
	if m.showAllPorts {
		return viewInactiveStyle.Render(fav) + viewActiveStyle.Render(all)
	}
	return viewActiveStyle.Render(fav) + viewInactiveStyle.Render(all)
}

// statusText is the human-readable status shown at the bottom: the current
// operation while one is in flight, otherwise a multi-state breakdown of what
// this machine is doing -- how many ports are listening locally, how many
// tailport serves on the tailnet, and how many are funnelled to the public
// internet. The served count reads plainly as "N on tailnet" (79xb: "exposed"
// conflated serve forwards with tailnet reachability -- a wildcard-bound port
// is on the tailnet with or without being served, so the retired word is
// gone, not just qualified); "public" is the funnel count, real since yt69.
// It abbreviates on narrow terminals rather than overflowing the line.
func (m model) statusText() string {
	switch {
	case m.pending != 0:
		return fmt.Sprintf("toggling :%d...", m.pending)
	case m.cleaning != 0:
		return fmt.Sprintf("cleaning %d stale forward(s)...", m.cleaning)
	}

	listening := len(m.allPorts)
	tailnet := 0
	for _, on := range m.active {
		if on {
			tailnet++
		}
	}
	public := len(m.funnel)

	// The public count is m.funnel -- ports made public via `tailscale
	// funnel`, not an independent public bind -- so it's labelled "public
	// (funnel)" to name the mechanism (67zk) and pair with "on tailnet".
	full := fmt.Sprintf("%d listening · %d on tailnet · %d public (funnel)", listening, tailnet, public)
	// The host rides the "listening" segment ("N listening on <host>") rather
	// than a trailing "— <host>" (20w6); the other segments are unchanged.
	withHost := full
	if m.host != "" {
		withHost = fmt.Sprintf("%d listening on %s · %d on tailnet · %d public (funnel)", listening, m.host, tailnet, public)
	}
	// Persistent quiet-degrade fragment (kata v1z5 step 5): only when publishing
	// is configured (a blank caddy.domain suppresses the poll entirely) and the
	// last edge poll failed. Never a toast -- a small trailing status fragment.
	// It MUST be sized into the width fit below (roborev 0k12 #4): ordinary
	// status text isn't wrapped (unlike toasts), so a suffix appended after the
	// variant is chosen would overflow m.width and get truncated -- silently
	// dropping the health warning on narrow terminals. On a very narrow line we
	// shorten "edge unreachable" to "edge down" before sacrificing any of it.
	suffix := ""
	if m.cfg.Caddy.Domain != "" && !m.publishReachable {
		suffix = " · edge unreachable"
		initials := fmt.Sprintf("%dL · %dT · %dP", listening, tailnet, public)
		if m.width > 0 && lipgloss.Width(initials)+lipgloss.Width(suffix) > m.width {
			suffix = " · edge down"
		}
	}
	// avail is the width the base variant must fit within, after reserving room
	// for the (possibly shortened) suffix.
	avail := m.width
	if suffix != "" && m.width > 0 {
		avail -= lipgloss.Width(suffix)
	}
	// Widest form that fits, degrading host -> shorter labels -> initials. A
	// width of 0 means "unknown" (pre-first-resize): fall back to the fullest
	// form, same as before the suffix accounting. A positive width whose room
	// the suffix has entirely consumed (avail <= 0) instead falls through to the
	// most compact variant rather than overflowing with the longest.
	var base string
	switch {
	case m.width <= 0 || lipgloss.Width(withHost) <= avail:
		base = withHost
	case lipgloss.Width(full) <= avail:
		base = full
	default:
		// Medium drops the host and shortens labels; "funnel" stands in for
		// "public (funnel)" for compactness while staying precise.
		medium := fmt.Sprintf("%d listening · %d tailnet · %d funnel", listening, tailnet, public)
		if lipgloss.Width(medium) <= avail {
			base = medium
		} else {
			base = fmt.Sprintf("%dL · %dT · %dP", listening, tailnet, public)
		}
	}
	// Even the most compact base plus the (possibly already-shortened) suffix
	// can still overflow (roborev bps9 #2): the compact fallback above is
	// chosen unconditionally when nothing else fits, without itself being
	// checked against avail, so on terminals under ~24 columns -- or with
	// multi-digit port counts widening the compact form -- base+suffix can
	// exceed m.width. The health warning is the one thing that must survive
	// at any width the terminal actually gives us, so when the combined form
	// still doesn't fit, drop the base entirely and show the warning alone
	// rather than let it be clipped from the end.
	if suffix != "" && m.width > 0 && lipgloss.Width(base)+lipgloss.Width(suffix) > m.width {
		warning := strings.TrimPrefix(suffix, " · ")
		if lipgloss.Width(warning) > m.width {
			// Even the bare warning doesn't fit an extreme width; keep as
			// much of it (from the left) as fits rather than show nothing.
			runes := []rune(warning)
			if m.width < len(runes) {
				runes = runes[:m.width]
			}
			warning = string(runes)
		}
		return warning
	}
	return base + suffix
}

// operatorHintText returns the STICKY banner guiding the user through
// tailscale's operator requirement (kata tapv), or "" when the hint isn't
// active (see m.operatorNotSet). The fix command has $USER EXPANDED --
// m.operatorUser, resolved once at New() via tsserve.CurrentUsername -- so
// it's directly copy-pasteable, no manual substitution needed. Falls back
// to a "<you>" placeholder in the unlikely case the OS username couldn't be
// determined at all, so the line still reads sensibly.
func (m model) operatorHintText() string {
	if !m.operatorNotSet {
		return ""
	}
	return m.operatorHintTextRaw()
}

// operatorHintTextRaw builds the operator-banner TEXT unconditionally, ignoring
// the m.operatorNotSet active flag. operatorHintText gates on that flag for the
// RENDER path; bannerReservationLines calls this instead so it can measure the
// banner's worst-case wrapped height even while the banner is currently off (it
// can appear async with no intervening WindowSizeMsg). The two must build the
// SAME text or the reservation would measure a different string than the render.
func (m model) operatorHintTextRaw() string {
	you := m.operatorUser
	if you == "" {
		you = "<you>"
	}
	return fmt.Sprintf("⚠ tailscale operator not set — run once: sudo tailscale set --operator=%s  (then press r)  — or run tailport with sudo", you)
}

// domainSetupHintText returns the STICKY setup-reminder banner raised after the
// `p` flow (swapped from `P` under vzj4) captures a blank caddy.domain inline
// (kata w131, ycv1 r3-NEW-1), or
// "" when it isn't active (see m.domainSetupPending). It follows
// operatorHintText's PATTERN -- sticky, single line, warnStyle at the render
// site -- but is a SEPARATE, parallel slot: filling the config field is not the
// same as doing the edge setup, so this reminds the user of the standing actions
// the assist can't do for them (the *.<domain> wildcard DNS + a deployed edge).
// Both banners can be active at once, which is why this is a distinct field/func
// rather than a reuse of operatorNotSet.
func (m model) domainSetupHintText() string {
	if !m.domainSetupPending {
		return ""
	}
	return m.domainSetupTextRaw()
}

// domainSetupTextRaw builds the domain-setup-banner TEXT unconditionally,
// ignoring the m.domainSetupPending active flag (mirrors operatorHintTextRaw).
// bannerReservationLines uses this to measure the banner's worst-case wrapped
// height while it's currently off. It reads the CURRENT m.cfg.Caddy.Domain, so
// the reservation tracks the same (possibly long) domain the render will show
// -- listBodyHeight is recomputed live per render (via gridDims), so a domain
// captured without a fresh WindowSizeMsg is still measured against the truth.
func (m model) domainSetupTextRaw() string {
	return fmt.Sprintf("⚠ domain saved — you still need *.%s DNS pointed at your edge, and the edge deployed (see docs/caddy-edge.md)", m.cfg.Caddy.Domain)
}

// listBodyHeight computes the vertical space available for the port grid/
// list body: the current terminal height minus every reservation the bottom
// bar (and now the grid's own page indicator) makes below it. Factored out
// of resizeList (9gys) so gridDims -- and thus renderGrid's row count and
// the Left/Right grid-nav jump distance -- shares the EXACT same height
// accounting resizeList feeds to m.list.SetSize; the two must never drift
// apart or the grid would compute a different row count than the space
// list.Model itself was actually given.
// renderBanner wraps a sticky-banner line to m.width with warnStyle, exactly as
// renderStatusLine wraps its toast: lipgloss.Style.Width word-wraps content that
// exceeds the width instead of bubbletea hard-truncating it (no ellipsis), so
// the actionable tail of a long banner (e.g. "the edge deployed (see
// docs/caddy-edge.md)") stays visible on an ~80-col terminal rather than being
// clipped off. Returns "" for an empty hint so the render's active-flag gating
// is preserved, and guards m.width <= 0 (pre-first-resize) so we never set a
// zero/negative Width. This is the SINGLE wrapping used by BOTH the render
// (renderBottom) and the reservation (bannerReservationLines) so the two can
// never disagree on a banner's wrapped height -- the invariant
// TestBannerReservationDominatesBothLive pins.
func (m model) renderBanner(hint string) string {
	if hint == "" {
		return ""
	}
	if m.width <= 0 {
		return warnStyle.Render(hint)
	}
	return warnStyle.Width(m.width).Render(hint)
}

// bannerReservationLines is the total height the two orthogonal sticky setup
// banners can ever occupy below the list at the current width, WORST-CASED as if
// BOTH are live -- mirroring legendReservationLines' unconditional reservation,
// NOT gated on the current operatorNotSet/domainSetupPending. Either banner can
// appear asynchronously (the operator hint from a failed toggle's toggleDoneMsg
// or the startup detectOperatorMsg; the domain reminder from the P flow
// capturing a blank caddy.domain) with no fresh WindowSizeMsg in between, so
// sizing must already assume both. Each banner's would-be TEXT is built
// regardless of its active flag (the *Raw builders) and wrapped through the SAME
// renderBanner the render uses, so a banner that wraps to more than one line at
// a narrow width is fully reserved and the reservation can never fall short of
// the live wrapped height. Floor is 2 (each raw line is non-empty, so each wraps
// to at least one row), matching the pre-w131 constant.
func (m model) bannerReservationLines() int {
	return lipgloss.Height(m.renderBanner(m.operatorHintTextRaw())) +
		lipgloss.Height(m.renderBanner(m.domainSetupTextRaw()))
}

// legendReservationLines is the number of rows the bottom-bar legend can ever
// occupy at the current width: the MAX of the two cleanEnabled renders (with
// and without the contextual "C clean stale" hint). listBodyHeight reserves
// this so a dangling forward appearing or vanishing between resizes can never
// leave the live legend taller than what was reserved -- the invariant
// TestLegendReservationDominatesLive pins. Reserving the max (rather than
// assuming cleanEnabled=true is worst-case) is robust to 04rb's non-monotonic
// fold, where one more binding can shrink a group.
func (m model) legendReservationLines() int {
	lines := 1
	for _, cleanEnabled := range []bool{true, false} {
		if legend := m.renderLegendWith(cleanEnabled); legend != "" {
			if n := strings.Count(legend, "\n") + 1; n > lines {
				lines = n
			}
		}
	}
	return lines
}

func (m model) listBodyHeight() int {
	if m.width <= 0 || m.height <= 0 {
		return 1
	}
	// Reserve the WORST-CASE legend height so the list never overlaps the bar
	// when a dangling forward appears (or vanishes) between resizes. Because
	// 04rb's fold is width-driven and keyed off each group's UNFOLDED binding
	// count, adding/removing the contextual "C clean stale" hint can make the
	// bar TALLER *or* shorter at a given width (folding is non-monotonic: an
	// extra binding can push a group over a fold threshold and shrink it). So we
	// reserve the max of both cleanEnabled states rather than assuming
	// cleanEnabled=true dominates -- see legendReservationLines and
	// TestLegendReservationDominatesLive.
	legendLines := m.legendReservationLines()
	// Reserve the WORST-CASE sticky-banner height too, unconditionally -- like
	// cleanEnabled=true above, NOT gated on the CURRENT banner state. There are
	// now TWO independent sticky banners that can each appear asynchronously with
	// no fresh WindowSizeMsg in between: the operator-hint banner (kata tapv -- a
	// failed toggle's toggleDoneMsg, or the startup detectOperatorMsg) and the
	// domain-setup reminder (kata w131 -- raised when the P flow captures a blank
	// caddy.domain). They are ORTHOGONAL and can be on at the SAME time, and View
	// stacks both above the status line, so sizing here must assume the worst
	// case of both live or a later appearance would clip the list. Each banner is
	// now WRAPPED to m.width at the render site (renderBanner) instead of being
	// hard-truncated, so a long line (the domain reminder easily runs past 80
	// cols) can span more than one row -- the reservation MEASURES that wrapped
	// worst case (bannerReservationLines) through the SAME renderBanner the render
	// uses, rather than assuming a flat one-line-each constant, so a wrapped
	// banner can never clip the list. Floor is 2 (both lines non-empty).
	bannerLines := m.bannerReservationLines()
	// Reserve the persistent top header (one row) plus the bottom bar: one
	// blank separator, the status line (now measured live -- see below,
	// rather than a flat 1, since a wrapped flash toast can span multiple
	// rows), and the grouped shortcuts legend. View then pads the gap so the
	// bar lands on the last rows (see renderHeader / renderBottom).
	headerLines := lipgloss.Height(m.renderHeader()) + headerSpacerLines
	// statusLines is the CURRENT height of renderStatusLine -- >=1, and >1
	// while a long flash toast is wrapped across lines (83wv pt2). Unlike the
	// legend/banner reservations above, this one is NOT worst-cased to some
	// fixed maximum: it tracks the live flash text exactly, and every m.flash
	// mutation site calls resizeList so the reservation is always current by
	// the next render.
	statusLines := lipgloss.Height(m.renderStatusLine())
	// pageIndicatorLines reserves a constant 1 line for renderGrid's compact
	// "page N/M" line (blank when the grid is single-page), so the body
	// height -- and thus gridDims' row count -- never shifts as pagination
	// appears or disappears between resizes (9gys; mirrors the bannerLines
	// worst-casing above).
	const pageIndicatorLines = 1
	h := m.height - headerLines - legendLines - bannerLines - 1 - statusLines - pageIndicatorLines
	if h < 1 {
		h = 1
	}
	return h
}

// resizeList recomputes the bubbles/list viewport height (listBodyHeight)
// and applies it via list.SetSize -- still needed even though the grid body
// is now rendered by renderGrid rather than list.View(), because list.Model
// uses m.width/m.height to size its own internal state: FilterInput.Width
// (renderFilterRow renders it directly) and the keybinding/pagination
// enablement m.list.Update relies on for Up/Down/PgUp/PgDn/Home/End (9gys;
// see gridDims' doc comment -- those keys stay correct under the grid
// because Index()/Select() are self-consistent regardless of the list's own
// internal PerPage, not because this height happens to equal the grid's
// page size). It's called from the WindowSizeMsg handler (m.width/m.height
// just changed) and again from every site that mutates m.flash (set or
// clear), because a wrapped multi-line toast changes renderStatusLine's
// height without any resize -- so the list's reserved height has to track it
// live, shrinking while a wrapped toast shows and growing back the moment it
// clears, or the two would either overlap or leave a stale gap. A no-op
// before the first WindowSizeMsg (m.width/height still zero).
func (m *model) resizeList() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	m.list.SetSize(m.width, m.listBodyHeight())
}

// renderStatusLine renders the bottom bar's status portion: a transient toast
// (copy confirmation, refusal, or error) when one is showing, else the plain
// statusText(). The severity colours the toast: info green, warn amber, error
// red (q89g).
//
// The toast is WRAPPED to m.width rather than left as a single line, because
// bubbletea's renderer hard-truncates any line that exceeds the terminal
// width with no ellipsis -- and the honest, actionable guard-toast strings
// (83wv pt2) can run well past 80 columns. lipgloss.Style.Width word-wraps
// content that exceeds the set width instead of clipping it, so setting it
// here makes the full message visible on any terminal, at the cost of the
// toast (and the bar) growing to multiple lines -- resizeList reserves for
// that. None of activeStyle/warnStyle/errStyle/helpStyle set a Background, so
// the padding Width() adds to fill short wrapped lines is invisible (no
// visible block behind the text).
//
// Before the first WindowSizeMsg, m.width is 0; Width(0) would collapse the
// render to nothing, so that case falls back to the unwrapped render (still
// only reachable pre-resize, when there's no real terminal width to wrap to
// anyway).
func (m model) renderStatusLine() string {
	var flashRender string
	if m.flash != "" {
		toast := activeStyle
		switch m.flashLevel {
		case flashWarn:
			toast = warnStyle
		case flashError:
			toast = errStyle
		}
		if m.width <= 0 {
			flashRender = toast.Render(m.flash)
		} else {
			flashRender = toast.Width(m.width).Render(m.flash)
		}
	}
	// Precedence when both a flash and a poof want the slot (kata dw57; corrected
	// per roborev 65qc-#1): a WARN/ERROR flash ALWAYS wins -- the user must see a
	// failure/refusal even mid-poof (e.g. a take-over that failed after the
	// delete). But the poof OUTRANKS an INFO flash: the resumed take-over's "took
	// over <host>" toast (flashInfo) commonly lands within the poof's ~500ms life,
	// and the original flash-always-wins rule hid nearly the whole animation.
	// Now the poof plays out and the info flash -- still within its own, longer
	// ttl -- shows the moment the poof clears. renderPoofLine is width-bounded
	// (never wraps), so either branch self-reserves through the same live
	// statusLines measurement.
	if flashRender != "" && m.flashLevel != flashInfo {
		return flashRender
	}
	if m.poof != nil {
		return renderPoofLine(*m.poof, m.width)
	}
	if flashRender != "" {
		return flashRender
	}
	// The restore affordance (kata ttfh; design §3.5/§3.6) shares this SAME
	// status-slot action line the poof used: once the poof and any "took over"
	// info flash have cleared, the armed slot shows the deleted route's descriptor
	// and the restore key for the affordance's lifetime. It sits BELOW a flash and
	// the poof (so a failure or the take-over toast is never hidden) and ABOVE the
	// plain status text. While a restore is in flight it reads "restoring …".
	if m.lastPurge != nil {
		lp := m.lastPurge
		if m.restoring {
			return helpStyle.Render(fmt.Sprintf("restoring %s…", lp.hostname))
		}
		desc := lp.hostname
		if lp.deletedDesc != "" {
			desc = fmt.Sprintf("%s → %s", lp.hostname, lp.deletedDesc)
		}
		return helpStyle.Render("purged "+desc+" — press ") + helpKeyStyle.Render("R") + helpStyle.Render(" to restore")
	}
	return helpStyle.Render(m.statusText())
}

// promptLine composes a modal entry prompt so it fits the viewport width (kata
// 78p6). On a normal-width terminal the one-line form -- styled label + the
// active input field + a trailing key-hint -- renders inline, exactly as
// before. When that one line would exceed m.width (a narrow terminal), the label
// WRAPS to width on its own row(s) and the field drops to the next row, keeping
// the hint only if that field row still fits; the input field is never truncated
// (you're typing into it). Everything is derived from m.width at render time, so
// a resize just re-renders correctly -- View runs after every WindowSizeMsg --
// with no stored width to keep in sync. label and hint are RAW text (styled here
// with helpStyle); field is the already-styled input content.
func (m model) promptLine(label, field, hint string) string {
	oneLine := helpStyle.Render(label) + field + helpStyle.Render(hint)
	if m.width <= 0 || lipgloss.Width(oneLine) <= m.width {
		return oneLine
	}
	fieldRow := field
	if lipgloss.Width(field)+lipgloss.Width(hint) <= m.width {
		fieldRow += helpStyle.Render(hint)
	}
	// helpStyle.Width wraps the (foreground-only) label to the viewport; the
	// field, which must stay whole, sits on its own row below.
	return helpStyle.Width(m.width).Render(label) + "\n" + fieldRow
}

// fitField renders a modal input for a prompt row, bounding it to the viewport
// so a value (or the publish host step's locked ".<domain>" suffix) longer than
// the terminal scrolls INSIDE the field rather than overflowing the line (kata
// 78p6 / roborev a05w). suffix is plain text appended after the input. The
// input's render width is only ever REDUCED to fit -- never grown -- so a
// normal, wide terminal renders exactly as before. An immutable suffix wider
// than the whole viewport (a domain longer than the terminal) is the one thing
// that can't be shrunk -- truncating it would hide which domain you're
// publishing under -- so it sets the practical floor.
func (m model) fitField(in textinput.Model, suffix string) string {
	if m.width > 0 {
		// Width bounds the VALUE cells; the textinput also renders its prompt
		// glyph and a trailing block-cursor cell on top, so leave room for both.
		avail := m.width - lipgloss.Width(suffix) - lipgloss.Width(in.Prompt) - 1
		if avail < 1 {
			avail = 1
		}
		bounded := false
		switch {
		case in.Width == 0:
			// Content-sized: only bound it when content + suffix would overflow;
			// then a fixed width makes the textinput scroll to keep the cursor
			// visible.
			if lipgloss.Width(in.View())+lipgloss.Width(suffix) > m.width {
				in.Width = avail
				bounded = true
			}
		case in.Width > avail:
			in.Width = avail
			bounded = true
		}
		if bounded {
			// textinput computes its horizontal scroll window when the value or
			// cursor moves, not when Width is set after the fact -- nudge the
			// cursor so the new width actually takes effect and the tail (where
			// the cursor and the locked suffix live) stays visible.
			in.CursorEnd()
		}
	}
	return in.View() + suffix
}

// renderBottom builds the bottom bar. In a modal entry mode it's the prompt
// for that flow; otherwise it's the status line, with the shortcuts legend on
// the last row(s). The Favorites|All-ports toggle lives in the top header
// (renderHeader), not here. View pins whatever this returns to the bottom of
// the viewport.
func (m model) renderBottom() string {
	switch m.mode {
	case entryAddPort:
		return m.promptLine("add port to favorites: ", m.fitField(m.portInput, ""), "  (enter: confirm, esc: cancel)")
	case entryLabel:
		return m.promptLine(fmt.Sprintf("label :%d: ", m.labelPort), m.fitField(m.labelInput, ""), "  (enter: confirm, esc: cancel)")
	case entryConfirmClean:
		targets := make([]string, len(m.cleanTargets))
		for i, p := range m.cleanTargets {
			targets[i] = strconv.Itoa(p)
		}
		return helpStyle.Render(fmt.Sprintf("tear down forwards on :%s? ", strings.Join(targets, ", :"))) + helpStyle.Render("(y: confirm, any other key: cancel)")
	case entryConfirm22:
		action := "expose :22 (SSH) via tailscale serve"
		if !m.confirmTurnOn {
			action = "stop serving :22 (SSH) -- this can drop your live session"
		}
		return warnStyle.Render(action+"? ") + helpStyle.Render("(y: confirm, any other key: cancel)")
	case entryConfirmUnlockSSH:
		return warnStyle.Render("⚠ this can break SSH access to this machine — ") +
			helpStyle.Render("type ") + helpKeyStyle.Render("ssh") + helpStyle.Render(" to confirm unlocking :22: ") +
			m.sshInput.View() + helpStyle.Render("  (esc: cancel)")
	case entryConfirmFunnel:
		url := tsserve.PublicURL(m.fqdn, m.funnelPublic)
		lines := []string{
			warnStyle.Render(fmt.Sprintf("⚠ Expose :%d to the PUBLIC INTERNET via funnel?", m.funnelPort)),
			helpStyle.Render("   → ") + publicStyle.Render(url) + helpStyle.Render("   (reachable by anyone on the internet)"),
		}
		// Funnel proxies HTTP(S); warn when the target may not speak it.
		if proc := m.selectedProcess(m.funnelPort); !looksHTTP(m.funnelPort, proc) {
			lines = append(lines, warnStyle.Render(fmt.Sprintf("   note: funnel only proxies HTTP; :%d may not speak it", m.funnelPort)))
		}
		lines = append(lines, helpStyle.Render("   (y: confirm, any other key: cancel)"))
		return strings.Join(lines, "\n")
	case entryPublishHostname:
		return m.promptLine(fmt.Sprintf("publish :%d — Caddy edge's tailnet hostname (short MagicDNS label, default \"caddy\"): ", m.publishPort),
			m.fitField(m.publishInput, ""), "  (enter: save & next, esc: cancel)")
	case entryPublishDomain:
		return m.promptLine(fmt.Sprintf("publish :%d — set your public base domain: ", m.publishPort),
			m.fitField(m.publishInput, ""), "  (enter: save & next, esc: cancel)")
	case entryPublishHost:
		// The input holds only the editable label; render the locked ".<domain>"
		// suffix contiguously after it (plain, same style as typed text) so it
		// reads as one field with the cursor before the first dot -- the suffix
		// can't be deleted because it isn't in the buffer.
		return m.promptLine(fmt.Sprintf("publish :%d — public hostname: ", m.publishPort),
			m.fitField(m.publishInput, "."+m.cfg.Caddy.Domain), "  (enter: next, esc: cancel)")
	case entryPublishAuth:
		return helpStyle.Render(fmt.Sprintf("protect :%d behind basic auth at the edge? ", m.publishPort)) +
			helpStyle.Render("(y: yes / n: no auth / esc: cancel)")
	case entryPublishCredUser:
		return m.promptLine("basic-auth username: ", m.fitField(m.publishInput, ""), "  (enter: next, esc: cancel)")
	case entryPublishCredPass:
		return m.promptLine("basic-auth password: ", m.fitField(m.publishInput, ""), "  (enter: confirm, esc: cancel)")
	case entryConfirmPublish:
		url := "https://" + m.publishHostname
		lines := []string{
			warnStyle.Render(fmt.Sprintf("⚠ Publish :%d to the PUBLIC INTERNET via the caddy edge?", m.publishPort)),
			helpStyle.Render("   → ") + publicStyle.Render(url) + helpStyle.Render("   (reachable by anyone on the internet)"),
		}
		if m.publishWithAuth {
			lines = append(lines, helpStyle.Render("   protected by basic auth at the edge"))
		} else {
			lines = append(lines, warnStyle.Render("   no basic auth — anyone with the URL can reach it"))
		}
		if m.publishEnableServe {
			lines = append(lines, helpStyle.Render(fmt.Sprintf("   will also turn tailscale serve on for :%d", m.publishPort)))
		}
		lines = append(lines, helpStyle.Render("   (y: confirm, any other key: cancel)"))
		return strings.Join(lines, "\n")
	case entryConfirmPurgeOwned:
		// Normal y/n force-purge of an owned route. Name the backend; for a
		// cross-machine owned route (backend label != this machine's short label),
		// name that it belongs to another machine so a take-over is never invisible.
		desc := backendDesc(m.purgeInfo)
		var head string
		if m.purgeInfo.BackendParseable && m.purgeInfo.Label != shortLabel(m.fqdn) {
			head = fmt.Sprintf("⚠ Take over %s from ANOTHER machine (%s)?", m.purgeHostname, desc)
		} else {
			head = fmt.Sprintf("⚠ Take over %s from your %s?", m.purgeHostname, desc)
		}
		lines := []string{
			warnStyle.Render(head),
			helpStyle.Render("   force-purges that edge route and republishes it to this port"),
			helpStyle.Render("   (y: confirm, any other key: cancel)"),
		}
		return strings.Join(lines, "\n")
	case entryConfirmPurgeForeign:
		// Scary first gate: a y/n drift warning naming that the route wasn't created
		// by tailport (AGENTS.md treats foreign routes as drift, never silently
		// overridden).
		lines := []string{
			warnStyle.Render(fmt.Sprintf("⚠ %s is held by a route tailport did NOT create (%s).", m.purgeHostname, backendDesc(m.purgeInfo))),
			warnStyle.Render("   Force-purging deletes a route you didn't make through tailport — this is drift."),
		}
		// Disclose the blast radius: every other host pattern this route carries
		// (roborev hped #3), so a wildcard/multi-host route isn't purged blind.
		lines = append(lines, m.purgeBlastRadiusLines()...)
		lines = append(lines, helpStyle.Render("   (y: continue to the typed confirm, any other key: cancel)"))
		return strings.Join(lines, "\n")
	case entryConfirmPurgeForeignType:
		// Typed-word commit (mirror entryConfirmUnlockSSH's shape). The blast-radius
		// disclosure (roborev hped #3) rides above the input line so it stays visible
		// through the final commit gate.
		inputLine := warnStyle.Render(fmt.Sprintf("⚠ permanently delete the foreign route holding %s — ", m.purgeHostname)) +
			helpStyle.Render("type ") + helpKeyStyle.Render("purge") + helpStyle.Render(" to confirm: ") +
			m.purgeInput.View() + helpStyle.Render("  (esc: cancel)")
		if radius := m.purgeBlastRadiusLines(); len(radius) > 0 {
			return strings.Join(radius, "\n") + "\n" + inputLine
		}
		return inputLine
	}
	bar := m.renderStatusLine()
	// The sticky banners, when active, sit ABOVE the status line -- unlike the
	// transient toast they never auto-dismiss, so they stay put through
	// refreshes and keypresses that would otherwise clear m.flash. There are TWO
	// independent ones (kata w131): the operator hint (kata tapv) and the
	// domain-setup reminder; both are orthogonal and can show at once, so both
	// are rendered here, stacked. Each is styled via warnStyle (a NAMED style,
	// not a hardcoded color) so it stays legible under any future light/dark
	// AdaptiveColor conversion of that style, and listBodyHeight's bannerLines
	// reserves the worst case of both being live. Each is WRAPPED to m.width via
	// renderBanner (the SAME helper the reservation measures with, mirroring how
	// renderStatusLine wraps its toast) so a long banner word-wraps instead of
	// being hard-truncated with its actionable tail lost -- and never introduces
	// a horizontal scroll.
	if banner := m.renderBanner(m.domainSetupHintText()); banner != "" {
		bar = banner + "\n" + bar
	}
	if banner := m.renderBanner(m.operatorHintText()); banner != "" {
		bar = banner + "\n" + bar
	}
	if legend := m.renderLegend(); legend != "" {
		bar += "\n" + legend
	}
	return bar
}

// renderGrid lays out the current page of the port list in gridDims' cols
// side-by-side columns (1 on an ordinary terminal, up to maxCols on a wide
// one), taking over LAYOUT and paging from bubbles/list while list.Model
// stays the single source of truth for STATE -- Items/VisibleItems, Index,
// Select, FilterState (9gys). It pages off m.list.Index() directly rather
// than the list's own internal paginator (which bubbles/list sizes for a
// single column and which this code never touches): perPage = cols*rows,
// and the window is VisibleItems()[page*perPage : ...].
//
// Each cell in the window is rendered by REUSING m.delegate -- the exact
// same delegate list.New(nil, del, ...) was built with -- on a scratch copy
// of m.list (sl) whose width is narrowed to the column width. m is already a
// value copy (View/renderGrid have value receivers), so mutating sl is safe
// and never leaks back to the real m.list. Because sl.Index() == m.list.Index()
// == sel unchanged, the delegate still selects/highlights the right cell,
// and dimmed/ANSI styling is byte-identical to the old single-column
// list.View() path -- only the surrounding layout differs.
//
// Items fill COLUMN-MAJOR (gridPlacement): the k-th window item lands at
// column k/rows, row k%rows, so a column reads top-to-bottom before
// wrapping to the next, like a newspaper.
func (m model) renderGrid() string {
	cols, rows, colWidth := m.gridDims()
	items := m.list.VisibleItems()
	perPage := cols * rows
	if perPage < 1 {
		perPage = 1
	}
	sel := m.list.Index()
	page := sel / perPage
	start := page * perPage
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	window := items[start:end]

	sl := m.list
	sl.SetWidth(colWidth)
	spacing := m.delegate.Spacing()

	columns := make([][]string, cols)
	for k, it := range window {
		var buf bytes.Buffer
		m.delegate.Render(&buf, sl, start+k, it)
		c, _ := gridPlacement(k, rows)
		columns[c] = append(columns[c], buf.String())
	}

	// Join each column's cells vertically with `spacing` blank line(s)
	// between them (matching the delegate's own inter-item gap), then pad
	// every column to the tallest one's line count so JoinHorizontal aligns
	// them on a common baseline, with a colGutter-wide blank gutter between
	// columns.
	colStrs := make([]string, cols)
	tallest := 0
	for c := 0; c < cols; c++ {
		colStrs[c] = strings.Join(columns[c], strings.Repeat("\n", spacing+1))
		if hgt := lipgloss.Height(colStrs[c]); hgt > tallest {
			tallest = hgt
		}
	}
	gutter := lipgloss.NewStyle().Width(colGutter).Height(tallest).Render("")
	parts := make([]string, 0, cols*2-1)
	for c := 0; c < cols; c++ {
		block := colStrs[c]
		if pad := tallest - lipgloss.Height(block); pad > 0 {
			block += strings.Repeat("\n", pad)
		}
		parts = append(parts, lipgloss.NewStyle().Width(colWidth).Render(block))
		if c < cols-1 {
			parts = append(parts, gutter)
		}
	}
	grid := lipgloss.JoinHorizontal(lipgloss.Top, parts...)

	// A compact "page N/M" indicator when there's more than one page, so the
	// more-below/next-page affordance bubbles/list's own paginator used to
	// give isn't lost. Always emitted as exactly one line -- blank when
	// there's only one page -- so listBodyHeight's pageIndicatorLines
	// reservation never has to change between single- and multi-page states.
	indicator := ""
	if len(items) > perPage {
		totalPages := (len(items) + perPage - 1) / perPage
		indicator = helpStyle.Render(fmt.Sprintf("page %d/%d", page+1, totalPages))
	}
	return grid + "\n" + indicator
}

func (m model) View() string {
	if m.showEgg {
		return m.eggView()
	}
	if m.showHelp {
		return m.helpView()
	}

	// Persistent top header (logo + view toggle), drawn above both the list
	// and the empty state so the wordmark never disappears. While a filter is
	// active its input gets a dedicated row just beneath the header (4ye6),
	// rather than stacking in bubbles/list's title area.
	header := m.renderHeader()
	filtering := m.list.FilterState() != list.Unfiltered
	if filtering {
		header += "\n" + m.renderFilterRow()
	}

	// Body selection:
	//   - filtering with no matches -> a clear "no ports match" line (not
	//     bubbles/list's bare "No items." nor the fresh-install explainer);
	//   - the current view genuinely empty (no favorites / nothing listening)
	//     and not filtering -> the contextual empty-state explainer;
	//   - otherwise the list itself.
	var body string
	switch {
	case filtering && len(m.list.VisibleItems()) == 0:
		body += m.noMatchMessage()
	case len(m.list.Items()) == 0 && !filtering:
		body += m.renderEmptyState()
	default:
		body += m.renderGrid()
	}

	// Pin the bottom bar (shortcuts, status -- or a modal prompt) to the last
	// rows of the viewport by padding the gap. Before the first WindowSizeMsg
	// (m.height == 0) this falls back to a single blank separator line.
	bottom := m.renderBottom()
	gap := m.height - lipgloss.Height(header) - headerSpacerLines - lipgloss.Height(body) - lipgloss.Height(bottom)
	if gap < 1 {
		gap = 1
	}
	return header + strings.Repeat("\n", 1+headerSpacerLines) + body + strings.Repeat("\n", gap) + bottom
}

// renderFilterRow renders the on-demand filter input shown beneath the header
// while filtering. The input is bubbles/list's own FilterInput (built-in
// display suppressed via SetShowFilter(false)); we surface it here so the
// prompt sits in its own row and gives immediate feedback the moment "/" is
// pressed.
func (m model) renderFilterRow() string {
	return m.list.FilterInput.View()
}

// noMatchMessage is shown in place of the list body when an active filter
// matches nothing, naming the query so it's clear why the list is empty.
func (m model) noMatchMessage() string {
	q := strings.TrimSpace(m.list.FilterValue())
	msg := helpTitleStyle.Render("No matches")
	if q != "" {
		msg = helpTitleStyle.Render(fmt.Sprintf("No ports match %q", q))
	}
	return msg + "\n\n" + helpTextStyle.Render("Try a different query, or press ") +
		helpKeyStyle.Render("esc") + helpTextStyle.Render(" to clear the filter.")
}
