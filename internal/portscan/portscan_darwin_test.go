//go:build darwin

package portscan

import "testing"

// Sample `lsof -iTCP -sTCP:LISTEN -n -P` output covering the cases the parser
// must get right: wildcard bind, IPv4/IPv6 loopback for the same port (dedup),
// a tailscaled serve-proxy socket on a tailnet IP (must be skipped), and a
// short/garbage line (must be ignored). Device columns are elided with 0x....
const lsofFixture = `COMMAND     PID USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME
launchd       1 root    7u  IPv4 0x1111111111111111      0t0  TCP *:3000 (LISTEN)
node        501   mg   23u  IPv4 0x2222222222222222      0t0  TCP 127.0.0.1:5173 (LISTEN)
node        501   mg   24u  IPv6 0x3333333333333333      0t0  TCP [::1]:5173 (LISTEN)
tailscaled  310 root   12u  IPv4 0x4444444444444444      0t0  TCP 100.101.102.103:8808 (LISTEN)
tailscaled  310 root   13u  IPv6 0x5555555555555555      0t0  TCP [fd7a:115c:a1e0::1]:8808 (LISTEN)
Dropbox     720   mg   30u  IPv4 0x6666666666666666      0t0  TCP 127.0.0.1:17600 (LISTEN)
garbage line
`

func TestParseLsof(t *testing.T) {
	ports, err := parseLsof([]byte(lsofFixture))
	if err != nil {
		t.Fatalf("parseLsof error: %v", err)
	}

	got := map[int]string{}
	for _, p := range ports {
		if _, dup := got[p.Number]; dup {
			t.Errorf("port %d reported more than once (dedup failed): %+v", p.Number, ports)
		}
		got[p.Number] = p.Process
	}

	// Wildcard + loopback app sockets are kept, with their process names.
	for _, tc := range []struct {
		port int
		proc string
	}{
		{3000, "launchd"},
		{5173, "node"}, // IPv4 + IPv6 lines collapse to one entry
		{17600, "Dropbox"},
	} {
		if proc, ok := got[tc.port]; !ok {
			t.Errorf("expected port %d in %+v", tc.port, ports)
		} else if proc != tc.proc {
			t.Errorf("port %d process = %q, want %q", tc.port, proc, tc.proc)
		}
	}

	// tailscaled's serve-proxy sockets (tailnet v4 + v6) must not surface as
	// a listening app port -- this is the whole point of the tailnet filter.
	if _, ok := got[8808]; ok {
		t.Errorf("tailnet-bound :8808 should be filtered out; got %+v", ports)
	}

	// Header and the short "garbage line" must not be parsed as ports.
	if len(ports) != 3 {
		t.Errorf("parsed %d ports, want 3 (3000, 5173, 17600); got %+v", len(ports), ports)
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
