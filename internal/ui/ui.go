// Package ui implements tailport's Bubble Tea TUI: a list of locally
// listening ports, toggled on/off tailnet-wide via tailscale serve.
package ui

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
)

// keyMap describes every keybinding the TUI responds to, for the bubbles/help
// legend. It satisfies help.KeyMap. Most bindings carry static help text;
// ShowAll's Desc is refreshed on each render to reflect the current view
// (favorites vs all ports; see model.renderLegend).
type keyMap struct {
	Toggle     key.Binding
	NewPort    key.Binding
	Label      key.Binding
	Favorite   key.Binding
	Unfavorite key.Binding
	Lock       key.Binding
	ShowAll    key.Binding
	Clean      key.Binding
	Refresh    key.Binding
	Quit       key.Binding
}

// ShortHelp and FullHelp return the same flat list: the legend doesn't
// distinguish a "short" vs "full" mode, it always shows every binding and
// relies on width-based wrapping (see wrapBindings) instead of truncation.
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{
		k.Toggle, k.NewPort, k.Label, k.Favorite, k.Unfavorite,
		k.Lock, k.ShowAll, k.Clean, k.Refresh, k.Quit,
	}
}

func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{k.ShortHelp()}
}

func newKeyMap() keyMap {
	return keyMap{
		Toggle:     key.NewBinding(key.WithKeys(" "), key.WithHelp("space", "toggle")),
		NewPort:    key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "new port")),
		Label:      key.NewBinding(key.WithKeys("l"), key.WithHelp("l", "label")),
		Favorite:   key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "favorite")),
		Unfavorite: key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "unfavorite")),
		Lock:       key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "lock/unlock")),
		ShowAll:    key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "filtered")),
		Clean:      key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "clean stale")),
		Refresh:    key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		Quit:       key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

type portItem struct {
	port      portscan.Port
	active    bool
	listening bool
	host      string
	meta      config.PortMeta
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
	err    error
}

type toggleDoneMsg struct {
	port int
	err  error
}

type cleanupDoneMsg struct{ err error }

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
)

type model struct {
	list    list.Model
	help    help.Model
	keys    keyMap
	cfg     config.Config
	host    string
	// showAllPorts selects the list view: false = Favorites (only ports
	// marked meta.Favorite), true = All ports (every currently-listening
	// port). Toggled by "a".
	showAllPorts bool

	allPorts []portscan.Port
	active   map[int]bool

	mode       entryMode
	portInput  textinput.Model
	labelInput textinput.Model
	labelPort  int // port being labeled while mode == entryLabel

	pending      int   // port currently being toggled; 0 = none
	cleaning     int   // number of dangling forwards being torn down; 0 = none
	cleanTargets []int // ports the entryConfirmClean prompt is asking about
	err          error
}

func New(cfg config.Config) model {
	host, _ := os.Hostname()

	l := list.New(nil, list.NewDefaultDelegate(), 0, 0)
	l.Title = "tailport"
	l.SetShowHelp(false)

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

	if cfg.Ports == nil {
		cfg.Ports = map[int]config.PortMeta{}
	}

	h := help.New()

	return model{list: l, help: h, keys: newKeyMap(), cfg: cfg, host: host, active: map[int]bool{}, portInput: ti, labelInput: li}
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
	return refreshMsg{ports: ports, active: active}
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

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.help.Width = msg.Width
		legendLines := 1
		if legend := m.renderLegend(); legend != "" {
			legendLines = strings.Count(legend, "\n") + 1
		}
		// Reserve one row for the blank line separating the list from the
		// legend, plus one more for the status line below it.
		m.list.SetSize(msg.Width, msg.Height-legendLines-2)
		return m, nil

	case refreshMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.allPorts = msg.ports
		m.active = msg.active
		m.rebuildItems()
		return m, nil

	case toggleDoneMsg:
		m.pending = 0
		if msg.err != nil {
			m.err = msg.err
		}
		return m, refresh

	case cleanupDoneMsg:
		m.cleaning = 0
		if msg.err != nil {
			m.err = msg.err
		}
		return m, refresh

	case tea.KeyMsg:
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
			switch msg.String() {
			case "esc":
				m.mode = entryNone
				m.portInput.Reset()
				m.labelInput.Reset()
				m.labelPort = 0
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
					if m.pending != 0 {
						return m, nil
					}
					turnOn := !m.active[port]
					if turnOn && m.cfg.Ports[port].Locked {
						m.err = fmt.Errorf("port :%d is locked -- press x to unlock", port)
						return m, nil
					}
					if turnOn {
						m.remember(port)
					}
					m.pending = port
					return m, toggle(port, turnOn)
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
					m.rebuildItems()
					return m, nil
				}
			}
			var cmd tea.Cmd
			if m.mode == entryAddPort {
				m.portInput, cmd = m.portInput.Update(msg)
			} else {
				m.labelInput, cmd = m.labelInput.Update(msg)
			}
			return m, cmd
		}

		if m.list.FilterState() == list.Filtering {
			break // let bubbles/list's own "/" filter input consume the keys
		}

		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
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
			m.rebuildItems()
			if cur != 0 {
				m.selectPort(cur)
			}
			return m, nil
		case "r":
			return m, refresh
		case "c":
			// Batch-tear-down of dangling forwards, behind a y/n confirm.
			// No-op while a toggle or cleanup is already in flight, and a
			// silent no-op when there's nothing to clean (setting m.err here
			// would render red, like a failure).
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
			m.labelInput.SetValue(sel.port.Process)
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
			m.rebuildItems()
			return m, nil
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
				m.rebuildItems()
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
			meta.Locked = !meta.Locked
			m.cfg.Ports[sel.port.Number] = meta
			if err := m.cfg.Save(); err != nil {
				m.err = err
			}
			m.rebuildItems()
			return m, nil
		case " ":
			if m.pending != 0 {
				return m, nil // a toggle is already in flight
			}
			sel, ok := m.list.SelectedItem().(portItem)
			if !ok {
				return m, nil
			}
			turnOn := !sel.active
			if turnOn && sel.meta.Locked {
				m.err = fmt.Errorf("port :%d is locked -- press x to unlock", sel.port.Number)
				return m, nil
			}
			if turnOn {
				m.remember(sel.port.Number)
			}
			m.pending = sel.port.Number
			return m, toggle(sel.port.Number, turnOn)
		}
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

// rebuildItems recomputes the visible list for the current view. In the
// All ports view (showAllPorts) it shows every currently-listening port,
// full stop. In the Favorites view it shows only ports marked
// meta.Favorite, merging in synthetic entries (empty Process) for favorite
// ports that aren't currently listening locally so a favorite never
// silently disappears just because its process is down.
func (m *model) rebuildItems() {
	portsByNumber := make(map[int]portscan.Port, len(m.allPorts))
	for _, p := range m.allPorts {
		portsByNumber[p.Number] = p
	}

	if m.showAllPorts {
		numbers := make([]int, 0, len(portsByNumber))
		for n := range portsByNumber {
			numbers = append(numbers, n)
		}
		sort.Ints(numbers)
		items := make([]list.Item, 0, len(numbers))
		for _, n := range numbers {
			// This branch iterates portsByNumber, so every port here is
			// currently listening.
			items = append(items, portItem{port: portsByNumber[n], active: m.active[n], listening: true, host: m.host, meta: m.cfg.Ports[n]})
		}
		m.setItems(items)
		return
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
		items = append(items, portItem{port: p, active: m.active[n], listening: ok, host: m.host, meta: m.cfg.Ports[n]})
	}
	m.setItems(items)
}

// setItems replaces the list's items and clamps the selection if it's now
// out of range. list.Model.SetItems does not do this itself: its cursor
// index is left pointing past the end whenever the item count shrinks
// (e.g. switching from the All ports view back to Favorites, or a refresh
// that drops a since-unregistered port), which makes SelectedItem() return
// nil and silently turns every selection-based key (space, l, f, u) into a
// no-op.
func (m *model) setItems(items []list.Item) {
	m.list.SetItems(items)
	if idx := m.list.Index(); len(items) > 0 && (idx < 0 || idx >= len(items)) {
		m.list.Select(len(items) - 1)
	}
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
	view := "favorites"
	if m.showAllPorts {
		view = "all ports"
	}
	keys.ShowAll.SetHelp("a", view)
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

func (m model) View() string {
	var b string
	if m.err != nil {
		b += errStyle.Render("error: "+m.err.Error()) + "\n"
	}
	b += m.list.View()

	switch m.mode {
	case entryAddPort:
		b += "\n" + helpStyle.Render("expose port: ") + m.portInput.View() + helpStyle.Render("  (enter: confirm, esc: cancel)")
		return b
	case entryLabel:
		b += "\n" + helpStyle.Render(fmt.Sprintf("label :%d: ", m.labelPort)) + m.labelInput.View() + helpStyle.Render("  (enter: confirm, esc: cancel)")
		return b
	case entryConfirmClean:
		targets := make([]string, len(m.cleanTargets))
		for i, p := range m.cleanTargets {
			targets[i] = strconv.Itoa(p)
		}
		b += "\n" + helpStyle.Render(fmt.Sprintf("tear down forwards on :%s? ", strings.Join(targets, ", :"))) + helpStyle.Render("(y: confirm, any other key: cancel)")
		return b
	}

	status := "ready"
	if m.pending != 0 {
		status = fmt.Sprintf("toggling :%d...", m.pending)
	}
	if m.cleaning != 0 {
		status = fmt.Sprintf("cleaning %d stale forwards...", m.cleaning)
	}
	if legend := m.renderLegend(); legend != "" {
		b += "\n" + legend
	}
	b += "\n" + helpStyle.Render(fmt.Sprintf("[%s]", status))
	return b
}
