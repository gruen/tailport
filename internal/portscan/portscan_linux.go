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
		port, ok := extractPort(fields[3])
		if !ok || seen[port] {
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

func extractPort(addr string) (int, bool) {
	var portStr string
	switch {
	case strings.Contains(addr, "]:"):
		portStr = addr[strings.LastIndex(addr, "]:")+2:]
	case strings.Contains(addr, ":"):
		portStr = addr[strings.LastIndex(addr, ":")+1:]
	default:
		return 0, false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, false
	}
	return port, true
}
