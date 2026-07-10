// Package ui implements tailport's Bubble Tea TUI: a list of locally
// listening ports, toggled on/off tailnet-wide via tailscale serve.
package ui

import (
	"fmt"
	"os"
	"strconv"

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
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	helpStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
)

type portItem struct {
	port   portscan.Port
	active bool
	host   string
}

func (i portItem) Title() string {
	marker := "○"
	if i.active {
		marker = activeStyle.Render("●")
	}
	proc := i.port.Process
	if proc == "" {
		proc = "?"
	}
	return fmt.Sprintf("%s :%d  %s", marker, i.port.Number, proc)
}

func (i portItem) Description() string {
	if i.active {
		return fmt.Sprintf("http://%s:%d", i.host, i.port.Number)
	}
	return "not exposed"
}

func (i portItem) FilterValue() string {
	return fmt.Sprintf("%d %s", i.port.Number, i.port.Process)
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

type model struct {
	list    list.Model
	cfg     config.Config
	host    string
	showAll bool

	allPorts []portscan.Port
	active   map[int]bool

	entering  bool // manual port-entry mode is active
	portInput textinput.Model

	pending int // port currently being toggled; 0 = none
	err     error
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

	return model{list: l, cfg: cfg, host: host, active: map[int]bool{}, portInput: ti}
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

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.list.SetSize(msg.Width, msg.Height-2)
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

	case tea.KeyMsg:
		if m.entering {
			switch msg.String() {
			case "esc":
				m.entering = false
				m.portInput.Reset()
				return m, nil
			case "enter":
				port, err := strconv.Atoi(m.portInput.Value())
				m.entering = false
				m.portInput.Reset()
				if err != nil || port < 1 || port > 65535 {
					m.err = fmt.Errorf("invalid port")
					return m, nil
				}
				if m.pending != 0 {
					return m, nil
				}
				m.pending = port
				return m, toggle(port, !m.active[port])
			}
			var cmd tea.Cmd
			m.portInput, cmd = m.portInput.Update(msg)
			return m, cmd
		}

		if m.list.FilterState() == list.Filtering {
			break // let bubbles/list's own "/" filter input consume the keys
		}

		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "a":
			m.showAll = !m.showAll
			m.rebuildItems()
			return m, nil
		case "r":
			return m, refresh
		case "n":
			m.entering = true
			m.portInput.Focus()
			return m, nil
		case "enter", " ":
			if m.pending != 0 {
				return m, nil // a toggle is already in flight
			}
			sel, ok := m.list.SelectedItem().(portItem)
			if !ok {
				return m, nil
			}
			m.pending = sel.port.Number
			return m, toggle(sel.port.Number, !sel.active)
		}
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m *model) rebuildItems() {
	items := make([]list.Item, 0, len(m.allPorts))
	for _, p := range m.allPorts {
		active := m.active[p.Number]
		if !m.showAll && !active && m.cfg.Excludes(p.Number) {
			continue
		}
		items = append(items, portItem{port: p, active: active, host: m.host})
	}
	m.list.SetItems(items)
}

func (m model) View() string {
	var b string
	if m.err != nil {
		b += errStyle.Render("error: "+m.err.Error()) + "\n"
	}
	b += m.list.View()

	if m.entering {
		b += "\n" + helpStyle.Render("expose port: ") + m.portInput.View() + helpStyle.Render("  (enter: confirm, esc: cancel)")
		return b
	}

	filter := "filtered"
	if m.showAll {
		filter = "all ports"
	}
	status := "ready"
	if m.pending != 0 {
		status = fmt.Sprintf("toggling :%d...", m.pending)
	}
	b += "\n" + helpStyle.Render(fmt.Sprintf("enter: toggle  n: new port  a: %s  r: refresh  q: quit  [%s]", filter, status))
	return b
}
