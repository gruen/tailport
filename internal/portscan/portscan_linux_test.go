//go:build linux

package portscan

import "testing"

func TestSplitHostPort(t *testing.T) {
	cases := []struct {
		addr     string
		wantHost string
		wantPort int
		ok       bool
	}{
		{"0.0.0.0:22", "0.0.0.0", 22, true},
		{"100.100.100.100:61584", "100.100.100.100", 61584, true},
		{"[::]:22", "::", 22, true},
		{"[fd7a:115c:a1e0::1]:57619", "fd7a:115c:a1e0::1", 57619, true},
		{"garbage", "", 0, false},
	}
	for _, c := range cases {
		host, port, ok := splitHostPort(c.addr)
		if ok != c.ok || (ok && (port != c.wantPort || host != c.wantHost)) {
			t.Errorf("splitHostPort(%q) = (%q, %d, %v), want (%q, %d, %v)", c.addr, host, port, ok, c.wantHost, c.wantPort, c.ok)
		}
	}
}

// ssFixture mirrors `ss -H -t -l -n -p` output and covers what parseSS must
// get right: a wildcard port on dual-stack v4+v6 rows, a loopback-only port, a
// LAN-IP port, a port bound on BOTH loopback and wildcard (must aggregate up to
// Wildcard), and tailnet-bound sockets (v4 + v6) that must be filtered as
// tailscaled's own. First-seen order is 22, 5432, 8080, 3000.
const ssFixture = `LISTEN 0      128            0.0.0.0:22            0.0.0.0:*    users:(("sshd",pid=100,fd=3))
LISTEN 0      128               [::]:22               [::]:*    users:(("sshd",pid=100,fd=4))
LISTEN 0      128          127.0.0.1:5432          0.0.0.0:*    users:(("postgres",pid=200,fd=5))
LISTEN 0      128        192.168.1.5:8080          0.0.0.0:*    users:(("nginx",pid=300,fd=6))
LISTEN 0      128          127.0.0.1:3000          0.0.0.0:*    users:(("node",pid=400,fd=7))
LISTEN 0      128            0.0.0.0:3000          0.0.0.0:*    users:(("node",pid=400,fd=8))
LISTEN 0      4096       100.101.102.103:8808      0.0.0.0:*    users:(("tailscaled",pid=50,fd=9))
LISTEN 0      4096 [fd7a:115c:a1e0::1]:9999          [::]:*    users:(("tailscaled",pid=50,fd=10))
short line
`

func TestParseSS(t *testing.T) {
	ports, err := parseSS([]byte(ssFixture))
	if err != nil {
		t.Fatalf("parseSS error: %v", err)
	}

	// Dedup + first-seen order preserved; tailnet-only ports absent.
	wantOrder := []int{22, 5432, 8080, 3000}
	if len(ports) != len(wantOrder) {
		t.Fatalf("parsed %d ports, want %d: %+v", len(ports), len(wantOrder), ports)
	}
	for i, want := range wantOrder {
		if ports[i].Number != want {
			t.Errorf("ports[%d].Number = %d, want %d (order): %+v", i, ports[i].Number, want, ports)
		}
	}

	byPort := map[int]Port{}
	for _, p := range ports {
		byPort[p.Number] = p
	}
	for _, tc := range []struct {
		port  int
		proc  string
		scope BindScope
		host  string
	}{
		{22, "sshd", ScopeWildcard, "0.0.0.0"},         // 0.0.0.0 + [::] -> Wildcard
		{5432, "postgres", ScopeLoopback, "127.0.0.1"}, // loopback-only stays Loopback
		{8080, "nginx", ScopeLAN, "192.168.1.5"},       // a specific LAN IP -> LAN
		{3000, "node", ScopeWildcard, "0.0.0.0"},       // 127.0.0.1 + 0.0.0.0 aggregates UP to Wildcard; host follows the wider bind
	} {
		p, ok := byPort[tc.port]
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

	// A port whose ONLY binds are tailnet-range sockets filters to empty.
	if _, ok := byPort[8808]; ok {
		t.Errorf("tailnet-only :8808 should be filtered out; got %+v", ports)
	}
	if _, ok := byPort[9999]; ok {
		t.Errorf("tailnet-only :9999 should be filtered out; got %+v", ports)
	}
}

func TestList(t *testing.T) {
	// Smoke test against the real `ss` binary: sshd should always be
	// listening on port 22 in this environment.
	ports, err := List()
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	found := false
	for _, p := range ports {
		if p.Number == 22 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected port 22 (sshd) in %+v", ports)
	}
}
