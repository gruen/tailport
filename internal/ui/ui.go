// Package ui implements tailport's Bubble Tea TUI: a list of locally
// listening ports, toggled on/off tailnet-wide via tailscale serve.
package ui

import (
	"fmt"
	"io"
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
	// publicStyle marks a port funnelled to the public internet -- deliberately
	// a hot magenta, distinct from the green ● tailnet-serve and amber ▲
	// dangling markers, so "this is on the public internet" reads at a glance.
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

func (i portItem) Title() string {
	marker := "○"
	if i.active {
		marker = activeStyle.Render("●")
		if !i.listening {
			// Dangling forward: exposed via serve, but nothing is bound
			// locally, so a tailnet peer hitting the URL gets connection
			// refused. Flag it distinctly rather than as a healthy ●.
			marker = warnStyle.Render("▲")
		}
	}
	if i.funnelPublic != 0 {
		// Public/funnel outranks serve state: a hot ◉ so it's unmistakable
		// this port is reachable from the open internet, not just the tailnet.
		marker = publicStyle.Render("◉")
	}
	lock := ""
	if i.meta.Locked {
		lock = " " + lockStyle.Render("🔒")
	}
	star := ""
	if i.meta.Favorite {
		star = favStyle.Render("★") + " "
	}
	name := i.meta.Label
	if name == "" {
		name = i.port.Process
	}
	if name == "" {
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
			return warnStyle.Render("exposed, nothing listening")
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
	fqdn   string
	err    error
}

type toggleDoneMsg struct {
	port int
	err  error
}

type cleanupDoneMsg struct{ err error }

// flashExpireMsg clears the transient toast if it's still the one that
// scheduled this expiry (matched by id), so a newer toast isn't cut short.
type flashExpireMsg struct{ id int }

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
	// flash is a transient toast shown in the bottom bar (e.g. after "c"
	// copies a URL). Unlike err it isn't red; flashWarn tints it amber for a
	// caveat ("port NOT exposed"). It clears on the next keypress or when a
	// matching flashExpireMsg (tagged with flashID) fires after a short delay.
	flash     string
	flashWarn bool
	flashID   int
	err       error
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

	return model{list: l, help: h, keys: newKeyMap(), cfg: cfg, host: host, active: map[int]bool{}, portInput: ti, labelInput: li, sshInput: si}
}

func Run(cfg config.Config) error {
	_, err := tea.NewProgram(New(cfg), tea.WithAltScreen()).Run()
	return err
}

func (m model) Init() tea.Cmd {
	return refresh
}

func refresh() tea.Msg {
	ports, err := portscan.List()
	if err != nil {
		return refreshMsg{err: err}
	}
	activeList, err := tsserve.ActivePorts()
	if err != nil {
		return refreshMsg{err: err}
	}
	active := make(map[int]bool, len(activeList))
	for _, p := range activeList {
		active[p] = true
	}
	funnel, err := tsserve.FunnelStatus()
	if err != nil {
		return refreshMsg{err: err}
	}
	// FQDN is best-effort: a failure here shouldn't blank the whole view, it
	// only degrades the public URL to a hostless form, so the error is dropped.
	fqdn, _ := tsserve.FQDN()
	return refreshMsg{ports: ports, active: active, funnel: funnel, fqdn: fqdn}
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
		flash = m.setFlash("copied ✓  "+url, false)
	} else {
		flash = m.setFlash(fmt.Sprintf("copied — :%d not exposed on tailnet (toggle it on first)", sel.port.Number), true)
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

// setFlash shows a transient toast (warn tints it amber) and returns the
// command that expires it after a short delay. flashID tags the expiry so a
// newer toast supersedes rather than being cut short by an older timer.
func (m *model) setFlash(text string, warn bool) tea.Cmd {
	m.flashID++
	m.flash = text
	m.flashWarn = warn
	id := m.flashID
	return tea.Tick(3*time.Second, func(time.Time) tea.Msg { return flashExpireMsg{id: id} })
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
func (m *model) remember(port int) {
	if m.cfg.Ports == nil {
		m.cfg.Ports = map[int]config.PortMeta{}
	}
	if _, ok := m.cfg.Ports[port]; ok {
		return
	}
	m.cfg.Ports[port] = config.PortMeta{}
	if err := m.cfg.Save(); err != nil {
		m.err = err
	}
}

// favorite registers port with Favorite=true, preserving any existing label
// or lock, and persists it. Backs the "n" add-port flow (ykgj): the port
// sticks in the Favorites view even before its service is running, ready to be
// served with space once it is.
func (m *model) favorite(port int) {
	if m.cfg.Ports == nil {
		m.cfg.Ports = map[int]config.PortMeta{}
	}
	meta := m.cfg.Ports[port]
	meta.Favorite = true
	m.cfg.Ports[port] = meta
	if err := m.cfg.Save(); err != nil {
		m.err = err
	}
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
		m.err = fmt.Errorf("port :%d is locked -- press x to unlock", port)
		return nil
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
	if turnOn {
		m.remember(port)
	}
	m.pending = port
	return toggle(port, turnOn)
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
		m.err = fmt.Errorf("refusing to funnel :22 (SSH) to the public internet")
		return nil
	}
	pub, ok := m.nextFunnelPort()
	if !ok {
		m.err = fmt.Errorf("all funnel ingress ports (443, 8443, 10000) are in use -- tailscale allows at most three")
		return nil
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
	if turnOn {
		m.remember(localPort)
	}
	m.pending = localPort
	return funnelCmd(localPort, publicPort, turnOn)
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
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.allPorts = msg.ports
		m.active = msg.active
		m.funnel = msg.funnel
		if msg.fqdn != "" {
			m.fqdn = msg.fqdn
		}
		return m, m.rebuildItems()

	case toggleDoneMsg:
		m.pending = 0
		if msg.err != nil {
			// Keep the error visible: a failed toggle/funnel changed no state,
			// and a follow-up refresh would immediately clear m.err (refreshMsg
			// resets it), swallowing messages like the funnel "not enabled"
			// guidance. The next user action refreshes.
			m.err = msg.err
			return m, nil
		}
		return m, refresh

	case cleanupDoneMsg:
		m.cleaning = 0
		if msg.err != nil {
			m.err = msg.err
		}
		return m, refresh

	case flashExpireMsg:
		// Only clear if no newer toast has replaced this one (see flashID).
		if msg.id == m.flashID {
			m.flash = ""
			m.flashWarn = false
		}
		return m, nil

	case tea.KeyMsg:
		// Any keypress dismisses a lingering toast (a fresh one is set below if
		// this key produces it), so the flash never sticks around stale.
		m.flash = ""
		m.flashWarn = false
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
						m.err = fmt.Errorf("invalid port")
						return m, nil
					}
					// "n" registers + favorites the port; it does NOT serve
					// (ykgj). Exposing is always space. A not-yet-running
					// favorite then shows in the Favorites view as a synthetic
					// entry, ready to serve once its service is up -- so an
					// added port sticks instead of vanishing. No lock/:22 guard
					// is needed here since nothing is exposed.
					m.favorite(port)
					return m, m.rebuildItems()
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
						if err := m.cfg.Save(); err != nil {
							m.err = err
						}
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
					if err := m.cfg.Save(); err != nil {
						m.err = err
					}
					return m, m.rebuildItems()
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
			// there's nothing to clean (setting m.err here would render red,
			// like a failure).
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
			if err := m.cfg.Save(); err != nil {
				m.err = err
			}
			return m, m.rebuildItems()
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
				if err := m.cfg.Save(); err != nil {
					m.err = err
				}
				return m, m.rebuildItems()
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
			if err := m.cfg.Save(); err != nil {
				m.err = err
			}
			return m, m.rebuildItems()
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
			items = append(items, portItem{port: p, active: m.active[n], listening: ok, host: m.host, fqdn: m.fqdn, funnelPublic: m.funnel[n], dimmed: dimNonFav && !meta.Favorite, meta: meta})
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
		items = append(items, portItem{port: p, active: m.active[n], listening: ok, host: m.host, fqdn: m.fqdn, funnelPublic: m.funnel[n], meta: m.cfg.Ports[n]})
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

// helpView renders the full-screen "?" overlay: a short intro to what
// tailport is, then a real explanation of every key (not the terse legend).
// It replaces the whole View while m.showHelp is set.
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
	b.WriteString(helpTitleStyle.Render("Keys"))
	b.WriteString("\n")

	rows := []struct{ key, desc string }{
		{"space", "Toggle tailscale serve for the selected port on/off. Once a port\nis exposed (●) its tailnet URL is shown beneath it."},
		{"p", "Funnel the selected port to the PUBLIC INTERNET via tailscale\nfunnel (◉), behind a strong y/n confirm. Funnel is HTTPS-only and\ncan use just three public ingress ports — 443, 8443, 10000\n(auto-assigned, max three at once) — so the public port won't match\nthe local one. :22 (SSH) is refused. Press p again to drop the port\nback to tailnet-served."},
		{"a", "Switch between the two list views: Favorites (only ★ ports) and\nAll ports (every port listening locally, plus your favorites even\nwhen their process is down)."},
		{"f", "Favorite the selected port (marks it ★). Favorites are a durable\nshortlist — one of the two `a` views — that survives restarts and\nstays visible even when the process isn't running."},
		{"u", "Unfavorite (clears ★); the port drops out of the Favorites view."},
		{"x", "Lock / unlock the selected port (🔒). A locked port can't be\ntoggled on until you unlock it — a guard against exposing something\nby accident. Port :22 is locked by default; unlocking it requires\ntyping \"ssh\" to confirm (it guards your SSH access)."},
		{"n", "Add a port by number to Favorites (★), even one not currently\nlistening. It doesn't serve — it just registers and sticks in the\nFavorites view; press space there to serve it once its service is up."},
		{"l", "Set a text label for the selected port."},
		{"r", "Refresh the port list and serve status."},
		{"/", "Filter by port number, process, or label (fuzzy). Searches ALL\nlistening ports regardless of view, so it works even from an empty\nFavorites screen; non-favorite matches show dimmed in the Favorites\nview. esc clears the filter."},
		{"c", "Copy the selected port's tailnet URL (http://<host>:<port>) to the\nclipboard, via OSC 52 so it works even over SSH (needs a terminal\nthat supports it; tmux: set -g set-clipboard on). Copies even when\nthe port isn't exposed yet — the toast says so."},
		{"C", "Tear down stale forwards — ports still served by tailscale with\nnothing listening locally (shown ▲). Offered only when some exist."},
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
	// A transient toast (e.g. "copied ✓ ...") takes the status line's spot
	// while it's showing; otherwise the normal breakdown. flashWarn tints an
	// amber caveat; a plain toast is green.
	statusLine := helpStyle.Render(m.statusText())
	if m.flash != "" {
		toast := activeStyle
		if m.flashWarn {
			toast = warnStyle
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
	if m.err != nil {
		body += errStyle.Render("error: "+m.err.Error()) + "\n"
	}
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
