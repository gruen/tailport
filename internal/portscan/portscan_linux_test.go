//go:build linux

package portscan

import "testing"

func TestExtractPort(t *testing.T) {
	cases := []struct {
		addr string
		want int
		ok   bool
	}{
		{"0.0.0.0:22", 22, true},
		{"100.100.100.100:61584", 61584, true},
		{"[::]:22", 22, true},
		{"[fd7a:115c:a1e0::1]:57619", 57619, true},
		{"garbage", 0, false},
	}
	for _, c := range cases {
		got, ok := extractPort(c.addr)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("extractPort(%q) = (%d, %v), want (%d, %v)", c.addr, got, ok, c.want, c.ok)
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
