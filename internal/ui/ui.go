// Package ui implements tailport's Bubble Tea TUI: a list of locally
// listening ports, toggled on/off tailnet-wide via tailscale serve.
package ui

import (
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
	Toggle     key.Binding
	Funnel     key.Binding
	Filter     key.Binding
	NewPort    key.Binding
	Label      key.Binding
	Favorite   key.Binding
	Unfavorite key.Binding
	Lock       key.Binding
	ShowAll    key.Binding
	Copy       key.Binding
	Clean      key.Binding
	Refresh    key.Binding
	Help       key.Binding
	Quit       key.Binding
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

// groups returns the approved like-for-like grouping: Expose, Favorites, View,
// App -- in display order, one group per bottom-bar column and one "?"-overlay
// section. (p39s introduced this grouping with a separate Protect column;
// folded into Expose here -- lock/unlock and the contextual clean-stale are
// exposure guards, so they live at the end of Expose with x lock/unlock always
// the last item. Clean is contextual: barGroups drops it unless a dangling
// forward exists, so ordering it before Lock keeps Lock last in every state.
// Copy moved from Expose to sit under "n add favorite" in Favorites.)
func (k keyMap) groups() []keyGroup {
	return []keyGroup{
		{"Expose", []key.Binding{k.Toggle, k.Funnel, k.Clean, k.Lock}},
		{"Favorites", []key.Binding{k.Favorite, k.Unfavorite, k.NewPort, k.Copy, k.Label}},
		{"View", []key.Binding{k.Filter, k.ShowAll, k.Refresh}},
		{"App", []key.Binding{k.Help, k.Quit}},
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
		// "serve" (not "toggle") in the bar's Expose column: it names the action
		// space performs, matching the approved p39s grouping table. The "?"
		// overlay keeps the fuller "toggle serve on/off" prose (keyLegendDescs).
		Toggle: key.NewBinding(key.WithKeys(" "), key.WithHelp("space", "serve")),
		Funnel: key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "funnel public")),
		// Filter is display-only (legend + help): the actual "/" handling lives
		// in bubbles/list. Listed here so the feature is discoverable.
		Filter:     key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
		NewPort:    key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "add favorite")),
		Label:      key.NewBinding(key.WithKeys("l"), key.WithHelp("l", "label")),
		Favorite:   key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "favorite")),
		Unfavorite: key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "unfavorite")),
		Lock:       key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "lock/unlock")),
		ShowAll:    key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "filtered")),
		Copy:       key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy URL")),
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
	// dimmed de-emphasises this row: set on non-favorite ports pulled into the
	// Favorites view by an active "/" filter (4ye6), so real favorites still
	// stand out among the wider search results. See portDelegate.Render.
	dimmed bool
	meta   config.PortMeta
	// emoji selects the moon-phase reach-ramp marker set
	// (🌕/🌔/🌓/🌒/🌑/🌫️/✕) over the ASCII fallback (○/◔/◑/◉/●/▲/✕). Resolved
	// once for the model and copied onto each item.
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
// only, 🌓/◑ on tailnet, 🌒/◉ served (a notch more established than merely
// reachable), 🌑/● funnelled to the public internet. The two BROKEN states
// sit OFF the ramp as plain glyphs, not moons: 🌫️/▲ a stale dangling
// forward, ✕/✕ a favorite whose process is down. It switches on the SAME
// i.reach() resolver Description() uses, so the glyph and the row's text can
// never disagree about a port's state. Emoji markers are padded to a stable
// 2-cell width so the :port column stays aligned even if a terminal renders
// a given emoji (or the naturally 1-cell ✕) narrow.
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
	case reachServed:
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
	case reachTailnet:
		// Already tailnet-reachable by IP (wildcard/tailnet bind), unserved.
		// Same plain, unstyled tier as reachLocalhost/reachLAN below -- these
		// three are healthy ramp states, not warnings.
		if i.emoji {
			m = "🌓"
		} else {
			m = "◑"
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
	return fmt.Sprintf("%s%s :%d  %s%s", marker, lock, i.port.Number, star, name)
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
	reachStale                       // E: served but nothing listening -- a dangling forward
	reachOffline                     // F: not served, not listening (e.g. a down favorite)
)

// reach resolves a portItem's reachState. Precedence (top to bottom): funnel
// outranks serve state (a funnelled port is public regardless of its tailnet
// serve status); a served port is either healthy (C, listening) or a stale
// dangling forward (E, not listening); an unserved port's reachability comes
// straight from its widest bind scope (portscan.BindScope); anything neither
// served nor listening is simply offline.
func (i portItem) reach() reachState {
	switch {
	case i.funnelPublic != 0:
		return reachFunnel
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

func (i portItem) Description() string {
	switch i.reach() {
	case reachFunnel:
		// Public URL, not the tailnet one: this is what "anyone on the
		// internet" reaches. Degrades to a hostless URL if the FQDN is unknown.
		return publicStyle.Render("on the internet · " + tsserve.PublicURL(i.fqdn, i.funnelPublic))
	case reachStale:
		// Dangling forward: served, but no local process holds it. Lead with
		// the plain state and WHY it looks exposed-yet-empty -- tailscale is
		// still holding the port -- since that's the confusing part. The fix
		// (bind the app to loopback, not 0.0.0.0; or un-expose) is spelled out
		// in ? help and the README, where there's room to explain it.
		return warnStyle.Render("bound to tailnet, but stale — space to unbind")
	case reachServed:
		desc := i.servedDescPlain()
		if i.justCopied {
			// Pre-styled bold-green suffix (py5b): the delegate's rune
			// highlighter is ANSI-unaware, but filterNoHighlight (see New)
			// already strips the per-char match highlight from every row,
			// so embedding raw ANSI here is safe -- confirmed by the tmux
			// capture in the close message.
			desc += activeStyle.Render(copiedSuffix)
		}
		return desc
	case reachTailnet:
		if i.port.Number == 22 {
			// sshd on a wildcard bind is the canonical case: already
			// reachable, and the very SSH session the operator may be
			// reading this over -- call it out so it's obviously not
			// something to serve.
			return "on tailnet · reachable via SSH"
		}
		return "on tailnet"
	case reachLAN:
		return "local network only"
	case reachOffline:
		return "offline"
	default: // reachLocalhost
		return "localhost only"
	}
}

// servedDescPlain returns the UNSTYLED state-C description text ("on
// tailnet · http://host:port") -- the row text Description() renders for
// reachServed, and the exact string whose URL copyURL copies. Shared by
// Description() and copyURL's inlineCopyFits width check (py5b) so the two
// can never drift out of sync about what the row actually shows.
func (i portItem) servedDescPlain() string {
	return fmt.Sprintf("on tailnet · http://%s:%d", i.host, i.port.Number)
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

type toggleDoneMsg struct {
	port int
	err  error
}

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
)

type model struct {
	list list.Model
	help help.Model
	keys keyMap
	cfg  config.Config
	host string
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
	// ramp (🌕🌔🌓🌒🌑🌫️✕) over the mono ASCII/Unicode-symbol fallback
	// (○◔◑◉●▲✕), resolved once at New() from resolveMarkerEmoji(markersMode)
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
// ramp 🌕🌔🌓🌒🌑🌫️✕ vs its mono fallback ○◔◑◉●▲✕) from the configured
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

	l := list.New(nil, newPortDelegate(), 0, 0)
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
		list: l, help: h, keys: newKeyMap(), cfg: cfg, host: host, active: map[int]bool{},
		portInput: ti, labelInput: li, sshInput: si, configPath: configPath,
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
// cfg.Markers rather than overwriting it.
func Run(cfg config.Config, markersOverride string) error {
	_, err := tea.NewProgram(New(cfg, markersOverride), tea.WithAltScreen()).Run()
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
	return tea.Batch(refresh, fetchFQDN, detectOperator, refreshTick(), tea.SetWindowTitle(title))
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

// copiedSuffix is the plain (unstyled) text of the inline copy confirmation
// (py5b), appended -- pre-styled bold-green via activeStyle -- to a state-C
// row's description when its portItem.justCopied is set. Kept as one
// constant so the width-fit check (inlineCopyFits) and the styled render
// (portItem.Description) can never drift out of sync about its width.
const copiedSuffix = "  ✓ copied"

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

// availableDescriptionWidth returns the width (in cells) the list delegate
// truncates a row's title/description to, given the model's current
// terminal width -- the same budget bubbles/list enforces at render time, so
// inlineCopyFits can decide whether the "✓ copied" suffix will actually be
// visible before copyURL appends it.
func (m *model) availableDescriptionWidth() int {
	return m.width - descTruncateStyle.GetPaddingLeft() - descTruncateStyle.GetPaddingRight()
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

// copyURL copies the selected port's TAILNET URL to the clipboard and
// confirms the copy. The URL is always the tailnet form
// (http://<host>:<port>), regardless of any public funnel. State C
// (reachServed: served, listening, not funnelled) is the one case where the
// row's description IS that exact URL, so -- provided the annotation fits
// the terminal width (inlineCopyFits) -- the confirmation goes inline as a
// transient "✓ copied" on the row instead of the bottom-bar toast (py5b).
// Every other case keeps the toast, unchanged: it's the only place that can
// name a MISMATCH between the shown URL and the copied one (funnelled), flag
// a dangling forward (no URL shown at all), or carry "serve it first"
// guidance (localhost/LAN-only or offline) -- and it's also the fallback
// when a state-C copy wouldn't fit inline.
func (m *model) copyURL(sel portItem) tea.Cmd {
	url := fmt.Sprintf("http://%s:%d", m.host, sel.port.Number)

	if sel.reach() == reachServed && inlineCopyFits(lipgloss.Width(sel.servedDescPlain()), m.availableDescriptionWidth()) {
		m.copiedID++
		id := m.copiedID
		m.copiedPort = sel.port.Number
		// Clear any lingering toast so the two confirmation channels never
		// show at once (the KeyMsg handler already does this on every
		// keypress before dispatch, but copyURL is the one place that
		// decides inline-vs-toast, so it's made explicit here too).
		m.flash = ""
		m.flashLevel = flashInfo
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
		// reachLAN message (ui.go ~2019) so c and space agree.
		flash = m.setFlash(fmt.Sprintf("copied — :%d is bound to your LAN only; serve can't reach this bind", sel.port.Number), flashWarn)
	default:
		// reachLocalhost / reachOffline: genuinely localhost-only (or a down
		// favorite) -- "press space to serve it" is TRUE here. reachServed
		// (the inline path didn't fit) / reachFunnel / reachStale are all
		// `active`, so keep the plain "copied ✓ url" confirmation.
		if sel.active {
			flash = m.setFlash("copied ✓  "+url, flashInfo)
		} else {
			flash = m.setFlash(fmt.Sprintf("copied — :%d is localhost only; press space to serve it", sel.port.Number), flashWarn)
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

// requestFunnel begins toggling the public funnel for a port (the "p" key).
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

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.help.Width = msg.Width
		m.width = msg.Width
		m.height = msg.Height
		// Reserve the WORST-CASE legend height -- render it as if a dangling
		// forward were present (cleanEnabled=true) so the grouped grid/bar always
		// fits. That way the list never overlaps the bar when a dangling appears
		// (or vanishes) between resizes: the grid is a constant 5 rows regardless,
		// and the wrapped fallback is measured with the extra "C" hint included.
		legendLines := 1
		if legend := m.renderLegendWith(true); legend != "" {
			legendLines = strings.Count(legend, "\n") + 1
		}
		// Reserve the WORST-CASE operator-hint banner height too (kata tapv),
		// unconditionally -- like cleanEnabled=true above, NOT gated on the
		// CURRENT m.operatorNotSet. The banner can appear asynchronously (a
		// failed toggle's toggleDoneMsg, or the startup detectOperatorMsg)
		// with no fresh WindowSizeMsg in between, so sizing done here has to
		// already assume the worst case or a later appearance would clip the
		// list by one row. operatorHintText() is always exactly one line (no
		// embedded newlines), so the reservation is a constant 1.
		const bannerLines = 1
		// Reserve the persistent top header (one row) plus the bottom bar: one
		// blank separator, the status line, and the grouped shortcuts legend.
		// View then pads the gap so the bar lands on the last rows (see
		// renderHeader / renderBottom).
		headerLines := lipgloss.Height(m.renderHeader())
		m.list.SetSize(msg.Width, msg.Height-headerLines-legendLines-bannerLines-2)
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
			m.fqdn = msg.fqdn
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
					return m, tea.Batch(m.favorite(port), m.rebuildItems())
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
						meta := m.cfg.Ports[port]
						meta.Label = label
						m.cfg.Ports[port] = meta
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
					meta := m.cfg.Ports[port]
					meta.Locked = false
					m.cfg.Ports[port] = meta
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

		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "/":
			// Widen scope to ALL listening ports BEFORE handing "/" to the list
			// (4ye6): the list snapshots its current items when filtering
			// begins, so the items must already be the full set. Rebuilding
			// while still Unfiltered avoids bubbles/list's async re-filter path.
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
			meta := m.cfg.Ports[sel.port.Number]
			meta.Favorite = true
			m.cfg.Ports[sel.port.Number] = meta
			return m, tea.Batch(m.saveConfig(), m.rebuildItems())
		case "u":
			sel, ok := m.list.SelectedItem().(portItem)
			if !ok {
				return m, nil
			}
			if meta, ok := m.cfg.Ports[sel.port.Number]; ok {
				meta.Favorite = false
				if meta.Label == "" && !meta.Locked {
					delete(m.cfg.Ports, sel.port.Number)
				} else {
					m.cfg.Ports[sel.port.Number] = meta
				}
				return m, tea.Batch(m.saveConfig(), m.rebuildItems())
			}
			return m, nil
		case "x":
			sel, ok := m.list.SelectedItem().(portItem)
			if !ok {
				return m, nil
			}
			if m.cfg.Ports == nil {
				m.cfg.Ports = map[int]config.PortMeta{}
			}
			// Unlike "u", unlocking never deletes the registry entry: once
			// a port has been locked (or explicitly unlocked), it stays
			// "known" and visible in the default view, same as a port
			// that's been toggled on at least once (see remember).
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
						return m, m.setFlash("already on tailnet as SSH — this is how you're connected; nothing for tailport to serve", flashInfo)
					}
					// Wildcard bind owned by the APP (0.0.0.0), not a tailport
					// serve, so there's no mapping to toggle. Honest about WHY,
					// and actionable: rebinding to loopback -> reach()==reachLocalhost,
					// which the guard does NOT block -> space then serves it.
					return m, m.setFlash("already on tailnet — app bound wide (0.0.0.0), not tailport; rebind it to localhost (or 127.0.0.1) to make serve toggleable", flashInfo)
				case reachLAN:
					return m, m.setFlash("on your LAN only; serve can't reach this bind", flashInfo)
				}
			}
			// requestToggle applies the lock guard and, for :22 only, the SSH
			// y/n confirm before any serve call.
			return m, m.requestToggle(sel.port.Number, !sel.active)
		case "p":
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
		}
	}

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
			items = append(items, portItem{port: p, active: m.active[n], listening: ok, host: m.host, fqdn: m.fqdn, funnelPublic: m.funnel[n], dimmed: dimNonFav && !meta.Favorite, meta: meta, emoji: m.markerEmoji, justCopied: m.copiedPort == n})
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
		items = append(items, portItem{port: p, active: m.active[n], listening: ok, host: m.host, fqdn: m.fqdn, funnelPublic: m.funnel[n], meta: m.cfg.Ports[n], emoji: m.markerEmoji, justCopied: m.copiedPort == n})
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
// here -- grey/hide "space serve" when the selected port is already B/B'
// tailnet/LAN reachable (space would just no-op it with an info toast; see
// the pt3 guard in the space key handler). Prototyped as a second
// spaceEnabled bool threaded through here/barGroups exactly like
// cleanEnabled, but MEASURED to break TestLegendReservationDominatesLive's
// brute-force width scan: at width 54, hiding "space" while "clean" stays
// shown renders the Expose column at 6 lines against a 4-line worst-case
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
// stale" unless cleanEnabled. The Expose column therefore ends at "x
// lock/unlock" (no reserved blank slot) when nothing is dangling, and gains
// "C clean stale" just above lock when a dangling forward exists.
func (m model) barGroups(cleanEnabled bool) []keyGroup {
	keys := m.keys
	keys.ShowAll.SetHelp("a", "switch view")
	keys.Clean.SetEnabled(cleanEnabled)
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
	return (rand.Float64()+rand.Float64()+rand.Float64())/3*2 - 1
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
	nSmoke := fwSmokeMin + rand.Intn(fwSmokeMax-fwSmokeMin+1)
	smoke := make([]fwParticle, 0, nSmoke)
	for i := 0; i < nSmoke; i++ {
		smoke = append(smoke, fwParticle{
			x:   x0 + bellRange(-0.75, 0.75),
			y:   y0,
			vx:  bellRange(-0.2, 0.2),
			vy:  bellRange(-0.35, -0.1),
			ttl: fwSmokeLifeMin + rand.Intn(fwSmokeLifeMax-fwSmokeLifeMin+1),
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
		scheme:     rand.Intn(len(fwSchemes)),
		flourish:   rand.Float64() < fwFlourishChance,
		flourishAt: 3 + rand.Intn(4),
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
		ang := 2*math.Pi*float64(i)/float64(n) + (rand.Float64()-0.5)*0.6
		speed := f.radius * (0.45 + 0.55*rand.Float64())
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
	n := 6 + rand.Intn(6)
	for i := 0; i < n; i++ {
		ang := 2*math.Pi*float64(i)/float64(n) + rand.Float64()
		speed := f.radius * 1.4 * (0.5 + 0.6*rand.Float64())
		f.particles = append(f.particles, fwParticle{
			x:       f.xExp,
			y:       f.yExp,
			vx:      math.Cos(ang) * speed * fwAspect,
			vy:      math.Sin(ang) * speed,
			ttl:     4 + rand.Intn(4),
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
	served, funneled, dangling := "◉", "●", "▲"
	if emoji {
		served, funneled, dangling = "🌒", "🌑", "🌫️"
	}
	return map[string]string{
		"space": "Toggle tailscale serve for the selected port on/off. Once a port\nis served (" + served + ") its tailnet URL is shown beneath it. Only offered\nfor a loopback-bound port -- one already reachable on the tailnet\nneeds no serving, so space is a no-op there.",
		"p":     "Funnel the selected port to the PUBLIC INTERNET via tailscale\nfunnel (" + funneled + "), behind a strong y/n confirm. Funnel is HTTPS-only and\ncan use just three public ingress ports — 443, 8443, 10000\n(auto-assigned, max three at once) — so the public port won't match\nthe local one. :22 (SSH) is refused. Press p again to drop the port\nback to tailnet-served.",
		"c":     "Copy the selected port's tailnet URL (http://<host>:<port>) to the\nclipboard, via OSC 52 so it works even over SSH (needs a terminal\nthat supports it; tmux: set -g set-clipboard on). Copies even before\nit's served — the toast says so.",
		"f":     "Favorite the selected port (marks it ★). Favorites are a durable\nshortlist — one of the two `a` views — that survives restarts and\nstays visible even when the process isn't running.",
		"u":     "Unfavorite (clears ★); the port drops out of the Favorites view.",
		"n":     "Add a port by number to Favorites (★), even one not currently\nlistening. It doesn't serve — it just registers and sticks in the\nFavorites view; press space there to serve it once its service is up.",
		"l":     "Set a text label for the selected port.",
		"x":     "Lock / unlock the selected port (🔒). A locked port can't be\ntoggled on until you unlock it — a guard against exposing something\nby accident. Port :22 is locked by default; unlocking it requires\ntyping \"ssh\" to confirm (it guards your SSH access).",
		"C":     "Tear down stale forwards — ports still served by tailscale with\nnothing listening locally (shown " + dangling + "). Offered only when some exist.",
		"/":     "Filter by port number, process, or label (fuzzy). Searches ALL\nlistening ports regardless of view, so it works even from an empty\nFavorites screen; non-favorite matches show dimmed in the Favorites\nview. esc clears the filter.",
		"a":     "Switch between the two list views: Favorites (only ★ ports) and\nAll ports (every port listening locally, plus your favorites even\nwhen their process is down).",
		"r":     "Refresh the port list and serve status.",
		"?":     "Toggle this help. esc or q also close it.",
		"q":     "Quit.",
	}
}

// KeyLegendGroups returns the full keybinding legend grouped into the same four
// sections, in the same order, as the bottom-bar grid -- Expose, Favorites,
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
		return "🌕 localhost   🌔 local network   🌓 on tailnet   🌒 served\n" +
			"🌑 internet (funnel)   🌫️ stale   ✕ offline\n" +
			"🔒 locked   ★ favorite"
	}
	return "○ localhost   ◔ local network   ◑ on tailnet   ◉ served\n" +
		"● internet (funnel)   ▲ stale   ✕ offline\n" +
		"🔒 locked   ★ favorite"
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
			"A port can also be funnelled to the PUBLIC internet with `p` (opt-in,\n" +
			"see below)."))
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
	// (Expose, Favorites, View, App) -- each section's own header stands in for
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
		t("plain HTTP, same port in and out. Press ") + k("p") + t(" to funnel one publicly."),
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
// pinned top-left and the Favorites|All-ports toggle right-aligned on the same
// row, spanning the terminal width. View() draws it above both the list body
// and the empty state, so the logo never disappears when a view is empty or
// when switching views. Before the first WindowSizeMsg (width == 0) the two
// fall back to a single-space separation.
func (m model) renderHeader() string {
	logo := logoStyle.Render("tailport")
	toggle := m.renderViewIndicator()
	gap := m.width - lipgloss.Width(logo) - lipgloss.Width(toggle)
	if gap < 1 {
		gap = 1 // too narrow to justify; keep at least a space (may wrap)
	}
	return logo + strings.Repeat(" ", gap) + toggle
}

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
	// Widest form that fits, degrading host -> shorter labels -> initials.
	switch {
	case m.width <= 0 || lipgloss.Width(withHost) <= m.width:
		return withHost
	case lipgloss.Width(full) <= m.width:
		return full
	default:
		// Medium drops the host and shortens labels; "funnel" stands in for
		// "public (funnel)" for compactness while staying precise.
		medium := fmt.Sprintf("%d listening · %d tailnet · %d funnel", listening, tailnet, public)
		if lipgloss.Width(medium) <= m.width {
			return medium
		}
		return fmt.Sprintf("%dL · %dT · %dP", listening, tailnet, public)
	}
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
	you := m.operatorUser
	if you == "" {
		you = "<you>"
	}
	return fmt.Sprintf("⚠ tailscale operator not set — run once: sudo tailscale set --operator=%s  (then press r)  — or run tailport with sudo", you)
}

// renderBottom builds the bottom bar. In a modal entry mode it's the prompt
// for that flow; otherwise it's the status line, with the shortcuts legend on
// the last row(s). The Favorites|All-ports toggle lives in the top header
// (renderHeader), not here. View pins whatever this returns to the bottom of
// the viewport.
func (m model) renderBottom() string {
	switch m.mode {
	case entryAddPort:
		return helpStyle.Render("add port to favorites: ") + m.portInput.View() + helpStyle.Render("  (enter: confirm, esc: cancel)")
	case entryLabel:
		return helpStyle.Render(fmt.Sprintf("label :%d: ", m.labelPort)) + m.labelInput.View() + helpStyle.Render("  (enter: confirm, esc: cancel)")
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
	}
	// A transient toast (copy confirmation, refusal, or error) takes the
	// status line's spot while it's showing; otherwise the normal breakdown.
	// The severity colours it: info green, warn amber, error red (q89g).
	statusLine := helpStyle.Render(m.statusText())
	if m.flash != "" {
		toast := activeStyle
		switch m.flashLevel {
		case flashWarn:
			toast = warnStyle
		case flashError:
			toast = errStyle
		}
		statusLine = toast.Render(m.flash)
	}
	bar := statusLine
	// The sticky operator hint (kata tapv), when active, sits ABOVE the
	// status line -- unlike the transient toast it never auto-dismisses, so
	// it stays put through refreshes and keypresses that would otherwise
	// clear m.flash. Styled via warnStyle (a NAMED style, not a hardcoded
	// color) so it stays legible under any future light/dark AdaptiveColor
	// conversion of that style.
	if hint := m.operatorHintText(); hint != "" {
		bar = warnStyle.Render(hint) + "\n" + bar
	}
	if legend := m.renderLegend(); legend != "" {
		bar += "\n" + legend
	}
	return bar
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
		body += m.list.View()
	}

	// Pin the bottom bar (shortcuts, status -- or a modal prompt) to the last
	// rows of the viewport by padding the gap. Before the first WindowSizeMsg
	// (m.height == 0) this falls back to a single blank separator line.
	bottom := m.renderBottom()
	gap := m.height - lipgloss.Height(header) - lipgloss.Height(body) - lipgloss.Height(bottom)
	if gap < 1 {
		gap = 1
	}
	return header + "\n" + body + strings.Repeat("\n", gap) + bottom
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
