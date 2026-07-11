package tsserve

import (
	"encoding/json"
	"reflect"
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
