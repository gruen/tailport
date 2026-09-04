package ui

import (
	"reflect"
	"testing"

	"github.com/gruen/tailport/internal/portscan"
)

func TestRoutesFor(t *testing.T) {
	tests := []struct {
		name string
		in   serviceState
		want []route
	}{
		{
			name: "localhost-only loopback",
			in: serviceState{
				port:      3000,
				bindScope: portscan.ScopeLoopback,
				listening: true,
				// host == "": tailscale down, no tailnet route possible.
			},
			want: []route{
				{kind: routeLocalhost, url: "http://localhost:3000"},
			},
		},
		{
			// th05 follow-up: a listening-but-UNCLASSIFIED bind is treated as
			// localhost-only, matching reach()'s conservative default -- it must
			// get a localhost route, not collapse to the offline pseudo-route.
			name: "unclassified (ScopeUnknown) listener -> localhost only",
			in: serviceState{
				port:      5000,
				bindScope: portscan.ScopeUnknown,
				listening: true,
				// host == "": even with tailscale up, an unknown bind gets no
				// tailnet route (the tailnet clause requires Wildcard/Tailnet).
			},
			want: []route{
				{kind: routeLocalhost, url: "http://localhost:5000"},
			},
		},
		{
			name: "wildcard bind, not served -> localhost + tailnet(served=false)",
			in: serviceState{
				port:      8080,
				bindScope: portscan.ScopeWildcard,
				listening: true,
				served:    false,
				host:      "myhost",
			},
			want: []route{
				{kind: routeLocalhost, url: "http://localhost:8080"},
				{kind: routeTailnet, served: false, stale: false, url: "http://myhost:8080"},
			},
		},
		{
			name: "loopback bind, served -> localhost + tailnet(served=true)",
			in: serviceState{
				port:      3000,
				bindScope: portscan.ScopeLoopback,
				listening: true,
				served:    true,
				host:      "myhost",
			},
			want: []route{
				{kind: routeLocalhost, url: "http://localhost:3000"},
				{kind: routeTailnet, served: true, stale: false, url: "http://myhost:3000"},
			},
		},
		{
			name: "fully exposed: loopback + served + funnel + publish + tunnel -> 5 routes in order",
			in: serviceState{
				port:         3000,
				bindScope:    portscan.ScopeLoopback,
				listening:    true,
				served:       true,
				host:         "myhost",
				funnelPub:    443,
				fqdn:         "myhost.tail1234.ts.net",
				publish:      &publishInfo{hostname: "app.example.com", auth: false},
				tunnelActive: true,
				tunnelHost:   "witty-fox-42.trycloudflare.com",
			},
			want: []route{
				{kind: routeLocalhost, url: "http://localhost:3000"},
				{kind: routeTailnet, served: true, stale: false, url: "http://myhost:3000"},
				{kind: routeFunnel, url: "https://myhost.tail1234.ts.net"},
				{kind: routePublish, url: "https://app.example.com", auth: false},
				{kind: routeTunnel, url: "https://witty-fox-42.trycloudflare.com"},
			},
		},
		{
			name: "LAN-only bind -> just LAN, no localhost",
			in: serviceState{
				port:      5432,
				bindScope: portscan.ScopeLAN,
				bindHost:  "192.168.1.5",
				listening: true,
				// host == "": no tailnet identity in this scenario.
			},
			want: []route{
				{kind: routeLAN, url: "http://192.168.1.5:5432"},
			},
		},
		{
			name: "port 22 wildcard -> ssh localhost + ssh host",
			in: serviceState{
				port:      22,
				bindScope: portscan.ScopeWildcard,
				listening: true,
				served:    false,
				host:      "myhost",
			},
			want: []route{
				{kind: routeLocalhost, url: "ssh localhost"},
				{kind: routeTailnet, served: false, stale: false, url: "ssh myhost"},
			},
		},
		{
			name: "served but not listening -> single stale tailnet route, no localhost",
			in: serviceState{
				port:      8025,
				bindScope: portscan.ScopeLoopback,
				listening: false,
				served:    true,
				host:      "myhost",
			},
			want: []route{
				{kind: routeTailnet, served: true, stale: true, url: "http://myhost:8025"},
			},
		},
		{
			name: "not listening, not served, no exposure -> offline",
			in: serviceState{
				port:      6379,
				bindScope: portscan.ScopeLoopback,
				listening: false,
				served:    false,
			},
			want: []route{
				{kind: routeOffline, url: ""},
			},
		},
		{
			name: "publish with auth -> auth true on publish route",
			in: serviceState{
				port:    443,
				publish: &publishInfo{hostname: "app.example.com", auth: true},
			},
			want: []route{
				{kind: routePublish, url: "https://app.example.com", auth: true},
			},
		},
		{
			name: "tunnel with empty host (still starting) -> tunnel route with empty url",
			in: serviceState{
				port:         8000,
				tunnelActive: true,
				tunnelHost:   "",
			},
			want: []route{
				{kind: routeTunnel, url: ""},
			},
		},
		{
			name: "host==\"\" with wildcard bind -> no tailnet route",
			in: serviceState{
				port:      8080,
				bindScope: portscan.ScopeWildcard,
				listening: true,
				served:    false,
				host:      "", // tailscale down: no tailnet identity
			},
			want: []route{
				{kind: routeLocalhost, url: "http://localhost:8080"},
			},
		},
		{
			// Bonus: a tailnet-scope (not wildcard) bind is also a "wide bind"
			// for the tailnet-route condition, but does NOT qualify for a
			// localhost route (only Loopback/Wildcard do).
			name: "tailnet-scope bind, not served -> tailnet route only, no localhost",
			in: serviceState{
				port:      9000,
				bindScope: portscan.ScopeTailnet,
				listening: true,
				served:    false,
				host:      "myhost",
			},
			want: []route{
				{kind: routeTailnet, served: false, stale: false, url: "http://myhost:9000"},
			},
		},
		{
			// Bonus: a LAN bind with tailscale also up but not served and not a
			// wide bind -- LAN scope never triggers the tailnet branch.
			name: "LAN bind with tailnet identity known -> LAN only, no tailnet route",
			in: serviceState{
				port:      5432,
				bindScope: portscan.ScopeLAN,
				bindHost:  "192.168.1.5",
				listening: true,
				served:    false,
				host:      "myhost",
			},
			want: []route{
				{kind: routeLAN, url: "http://192.168.1.5:5432"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := routesFor(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("routesFor(%+v) =\n  %#v\nwant\n  %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestRouteLabel(t *testing.T) {
	tests := []struct {
		kind routeKind
		want string
	}{
		{routeLocalhost, "localhost"},
		{routeLAN, "LAN"},
		{routeTailnet, "tailnet"},
		{routeFunnel, "ts.net"},
		{routePublish, "caddy"},
		{routeTunnel, "cloudflare"},
		{routeOffline, "offline"},
	}
	for _, tt := range tests {
		r := route{kind: tt.kind}
		if got := r.label(); got != tt.want {
			t.Errorf("route{kind: %v}.label() = %q, want %q", tt.kind, got, tt.want)
		}
	}
}

// TestRouteLabelIgnoresProvenanceAndStale locks in the design rule (§1/§2):
// served/stale never change a route's label -- only its marker (a later th05
// phase) does. A stale served tailnet route still labels "tailnet".
func TestRouteLabelIgnoresProvenanceAndStale(t *testing.T) {
	tests := []route{
		{kind: routeTailnet, served: true, stale: true},
		{kind: routeTailnet, served: true, stale: false},
		{kind: routeTailnet, served: false, stale: false},
	}
	for _, r := range tests {
		if got := r.label(); got != "tailnet" {
			t.Errorf("route{served: %v, stale: %v}.label() = %q, want \"tailnet\"", r.served, r.stale, got)
		}
	}
}
