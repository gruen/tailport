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
		// Zone suffix survives splitHostPort (splitZone owns that policy).
		{"100.110.98.29%tailscale0:5173", "100.110.98.29%tailscale0", 5173, true},
		{"[fe80::1%eth0]:8080", "fe80::1%eth0", 8080, true},
		// Real `ss` spelling (observed live, kata np07): the zone sits AFTER
		// the closing bracket, not inside it. splitHostPort normalises it into
		// the same "addr%iface" host form so splitZone sees one shape.
		{"[fe80::2eec:b657:f571:8457]%eno2:18891", "fe80::2eec:b657:f571:8457%eno2", 18891, true},
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
		pid   int
		scope BindScope
		host  string
	}{
		{22, "sshd", 100, ScopeWildcard, "0.0.0.0"},         // 0.0.0.0 + [::] -> Wildcard
		{5432, "postgres", 200, ScopeLoopback, "127.0.0.1"}, // loopback-only stays Loopback
		{8080, "nginx", 300, ScopeLAN, "192.168.1.5"},       // a specific LAN IP -> LAN
		{3000, "node", 400, ScopeWildcard, "0.0.0.0"},       // 127.0.0.1 + 0.0.0.0 aggregates UP to Wildcard; host follows the wider bind
	} {
		p, ok := byPort[tc.port]
		if !ok {
			t.Errorf("expected port %d in %+v", tc.port, ports)
			continue
		}
		if p.Process != tc.proc {
			t.Errorf("port %d process = %q, want %q", tc.port, p.Process, tc.proc)
		}
		if p.Pid != tc.pid {
			t.Errorf("port %d pid = %d, want %d", tc.port, p.Pid, tc.pid)
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
	// Smoke test of the REAL `ss` invocation + parsing. It MUST be
	// environment-independent: the package's check() (go test ./...) runs in
	// clean build sandboxes and on machines that don't run sshd, so it must not
	// assume any particular service is listening. A previous hard "sshd on :22"
	// assertion here broke a real install on a machine without SSH (kata gxt5).
	// Parsing correctness is covered exhaustively by TestParseSS (and the
	// :22-less TestParseSSNoSSH); here we only assert the live call succeeds and
	// returns well-formed ports.
	ports, err := List()
	if err != nil {
		// `ss` (iproute2) can be absent in a minimal build sandbox; that is not
		// a tailport failure, so skip rather than fail the package's check().
		t.Skipf("List() unavailable in this environment (ss missing?): %v", err)
	}
	for _, p := range ports {
		if p.Number < 1 || p.Number > 65535 {
			t.Errorf("List() returned an out-of-range port: %+v", p)
		}
	}
}

// TestParseSSNoSSH guards the case that broke a real install (kata gxt5): a
// machine with NO sshd / nothing on :22. parseSS must handle it cleanly and
// never require a particular port -- here a box whose only listeners are
// systemd-resolved (:53) and cups (:631), mirroring the failing environment.
func TestParseSSNoSSH(t *testing.T) {
	const fixture = `LISTEN 0      4096       127.0.0.53%lo:53            0.0.0.0:*    users:(("systemd-resolve",pid=700,fd=13))
LISTEN 0      128            127.0.0.1:631           0.0.0.0:*    users:(("cupsd",pid=800,fd=7))
`
	ports, err := parseSS([]byte(fixture))
	if err != nil {
		t.Fatalf("parseSS error: %v", err)
	}
	if len(ports) != 2 {
		t.Fatalf("parsed %d ports, want 2 (:53, :631): %+v", len(ports), ports)
	}
	byPort := map[int]Port{}
	for _, p := range ports {
		if p.Number == 22 {
			t.Errorf(":22 must never appear when nothing binds it: %+v", ports)
		}
		byPort[p.Number] = p
	}
	if p := byPort[53]; p.Process != "systemd-resolve" || p.Pid != 700 {
		t.Errorf(":53 = %+v, want process systemd-resolve pid 700", p)
	}
	// The row is "127.0.0.53%lo:53": the "%lo" zone must be stripped before
	// classification, so :53 is Loopback with a BARE BindHost, never a
	// ScopeLAN fallback from an unparseable "127.0.0.53%lo" (kata np07).
	if p := byPort[53]; p.BindScope != ScopeLoopback || p.BindHost != "127.0.0.53" {
		t.Errorf(":53 = %+v, want BindScope=loopback BindHost=127.0.0.53", p)
	}
	if p := byPort[631]; p.Process != "cupsd" || p.Pid != 800 {
		t.Errorf(":631 = %+v, want process cupsd pid 800", p)
	}
}

// TestParseSSDeviceZone pins kata np07 end to end in the parser: `ss` prints a
// socket bound to a device (SO_BINDTODEVICE, or an IPv6 link-local scope) as
// "addr%iface:port". The zone must be split off BEFORE classification, so a
// device-bound tailnet socket classifies ScopeTailnet with a bare BindHost
// (no bogus LAN route), while an IPv6 link-local socket keeps its zone in
// BindHost because the address is undialable without it. Tailnet rows are
// split by OWNERSHIP: zoneless, ownerless, or tailscaled-owned rows are
// tailscaled's serve proxy and stay filtered (79xb); a zoned row owned by an
// app is that app's own tailnet bind.
func TestParseSSDeviceZone(t *testing.T) {
	const fixture = `LISTEN 0      511 100.110.98.29%tailscale0:5173      0.0.0.0:*    users:(("node",pid=449406,fd=20))
LISTEN 0      128              [fe80::1%eth0]:8080           [::]:*    users:(("app",pid=1234,fd=3))
LISTEN 0      128              [fe80::1]%eth0:9090           [::]:*    users:(("app",pid=1234,fd=6))
LISTEN 0      128          127.0.0.1:5174          0.0.0.0:*    users:(("mixed",pid=2000,fd=4))
LISTEN 0      4096 100.110.98.29%tailscale0:5174      0.0.0.0:*    users:(("mixed",pid=2000,fd=5))
LISTEN 0      4096       100.101.102.103:8808      0.0.0.0:*    users:(("tailscaled",pid=50,fd=9))
LISTEN 0      511          127.0.0.1:5180          0.0.0.0:*    users:(("chorse-fe",pid=449406,fd=20))
LISTEN 0      4096 100.110.98.29%tailscale0:5180      0.0.0.0:*
LISTEN 0      4096 100.110.98.29%tailscale0:5181      0.0.0.0:*    users:(("tailscaled",pid=50,fd=11))
`
	ports, err := parseSS([]byte(fixture))
	if err != nil {
		t.Fatalf("parseSS error: %v", err)
	}
	byPort := map[int]Port{}
	for _, p := range ports {
		byPort[p.Number] = p
	}

	// An app that pinned itself to tailscale0 (zoned, owned by "node") is
	// NOT filtered as tailscaled -- it classifies Tailnet with the bare
	// address.
	if p, ok := byPort[5173]; !ok {
		t.Errorf("device-bound tailnet :5173 should be kept; got %+v", ports)
	} else if p.BindScope != ScopeTailnet || p.BindHost != "100.110.98.29" || p.No127 != true {
		t.Errorf(":5173 = %+v, want BindScope=tailnet BindHost=100.110.98.29 No127=true", p)
	}

	// IPv6 link-local keeps its zone (needed to dial), but classifies LAN on
	// the bare address.
	if p, ok := byPort[8080]; !ok {
		t.Errorf("link-local :8080 should be kept; got %+v", ports)
	} else if p.BindScope != ScopeLAN || p.BindHost != "fe80::1%eth0" {
		t.Errorf(":8080 = %+v, want BindScope=lan BindHost=fe80::1%%eth0", p)
	}

	// The REAL ss spelling puts the zone outside the brackets
	// ("[fe80::1]%eth0:9090"); it must parse to the same bind form.
	if p, ok := byPort[9090]; !ok {
		t.Errorf("outside-bracket-zone :9090 should be kept; got %+v", ports)
	} else if p.BindScope != ScopeLAN || p.BindHost != "fe80::1%eth0" {
		t.Errorf(":9090 = %+v, want BindScope=lan BindHost=fe80::1%%eth0", p)
	}

	// Loopback + device-bound tailnet on one port: scope aggregates to the
	// wider Tailnet, but Loopback survives, No127 is cleared by the 127.0.0.1
	// row, and BindHost is the bare tailnet address (no zone).
	if p, ok := byPort[5174]; !ok {
		t.Errorf(":5174 should be present; got %+v", ports)
	} else if p.BindScope != ScopeTailnet || !p.Loopback || p.No127 || p.BindHost != "100.110.98.29" {
		t.Errorf(":5174 = %+v, want BindScope=tailnet Loopback=true No127=false BindHost=100.110.98.29", p)
	}

	// A zoneless tailnet row is still tailscaled's own: filtered (79xb).
	if _, ok := byPort[8808]; ok {
		t.Errorf("zoneless tailnet-only :8808 should still be filtered; got %+v", ports)
	}

	// The likelier reading of the owner's moth report: a newer tailscaled
	// binds its SERVE proxy to the tailscale0 device, and an unprivileged ss
	// shows no owning process for it (tailscaled runs as root). That zoned
	// row must be skipped like tailscaled's zoneless one, leaving just the
	// app's loopback bind -- so a served port still reads as loopback-bound
	// (t keeps offering to unserve it), never as the app's own tailnet bind.
	if p, ok := byPort[5180]; !ok {
		t.Errorf(":5180 (app on loopback) should be present; got %+v", ports)
	} else if p.BindScope != ScopeLoopback || p.BindHost != "127.0.0.1" || p.Process != "chorse-fe" {
		t.Errorf(":5180 = %+v, want BindScope=loopback BindHost=127.0.0.1 Process=chorse-fe (the ownerless zoned tailnet row skipped)", p)
	}
	// ...and the same zoned row attributed to tailscaled (ss run as root).
	if _, ok := byPort[5181]; ok {
		t.Errorf("zoned tailnet row owned by tailscaled (:5181) should be filtered; got %+v", ports)
	}
}

// TestParseSSZonedWildcard pins roborev 6053 through the real parser: a
// wildcard restricted to a LAN device ("0.0.0.0%eth0") aggregates to a LAN
// bind on that interface's address -- never ScopeWildcard, never Loopback, and
// No127 stays true -- so the ui can't advertise localhost/tailnet routes or
// mark a serve route healthy for a socket that can't receive them.
func TestParseSSZonedWildcard(t *testing.T) {
	orig := ifaceAddr
	ifaceAddr = func(string) string { return "192.168.1.50" }
	t.Cleanup(func() { ifaceAddr = orig })

	const fixture = `LISTEN 0      511       0.0.0.0%eth0:8080        0.0.0.0:*    users:(("app",pid=77,fd=3))
`
	ports, err := parseSS([]byte(fixture))
	if err != nil || len(ports) != 1 {
		t.Fatalf("parseSS = %+v, %v; want one port", ports, err)
	}
	p := ports[0]
	if p.BindScope != ScopeLAN || p.BindHost != "192.168.1.50" || p.Loopback || !p.No127 {
		t.Errorf(":8080 = %+v, want BindScope=lan BindHost=192.168.1.50 Loopback=false No127=true", p)
	}
}

// TestParseSSPreservesLoopbackAcrossWiderBind guards t12m: a port bound on
// BOTH loopback and a specific LAN address must aggregate BindScope to the
// wider LAN scope (widerScope's existing behavior, unchanged) but ALSO keep
// Loopback=true, so downstream route derivation doesn't lose the localhost
// route just because a wider bind coexists. Ports bound on only one scope
// pin the flag's other states: LAN-only stays false, loopback-only is true.
func TestParseSSPreservesLoopbackAcrossWiderBind(t *testing.T) {
	const fixture = `LISTEN 0      128          127.0.0.1:9090          0.0.0.0:*    users:(("mixed",pid=900,fd=3))
LISTEN 0      128        192.168.1.20:9090          0.0.0.0:*    users:(("mixed",pid=900,fd=4))
LISTEN 0      128        192.168.1.21:9091          0.0.0.0:*    users:(("lanonly",pid=901,fd=5))
LISTEN 0      128           127.0.0.1:9092          0.0.0.0:*    users:(("looponly",pid=902,fd=6))
`
	ports, err := parseSS([]byte(fixture))
	if err != nil {
		t.Fatalf("parseSS error: %v", err)
	}
	byPort := map[int]Port{}
	for _, p := range ports {
		byPort[p.Number] = p
	}

	if p := byPort[9090]; p.BindScope != ScopeLAN || p.BindHost != "192.168.1.20" || !p.Loopback {
		t.Errorf(":9090 (loopback+LAN) = %+v, want BindScope=LAN BindHost=192.168.1.20 Loopback=true", p)
	}
	if p := byPort[9091]; p.BindScope != ScopeLAN || p.Loopback {
		t.Errorf(":9091 (LAN-only) = %+v, want BindScope=LAN Loopback=false", p)
	}
	if p := byPort[9092]; p.BindScope != ScopeLoopback || !p.Loopback {
		t.Errorf(":9092 (loopback-only) = %+v, want BindScope=Loopback Loopback=true", p)
	}
}

// TestParseSSNo127 pins 2z0v(b): No127 must be true only when NOT ONE bind row
// for a port accepts a dial to 127.0.0.1:PORT -- the address `tailscale serve`
// always proxies to. Every port below classifies as ScopeLoopback or
// ScopeWildcard under classifyBindScope's truth table (so BindScope/Loopback
// alone can't be used to infer this), pinning that No127 is a genuinely
// separate bit:
//   - [::1] only (IPv6 loopback, a distinct address from 127.0.0.1) -> No127
//   - [::] only (V6ONLY unspecified -- ss's "[::]", not "*") -> No127
//   - 127.0.0.2 only (a non-.1 loopback address) -> No127
//   - 127.0.0.1 + [::1] (the .1 row accepts) -> NOT No127
//   - *:PORT (ss's dual-stack wildcard spelling, includes 127.0.0.1) -> NOT No127
//   - 0.0.0.0 + [::] (the 0.0.0.0 row accepts even though [::] alone wouldn't) -> NOT No127
//   - ::ffff:127.0.0.1 (the v4-mapped IPv6 form of 127.0.0.1) -> NOT No127
func TestParseSSNo127(t *testing.T) {
	const fixture = `LISTEN 0      128              [::1]:18791          [::]:*    users:(("python3",pid=1001,fd=3))
LISTEN 0      128               [::]:18792           [::]:*    users:(("python3",pid=1002,fd=3))
LISTEN 0      128         127.0.0.2:18793          0.0.0.0:*    users:(("python3",pid=1003,fd=3))
LISTEN 0      128         127.0.0.1:18794          0.0.0.0:*    users:(("python3",pid=1004,fd=3))
LISTEN 0      128              [::1]:18794           [::]:*    users:(("python3",pid=1004,fd=4))
LISTEN 0      128                 *:18795          0.0.0.0:*    users:(("python3",pid=1005,fd=3))
LISTEN 0      128           0.0.0.0:18796          0.0.0.0:*    users:(("python3",pid=1006,fd=3))
LISTEN 0      128              [::]:18796           [::]:*    users:(("python3",pid=1006,fd=4))
LISTEN 0      128 [::ffff:127.0.0.1]:18797          0.0.0.0:*    users:(("python3",pid=1007,fd=3))
`
	ports, err := parseSS([]byte(fixture))
	if err != nil {
		t.Fatalf("parseSS error: %v", err)
	}
	byPort := map[int]Port{}
	for _, p := range ports {
		byPort[p.Number] = p
	}

	for _, tc := range []struct {
		port      int
		wantNo127 bool
	}{
		{18791, true},  // [::1] only
		{18792, true},  // [::] only (V6ONLY)
		{18793, true},  // 127.0.0.2 only
		{18794, false}, // 127.0.0.1 + [::1]
		{18795, false}, // *:PORT
		{18796, false}, // 0.0.0.0 + [::]
		{18797, false}, // ::ffff:127.0.0.1
	} {
		p, ok := byPort[tc.port]
		if !ok {
			t.Errorf("expected port %d in %+v", tc.port, ports)
			continue
		}
		if p.No127 != tc.wantNo127 {
			t.Errorf("port %d No127 = %v, want %v (%+v)", tc.port, p.No127, tc.wantNo127, p)
		}
	}
}

// TestParseSSEmpty: no listeners at all (a fresh/clean sandbox) -> no ports, no
// error. The scanner (and everything downstream) must tolerate an empty world.
func TestParseSSEmpty(t *testing.T) {
	ports, err := parseSS([]byte(""))
	if err != nil {
		t.Fatalf("parseSS error: %v", err)
	}
	if len(ports) != 0 {
		t.Errorf("empty ss output should yield no ports; got %+v", ports)
	}
}

// TestParseSSFirstNonEmptyProcessAcrossDifferingRows guards 9094 item 3: every
// existing multi-row fixture above repeats the SAME proc+pid on every row for
// a given port, which never actually exercises "the first non-empty process
// wins, with the pid paired from that SAME row" across rows that DIFFER --
// the aggregation could coincidentally look right while secretly reusing a
// stale value. Row order for :4000: the FIRST-seen row is foreign/unattributed
// (ss omits the whole users:(...) field when it can't attribute a socket, e.g.
// one owned by another user) and a LATER row for the SAME port carries the
// real process+pid, which the aggregate must adopt -- paired together, not
// mixed with anything from the first row. :5000 is a lone foreign/unattributed
// socket, covering the other half of 9094 item 3: Process/Pid must stay their
// zero values, never a stray pid picked up from an unrelated row.
func TestParseSSFirstNonEmptyProcessAcrossDifferingRows(t *testing.T) {
	const fixture = `LISTEN 0      128          127.0.0.1:4000          0.0.0.0:*
LISTEN 0      128            0.0.0.0:4000          0.0.0.0:*    users:(("app",pid=555,fd=11))
LISTEN 0      128            0.0.0.0:5000          0.0.0.0:*
`
	ports, err := parseSS([]byte(fixture))
	if err != nil {
		t.Fatalf("parseSS error: %v", err)
	}
	byPort := map[int]Port{}
	for _, p := range ports {
		byPort[p.Number] = p
	}

	if p := byPort[4000]; p.Process != "app" || p.Pid != 555 {
		t.Errorf(":4000 = %+v, want process=app pid=555 (paired from the later, non-empty row)", p)
	}
	// Assert the socket is actually PRESENT before checking it's zero-valued
	// (2236): a missing map entry also returns a zero Port, so a regression that
	// dropped every process-less row would satisfy the field checks alone. The
	// unattributed socket must be listed, just with no process/pid attributed.
	if p, ok := byPort[5000]; !ok {
		t.Error(":5000 (foreign/unattributed) must still be listed, not dropped for lacking a process")
	} else if p.Process != "" || p.Pid != 0 {
		t.Errorf(":5000 (foreign/unattributed) = %+v, want Process=\"\" Pid=0", p)
	}
}
