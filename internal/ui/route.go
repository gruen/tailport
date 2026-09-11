package ui

// Multi-route data model (kata th05, P0): the pure derivation from a service's
// live state to the ordered list of ROUTES it exposes. This file is
// deliberately data-only -- no rendering, no markers/colors/glyphs, no bubbles
// wiring. Those land in later th05 phases; see
// docs/tmp/th05-multiroute-design-DRAFT.md for the full row-anatomy design this
// model feeds.
//
// Today (pre-th05) each listening port renders as ONE row carrying a single
// "widest" reachability (see portscan.BindScope's widerScope aggregation and
// ui.go's reach()). th05 moves to: each service (port) owns a list of routes
// -- localhost, LAN, tailnet, ts.net funnel, caddy publish, cloudflare tunnel
// -- each its own line, narrow to wide. routesFor is the pure function that
// derives that list from a serviceState snapshot.

import (
	"fmt"
	"strings"

	"github.com/gruen/tailport/internal/portscan"
	"github.com/gruen/tailport/internal/tsserve"
)

// routeKind identifies a route type. Ordered narrow -> wide; that order is the
// order routes appear within a service.
type routeKind int

const (
	routeLocalhost routeKind = iota
	routeLAN
	routeTailnet
	routeFunnel  // ts.net
	routePublish // caddy
	routeTunnel  // cloudflare
	routeOffline // pseudo-route: a down favorite / nothing reachable
)

// route is one reachable (or degraded) path to a service.
type route struct {
	kind    routeKind
	url     string // copyable address; "" for offline, a still-starting tunnel, or a foreign one
	auth    bool   // publish (caddy) route behind shared basic-auth
	served  bool   // tailnet route provenance: true = via `tailscale serve`, false = via a wide/tailnet-IP bind
	stale   bool   // administratively present but not functionally live: tailnet served-but-nothing-listening (a dangling forward), OR a tunnel with zero ready edge connections (kata aprt)
	foreign bool   // tunnel only (kata aprt): a cloudflared tailport does NOT own covers this port -- surfaced as drift, never signalled or touched
}

// serviceState is the derivation input -- deliberately a plain struct (not the
// UI model) so routesFor is pure and table-testable.
type serviceState struct {
	port      int
	bindScope portscan.BindScope // from portscan
	bindHost  string             // widest-scope bind host (for a LAN bind's URL)
	// bindLoopback is true when a loopback bind was ALSO seen for this port,
	// even when bindScope aggregated to something wider (LAN/wildcard) --
	// t12m. Preserves the one bit that widest-scope aggregation would
	// otherwise discard, so a loopback+LAN service still gets its localhost
	// route (and its serve-route health still reflects the loopback proxy
	// target `tailscale serve` actually dials).
	bindLoopback  bool
	listening     bool         // a local process is bound
	served        bool         // tailscale serve active for this port
	funnelPub     int          // funnel public ingress port; 0 = not funnelled
	publish       *publishInfo // caddy publish state; nil = not published
	tunnelHost    string       // cloudflare tunnel hostname; only meaningful if tunnelActive
	tunnelActive  bool         // a cloudflared tunnel covers this port
	tunnelReady   bool         // the tunnel's polled edge-connection state; only meaningful if tunnelActive
	tunnelForeign bool         // a cloudflared tailport does NOT own covers this port (AGENTS.md: surfaced as drift)
	host          string       // this node's tailnet short host (m.host) for tailnet URLs; "" if tailscale down
	fqdn          string       // this node's FQDN (m.fqdn) for the funnel PublicURL
}

// routesFor derives the ordered route list for one service from its live
// state. Routes are built ADDITIVELY, narrow -> wide, matching the fixed
// on-screen order from the th05 design (§2): localhost, LAN, tailnet, ts.net,
// caddy, cloudflare. If nothing qualifies, a single pseudo-route
// (routeOffline) is appended so every service always has >=1 route (the nav
// invariant the design calls out).
func routesFor(s serviceState) []route {
	var routes []route

	// loopbackUp reports whether something is ACTUALLY bound to the loopback
	// address `tailscale serve` proxies to (127.0.0.1:PORT) -- t12m. A loopback
	// or wildcard bind qualifies directly; bindLoopback covers the case
	// aggregation would otherwise hide (a loopback bind coexisting with a wider
	// one, e.g. LAN, that won BindScope). An UNCLASSIFIED (ScopeUnknown)
	// listener is treated as loopback-reachable too, matching reach()'s
	// conservative default (its bind switch maps Loopback AND Unknown to
	// reachLocalhost) -- so a listening-but-unclassified port shows a
	// localhost route rather than misleadingly collapsing to the offline row.
	loopbackUp := s.listening && (s.bindScope == portscan.ScopeLoopback || s.bindScope == portscan.ScopeWildcard || s.bindScope == portscan.ScopeUnknown || s.bindLoopback)

	// 1. localhost: reachable from this machine whenever loopbackUp holds,
	// regardless of tailnet/LAN state.
	if loopbackUp {
		routes = append(routes, route{kind: routeLocalhost, url: addressFor("localhost", s.port)})
	}

	// 2. LAN: a specific non-tailnet bind IP is only reachable at that
	// address -- no localhost/tailnet fallback applies. Independent of
	// loopbackUp: a loopback+LAN service gets BOTH routes (t12m).
	if s.bindScope == portscan.ScopeLAN && s.listening && s.bindHost != "" {
		routes = append(routes, route{kind: routeLAN, url: addressFor(s.bindHost, s.port)})
	}

	// 3. tailnet (at most one): requires tailnet identity to build a URL at
	// all. Provenance decides served/stale: a `tailscale serve` route is stale
	// when its loopback proxy target isn't actually up -- t12m -- covering both
	// a dangling forward (nothing listening at all) AND a LAN-only listener
	// (something is listening, but not where serve dials); a wide bind's route
	// is never stale (it only exists while listening).
	if s.host != "" {
		tailnetURL := addressFor(s.host, s.port)
		if s.served {
			routes = append(routes, route{kind: routeTailnet, served: true, stale: !loopbackUp, url: tailnetURL})
		} else if (s.bindScope == portscan.ScopeWildcard || s.bindScope == portscan.ScopeTailnet) && s.listening {
			routes = append(routes, route{kind: routeTailnet, served: false, stale: false, url: tailnetURL})
		}
	}

	// 4. funnel: public ingress via Tailscale Funnel.
	if s.funnelPub != 0 {
		routes = append(routes, route{kind: routeFunnel, url: tsserve.PublicURL(s.fqdn, s.funnelPub)})
	}

	// 5. publish: public ingress via the Caddy edge.
	if s.publish != nil {
		routes = append(routes, route{kind: routePublish, url: "https://" + s.publish.hostname, auth: s.publish.auth})
	}

	// 6. tunnel: public ingress via a cloudflared tunnel. The URL is empty
	// while a quick tunnel is still starting (its *.trycloudflare.com
	// hostname isn't assigned until cloudflared actually starts). Once a
	// hostname IS known but the tunnel still reports zero ready edge
	// connections, mark it stale (kata aprt): otherwise a tunnel whose edge
	// connection never came up, or dropped, would show as reachable forever
	// off nothing but its process being alive. Before a hostname is known
	// there's nothing misleading to correct -- the empty url already reads as
	// "still starting" -- so staleness isn't judged until there's a URL to
	// judge it against.
	if s.tunnelActive {
		url := ""
		if s.tunnelHost != "" {
			url = "https://" + s.tunnelHost
		}
		routes = append(routes, route{kind: routeTunnel, url: url, stale: s.tunnelHost != "" && !s.tunnelReady})
	}
	// A foreign cloudflared can cover this port AT THE SAME TIME as a
	// tailport-owned one -- two separate processes both targeting :PORT -- so
	// this is an INDEPENDENT check, never an else on tunnelActive. Collapsing
	// the two into a switch hid the foreign tunnel behind an owned one, exactly
	// the drift this feature exists to surface (AGENTS.md: a foreign tunnel
	// must never be invisible; roborev 2196). No URL to offer: its hostname is
	// never probed and a foreign process's metrics endpoint is left alone, like
	// the process itself.
	if s.tunnelForeign {
		routes = append(routes, route{kind: routeTunnel, foreign: true})
	}

	// 7. offline fallback: nothing above qualified -- a down favorite or a
	// port with no reachability at all. Every service still needs >=1 route.
	if len(routes) == 0 {
		routes = append(routes, route{kind: routeOffline, url: ""})
	}

	return routes
}

// nonHTTPPorts is a conservative DENYLIST of well-known TCP ports whose
// standard protocol is not HTTP -- th05 §2 ("Non-HTTP: unknown -> host:port")
// via t12m. It is deliberately a denylist, not an allowlist: the vast
// majority of ports a local dev/service tool like tailport sees are ad-hoc
// HTTP servers with no reserved port of their own (a Vite dev server on
// :3000, an API on :8080, ...), so the default assumption stays HTTP and only
// positively-known non-HTTP daemons (mostly databases and other binary
// protocols) are called out here. :22 is handled separately (always "ssh
// host") and doesn't need an entry. Extend this table if another common
// non-HTTP dev dependency bites (roborev-style: fix real cases, don't
// speculate a complete IANA table).
var nonHTTPPorts = map[int]bool{
	21:    true, // FTP
	23:    true, // Telnet
	25:    true, // SMTP
	53:    true, // DNS
	110:   true, // POP3
	123:   true, // NTP
	143:   true, // IMAP
	389:   true, // LDAP
	445:   true, // SMB
	465:   true, // SMTPS
	587:   true, // SMTP submission
	636:   true, // LDAPS
	993:   true, // IMAPS
	995:   true, // POP3S
	1433:  true, // Microsoft SQL Server
	1521:  true, // Oracle
	2181:  true, // ZooKeeper
	3306:  true, // MySQL/MariaDB
	3389:  true, // RDP
	5432:  true, // PostgreSQL
	5672:  true, // AMQP (RabbitMQ)
	5900:  true, // VNC
	6379:  true, // Redis
	9092:  true, // Kafka
	11211: true, // Memcached
	27017: true, // MongoDB
}

// isPlausiblyHTTP reports whether port is a reasonable candidate to speak
// HTTP for URL-building purposes -- true unless it's in the nonHTTPPorts
// denylist above.
func isPlausiblyHTTP(port int) bool {
	return !nonHTTPPorts[port]
}

// hostPort renders a bare "host:port" address (no scheme), bracketing an IPv6
// literal host exactly like httpURL -- the th05 §2 non-HTTP fallback.
func hostPort(host string, port int) string {
	if strings.Contains(host, ":") { // IPv6 literal needs brackets
		return fmt.Sprintf("[%s]:%d", host, port)
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// addressFor builds the copyable address for host:port, honoring the design's
// non-HTTP rules (th05 §2, t12m): :22 always renders as "ssh host" (SSH has no
// URL scheme of its own); a port in the nonHTTPPorts denylist falls back to a
// bare "host:port" (e.g. Postgres on :5432); everything else -- the common
// case, an ad-hoc dev server -- keeps the http:// scheme.
func addressFor(host string, port int) string {
	switch {
	case port == 22:
		return "ssh " + host
	case isPlausiblyHTTP(port):
		return httpURL(host, port)
	default:
		return hostPort(host, port)
	}
}

// label returns r's display label. Provenance/stale never change the label --
// a stale tailnet route still labels "tailnet"; markers (not labels) carry
// that distinction (a later th05 phase).
func (r route) label() string {
	switch r.kind {
	case routeLocalhost:
		return "localhost"
	case routeLAN:
		return "LAN"
	case routeTailnet:
		return "tailnet"
	case routeFunnel:
		return "ts.net"
	case routePublish:
		return "caddy"
	case routeTunnel:
		return "cloudflare"
	case routeOffline:
		return "offline"
	default:
		return ""
	}
}
