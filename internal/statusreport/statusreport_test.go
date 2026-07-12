package statusreport

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

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

	got := Build(ports, active, funnel, "host-a", "host-a.tailnet.ts.net")

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
	got := Build(nil, []int{3000}, map[int]int{3000: 443}, "host-a", "host-a.tailnet.ts.net")
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
	got := Build(nil, nil, nil, "host-a", "")
	if got == nil {
		t.Fatal("Build(nil, nil, nil, ...) = nil, want a non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("Build(nil, nil, nil, ...) = %+v, want empty", got)
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
	if !strings.Contains(got, "No ports currently exposed") {
		t.Errorf("WriteTable(nil) = %q, want a no-ports-exposed message", got)
	}
	if strings.Contains(got, "MODE") {
		t.Errorf("WriteTable(nil) = %q, want no table header when there's nothing to show", got)
	}
}
