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

// TestAccepts127 pins the exact set of bind hosts `tailscale serve` can
// actually reach (2z0v(b)): serve always dials the literal address
// 127.0.0.1:PORT (tsserve.go:33), so this is a narrower, more literal
// question than classifyBindScope's reachability-scope truth table -- several
// hosts that classify as ScopeLoopback or ScopeWildcard still don't accept a
// 127.0.0.1 dial. Runs on every platform; no live ss/lsof needed.
func TestAccepts127(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		// Accepts a 127.0.0.1 dial.
		{"*", true},                // lsof's / ss's wildcard spelling
		{"0.0.0.0", true},          // IPv4 ANY
		{"127.0.0.1", true},        // the exact address
		{"::ffff:127.0.0.1", true}, // IPv4-mapped IPv6 form of the same address

		// Loopback-scoped (or wildcard-adjacent) but does NOT accept.
		{"::1", false},       // IPv6 loopback -- a distinct address, not 127.0.0.1
		{"::", false},        // V6ONLY unspecified (Linux ss: "[::]", not "*") -- see classifyBindScope's note
		{"127.0.0.2", false}, // non-.1 loopback: ScopeLoopback, but not THE address serve dials

		// Never accepts.
		{"192.168.1.5", false},
		{"100.64.0.1", false},
		{"fd7a:115c:a1e0::1", false},
		{"", false},
		{"garbage", false},
	}
	for _, c := range cases {
		if got := accepts127(c.host); got != c.want {
			t.Errorf("accepts127(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// TestSplitZone pins kata np07's zone policy, shared by the ss and lsof
// parsers. A device zone must never reach classifyBindScope/accepts127 (they
// only parse bare addresses): leaving it in place made ParseIP fail, which
// fell back to ScopeLAN and produced an invalid "100.110.98.29%tailscale0"
// LAN URL. The zone survives in the returned bind form ONLY for an IPv6
// link-local address, where RFC 4007 scope is required to dial at all.
func TestSplitZone(t *testing.T) {
	cases := []struct {
		host     string
		wantBare string
		wantZone string
	}{
		// No zone at all.
		{"0.0.0.0", "0.0.0.0", ""},
		{"fe80::1", "fe80::1", ""},

		// Zone dropped: IPv4 (whatever the interface) and non-link-local IPv6.
		{"100.110.98.29%tailscale0", "100.110.98.29", ""}, // the moth :5173 row
		{"127.0.0.53%lo", "127.0.0.53", ""},               // systemd-resolved
		{"192.168.1.5%eth0", "192.168.1.5", ""},
		{"fd7a:115c:a1e0::1%tailscale0", "fd7a:115c:a1e0::1", ""},
		{"::1%lo", "::1", ""},

		// Zone kept: IPv6 link-local, not dialable without it.
		{"fe80::1%eth0", "fe80::1", "eth0"},
		{"fe80::2eec:b657:f571:8457%eno2", "fe80::2eec:b657:f571:8457", "eno2"},

		// Unparseable / empty zone: the zone is dropped, never kept.
		{"example%foo", "example", ""},
		{"fe80::1%", "fe80::1", ""},
	}
	for _, c := range cases {
		bare, zone := splitZone(c.host)
		if bare != c.wantBare || zone != c.wantZone {
			t.Errorf("splitZone(%q) = (%q, %q), want (%q, %q)", c.host, bare, zone, c.wantBare, c.wantZone)
		}
	}
}

// TestClassifyBindDeviceRestriction pins classifyBind's device rule (kata
// np07, roborev 6053): a "%iface" suffix restricts the socket to that device,
// so the DEVICE -- not the address -- decides reachability. In particular a
// zoned wildcard is NOT a wildcard: "0.0.0.0%eth0" accepts nothing on
// loopback or tailscale0, so it must never yield localhost/tailnet routes or
// clear No127.
func TestClassifyBindDeviceRestriction(t *testing.T) {
	orig := ifaceAddr
	ifaceAddr = func(name string) string {
		if name == "eth0" {
			return "192.168.1.50"
		}
		return ""
	}
	t.Cleanup(func() { ifaceAddr = orig })

	tests := []struct {
		in       string
		bare     string
		zoned    bool
		scope    BindScope
		bindHost string
		ok127    bool
	}{
		// unzoned: unchanged behavior
		{"0.0.0.0", "0.0.0.0", false, ScopeWildcard, "0.0.0.0", true},
		{"127.0.0.1", "127.0.0.1", false, ScopeLoopback, "127.0.0.1", true},
		// loopback device
		{"127.0.0.53%lo", "127.0.0.53", true, ScopeLoopback, "127.0.0.53", false},
		{"0.0.0.0%lo", "0.0.0.0", true, ScopeLoopback, "0.0.0.0", true},
		{"127.0.0.1%lo0", "127.0.0.1", true, ScopeLoopback, "127.0.0.1", true},
		// tailscale device
		{"100.110.98.29%tailscale0", "100.110.98.29", true, ScopeTailnet, "100.110.98.29", false},
		{"0.0.0.0%tailscale0", "0.0.0.0", true, ScopeTailnet, "", false},
		// any other device: that LAN interface only
		{"0.0.0.0%eth0", "0.0.0.0", true, ScopeLAN, "192.168.1.50", false},
		{"*%eth0", "*", true, ScopeLAN, "192.168.1.50", false},
		{"0.0.0.0%wlan9", "0.0.0.0", true, ScopeLAN, "", false}, // no address on it
		{"192.168.1.5%eth0", "192.168.1.5", true, ScopeLAN, "192.168.1.5", false},
		{"127.0.0.1%eth0", "127.0.0.1", true, ScopeLAN, "", false}, // loopback addr on a non-loopback device: unreachable
		{"fe80::1%eth0", "fe80::1", true, ScopeLAN, "fe80::1%eth0", false},
	}
	for _, tt := range tests {
		bare, zoned, scope, bindHost, ok127 := classifyBind(tt.in)
		if bare != tt.bare || zoned != tt.zoned || scope != tt.scope || bindHost != tt.bindHost || ok127 != tt.ok127 {
			t.Errorf("classifyBind(%q) = (%q, %v, %v, %q, %v), want (%q, %v, %v, %q, %v)",
				tt.in, bare, zoned, scope, bindHost, ok127, tt.bare, tt.zoned, tt.scope, tt.bindHost, tt.ok127)
		}
	}
}
