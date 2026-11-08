//go:build linux || darwin

package procid

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOwnIdentity reads THIS test process's identity through the real
// platform reader (/proc on Linux; ps + lsof on macOS, where the opt-in
// `[ci darwin]` job runs it) and checks it against what the Go runtime says.
// Symlinks are resolved on both sides (macOS reports /private/var for /var).
func TestOwnIdentity(t *testing.T) {
	got := Of(os.Getpid())
	if !got.Known() {
		t.Fatalf("own identity should be readable, got %+v", got)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wantExe, _ := filepath.EvalSymlinks(exe)
	gotExe, _ := filepath.EvalSymlinks(got.Exe)
	if gotExe != wantExe {
		t.Errorf("exe = %q, want %q", got.Exe, exe)
	}
	cwd, _ := os.Getwd()
	wantCwd, _ := filepath.EvalSymlinks(cwd)
	gotCwd, _ := filepath.EvalSymlinks(got.Cwd)
	if gotCwd != wantCwd {
		t.Errorf("cwd = %q, want %q", got.Cwd, cwd)
	}
	if !got.Matches(Of(os.Getpid())) {
		t.Error("a process must match itself")
	}
}
