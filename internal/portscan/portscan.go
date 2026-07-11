// Package portscan enumerates locally listening TCP ports.
package portscan

import "net"

// Port is a locally listening TCP port, optionally attributed to a process.
type Port struct {
	Number  int
	Process string // best-effort; empty if not resolvable (e.g. owned by another user)
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
