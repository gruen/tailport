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
	kind   routeKind
	url    string // copyable address; "" for offline and for a still-starting tunnel
	auth   bool   // publish (caddy) route behind shared basic-auth
	served bool   // tailnet route provenance: true = via `tailscale serve`, false = via a wide/tailnet-IP bind
	stale  bool   // tailnet route: served but nothing listening (a dangling forward)
}

// serviceState is the derivation input -- deliberately a plain struct (not the
// UI model) so routesFor is pure and table-testable.
type serviceState struct {
	port         int
	bindScope    portscan.BindScope // from portscan
	bindHost     string             // widest-scope bind host (for a LAN bind's URL)
	listening    bool               // a local process is bound
	served       bool               // tailscale serve active for this port
	funnelPub    int                // funnel public ingress port; 0 = not funnelled
	publish      *publishInfo       // caddy publish state; nil = not published
	tunnelHost   string             // cloudflare tunnel hostname; only meaningful if tunnelActive
	tunnelActive bool               // a cloudflared tunnel covers this port
	host         string             // this node's tailnet short host (m.host) for tailnet URLs; "" if tailscale down
	fqdn         string             // this node's FQDN (m.fqdn) for the funnel PublicURL
}

// routesFor derives the ordered route list for one service from its live
// state. Routes are built ADDITIVELY, narrow -> wide, matching the fixed
// on-screen order from the th05 design (§2): localhost, LAN, tailnet, ts.net,
// caddy, cloudflare. If nothing qualifies, a single pseudo-route
// (routeOffline) is appended so every service always has >=1 route (the nav
// invariant the design calls out).
func routesFor(s serviceState) []route {
	var routes []route

	// 1. localhost: a loopback or wildcard bind is reachable from this
	// machine via localhost regardless of tailnet/LAN state. An UNCLASSIFIED
	// (ScopeUnknown) listener is treated as localhost-only here too, matching
	// reach()'s conservative default (its bind switch maps Loopback AND Unknown
	// to reachLocalhost) -- so a listening-but-unclassified port shows a
	// localhost route rather than misleadingly collapsing to the offline row.
	if s.listening && (s.bindScope == portscan.ScopeLoopback || s.bindScope == portscan.ScopeWildcard || s.bindScope == portscan.ScopeUnknown) {
		url := fmt.Sprintf("http://localhost:%d", s.port)
		if s.port == 22 {
			url = "ssh localhost"
		}
		routes = append(routes, route{kind: routeLocalhost, url: url})
	}

	// 2. LAN: a specific non-tailnet bind IP is only reachable at that
	// address -- no localhost/tailnet fallback applies.
	if s.bindScope == portscan.ScopeLAN && s.listening && s.bindHost != "" {
		routes = append(routes, route{kind: routeLAN, url: httpURL(s.bindHost, s.port)})
	}

	// 3. tailnet (at most one): requires tailnet identity to build a URL at
	// all. Provenance decides served/stale: a `tailscale serve` route can be
	// stale (served but nothing listening -- a dangling forward); a wide
	// bind's route is never stale (it only exists while listening).
	if s.host != "" {
		tailnetURL := httpURL(s.host, s.port)
		if s.port == 22 {
			tailnetURL = "ssh " + s.host
		}
		if s.served {
			routes = append(routes, route{kind: routeTailnet, served: true, stale: !s.listening, url: tailnetURL})
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
	// hostname isn't assigned until cloudflared actually starts).
	if s.tunnelActive {
		url := ""
		if s.tunnelHost != "" {
			url = "https://" + s.tunnelHost
		}
		routes = append(routes, route{kind: routeTunnel, url: url})
	}

	// 7. offline fallback: nothing above qualified -- a down favorite or a
	// port with no reachability at all. Every service still needs >=1 route.
	if len(routes) == 0 {
		routes = append(routes, route{kind: routeOffline, url: ""})
	}

	return routes
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
