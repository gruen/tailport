package ui

// Two-level (service, route) navigation + single-column body rendering (kata
// th05, P3b/P4/P5). The body is one vertical stack of SERVICE BLOCKS
// (renderServiceBlock), one per visible list item, separated by a blank line.
// bubbles/list stays the store + "/" filter + SERVICE cursor (m.list.Index()
// indexes VisibleItems); a SECOND cursor, m.routeIdx, selects a ROUTE sub-row
// within the current service. The old multi-column grid (renderGrid) is retired:
// extra terminal width now widens the URLs instead of spawning columns.

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func clampInt(v, lo, hi int) int {
	if hi < lo {
		hi = lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// serviceState projects a portItem onto the pure routesFor() input.
func (i portItem) serviceState() serviceState {
	var pub *publishInfo
	if i.publishHostname != "" {
		pub = &publishInfo{hostname: i.publishHostname, auth: i.publishAuth}
	}
	return serviceState{
		port:         i.port.Number,
		bindScope:    i.port.BindScope,
		bindHost:     i.port.BindHost,
		listening:    i.listening,
		served:       i.active,
		funnelPub:    i.funnelPublic,
		publish:      pub,
		tunnelHost:   i.tunnelHostname,
		tunnelActive: i.tunnelActive,
		host:         i.host,
		fqdn:         i.fqdn,
	}
}

// routes returns the ordered, active route list for this service (always >=1: an
// offline pseudo-route stands in when nothing is reachable).
func (i portItem) routes() []route { return routesFor(i.serviceState()) }

// displayName resolves the header name plus whether it's the muted "was <proc>"
// memory form, matching the retired Title()'s precedence: an explicit user label
// wins; else the live process name; else the remembered process ("was mailpit");
// else "?".
func (i portItem) displayName() (string, bool) {
	switch {
	case i.meta.Label != "":
		return i.meta.Label, false
	case i.port.Process != "":
		return i.port.Process, false
	case i.meta.LastProcess != "":
		return "was " + i.meta.LastProcess, true
	default:
		return "?", false
	}
}

// currentService returns the selected visible portItem and its index into
// VisibleItems, or ok=false when the list is empty.
func (m model) currentService() (portItem, int, bool) {
	items := m.list.VisibleItems()
	if len(items) == 0 {
		return portItem{}, -1, false
	}
	idx := clampInt(m.list.Index(), 0, len(items)-1)
	pi, ok := items[idx].(portItem)
	return pi, idx, ok
}

// clampRouteIdx keeps m.routeIdx within the current service's route count.
// Called after every rebuild (setItems) so a shrinking route list can't leave
// the route cursor dangling past the end.
func (m *model) clampRouteIdx() {
	pi, _, ok := m.currentService()
	if !ok {
		m.routeIdx = 0
		return
	}
	m.routeIdx = clampInt(m.routeIdx, 0, len(pi.routes())-1)
}

// moveRoute walks the flattened route list by delta (±1 for the arrow/jk keys),
// crossing into the adjacent service at a service's first/last route. It updates
// BOTH the service cursor (m.list) and m.routeIdx, and stops at the very
// top/bottom of the whole list.
func (m *model) moveRoute(delta int) {
	items := m.list.VisibleItems()
	if len(items) == 0 {
		return
	}
	svc := clampInt(m.list.Index(), 0, len(items)-1)
	count := func(i int) int {
		if pi, ok := items[i].(portItem); ok {
			if n := len(pi.routes()); n > 0 {
				return n
			}
		}
		return 1
	}
	idx := clampInt(m.routeIdx, 0, count(svc)-1)
	step := 1
	if delta < 0 {
		step, delta = -1, -delta
	}
	for ; delta > 0; delta-- {
		if step > 0 {
			switch {
			case idx < count(svc)-1:
				idx++
			case svc < len(items)-1:
				svc, idx = svc+1, 0
			default:
				delta = 1 // at the very bottom: stop
			}
		} else {
			switch {
			case idx > 0:
				idx--
			case svc > 0:
				svc--
				idx = count(svc) - 1
			default:
				delta = 1 // at the very top: stop
			}
		}
	}
	m.list.Select(svc)
	m.routeIdx = idx
}

// jumpService moves the SERVICE cursor by delta (Shift+arrows / J,K), landing on
// the target service's FIRST route (design §4).
func (m *model) jumpService(delta int) {
	items := m.list.VisibleItems()
	if len(items) == 0 {
		return
	}
	m.list.Select(clampInt(m.list.Index()+delta, 0, len(items)-1))
	m.routeIdx = 0
}

// bodyLines renders every visible service block into one flat line slice (blocks
// separated by a single blank line) and reports the line index of the
// currently-selected route (-1 when the list is empty). It is the SINGLE layout
// shared by the renderer (renderList) and the scroll-keeper (ensureRouteVisible),
// so the two can never disagree about where a given route sits.
func (m model) bodyLines() (lines []string, headerLine, selLine int) {
	items := m.list.VisibleItems()
	headerLine, selLine = -1, -1
	cur := m.list.Index()
	for i, it := range items {
		pi, ok := it.(portItem)
		if !ok {
			continue
		}
		routes := pi.routes()
		name, was := pi.displayName()
		b := blockInput{
			port:          pi.port.Number,
			name:          name,
			nameWas:       was,
			pid:           pi.pid,
			favorite:      pi.meta.Favorite,
			locked:        pi.meta.Locked,
			routes:        routes,
			current:       i == cur,
			selectedRoute: -1,
			copiedRoute:   -1,
			emoji:         pi.emoji,
			width:         m.width,
		}
		if i == cur {
			b.selectedRoute = clampInt(m.routeIdx, 0, len(routes)-1)
		}
		if m.copiedPort != 0 && pi.port.Number == m.copiedPort {
			b.copiedRoute = clampInt(m.copiedRouteIdx, 0, len(routes)-1)
		}
		if len(lines) > 0 {
			lines = append(lines, "") // blank separator between records
		}
		if i == cur {
			headerLine = len(lines)
			selLine = headerLine + 1 + b.selectedRoute // +1 for the header line
		}
		lines = append(lines, renderServiceBlock(b)...)
	}
	return lines, headerLine, selLine
}

// ensureRouteVisible nudges m.scrollOff just enough to keep the selected route
// on screen within the list body height. A no-op before the first WindowSizeMsg.
func (m *model) ensureRouteVisible() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	h := m.listBodyHeight()
	lines, headerLine, selLine := m.bodyLines()
	if selLine >= 0 {
		// Scroll DOWN just enough if the selected route sits below the viewport.
		if selLine >= m.scrollOff+h {
			m.scrollOff = selLine - h + 1
		}
		// Scroll UP to reveal the current service's HEADER (so you always see
		// which service the selected route belongs to), not merely the selected
		// route line. Anchoring on the route left the top record's header
		// (usually :22) clipped when you scrolled to the top: the topmost
		// SELECTABLE line is the first route, one row BELOW the header, so the
		// offset floored there and dropped the header. For a block taller than
		// the viewport we can't show both, so fall back to keeping the route on
		// screen.
		if headerLine < m.scrollOff {
			m.scrollOff = headerLine
			if selLine >= m.scrollOff+h {
				m.scrollOff = selLine - h + 1
			}
		}
	}
	m.scrollOff = clampInt(m.scrollOff, 0, maxInt(0, len(lines)-h))
}

// reconcileViewport re-syncs the list to a changed body height: resizeList
// re-applies listBodyHeight to the bubbles/list, and ensureRouteVisible nudges
// scrollOff so the SELECTED route stays on screen. Call it wherever a sticky
// setup banner is RAISED. Unlike every other height-changing site (flash, poof,
// prompt entry, resize -- all of which already call resizeList), the banner
// flags historically needed no reconcile: the reservation was worst-cased and
// constant, so raising a banner never shrank the visible list. Now that the
// reservation is LIVE (bannerReservationLines measures only active banners), an
// appearing banner really does claim rows from the list, so -- exactly like the
// nav keys -- it must nudge scrollOff or a selection near the bottom drops below
// the fold (roborev job 2190). Clearing a banner only GROWS the list, which
// can't hide the selection, so those sites don't need it.
func (m *model) reconcileViewport() {
	m.resizeList()
	m.ensureRouteVisible()
}

// renderList is the single-column body: the stacked service blocks (bodyLines),
// scrolled to m.scrollOff and sliced to the list body height, followed by a
// one-line scroll indicator (blank when everything fits) so renderList always
// emits exactly the body-height + pageIndicatorLines rows listBodyHeight
// reserves. Called from View in place of the retired renderGrid.
func (m model) renderList() string {
	lines, _, _ := m.bodyLines()
	h := m.listBodyHeight()
	off := clampInt(m.scrollOff, 0, maxInt(0, len(lines)-h))
	end := off + h
	if end > len(lines) {
		end = len(lines)
	}
	body := strings.Join(lines[off:end], "\n")
	indicator := ""
	if off > 0 || end < len(lines) {
		indicator = helpStyle.Render(fmt.Sprintf("%d–%d of %d", off+1, end, len(lines)))
	}
	return body + "\n" + indicator
}

// copyRoute copies one route's URL to the clipboard and flags that exact route
// for the transient inline "✓ copied" annotation (route-scoped copy, kata th05
// P5 -- retires the old per-port aggregate copyURL/copyTargetURL). An empty-URL
// route (offline, or a still-starting quick tunnel) has nothing to copy and
// never reaches here; the caller toasts instead.
func (m *model) copyRoute(port, routeIdx int, url string) tea.Cmd {
	m.copiedID++
	id := m.copiedID
	m.copiedPort = port
	m.copiedRouteIdx = routeIdx
	// Clear any lingering toast so the two confirmation channels never show at
	// once (mirrors the retired copyURL).
	m.flash = ""
	m.flashLevel = flashInfo
	m.resizeList()
	return tea.Batch(
		copyCmd(url),
		m.rebuildItems(),
		tea.Tick(3*time.Second, func(time.Time) tea.Msg { return copiedExpireMsg{id: id} }),
	)
}
