// Package stale decides which dangling serve forwards tailport may tear down
// without a keypress (kata 9p43): the TUI's grace-period auto-clear and the
// `tailport clean` command share this one rule, so they cannot drift apart.
//
// A forward is DANGLING when it is a plain 1:1 serve of port P (proxying to
// 127.0.0.1:P / localhost:P, nothing else) and nothing listens locally on P.
// A REMAPPED serve (tailnet 15173 -> local 5173) is never eligible: the served
// port's own number says nothing about whether its target is up.
//
// A dangling forward still holds P on the node's tailnet IPs, so any app binding the wildcard address there fails with EADDRINUSE. That is
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
// something the owner set up. Only a plain 1:1 tailport-shaped serve
// qualifies at all (oneToOne, see tsserve.Detail.OneToOne): a remap, a
// non-proxy handler, several paths, TCP forwarding or anything unparseable
// never does.
//
// served: ports with a live serve mapping. listening: ports with any local
// listener, on any bind. funnel: local port -> public ingress port. public:
// ports carrying a Caddy publish or a Cloudflare tunnel. oneToOne: served
// ports whose mapping is the plain 1:1 shape.
func Clearable(cfg config.Config, served, listening map[int]bool, funnel map[int]int, public, oneToOne map[int]bool) []int {
	var ports []int
	for p, on := range served {
		if !on || !oneToOne[p] || listening[p] || p == 22 || cfg.Ports[p].Locked || public[p] {
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
