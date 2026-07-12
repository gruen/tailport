package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gruen/tailport/internal/selfupdate"
)

// newMockServer stands up a fake GitHub release: the API latest endpoint
// returns tag, and the asset for THIS machine plus its .sha256 are served from
// binary. shaOverride, when set, is served instead of the real digest to
// simulate a tampered download. It returns the server URL for wiring.
func newMockServer(t *testing.T, tag string, binary []byte, shaOverride string) *httptest.Server {
	t.Helper()
	u := selfupdate.NewUpdater("x")
	asset := selfupdate.AssetName(u.GOOS, u.GOARCH)
	sum := sha256.Sum256(binary)
	sha := hex.EncodeToString(sum[:])
	if shaOverride != "" {
		sha = shaOverride
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+selfupdate.Repo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name": %q}`, tag)
	})
	mux.HandleFunc("/"+asset, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(binary)
	})
	mux.HandleFunc("/"+asset+".sha256", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", sha, asset)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// updaterFor returns an Updater wired to srv with the given current version.
func updaterFor(srv *httptest.Server, current string) *selfupdate.Updater {
	u := selfupdate.NewUpdater(current)
	u.HTTPClient = srv.Client()
	u.APIBase = srv.URL
	u.DownloadBase = srv.URL
	return u
}

func TestConfirm(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"y\n", true},
		{"Y\n", true},
		{"yes\n", true},
		{"  yes  \n", true},
		{"n\n", false},
		{"no\n", false},
		{"\n", false}, // bare Enter defaults to no
		{"", false},   // EOF / closed stdin defaults to no
		{"maybe\n", false},
	}
	for _, c := range cases {
		var out bytes.Buffer
		got, err := confirm(strings.NewReader(c.in), &out, "install? [y/N] ")
		if err != nil {
			t.Errorf("confirm(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("confirm(%q) = %v, want %v", c.in, got, c.want)
		}
		if !strings.Contains(out.String(), "install? [y/N] ") {
			t.Errorf("confirm(%q) did not print the prompt", c.in)
		}
	}
}

func TestUpdateRunCheckAvailable(t *testing.T) {
	srv := newMockServer(t, "v0.2.0", []byte("bin"), "")
	u := updaterFor(srv, "0.1.0")
	var out, errB bytes.Buffer
	code := updateRun(context.Background(), u, updateFlags{checkOnly: true}, "", strings.NewReader(""), &out, &errB)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errB.String())
	}
	if !strings.Contains(out.String(), "Update available: v0.1.0 -> v0.2.0") {
		t.Errorf("stdout = %q, want an 'Update available' line", out.String())
	}
	if strings.Contains(out.String(), "Downloading") {
		t.Errorf("--check must not download; stdout = %q", out.String())
	}
}

func TestUpdateRunCheckUpToDate(t *testing.T) {
	srv := newMockServer(t, "v0.1.0", []byte("bin"), "")
	u := updaterFor(srv, "0.1.0")
	var out, errB bytes.Buffer
	code := updateRun(context.Background(), u, updateFlags{checkOnly: true}, "", strings.NewReader(""), &out, &errB)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "up to date") {
		t.Errorf("stdout = %q, want 'up to date'", out.String())
	}
}

func TestUpdateRunInstallSuccess(t *testing.T) {
	binary := []byte("BRAND NEW TAILPORT BINARY")
	srv := newMockServer(t, "v0.2.0", binary, "")
	u := updaterFor(srv, "0.1.0")

	dir := t.TempDir()
	exec := filepath.Join(dir, "tailport")
	if err := os.WriteFile(exec, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out, errB bytes.Buffer
	code := updateRun(context.Background(), u, updateFlags{assumeYes: true}, exec, strings.NewReader(""), &out, &errB)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errB.String())
	}
	got, err := os.ReadFile(exec)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(binary) {
		t.Errorf("installed content = %q, want %q", got, binary)
	}
	if info, _ := os.Stat(exec); info.Mode().Perm() != 0o755 {
		t.Errorf("installed mode = %v, want 0755", info.Mode().Perm())
	}
	if !strings.Contains(out.String(), "Updated tailport to v0.2.0") {
		t.Errorf("stdout = %q, want success line", out.String())
	}
}

func TestUpdateRunTamperedChecksumDoesNotInstall(t *testing.T) {
	binary := []byte("BRAND NEW TAILPORT BINARY")
	wrong := sha256.Sum256([]byte("attacker-swapped payload"))
	srv := newMockServer(t, "v0.2.0", binary, hex.EncodeToString(wrong[:]))
	u := updaterFor(srv, "0.1.0")

	dir := t.TempDir()
	exec := filepath.Join(dir, "tailport")
	const original = "ORIGINAL -- MUST SURVIVE"
	if err := os.WriteFile(exec, []byte(original), 0o755); err != nil {
		t.Fatal(err)
	}

	var out, errB bytes.Buffer
	code := updateRun(context.Background(), u, updateFlags{assumeYes: true}, exec, strings.NewReader(""), &out, &errB)
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero on checksum mismatch")
	}
	if !strings.Contains(errB.String(), "checksum verification FAILED") {
		t.Errorf("stderr = %q, want checksum-failure message", errB.String())
	}
	got, _ := os.ReadFile(exec)
	if string(got) != original {
		t.Errorf("binary was replaced despite bad checksum: %q", got)
	}
}

func TestUpdateRunPkgManagedRefusal(t *testing.T) {
	binary := []byte("BRAND NEW TAILPORT BINARY")
	srv := newMockServer(t, "v0.2.0", binary, "")
	u := updaterFor(srv, "0.1.0")

	// A writable path that still trips the Homebrew "/Cellar/" heuristic, so we
	// exercise the real refusal (not a stubbed detector) against a real file.
	dir := t.TempDir()
	cellarBin := filepath.Join(dir, "Cellar", "tailport", "0.1.0", "bin")
	if err := os.MkdirAll(cellarBin, 0o755); err != nil {
		t.Fatal(err)
	}
	exec := filepath.Join(cellarBin, "tailport")
	const original = "PACKAGE-MANAGED ORIGINAL"
	if err := os.WriteFile(exec, []byte(original), 0o755); err != nil {
		t.Fatal(err)
	}

	// Without --force: refuse, note the package manager, install nothing.
	var out, errB bytes.Buffer
	code := updateRun(context.Background(), u, updateFlags{assumeYes: true}, exec, strings.NewReader(""), &out, &errB)
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero refusal")
	}
	if !strings.Contains(errB.String(), "looks installed via Homebrew") {
		t.Errorf("stderr = %q, want a Homebrew note", errB.String())
	}
	if !strings.Contains(errB.String(), "command -v -a tailport") {
		t.Errorf("stderr = %q, want the PATH hint", errB.String())
	}
	if strings.Contains(out.String(), "Downloading") {
		t.Errorf("refusal must not download; stdout = %q", out.String())
	}
	if got, _ := os.ReadFile(exec); string(got) != original {
		t.Errorf("refusal must not touch the binary; got %q", got)
	}

	// With --force: still note it, but override and actually replace.
	out.Reset()
	errB.Reset()
	code = updateRun(context.Background(), u, updateFlags{assumeYes: true, force: true}, exec, strings.NewReader(""), &out, &errB)
	if code != 0 {
		t.Fatalf("--force exit = %d, want 0; stderr=%q", code, errB.String())
	}
	if !strings.Contains(errB.String(), "--force given") {
		t.Errorf("--force stderr = %q, want the override note", errB.String())
	}
	if got, _ := os.ReadFile(exec); string(got) != string(binary) {
		t.Errorf("--force should have installed; got %q", got)
	}
}

func TestUpdateRunPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission bits")
	}
	binary := []byte("BRAND NEW TAILPORT BINARY")
	srv := newMockServer(t, "v0.2.0", binary, "")
	u := updaterFor(srv, "0.1.0")

	dir := t.TempDir()
	exec := filepath.Join(dir, "tailport")
	const original = "ORIGINAL -- MUST SURVIVE"
	if err := os.WriteFile(exec, []byte(original), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	var out, errB bytes.Buffer
	code := updateRun(context.Background(), u, updateFlags{assumeYes: true}, exec, strings.NewReader(""), &out, &errB)
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero on permission error")
	}
	if !strings.Contains(errB.String(), "no write permission") {
		t.Errorf("stderr = %q, want a permission message", errB.String())
	}
	if got, _ := os.ReadFile(exec); string(got) != original {
		t.Errorf("existing binary must survive a permission error; got %q", got)
	}
}

func TestRunUpdateFlagErrors(t *testing.T) {
	// Unknown flag -> exit 2 + usage.
	var out, errB bytes.Buffer
	if code := runUpdate([]string{"--nope"}, &out, &errB); code != 2 {
		t.Errorf("runUpdate(--nope) = %d, want 2", code)
	}
	if !strings.Contains(errB.String(), "Usage:") {
		t.Errorf("bad flag should print usage; stderr=%q", errB.String())
	}

	// -h -> exit 0 + usage on stdout.
	out.Reset()
	errB.Reset()
	if code := runUpdate([]string{"-h"}, &out, &errB); code != 0 {
		t.Errorf("runUpdate(-h) = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "self-update tailport") {
		t.Errorf("-h should print usage to stdout; stdout=%q", out.String())
	}

	// Stray positional arg -> exit 2.
	out.Reset()
	errB.Reset()
	if code := runUpdate([]string{"extra"}, &out, &errB); code != 2 {
		t.Errorf("runUpdate(extra) = %d, want 2", code)
	}
}

// TestCompiledBinaryAgainstMockServer builds the real binary and drives
// `tailport update` against an httptest.Server via the env test seams, proving
// the whole path works end-to-end in a separate process with no network. It is
// the durable form of the manual verification harness.
func TestCompiledBinaryAgainstMockServer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping compiled-binary integration test in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	binDir := t.TempDir()
	bin := filepath.Join(binDir, "tailport-under-test")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("building tailport: %v", err)
	}

	payload := []byte("THE MOCK RELEASE PAYLOAD")
	srv := newMockServer(t, "v9.9.9", payload, "")

	run := func(args []string, execTarget string) (string, string, int) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(),
			envUpdateAPIURL+"="+srv.URL,
			envUpdateDownloadURL+"="+srv.URL,
		)
		if execTarget != "" {
			cmd.Env = append(cmd.Env, envUpdateExec+"="+execTarget)
		}
		var out, errB bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errB
		code := 0
		if err := cmd.Run(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				t.Fatalf("running binary: %v", err)
			}
		}
		return out.String(), errB.String(), code
	}

	// --check: the compiled `version` is "dev", so any release is newer.
	out, _, code := run([]string{"update", "--check"}, "")
	if code != 0 || !strings.Contains(out, "Update available") {
		t.Errorf("update --check: code=%d out=%q", code, out)
	}
	t.Logf("[compiled] update --check ->\n%s", out)

	// -y install into a throwaway target.
	target := filepath.Join(t.TempDir(), "tailport")
	if err := os.WriteFile(target, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, errB, code := run([]string{"update", "-y"}, target)
	if code != 0 {
		t.Fatalf("update -y: code=%d stderr=%q", code, errB)
	}
	if got, _ := os.ReadFile(target); string(got) != string(payload) {
		t.Errorf("update -y installed %q, want %q", got, payload)
	}
	t.Logf("[compiled] update -y ->\n%s", out)

	// pkg-managed refusal via a /Cellar/ target.
	managed := filepath.Join(t.TempDir(), "Cellar", "tailport", "bin", "tailport")
	if err := os.MkdirAll(filepath.Dir(managed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managed, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, errB, code = run([]string{"update", "-y"}, managed)
	if code == 0 || !strings.Contains(errB, "Homebrew") {
		t.Errorf("pkg-managed refusal: code=%d stderr=%q", code, errB)
	}
	t.Logf("[compiled] update -y (pkg-managed) ->\n%s", errB)
}
