package tsserve

import (
	"encoding/json"
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
