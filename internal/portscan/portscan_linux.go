//go:build linux

package portscan

import (
	"bufio"
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

	seen := map[int]bool{}
	var ports []Port
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
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
		// address) BEFORE the seen[] dedup, so that a real app socket
		// sharing the port (e.g. 127.0.0.1:8808 alongside 100.x:8808) still
		// registers the port as listening regardless of line order. A
		// dangling serve -- only tailnet-range sockets -- filters to empty,
		// so its port is never added and reads as not-listening.
		if isTailscaleAddr(host) {
			continue
		}
		if seen[port] {
			continue
		}
		seen[port] = true

		proc := ""
		if len(fields) > 5 {
			if m := procNameRe.FindStringSubmatch(strings.Join(fields[5:], " ")); m != nil {
				proc = m[1]
			}
		}
		ports = append(ports, Port{Number: port, Process: proc})
	}
	return ports, scanner.Err()
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
