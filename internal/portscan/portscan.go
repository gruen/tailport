// Package portscan enumerates locally listening TCP ports.
package portscan

import "net"

// Port is a locally listening TCP port, optionally attributed to a process.
type Port struct {
	Number  int
	Process string // best-effort; empty if not resolvable (e.g. owned by another user)
	// BindScope is the WIDEST (most tailnet-reachable) scope across all of
	// this port's non-filtered bind rows -- see classifyBindScope and the
	// per-port aggregation in the scanners. It answers "who can actually
	// reach this?" so callers can distinguish a truly-local loopback dev
	// server from something already reachable by tailnet peers (a wildcard
	// bind), which `tailscale serve` has no bearing on.
	BindScope BindScope
	// BindHost is the bare bind address (no brackets, no port) of the
	// WIDEST-scope bind row -- the same row BindScope was taken from. It lets a
	// caller build a URL that actually resolves for that scope: a LAN-only port
	// (ScopeLAN) copies http://<BindHost>:<port>. For loopback/wildcard binds
	// the UI substitutes "localhost"/the tailnet host instead, so BindHost
	// matters chiefly for the LAN case. Empty if unset.
	BindHost string
}

// BindScope classifies how far a listening socket's bind address reaches. It
// captures the correctness insight behind 79xb: reachability is a property of
// the BIND, not of `tailscale serve`. A wildcard-bound socket is already
// reachable by tailnet peers at the IP layer (WireGuard, subject to ACLs);
// `serve` is a separate app-layer reverse proxy that only matters for
// loopback-bound apps.
type BindScope int

// The constants are declared in ascending reachability width, and that integer
// ordering IS the reachability ranking used by widerScope for per-port
// aggregation (widest wins). Keep them in this order. ScopeUnknown is the
// zero value and deliberately ranks LOWEST: an unclassified port promises
// nothing, so the first real bind a port is seen on always wins the
// aggregation, and we never over-promise reachability by default.
const (
	ScopeUnknown  BindScope = iota // zero value; unclassified -- least reachable
	ScopeLoopback                  // 127.0.0.0/8, ::1 -- this machine only
	ScopeLAN                       // a specific non-tailnet IP -- LAN reachable, NOT tailnet
	ScopeTailnet                   // the node's own tailnet IP (100.64/10, fd7a:.../48)
	ScopeWildcard                  // 0.0.0.0 / :: / * -- reaches the tailnet
)

// String renders a BindScope for debugging and tests. These are internal
// diagnostic labels, not the user-facing lexicon (that lives in internal/ui).
func (s BindScope) String() string {
	switch s {
	case ScopeLoopback:
		return "loopback"
	case ScopeLAN:
		return "lan"
	case ScopeTailnet:
		return "tailnet"
	case ScopeWildcard:
		return "wildcard"
	default:
		return "unknown"
	}
}

// classifyBindScope maps a bare host string (no brackets, no port -- exactly as
// the scanners already extract it) to its reachability scope. It is a pure
// function of the host so it can be unit-tested on any platform.
//
// Rules (79xb truth table):
//   - "*" (lsof's literal wildcard), 0.0.0.0, :: (the unspecified addresses)
//     -> ScopeWildcard: reachable by tailnet peers at the IP layer.
//   - 127.0.0.0/8, ::1 -> ScopeLoopback: this machine only.
//   - the node's own Tailscale ranges -> ScopeTailnet.
//   - any other parseable IP (LAN 10/172.16/192.168, or some other specific
//     address) -> ScopeLAN.
//   - a non-"*" host we cannot parse (a hostname, empty string, garbage) ->
//     ScopeLAN, the CONSERVATIVE default: we never fall back to Wildcard/
//     Tailnet, because that would over-promise tailnet reachability for
//     something we could not positively confirm reaches the tailnet.
func classifyBindScope(host string) BindScope {
	// lsof emits a literal "*" for a wildcard bind; net.ParseIP can't parse
	// it, so recognise it explicitly before parsing.
	if host == "*" {
		return ScopeWildcard
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return ScopeLAN // unparseable, non-"*": conservative -- never over-promise.
	}
	switch {
	case ip.IsUnspecified(): // 0.0.0.0 and :: -- reaches the tailnet
		return ScopeWildcard
	case ip.IsLoopback(): // 127.0.0.0/8 and ::1
		return ScopeLoopback
	case isTailscaleAddr(host): // node's own 100.64/10 or fd7a:.../48
		return ScopeTailnet
	default: // some other specific IP: LAN or public, reachable but not (by itself) tailnet
		return ScopeLAN
	}
}

// widerScope returns whichever of a and b is the more tailnet-reachable
// (wider) scope. It is how the scanners collapse a port that appears on
// several bind rows (dual-stack 0.0.0.0 + [::], or 127.0.0.1 + 0.0.0.0) into
// one honest reachability: a port bound on BOTH 127.0.0.1 and 0.0.0.0 IS
// tailnet-reachable, so it must aggregate to Wildcard, not Loopback.
// Reachability order, widest first: Wildcard > Tailnet > LAN > Loopback >
// Unknown -- encoded directly by the constant ordering above. (Tailnet-scope
// binds are filtered out upstream as tailscaled's own, so in practice only
// Wildcard/LAN/Loopback ever reach this helper.)
func widerScope(a, b BindScope) BindScope {
	if a >= b {
		return a
	}
	return b
}

// Tailscale's own address ranges. A socket bound to one of these is always
// tailscaled itself (e.g. the serve-proxy listener it opens for every active
// `tailscale serve` mapping), never a local application. Filtering these out
// is what lets an exposed-but-not-listening port register as genuinely
// not-listening -- see internal/ui dangling-forward detection.
//   - 100.64.0.0/10 is the CGNAT range Tailscale carves tailnet IPv4s from.
//   - fd7a:115c:a1e0::/48 is Tailscale's ULA prefix for tailnet IPv6s.
var tailscaleNets = func() []*net.IPNet {
	var nets []*net.IPNet
	for _, cidr := range []string{"100.64.0.0/10", "fd7a:115c:a1e0::/48"} {
		if _, n, err := net.ParseCIDR(cidr); err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}()

// isTailscaleAddr reports whether host (a bare IP string, no port/brackets)
// falls in one of Tailscale's own ranges. Non-IP hosts (e.g. "*", or an
// unparseable string) and out-of-range IPs return false, so loopback,
// 0.0.0.0/::, and LAN addresses are all correctly kept.
func isTailscaleAddr(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range tailscaleNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
