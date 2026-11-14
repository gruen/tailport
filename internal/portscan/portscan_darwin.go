//go:build darwin

package portscan

import (
	"bufio"
	"bytes"
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

// List enumerates locally listening TCP ports via `lsof`.
func List() ([]Port, error) {
	out, err := exec.Command("lsof", "-iTCP", "-sTCP:LISTEN", "-n", "-P").Output()
	if err != nil {
		// `lsof` exits non-zero in two benign cases: when nothing matches the
		// filter (no listening sockets) and when it prints usable output to
		// stdout while also warning on stderr (e.g. it can't stat a mount).
		// Neither is a real failure -- on an *exec.ExitError we parse whatever
		// stdout we captured, which may be empty (an empty port list). Only a
		// genuine failure (lsof missing, killed, any non-ExitError) propagates.
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return nil, err
		}
	}
	return parseLsof(out)
}

// parseLsof extracts listening TCP ports from the output of
// `lsof -iTCP -sTCP:LISTEN -n -P`. It is split out from List so the macOS
// parsing can be unit-tested against fixture data (portscan_darwin_test.go)
// and exercised natively by the opt-in `[ci darwin]` CI job (5y04) -- the
// darwin path was previously build-only, never run.
func parseLsof(out []byte) ([]Port, error) {
	// A single port can appear on several bind rows (dual-stack *:P + [::]:P,
	// or 127.0.0.1:P + *:P), so instead of first-wins dedup we aggregate each
	// port to the WIDEST scope across its non-filtered binds -- a port bound
	// on both 127.0.0.1 and * IS tailnet-reachable and must read Wildcard, not
	// Loopback. First-seen output order is preserved (tests may assert it) by
	// tracking port numbers in the order first encountered. Mirrors the Linux
	// parseSS path. A loopback bind row also sets Loopback=true regardless of
	// what wins BindScope (t12m), so a port bound on BOTH loopback and
	// something wider (e.g. a specific LAN IP) doesn't silently lose its
	// localhost reachability.
	//
	// No127 is computed the same way, across every row seen for the port
	// BEFORE any of them is discarded by the widest-bind aggregation below: it
	// starts true (not accepted) on the port's first row and flips to false
	// the moment ANY row accepts a 127.0.0.1 dial (2z0v(b)), regardless of
	// which row's scope ends up winning BindScope.
	agg := map[int]*Port{}
	acc127 := pidAcc{}
	unattr := unattrAcc{}
	seen := map[int]bool{}
	var order []int
	scanner := bufio.NewScanner(bytes.NewReader(out))
	first := true
	for scanner.Scan() {
		if first {
			first = false // header row: COMMAND PID USER FD TYPE DEVICE SIZE/OFF NODE NAME
			continue
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) < 9 {
			continue
		}
		proc := fields[0]
		pid, _ := strconv.Atoi(fields[1]) // best-effort; 0 if unparseable
		name := fields[8]                 // e.g. "*:3000", "127.0.0.1:5173", "[fd7a:...]:port"
		idx := strings.LastIndex(name, ":")
		if idx == -1 {
			continue
		}
		port, err := strconv.Atoi(name[idx+1:])
		if err != nil {
			continue
		}
		// tailscaled's own serve-proxy sockets (bound to a tailnet address)
		// are dropped BEFORE aggregation, so a real app socket sharing the
		// port still registers it as listening. The host is everything before
		// the last colon, with any IPv6 brackets stripped. A dangling serve --
		// only tailscaled's tailnet-range sockets on a port -- aggregates to
		// nothing, so its port reads as not-listening (internal/ui
		// dangling-forward detection depends on this).
		//
		// The same "%iface" zone handling as the Linux parser (kata np07):
		// whether macOS lsof ever emits a zone is unverified here, but if it
		// does, leaving it in place would make net.ParseIP fail and resurrect
		// the bogus LAN row that ticket removed. So split it identically.
		//
		// Ownership decides whose a tailnet-IP row is (kata nvv6), zoned or
		// not: Tailscale's own processes (tailscaled, the app's network
		// extension -- see isTailscaleLsofCommand) own its serve proxy and are
		// dropped; any other visible owner is the app's own tailnet bind and
		// is kept. A row with no owner (lsof normally always names one) is
		// FLAGGED for ResolveTailnetRows, exactly as on Linux.
		rawHost := strings.TrimSuffix(strings.TrimPrefix(name[:idx], "["), "]")
		_, _, scope, bindHost, ok127 := classifyBind(rawHost)
		if canBeTailscaledProxy(rawHost) {
			if isTailscaleLsofCommand(proc) {
				continue
			}
			if proc == "" {
				unattr.note(port, bindHost, scope)
				if !seen[port] {
					seen[port] = true
					order = append(order, port)
				}
				continue
			}
		}
		// lsof prints one line per (process, socket), so a socket shared by several
		// processes arrives as several rows and accumulates several distinct pids.
		var owners []int
		if pid > 0 {
			owners = []int{pid}
		}
		acc127.note(port, owners, ok127)

		p, ok := agg[port]
		if !ok {
			agg[port] = &Port{Number: port, Process: proc, Pid: pid, BindScope: scope, BindHost: bindHost, Loopback: scope == ScopeLoopback, No127: !ok127}
			if !seen[port] {
				seen[port] = true
				order = append(order, port)
			}
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
		p := agg[port]
		if p == nil { // only unattributed tailnet rows (kata nvv6)
			p = &Port{Number: port}
		}
		acc127.apply(p)
		unattr.apply(p)
		ports = append(ports, *p)
	}
	return ports, nil
}
