// Package tsserve wraps the `tailscale` CLI to inspect and control
// `tailscale serve` HTTP mappings. Only plain HTTP mode is used
// (--http=PORT, 1:1 port mapping); funnel is never invoked.
package tsserve

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
)

// ActivePorts returns the set of ports currently exposed via
// `tailscale serve --http`.
//
// Confirmed live against tailscaled 1.98.8 on host-a: a populated
// status looks like {"TCP": {"8123": {"HTTP": true}}, "Web":
// {"host:8123": {"Handlers": {"/": {"Proxy": "http://127.0.0.1:8123"}}}}}.
// This still walks the JSON generically (bare-int keys, or keys ending
// in ":<port>") rather than binding to a strict struct, since that's
// resilient to minor schema differences across tailscaled versions
// across the fleet (host-a/host-b/mac-a/mac-b/mac-c).
func ActivePorts() ([]int, error) {
	out, err := exec.Command("tailscale", "serve", "status", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("tailscale serve status: %w", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parsing serve status: %w", err)
	}

	found := map[int]bool{}
	collectPorts(raw, found)

	ports := make([]int, 0, len(found))
	for p := range found {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	return ports, nil
}

var hostPortRe = regexp.MustCompile(`:(\d+)$`)

func collectPorts(v any, found map[int]bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, sub := range t {
			if p, err := strconv.Atoi(k); err == nil {
				found[p] = true
			} else if m := hostPortRe.FindStringSubmatch(k); m != nil {
				if p, err := strconv.Atoi(m[1]); err == nil {
					found[p] = true
				}
			}
			collectPorts(sub, found)
		}
	case []any:
		for _, sub := range t {
			collectPorts(sub, found)
		}
	}
}

// On exposes localPort tailnet-wide over plain HTTP at the same port
// number (1:1 mapping only, never funnel).
func On(port int) error {
	arg := fmt.Sprintf("--http=%d", port)
	target := strconv.Itoa(port)
	out, err := exec.Command("tailscale", "serve", "--bg", arg, target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tailscale serve --bg %s %s: %w: %s", arg, target, err, out)
	}
	return nil
}

// Off disables serve for port only, leaving any other active mappings
// untouched. Confirmed live on host-a (1.98.8): `tailscale serve
// --http=<port> off` is a real, surgical single-mapping removal — the
// CLI itself prints this as the suggested command after `On`. No
// reset-and-reapply needed.
func Off(port int) error {
	arg := fmt.Sprintf("--http=%d", port)
	out, err := exec.Command("tailscale", "serve", arg, "off").CombinedOutput()
	if err != nil {
		return fmt.Errorf("tailscale serve %s off: %w: %s", arg, err, out)
	}
	return nil
}
