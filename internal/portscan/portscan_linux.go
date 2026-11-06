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

// procRe binds a process's name and pid together atomically (kata 9094):
// name and pid are captured from the SAME `(("name",pid=N` match, rather than
// two independent regexes scanning the whole users:(...) field separately.
// The independent-scan form was correct for every real `ss` output (a
// process name can't literally contain "pid=<digits>") but mis-paired on
// paper if it ever did; one combined regex makes that impossible by
// construction.
var procRe = regexp.MustCompile(`\(\("([^"]+)",pid=(\d+)`)

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
// tracking port numbers in the order first encountered. A loopback bind row
// also sets Loopback=true regardless of what wins BindScope (t12m), so a port
// bound on BOTH loopback and something wider (e.g. a specific LAN IP) doesn't
// silently lose its localhost reachability.
//
// No127 is computed the same way, across every row seen for the port BEFORE
// any of them is discarded by the widest-bind aggregation above: it starts
// true (not accepted) on the port's first row and flips to false the moment
// ANY row accepts a 127.0.0.1 dial (2z0v(b)), regardless of which row's scope
// ends up winning BindScope.
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
		// Strip the "%iface" zone before anything looks at the address (kata
		// np07): with it attached, the address doesn't parse, so a tailnet
		// listener fell through to ScopeLAN and rendered a bogus LAN route
		// like "http://100.110.98.29%tailscale0:5173". classifyBind applies the
		// device restriction (see its doc comment).
		bare, zoned, scope, bindHost, ok127 := classifyBind(host)
		// Skip tailscaled's own serve-proxy sockets (bound to a tailnet
		// address) BEFORE aggregation, so that a real app socket sharing the
		// port (e.g. 127.0.0.1:8808 alongside 100.x:8808) still registers the
		// port as listening regardless of line order. A dangling serve --
		// only tailnet-range sockets on a port -- aggregates to nothing, so
		// its port is never added and reads as not-listening (internal/ui
		// dangling-forward detection depends on this).
		//
		// Known limitation (79xb): an app that binds the node's tailnet IP
		// *directly* -- WITHOUT a device zone -- is indistinguishable from
		// tailscaled's own listener here and so is dropped/classified-away as
		// tailscaled. Accepted for now; revisit only if it bites.
		//
		// A DEVICE-ZONED tailnet listener ("100.x%tailscale0") is ambiguous
		// (kata np07): newer tailscaled binds its serve proxy to the
		// tailscale0 device, but an app can also pin itself there with
		// SO_BINDTODEVICE. Ownership tells them apart: tailscaled runs as
		// root, so an unprivileged `ss` shows NO owning process for its
		// sockets (or "tailscaled" when run as root), while an app run by the
		// user shows its own. Zoned rows with no process or tailscaled are
		// skipped exactly like the zoneless case; a zoned row owned by some
		// other process is the app's own tailnet bind and is kept. The same
		// 79xb limitation therefore extends to an app run by ANOTHER user
		// (e.g. root) pinned to tailscale0: ss can't attribute it either, so
		// it is treated as tailscaled's. Telling those apart needs the serve
		// state (an unattributed tailnet listener on a port that isn't served
		// can't be tailscaled's) -- tracked as kata nvv6.
		proc := ""
		pid := 0
		if len(fields) > 5 {
			joined := strings.Join(fields[5:], " ")
			if m := procRe.FindStringSubmatch(joined); m != nil {
				proc = m[1]
				if v, err := strconv.Atoi(m[2]); err == nil {
					pid = v
				}
			}
		}
		if isTailscaleAddr(bare) && (!zoned || proc == "" || proc == "tailscaled") {
			continue
		}

		p, ok := agg[port]
		if !ok {
			agg[port] = &Port{Number: port, Process: proc, Pid: pid, BindScope: scope, BindHost: bindHost, Loopback: scope == ScopeLoopback, No127: !ok127}
			order = append(order, port)
			continue
		}
		if scope > p.BindScope { // strictly wider bind: adopt its scope AND host
			p.BindScope = scope
			p.BindHost = bindHost
		}
		if scope == ScopeLoopback { // t12m: remember a loopback bind even when a wider one wins BindScope
			p.Loopback = true
		}
		if ok127 { // 2z0v(b): ANY row accepting 127.0.0.1 clears No127, regardless of which row wins BindScope
			p.No127 = false
		}
		if p.Process == "" { // keep the first non-empty process name, paired with its pid
			p.Process = proc
			p.Pid = pid
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
// brackets) and port. Handles bracketed IPv6 (`[fd7a:...]:57619`), plain
// v4/wildcard (`0.0.0.0:22`, `100.100.100.100:61584`), and an IPv6 zone --
// which ss renders OUTSIDE the brackets, `[fe80::1]%eth0:8080` (observed live,
// kata np07), as well as the inside-the-brackets fixture spelling
// `[fe80::1%eth0]:8080`. The returned host always carries the zone as
// "addr%iface" so splitZone sees one shape.
func splitHostPort(addr string) (host string, port int, ok bool) {
	// Normalise ss's "[addr]%iface:port" spelling to "[addr%iface]:port" --
	// without this the zone lands outside the brackets, ends up attached to a
	// still-bracketed host, and every downstream parse (splitZone, ParseIP)
	// fails.
	if strings.HasPrefix(addr, "[") {
		if i := strings.Index(addr, "]%"); i >= 0 {
			if j := strings.IndexByte(addr[i+2:], ':'); j >= 0 {
				addr = "[" + addr[1:i] + "%" + addr[i+2:i+2+j] + "]" + addr[i+2+j:]
			}
		}
	}
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
