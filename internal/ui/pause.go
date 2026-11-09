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

	"github.com/gruen/tailport/internal/cftunnel"
	"github.com/gruen/tailport/internal/pausedserve"
	"github.com/gruen/tailport/internal/portscan"
	"github.com/gruen/tailport/internal/procid"
	"github.com/gruen/tailport/internal/tsserve"
)

// identify reads a listener's identity; a var so tests stay hermetic.
// serveOn is the serve-ON call the guarded resume makes (a var for tests).
var (
	identify = procid.Of
	serveOn  = tsserve.On
)

// dispPath / dispExe are the DISPLAY copies of a remembered path: abbreviated
// and passed through cftunnel.SanitizeDisplay so a directory name carrying
// escapes, newlines or bidi/format characters can never inject content into a
// toast or a route line (roborev 6305 #5). Matching always uses the raw
// strings, never these.
func dispPath(p string) string { return cftunnel.SanitizeDisplay(shortPath(p)) }
func dispExe(p string) string  { return cftunnel.SanitizeDisplay(filepath.Base(p)) }

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
		return fmt.Sprintf("paused :%d (app stopped) — resumes when %s in %s returns · t to forget", e.Port, dispExe(e.Exe), dispPath(e.Cwd))
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
		if !m.active[port] {
			delete(m.unresolved, port) // the exposure is gone; resumable again
		}
		switch {
		case m.active[port] && m.unresolved[port]:
			continue // an unresolved resume: keep the original entry for now
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
			// Resume is an automatic EXPOSURE, so it obeys every guard pause
			// does (roborev 6305 #4): required public-state sources known, and
			// no funnel/publish/tunnel on the port. While any blocks it the
			// paused memory is KEPT.
			if !m.publicStateKnown() || m.hasPublicExposure(port) || m.autoPauseBlocked() {
				continue // try again on a later refresh
			}
			if m.resuming == nil {
				m.resuming = map[int]bool{}
			}
			m.resuming[port] = true
			cmds = append(cmds, m.beginResume(port, e))
			if m.pending != port { // refused (e.g. a cloudflared metrics port)
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
	exe := dispExe(r.pausedExe)
	cwd := dispPath(r.pausedCwd)
	if r.pausedOther {
		return fmt.Sprintf("· paused — a different program is listening (was %s in %s)", exe, cwd)
	}
	return fmt.Sprintf("· paused — resumes when %s in %s returns", exe, cwd)
}

// hasPublicExposure reports funnel, a Caddy publish or a Cloudflare tunnel on
// port (owned or foreign): the same exclusion pause applies.
func (m model) hasPublicExposure(port int) bool {
	if _, ok := m.funnel[port]; ok {
		return true
	}
	if _, ok := m.published[port]; ok {
		return true
	}
	if _, ok := m.tunnels[port]; ok {
		return true
	}
	return m.tunnelForeign[port]
}

// beginResume starts the guarded re-serve of a paused port: the same
// bookkeeping as beginToggle(port, true), but the serve itself runs through
// guardedResume, which re-verifies the listener right before tsserve.On.
func (m *model) beginResume(port int, e pausedserve.Entry) tea.Cmd {
	if _, refused := m.metricsRefusal(port); refused {
		return nil
	}
	save := m.remember(port)
	m.pending = port
	return tea.Batch(save, guardedResume(port, pausedIdentity(e)))
}

// resumeAbortedMsg: the guarded resume declined to serve (or undid its serve
// at once) because the listener on the port was not provably the remembered
// program. The paused memory is kept.
type resumeAbortedMsg struct {
	port    int
	why     string
	undone  bool  // a serve was made and rolled back again
	rollErr error // the rollback itself failed: the port may be shared with the wrong program
}

// verifyListener freshly re-derives who is behind port's 127.0.0.1 listener
// and checks it is want, bound to loopback. It returns the verified identity,
// or a non-empty reason it is not provably the same program.
func verifyListener(port int, want procid.Identity) (procid.Identity, string) {
	ports, err := scanListeners()
	if err != nil {
		return procid.Identity{}, "the listener scan failed"
	}
	for _, p := range ports {
		if p.Number != port {
			continue
		}
		if p.Pid127 <= 0 {
			return procid.Identity{}, "the 127.0.0.1 listener is ambiguous or unattributed"
		}
		if p.BindScope != portscan.ScopeLoopback || p.No127 {
			return procid.Identity{}, "the listener is no longer loopback-bound"
		}
		id := identify(p.Pid127)
		if !id.Matches(want) {
			return procid.Identity{}, "the program on the port changed"
		}
		return id, ""
	}
	return procid.Identity{}, "nothing is listening any more"
}

// guardedResume is the off-Update-thread resume (roborev 6305 #2): the
// refresh snapshot that decided to resume may be seconds old, and an app can
// release its socket (staying alive) while another program binds the port. So
// immediately before tsserve.On it re-scans, re-derives the 127.0.0.1 owner and
// re-reads ITS identity, serving only if it still matches; and re-verifies
// once more right after, undoing the serve if the answer flipped.
func guardedResume(port int, want procid.Identity) tea.Cmd {
	return func() tea.Msg {
		id, why := verifyListener(port, want)
		if why != "" {
			return resumeAbortedMsg{port: port, why: why}
		}
		if err := serveOn(port); err != nil {
			return toggleDoneMsg{port: port, err: err, kind: routeTailnet, turnOn: true}
		}
		if _, why := verifyListener(port, want); why != "" {
			if err := serveOff(port); err != nil {
				// The serve exists and the listener is not provably ours: do NOT
				// report success. The handler surfaces the unresolved exposure.
				return resumeAbortedMsg{port: port, why: why, rollErr: err}
			}
			return resumeAbortedMsg{port: port, why: why, undone: true}
		}
		return toggleDoneMsg{port: port, kind: routeTailnet, turnOn: true, verified: id}
	}
}
