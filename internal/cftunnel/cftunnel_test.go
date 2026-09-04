package cftunnel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestBuildArgs(t *testing.T) {
	quick := buildArgs(Spec{Port: 3000, Mode: ModeQuick, MetricsPort: 20941}, "/state/cftunnel-3000.log")
	wantQuick := []string{
		"tunnel",
		"--url", "http://localhost:3000",
		"--metrics", "127.0.0.1:20941",
		"--logfile", "/state/cftunnel-3000.log",
		"--no-autoupdate",
	}
	if !reflect.DeepEqual(quick, wantQuick) {
		t.Errorf("quick args:\n got %q\nwant %q", quick, wantQuick)
	}

	named := buildArgs(Spec{Port: 8080, Mode: ModeNamed, TunnelName: "web", MetricsPort: 20942}, "/state/cftunnel-8080.log")
	wantNamed := []string{
		"tunnel", "run",
		"--url", "http://localhost:8080",
		"--metrics", "127.0.0.1:20942",
		"--logfile", "/state/cftunnel-8080.log",
		"--no-autoupdate",
		"web", // trailing positional -- namedTunnelName relies on this
	}
	if !reflect.DeepEqual(named, wantNamed) {
		t.Errorf("named args:\n got %q\nwant %q", named, wantNamed)
	}
}

func TestParseRunning(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want Running
		ok   bool
	}{
		{
			name: "quick owned",
			args: []string{"cloudflared", "tunnel",
				"--url", "http://localhost:3000",
				"--metrics", "127.0.0.1:20941",
				"--logfile", "/home/u/.local/state/tailport/cftunnel-3000.log",
				"--no-autoupdate"},
			want: Running{Port: 3000, Mode: ModeQuick, MetricsPort: 20941, Owned: true},
			ok:   true,
		},
		{
			name: "named owned (hostname recovered from logfile)",
			args: []string{"/usr/bin/cloudflared", "tunnel", "run",
				"--url", "http://localhost:8080",
				"--metrics", "127.0.0.1:20942",
				"--logfile", "/home/u/.local/state/tailport/cftunnel-8080-app.example.com.log",
				"--no-autoupdate", "web"},
			want: Running{Port: 8080, Mode: ModeNamed, TunnelName: "web", MetricsPort: 20942, Hostname: "app.example.com", Owned: true},
			ok:   true,
		},
		{
			name: "foreign quick (no sentinel logfile)",
			args: []string{"cloudflared", "tunnel", "--url", "http://localhost:5000"},
			want: Running{Port: 5000, Mode: ModeQuick, Owned: false},
			ok:   true,
		},
		{
			name: "foreign with a non-tailport logfile",
			args: []string{"cloudflared", "tunnel", "--url", "http://localhost:5000", "--logfile", "/var/log/cloudflared.log"},
			want: Running{Port: 5000, Mode: ModeQuick, Owned: false},
			ok:   true,
		},
		{
			name: "equals-form flags",
			args: []string{"cloudflared", "tunnel",
				"--url=http://localhost:9000",
				"--metrics=127.0.0.1:20950",
				"--logfile=/x/cftunnel-9000.log"},
			want: Running{Port: 9000, Mode: ModeQuick, MetricsPort: 20950, Owned: true},
			ok:   true,
		},
		{
			name: "not a tunnel invocation",
			args: []string{"cloudflared", "update"},
			ok:   false,
		},
		{
			name: "tunnel but no --url (config-file ingress -> unmappable)",
			args: []string{"cloudflared", "tunnel", "run", "web"},
			ok:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseRunning(42, tt.args)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			tt.want.PID = 42
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("\n got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestFlagValue(t *testing.T) {
	args := []string{"tunnel", "--url", "http://localhost:1", "--metrics=127.0.0.1:2", "--no-autoupdate"}
	if v, ok := flagValue(args, "--url"); !ok || v != "http://localhost:1" {
		t.Errorf("--url space form: %q %v", v, ok)
	}
	if v, ok := flagValue(args, "--metrics"); !ok || v != "127.0.0.1:2" {
		t.Errorf("--metrics equals form: %q %v", v, ok)
	}
	if _, ok := flagValue(args, "--logfile"); ok {
		t.Error("--logfile should be absent")
	}
	// a trailing flag with no following value must not panic or falsely match
	if _, ok := flagValue([]string{"tunnel", "--url"}, "--url"); ok {
		t.Error("dangling --url should report absent value")
	}
}

func TestNamedTunnelName(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"cloudflared", "tunnel", "run", "--url", "http://localhost:80", "--logfile", "/x/cftunnel-80.log", "web"}, "web"},
		{[]string{"cloudflared", "tunnel", "run", "my-tunnel"}, "my-tunnel"},
		{[]string{"cloudflared", "tunnel", "run", "--metrics", "127.0.0.1:1", "prod-api"}, "prod-api"},
		{[]string{"cloudflared", "tunnel", "run"}, ""}, // no name present
	}
	for _, c := range cases {
		if got := namedTunnelName(c.args); got != c.want {
			t.Errorf("namedTunnelName(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}

func TestSentinelHost(t *testing.T) {
	// (path -> expected hostname); all of these are ours.
	yes := map[string]string{
		"/home/u/.local/state/tailport/cftunnel-3000.log": "",
		"cftunnel-22.log":        "",
		`C:\x\cftunnel-8080.log`: "",
		"/home/u/.local/state/tailport/cftunnel-8080-app.example.com.log": "app.example.com",
		"cftunnel-443-api.corp.internal.log":                              "api.corp.internal",
	}
	for p, wantHost := range yes {
		host, ok := sentinelHost(p)
		if !ok {
			t.Errorf("sentinelHost(%q) ok = false, want true", p)
			continue
		}
		if host != wantHost {
			t.Errorf("sentinelHost(%q) host = %q, want %q", p, host, wantHost)
		}
	}
	no := []string{
		"/var/log/cloudflared.log",
		"/x/cftunnel.log",        // no port
		"/x/cftunnel-3000.txt",   // wrong ext
		"/x/mycftunnel-3000.log", // prefix garbage
		"",
	}
	for _, p := range no {
		if _, ok := sentinelHost(p); ok {
			t.Errorf("sentinelHost(%q) ok = true, want false", p)
		}
	}
}

func TestParseLocalPort(t *testing.T) {
	if p, ok := parseLocalPort("http://localhost:3000"); !ok || p != 3000 {
		t.Errorf("localhost: %d %v", p, ok)
	}
	if p, ok := parseLocalPort("http://127.0.0.1:8080"); !ok || p != 8080 {
		t.Errorf("127.0.0.1: %d %v", p, ok)
	}
	if _, ok := parseLocalPort("http://localhost"); ok {
		t.Error("missing port should fail")
	}
}

func TestParseMetricsPort(t *testing.T) {
	if parseMetricsPort("127.0.0.1:20941") != 20941 {
		t.Error("127.0.0.1 host")
	}
	if parseMetricsPort("localhost:1") != 1 {
		t.Error("localhost host")
	}
	if parseMetricsPort("garbage") != 0 {
		t.Error("garbage should be 0")
	}
}

func TestHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ready":
			// the real endpoint carries an extra connectorId field, which our
			// parser must tolerate.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":200,"readyConnections":2,"connectorId":"abc"}`))
		case "/quicktunnel":
			_, _ = w.Write([]byte(`{"hostname":"foo-bar-baz.trycloudflare.com"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	port := serverPort(t, srv)
	c := &Client{HTTPClient: srv.Client()}
	h := c.Health(context.Background(), port)
	if !h.Ready || h.ReadyConnections != 2 {
		t.Errorf("ready: %+v", h)
	}
	if h.Hostname != "foo-bar-baz.trycloudflare.com" {
		t.Errorf("hostname: %q", h.Hostname)
	}
}

func TestHealthNotReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ready" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":503,"readyConnections":0}`))
			return
		}
		w.WriteHeader(http.StatusNotFound) // no /quicktunnel yet
	}))
	defer srv.Close()

	c := &Client{HTTPClient: srv.Client()}
	h := c.Health(context.Background(), serverPort(t, srv))
	if h.Ready {
		t.Error("should not be ready on 503")
	}
	if h.Hostname != "" {
		t.Errorf("hostname should be empty, got %q", h.Hostname)
	}
}

func TestHealthZeroMetricsPort(t *testing.T) {
	c := &Client{}
	if h := c.Health(context.Background(), 0); h.Ready || h.Hostname != "" {
		t.Errorf("zero metrics port should short-circuit: %+v", h)
	}
}

// serverPort extracts the numeric port an httptest.Server is listening on, so a
// test can feed it to Health as the metrics port (Health builds
// http://127.0.0.1:<port>/... itself).
func serverPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	_, p, ok := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	if !ok {
		t.Fatalf("cannot parse server URL %q", srv.URL)
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		t.Fatalf("port %q: %v", p, err)
	}
	return n
}
