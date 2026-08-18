// This file is the opt-in real-Caddy integration test named in kata pxrx.
// Everything else in this package is exercised only against in-process
// httptest fakes (see caddyedge_test.go); no real `caddy` binary had ever
// accepted the route JSON or bootstrap-caddy.json before this file existed.
//
// It is a NORMAL _test.go file with no build tag, so `go build ./...` and
// `go vet ./...` always see it -- but every test here calls exec.LookPath
// ("caddy") first and t.Skip()s immediately when it's absent. That means an
// ordinary `go test ./...` on a machine without caddy on PATH (this dev box,
// and most CI) NEVER starts a real caddy process or binds a real port. Only
// the opt-in .github/workflows/caddy-integration.yml job -- which installs a
// real caddy -- actually exercises the skipped assertions.
//
// Every caddy instance this file starts is inert to the host, by construction:
//   - automatic HTTPS is explicitly disabled on the test server (no ACME, no
//     attempt to bind :80/:443, no local CA installed into the system trust
//     store)
//   - the admin API and the http server both listen on 127.0.0.1, on
//     ephemeral ports discovered at runtime via freePort (never 80, 443, or a
//     fixed 2019)
//   - XDG_CONFIG_HOME / XDG_DATA_HOME are redirected into a t.TempDir() so
//     caddy never touches the invoking user's real config/data/storage
//   - caddy runs as the invoking user -- no sudo, no privilege change
//   - every process this file starts is killed and waited on via t.Cleanup
package caddyedge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// skipUnlessCaddy is the single gate every test in this file goes through.
// See the package-level (file-level) doc comment above for why this makes
// the file safe to run unconditionally as part of `go test ./...`.
func skipUnlessCaddy(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("caddy"); err != nil {
		t.Skip("caddy not found on PATH: skipping real-caddy integration test " +
			"(expected on dev machines and most CI; see " +
			".github/workflows/caddy-integration.yml, the opt-in job that " +
			"installs caddy and runs this test for real)")
	}
}

// freePort asks the OS for a free TCP port on 127.0.0.1 by binding to port 0
// and immediately releasing it, then returns the port number. There is an
// inherent, unavoidable TOCTOU race between releasing it here and caddy
// binding it moments later; that is the standard "find an ephemeral port"
// technique and acceptable for a test.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startCaddy writes cfg to a temp file, launches `caddy run` against it with
// XDG_CONFIG_HOME/XDG_DATA_HOME redirected into the test's temp dir, and
// blocks until adminURL's admin API answers 200 on GET /config/ (or the test
// fails on timeout). It registers a t.Cleanup that kills the process and
// waits for it to exit, logging captured output if the test failed.
func startCaddy(t *testing.T, cfg any, adminURL string) {
	t.Helper()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "caddy.json")
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal caddy config: %v", err)
	}
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		t.Fatalf("write caddy config: %v", err)
	}

	xdgConfig := filepath.Join(dir, "xdg-config")
	xdgData := filepath.Join(dir, "xdg-data")
	if err := os.MkdirAll(xdgConfig, 0o700); err != nil {
		t.Fatalf("mkdir XDG_CONFIG_HOME: %v", err)
	}
	if err := os.MkdirAll(xdgData, 0o700); err != nil {
		t.Fatalf("mkdir XDG_DATA_HOME: %v", err)
	}

	cmd := exec.Command("caddy", "run", "--config", cfgPath, "--adapter", "json")
	// No sudo, no user change -- runs as whoever invoked `go test`. Redirecting
	// these two vars keeps caddy's autosave config, storage (certs, OCSP
	// staples, ...), and any other state entirely inside the temp dir, never
	// touching the invoking user's real ~/.config or ~/.local/share.
	cmd.Env = append(os.Environ(),
		"XDG_CONFIG_HOME="+xdgConfig,
		"XDG_DATA_HOME="+xdgData,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start caddy: %v", err)
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-exited // reap; avoids a zombie and matches the single reader of exited
		if t.Failed() {
			t.Logf("caddy process output:\n%s", out.String())
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(adminURL + "/config/")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("caddy admin API at %s never became ready; output so far:\n%s", adminURL, out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestCaddyIntegration drives a real caddy process through caddyedge's
// public API (BuildRoute via Client.Publish/Unpublish/List) and asserts the
// behavior the httptest fakes in caddyedge_test.go can only approximate:
// that real Caddy accepts the route JSON, actually proxies and rewrites
// Host, enforces http_basic, and treats a republish as an idempotent PATCH.
func TestCaddyIntegration(t *testing.T) {
	skipUnlessCaddy(t)

	adminPort := freePort(t)
	httpPort := freePort(t)
	adminAddr := fmt.Sprintf("127.0.0.1:%d", adminPort)
	adminURL := "http://" + adminAddr
	httpAddr := fmt.Sprintf("127.0.0.1:%d", httpPort)

	cfg := map[string]any{
		"admin": map[string]any{
			"listen":  adminAddr,
			"origins": []string{adminAddr},
		},
		"apps": map[string]any{
			"http": map[string]any{
				"servers": map[string]any{
					"tailport": map[string]any{
						"listen": []string{httpAddr},
						// Automatic HTTPS OFF: no ACME, no attempt at :80/:443, no
						// local CA into the system trust store. Without this,
						// Caddy would try to manage certificates for the
						// domain-shaped host matchers this test publishes
						// (myapp.example.com, secure.example.com) even though the
						// server listens on a non-standard port -- a well known
						// Caddy gotcha, and exactly what this test must not do.
						"automatic_https": map[string]any{"disable": true},
						"routes":          []any{},
					},
				},
			},
		},
	}
	startCaddy(t, cfg, adminURL)

	client := &Client{AdminURL: adminURL, ServerName: "tailport"}

	// A dummy backend that echoes the Host header it received, so a request
	// proxied through Caddy can be checked for the required rewrite.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo-Host", r.Host)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "backend saw Host=%s", r.Host)
	}))
	t.Cleanup(backend.Close)
	backendPort := backend.Listener.Addr().(*net.TCPAddr).Port

	const label = "127.0.0.1" // stands in for a MagicDNS label; dial is "<label>:<port>"
	proxyBase := "http://" + httpAddr
	wantBackendHost := fmt.Sprintf("%s:%d", label, backendPort)

	t.Run("publish and proxy rewrites Host to backend, never the public hostname", func(t *testing.T) {
		ctx := context.Background()
		if err := client.Publish(ctx, "myapp.example.com", label, backendPort, nil); err != nil {
			t.Fatalf("Publish: %v", err)
		}

		req, err := http.NewRequest(http.MethodGet, proxyBase+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "myapp.example.com" // the public hostname a real client would send
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request through caddy: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, body)
		}
		if got := resp.Header.Get("X-Echo-Host"); got != wantBackendHost {
			t.Errorf("backend saw Host = %q, want %q (the required rewrite)", got, wantBackendHost)
		}
		if got := resp.Header.Get("X-Echo-Host"); got == "myapp.example.com" {
			t.Errorf("backend saw the PUBLIC hostname as Host -- the rewrite did not happen")
		}
	})

	t.Run("basic_auth: 401 without creds, 200 with correct creds", func(t *testing.T) {
		ctx := context.Background()
		hash, err := bcrypt.GenerateFromPassword([]byte("s3cret-test-password"), bcrypt.DefaultCost)
		if err != nil {
			t.Fatalf("bcrypt hash: %v", err)
		}
		auth := &BasicAuth{User: "tester", Hash: string(hash)}
		if err := client.Publish(ctx, "secure.example.com", label, backendPort, auth); err != nil {
			t.Fatalf("Publish with auth: %v", err)
		}

		req, _ := http.NewRequest(http.MethodGet, proxyBase+"/", nil)
		req.Host = "secure.example.com"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("unauthenticated request: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("unauthenticated status = %d, want 401", resp.StatusCode)
		}

		req2, _ := http.NewRequest(http.MethodGet, proxyBase+"/", nil)
		req2.Host = "secure.example.com"
		req2.SetBasicAuth("tester", "s3cret-test-password")
		resp2, err := http.DefaultClient.Do(req2)
		if err != nil {
			t.Fatalf("authenticated request: %v", err)
		}
		defer resp2.Body.Close()
		if resp2.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp2.Body)
			t.Errorf("authenticated status = %d, want 200; body = %s", resp2.StatusCode, body)
		}
		if got := resp2.Header.Get("X-Echo-Host"); got != wantBackendHost {
			t.Errorf("authenticated request: backend saw Host = %q, want %q", got, wantBackendHost)
		}
	})

	t.Run("PATCH-based idempotent republish, then Unpublish/DELETE", func(t *testing.T) {
		ctx := context.Background()

		// Republish the SAME backend: must take the PATCH path (idempotent),
		// never create a second route for the same hostname.
		if err := client.Publish(ctx, "myapp.example.com", label, backendPort, nil); err != nil {
			t.Fatalf("idempotent republish: %v", err)
		}
		routes, err := client.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		count := 0
		for _, r := range routes {
			if r.Hostname == "myapp.example.com" {
				count++
			}
		}
		if count != 1 {
			t.Errorf("routes for myapp.example.com after republish = %d, want exactly 1 (idempotent, no duplicate)", count)
		}
		// Still proxies after the idempotent republish.
		req, _ := http.NewRequest(http.MethodGet, proxyBase+"/", nil)
		req.Host = "myapp.example.com"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request after republish: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status after republish = %d, want 200", resp.StatusCode)
		}

		if err := client.Unpublish(ctx, "myapp.example.com", label, backendPort); err != nil {
			t.Fatalf("Unpublish: %v", err)
		}
		// The route is now gone; a second Unpublish hitting ErrNotFound is the
		// documented, expected outcome here -- tolerated, not treated as a bug.
		if err := client.Unpublish(ctx, "myapp.example.com", label, backendPort); !errors.Is(err, ErrNotFound) {
			t.Errorf("second Unpublish = %v, want ErrNotFound", err)
		}

		// Proxying the now-unpublished hostname must stop working (Caddy 404s
		// a host with no matching route).
		req2, _ := http.NewRequest(http.MethodGet, proxyBase+"/", nil)
		req2.Host = "myapp.example.com"
		resp2, err := http.DefaultClient.Do(req2)
		if err != nil {
			t.Fatalf("request after unpublish: %v", err)
		}
		resp2.Body.Close()
		if resp2.StatusCode == http.StatusOK {
			t.Errorf("myapp.example.com still proxies (status 200) after Unpublish")
		}
	})
}

// TestBootstrapConfigAcceptedByCaddy validates that
// packaging/caddy-edge/bootstrap-caddy.json -- the exact config the edge
// boots from in production (server_name "tailport", widened admin origins,
// empty routes) -- is accepted by a real caddy binary.
//
// It uses `caddy validate`, not `caddy run`, deliberately: validate loads and
// PROVISIONS the config end to end (checking it is well-formed and every
// referenced module is valid) but never STARTS it, so it never attempts to
// bind the bootstrap file's literal :443/:80 listeners. Binding those would
// need root and would violate this test's "never touch a real privileged
// port" contract -- and this way the exact, unmodified bytes tailport ships
// are what gets checked, not a rewritten copy.
func TestBootstrapConfigAcceptedByCaddy(t *testing.T) {
	skipUnlessCaddy(t)

	bootstrapPath, err := filepath.Abs(filepath.Join("..", "..", "packaging", "caddy-edge", "bootstrap-caddy.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bootstrapPath); err != nil {
		t.Fatalf("bootstrap config not found at %s: %v", bootstrapPath, err)
	}

	dir := t.TempDir()
	xdgConfig := filepath.Join(dir, "xdg-config")
	xdgData := filepath.Join(dir, "xdg-data")
	for _, d := range []string{xdgConfig, xdgData} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	cmd := exec.Command("caddy", "validate", "--config", bootstrapPath, "--adapter", "json")
	cmd.Env = append(os.Environ(),
		"XDG_CONFIG_HOME="+xdgConfig,
		"XDG_DATA_HOME="+xdgData,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("caddy validate %s: %v\n%s", bootstrapPath, err, out)
	}
	t.Logf("caddy validate output:\n%s", out)
}
