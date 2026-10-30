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

	"github.com/charmbracelet/bubbles/list"
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
		port:          i.port.Number,
		bindScope:     i.port.BindScope,
		bindHost:      i.port.BindHost,
		bindLoopback:  i.port.Loopback,
		no127:         i.port.No127,
		listening:     i.listening,
		served:        i.active,
		funnelPub:     i.funnelPublic,
		publish:       pub,
		tunnelHost:    i.tunnelHostname,
		tunnelActive:  i.tunnelActive,
		tunnelReady:   i.tunnelReady,
		tunnelForeign: i.tunnelForeign,
		host:          i.host,
		fqdn:          i.fqdn,
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
	m.noteNav()
}

// selKey is the user's selected route by IDENTITY -- (port, routeID) -- not
// by position (kata 4hcg). The cursor proper is an index pair (m.list's service
// row + m.routeIdx), but a service's route list shifts and gains rows across
// refreshes (a publish on an unserved port inserts routeTailnet above
// routePublish; a failed status refresh hides the tailnet route and the next
// good one restores it), so an index alone drifts onto a different route.
// reconcileSelection re-points the index pair at selKey after every rebuild.
type selKey struct {
	port int
	id   routeID
	set  bool
}

// routeID identifies a route within one service: its kind plus the one
// discriminator routesFor needs. Kinds are unique per service EXCEPT
// routeTunnel, where an owned and a foreign cloudflared can cover the same
// port at once, each its own row; foreign tells them apart.
type routeID struct {
	kind    routeKind
	foreign bool
}

func (r route) id() routeID { return routeID{kind: r.kind, foreign: r.foreign} }

// pendingLand is a requested landing on a route that may not exist yet (kata
// 4hcg): an exposure op succeeded, but the route it creates only appears once
// the confirming refresh/poll reflects it. reconcileSelection retries it after
// every rebuild until the route shows up, the user navigates, or the deadline
// passes (checked lazily there -- no ticker).
type pendingLand struct {
	port     int
	id       routeID
	deadline time.Time
	set      bool
}

// landTTL bounds how long a pendingLand waits for its route to appear.
const landTTL = 10 * time.Second

// syncSelKey records the cursor's current (service, route kind) as selKey and
// pins routeIdx into range. Called wherever the selection changes for a real
// reason (navigation, selectPort). No-op when no service is readable, so a
// filter re-match's momentarily empty visible set never overwrites selKey.
func (m *model) syncSelKey() {
	pi, _, ok := m.currentService()
	if !ok {
		return
	}
	rs := pi.routes()
	m.routeIdx = clampInt(m.routeIdx, 0, len(rs)-1)
	m.selKey = selKey{port: pi.port.Number, id: rs[m.routeIdx].id(), set: true}
}

// noteNav is the user-navigation hook: the new cursor becomes the selection
// identity, and any pending landing is cancelled (the user has moved on).
func (m *model) noteNav() {
	m.pendingLand = pendingLand{}
	m.syncSelKey()
}

// reconcileSelection re-points the cursors at selKey after anything that can
// shift rows underneath them: every items rebuild (setItems) and the async
// "/" filter result (Update). It also retries a pendingLand. While the visible
// set is empty (a filter re-match in flight) it changes nothing -- selKey and
// pendingLand survive until the result lands. While the user is TYPING a
// filter the cursor is index-driven as bubbles/list has always done it, so
// reconcile adopts the cursor instead of fighting it. If selKey's service is
// gone, or has no route with selKey.id, the cursor falls back to the plain
// clamp and selKey follows what is actually selected.
func (m *model) reconcileSelection() {
	vis := m.list.VisibleItems()
	if len(vis) == 0 {
		if len(m.list.Items()) == 0 {
			m.routeIdx = 0
		}
		return
	}
	if p := m.pendingLand; p.set {
		if time.Now().After(p.deadline) || !m.selKey.set || m.selKey.port != p.port {
			m.pendingLand = pendingLand{}
		} else if m.hasRoute(vis, p.port, p.id) {
			m.selKey = selKey{port: p.port, id: p.id, set: true}
			m.pendingLand = pendingLand{}
		}
	}
	if m.selKey.set && m.list.FilterState() != list.Filtering {
		for i, it := range vis {
			pi, ok := it.(portItem)
			if !ok || pi.port.Number != m.selKey.port {
				continue
			}
			rs := pi.routes()
			m.list.Select(i)
			for ri, r := range rs {
				if r.id() == m.selKey.id {
					m.routeIdx = ri
					m.ensureRouteVisible()
					return
				}
			}
			break // service kept, its route is gone: clamp below and adopt
		}
	}
	if m.list.Index() >= len(vis) || m.list.Index() < 0 {
		m.list.Select(len(vis) - 1)
	}
	m.syncSelKey()
	m.ensureRouteVisible()
}

// hasRoute reports whether items holds port with a route of the given kind.
func (m model) hasRoute(items []list.Item, port int, id routeID) bool {
	for _, it := range items {
		if pi, ok := it.(portItem); ok && pi.port.Number == port {
			for _, r := range pi.routes() {
				if r.id() == id {
					return true
				}
			}
		}
	}
	return false
}

// landOn requests that the cursor land on port's route of the given kind once
// it exists (kata 4hcg): the exposure ops' done handlers call it after a
// successful turn-ON. The "don't yank" guard is selection identity -- an op is
// async, and a user who has navigated to another service while it was in
// flight keeps their place. selKey (not the visible rows) is what is compared,
// so the guard still holds while a filter re-match has emptied VisibleItems.
// The landing is resolved immediately when the route already exists, else on
// the first rebuild that brings it (reconcileSelection).
func (m *model) landOn(port int, kind routeKind) {
	m.syncSelKey() // no-op while the visible set is empty; selKey stays authoritative
	if !m.selKey.set || m.selKey.port != port {
		return
	}
	m.pendingLand = pendingLand{port: port, id: routeID{kind: kind}, deadline: time.Now().Add(landTTL), set: true}
	m.reconcileSelection()
}

// selectRoute points BOTH cursors -- the service (m.list) and the route
// sub-row (m.routeIdx) -- at port's route of the given kind, by recording it
// as the selection identity and reconciling, so a caller can steer the user at
// a SPECIFIC route rather than just the service (kata h2ef). If kind isn't
// found among the port's current routes the service cursor still moves and the
// route cursor is only clamped into range; if port isn't visible the selection
// is left where it is.
func (m *model) selectRoute(port int, kind routeKind) {
	m.selKey = selKey{port: port, id: routeID{kind: kind}, set: true}
	m.reconcileSelection()
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
	m.noteNav()
}

// bodyLines renders every visible service block into one flat line slice (blocks
// separated by a single blank line) and reports the line index of the
// currently-selected route (-1 when the list is empty). It is the SINGLE layout
// shared by the renderer (renderList) and the scroll-keeper (ensureRouteVisible),
// so the two can never disagree about where a given route sits. It's a thin
// wrapper around bodyLinesFull (kata k4cj) that drops the per-line owner slice,
// keeping the existing ~10 callers (ensureRouteVisible, ui_test.go) unchanged.
func (m model) bodyLines() (lines []string, headerLine, selLine int) {
	lines, _, headerLine, selLine = m.bodyLinesFull()
	return lines, headerLine, selLine
}

// bodyLinesFull is bodyLines' full form (kata k4cj): alongside the flat line
// slice it also reports, per line, which service header OWNS that line --
// owners[i] is the index in lines of the service-header line that line i
// belongs to, or -1 for a blank block-separator line. renderList uses this to
// pin the owning header as the top row when the viewport clips mid-block (the
// sticky_header feature); ensureRouteVisible and everything else keeps using
// the bodyLines wrapper above and never sees owners.
func (m model) bodyLinesFull() (lines []string, owners []int, headerLine, selLine int) {
	items := m.list.VisibleItems()
	headerLine, selLine = -1, -1
	// Clamp like currentService() does (z6yf): bubbles/list's OWN cursor
	// isn't re-clamped when VisibleItems narrows out from under it -- most
	// notably list.FilterMatchesMsg, the async result of a "/" query, which
	// only ever replaces m.filteredItems and returns before reaching its
	// usual updatePagination() clamp. An unclamped cur that no longer
	// matches any i below would make NO item "current", hiding the
	// selection bar/pointer entirely rather than merely leaving it
	// off-screen -- clamping keeps the last item selected instead, which
	// ensureRouteVisible can then scroll into view.
	cur := clampInt(m.list.Index(), 0, len(items)-1)
	// Precomputed once per render (kata h2ef): every pending-tunnel route
	// shares the same animation frame, so there's no need to recompute this
	// per service. m.emoji -- the egg's own resolved emojiCapable(), not the
	// separate --markers-driven m.markerEmoji -- is the terminal-capability
	// signal this reuses, matching how the egg picks its own UTF-8-vs-ASCII
	// fallback.
	pendingLabel := tunnelSpinnerLabel(m.tunnelSpinnerFrame, m.emoji)
	for i, it := range items {
		pi, ok := it.(portItem)
		if !ok {
			continue
		}
		routes := pi.routes()
		name, was := pi.displayName()
		// A port that is some owned tunnel's cloudflared metrics listener is
		// labelled in place of its process/label name (kata cn3a): the row
		// stays otherwise normal, but nobody should mistake this loopback
		// endpoint -- which serves /config, /debug/pprof/ and /diag/* -- for an
		// app, or wonder why it can't be exposed. Computed from the live poll
		// state, so the label disappears with the tunnel.
		if tp, ok := m.metricsPortOwner(pi.port.Number); ok {
			name, was = fmt.Sprintf("cloudflared metrics for :%d", tp), false
		}
		b := blockInput{
			port:                     pi.port.Number,
			name:                     name,
			nameWas:                  was,
			pid:                      pi.port.Pid,
			favorite:                 pi.meta.Favorite,
			locked:                   pi.meta.Locked,
			dimmed:                   pi.dimmed,
			routes:                   routes,
			current:                  i == cur,
			selectedRoute:            -1,
			copiedRoute:              -1,
			emoji:                    pi.emoji,
			width:                    m.width,
			tunnelPendingPlaceholder: pendingLabel,
		}
		if i == cur {
			b.selectedRoute = clampInt(m.routeIdx, 0, len(routes)-1)
		}
		if m.copiedPort != 0 && pi.port.Number == m.copiedPort {
			// Stable identity, not index (z6yf): find the route whose
			// kind+url still matches what was copied, wherever it now sits.
			// A route list reorders/grows/shrinks across rebuilds (a poll
			// discovering a new bind, a route torn down) -- an INDEX-based
			// lookup (the old m.copiedRouteIdx, clamped into range) could
			// silently land the "✓ copied" annotation on a different route
			// that happens to now occupy that slot. If the copied route is
			// simply gone, b.copiedRoute stays -1 (its zero value set above)
			// and the annotation just disappears, which is correct.
			for ri, rt := range routes {
				if rt.kind == m.copiedRouteKind && rt.url == m.copiedRouteURL {
					b.copiedRoute = ri
					break
				}
			}
		}
		if len(lines) > 0 {
			lines = append(lines, "") // blank separator between records
			owners = append(owners, -1)
		}
		headerIdx := len(lines)
		if i == cur {
			headerLine = headerIdx
			selLine = headerLine + 1 + b.selectedRoute // +1 for the header line
		}
		block := renderServiceBlock(b)
		lines = append(lines, block...)
		for range block {
			owners = append(owners, headerIdx)
		}
	}
	return lines, owners, headerLine, selLine
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
// scrollOff so the SELECTED route stays on screen. Originally added (roborev
// job 2190) for wherever a sticky setup banner is RAISED: unlike every other
// height-changing site (flash, poof, prompt entry, resize -- all of which
// already called resizeList), the banner flags historically needed no
// reconcile, because the reservation was worst-cased and constant, so raising
// a banner never shrank the visible list. Once the reservation went LIVE
// (bannerReservationLines measures only active banners), an appearing banner
// really does claim rows from the list, so -- exactly like the nav keys -- it
// must nudge scrollOff or a selection near the bottom drops below the fold.
//
// z6yf broadened this beyond banners to every OTHER viewport/visible-item
// change that isn't already followed by its own ensureRouteVisible: a
// WindowSizeMsg resize, and setFlash (a wrapped multi-line toast shrinks the
// list exactly like a banner does). ensureRouteVisible alone (no resize
// needed) is folded into setItems and selectPort instead, since those don't
// change m.width/m.height -- see their doc comments -- and the async
// list.FilterMatchesMsg path gets its own explicit call in Update, since that
// message narrows/widens VisibleItems without ever going through setItems.
//
// Clearing a banner (or a toast) only GROWS the list, which can't hide the
// selection, so those sites don't need it.
func (m *model) reconcileViewport() {
	m.resizeList()
	m.ensureRouteVisible()
}

// renderList is the single-column body: the stacked service blocks
// (bodyLinesFull), scrolled to m.scrollOff and sliced to the list body height,
// followed by a one-line scroll indicator (blank when everything fits) so
// renderList always emits exactly the body-height + pageIndicatorLines rows
// listBodyHeight reserves. Called from View in place of the retired
// renderGrid.
func (m model) renderList() string {
	lines, owners, _, selLine := m.bodyLinesFull()
	h := m.listBodyHeight()
	off := clampInt(m.scrollOff, 0, maxInt(0, len(lines)-h))
	end := off + h
	if end > len(lines) {
		end = len(lines)
	}
	visible := make([]string, end-off)
	copy(visible, lines[off:end])
	// Sticky service header (kata k4cj): when the viewport top lands mid-block,
	// replace that top route line with its owning service's header so the
	// routes below always show which service they belong to. Height-neutral
	// (replaces, never adds a row) so ensureRouteVisible's math is unaffected.
	// Guarded by off != selLine so the SELECTED route is never the line hidden
	// (matters only in the h==1 tall-block corner).
	if m.stickyHeader && off > 0 && off != selLine && len(visible) > 0 {
		if oh := owners[off]; oh >= 0 && oh < off {
			visible[0] = lines[oh]
		}
	}
	body := strings.Join(visible, "\n")
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
// never reaches here; the caller toasts instead. kind+url (not an index --
// z6yf) is the route's stable identity: bodyLines re-finds it by that pair on
// every render, so a route list that reorders/grows/shrinks underneath the
// annotation (a poll discovering a new bind, an earlier route torn down)
// can't move the checkmark onto a different route.
func (m *model) copyRoute(port int, kind routeKind, url string) tea.Cmd {
	m.copiedID++
	id := m.copiedID
	m.copiedPort = port
	m.copiedRouteKind = kind
	m.copiedRouteURL = url
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
