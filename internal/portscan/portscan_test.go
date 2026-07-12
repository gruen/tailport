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

// TestClassifyBindScope pins the reachability truth table (79xb): who can
// actually reach a socket, derived purely from its bind host. Runs on every
// platform; no live tailscale needed.
func TestClassifyBindScope(t *testing.T) {
	cases := []struct {
		host string
		want BindScope
	}{
		// Loopback: this machine only.
		{"127.0.0.1", ScopeLoopback},
		{"127.0.0.53", ScopeLoopback}, // anywhere in 127.0.0.0/8
		{"::1", ScopeLoopback},

		// Wildcard (unspecified): reaches the tailnet.
		{"0.0.0.0", ScopeWildcard},
		{"::", ScopeWildcard},
		{"*", ScopeWildcard}, // lsof's literal wildcard

		// The node's own tailnet IP.
		{"100.64.0.1", ScopeTailnet},
		{"100.100.100.100", ScopeTailnet},
		{"fd7a:115c:a1e0::1", ScopeTailnet},

		// A specific non-tailnet IP: LAN-reachable, not (by itself) tailnet.
		{"192.168.1.5", ScopeLAN},
		{"10.0.0.5", ScopeLAN},
		{"172.16.3.9", ScopeLAN},
		{"8.8.8.8", ScopeLAN}, // a public IP is still "some specific address"

		// Unparseable, non-"*": the documented conservative default. We never
		// fall back to Wildcard/Tailnet -- that would over-promise reach.
		{"garbage", ScopeLAN},
		{"localhost", ScopeLAN}, // a hostname, not an IP: can't confirm loopback
		{"", ScopeLAN},
	}
	for _, c := range cases {
		if got := classifyBindScope(c.host); got != c.want {
			t.Errorf("classifyBindScope(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// TestWiderScope guards the aggregation ranking used to collapse a port's
// multiple bind rows to one honest reachability. Widest first:
// Wildcard > Tailnet > LAN > Loopback > Unknown, and the operation is
// commutative.
func TestWiderScope(t *testing.T) {
	cases := []struct {
		a, b, want BindScope
	}{
		{ScopeLoopback, ScopeWildcard, ScopeWildcard}, // the key case: 127.0.0.1 + 0.0.0.0 -> Wildcard
		{ScopeLoopback, ScopeLoopback, ScopeLoopback}, // loopback-only stays loopback
		{ScopeLAN, ScopeLoopback, ScopeLAN},
		{ScopeWildcard, ScopeLAN, ScopeWildcard},
		{ScopeTailnet, ScopeLAN, ScopeTailnet},
		{ScopeUnknown, ScopeLoopback, ScopeLoopback}, // zero value never wins
		{ScopeUnknown, ScopeUnknown, ScopeUnknown},
	}
	for _, c := range cases {
		if got := widerScope(c.a, c.b); got != c.want {
			t.Errorf("widerScope(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
		if got := widerScope(c.b, c.a); got != c.want { // commutative
			t.Errorf("widerScope(%v, %v) = %v, want %v (commutativity)", c.b, c.a, got, c.want)
		}
	}
}
