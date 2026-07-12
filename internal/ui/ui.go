// Package ui implements tailport's Bubble Tea TUI: a list of locally
// listening ports, toggled on/off tailnet-wide via tailscale serve.
package ui

import (
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

var (
	activeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	favStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Bold(true)
	lockStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Bold(true)
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	helpStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	// wasStyle renders a remembered-but-gone process name ("was mailpit") as a
	// muted italic, so it reads as a memory rather than a live label.
	wasStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Italic(true)
	// publicStyle marks a port funnelled to the public internet -- deliberately
	// a hot magenta ● (ASCII mode), distinct from the green ◉ tailnet-serve and
	// amber ▲ dangling markers, so "this is on the public internet" reads at a glance.
	publicStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("201")).Bold(true)

	helpTitleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	helpKeyStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("81")).Bold(true)
	helpTextStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))

	// logoStyle draws the persistent cyan "tailport" wordmark pinned to the
	// top-left of every view (list and empty-state alike); see renderHeader.
	logoStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("51")).Bold(true)

	// The two segments of the Favorites|All-ports view indicator: the active
	// view is a filled green chip, the inactive one is dim.
	viewActiveStyle   = lipgloss.NewStyle().Background(lipgloss.Color("42")).Foreground(lipgloss.Color("233")).Bold(true)
	viewInactiveStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
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
// derive from it, so the five columns can never drift apart.
type keyGroup struct {
	name     string
	bindings []key.Binding
}

// groups returns the approved like-for-like grouping (kata p39s): Expose,
// Favorites, Protect, View, App -- in display order, one group per bottom-bar
// column and one "?"-overlay section.
func (k keyMap) groups() []keyGroup {
	return []keyGroup{
		{"Expose", []key.Binding{k.Toggle, k.Funnel, k.Copy}},
		{"Favorites", []key.Binding{k.Favorite, k.Unfavorite, k.NewPort, k.Label}},
		{"Protect", []key.Binding{k.Lock, k.Clean}},
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
	// emoji selects the egg-lifecycle marker set (🥚/🐣/🐦/🪹) over the ASCII
	// fallback (○/●/◉/▲). Resolved once for the model and copied onto each item.
	emoji bool
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

// markerGlyph is the port's exposure-state marker. In emoji mode it's the egg
// life cycle (🥚 idle → 🐣 tailnet → 🐦 public, 🪹 empty nest for a dead
// forward); otherwise the styled ASCII fallback. Funnel outranks serve state,
// which outranks a dangling forward, which outranks idle. Emoji markers are
// padded to a stable 2-cell width so the :port column stays aligned even if a
// terminal renders a given emoji narrow.
func (i portItem) markerGlyph() string {
	var m string
	switch {
	case i.funnelPublic != 0:
		// Reachable from the open internet -- outranks tailnet-serve state.
		if i.emoji {
			m = "🐦"
		} else {
			m = publicStyle.Render("●")
		}
	case i.active && !i.listening:
		// Dangling forward: served, but nothing is bound locally, so a tailnet
		// peer hitting the URL gets connection refused. An empty nest.
		if i.emoji {
			m = "🪹"
		} else {
			m = warnStyle.Render("▲")
		}
	case i.active:
		if i.emoji {
			m = "🐣"
		} else {
			m = activeStyle.Render("◉")
		}
	default:
		if i.emoji {
			m = "🥚"
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

func (i portItem) Description() string {
	if i.funnelPublic != 0 {
		// Public URL, not the tailnet one: this is what "anyone on the
		// internet" reaches. Degrades to a hostless URL if the FQDN is unknown.
		return publicStyle.Render("public: " + tsserve.PublicURL(i.fqdn, i.funnelPublic))
	}
	if i.active {
		if !i.listening {
			// Dangling forward: served, but no local process holds it. Lead with
			// the plain state and WHY it looks exposed-yet-empty -- tailscale is
			// still holding the port -- since that's the confusing part. The fix
			// (bind the app to loopback, not 0.0.0.0; or un-expose) is spelled out
			// in ? help and the README, where there's room to explain it.
			return warnStyle.Render("bound to tailscale, press space to release/unbind")
		}
		return fmt.Sprintf("http://%s:%d", i.host, i.port.Number)
	}
	return "not exposed"
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
	// except the ?/esc/q that dismiss it.
	showHelp bool
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
	// configPath is the resolved absolute path where preferences (the port
	// registry: favorites, labels, locks) are persisted, captured once at
	// New() from config.Path(cfg.ResolvedPath()) so the help overlay can
	// state it exactly -- honoring an explicit -c/--config override or
	// XDG_CONFIG_HOME rather than guessing (gahj, y4gt). Empty if
	// config.Path() errored, in which case helpView describes the rule instead.
	configPath string
	// emoji selects the egg-lifecycle exposure markers over the ASCII fallback,
	// resolved once at New() from cfg.Markers + the terminal's capabilities.
	emoji bool
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

// resolveEmoji picks the marker set from the configured mode: "emoji"/"ascii"
// force it; anything else ("auto" or empty) defers to the terminal's apparent
// UTF-8 capability.
func resolveEmoji(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "emoji":
		return true
	case "ascii":
		return false
	default:
		return emojiCapable()
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

// ResolveEmoji exports resolveEmoji's marker-glyph resolution for callers
// outside this package. `tailport quickstart` (kata x4cg) uses it so its
// printed legend picks the same glyph set (see keyLegendDescs) the "?"
// overlay would for the same markers mode, not just the same key text.
func ResolveEmoji(mode string) bool {
	return resolveEmoji(mode)
}

// New builds the initial model from a loaded Config. markersOverride is an
// optional, run-only "--markers" value (zn2x): the caller (main.go, after
// its own validation) passes at most one string. When it's non-empty it
// wins for THIS session's glyph choice (m.emoji, resolved once below), but
// it deliberately never touches cfg.Markers itself -- cfg is stored as-is
// into m.cfg, which is what any later Save() (triggered by an unrelated
// mutation: favorite/label/lock/etc.) writes back to disk. Mutating
// cfg.Markers here would leak the run-only override into the persisted
// config on the next unrelated save, which is exactly what "applies to the
// current run only; never rewrites config" rules out.
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

	// Resolve the config path once here (best-effort) so the help overlay can
	// show exactly where settings live, -c/--config and XDG overrides all. If
	// cfg came from config.Load, ResolvedPath() already pins the exact file;
	// otherwise (e.g. a literal built directly by a test) this falls back to
	// normal XDG/~/.config resolution. On error we leave it empty and
	// helpView falls back to describing the rule.
	configPath, _ := config.Path(cfg.ResolvedPath())

	// Run-only markers override (zn2x): flag > cfg.Markers > terminal
	// auto-detect. Only the resolved emoji bool below is affected; cfg
	// itself (and thus what a later Save() persists) is untouched.
	markersMode := cfg.Markers
	if len(markersOverride) > 0 && markersOverride[0] != "" {
		markersMode = markersOverride[0]
	}

	return model{list: l, help: h, keys: newKeyMap(), cfg: cfg, host: host, active: map[int]bool{}, portInput: ti, labelInput: li, sshInput: si, configPath: configPath, emoji: resolveEmoji(markersMode)}
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
	return tea.Batch(refresh, fetchFQDN, refreshTick())
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
	// ignored (perf-safe under mashing).
	fwCap = 25
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

// copyURL copies the selected port's TAILNET URL to the clipboard and returns
// a command that runs the copy and shows a confirmation toast. The URL is
// always the tailnet form (http://<host>:<port>), matching the row
// description, regardless of any public funnel. When the port isn't served on
// the tailnet the toast warns that the URL won't resolve until it's toggled on.
func (m *model) copyURL(sel portItem) tea.Cmd {
	url := fmt.Sprintf("http://%s:%d", m.host, sel.port.Number)
	var flash tea.Cmd
	if sel.active {
		flash = m.setFlash("copied ✓  "+url, flashInfo)
	} else {
		flash = m.setFlash(fmt.Sprintf("copied — :%d not exposed on tailnet (toggle it on first)", sel.port.Number), flashWarn)
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
		// Reserve the persistent top header (one row) plus the bottom bar: one
		// blank separator, the status line, and the grouped shortcuts legend.
		// View then pads the gap so the bar lands on the last rows (see
		// renderHeader / renderBottom).
		headerLines := lipgloss.Height(m.renderHeader())
		m.list.SetSize(msg.Width, msg.Height-headerLines-legendLines-2)
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
			return m, nil
		}
		m.fireworks = stepFireworks(m.fireworks)
		if len(m.fireworks) == 0 {
			m.fwTicking = false
			return m, nil
		}
		return m, fwTick()

	case toggleDoneMsg:
		m.pending = 0
		if msg.err != nil {
			// The error toast auto-dismisses (q89g); still refresh to reconcile
			// state -- the toast survives a refresh now that notifications no
			// longer live in a field the refresh clears.
			return m, tea.Batch(m.setErr(msg.err.Error()), refresh)
		}
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
				if len(m.fireworks) < fwCap {
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
		// The help overlay is modal: while it's open, only ?/esc/q close it
		// and every other key is swallowed (so nothing happens "behind" it).
		if m.showHelp {
			switch msg.String() {
			case "?", "esc", "q", "ctrl+c":
				m.showHelp = false
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
			return m, refresh
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
			items = append(items, portItem{port: p, active: m.active[n], listening: ok, host: m.host, fqdn: m.fqdn, funnelPublic: m.funnel[n], dimmed: dimNonFav && !meta.Favorite, meta: meta, emoji: m.emoji})
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
		items = append(items, portItem{port: p, active: m.active[n], listening: ok, host: m.host, fqdn: m.fqdn, funnelPublic: m.funnel[n], meta: m.cfg.Ports[n], emoji: m.emoji})
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
// aligned column grid fits the width it renders that; otherwise it falls back to
// a wrapped grouped bar. Neither ever truncates or ellipsizes a hint -- narrow
// terminals wrap, they never clip. The width source is m.help.Width (set from
// each WindowSizeMsg); width <= 0 means unbounded, so the full grid renders.
//
// cleanEnabled controls the contextual "C clean stale" hint (shown only when
// dangling forwards exist). WindowSizeMsg passes true to reserve the worst-case
// height; the live render passes m.hasDangling().
func (m model) renderLegendWith(cleanEnabled bool) string {
	groups := m.barGroups(cleanEnabled)
	grid, gridWidth := renderLegendGrid(m.help.Styles, groups)
	if m.help.Width > 0 && m.help.Width < gridWidth {
		return renderGroupedBar(m.help.Styles, groups, m.help.Width)
	}
	return grid
}

// barGroups adapts keyMap.groups() for the bottom bar: it relabels "a" to
// "switch view" (its keymap help is "filtered"; the active view is shown by the
// header indicator, renderViewIndicator) and drops the contextual "C clean
// stale" unless cleanEnabled. The Protect column therefore collapses to just
// "x lock/unlock" when nothing is dangling, with no reserved blank slot.
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

// renderLegendGrid lays the groups out as an aligned column grid: a styled
// header row over, per group, a left-aligned key gutter + description. Column
// widths are computed from content (header vs the widest "key desc"), and every
// cell is padded so the columns line up. It returns the rendered grid and its
// total display width, which renderLegendWith uses as the responsive threshold.
func renderLegendGrid(styles help.Styles, groups []keyGroup) (string, int) {
	n := len(groups)
	if n == 0 {
		return "", 0
	}
	type cell struct{ key, desc string }
	cols := make([][]cell, n)
	keyGutter := make([]int, n)
	colWidth := make([]int, n)
	maxRows := 0
	for c, g := range groups {
		for _, b := range g.bindings {
			h := b.Help()
			cols[c] = append(cols[c], cell{h.Key, h.Desc})
			if w := lipgloss.Width(h.Key); w > keyGutter[c] {
				keyGutter[c] = w
			}
		}
		if len(cols[c]) > maxRows {
			maxRows = len(cols[c])
		}
		colWidth[c] = lipgloss.Width(g.name)
		for _, cl := range cols[c] {
			if w := keyGutter[c] + 1 + lipgloss.Width(cl.desc); w > colWidth[c] {
				colWidth[c] = w
			}
		}
	}

	total := 0
	for c, w := range colWidth {
		total += w
		if c < n-1 {
			total += legendColGap
		}
	}

	gap := strings.Repeat(" ", legendColGap)
	var lines []string

	// Header row: the group names, each padded to its column width.
	var hb strings.Builder
	for c, g := range groups {
		hb.WriteString(helpTitleStyle.Render(g.name))
		hb.WriteString(strings.Repeat(" ", colWidth[c]-lipgloss.Width(g.name)))
		if c < n-1 {
			hb.WriteString(gap)
		}
	}
	lines = append(lines, strings.TrimRight(hb.String(), " "))

	// Data rows: key gutter + description, padded to align.
	for r := 0; r < maxRows; r++ {
		var rb strings.Builder
		for c := range groups {
			plainW := 0
			if r < len(cols[c]) {
				cl := cols[c][r]
				rb.WriteString(styles.ShortKey.Inline(true).Render(cl.key))
				rb.WriteString(strings.Repeat(" ", keyGutter[c]-lipgloss.Width(cl.key)))
				rb.WriteString(" ")
				rb.WriteString(styles.ShortDesc.Inline(true).Render(cl.desc))
				plainW = keyGutter[c] + 1 + lipgloss.Width(cl.desc)
			}
			rb.WriteString(strings.Repeat(" ", colWidth[c]-plainW))
			if c < n-1 {
				rb.WriteString(gap)
			}
		}
		lines = append(lines, strings.TrimRight(rb.String(), " "))
	}
	return strings.Join(lines, "\n"), total
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
	eggGold         = []string{"220", "214", "178", "226", "184"}
	eggSparks       = []rune("✦✧⋆∗✺❋✸•*")
	eggSparkColors  = []string{"196", "202", "226", "46", "51", "201", "213", "129"}
	eggCreditColors = []string{"213", "219", "225", "51", "45", "87"}
	eggZalgoMarks   = []rune{'́', '҉', '̴', '͓', 'ͯ'}
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

// eggSparkleLine builds one row of fireworks: mostly spaces with a few
// deterministically-placed coloured sparkles (varying per frame). Width is
// clamped so it never overflows the screen.
func eggSparkleLine(w, frame, salt int) string {
	var b strings.Builder
	for _, c := range eggSparkleCells(w, frame, salt) {
		b.WriteString(c.render())
	}
	return b.String()
}

// eggSparkleCells is eggSparkleLine's compositing core: the same deterministic
// sparkle pattern as styledCells, so the FLOATING rainbow fanfare can be laid
// into the fireworks grid (and its absolute row recorded for the burst band).
func eggSparkleCells(w, frame, salt int) []styledCell {
	if w > 56 {
		w = 56
	}
	out := make([]styledCell, w)
	for x := 0; x < w; x++ {
		if (x*29+frame*13+salt*7)%9 == 0 {
			c := eggSparkColors[(x+frame+salt)%len(eggSparkColors)]
			s := eggSparks[(x*3+frame)%len(eggSparks)]
			out[x] = styledCell{s: string(s), color: lipgloss.Color(c)}
		} else {
			out[x] = blankCell()
		}
	}
	return out
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
	fwCountMax        = 34.0
	fwRadiusMin       = 0.45 // burst expansion speed (bell) -> radius
	fwRadiusMax       = 1.25
	fwBurstLifeMin    = 8.0 // per-particle life in frames (bell)
	fwBurstLifeMax    = 18.0
	fwFlourishChance  = 0.4  // fraction of fireworks that get a secondary crackle
	fwLaunchSpreadPct = 0.15 // horizontal launch offset: +/-15% of viewport width
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

	// Launch: centre-bottom, horizontal offset bell within +/-15% of width.
	x0 := float64(w)/2 + bellRange(-fwLaunchSpreadPct*float64(w), fwLaunchSpreadPct*float64(w))
	y0 := float64(h - 1)

	// Explosion band: a few rows above the top fanfare to a few below the
	// bottom fanfare -- against the FLOATING rows, not fixed lines. Degenerate
	// (tiny) layouts fall back to the mid-viewport so nothing panics.
	bandTop := float64(lay.topFanfareRow - 3)
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

	return firework{
		x0:         x0,
		y0:         y0,
		v0:         v0,
		g:          fwGravity,
		vx:         bellRange(-0.3, 0.3),
		tExp:       tExp,
		yExp:       yExp,
		stage:      fwRising,
		count:      int(bellRange(fwCountMin, fwCountMax)),
		radius:     bellRange(fwRadiusMin, fwRadiusMax),
		scheme:     rand.Intn(len(fwSchemes)),
		flourish:   rand.Float64() < fwFlourishChance,
		flourishAt: 3 + rand.Intn(4),
		emoji:      emoji,
	}
}

func (f *firework) posY(t float64) float64 { return f.y0 - f.v0*t + 0.5*f.g*t*t }
func (f *firework) posX(t float64) float64 { return f.x0 + f.vx*t }

// step advances one firework by one frame: climb then explode, then age the
// embers (with gravity) and, once, add a flourish crackle.
func (f *firework) step() {
	f.t++
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

// done reports a spent firework: burst, all embers expired, and any flourish
// already emitted (so we don't reap it before the secondary crackle fires).
func (f *firework) done() bool {
	return f.stage == fwBurst && len(f.particles) == 0 && (!f.flourish || f.flourished)
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
	fwGlyphsUnicode = []rune{'·', '∗', '✦', '❋', '✺'} // dim -> bright
	fwGlyphsASCII   = []rune{'.', ':', '+', '*', '@'} // ascii fallback (no mojibake)
)

// glyphSet gates the glyph vocabulary on emoji capability (5x1e is stricter than
// the egg's sparkles, which assumed UTF-8): ascii terminals get pure-ASCII sparks.
func (f *firework) glyphSet() []rune {
	if f.emoji {
		return fwGlyphsUnicode
	}
	return fwGlyphsASCII
}

// glyph picks a spark by brightness (dim -> bright) from the active set.
func (f *firework) glyph(br float64) rune {
	set := f.glyphSet()
	i := int(br * float64(len(set)))
	if i < 0 {
		i = 0
	} else if i > len(set)-1 {
		i = len(set) - 1
	}
	return set[i]
}

// draw renders a firework into the grid at its current frame. Fireworks are
// drawn last (in eggView), so they sit ON TOP of the egg/fanfare/credits.
func (f *firework) draw(grid [][]styledCell, w, h int) {
	switch f.stage {
	case fwRising:
		// A comet: bright head plus a few analytic samples behind it, fading.
		for k := 0; k < fwTrailLen; k++ {
			tt := f.t - float64(k)
			if tt < 0 {
				break
			}
			br := 1 - 0.24*float64(k)
			f.plot(grid, w, h, f.posX(tt), f.posY(tt), f.glyph(br), f.color(br, 0))
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

	// The full art needs room for a ~7+ row egg plus ~9 rows of sparkles/
	// credits/hint (eggLayout's gate: w >= 52, h >= 17). Below it, a bounded
	// fallback and no fireworks (nowhere safe to place them).
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
	var block [][]styledCell
	block = append(block, centerCells(eggSparkleCells(sw, f, 1), sw)) // top fanfare
	for _, row := range eggSpinCells(f, lay.eggCols, lay.eggRows) {
		block = append(block, centerCells(row, sw)) // egg body
	}
	block = append(block, centerCells(eggSparkleCells(sw, f, 2), sw)) // bottom fanfare

	titleColor := lipgloss.Color(eggSparkColors[f%len(eggSparkColors)])
	block = append(block, centerCells(plainCells("✦ "+eggZalgo("tailport", f)+" ✦", titleColor, true), sw))

	credits := []string{
		"Michael E. Gruen",
		"· The LLM Agent Fleet ·",
		"Claude Opus 4.8 · Sonnet 5 · Haiku 4.5 · Fable 5",
		eggURL,
		eggRepoURL,
		"c: copy site · g: copy repo · esc / q: back",
	}
	for i, s := range credits {
		color := lipgloss.Color(eggCreditColors[(f/2+i)%len(eggCreditColors)])
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

	// Fireworks last -> ON TOP of the egg/fanfare/credits.
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
// exposure glyph (🐣/🐦/🪹 vs ◉/●/▲) is quoted inline in the space/p/C rows,
// matching whichever marker set the caller is using (see resolveEmoji).
func keyLegendDescs(emoji bool) map[string]string {
	served, funneled, dangling := "◉", "●", "▲"
	if emoji {
		served, funneled, dangling = "🐣", "🐦", "🪹"
	}
	return map[string]string{
		"space": "Toggle tailscale serve for the selected port on/off. Once a port\nis exposed (" + served + ") its tailnet URL is shown beneath it.",
		"p":     "Funnel the selected port to the PUBLIC INTERNET via tailscale\nfunnel (" + funneled + "), behind a strong y/n confirm. Funnel is HTTPS-only and\ncan use just three public ingress ports — 443, 8443, 10000\n(auto-assigned, max three at once) — so the public port won't match\nthe local one. :22 (SSH) is refused. Press p again to drop the port\nback to tailnet-served.",
		"c":     "Copy the selected port's tailnet URL (http://<host>:<port>) to the\nclipboard, via OSC 52 so it works even over SSH (needs a terminal\nthat supports it; tmux: set -g set-clipboard on). Copies even when\nthe port isn't exposed yet — the toast says so.",
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

// KeyLegendGroups returns the full keybinding legend grouped into the same five
// sections, in the same order, as the bottom-bar grid -- Expose, Favorites,
// Protect, View, App -- each row carrying the RICH prose (keyLegendDescs), not
// the terse bar label. The sections and their membership are taken from
// keyMap.groups(), the one grouping source, so the "?" overlay and the bar
// cannot diverge.
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

// markerLegend describes the exposure-state glyph column, using whichever
// marker set (emoji or ASCII) is active, plus the always-present lock/favorite.
func (m model) markerLegend() string {
	if m.emoji {
		return "🥚 idle   🐣 tailnet-served   🐦 public (funnel)   🪹 served, nothing listening\n" +
			"🔒 locked   ★ favorite"
	}
	return "○ idle   ◉ tailnet-served   ● public (funnel)   ▲ served, nothing listening\n" +
		"🔒 locked   ★ favorite"
}

func (m model) helpView() string {
	var b strings.Builder

	b.WriteString(helpTitleStyle.Render("tailport — expose local ports across your tailnet"))
	b.WriteString("\n\n")
	b.WriteString(helpTextStyle.Render(
		"tailport lists the TCP ports listening on this machine and toggles\n" +
			"`tailscale serve` on or off for each one. An exposed port is reachable\n" +
			"by your other tailnet devices over plain HTTP at http://<host>:<port> —\n" +
			"tailnet-only and a 1:1 port mapping (same port in and out). A port can\n" +
			"also be funnelled to the PUBLIC internet with `p` (opt-in, see below)."))
	b.WriteString("\n\n")
	b.WriteString(helpTitleStyle.Render("Markers"))
	b.WriteString("\n")
	b.WriteString(helpTextStyle.Render(m.markerLegend()))
	b.WriteString("\n\n")
	// Keys, grouped into the same five sections/order as the bottom-bar grid
	// (Expose, Favorites, Protect, View, App) -- each section's own header stands
	// in for the old flat "Keys" title -- but keeping the rich per-key prose.
	b.WriteString(RenderKeyLegendGroups(KeyLegendGroups(m.emoji)))

	b.WriteString("\n")
	b.WriteString(warnStyle.Render(
		"Toggling port :22 (SSH) asks for a y/n confirmation first, in both\n" +
			"directions — turning serve off for :22 can drop your live SSH session."))
	b.WriteString("\n\n")
	// Dangling-forward glyph, same resolution as keyLegendDescs' inline "C" row
	// glyph, quoted again here since this paragraph sits outside that legend.
	dangling := "▲"
	if m.emoji {
		dangling = "🪹"
	}
	b.WriteString(helpTextStyle.Render(
		"A port marked " + dangling + " (its row reads \"bound to tailscale …\") is a\n" +
			"dangling forward: served, but no local process holds it. If your app\n" +
			"won't start with \"address already in use\", it's binding 0.0.0.0:<port>,\n" +
			"which collides with tailscale's serve listener on that port — bind it to\n" +
			"127.0.0.1:<port> instead (what serve proxies to, and off your LAN). Or\n" +
			"release it: space on the row, or C to clear all stale forwards."))
	b.WriteString("\n\n")
	for _, line := range configSaveLines(m.configPath) {
		b.WriteString(helpTextStyle.Render(line) + "\n")
	}

	b.WriteString("\n")
	b.WriteString(helpStyle.Render("press ? / esc / q to close"))
	return b.String()
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
// internet. The single word "exposed" (which conflated serve forwards with
// tailnet reachability) is qualified to "exposed on tailnet"; "public" is the
// funnel count, real since yt69. It abbreviates on narrow terminals rather
// than overflowing the line.
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

	// The public count is m.funnel -- ports exposed publicly via `tailscale
	// funnel`, not an independent public bind -- so it's labelled "public
	// (funnel)" to name the mechanism (67zk) and pair with "exposed on
	// tailnet".
	full := fmt.Sprintf("%d listening · %d exposed on tailnet · %d public (funnel)", listening, tailnet, public)
	// The host rides the "listening" segment ("N listening on <host>") rather
	// than a trailing "— <host>" (20w6); the other segments are unchanged.
	withHost := full
	if m.host != "" {
		withHost = fmt.Sprintf("%d listening on %s · %d exposed on tailnet · %d public (funnel)", listening, m.host, tailnet, public)
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
