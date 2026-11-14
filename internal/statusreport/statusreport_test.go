package statusreport

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/gruen/tailport/internal/caddyedge"
	"github.com/gruen/tailport/internal/config"
	"github.com/gruen/tailport/internal/portscan"
)

// ansiRe strips ANSI SGR escape sequences for tests that need to reason
// about a rendered line's DISPLAY width/offsets rather than its raw byte
// length -- an escape sequence takes zero screen columns on a real
// terminal, so comparing raw strings.Index across a styled and unstyled row
// would flag a false misalignment (the columns line up on screen; only the
// invisible byte count differs). See TestWriteTableAlignment.
var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

// TestBuild covers the core row-assembly logic against fake portscan/tsserve
// data -- no live tailscaled needed, per this issue's verification bar.
func TestBuild(t *testing.T) {
	ports := []portscan.Port{
		{Number: 3000, Process: "node"},
		{Number: 22, Process: "sshd"},
		{Number: 9000, Process: "unused-not-exposed"}, // listening but never served/funnelled
	}
	active := []int{3000, 22, 5000} // 5000 is a dangling forward: active, not listening
	funnel := map[int]int{8080: 8443}

	got := Build(ports, active, funnel, nil, nil, "host-a", "host-a.tailnet.ts.net")

	want := []Row{
		{Port: 22, Process: "sshd", Mode: ModeServe, URL: "http://host-a:22"},
		{Port: 3000, Process: "node", Mode: ModeServe, URL: "http://host-a:3000"},
		{Port: 5000, Process: "", Mode: ModeServe, URL: "http://host-a:5000"},
		{Port: 8080, Process: "", Mode: ModeFunnel, URL: "https://host-a.tailnet.ts.net:8443"},
	}

	if len(got) != len(want) {
		t.Fatalf("Build() returned %d rows, want %d; got %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Build()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	// 9000 is listening locally but neither served nor funnelled -- a status
	// report is about EXPOSURE, not every open local port (that's the TUI's
	// full port list), so it must not appear.
	for _, r := range got {
		if r.Port == 9000 {
			t.Errorf("Build() included port 9000, which is listening but not exposed; got %+v", got)
		}
	}
}

// TestBuildFunnelOutranksServe covers the same-port precedence rule
// (mirroring internal/ui's portItem.markerGlyph): a port that is BOTH
// tailnet-served and funnelled reports as funnel, since that's what
// actually governs reachability (public internet).
func TestBuildFunnelOutranksServe(t *testing.T) {
	got := Build(nil, []int{3000}, map[int]int{3000: 443}, nil, nil, "host-a", "host-a.tailnet.ts.net")
	if len(got) != 1 {
		t.Fatalf("Build() returned %d rows, want 1; got %+v", len(got), got)
	}
	if got[0].Mode != ModeFunnel {
		t.Errorf("Build()[0].Mode = %q, want %q (funnel outranks serve on the same port)", got[0].Mode, ModeFunnel)
	}
	if got[0].URL != "https://host-a.tailnet.ts.net" {
		t.Errorf("Build()[0].URL = %q, want the public funnel URL (443 implicit)", got[0].URL)
	}
}

// TestBuildEmpty covers the "nothing exposed" case: an empty, non-nil slice
// (WriteJSON depends on this to emit [] rather than null; this pins Build's
// half of that contract).
func TestBuildEmpty(t *testing.T) {
	got := Build(nil, nil, nil, nil, nil, "host-a", "")
	if got == nil {
		t.Fatal("Build(nil, nil, nil, nil, nil, ...) = nil, want a non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("Build(nil, nil, nil, nil, nil, ...) = %+v, want empty", got)
	}
}

// TestBuildPublishOnly covers the plain publish case: a port that carries
// ONLY a publish path (no funnel, no tunnel) reports a single ModePublish
// row, with the public https://<hostname> URL (no port) and the route's Auth
// flag carried through.
func TestBuildPublishOnly(t *testing.T) {
	published := map[int]Published{3000: {Hostname: "myapp.example.com", Auth: true}}
	got := Build(nil, []int{3000}, nil, published, nil, "host-a", "host-a.tailnet.ts.net")

	want := []Row{
		{Port: 3000, Process: "", Mode: ModePublish, URL: "https://myapp.example.com", Auth: true},
	}
	if len(got) != 1 {
		t.Fatalf("Build() returned %d rows, want 1; got %+v", len(got), got)
	}
	if got[0] != want[0] {
		t.Errorf("Build()[0] = %+v, want %+v", got[0], want[0])
	}
}

// TestBuildPublishOutranksServe covers a port that is BOTH tailnet-served
// (active) and published (the normal by-design shape: a publish auto-enables
// serve as its backing plumbing): only a single ModePublish row is emitted,
// mirroring how funnel outranks serve for display (TestBuildFunnelOutranksServe).
func TestBuildPublishOutranksServe(t *testing.T) {
	published := map[int]Published{3000: {Hostname: "myapp.example.com"}}
	got := Build(nil, []int{3000}, nil, published, nil, "host-a", "host-a.tailnet.ts.net")

	if len(got) != 1 {
		t.Fatalf("Build() returned %d rows, want 1; got %+v", len(got), got)
	}
	if got[0].Mode != ModePublish {
		t.Errorf("Build()[0].Mode = %q, want %q (publish outranks serve on the same port)", got[0].Mode, ModePublish)
	}
}

// TestBuildFunnelAndPublishBothObserved covers a port carrying BOTH a funnel
// and a Caddy-owned publish route -- a legitimate, expected state since kata
// th05 relaxed the old mutual exclusion. Build must emit BOTH rows for that
// port rather than ranking one over the other, and must add no new field to
// encode it.
func TestBuildFunnelAndPublishBothObserved(t *testing.T) {
	funnel := map[int]int{3000: 443}
	published := map[int]Published{3000: {Hostname: "myapp.example.com", Auth: true}}
	got := Build(nil, []int{3000}, funnel, published, nil, "host-a", "host-a.tailnet.ts.net")

	if len(got) != 2 {
		t.Fatalf("Build() returned %d rows for a port with both funnel and publish, want 2 (both rows); got %+v", len(got), got)
	}

	var sawFunnel, sawPublish bool
	for _, r := range got {
		if r.Port != 3000 {
			t.Errorf("Build() row %+v has Port = %d, want 3000", r, r.Port)
		}
		switch r.Mode {
		case ModeFunnel:
			sawFunnel = true
			if r.URL != "https://host-a.tailnet.ts.net" {
				t.Errorf("funnel row URL = %q, want the public funnel URL", r.URL)
			}
		case ModePublish:
			sawPublish = true
			if r.URL != "https://myapp.example.com" || !r.Auth {
				t.Errorf("publish row = %+v, want URL=https://myapp.example.com Auth=true", r)
			}
		default:
			t.Errorf("Build() row %+v has unexpected Mode %q, want ModeFunnel or ModePublish", r, r.Mode)
		}
	}
	if !sawFunnel || !sawPublish {
		t.Errorf("Build() = %+v, want one ModeFunnel row AND one ModePublish row", got)
	}
}

// TestBuildPublishedButNotServed covers the dangling-forward drift case
// (published's doc comment / this issue's spec): a port present ONLY in
// published (no active serve, no funnel) still appears -- a public 502 is
// the worst dangling-forward state, so that drift must stay visible.
func TestBuildPublishedButNotServed(t *testing.T) {
	published := map[int]Published{9999: {Hostname: "gone.example.com"}}
	got := Build(nil, nil, nil, published, nil, "host-a", "host-a.tailnet.ts.net")

	if len(got) != 1 {
		t.Fatalf("Build() returned %d rows, want 1; got %+v", len(got), got)
	}
	if got[0].Port != 9999 || got[0].Mode != ModePublish {
		t.Errorf("Build()[0] = %+v, want Port=9999 Mode=%q (published-but-not-served must stay visible)", got[0], ModePublish)
	}
	if got[0].Process != "" {
		t.Errorf("Build()[0].Process = %q, want \"\" (nothing is listening -- this IS the dangling forward)", got[0].Process)
	}
}

// TestWriteJSON covers the stable Document schema: valid JSON, exact field
// names, and an always-present array (never null) for "ports".
func TestWriteJSON(t *testing.T) {
	rows := []Row{
		{Port: 3000, Process: "node", Mode: ModeServe, URL: "http://host-a:3000"},
		{Port: 8080, Process: "", Mode: ModeFunnel, URL: "https://host-a.tailnet.ts.net:8443"},
	}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, rows); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}

	// Parseable, and field names match the documented schema exactly (a
	// generic map catches a field-name typo/rename that a struct-based
	// Unmarshal into Document would silently absorb).
	var generic struct {
		Ports []map[string]any `json:"ports"`
	}
	if err := json.Unmarshal(buf.Bytes(), &generic); err != nil {
		t.Fatalf("WriteJSON() output did not parse as JSON: %v; got:\n%s", err, buf.String())
	}
	if len(generic.Ports) != 2 {
		t.Fatalf("got %d ports, want 2", len(generic.Ports))
	}
	for _, key := range []string{"port", "process", "mode", "url"} {
		if _, ok := generic.Ports[0][key]; !ok {
			t.Errorf("ports[0] missing documented field %q; got %+v", key, generic.Ports[0])
		}
	}
	if mode := generic.Ports[1]["mode"]; mode != "funnel" {
		t.Errorf(`ports[1]["mode"] = %v, want "funnel"`, mode)
	}

	// Round-trips cleanly through the exported Document type too.
	var doc Document
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("re-parsing into Document: %v", err)
	}
	if len(doc.Ports) != 2 || doc.Ports[0] != rows[0] || doc.Ports[1] != rows[1] {
		t.Errorf("Document round-trip = %+v, want %+v", doc.Ports, rows)
	}
}

// TestWriteJSONAuthField covers Row.Auth's serialization contract (kata
// v1z5 step 6): omitempty means a false Auth (every ModeServe/ModeFunnel
// row, and a no-auth ModePublish row) is simply ABSENT from the JSON object
// -- not present as `"auth": false` -- while a true Auth (an authed
// ModePublish row) is present as `"auth": true`. This is what makes the
// field additive: an old consumer that never learned about "auth" sees
// nothing new on the rows it already understood.
func TestWriteJSONAuthField(t *testing.T) {
	rows := []Row{
		{Port: 3000, Process: "node", Mode: ModeServe, URL: "http://host-a:3000"},
		{Port: 4000, Process: "", Mode: ModePublish, URL: "https://open.example.com", Auth: false},
		{Port: 5000, Process: "", Mode: ModePublish, URL: "https://secret.example.com", Auth: true},
	}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, rows); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}

	var generic struct {
		Ports []map[string]any `json:"ports"`
	}
	if err := json.Unmarshal(buf.Bytes(), &generic); err != nil {
		t.Fatalf("WriteJSON() output did not parse as JSON: %v; got:\n%s", err, buf.String())
	}
	if len(generic.Ports) != 3 {
		t.Fatalf("got %d ports, want 3", len(generic.Ports))
	}
	if _, ok := generic.Ports[0]["auth"]; ok {
		t.Errorf(`ports[0] (serve, Auth=false) has "auth" key present; want it OMITTED entirely, got %+v`, generic.Ports[0])
	}
	if _, ok := generic.Ports[1]["auth"]; ok {
		t.Errorf(`ports[1] (publish, Auth=false) has "auth" key present; want it OMITTED entirely, got %+v`, generic.Ports[1])
	}
	if auth, ok := generic.Ports[2]["auth"]; !ok || auth != true {
		t.Errorf(`ports[2] (publish, Auth=true) "auth" = %v (present=%v), want true`, auth, ok)
	}
}

// TestWriteJSONEmptyIsArrayNotNull covers WriteJSON's explicit nil-handling:
// no exposed ports must still serialize "ports" as [], not null, so a
// consumer never needs a null check.
func TestWriteJSONEmptyIsArrayNotNull(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, nil); err != nil {
		t.Fatalf("WriteJSON(nil) error = %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `"ports": []`) {
		t.Errorf("WriteJSON(nil) = %q, want a \"ports\": [] array", got)
	}
	if strings.Contains(got, "null") {
		t.Errorf("WriteJSON(nil) = %q, want no null", got)
	}
}

// TestWriteTable covers the human-readable table: header, both rows'
// content, and the funnel row's visually-distinct mode text.
func TestWriteTable(t *testing.T) {
	// Force the Ascii color profile so this assertion is deterministic
	// regardless of the test environment's terminal detection (mirrors
	// cmd/tailport's TestApplyNoColorForcesAsciiProfile).
	lipgloss.SetColorProfile(termenv.Ascii)

	rows := []Row{
		{Port: 3000, Process: "node", Mode: ModeServe, URL: "http://host-a:3000"},
		{Port: 8080, Process: "", Mode: ModeFunnel, URL: "https://host-a.tailnet.ts.net:8443"},
	}
	var buf bytes.Buffer
	WriteTable(&buf, rows)
	got := buf.String()

	for _, want := range []string{"MODE", "PORT", "PROCESS", "URL", "3000", "node", "http://host-a:3000", "serve (tailnet)", "8080", "FUNNEL (public)", "https://host-a.tailnet.ts.net:8443"} {
		if !strings.Contains(got, want) {
			t.Errorf("WriteTable() missing %q; got:\n%s", want, got)
		}
	}
	// The dangling-forward/no-process row falls back to "?" rather than a
	// blank cell (an empty cell would misread as a parsing gap, not "unknown
	// process").
	if !strings.Contains(got, "?") {
		t.Errorf("WriteTable() missing the \"?\" process placeholder for an empty Process; got:\n%s", got)
	}
	if strings.Contains(got, "\x1b[") {
		t.Errorf("WriteTable() under the forced Ascii profile contains ANSI escapes; got:\n%q", got)
	}

	// "serve" must never appear as a case-insensitive-ambiguous substring of
	// "FUNNEL" or vice versa -- the two must be textually distinguishable at
	// a glance even without color (this issue's safety requirement).
	if strings.Contains(strings.ToLower(got), "funnel") && !strings.Contains(got, "FUNNEL") {
		t.Errorf("WriteTable() renders funnel without the upper-case FUNNEL marker; got:\n%s", got)
	}
}

// TestWriteTableAlignment covers the tabwriter-then-colorize ordering
// (WriteTable's doc comment): every data row's URL column must start at the
// same byte offset, proving ANSI styling never skews column alignment. This
// is checked with color enabled (funnelStyle actually emits escapes) --
// the scenario the doc comment calls out as risky if done in the wrong
// order.
func TestWriteTableAlignment(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)

	rows := []Row{
		{Port: 3000, Process: "node", Mode: ModeServe, URL: "http://host-a:3000"},
		{Port: 8080, Process: "python3", Mode: ModeFunnel, URL: "https://host-a.tailnet.ts.net:8443"},
	}
	var buf bytes.Buffer
	WriteTable(&buf, rows)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 { // header + 2 rows
		t.Fatalf("WriteTable() produced %d lines, want 3 (header + 2 rows); got:\n%s", len(lines), buf.String())
	}

	urlCol := func(line string) int { return strings.Index(stripANSI(line), "http") }
	header, serveLine, funnelLine := lines[0], lines[1], lines[2]
	hIdx, sIdx, fIdx := strings.Index(stripANSI(header), "URL"), urlCol(serveLine), urlCol(funnelLine)
	if hIdx == -1 || sIdx == -1 || fIdx == -1 {
		t.Fatalf("could not locate URL column in one of the lines: header=%q serve=%q funnel=%q", header, serveLine, funnelLine)
	}
	if hIdx != sIdx || sIdx != fIdx {
		t.Errorf("URL column misaligned: header at %d, serve row at %d, funnel row at %d; ANSI styling likely skewed tabwriter -- got:\n%s", hIdx, sIdx, fIdx, buf.String())
	}
}

// TestWriteTableEmpty covers the no-exposed-ports case: a plain sentence,
// not a header with zero data rows.
func TestWriteTableEmpty(t *testing.T) {
	var buf bytes.Buffer
	WriteTable(&buf, nil)
	got := buf.String()
	if !strings.Contains(got, "No ports are currently served, funnelled, published, or tunnelled") {
		t.Errorf("WriteTable(nil) = %q, want a no-ports-served message", got)
	}
	if strings.Contains(got, "MODE") {
		t.Errorf("WriteTable(nil) = %q, want no table header when there's nothing to show", got)
	}
}

// TestWriteTablePublish covers the PUBLISH mode label (kata v1z5 step 6):
// "PUBLISH (edge, public)" appears, gets the same funnel-style coloring
// (i.e. is a no-op transform under the forced Ascii profile, so no stray
// ANSI leaks into a no-color table), and stays textually distinct from both
// "serve" and "FUNNEL".
func TestWriteTablePublish(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)

	rows := []Row{
		{Port: 3000, Process: "node", Mode: ModeServe, URL: "http://host-a:3000"},
		{Port: 8080, Process: "", Mode: ModeFunnel, URL: "https://host-a.tailnet.ts.net:8443"},
		{Port: 9090, Process: "app", Mode: ModePublish, URL: "https://myapp.example.com", Auth: true},
	}
	var buf bytes.Buffer
	WriteTable(&buf, rows)
	got := buf.String()

	for _, want := range []string{"PUBLISH (edge, public)", "9090", "app", "https://myapp.example.com"} {
		if !strings.Contains(got, want) {
			t.Errorf("WriteTable() missing %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b[") {
		t.Errorf("WriteTable() under the forced Ascii profile contains ANSI escapes; got:\n%q", got)
	}

	// PUBLISH must never read as a substring/case-fold of FUNNEL or serve.
	if strings.Contains(strings.ToLower(got), "publish") && !strings.Contains(got, "PUBLISH") {
		t.Errorf("WriteTable() renders publish without the upper-case PUBLISH marker; got:\n%s", got)
	}
}

// TestWriteTablePublishAlignment mirrors TestWriteTableAlignment for the
// third mode: with color enabled, the PUBLISH row's URL column must still
// line up with the header and every other row.
func TestWriteTablePublishAlignment(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)

	rows := []Row{
		{Port: 3000, Process: "node", Mode: ModeServe, URL: "http://host-a:3000"},
		{Port: 8080, Process: "python3", Mode: ModeFunnel, URL: "https://host-a.tailnet.ts.net:8443"},
		{Port: 9090, Process: "app", Mode: ModePublish, URL: "https://myapp.example.com"},
	}
	var buf bytes.Buffer
	WriteTable(&buf, rows)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 4 { // header + 3 rows
		t.Fatalf("WriteTable() produced %d lines, want 4 (header + 3 rows); got:\n%s", len(lines), buf.String())
	}

	urlCol := func(line string) int { return strings.Index(stripANSI(line), "http") }
	hIdx := strings.Index(stripANSI(lines[0]), "URL")
	if hIdx == -1 {
		t.Fatalf("could not locate URL column in header: %q", lines[0])
	}
	for _, line := range lines[1:] {
		if idx := urlCol(line); idx != hIdx {
			t.Errorf("URL column misaligned: header at %d, row %q at %d; ANSI styling likely skewed tabwriter -- got:\n%s", hIdx, line, idx, buf.String())
		}
	}
}

// --- shortLabel / gatherPublished tests -------------------------------------

// TestShortLabel covers the pure derivation Gather uses to address this
// machine's own routes: the first `.`-component of a full FQDN, per v1z5's
// Architecture section (Self.DNSName, not Self.HostName).
func TestShortLabel(t *testing.T) {
	cases := []struct{ fqdn, want string }{
		{"host-a.tailnet.ts.net", "host-a"},
		{"host-a", "host-a"}, // no dot -- the whole string is the label
		{"", ""},
	}
	for _, c := range cases {
		if got := shortLabel(c.fqdn); got != c.want {
			t.Errorf("shortLabel(%q) = %q, want %q", c.fqdn, got, c.want)
		}
	}
}

// fakeCaddyAdmin starts a minimal httptest.Server answering Caddy admin's
// list-routes endpoint (GET /config/apps/http/servers/<serverName>/routes)
// with a fixed routes array -- the one call gatherPublished makes. It
// returns the host/port to plug into config.CaddyConfig; gatherPublished
// builds its own caddyedge.Client internally from cfg.Caddy, so there's no
// Client field to inject directly (mirroring how the real Gather works).
func fakeCaddyAdmin(t *testing.T, serverName string, routes []caddyedge.Route) (host string, port int) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/config/apps/http/servers/"+serverName+"/routes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Etag", `"1"`)
		if err := json.NewEncoder(w).Encode(routes); err != nil {
			t.Fatal(err)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return splitTestServerURL(t, srv.URL)
}

// splitTestServerURL splits an httptest.Server's URL into a bare host and
// numeric port, the shape config.CaddyConfig's Hostname/AdminPort fields
// want.
func splitTestServerURL(t *testing.T, rawURL string) (host string, port int) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parsing test server URL %q: %v", rawURL, err)
	}
	h, p, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("splitting test server host:port %q: %v", u.Host, err)
	}
	portNum, err := strconv.Atoi(p)
	if err != nil {
		t.Fatalf("parsing test server port %q: %v", p, err)
	}
	return h, portNum
}

// TestGatherPublishedUnconfiguredSkipsEdge covers the "blank domain = zero
// cost" contract (v1z5's config semantics, and this issue's spec): with
// cfg.Caddy.Domain == "", gatherPublished must return nil WITHOUT ever
// touching the edge -- proven here by a test server that fails the test if
// hit at all, not just by checking the returned value.
func TestGatherPublishedUnconfiguredSkipsEdge(t *testing.T) {
	hit := false
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { hit = true })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	h, p := splitTestServerURL(t, srv.URL)

	cfg := config.Config{Caddy: config.CaddyConfig{
		Domain:     "", // unconfigured
		Hostname:   h,
		AdminPort:  p,
		ServerName: "tailport",
	}}
	got := gatherPublished(cfg, "host-a.tailnet.ts.net")
	if got != nil {
		t.Errorf("gatherPublished() = %+v, want nil when cfg.Caddy.Domain is unconfigured", got)
	}
	if hit {
		t.Error("gatherPublished() reached the edge even though cfg.Caddy.Domain is unconfigured (must be zero cost)")
	}
}

// TestGatherPublishedEmptyFQDNSkipsEdge covers the companion degrade case: an
// unresolved fqdn (e.g. tailscaled unreachable) means the machine's own
// short label can't be computed, so there's nothing to filter routes on --
// gatherPublished must not guess, and must not touch the edge either.
func TestGatherPublishedEmptyFQDNSkipsEdge(t *testing.T) {
	hit := false
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { hit = true })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	h, p := splitTestServerURL(t, srv.URL)

	cfg := config.Config{Caddy: config.CaddyConfig{
		Domain: "example.com", Hostname: h, AdminPort: p, ServerName: "tailport",
	}}
	got := gatherPublished(cfg, "")
	if got != nil {
		t.Errorf("gatherPublished() = %+v, want nil when fqdn is empty", got)
	}
	if hit {
		t.Error("gatherPublished() reached the edge even though fqdn is empty (no label to filter on)")
	}
}

// TestGatherPublishedFiltersToThisMachinesLabel is the main happy-path
// wiring test: List returns routes for two different backends sharing one
// Caddy edge (the normal multi-computer-per-edge shape, per v1z5's
// Architecture section); gatherPublished must keep only the one whose
// backend Label matches THIS machine's short label (derived from fqdn), and
// map it by local port with Hostname/Auth carried through.
func TestGatherPublishedFiltersToThisMachinesLabel(t *testing.T) {
	mine := caddyedge.BuildRoute("myapp.example.com", "host-a", 3000, &caddyedge.BasicAuth{User: "u", Hash: "$2a$hash"})
	other := caddyedge.BuildRoute("other.example.com", "host-b", 4000, nil)
	h, p := fakeCaddyAdmin(t, "tailport", []caddyedge.Route{mine, other})

	cfg := config.Config{Caddy: config.CaddyConfig{
		Domain: "example.com", Hostname: h, AdminPort: p, ServerName: "tailport",
	}}
	got := gatherPublished(cfg, "host-a.tailnet.ts.net")

	if len(got) != 1 {
		t.Fatalf("gatherPublished() = %+v, want exactly 1 entry (host-b's route excluded)", got)
	}
	if want := (Published{Hostname: "myapp.example.com", Auth: true}); got[3000] != want {
		t.Errorf("gatherPublished()[3000] = %+v, want %+v", got[3000], want)
	}
}

// TestGatherPublishedIgnoresForeignRouteMatchingLabel pins the deliberate
// choice documented on gatherPublished: filtering requires BOTH backend
// Label match AND RouteInfo.Owned. Published state is defined by
// tailport-owned routes (@id = tailport-*, per v1z5's spec). A route with no
// tailport-<hostname> @id (e.g. hand-edited into Caddy's admin config, or
// created by some other tool) that happens to point at this machine's
// label:port is NOT a tailport publish -- reporting it as one would be a
// false publish/drift signal, so it must be ignored.
func TestGatherPublishedIgnoresForeignRouteMatchingLabel(t *testing.T) {
	foreign := caddyedge.Route{
		ID:     "hand-edited",
		Match:  []caddyedge.Match{{Host: []string{"manual.example.com"}}},
		Handle: []caddyedge.Handler{{Handler: "reverse_proxy", Upstreams: []caddyedge.Upstream{{Dial: "host-a:5000"}}}},
	}
	h, p := fakeCaddyAdmin(t, "tailport", []caddyedge.Route{foreign})

	cfg := config.Config{Caddy: config.CaddyConfig{
		Domain: "example.com", Hostname: h, AdminPort: p, ServerName: "tailport",
	}}
	got := gatherPublished(cfg, "host-a.tailnet.ts.net")
	if len(got) != 0 {
		t.Errorf("gatherPublished() = %+v, want empty (foreign non-owned route must be ignored, not reported as published)", got)
	}
}

// TestGatherPublishedDegradesOnUnreachableEdge covers this issue's "a List
// failure should degrade gracefully" requirement: an unreachable admin API
// must not panic or propagate an error out of gatherPublished -- it quietly
// returns nil so Gather still reports serve/funnel rows.
func TestGatherPublishedDegradesOnUnreachableEdge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h, p := splitTestServerURL(t, srv.URL)
	srv.Close() // now refuses connections

	cfg := config.Config{Caddy: config.CaddyConfig{
		Domain: "example.com", Hostname: h, AdminPort: p, ServerName: "tailport",
	}}
	got := gatherPublished(cfg, "host-a.tailnet.ts.net")
	if got != nil {
		t.Errorf("gatherPublished() = %+v, want nil (quiet degrade) when the edge is unreachable", got)
	}
}

// --- Cloudflare tunnel tests (kata afk4) -----------------------------------

// TestBuildTunnel covers the tunnel row shapes this package must emit:
//   - a named tunnel, whose hostname is recovered from tailport's sentinel
//     logfile, gets its public https URL;
//   - a quick tunnel, whose hostname cloudflared only assigns after it
//     connects, gets the URL once known;
//   - a quick tunnel still waiting for that assignment gets an EMPTY URL
//     (Row.URL's doc comment: the machine-readable schema stays URL-or-empty;
//     WriteTable supplies the pending note).
func TestBuildTunnel(t *testing.T) {
	cases := []struct {
		name    string
		tunnels []Tunnel
		port    int
		wantURL string
	}{
		{"named", []Tunnel{{Port: 8080, Hostname: "app.example.com"}}, 8080, "https://app.example.com"},
		{"quick host known", []Tunnel{{Port: 3000, Hostname: "fake-3000.trycloudflare.com"}}, 3000, "https://fake-3000.trycloudflare.com"},
		{"quick host pending", []Tunnel{{Port: 3001}}, 3001, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Build(nil, nil, nil, nil, tc.tunnels, "host-a", "host-a.tailnet.ts.net")
			if len(got) != 1 {
				t.Fatalf("Build() returned %d rows, want 1; got %+v", len(got), got)
			}
			if got[0].Mode != ModeTunnel {
				t.Errorf("Build()[0].Mode = %q, want %q", got[0].Mode, ModeTunnel)
			}
			if got[0].Port != tc.port {
				t.Errorf("Build()[0].Port = %d, want %d", got[0].Port, tc.port)
			}
			if got[0].URL != tc.wantURL {
				t.Errorf("Build()[0].URL = %q, want %q", got[0].URL, tc.wantURL)
			}
			if got[0].Auth {
				t.Errorf("Build()[0].Auth = true, want false (auth is a publish-only concept)")
			}
		})
	}
}

// TestBuildTunnelAndFunnelBothObserved pins the th05 shape: public paths
// COEXIST, so a port carrying a funnel and a Cloudflare tunnel gets one row
// per path (never collapsed to a single winner), exactly like the existing
// funnel+publish case.
func TestBuildTunnelAndFunnelBothObserved(t *testing.T) {
	got := Build(nil, nil, map[int]int{3000: 443}, nil, []Tunnel{{Port: 3000, Hostname: "fake-3000.trycloudflare.com"}}, "host-a", "host-a.tailnet.ts.net")
	if len(got) != 2 {
		t.Fatalf("Build() returned %d rows for a port with both funnel and tunnel, want 2 (one per public path); got %+v", len(got), got)
	}
	var sawFunnel, sawTunnel bool
	for _, r := range got {
		switch r.Mode {
		case ModeFunnel:
			sawFunnel = true
			if r.URL != "https://host-a.tailnet.ts.net" {
				t.Errorf("funnel row URL = %q, want the public funnel URL", r.URL)
			}
		case ModeTunnel:
			sawTunnel = true
			if r.URL != "https://fake-3000.trycloudflare.com" {
				t.Errorf("tunnel row URL = %q, want https://fake-3000.trycloudflare.com", r.URL)
			}
		default:
			t.Errorf("unexpected row %+v (Mode %q); want one funnel and one tunnel row", r, r.Mode)
		}
	}
	if !sawFunnel || !sawTunnel {
		t.Errorf("Build() = %+v, want one ModeFunnel row AND one ModeTunnel row", got)
	}
}

// TestWriteJSONTunnel covers the tunnel row in the stable JSON schema: mode
// "tunnel", the public URL, and no auth field (omitempty keeps a non-publish
// row free of it).
func TestWriteJSONTunnel(t *testing.T) {
	rows := []Row{
		{Port: 3000, Process: "python3", Mode: ModeTunnel, URL: "https://fake-3000.trycloudflare.com"},
		{Port: 3001, Process: "", Mode: ModeTunnel, URL: ""},
	}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, rows); err != nil {
		t.Fatalf("WriteJSON() error: %v", err)
	}
	var doc struct {
		Ports []map[string]any `json:"ports"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("WriteJSON() output did not parse: %v; got:\n%s", err, buf.String())
	}
	if len(doc.Ports) != 2 {
		t.Fatalf("JSON ports has %d entries, want 2; got:\n%s", len(doc.Ports), buf.String())
	}
	if got := doc.Ports[0]["mode"]; got != "tunnel" {
		t.Errorf("ports[0].mode = %v, want \"tunnel\"", got)
	}
	if got := doc.Ports[0]["url"]; got != "https://fake-3000.trycloudflare.com" {
		t.Errorf("ports[0].url = %v, want the tunnel's public URL", got)
	}
	if _, ok := doc.Ports[0]["auth"]; ok {
		t.Errorf("ports[0] carries an auth field, want it omitted for a tunnel row:\n%s", buf.String())
	}
	// A pending hostname stays an empty URL, never prose -- the note is
	// display-only (see pendingHostnameNote).
	if got, ok := doc.Ports[1]["url"]; !ok || got != "" {
		t.Errorf("ports[1].url = %v (present=%v), want \"\" for a tunnel with no hostname yet", got, ok)
	}
}

// TestWriteTableTunnel covers the TUNNEL mode label and the pending-hostname
// note: a known hostname renders its https URL; an unknown one renders
// pendingHostnameNote in the URL cell instead of a blank column.
func TestWriteTableTunnel(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)

	rows := []Row{
		{Port: 3000, Process: "python3", Mode: ModeTunnel, URL: "https://fake-3000.trycloudflare.com"},
		{Port: 3001, Process: "", Mode: ModeTunnel, URL: ""},
	}
	var buf bytes.Buffer
	WriteTable(&buf, rows)
	got := buf.String()

	for _, want := range []string{"TUNNEL (cloudflare, public)", "3000", "python3", "https://fake-3000.trycloudflare.com", "3001", pendingHostnameNote} {
		if !strings.Contains(got, want) {
			t.Errorf("WriteTable() missing %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b[") {
		t.Errorf("WriteTable() under the forced Ascii profile contains ANSI escapes; got:\n%q", got)
	}
	if strings.Contains(strings.ToLower(got), "tunnel") && !strings.Contains(got, "TUNNEL") {
		t.Errorf("WriteTable() renders tunnel without the upper-case TUNNEL marker; got:\n%s", got)
	}
	// The three public labels must stay textually distinct with color off.
	out := stripANSI(got)
	if !strings.Contains(out, "FUNNEL (public)") && strings.Contains(out, "TUNNEL (public)") {
		t.Errorf("TUNNEL's label collides with FUNNEL's; got:\n%s", out)
	}
}

// TestGatherTunnelsAbsentCloudflared pins the degrade contract from the
// ticket: when the configured cloudflared binary doesn't exist, tunnel
// gathering is a no-op -- nil, no rows, no panic -- so `tailport status`
// still reports serve/funnel/publish instead of failing. Deterministic and
// CI-safe: it points the client at a path that cannot exist rather than
// depending on cloudflared's presence on the test box.
func TestGatherTunnelsAbsentCloudflared(t *testing.T) {
	cfg := config.Config{Cloudflared: config.CloudflaredConfig{Binary: "/nonexistent/cloudflared-for-afk4"}}
	got := gatherTunnels(cfg)
	if got != nil {
		t.Errorf("gatherTunnels() = %+v, want nil when cloudflared is absent", got)
	}
}

// TestReadmeDocumentsStatusTunnels pins the README claim that `tailport
// status` reports Cloudflare tunnels (kata afk4's docs-honesty requirement):
// the subcommand's bullet must name all four exposure paths this package can
// emit, so a future README edit that drops one fails here rather than
// shipping drift.
func TestReadmeDocumentsStatusTunnels(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		// Cross-compiled/out-of-tree build: README isn't in the source tree.
		t.Skipf("README.md not readable from %q: %v", mustCwd(t), err)
	}
	body := string(readme)
	const marker = "- **`tailport status`**"
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatalf("README has no %q subcommand bullet; the docs moved or the marker changed", marker)
	}
	bullet := body[start:]
	if end := strings.Index(bullet[len(marker):], "\n- **"); end >= 0 {
		bullet = bullet[:len(marker)+end]
	}
	for _, want := range []string{"serve", "funnel", "publish", "tunnel"} {
		if !strings.Contains(strings.ToLower(bullet), want) {
			t.Errorf("README's `tailport status` bullet does not mention %q; got:\n%s", want, bullet)
		}
	}
	// roborev 5416's two visible behaviors must stay documented: one row per
	// tunnel (two on one port are not collapsed) and tunnels still reported
	// when tailscaled is down, with a non-zero exit. TestBuildTwoTunnelsOnePort
	// and TestRunStatusGatherErrorWithRows pin the behavior itself.
	for _, want := range []string{"one row each", "tailscaled is down", "non-zero"} {
		if !strings.Contains(bullet, want) {
			t.Errorf("README's `tailport status` bullet does not document %q; got:\n%s", want, bullet)
		}
	}
}

// TestReadmeDocumentsTailnetBoundApps pins the README's nvv6 claims: status
// lists a tailnet-bound app (TestBuildTailnetBoundApp pins the behavior), and
// "How it works" states the owner-then-serve-state rule the scanners and
// portscan.ResolveTailnetRows implement (TestParseSSTailnetOwnership,
// TestResolveTailnetRows).
func TestReadmeDocumentsTailnetBoundApps(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Skipf("README.md not readable from %q: %v", mustCwd(t), err)
	}
	body := strings.Join(strings.Fields(string(readme)), " ")
	for _, want := range []string{
		"An app that bound your node's tailnet IP itself is listed too, as `tailnet (app bound)` (JSON mode `tailnet`)",
		"also marks an app that bound your node's **tailnet IP** directly",
		"If the scanner can name its owner, that decides it",
		"the serve state decides: on a served port it is the proxy, on an unserved port it is an app",
		"except tailscaled's own PeerAPI ports, which `tailscale status` names and tailport never lists",
		"If serve status can't be read, such rows are treated as the proxy and not listed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("README should contain %q", want)
		}
	}
}

// mustCwd mirrors internal/ui's docs_consistency_test helper: the working
// directory, for a useful skip message.
func mustCwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		return "<unknown>"
	}
	return wd
}

// TestBuildTwoTunnelsOnePort is the regression test for roborev 5416(a): two
// owned tunnels can share one local port (two tailport instances racing).
// Build used to key tunnels by port, silently overwriting one public URL, so
// the report hid an exposure. Every owned tunnel must get its own row.
func TestBuildTwoTunnelsOnePort(t *testing.T) {
	got := Build(nil, nil, nil, nil, []Tunnel{
		{Port: 3000, Hostname: "a.trycloudflare.com"},
		{Port: 3000, Hostname: "b.trycloudflare.com"},
	}, "host-a", "host-a.tailnet.ts.net")
	if len(got) != 2 {
		t.Fatalf("Build() returned %d rows, want 2 (one per tunnel); got %+v", len(got), got)
	}
	urls := make(map[string]bool, 2)
	for _, r := range got {
		if r.Mode != ModeTunnel || r.Port != 3000 {
			t.Errorf("row %+v, want Mode=%q Port=3000", r, ModeTunnel)
		}
		urls[r.URL] = true
	}
	for _, want := range []string{"https://a.trycloudflare.com", "https://b.trycloudflare.com"} {
		if !urls[want] {
			t.Errorf("Build() = %+v, want a row with URL %q", got, want)
		}
	}
}

// TestGatherTunnelsIndependentOfTailscale is the regression test for roborev
// 5416(b): Gather used to return immediately on a tailscale status error,
// before discovering tunnels. A detached cloudflared tunnel stays public when
// tailscaled is down, so the report must still carry its row -- with the
// tailscale failure surfaced alongside it, not instead of it.
func TestGatherTunnelsIndependentOfTailscale(t *testing.T) {
	wantErr := errors.New("tailscaled is down")
	status := func() ([]int, map[int]int, error) { return nil, nil, wantErr }
	tunnels := func(config.Config) []Tunnel {
		return []Tunnel{{Port: 3000, Hostname: "fake-3000.trycloudflare.com"}}
	}
	// Every source is a fake (roborev 5419): the real port scanner needs ss
	// or lsof, and tsserve.FQDN needs tailscaled -- neither is guaranteed on a
	// clean CI runner.
	rows, err := gatherFrom(config.Config{}, gatherSources{
		listPorts:       func() ([]portscan.Port, error) { return []portscan.Port{{Number: 3000, Process: "node"}}, nil },
		fqdn:            func() (string, error) { return "", errors.New("no tailscaled") },
		tailscaleStatus: status,
		discoverTunnels: tunnels,
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("gatherFrom error = %v, want it to wrap %v", err, wantErr)
	}
	if !strings.Contains(err.Error(), "reading tailscale serve status") {
		t.Errorf("gatherFrom error = %q, want the tailscale diagnostic", err)
	}
	for _, r := range rows {
		if r.Mode == ModeTunnel && r.Port == 3000 && r.URL == "https://fake-3000.trycloudflare.com" {
			return
		}
	}
	t.Errorf("gatherFrom rows = %+v, want the tunnel row even though tailscale failed", rows)
}

// Kata nvv6: an app that bound the node's tailnet IP itself is exposed to the
// tailnet by its own bind -- no `tailscale serve` -- and `tailport status`
// lists it (mode "tailnet") at its own URL. A merely-listening wildcard/
// loopback port is still out of scope, and a served port keeps its serve row.
func TestBuildTailnetBoundApp(t *testing.T) {
	ports := []portscan.Port{
		{Number: 18951, Process: "python3", BindScope: portscan.ScopeTailnet, BindHost: "100.81.94.92", No127: true},
		{Number: 3000, Process: "node", BindScope: portscan.ScopeWildcard},
		{Number: 5173, Process: "vite", BindScope: portscan.ScopeLoopback},
		{Number: 8080, Process: "web", BindScope: portscan.ScopeTailnet}, // also served
	}
	rows := Build(ports, []int{8080}, nil, nil, nil, "lysander", "lysander.tailnet.ts.net")
	want := []Row{
		{Port: 8080, Process: "web", Mode: ModeServe, URL: "http://lysander:8080"},
		{Port: 18951, Process: "python3", Mode: ModeTailnet, URL: "http://lysander:18951"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows = %+v, want %+v", rows, want)
	}
	var buf bytes.Buffer
	WriteTable(&buf, rows[1:])
	if !strings.Contains(buf.String(), "tailnet (app bound)") || !strings.Contains(buf.String(), "http://lysander:18951") {
		t.Errorf("table = %q", buf.String())
	}
	buf.Reset()
	if err := WriteJSON(&buf, rows[1:]); err != nil || !strings.Contains(buf.String(), `"mode": "tailnet"`) {
		t.Errorf("json = %q err=%v", buf.String(), err)
	}
}

// Gather resolves the scanner's unattributed tailnet-IP rows with the serve
// set: an app run by another user on an UNSERVED port is listed; the same row
// on a SERVED port is tailscaled's proxy (a dangling forward: empty process);
// with serve status unreadable it is dropped (as before nvv6).
func TestGatherResolvesUnattributedTailnetRows(t *testing.T) {
	ports := func() ([]portscan.Port, error) {
		return []portscan.Port{
			{Number: 18951, UnattribTailnet: true, UnattribHost: "100.81.94.92"},
			{Number: 18952, UnattribTailnet: true, UnattribHost: "100.81.94.92"},
			{Number: 18954, UnattribTailnet: true, UnattribHost: "100.81.94.92"}, // tailscaled's PeerAPI
			{Number: 443, UnattribTailnet: true, UnattribHost: "100.81.94.92"},   // funnel ingress
		}, nil
	}
	src := func(status func() ([]int, map[int]int, error)) gatherSources {
		return gatherSources{
			listPorts:       ports,
			peerAPIPorts:    func() (map[int]bool, error) { return map[int]bool{18954: true}, nil },
			listenPorts:     func() (map[int]bool, error) { return map[int]bool{443: true}, nil },
			fqdn:            func() (string, error) { return "", errors.New("no tailscaled") },
			tailscaleStatus: status,
			discoverTunnels: func(config.Config) []Tunnel { return nil },
		}
	}
	rows, err := gatherFrom(config.Config{}, src(func() ([]int, map[int]int, error) { return []int{18952}, nil, nil }))
	if err != nil || len(rows) != 2 || rows[0].Port != 18951 || rows[0].Mode != ModeTailnet || rows[1].Port != 18952 || rows[1].Mode != ModeServe || rows[1].Process != "" {
		t.Fatalf("rows = %+v err=%v, want :18951 tailnet-bound and :18952 a dangling serve", rows, err)
	}
	rows, _ = gatherFrom(config.Config{}, src(func() ([]int, map[int]int, error) { return nil, nil, errors.New("down") }))
	if len(rows) != 0 {
		t.Errorf("serve state unknown must drop unattributed rows: %+v", rows)
	}
}
