package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in   string
		want Version
		ok   bool
	}{
		{"0.1.0", Version{0, 1, 0, ""}, true},
		{"v0.1.0", Version{0, 1, 0, ""}, true},
		{"V2.3.4", Version{2, 3, 4, ""}, true},
		{" v1.2.3 ", Version{1, 2, 3, ""}, true},
		{"1.2.3-rc1", Version{1, 2, 3, "rc1"}, true},
		{"1.2.3+build7", Version{1, 2, 3, ""}, true},
		{"1.2.3-rc1+build7", Version{1, 2, 3, "rc1"}, true},
		{"dev", Version{}, false},
		{"1.2", Version{}, false},
		{"1.2.3.4", Version{}, false},
		{"1.x.3", Version{}, false},
		{"", Version{}, false},
	}
	for _, c := range cases {
		got, err := ParseVersion(c.in)
		if c.ok {
			if err != nil {
				t.Errorf("ParseVersion(%q) unexpected error: %v", c.in, err)
				continue
			}
			if got != c.want {
				t.Errorf("ParseVersion(%q) = %+v, want %+v", c.in, got, c.want)
			}
		} else if err == nil {
			t.Errorf("ParseVersion(%q) = %+v, want error", c.in, got)
		}
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.1", -1},     // patch older
		{"1.0.1", "1.0.0", 1},      // patch newer
		{"1.0.0", "1.0.0", 0},      // equal
		{"v1.0.0", "1.0.0", 0},     // v-prefix ignored
		{"1.2.0", "1.10.0", -1},    // numeric (not lexical) minor compare
		{"2.0.0", "1.99.99", 1},    // major dominates
		{"1.0.0-rc1", "1.0.0", -1}, // prerelease < release
		{"1.0.0", "1.0.0-rc1", 1},
		{"1.0.0-rc1", "1.0.0-rc2", -1},    // single-identifier lexical ordering
		{"1.0.0-rc.2", "1.0.0-rc.10", -1}, // dotted numeric identifier compares numerically
		{"1.0.0-alpha", "1.0.0-beta", -1}, // alphabetic ordering
	}
	for _, c := range cases {
		a, err := ParseVersion(c.a)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", c.a, err)
		}
		b, err := ParseVersion(c.b)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", c.b, err)
		}
		if got := Compare(a, b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestAssetName(t *testing.T) {
	cases := []struct {
		goos, goarch, want string
	}{
		{"linux", "amd64", "tailport-linux-amd64"},
		{"linux", "arm64", "tailport-linux-arm64"},
		{"darwin", "arm64", "tailport-darwin-arm64"},
	}
	for _, c := range cases {
		if got := AssetName(c.goos, c.goarch); got != c.want {
			t.Errorf("AssetName(%q, %q) = %q, want %q", c.goos, c.goarch, got, c.want)
		}
	}
}

func TestDetectPkgManager(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/home/someone"
	}
	cases := []struct {
		path        string
		wantManaged bool
	}{
		{"/usr/bin/tailport", true},                             // pacman/AUR
		{"/opt/homebrew/bin/tailport", true},                    // Homebrew (Apple Silicon)
		{"/home/linuxbrew/.linuxbrew/bin/tailport", true},       // Homebrew (Linux)
		{"/usr/local/Cellar/tailport/0.1.0/bin/tailport", true}, // Homebrew Cellar
		{"/usr/local/bin/tailport", true},                       // ambiguous -> conservative refuse
		{filepath.Join(home, ".local/bin/tailport"), false},     // install.sh default -> NOT managed
		{"/home/dev/bin/tailport", false},                       // hand-rolled location
		{"/tmp/throwaway/tailport", false},                      // test/throwaway dir
	}
	for _, c := range cases {
		pm := DetectPkgManager(c.path)
		if pm.Managed() != c.wantManaged {
			t.Errorf("DetectPkgManager(%q).Managed() = %v (name %q), want %v", c.path, pm.Managed(), pm.Name, c.wantManaged)
		}
	}
}

func TestParseChecksumFile(t *testing.T) {
	good := hex.EncodeToString(func() []byte { s := sha256.Sum256([]byte("x")); return s[:] }())
	if got, err := ParseChecksumFile([]byte(good + "  tailport-linux-amd64\n")); err != nil || got != good {
		t.Errorf("ParseChecksumFile(GNU line) = %q, %v; want %q, nil", got, err, good)
	}
	if got, err := ParseChecksumFile([]byte(good + "\n")); err != nil || got != good {
		t.Errorf("ParseChecksumFile(bare hash) = %q, %v; want %q, nil", got, err, good)
	}
	for _, bad := range []string{"", "  \n", "nothex  file", "abc123  file"} {
		if _, err := ParseChecksumFile([]byte(bad)); err == nil {
			t.Errorf("ParseChecksumFile(%q) = nil error, want error", bad)
		}
	}
}

func TestVerifyChecksum(t *testing.T) {
	data := []byte("the real binary bytes")
	sum := sha256.Sum256(data)
	good := hex.EncodeToString(sum[:])

	if err := VerifyChecksum(data, good); err != nil {
		t.Errorf("VerifyChecksum(good) = %v, want nil", err)
	}
	// Case-insensitive on the expected digest.
	if err := VerifyChecksum(data, upper(good)); err != nil {
		t.Errorf("VerifyChecksum(upper good) = %v, want nil", err)
	}
	// Tampered data must fail with the typed error.
	if err := VerifyChecksum([]byte("tampered bytes"), good); !errors.Is(err, ErrChecksumMismatch) {
		t.Errorf("VerifyChecksum(tampered) = %v, want ErrChecksumMismatch", err)
	}
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'f' {
			b[i] = c - 32
		}
	}
	return string(b)
}

func TestReplaceExecutableSuccess(t *testing.T) {
	dir := t.TempDir()
	exec := filepath.Join(dir, "tailport")
	if err := os.WriteFile(exec, []byte("OLD BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	newData := []byte("NEW BINARY CONTENTS")

	if err := ReplaceExecutable(exec, newData, 0o755); err != nil {
		t.Fatalf("ReplaceExecutable: %v", err)
	}
	got, err := os.ReadFile(exec)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newData) {
		t.Errorf("after replace, content = %q, want %q", got, newData)
	}
	info, err := os.Stat(exec)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("after replace, mode = %v, want 0755", info.Mode().Perm())
	}
	// No stray temp files left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("expected exactly the target file to remain, got %d entries", len(entries))
	}
}

func TestReplaceExecutablePermissionDeniedDoesNotCorrupt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission bits; can't simulate EACCES")
	}
	dir := t.TempDir()
	exec := filepath.Join(dir, "tailport")
	const original = "ORIGINAL BINARY -- MUST SURVIVE"
	if err := os.WriteFile(exec, []byte(original), 0o755); err != nil {
		t.Fatal(err)
	}
	// Make the directory non-writable so CreateTemp fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) }) // let t.TempDir cleanup remove it

	err := ReplaceExecutable(exec, []byte("NEW BINARY"), 0o755)
	if err == nil {
		t.Fatal("ReplaceExecutable succeeded on a read-only dir, want error")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("error = %v, want one satisfying errors.Is(os.ErrPermission)", err)
	}
	// Critically: the existing binary must be byte-for-byte intact.
	got, readErr := os.ReadFile(exec)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != original {
		t.Errorf("existing binary was corrupted: content = %q, want %q", got, original)
	}
}

// mockRelease spins up an httptest.Server that answers the GitHub API latest
// endpoint with tag and serves the current machine's asset + its .sha256 from
// binary. shaOverride, if non-empty, is served instead of the real checksum so
// tests can simulate a tampered/mismatched download.
func mockRelease(t *testing.T, tag string, binary []byte, shaOverride string) *Updater {
	t.Helper()
	asset := AssetName(runtime_GOOS, runtime_GOARCH)
	sum := sha256.Sum256(binary)
	sha := hex.EncodeToString(sum[:])
	if shaOverride != "" {
		sha = shaOverride
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+Repo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
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

	u := NewUpdater("0.1.0")
	u.HTTPClient = srv.Client()
	u.APIBase = srv.URL
	u.DownloadBase = srv.URL
	return u
}

// runtime_GOOS/ARCH mirror the running machine so mockRelease serves the same
// asset the Updater (which defaults GOOS/GOARCH from runtime) will request.
var runtime_GOOS, runtime_GOARCH = func() (string, string) {
	u := NewUpdater("x")
	return u.GOOS, u.GOARCH
}()

func TestCheckUpdateAvailable(t *testing.T) {
	u := mockRelease(t, "v0.2.0", []byte("binary"), "")
	u.Current = "0.1.0"
	got, err := u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Available || got.Latest != "v0.2.0" {
		t.Errorf("Check() = %+v, want Available=true Latest=v0.2.0", got)
	}
}

func TestCheckUpToDate(t *testing.T) {
	u := mockRelease(t, "v0.1.0", []byte("binary"), "")
	u.Current = "0.1.0"
	got, err := u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Available {
		t.Errorf("Check() = %+v, want Available=false", got)
	}
}

func TestCheckDevBuildAlwaysUpdatable(t *testing.T) {
	u := mockRelease(t, "v0.1.0", []byte("binary"), "")
	u.Current = "dev"
	got, err := u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Available || !got.CurrentIsDev {
		t.Errorf("Check() for dev build = %+v, want Available=true CurrentIsDev=true", got)
	}
}

func TestCheckNoRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	u := NewUpdater("0.1.0")
	u.HTTPClient = srv.Client()
	u.APIBase = srv.URL
	if _, err := u.Check(context.Background()); !errors.Is(err, ErrNoRelease) {
		t.Errorf("Check() error = %v, want ErrNoRelease", err)
	}
}

func TestCheckRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	u := NewUpdater("0.1.0")
	u.HTTPClient = srv.Client()
	u.APIBase = srv.URL
	if _, err := u.Check(context.Background()); !errors.Is(err, ErrRateLimited) {
		t.Errorf("Check() error = %v, want ErrRateLimited", err)
	}
}

func TestDownloadVerifiedOK(t *testing.T) {
	binary := []byte("the freshly built tailport binary")
	u := mockRelease(t, "v0.2.0", binary, "")
	got, err := u.Download(context.Background())
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if string(got) != string(binary) {
		t.Errorf("Download returned %q, want %q", got, binary)
	}
}

func TestDownloadChecksumMismatch(t *testing.T) {
	binary := []byte("the freshly built tailport binary")
	// Serve a checksum for DIFFERENT bytes so verification fails.
	wrong := sha256.Sum256([]byte("something else entirely"))
	u := mockRelease(t, "v0.2.0", binary, hex.EncodeToString(wrong[:]))
	got, err := u.Download(context.Background())
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Errorf("Download error = %v, want ErrChecksumMismatch", err)
	}
	if got != nil {
		t.Errorf("Download returned %d bytes on mismatch, want nil (nothing to install)", len(got))
	}
}
