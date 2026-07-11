// Package tsserve wraps the `tailscale` CLI to inspect and control
// `tailscale serve` and `tailscale funnel`.
//
// Serve is the default, tailnet-only path: plain HTTP (--http=PORT), a
// strict 1:1 port mapping (exposed tailnet port == local port). Funnel is
// the opt-in public-internet path (see FunnelOn), gated in the UI behind a
// strong confirm; it is necessarily HTTPS and restricted by Tailscale to
// ingress ports 443/8443/10000, so it is exempt from the 1:1 rule. Both are
// backed by the same ServeConfig JSON: a funnel is a serve handler with its
// host:port flagged in AllowFunnel.
package tsserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
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
	out, err := statusJSON()
	if err != nil {
		return nil, err
	}
	return activePortsFrom(out)
}

// activePortsFrom parses the served (tailnet) local ports out of a ServeConfig
// JSON blob. Split from ActivePorts so Status can reuse a single fetch.
func activePortsFrom(out []byte) ([]int, error) {
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parsing serve status: %w", err)
	}

	found := map[int]bool{}
	collectPorts(raw, found)

	// A funnel's public ingress (host:443/8443/10000, flagged in AllowFunnel)
	// is not a 1:1 tailnet serve of a local port -- it's the public side of a
	// funnel, whose local target is reported by FunnelStatus instead. Drop
	// those public ports so they don't masquerade as served local ports.
	for pub := range funnelPublicPorts(raw) {
		delete(found, pub)
	}

	ports := make([]int, 0, len(found))
	for p := range found {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	return ports, nil
}

// Status fetches the ServeConfig ONCE and returns both the tailnet-serve
// local ports and the funnel (local->public) map. A refresh should prefer this
// over calling ActivePorts + FunnelStatus separately, which would run
// `tailscale serve status --json` twice for the same data (e40f).
func Status() (active []int, funnel map[int]int, err error) {
	out, err := statusJSON()
	if err != nil {
		return nil, nil, err
	}
	if active, err = activePortsFrom(out); err != nil {
		return nil, nil, err
	}
	if funnel, err = parseFunnel(out); err != nil {
		return nil, nil, err
	}
	return active, funnel, nil
}

// statusJSON fetches the current ServeConfig as JSON. Serve status carries
// the full config -- including AllowFunnel -- so a single call reconciles
// both serve (ActivePorts) and funnel (FunnelStatus) state.
func statusJSON() ([]byte, error) {
	out, err := exec.Command("tailscale", "serve", "status", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("tailscale serve status: %w", err)
	}
	return out, nil
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

// funnelPublicPorts returns the set of public ingress ports currently
// funnelled, read from the ServeConfig's AllowFunnel map (keys are
// "host:port" flagged true). These are the public side of a funnel.
func funnelPublicPorts(raw map[string]any) map[int]bool {
	out := map[int]bool{}
	af, ok := raw["AllowFunnel"].(map[string]any)
	if !ok {
		return out
	}
	for hp, v := range af {
		if allowed, _ := v.(bool); !allowed {
			continue
		}
		if m := hostPortRe.FindStringSubmatch(hp); m != nil {
			if p, err := strconv.Atoi(m[1]); err == nil {
				out[p] = true
			}
		}
	}
	return out
}

// FunnelStatus returns the currently funnelled ports as a map from local
// target port to public ingress port (e.g. {3000: 443}). It reconciles the
// UI's public state on refresh, mirroring ActivePorts for serve.
func FunnelStatus() (map[int]int, error) {
	out, err := statusJSON()
	if err != nil {
		return nil, err
	}
	return parseFunnel(out)
}

// parseFunnel extracts local->public funnel mappings from a ServeConfig JSON
// blob: for every host:port flagged in AllowFunnel, the public port is the
// key's port suffix and the local target is the port in that handler's proxy
// URL (Web[hostport].Handlers[*].Proxy, e.g. "http://127.0.0.1:3000"). Split
// out from FunnelStatus so it can be unit-tested without a live tailscaled.
func parseFunnel(out []byte) (map[int]int, error) {
	var cfg struct {
		Web map[string]struct {
			Handlers map[string]struct {
				Proxy string `json:"Proxy"`
			} `json:"Handlers"`
		} `json:"Web"`
		AllowFunnel map[string]bool `json:"AllowFunnel"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		return nil, fmt.Errorf("parsing funnel status: %w", err)
	}
	res := map[int]int{}
	for hp, allowed := range cfg.AllowFunnel {
		if !allowed {
			continue
		}
		pub := portSuffix(hp)
		if pub == 0 {
			continue
		}
		local := pub // fallback if the proxy target can't be read
		if w, ok := cfg.Web[hp]; ok {
			for _, h := range w.Handlers {
				if p := portSuffix(h.Proxy); p != 0 {
					local = p
					break
				}
			}
		}
		res[local] = pub
	}
	return res, nil
}

// portSuffix returns the trailing ":<port>" number of s (e.g. a "host:443"
// key or an "http://127.0.0.1:3000" proxy URL), or 0 if there isn't one.
func portSuffix(s string) int {
	if m := hostPortRe.FindStringSubmatch(s); m != nil {
		if p, err := strconv.Atoi(m[1]); err == nil {
			return p
		}
	}
	return 0
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

// FunnelPorts is the fixed set of public ingress ports Tailscale permits for
// funnel, in the order tailport auto-assigns them. Funnel cannot listen on
// any other port, and a node can run at most len(FunnelPorts) funnels.
var FunnelPorts = []int{443, 8443, 10000}

// errFunnelNotEnabled is returned when the tailnet doesn't permit funnel
// (missing HTTPS certs or the Funnel node attribute). Notably this also covers
// the case where the CLI *hangs* waiting for the operator to enable funnel in
// the admin console: `tailscale funnel` blocks on that gate even with --yes,
// so funnelExec bounds it with a timeout and maps the timeout here rather than
// letting a caller (e.g. the TUI toggle) freeze indefinitely.
var errFunnelNotEnabled = errors.New("funnel isn't enabled for this tailnet -- enable HTTPS certificates and the Funnel node attribute in the admin console")

// funnelTimeout bounds a `tailscale funnel` invocation. A real --bg funnel
// call returns in well under a second; the only thing that takes longer is the
// enablement gate, which blocks forever, so a generous ceiling cleanly
// separates "working" from "not enabled / hung".
const funnelTimeout = 15 * time.Second

// funnelExec runs `tailscale funnel <args...>` under funnelTimeout. A deadline
// overrun is reported as errFunnelNotEnabled (the CLI was blocking on the
// admin-console enablement gate); other failures are returned verbatim with
// their combined output for funnelError to translate.
func funnelExec(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), funnelTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tailscale", append([]string{"funnel"}, args...)...).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return out, errFunnelNotEnabled
	}
	return out, err
}

// FunnelOn exposes localPort to the PUBLIC INTERNET over HTTPS at publicPort
// (one of FunnelPorts). Unlike serve, the result is reachable by anyone on
// the internet; Tailscale terminates TLS with the node's ts.net cert and
// proxies to the plain-HTTP local target. Mirrors On's `serve --bg` pattern
// with `funnel --bg`; --yes suppresses tailscale's own per-command prompt
// (the UI's strong confirm is the real gate).
func FunnelOn(localPort, publicPort int) error {
	arg := fmt.Sprintf("--https=%d", publicPort)
	target := strconv.Itoa(localPort)
	out, err := funnelExec("--bg", "--yes", arg, target)
	if err != nil {
		return funnelError(fmt.Sprintf("tailscale funnel --bg %s %s", arg, target), err, out)
	}
	return nil
}

// FunnelOff removes the public funnel on publicPort and returns localPort to
// tailnet-only serve (it does NOT drop to fully-unexposed -- that's the
// 'p'-off behaviour chosen for yt69). Two steps: drop the public ingress,
// then (re)assert the plain-HTTP tailnet serve on the local port, so the port
// ends tailnet-served regardless of what `funnel ... off` leaves behind.
func FunnelOff(localPort, publicPort int) error {
	arg := fmt.Sprintf("--https=%d", publicPort)
	out, err := funnelExec(arg, "off")
	if err != nil {
		return funnelError(fmt.Sprintf("tailscale funnel %s off", arg), err, out)
	}
	return On(localPort)
}

// funnelError translates a failed funnel command into a user-facing error.
// The "funnel isn't enabled" family (a timeout on the enablement gate, or the
// CLI's own "not enabled / not available / node attribute" text) collapses to
// one actionable message; anything else is returned with its CLI output.
func funnelError(cmd string, err error, out []byte) error {
	if errors.Is(err, errFunnelNotEnabled) {
		return errFunnelNotEnabled
	}
	low := strings.ToLower(string(out))
	if strings.Contains(low, "funnel") && (strings.Contains(low, "not enabled") ||
		strings.Contains(low, "not available") || strings.Contains(low, "node attribute")) {
		return errFunnelNotEnabled
	}
	return fmt.Errorf("%s: %w: %s", cmd, err, strings.TrimSpace(string(out)))
}

// FQDN returns this node's fully-qualified MagicDNS name (e.g.
// "host.tailnet.ts.net"), used to build the public funnel URL shown in the
// confirm. Falls back to an empty string on error; callers degrade to a
// hostless URL rather than failing the whole refresh.
func FQDN() (string, error) {
	out, err := exec.Command("tailscale", "status", "--json").Output()
	if err != nil {
		return "", fmt.Errorf("tailscale status: %w", err)
	}
	var s struct {
		Self struct {
			DNSName string `json:"DNSName"`
		} `json:"Self"`
	}
	if err := json.Unmarshal(out, &s); err != nil {
		return "", fmt.Errorf("parsing status: %w", err)
	}
	return strings.TrimSuffix(s.Self.DNSName, "."), nil
}

// PublicURL builds the public HTTPS URL a funnel is reachable at. Port 443 is
// implicit (https://host); 8443/10000 are shown explicitly (https://host:port).
func PublicURL(fqdn string, publicPort int) string {
	if publicPort == 443 {
		return "https://" + fqdn
	}
	return fmt.Sprintf("https://%s:%d", fqdn, publicPort)
}
