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
		pid   int
		scope BindScope
		host  string
	}{
		{3000, "launchd", 1, ScopeWildcard, "*"},            // *:3000 -> Wildcard
		{5173, "node", 501, ScopeLoopback, "127.0.0.1"},     // 127.0.0.1 + [::1] collapse; loopback-only stays Loopback; tie keeps first-seen host
		{17600, "Dropbox", 720, ScopeLoopback, "127.0.0.1"}, // loopback
		{8080, "nginx", 800, ScopeLAN, "192.168.1.5"},       // a specific LAN IP -> LAN
		{4000, "vite", 900, ScopeWildcard, "*"},             // 127.0.0.1 + * aggregates UP to Wildcard; host follows the wider bind
	} {
		p, ok := got[tc.port]
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

// TestParseLsofPreservesLoopbackAcrossWiderBind guards t12m: a port bound on
// BOTH loopback and a specific LAN address must aggregate BindScope to the
// wider LAN scope (widerScope's existing behavior, unchanged) but ALSO keep
// Loopback=true, so downstream route derivation doesn't lose the localhost
// route just because a wider bind coexists. Ports bound on only one scope pin
// the flag's other states: LAN-only stays false, loopback-only is true.
func TestParseLsofPreservesLoopbackAcrossWiderBind(t *testing.T) {
	const fixture = `COMMAND     PID USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME
mixed       900   mg   10u  IPv4 0xaaaaaaaaaaaaaaaa      0t0  TCP 127.0.0.1:9090 (LISTEN)
mixed       900   mg   11u  IPv4 0xbbbbbbbbbbbbbbbb      0t0  TCP 192.168.1.20:9090 (LISTEN)
lanonly     901   mg   12u  IPv4 0xcccccccccccccccc      0t0  TCP 192.168.1.21:9091 (LISTEN)
looponly    902   mg   13u  IPv4 0xdddddddddddddddd      0t0  TCP 127.0.0.1:9092 (LISTEN)
`
	ports, err := parseLsof([]byte(fixture))
	if err != nil {
		t.Fatalf("parseLsof error: %v", err)
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

// TestParseLsofNo127 pins 2z0v(b) on darwin, mirroring Linux's
// TestParseSSNo127: No127 must be true only when NOT ONE bind row for a port
// accepts a dial to 127.0.0.1:PORT -- the address `tailscale serve` always
// proxies to.
//
// UNVERIFIED (design doc "Remaining weak assumptions" #2): this fixture is
// hand-written from documented lsof(8) output conventions, not captured on a
// real Mac. In particular, whether lsof ever prints a distinct spelling for a
// V6ONLY IPv6 wildcard bind (the way Linux ss prints "[::]" vs "*") is
// unconfirmed here -- this test only exercises "*" as the dual-stack/wildcard
// spelling, which accepts127 treats as accepting. It runs on darwin, so it is
// exercised only by the opt-in `[ci darwin]` job, never on this (Linux) box.
func TestParseLsofNo127(t *testing.T) {
	const fixture = `COMMAND     PID USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME
python3    1001   mg    3u  IPv6 0x1111111111111111      0t0  TCP [::1]:18791 (LISTEN)
python3    1002   mg    3u  IPv4 0x2222222222222222      0t0  TCP 127.0.0.2:18792 (LISTEN)
python3    1003   mg    3u  IPv4 0x3333333333333333      0t0  TCP 127.0.0.1:18793 (LISTEN)
python3    1003   mg    4u  IPv6 0x4444444444444444      0t0  TCP [::1]:18793 (LISTEN)
python3    1004   mg    3u  IPv4 0x5555555555555555      0t0  TCP *:18794 (LISTEN)
python3    1005   mg    3u  IPv6 0x6666666666666666      0t0  TCP *:18794 (LISTEN)
`
	ports, err := parseLsof([]byte(fixture))
	if err != nil {
		t.Fatalf("parseLsof error: %v", err)
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
		{18792, true},  // 127.0.0.2 only
		{18793, false}, // 127.0.0.1 + [::1]
		{18794, false}, // *:PORT (IPv4 row) + *:PORT (IPv6 row)
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

// TestParseLsofDeviceZone pins the "%iface" zone handling in the lsof parser
// (kata np07), mirroring Linux's TestParseSSDeviceZone. Whether macOS lsof
// ever emits a zone suffix is UNVERIFIED here (no Mac available while writing
// this), but the parser must not treat it as an opaque host: an unsplit zone
// makes ParseIP fail and classifyBindScope fall back to ScopeLAN, resurrecting
// the bogus LAN row np07 removed. This defensive fixture pins that.
//
// UNVERIFIED on macOS: exercised only by the opt-in `[ci darwin]` job, if its
// lsof behaves as these hand-written rows say.
func TestParseLsofDeviceZone(t *testing.T) {
	const fixture = `COMMAND     PID USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME
node        501   mg   23u  IPv4 0x2222222222222222      0t0  TCP 100.110.98.29%tailscale0:5173 (LISTEN)
app        600   mg   24u  IPv6 0x3333333333333333      0t0  TCP [fe80::1%en0]:8080 (LISTEN)
tailscaled   50 root   11u  IPv4 0x4444444444444444      0t0  TCP 100.110.98.29%tailscale0:5181 (LISTEN)
`
	ports, err := parseLsof([]byte(fixture))
	if err != nil {
		t.Fatalf("parseLsof error: %v", err)
	}
	got := map[int]Port{}
	for _, p := range ports {
		got[p.Number] = p
	}

	if p, ok := got[5173]; !ok {
		t.Errorf("device-bound tailnet :5173 should be kept; got %+v", ports)
	} else if p.BindScope != ScopeTailnet || p.BindHost != "100.110.98.29" {
		t.Errorf(":5173 = %+v, want BindScope=tailnet BindHost=100.110.98.29", p)
	}
	// tailscaled's own zoned serve-proxy listener is skipped, exactly like
	// its zoneless one (nvv6) -- same ownership rule as the Linux parser.
	if _, ok := got[5181]; ok {
		t.Errorf("tailscaled-owned zoned tailnet :5181 should be filtered; got %+v", ports)
	}
	if p, ok := got[8080]; !ok {
		t.Errorf("link-local :8080 should be kept; got %+v", ports)
	} else if p.BindScope != ScopeLAN || p.BindHost != "fe80::1%en0" {
		t.Errorf(":8080 = %+v, want BindScope=lan BindHost=fe80::1%%en0", p)
	}
}

// Kata p2r5: Pid127 mirrors the Linux parser on lsof output.
func TestParseLsofPid127(t *testing.T) {
	hdr := "COMMAND PID USER FD TYPE DEVICE SIZE/OFF NODE NAME\n"
	row := func(name string, pid int, addr string) string {
		return name + " " + itoa(pid) + " u 3u IPv4 0x1 0t0 TCP " + addr + " (LISTEN)\n"
	}
	ports, err := parseLsof([]byte(hdr + row("oldapp", 111, "[::1]:18921") + row("other", 222, "127.0.0.1:18921")))
	if err != nil || len(ports) != 1 || ports[0].Pid != 111 || ports[0].Pid127 != 222 {
		t.Fatalf("two programs: %+v %v", ports, err)
	}
	ports, _ = parseLsof([]byte(hdr + row("a", 111, "127.0.0.1:18921") + row("b", 222, "*:18921")))
	if ports[0].Pid127 != 0 {
		t.Errorf("two pids both accepting 127.0.0.1 must be ambiguous, got %d", ports[0].Pid127)
	}
	ports, _ = parseLsof([]byte(hdr + row("a", 111, "[::1]:18921")))
	if ports[0].Pid127 != 0 {
		t.Errorf("no row accepts 127.0.0.1, got %d", ports[0].Pid127)
	}
}

// lsof lists a socket shared by two processes as two rows; both accept
// 127.0.0.1, so the distinct pids make it ambiguous.
func TestParseLsofSharedSocketIsAmbiguous(t *testing.T) {
	hdr := "COMMAND PID USER FD TYPE DEVICE SIZE/OFF NODE NAME\n"
	row := func(name string, pid int) string {
		return name + " " + itoa(pid) + " u 3u IPv4 0x1 0t0 TCP 127.0.0.1:18921 (LISTEN)\n"
	}
	ports, err := parseLsof([]byte(hdr + row("parent", 111) + row("child", 222)))
	if err != nil || len(ports) != 1 || ports[0].Pid127 != 0 {
		t.Fatalf("%+v %v", ports, err)
	}
	ports, _ = parseLsof([]byte(hdr + row("app", 111) + row("app", 111)))
	if ports[0].Pid127 != 111 {
		t.Fatalf("same pid twice stays unambiguous, got %d", ports[0].Pid127)
	}
}

// TestParseLsofTailnetOwnership pins kata nvv6 on the lsof parser, mirroring
// Linux's TestParseSSTailnetOwnership: a tailnet-IP row (zoned or not, IPv4 or
// fd7a:...) owned by Tailscale's own processes -- tailscaled (lsof's default
// 9-char truncation prints it "tailscale"), the app's network extension
// ("IPNExtens...") -- is the serve proxy and is dropped; any other visible
// owner is the app's own tailnet bind and is kept. Fixture-only: the exact
// COMMAND strings the Tailscale app's extension prints on a real Mac are
// UNVERIFIED here, hence the prefix match (isTailscaleLsofCommand).
func TestParseLsofTailnetOwnership(t *testing.T) {
	const fixture = `COMMAND     PID USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME
python3     501   mg    3u  IPv4 0x1111111111111111      0t0  TCP 100.81.94.92:18951 (LISTEN)
tailscale   310 root   12u  IPv4 0x2222222222222222      0t0  TCP 100.81.94.92:18952 (LISTEN)
IPNExtens   320 root   13u  IPv4 0x3333333333333333      0t0  TCP 100.81.94.92:18953 (LISTEN)
Tailscale   330   mg   14u  IPv4 0x4444444444444444      0t0  TCP 100.81.94.92:18954 (LISTEN)
node        502   mg    5u  IPv6 0x5555555555555555      0t0  TCP [fd7a:115c:a1e0::1]:18955 (LISTEN)
IPNExtens   320 root   15u  IPv6 0x6666666666666666      0t0  TCP [fd7a:115c:a1e0::1]:18956 (LISTEN)
app         503   mg    6u  IPv4 0x7777777777777777      0t0  TCP 127.0.0.1:18957 (LISTEN)
IPNExtens   320 root   16u  IPv4 0x8888888888888888      0t0  TCP 100.81.94.92:18957 (LISTEN)
`
	ports, err := parseLsof([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]Port{}
	for _, p := range ports {
		if _, dup := got[p.Number]; dup {
			t.Errorf("port %d reported twice: %+v", p.Number, ports)
		}
		got[p.Number] = p
	}
	for _, n := range []int{18951, 18955} {
		p, ok := got[n]
		if !ok || p.BindScope != ScopeTailnet || p.UnattribTailnet || !p.No127 {
			t.Errorf(":%d = %+v (present=%v), want an app tailnet bind", n, p, ok)
		}
	}
	if p := got[18951]; p.Process != "python3" || p.BindHost != "100.81.94.92" {
		t.Errorf(":18951 = %+v", p)
	}
	for _, n := range []int{18952, 18953, 18954, 18956} {
		if p, ok := got[n]; ok {
			t.Errorf(":%d is Tailscale's own serve proxy and must be dropped: %+v", n, p)
		}
	}
	// A loopback app alongside the extension's proxy row: only the app's row.
	if p := got[18957]; p.BindScope != ScopeLoopback || p.Process != "app" || p.UnattribTailnet {
		t.Errorf(":18957 = %+v, want the app's loopback bind", p)
	}
}

// Kata nvv6 (roborev 7329 #2): a tailnet address restricted to a non-tailscale
// device keeps its device classification (fixture-only on macOS).
func TestParseLsofDeviceRestrictedTailnetRows(t *testing.T) {
	const fixture = `COMMAND     PID USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME
python3     501   mg    3u  IPv4 0x1111111111111111      0t0  TCP 100.81.94.92%lo0:18951 (LISTEN)
python3     502   mg    3u  IPv4 0x2222222222222222      0t0  TCP 100.81.94.92%en0:18952 (LISTEN)
`
	ports, err := parseLsof([]byte(fixture))
	if err != nil || len(ports) != 2 {
		t.Fatalf("%+v %v", ports, err)
	}
	if ports[0].BindScope != ScopeLoopback || ports[1].BindScope != ScopeLAN {
		t.Errorf("got %+v, want loopback then LAN", ports)
	}
}
