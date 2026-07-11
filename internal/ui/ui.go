// Package ui implements tailport's Bubble Tea TUI: a list of locally
// listening ports, toggled on/off tailnet-wide via tailscale serve.
package ui

import (
	"fmt"
	"io"
	"math"
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

// ShortHelp and FullHelp return the same flat list: the legend doesn't
// distinguish a "short" vs "full" mode, it always shows every binding and
// relies on width-based wrapping (see wrapBindings) instead of truncation.
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{
		k.Toggle, k.Funnel, k.Filter, k.Copy, k.NewPort, k.Label, k.Favorite,
		k.Unfavorite, k.Lock, k.ShowAll, k.Clean, k.Refresh, k.Help, k.Quit,
	}
}

func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{k.ShortHelp()}
}

func newKeyMap() keyMap {
	return keyMap{
		Toggle: key.NewBinding(key.WithKeys(" "), key.WithHelp("space", "toggle")),
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
			// Dangling forward: served, but nothing is on the loopback target.
			// The usual cause is the app binding 0.0.0.0:<port>, which collides
			// with tailscaled's tailnet-IP serve listener on the same port -- so
			// point at the fix (bind loopback) and the un-expose key (space acts
			// on this row; C batch-cleans all stale). Full story in ? help / README.
			return warnStyle.Render(fmt.Sprintf(
				"nothing on 127.0.0.1:%d — bind app to loopback, or space to un-expose",
				i.port.Number))
		}
		return fmt.Sprintf("http://%s:%d", i.host, i.port.Number)
	}
	return "not exposed"
}

func (i portItem) FilterValue() string {
	return fmt.Sprintf("%d %s %s", i.port.Number, i.port.Process, i.meta.Label)
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
	// New() from config.Path() so the help overlay can state it exactly --
	// honoring XDG_CONFIG_HOME rather than guessing (gahj). Empty if
	// config.Path() errored, in which case helpView describes the rule instead.
	configPath string
	// emoji selects the egg-lifecycle exposure markers over the ASCII fallback,
	// resolved once at New() from cfg.Markers + the terminal's capabilities.
	emoji bool
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

func New(cfg config.Config) model {
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
	// show exactly where settings live, XDG override and all. On error we leave
	// it empty and helpView falls back to describing the rule.
	configPath, _ := config.Path()

	return model{list: l, help: h, keys: newKeyMap(), cfg: cfg, host: host, active: map[int]bool{}, portInput: ti, labelInput: li, sshInput: si, configPath: configPath, emoji: resolveEmoji(cfg.Markers)}
}

func Run(cfg config.Config) error {
	_, err := tea.NewProgram(New(cfg), tea.WithAltScreen()).Run()
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
)

// eggTick schedules the next Easter-egg animation frame. It is only ever
// rescheduled while showEgg is true (see the eggTickMsg handler), so closing
// the overlay stops the ticker -- no leaked goroutine, no busy loop.
func eggTick() tea.Cmd {
	return tea.Tick(eggInterval, func(time.Time) tea.Msg { return eggTickMsg{} })
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
		legendLines := 1
		if legend := m.renderLegend(); legend != "" {
			legendLines = strings.Count(legend, "\n") + 1
		}
		// Reserve the persistent top header (one row) plus the bottom bar: one
		// blank separator, the status line, and the wrapped shortcuts legend.
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
				return m, nil
			case "ctrl+c":
				return m, tea.Quit
			case "c":
				return m, m.eggCopy(eggURL, eggDomain)
			case "g":
				return m, m.eggCopy(eggRepoURL, eggRepoDomain)
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

// renderLegend renders the keybinding legend, wrapping onto additional
// lines as needed to fit m.help.Width rather than truncating with an
// ellipsis (bubbles/help's own ShortHelpView/FullHelpView only truncate,
// they don't wrap, so the packing is done by hand in wrapBindings).
func (m model) renderLegend() string {
	keys := m.keys
	// The active view is shown by the segmented indicator (renderViewIndicator),
	// so "a" is just labeled with its action.
	keys.ShowAll.SetHelp("a", "switch view")
	// "clean stale" only makes sense when there's something stale to clean;
	// wrapBindings skips disabled bindings, so it drops out otherwise.
	keys.Clean.SetEnabled(m.hasDangling())
	return wrapBindings(m.help.Styles, keys.ShortHelp(), m.help.Width, m.help.ShortSeparator)
}

// wrapBindings greedily packs "key desc" items onto lines, starting a new
// line whenever the next item would overflow width (a width <= 0 means
// unbounded, matching bubbles/help's own convention). This is what gives
// the legend a stacked, multi-line layout in narrow terminals instead of
// bubbles/help's default single-line-plus-ellipsis truncation.
func wrapBindings(styles help.Styles, bindings []key.Binding, width int, sep string) string {
	if len(bindings) == 0 {
		return ""
	}
	renderedSep := styles.ShortSeparator.Inline(true).Render(sep)
	sepWidth := lipgloss.Width(renderedSep)

	var lines []string
	var cur strings.Builder
	curWidth := 0
	for _, kb := range bindings {
		if !kb.Enabled() {
			continue
		}
		item := styles.ShortKey.Inline(true).Render(kb.Help().Key) + " " + styles.ShortDesc.Inline(true).Render(kb.Help().Desc)
		itemWidth := lipgloss.Width(item)

		addWidth := itemWidth
		if curWidth > 0 {
			addWidth += sepWidth
		}
		if width > 0 && curWidth > 0 && curWidth+addWidth > width {
			lines = append(lines, cur.String())
			cur.Reset()
			curWidth = 0
		}

		if curWidth > 0 {
			cur.WriteString(renderedSep)
			curWidth += sepWidth
		}
		cur.WriteString(item)
		curWidth += itemWidth
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

	out := make([]string, h)
	for y := 0; y < h; y++ {
		half := halves[y]
		if half > maxHalf {
			half = maxHalf
		}
		var b strings.Builder
		for x := 0; x < w; x++ {
			if x < cx-half || x > cx+half {
				b.WriteByte(' ')
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
			st := lipgloss.NewStyle().Foreground(col).Bold(true)
			b.WriteString(st.Render(string(ch)))
		}
		out[y] = b.String()
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
	if w > 56 {
		w = 56
	}
	var b strings.Builder
	for x := 0; x < w; x++ {
		if (x*29+frame*13+salt*7)%9 == 0 {
			c := eggSparkColors[(x+frame+salt)%len(eggSparkColors)]
			s := eggSparks[(x*3+frame)%len(eggSparks)]
			b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(c)).Render(string(s)))
		} else {
			b.WriteRune(' ')
		}
	}
	return b.String()
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

// eggView renders the full-screen Easter egg for the current frame, centred
// and padded to exactly m.width x m.height. Tiny terminals get a bounded
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
	// credits/hint. The widest credit line is 48 cols, so require sw >= 48
	// (w >= 52) to keep everything untruncated; below that (or too short), a
	// bounded fallback rather than any overflow.
	if w < 52 || h < 17 {
		msg := lipgloss.NewStyle().Foreground(lipgloss.Color(eggGold[f%len(eggGold)])).Bold(true).
			Render("🥚 enlarge the terminal — esc: back")
		return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, msg)
	}

	sw := w - 4
	cred := func(i int, s string) string {
		c := eggCreditColors[(f/2+i)%len(eggCreditColors)]
		return lipgloss.NewStyle().Foreground(lipgloss.Color(c)).Render(s)
	}
	title := lipgloss.NewStyle().Foreground(lipgloss.Color(eggSparkColors[f%len(eggSparkColors)])).Bold(true).
		Render("✦ " + eggZalgo("tailport", f) + " ✦")

	// Size the egg to the remaining vertical budget (9 non-egg rows) and a
	// pleasing width, clamped so it never overflows.
	eggRows := h - 10
	if eggRows > 15 {
		eggRows = 15
	}
	eggCols := sw
	if eggCols > 21 {
		eggCols = 21
	}

	var lines []string
	lines = append(lines, eggSparkleLine(sw, f, 1))
	lines = append(lines, eggSpin(f, eggCols, eggRows)...)
	lines = append(lines, eggSparkleLine(sw, f, 2))
	lines = append(lines, title)
	lines = append(lines,
		cred(0, "Michael E. Gruen"),
		cred(1, "· The LLM Agent Fleet ·"),
		cred(2, "Claude Opus 4.8 · Sonnet 5 · Haiku 4.5 · Fable 5"),
		cred(3, eggURL),
		cred(4, eggRepoURL),
		lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("c: copy site · g: copy repo · esc / q: back"),
	)
	if m.flash != "" {
		lines = append(lines, activeStyle.Render(m.flash))
	}
	// Safety net: never exceed the viewport height.
	if len(lines) > h {
		lines = lines[:h]
	}
	for i, ln := range lines {
		lines[i] = lipgloss.PlaceHorizontal(sw, lipgloss.Center, ln)
	}
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, strings.Join(lines, "\n"))
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
	b.WriteString(helpTitleStyle.Render("Keys"))
	b.WriteString("\n")

	// Exposure glyph shown inline in the key descriptions below, matching the
	// active marker set so "exposed (X)" reads correctly in either mode.
	served, funneled, dangling := "◉", "●", "▲"
	if m.emoji {
		served, funneled, dangling = "🐣", "🐦", "🪹"
	}

	rows := []struct{ key, desc string }{
		{"space", "Toggle tailscale serve for the selected port on/off. Once a port\nis exposed (" + served + ") its tailnet URL is shown beneath it."},
		{"p", "Funnel the selected port to the PUBLIC INTERNET via tailscale\nfunnel (" + funneled + "), behind a strong y/n confirm. Funnel is HTTPS-only and\ncan use just three public ingress ports — 443, 8443, 10000\n(auto-assigned, max three at once) — so the public port won't match\nthe local one. :22 (SSH) is refused. Press p again to drop the port\nback to tailnet-served."},
		{"a", "Switch between the two list views: Favorites (only ★ ports) and\nAll ports (every port listening locally, plus your favorites even\nwhen their process is down)."},
		{"f", "Favorite the selected port (marks it ★). Favorites are a durable\nshortlist — one of the two `a` views — that survives restarts and\nstays visible even when the process isn't running."},
		{"u", "Unfavorite (clears ★); the port drops out of the Favorites view."},
		{"x", "Lock / unlock the selected port (🔒). A locked port can't be\ntoggled on until you unlock it — a guard against exposing something\nby accident. Port :22 is locked by default; unlocking it requires\ntyping \"ssh\" to confirm (it guards your SSH access)."},
		{"n", "Add a port by number to Favorites (★), even one not currently\nlistening. It doesn't serve — it just registers and sticks in the\nFavorites view; press space there to serve it once its service is up."},
		{"l", "Set a text label for the selected port."},
		{"r", "Refresh the port list and serve status."},
		{"/", "Filter by port number, process, or label (fuzzy). Searches ALL\nlistening ports regardless of view, so it works even from an empty\nFavorites screen; non-favorite matches show dimmed in the Favorites\nview. esc clears the filter."},
		{"c", "Copy the selected port's tailnet URL (http://<host>:<port>) to the\nclipboard, via OSC 52 so it works even over SSH (needs a terminal\nthat supports it; tmux: set -g set-clipboard on). Copies even when\nthe port isn't exposed yet — the toast says so."},
		{"C", "Tear down stale forwards — ports still served by tailscale with\nnothing listening locally (shown " + dangling + "). Offered only when some exist."},
		{"?", "Toggle this help. esc or q also close it."},
		{"q", "Quit."},
	}
	for _, r := range rows {
		lines := strings.Split(r.desc, "\n")
		b.WriteString("  " + helpKeyStyle.Render(fmt.Sprintf("%-6s", r.key)) + "  " + helpTextStyle.Render(lines[0]) + "\n")
		for _, extra := range lines[1:] {
			b.WriteString("          " + helpTextStyle.Render(extra) + "\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(warnStyle.Render(
		"Toggling port :22 (SSH) asks for a y/n confirmation first, in both\n" +
			"directions — turning serve off for :22 can drop your live SSH session."))
	b.WriteString("\n\n")
	b.WriteString(helpTextStyle.Render(
		"A served port showing \"nothing listening\" is a dangling forward: the\n" +
			"serve mapping is up but no local process holds it. If your app won't\n" +
			"start with \"address already in use\", it's binding 0.0.0.0:<port>, which\n" +
			"collides with tailscale's serve listener on that port — bind it to\n" +
			"127.0.0.1:<port> instead (what serve proxies to, and not exposed on your\n" +
			"LAN). Or un-expose the port first: space on the row, or C to clear all."))
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
