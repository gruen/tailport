package cftunnel

// This file is W4's Tier 2 harness (design-v033-final.md, kata nc1j): a
// from-scratch fake `cloudflared` (internal/cftunnel/fakecloudflared, see its
// own doc comment) that lets the WHOLE named flow -- argv shape, console
// capture, health polling, Stop, exit detection -- be exercised end to end
// with no account and no network beyond loopback.
//
// TestFakeCloudflaredLifecycle is CI-SAFE and runs unconditionally: it never
// touches a real cloudflared, builds its own fake with the SAME Go toolchain
// running the suite, and every process it spawns is killed and waited-on via
// t.Cleanup. It runs on CI (the ordinary push/PR suite has no -short and no
// cloudflared -- see .github/workflows/ci.yml's header) precisely because it
// carries no such gate itself.
//
// TestFakeMatchesRealPlacement is the ONE test in this file that touches a
// real cloudflared, and only to compare a verdict, never to run a tunnel: per
// coordinator amendment A1, it skips unless cloudflared is actually usable
// (exec.LookPath succeeds AND `cloudflared version --short` exits 0 with
// non-empty output), matching TestNamedArgvAcceptedByRealCloudflared's own
// gate in cftunnel_test.go.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// buildFakeCloudflared compiles internal/cftunnel/fakecloudflared into
// <tmp>/cloudflared and returns that path. It is built FRESH by every caller
// under a 120s context, rather than shipped as a prebuilt binary, so it is
// always built with the exact Go toolchain running this test suite -- no
// separate release/CI step to keep in sync (W4's "go vet/gofmt" note: a
// `package main` under internal/ is fine, since internal only restricts
// imports, and `./...` still builds and vets it; releases build only
// ./cmd/tailport per build.yml).
func buildFakeCloudflared(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on $PATH: cannot build the fake cloudflared")
	}
	out := filepath.Join(t.TempDir(), "cloudflared")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, ".")
	cmd.Dir = "fakecloudflared" // relative to this package's directory (go test's cwd)
	if outp, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building fake cloudflared: %v\n%s", err, outp)
	}
	return out
}

// killAndWait force-kills pid and waits (bounded) for Alive(pid) to go
// false, so a t.Cleanup never leaves a spawned fake cloudflared running past
// its test.
func killAndWait(t *testing.T, pid int) {
	t.Helper()
	if pid <= 0 {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Signal(syscall.SIGKILL)
	}
	deadline := time.Now().Add(5 * time.Second)
	for Alive(pid) {
		if time.Now().After(deadline) {
			t.Errorf("pid %d still alive 5s after cleanup SIGKILL", pid)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestFakeCloudflaredLifecycle drives the fake through the four scenarios
// W4's brief calls out. Every subtest is bounded well under 10s and cleans up
// after itself; none makes a network call beyond 127.0.0.1.
func TestFakeCloudflaredLifecycle(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on $PATH")
	}
	bin := buildFakeCloudflared(t)

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".cloudflared"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".cloudflared", "cert.pem"), []byte("fake-cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("TUNNEL_ORIGIN_CERT", "") // don't let an ambient override win over HOME
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	c := &Client{Binary: bin}

	// Not run with t.Parallel(): the FAKE_CF_FAIL_AFTER subtests below use
	// t.Setenv, which is process-global and forbidden alongside parallel
	// subtests: keeping every subtest here sequential is what lets them do
	// that safely.
	t.Run("named: argv order, Discover, Health ready, Stop", func(t *testing.T) {
		r, err := c.Start(Spec{Port: 19080, Mode: ModeNamed, TunnelName: "web", Hostname: "app.example.com"})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(func() { killAndWait(t, r.PID) })

		running, err := c.Discover()
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		var found *Running
		for i := range running {
			if running[i].PID == r.PID {
				found = &running[i]
			}
		}
		if found == nil {
			t.Fatalf("Discover did not find pid %d among %+v", r.PID, running)
		}
		// This is the argv-order assertion: parseRunning only recovers Owned/
		// Mode/TunnelName/Hostname/LogFile correctly if the fake's own
		// "Incorrect Usage" gate (mirroring the real binary's, kata nc1j)
		// didn't fire -- i.e. buildArgs put the tunnel-level flags before
		// `run`. A regression back to the old order would make the fake print
		// "Incorrect Usage" and exit 0, which Start's own liveness check would
		// have already turned into an error above.
		if !found.Owned || found.Mode != ModeNamed || found.TunnelName != "web" ||
			found.Hostname != "app.example.com" || found.LogFile == "" {
			t.Errorf("discovered = %+v, want Owned named \"web\" @ app.example.com with a LogFile", *found)
		}

		readyBy := time.Now().Add(3 * time.Second)
		for {
			if h := c.Health(context.Background(), found.MetricsPort); h.Ready {
				break
			}
			if time.Now().After(readyBy) {
				t.Fatal("tunnel never became ready (fake's /ready never flipped)")
			}
			time.Sleep(50 * time.Millisecond)
		}

		if err := c.Stop(r.PID, 19080); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		goneBy := time.Now().Add(2 * time.Second)
		for Alive(r.PID) {
			if time.Now().After(goneBy) {
				t.Fatal("pid still alive 2s after Stop")
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	t.Run("quick: hostname appears", func(t *testing.T) {
		r, err := c.Start(Spec{Port: 19081, Mode: ModeQuick})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(func() { killAndWait(t, r.PID) })

		hostBy := time.Now().Add(3 * time.Second)
		for {
			h := c.Health(context.Background(), r.MetricsPort)
			if h.Hostname != "" {
				if want := "fake-19081.trycloudflare.com"; h.Hostname != want {
					t.Errorf("quick hostname = %q, want %q", h.Hostname, want)
				}
				break
			}
			if time.Now().After(hostBy) {
				t.Fatal("quick tunnel hostname never appeared")
			}
			time.Sleep(50 * time.Millisecond)
		}
	})

	t.Run("FAIL_AFTER=10ms: Start's own error carries the message", func(t *testing.T) {
		t.Setenv("FAKE_CF_FAIL_AFTER", "10ms:boom from the fake")
		_, err := c.Start(Spec{Port: 19082, Mode: ModeQuick})
		if err == nil {
			t.Fatal("expected Start to surface the fake's near-immediate failure")
		}
		if !strings.Contains(err.Error(), "boom from the fake") {
			t.Errorf("error = %q, want it to contain the fake's message", err.Error())
		}
		// Nothing to clean up: Start returning an error means startupGrace
		// already caught the exit, so no process is left running.
	})

	t.Run("FAIL_AFTER=800ms: Start succeeds, then exits with the message in ConsoleTail", func(t *testing.T) {
		t.Setenv("FAKE_CF_FAIL_AFTER", "800ms:delayed boom")
		r, err := c.Start(Spec{Port: 19083, Mode: ModeQuick})
		if err != nil {
			t.Fatalf("Start should succeed (800ms is past startupGrace=%v): %v", startupGrace, err)
		}
		t.Cleanup(func() { killAndWait(t, r.PID) })

		deadBy := time.Now().Add(3 * time.Second)
		for Alive(r.PID) {
			if time.Now().After(deadBy) {
				t.Fatal("process should have exited on its own by FAKE_CF_FAIL_AFTER=800ms")
			}
			time.Sleep(50 * time.Millisecond)
		}

		tail := ConsoleTail(ConsolePath(r.LogFile))
		if !strings.Contains(tail, "delayed boom") {
			t.Errorf("ConsoleTail(%q) = %q, want it to contain %q", ConsolePath(r.LogFile), tail, "delayed boom")
		}
	})
}

// TestFakeMatchesRealPlacement is the Tier 1/2 cross-check (design-v033-
// final.md, kata nc1j): the fake and the real cloudflared binary must agree
// on the SAME "Incorrect Usage" verdict for the new (correct) and old
// (broken, pre-nc1j) named-argv flag placements. This is what makes the fake
// trustworthy as a stand-in for TestFakeCloudflaredLifecycle's argv-order
// assertion above -- if the fake accepted an order the real binary rejects
// (or vice versa), the lifecycle test could pass while the real feature was
// still broken.
//
// Per coordinator amendment A1, this skips (not fails) unless cloudflared is
// not just present but actually USABLE: exec.LookPath succeeding is not
// enough by itself -- `cloudflared version --short` must also exit 0 with
// non-empty output. That's what lets the "simulate CI" gate (a broken
// `exit 1` stand-in first on $PATH) make this skip rather than fail.
func TestFakeMatchesRealPlacement(t *testing.T) {
	realBin, err := exec.LookPath("cloudflared")
	if err != nil {
		t.Skip("cloudflared not on $PATH")
	}
	{
		ctx, cancel := context.WithTimeout(context.Background(), detectTimeout)
		out, verr := exec.CommandContext(ctx, realBin, "version", "--short").Output()
		cancel()
		if verr != nil || strings.TrimSpace(string(out)) == "" {
			t.Skip("cloudflared present but not usable (version --short failed or empty)")
		}
	}

	fakeBin := buildFakeCloudflared(t)
	home := t.TempDir()

	// The per-tunnel --config file must exist with valid content for the
	// REAL binary to parse it (see cftunnel.writeTunnelConfig, S4); the fake
	// doesn't care, but sharing one file keeps both probes identical. This
	// probe has no positional tunnel name, so the exact ingress content
	// doesn't matter -- any valid YAML that isn't a zero-byte file will do.
	configPath := filepath.Join(home, "cftunnel-test.yml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The named argv's flags minus the trailing positional (there's no real
	// tunnel to run against), plus --help so both binaries parse the flags
	// and print usage without ever dialing out -- mirrors
	// TestNamedArgvAcceptedByRealCloudflared's probe exactly, so the two
	// tests are provably checking the same shapes.
	newArgs := []string{
		"tunnel",
		"--config", configPath,
		"--metrics", "127.0.0.1:0",
		"--logfile", filepath.Join(home, "cftunnel-test.log"),
		"--no-autoupdate",
		"run",
		"--url", "http://localhost:1",
		"--help",
	}
	oldArgs := []string{"tunnel", "run", "--url", "http://localhost:1", "--metrics", "127.0.0.1:0", "--help"}
	// --config is ALSO tunnel-level-only (the hermetic-config fix, kata
	// nc1j): placed after `run`, it must be rejected identically by both
	// binaries too.
	configAfterRunArgs := []string{"tunnel", "run", "--config", configPath, "--help"}

	cases := []struct {
		name string
		args []string
		want bool // want "Incorrect Usage" present
	}{
		{"new order (tunnel-level flags before run, including --config) is accepted", newArgs, false},
		{"old order (tunnel-level flags after run) is rejected", oldArgs, true},
		{"--config after run is rejected", configAfterRunArgs, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			realCmd := exec.CommandContext(ctx, realBin, tc.args...)
			realCmd.Env = append(os.Environ(), "HOME="+home)
			realOut, _ := realCmd.CombinedOutput()
			realRejected := strings.Contains(string(realOut), "Incorrect Usage")
			if realRejected != tc.want {
				t.Fatalf("real cloudflared's own verdict is not what this test expects (Incorrect Usage=%v, want %v) -- the probe shape may have drifted from cftunnel.buildArgs:\n%s", realRejected, tc.want, realOut)
			}

			fakeCmd := exec.CommandContext(ctx, fakeBin, tc.args...)
			fakeOut, _ := fakeCmd.CombinedOutput()
			fakeRejected := strings.Contains(string(fakeOut), "Incorrect Usage")

			if fakeRejected != realRejected {
				t.Errorf("fake and real cloudflared DISAGREE on %q: fake Incorrect Usage=%v, real=%v\nfake output:\n%s\nreal output:\n%s",
					tc.name, fakeRejected, realRejected, fakeOut, realOut)
			}
		})
	}
}
