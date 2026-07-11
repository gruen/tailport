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
