package tsserve

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestCollectPorts(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []int
	}{
		{"empty", `{}`, nil},
		{
			"tcp map with bare port keys",
			`{"TCP": {"8080": {"HTTP": true}}}`,
			[]int{8080},
		},
		{
			"web map with host:port keys",
			`{"Web": {"host-a.example.ts.net:8080": {"Handlers": {"/": {"Proxy": "http://127.0.0.1:8080"}}}}}`,
			[]int{8080},
		},
		{
			"both maps agreeing, deduped",
			`{"TCP": {"3000": {}}, "Web": {"host:3000": {}}}`,
			[]int{3000},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var raw map[string]any
			if err := json.Unmarshal([]byte(c.raw), &raw); err != nil {
				t.Fatal(err)
			}
			found := map[int]bool{}
			collectPorts(raw, found)
			var got []int
			for p := range found {
				got = append(got, p)
			}
			if len(got) != len(c.want) {
				t.Fatalf("collectPorts(%s) = %v, want %v", c.raw, got, c.want)
			}
			for _, w := range c.want {
				if !found[w] {
					t.Errorf("collectPorts(%s) = %v, want %v", c.raw, got, c.want)
				}
			}
		})
	}
}

// A funnel of local :3000 on public :443 (Web handler for host:443 proxying
// to 127.0.0.1:3000, flagged in AllowFunnel), alongside a plain tailnet serve
// of :8080. Mirrors `tailscale serve status --json` on 1.98.8.
const funnelConfigJSON = `{
  "TCP": {"443": {"HTTPS": true}, "8080": {"HTTP": true}},
  "Web": {
    "host.example.ts.net:443": {"Handlers": {"/": {"Proxy": "http://127.0.0.1:3000"}}},
    "host.example.ts.net:8080": {"Handlers": {"/": {"Proxy": "http://127.0.0.1:8080"}}}
  },
  "AllowFunnel": {"host.example.ts.net:443": true}
}`

// TestParseFunnel covers the local->public mapping extraction (yt69): the
// public port comes from the AllowFunnel key's suffix, the local target from
// that handler's proxy URL.
func TestParseFunnel(t *testing.T) {
	got, err := parseFunnel([]byte(funnelConfigJSON))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[int]int{3000: 443}; !reflect.DeepEqual(got, want) {
		t.Errorf("parseFunnel = %v, want %v", got, want)
	}

	// No funnel -> empty map, not nil-panic.
	got, err = parseFunnel([]byte(`{"Web": {"host:8080": {}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("parseFunnel(no funnel) = %v, want empty", got)
	}
}

// TestFunnelPublicPortsExcludedFromActive covers ActivePorts' funnel guard
// (yt69): a funnel's public ingress (:443, in AllowFunnel) must NOT be
// reported as a served local port, but a genuine tailnet serve (:8080) must.
func TestFunnelPublicPortsExcludedFromActive(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(funnelConfigJSON), &raw); err != nil {
		t.Fatal(err)
	}
	pub := funnelPublicPorts(raw)
	if !pub[443] {
		t.Errorf("funnelPublicPorts should include 443; got %v", pub)
	}
	if pub[8080] {
		t.Errorf("funnelPublicPorts should NOT include the plain serve :8080; got %v", pub)
	}

	// Simulate ActivePorts' exclusion step on the collected ports.
	found := map[int]bool{}
	collectPorts(raw, found)
	for p := range pub {
		delete(found, p)
	}
	if found[443] {
		t.Error("ActivePorts must exclude funnel public port :443")
	}
	if !found[8080] {
		t.Error("ActivePorts must keep genuine tailnet serve :8080")
	}
}

// TestStatusParsers covers e40f's dedupe: activePortsFrom and parseFunnel,
// which Status() runs on a SINGLE serve-status fetch, agree on one config --
// serve reports the local :8080, funnel reports {local 3000 -> public 443},
// and the funnel's public :443 is not mistaken for a served local port.
func TestStatusParsers(t *testing.T) {
	active, err := activePortsFrom([]byte(funnelConfigJSON))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(active, []int{8080}) {
		t.Errorf("activePortsFrom = %v, want [8080] (funnel :443 excluded)", active)
	}
	funnel, err := parseFunnel([]byte(funnelConfigJSON))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(funnel, map[int]int{3000: 443}) {
		t.Errorf("parseFunnel = %v, want {3000:443}", funnel)
	}
}

// TestPublicURL covers the URL shown in the funnel confirm: :443 is implicit,
// the other ingress ports are explicit.
func TestPublicURL(t *testing.T) {
	cases := map[int]string{
		443:   "https://host.example.ts.net",
		8443:  "https://host.example.ts.net:8443",
		10000: "https://host.example.ts.net:10000",
	}
	for port, want := range cases {
		if got := PublicURL("host.example.ts.net", port); got != want {
			t.Errorf("PublicURL(port=%d) = %q, want %q", port, got, want)
		}
	}
}

// operatorDeniedOutput is tailscale's real combined output (1.98.8) when the
// invoking user isn't the configured operator, quoted verbatim from kata
// tapv's issue body -- the fixture classifyServeErr must recognize.
const operatorDeniedOutput = `sending serve config: Access denied: over-a-loop
Use 'sudo tailscale serve --bg --http=1025 1025'.
To not require root, use 'sudo tailscale set --operator=$USER' once.
`

// TestClassifyServeErr covers kata tapv's typed-error requirement: a
// representative Access-denied/operator stderr maps to ErrOperatorNotSet
// (via errors.Is, since it's returned as the bare sentinel), while an
// unrelated failure is wrapped verbatim as before and must NOT be
// misclassified.
func TestClassifyServeErr(t *testing.T) {
	underlying := errors.New("exit status 1")

	got := classifyServeErr("tailscale serve --bg --http=1025 1025", underlying, []byte(operatorDeniedOutput))
	if !errors.Is(got, ErrOperatorNotSet) {
		t.Errorf("classifyServeErr(operator-denied output) = %v, want ErrOperatorNotSet", got)
	}

	// Unrelated failure: a normal "address already in use"-style error must
	// NOT be misclassified as the operator error, and must still carry the
	// command/output context (previous un-typed behavior).
	other := classifyServeErr("tailscale serve --bg --http=8080 8080", underlying,
		[]byte("serve: address already in use"))
	if errors.Is(other, ErrOperatorNotSet) {
		t.Errorf("classifyServeErr(unrelated output) = %v, must not be ErrOperatorNotSet", other)
	}
	if !errors.Is(other, underlying) {
		t.Errorf("classifyServeErr(unrelated output) = %v, want it to wrap the underlying error", other)
	}
	if got, want := other.Error(), "address already in use"; !strings.Contains(got, want) {
		t.Errorf("classifyServeErr(unrelated output) = %q, want it to contain %q", got, want)
	}
}

// TestFunnelErrorOperatorNotSet covers the funnel path sharing the same
// operator classification as serve (funnelError), ahead of its existing
// "funnel isn't enabled" and generic-wrap branches.
func TestFunnelErrorOperatorNotSet(t *testing.T) {
	underlying := errors.New("exit status 1")
	got := funnelError("tailscale funnel --bg --https=443 3000", underlying, []byte(operatorDeniedOutput))
	if !errors.Is(got, ErrOperatorNotSet) {
		t.Errorf("funnelError(operator-denied output) = %v, want ErrOperatorNotSet", got)
	}

	// The pre-existing "funnel not enabled" classification must still work
	// (unaffected by the new operator check ahead of it).
	notEnabled := funnelError("tailscale funnel --bg --https=443 3000", underlying,
		[]byte("Funnel not available; HTTPS is not enabled for your tailnet."))
	if !errors.Is(notEnabled, errFunnelNotEnabled) {
		t.Errorf("funnelError(not-enabled output) = %v, want errFunnelNotEnabled", notEnabled)
	}
}

// TestOperatorMismatch covers the pure comparison DetectOperatorNotSet
// builds on, against `tailscale debug prefs` JSON fixtures: operator set to
// the current user (no mismatch), unset (empty string -- mismatch), and set
// to a DIFFERENT user (mismatch). Malformed JSON degrades to ok=false ("no
// opinion"), never a guessed answer.
func TestOperatorMismatch(t *testing.T) {
	prefsJSON := func(operator string) []byte {
		return []byte(fmt.Sprintf(`{"ControlURL":"https://controlplane.tailscale.com","OperatorUser":%q,"WantRunning":true}`, operator))
	}

	cases := []struct {
		name         string
		json         []byte
		currentUser  string
		wantMismatch bool
		wantOK       bool
	}{
		{"operator matches current user", prefsJSON("mg"), "mg", false, true},
		{"operator unset (empty string)", prefsJSON(""), "mg", true, true},
		{"operator set to a different user", prefsJSON("root"), "mg", true, true},
		{"malformed JSON is inconclusive, not a guess", []byte("not json"), "mg", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mismatch, ok := operatorMismatch(c.json, c.currentUser)
			if mismatch != c.wantMismatch || ok != c.wantOK {
				t.Errorf("operatorMismatch(%s, %q) = (%v, %v), want (%v, %v)",
					c.json, c.currentUser, mismatch, ok, c.wantMismatch, c.wantOK)
			}
		})
	}
}

// TestIsOperatorNotSet is a narrower unit check on the substring match
// classifyServeErr/funnelError share, confirming it's anchored on
// tailscale's specific remedy text rather than firing on any "access
// denied"-shaped output.
func TestIsOperatorNotSet(t *testing.T) {
	if !isOperatorNotSet([]byte(operatorDeniedOutput)) {
		t.Error("isOperatorNotSet(operator-denied output) = false, want true")
	}
	if isOperatorNotSet([]byte("Access denied: some other permission issue entirely")) {
		t.Error(`isOperatorNotSet("Access denied" without the operator remedy) = true, want false`)
	}
	if isOperatorNotSet(nil) {
		t.Error("isOperatorNotSet(nil) = true, want false")
	}
}
