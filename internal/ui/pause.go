package ui

// Auto-pause / auto-resume of dangling serve forwards (kata p2r5, reworking
// 9p43's auto-clear). A plain 1:1 serve whose app stops listening is PAUSED
// after a short grace: the forward is removed (so it stops holding the port on
// the tailnet IPs, the EADDRINUSE collision 9p43 verified) and tailport
// remembers it together with the identity -- executable path + working
// directory -- of the program that was behind it. When something listens on a
// paused port again, a refresh decides: the SAME program bound to loopback is
// re-served automatically; the same program bound wide is already reachable on
// the tailnet, so the memory is dropped; anything else is never auto-shared.
//
// Identity reads (/proc, ps, lsof) are I/O, so they happen in refresh()'s cmd
// (never in Update) and travel in refreshMsg.idents; everything below is pure
// model logic over that snapshot.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gruen/tailport/internal/pausedserve"
	"github.com/gruen/tailport/internal/portscan"
	"github.com/gruen/tailport/internal/procid"
)

// identify reads a listener's identity; a var so tests stay hermetic.
var identify = procid.Of

// pausedIdentity is the remembered program of a paused serve.
func pausedIdentity(e pausedserve.Entry) procid.Identity {
	return procid.Identity{Exe: e.Exe, Cwd: e.Cwd}
}

// pausedPorts lists the paused ports (sorted): refresh() reads the identity of
// any of these that is listening, to decide a resume.
func (m model) pausedPorts() []int {
	ports := make([]int, 0, len(m.paused))
	for p := range m.paused {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	return ports
}

// pausableStale is the clearable dangling set (internal/stale's shared rule)
// narrowed to ports whose app identity is KNOWN: with no identity there is
// nothing to resume against, so such a serve is never paused.
func (m model) pausableStale() []int {
	var out []int
	for _, p := range m.clearableStale() {
		if m.lastIdent[p].Known() {
			out = append(out, p)
		}
	}
	return out
}

// noteIdentities records the last-known identity of the listener behind each
// served 1:1 port from a freshly applied refresh. A port that is no longer a
// served 1:1 forward loses its entry; a listener whose identity is unreadable
// (another user's process) clears it, so the port can never be paused on the
// strength of a previous, different program.
func (m *model) noteIdentities(msg refreshMsg) {
	if m.lastIdent == nil {
		m.lastIdent = map[int]procid.Identity{}
	}
	for p := range m.lastIdent {
		if !m.active[p] || !msg.oneToOne[p] {
			delete(m.lastIdent, p)
		}
	}
	listening := make(map[int]bool, len(msg.ports))
	for _, p := range msg.ports {
		listening[p.Number] = true
	}
	for p := range m.active {
		if !m.active[p] || !msg.oneToOne[p] || !listening[p] {
			continue
		}
		if id, ok := msg.idents[p]; ok && id.Known() {
			m.lastIdent[p] = id
		} else {
			delete(m.lastIdent, p)
		}
	}
}

// shortPath abbreviates the home directory to "~" for toasts and row text.
func shortPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && home != "/" {
		if p == home {
			return "~"
		}
		if strings.HasPrefix(p, home+string(os.PathSeparator)) {
			return "~" + p[len(home):]
		}
	}
	return p
}

// pauseToast is the toast for serves just paused.
func pauseToast(es []pausedserve.Entry) string {
	if len(es) == 1 {
		e := es[0]
		return fmt.Sprintf("paused :%d (app stopped) — resumes when %s in %s returns · t to forget", e.Port, filepath.Base(e.Exe), shortPath(e.Cwd))
	}
	ports := make([]int, len(es))
	for i, e := range es {
		ports[i] = e.Port
	}
	return fmt.Sprintf("paused %s (apps stopped) — resume when the same apps return · t to forget", portList(ports))
}

// persistPaused writes the paused memory; callers invoke it only after a real
// change, so a steady state never touches the file. Returns the error toast
// cmd (nil on success or when persistence is unavailable).
func (m *model) persistPaused() tea.Cmd {
	if err := m.savePaused(); err != nil {
		return m.setErr(err.Error())
	}
	return nil
}

// savePaused is persistPaused without the toast, for a caller that folds the
// failure into a notification of its own.
func (m *model) savePaused() error {
	if m.pausedPath == "" {
		return nil
	}
	if err := pausedserve.Save(m.pausedPath, m.paused); err != nil {
		return fmt.Errorf("could not save paused serves: %w", err)
	}
	return nil
}

// forgetPaused drops port's paused memory and persists the change.
func (m *model) forgetPaused(port int) tea.Cmd {
	if _, ok := m.paused[port]; !ok {
		return nil
	}
	delete(m.paused, port)
	delete(m.pausedOther, port)
	return m.persistPaused()
}

// resumeReturned runs after a refresh applied: it expires old entries, drops
// ones the world has overtaken, and resumes paused ports whose same program is
// listening again. Returns the cmds to run and any informational notice.
func (m *model) resumeReturned(msg refreshMsg) (cmds []tea.Cmd, notice string) {
	now := m.clock()
	if m.pausedOther == nil {
		m.pausedOther = map[int]bool{}
	}
	clear(m.pausedOther)
	byPort := make(map[int]portscan.Port, len(msg.ports))
	for _, p := range msg.ports {
		byPort[p.Number] = p
	}
	changed := false
	var already []int
	for _, port := range m.pausedPorts() {
		e := m.paused[port]
		switch {
		case e.Expired(now), m.active[port], port == 22, m.cfg.Ports[port].Locked:
			// Expired, or overtaken: someone served it meanwhile, or the user
			// locked it. Either way the memory is no longer wanted.
			delete(m.paused, port)
			changed = true
			continue
		}
		p, listening := byPort[port]
		if !listening {
			continue
		}
		if !msg.idents[port].Matches(pausedIdentity(e)) {
			m.pausedOther[port] = true // a different (or unreadable) program: never auto-share
			continue
		}
		switch {
		case p.BindScope == portscan.ScopeLoopback && !p.No127:
			if m.autoPauseBlocked() {
				continue // try again on the next refresh
			}
			if m.resuming == nil {
				m.resuming = map[int]bool{}
			}
			m.resuming[port] = true
			cmd := m.beginToggle(port, true) // exactly what `t` does: tailnet only, no confirm
			cmds = append(cmds, cmd)
			if m.pending != port { // beginToggle refused (e.g. a cloudflared metrics port)
				delete(m.resuming, port)
				delete(m.paused, port)
				changed = true
			}
		case p.BindScope == portscan.ScopeWildcard || p.BindScope == portscan.ScopeTailnet:
			// Already reachable on the tailnet; a forward would only collide.
			delete(m.paused, port)
			changed = true
			already = append(already, port)
		}
	}
	if changed {
		cmds = append(cmds, m.persistPaused())
	}
	if len(already) > 0 {
		notice = fmt.Sprintf("%s is back (same app) and already reachable on the tailnet — not re-served", portList(already))
	}
	return cmds, notice
}

// decoratePaused stamps a portItem with its paused memory (if any) so
// routesFor can show the paused tailnet sub-row.
func (m model) decoratePaused(it portItem) portItem {
	if e, ok := m.paused[it.port.Number]; ok && !it.active {
		it.paused = true
		it.pausedExe, it.pausedCwd = e.Exe, e.Cwd
		it.pausedOther = m.pausedOther[it.port.Number]
	}
	return it
}

// pausedText is the adornment on a paused route line.
func (r route) pausedText() string {
	exe := filepath.Base(r.pausedExe)
	cwd := shortPath(r.pausedCwd)
	if r.pausedOther {
		return fmt.Sprintf("· paused — a different program is listening (was %s in %s)", exe, cwd)
	}
	return fmt.Sprintf("· paused — resumes when %s in %s returns", exe, cwd)
}
