// Package stale decides which dangling serve forwards tailport may tear down
// without a keypress (kata 9p43): the TUI's grace-period auto-clear and the
// `tailport clean` command share this one rule, so they cannot drift apart.
//
// A forward is DANGLING when it is served and nothing listens locally on its
// port. tailscaled then still holds that port on the node's tailnet IPs, so
// any app binding the wildcard address there fails with EADDRINUSE. That is
// the only state cleared. The "something listens but serve's 127.0.0.1 dial
// misses it" state (No127) is not dangling and is never touched: it causes no
// bind collision.
package stale

import (
	"sort"

	"github.com/gruen/tailport/internal/config"
)

// Clearable returns the sorted ports that are served, not listening, and
// eligible to be cleared automatically. Never :22, never a locked port, and
// never a port that also carries funnel, a Caddy publish or a Cloudflare
// tunnel: those are deliberate public exposure, and none of them holds a
// local-port collision, so removing the serve under them would only break
// something the owner set up.
//
// served: ports with a live serve mapping. listening: ports with any local
// listener, on any bind. funnel: local port -> public ingress port. public:
// ports carrying a Caddy publish or a Cloudflare tunnel.
func Clearable(cfg config.Config, served, listening map[int]bool, funnel map[int]int, public map[int]bool) []int {
	var ports []int
	for p, on := range served {
		if !on || listening[p] || p == 22 || cfg.Ports[p].Locked || public[p] {
			continue
		}
		if _, ok := funnel[p]; ok {
			continue
		}
		ports = append(ports, p)
	}
	sort.Ints(ports)
	return ports
}
