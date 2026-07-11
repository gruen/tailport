//go:build darwin

package portscan

import (
	"bufio"
	"bytes"
	"os/exec"
	"strconv"
	"strings"
)

// List enumerates locally listening TCP ports via `lsof`.
func List() ([]Port, error) {
	out, err := exec.Command("lsof", "-iTCP", "-sTCP:LISTEN", "-n", "-P").Output()
	if err != nil {
		return nil, err
	}
	return parseLsof(out)
}

// parseLsof extracts listening TCP ports from the output of
// `lsof -iTCP -sTCP:LISTEN -n -P`. It is split out from List so the macOS
// parsing can be unit-tested against fixture data (portscan_darwin_test.go)
// and exercised natively by the opt-in `[ci darwin]` CI job (5y04) -- the
// darwin path was previously build-only, never run.
func parseLsof(out []byte) ([]Port, error) {
	seen := map[int]bool{}
	var ports []Port
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
		// address) BEFORE the seen[] dedup, so a real app socket sharing
		// the port still registers it as listening. The host is everything
		// before the last colon, with any IPv6 brackets stripped. Mirrors
		// the Linux path.
		host := strings.TrimSuffix(strings.TrimPrefix(name[:idx], "["), "]")
		if isTailscaleAddr(host) {
			continue
		}
		if seen[port] {
			continue
		}
		seen[port] = true
		ports = append(ports, Port{Number: port, Process: proc})
	}
	return ports, scanner.Err()
}
