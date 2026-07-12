//go:build linux

package portscan

import (
	"bufio"
	"bytes"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

var procNameRe = regexp.MustCompile(`\(\("([^"]+)"`)

// List enumerates locally listening TCP ports via `ss`. Process names are
// best-effort: ss can only attribute a socket to a process when it's owned
// by the current user (or when running as root), and silently omits the
// name otherwise rather than failing.
func List() ([]Port, error) {
	out, err := exec.Command("ss", "-H", "-t", "-l", "-n", "-p").Output()
	if err != nil {
		out, err = exec.Command("ss", "-H", "-t", "-l", "-n").Output()
		if err != nil {
			return nil, err
		}
	}
	return parseSS(out)
}

// parseSS extracts listening TCP ports from the output of
// `ss -H -t -l -n [-p]`. It is split out from List (mirroring the darwin
// parseLsof) so the aggregation logic can be unit-tested against fixture
// strings without a live `ss`.
//
// A single port can show up on several bind rows (dual-stack 0.0.0.0 + [::],
// or 127.0.0.1 + 0.0.0.0), so instead of first-wins dedup we aggregate each
// port to the WIDEST scope across its non-filtered binds -- a port bound on
// both 127.0.0.1 and 0.0.0.0 IS tailnet-reachable and must read Wildcard, not
// Loopback. First-seen output order is preserved (tests may assert it) by
// tracking port numbers in the order first encountered.
func parseSS(out []byte) ([]Port, error) {
	agg := map[int]*Port{}
	var order []int
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		host, port, ok := splitHostPort(fields[3])
		if !ok {
			continue
		}
		// Skip tailscaled's own serve-proxy sockets (bound to a tailnet
		// address) BEFORE aggregation, so that a real app socket sharing the
		// port (e.g. 127.0.0.1:8808 alongside 100.x:8808) still registers the
		// port as listening regardless of line order. A dangling serve --
		// only tailnet-range sockets on a port -- aggregates to nothing, so
		// its port is never added and reads as not-listening (internal/ui
		// dangling-forward detection depends on this).
		//
		// Known limitation (79xb): an app that binds the node's tailnet IP
		// *directly* is indistinguishable from tailscaled's own listener here
		// and so is dropped/classified-away as tailscaled. Accepted for now;
		// revisit only if it bites.
		if isTailscaleAddr(host) {
			continue
		}
		scope := classifyBindScope(host)

		proc := ""
		if len(fields) > 5 {
			if m := procNameRe.FindStringSubmatch(strings.Join(fields[5:], " ")); m != nil {
				proc = m[1]
			}
		}

		p, ok := agg[port]
		if !ok {
			agg[port] = &Port{Number: port, Process: proc, BindScope: scope}
			order = append(order, port)
			continue
		}
		p.BindScope = widerScope(p.BindScope, scope) // widen to the most-reachable bind
		if p.Process == "" {                         // keep the first non-empty process name
			p.Process = proc
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	ports := make([]Port, 0, len(order))
	for _, port := range order {
		ports = append(ports, *agg[port])
	}
	return ports, nil
}

// splitHostPort splits an `ss` local-address field into its bare host (no
// brackets) and port. Handles bracketed IPv6 (`[fd7a:...]:57619`) and plain
// v4/wildcard (`0.0.0.0:22`, `100.100.100.100:61584`).
func splitHostPort(addr string) (host string, port int, ok bool) {
	var hostStr, portStr string
	switch {
	case strings.Contains(addr, "]:"):
		i := strings.LastIndex(addr, "]:")
		hostStr = strings.TrimPrefix(addr[:i], "[")
		portStr = addr[i+2:]
	case strings.Contains(addr, ":"):
		i := strings.LastIndex(addr, ":")
		hostStr = addr[:i]
		portStr = addr[i+1:]
	default:
		return "", 0, false
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, false
	}
	return hostStr, p, true
}
