//go:build darwin

package portscan

import "testing"

// Sample `lsof -iTCP -sTCP:LISTEN -n -P` output covering the cases the parser
// must get right, mirroring the Linux ssFixture scenarios: a wildcard bind, a
// loopback-only port on dual-stack v4+v6 rows (aggregates to Loopback), a
// LAN-IP port, a port bound on BOTH loopback and wildcard (aggregates up to
// Wildcard), tailscaled serve-proxy sockets on tailnet IPs (must be skipped),
// and a short/garbage line (must be ignored). Device columns are elided.
const lsofFixture = `COMMAND     PID USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME
launchd       1 root    7u  IPv4 0x1111111111111111      0t0  TCP *:3000 (LISTEN)
node        501   mg   23u  IPv4 0x2222222222222222      0t0  TCP 127.0.0.1:5173 (LISTEN)
node        501   mg   24u  IPv6 0x3333333333333333      0t0  TCP [::1]:5173 (LISTEN)
tailscaled  310 root   12u  IPv4 0x4444444444444444      0t0  TCP 100.101.102.103:8808 (LISTEN)
tailscaled  310 root   13u  IPv6 0x5555555555555555      0t0  TCP [fd7a:115c:a1e0::1]:8808 (LISTEN)
Dropbox     720   mg   30u  IPv4 0x6666666666666666      0t0  TCP 127.0.0.1:17600 (LISTEN)
nginx       800 root   40u  IPv4 0x7777777777777777      0t0  TCP 192.168.1.5:8080 (LISTEN)
vite        900   mg   50u  IPv4 0x8888888888888888      0t0  TCP 127.0.0.1:4000 (LISTEN)
vite        900   mg   51u  IPv4 0x9999999999999999      0t0  TCP *:4000 (LISTEN)
garbage line
`

func TestParseLsof(t *testing.T) {
	ports, err := parseLsof([]byte(lsofFixture))
	if err != nil {
		t.Fatalf("parseLsof error: %v", err)
	}

	got := map[int]Port{}
	for _, p := range ports {
		if _, dup := got[p.Number]; dup {
			t.Errorf("port %d reported more than once (dedup failed): %+v", p.Number, ports)
		}
		got[p.Number] = p
	}

	// Kept app sockets, with process names AND the widest-scope classification.
	for _, tc := range []struct {
		port  int
		proc  string
		scope BindScope
		host  string
	}{
		{3000, "launchd", ScopeWildcard, "*"},          // *:3000 -> Wildcard
		{5173, "node", ScopeLoopback, "127.0.0.1"},     // 127.0.0.1 + [::1] collapse; loopback-only stays Loopback; tie keeps first-seen host
		{17600, "Dropbox", ScopeLoopback, "127.0.0.1"}, // loopback
		{8080, "nginx", ScopeLAN, "192.168.1.5"},       // a specific LAN IP -> LAN
		{4000, "vite", ScopeWildcard, "*"},             // 127.0.0.1 + * aggregates UP to Wildcard; host follows the wider bind
	} {
		p, ok := got[tc.port]
		if !ok {
			t.Errorf("expected port %d in %+v", tc.port, ports)
			continue
		}
		if p.Process != tc.proc {
			t.Errorf("port %d process = %q, want %q", tc.port, p.Process, tc.proc)
		}
		if p.BindScope != tc.scope {
			t.Errorf("port %d scope = %v, want %v", tc.port, p.BindScope, tc.scope)
		}
		if p.BindHost != tc.host {
			t.Errorf("port %d bindhost = %q, want %q", tc.port, p.BindHost, tc.host)
		}
	}

	// tailscaled's serve-proxy sockets (tailnet v4 + v6) must not surface as
	// a listening app port -- this is the whole point of the tailnet filter.
	if _, ok := got[8808]; ok {
		t.Errorf("tailnet-bound :8808 should be filtered out; got %+v", ports)
	}

	// Header and the short "garbage line" must not be parsed as ports.
	if len(ports) != 5 {
		t.Errorf("parsed %d ports, want 5 (3000, 5173, 17600, 8080, 4000); got %+v", len(ports), ports)
	}
}

// TestListDarwin is the native smoke test: it runs the real `lsof` binary on
// the macOS runner (only reachable via the opt-in `[ci darwin]` job) and
// confirms List() executes and parses without error. It deliberately does not
// assert a specific port -- a CI runner's listener set is not guaranteed -- so
// it stays green while still exercising the exec + parse path end to end.
func TestListDarwin(t *testing.T) {
	ports, err := List()
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	t.Logf("lsof reported %d listening port(s): %+v", len(ports), ports)
}
