package portscan

import "testing"

// TestIsTailscaleAddr guards the address-range filter that distinguishes
// tailscaled's serve-proxy sockets from real local app sockets. This runs
// on every platform and needs no live tailscale. The boundary cases
// (100.63.x / 100.128.x) verify a true /10 CIDR check, not a naive "100."
// prefix match.
func TestIsTailscaleAddr(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		// Local / wildcard app sockets -- never filtered.
		{"127.0.0.1", false},
		{"0.0.0.0", false},
		{"::1", false},
		{"::", false},
		{"*", false},
		{"192.168.1.10", false}, // LAN
		{"10.0.0.5", false},     // LAN
		{"", false},
		{"garbage", false},

		// Tailscale CGNAT range 100.64.0.0/10 -- filtered.
		{"100.64.0.0", true},
		{"100.100.100.100", true},
		{"100.127.255.255", true},

		// Just outside 100.64.0.0/10 -- NOT filtered (proves /10, not "100.").
		{"100.63.255.255", false},
		{"100.128.0.0", false},
		{"100.0.0.1", false},

		// Tailscale ULA fd7a:115c:a1e0::/48 -- filtered.
		{"fd7a:115c:a1e0::1", true},
		{"fd7a:115c:a1e0:ab12::1", true},

		// Other IPv6 -- NOT filtered.
		{"fd7a:115c:a1e1::1", false}, // one nibble past the /48
		{"2001:db8::1", false},
		{"fe80::1", false},
	}
	for _, c := range cases {
		if got := isTailscaleAddr(c.host); got != c.want {
			t.Errorf("isTailscaleAddr(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}
