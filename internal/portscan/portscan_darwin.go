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
	// parseSS path.
	agg := map[int]*Port{}
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
		name := fields[8] // e.g. "*:3000", "127.0.0.1:5173", "[fd7a:...]:port"
		idx := strings.LastIndex(name, ":")
		if idx == -1 {
			continue
		}
		port, err := strconv.Atoi(name[idx+1:])
		if err != nil {
			continue
		}
		// Skip tailscaled's own serve-proxy sockets (bound to a tailnet
		// address) BEFORE aggregation, so a real app socket sharing the port
		// still registers it as listening. The host is everything before the
		// last colon, with any IPv6 brackets stripped. A dangling serve --
		// only tailnet-range sockets on a port -- aggregates to nothing, so
		// its port reads as not-listening (internal/ui dangling-forward
		// detection depends on this).
		//
		// Known limitation (79xb): an app that binds the node's tailnet IP
		// *directly* is indistinguishable from tailscaled's own listener here
		// and so is dropped/classified-away as tailscaled. Accepted for now;
		// revisit only if it bites.
		host := strings.TrimSuffix(strings.TrimPrefix(name[:idx], "["), "]")
		if isTailscaleAddr(host) {
			continue
		}
		scope := classifyBindScope(host)

		p, ok := agg[port]
		if !ok {
			agg[port] = &Port{Number: port, Process: proc, BindScope: scope, BindHost: host}
			order = append(order, port)
			continue
		}
		if scope > p.BindScope { // strictly wider bind: adopt its scope AND host
			p.BindScope = scope
			p.BindHost = host
		}
		if p.Process == "" { // keep the first non-empty process name
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
