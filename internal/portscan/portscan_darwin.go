//go:build darwin

package portscan

import (
	"bufio"
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

	seen := map[int]bool{}
	var ports []Port
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
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
		// the Linux path; unverified on macOS (no Mac available here).
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
