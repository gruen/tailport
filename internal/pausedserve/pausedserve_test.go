package pausedserve

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

var t0 = time.Unix(1_800_000_000, 0)

func TestSaveLoadRoundTripAndMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	p := filepath.Join(dir, FileName)
	in := map[int]Entry{
		8080: {Port: 8080, Exe: "/usr/bin/python3.12", Cwd: "/srv/a", PausedAt: t0},
		3000: {Port: 3000, Exe: "/usr/bin/node", Cwd: "/srv/b", PausedAt: t0.Add(-time.Hour)},
	}
	if err := Save(p, in); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v err=%v, want 0600", fi.Mode().Perm(), err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("state dir mode = %v, want 0700", fi.Mode().Perm())
	}
	out, err := Load(p, t0)
	if err != nil || len(out) != 2 || out[8080].Cwd != "/srv/a" || !out[3000].PausedAt.Equal(in[3000].PausedAt) {
		t.Fatalf("round trip: %+v err=%v", out, err)
	}
	// No temp litter.
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("state dir should hold only the file, got %v", ents)
	}
}

func TestLoadMissingIsEmptyNoError(t *testing.T) {
	out, err := Load(filepath.Join(t.TempDir(), "nope.json"), t0)
	if err != nil || len(out) != 0 {
		t.Fatalf("out=%v err=%v", out, err)
	}
}

func TestLoadDropsEntriesOlderThanSevenDays(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	if err := Save(p, map[int]Entry{
		1: {Port: 1, Exe: "/a", Cwd: "/c", PausedAt: t0.Add(-MaxAge + time.Minute)}, // still fresh
		2: {Port: 2, Exe: "/a", Cwd: "/c", PausedAt: t0.Add(-MaxAge - time.Minute)}, // expired
	}); err != nil {
		t.Fatal(err)
	}
	out, err := Load(p, t0)
	if err != nil || len(out) != 1 || out[1].Port != 1 {
		t.Fatalf("expiry: %+v err=%v", out, err)
	}
}

func TestLoadCorruptIsEmptyWithError(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := Load(p, t0)
	if err == nil || len(out) != 0 {
		t.Fatalf("corrupt file must yield empty + error, got %v %v", out, err)
	}
}

func TestLoadSkipsMalformedEntries(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	raw := `{"version":1,"paused":[{"port":0,"exe":"/a","cwd":"/c","paused_at":"2027-01-01T00:00:00Z"},{"port":5,"exe":"","cwd":"/c","paused_at":"2027-01-01T00:00:00Z"}]}`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := Load(p, time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)); err != nil || len(out) != 0 {
		t.Fatalf("malformed entries must be skipped: %v %v", out, err)
	}
}

func TestSymlinkIsNotFollowed(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, FileName)
	if err := os.Symlink(victim, p); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p, t0); err == nil {
		t.Error("Load must refuse to read through a symlink")
	}
	if err := Save(p, map[int]Entry{1: {Port: 1, Exe: "/a", Cwd: "/c", PausedAt: t0}}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Errorf("Save wrote through the symlink: victim = %q", b)
	}
	if fi, _ := os.Lstat(p); fi.Mode()&os.ModeSymlink != 0 {
		t.Error("Save should replace the symlink with a regular file")
	}
}

// Kata p2r5 (roborev 6305 #3): the file drives an automatic serve, so a state
// dir/file another user could have written is never trusted.
func TestLoadRefusesUntrustworthyDirOrFile(t *testing.T) {
	good := map[int]Entry{1: {Port: 1, Exe: "/a", Cwd: "/c", PausedAt: t0}}
	mk := func(t *testing.T) (dir, path string) {
		dir = filepath.Join(t.TempDir(), "state")
		path = filepath.Join(dir, FileName)
		if err := Save(path, good); err != nil {
			t.Fatal(err)
		}
		if out, err := Load(path, t0); err != nil || len(out) != 1 {
			t.Fatalf("baseline must load: %v %v", out, err)
		}
		return dir, path
	}
	refuse := func(t *testing.T, why, path string) {
		t.Helper()
		out, err := Load(path, t0)
		if err == nil || len(out) != 0 {
			t.Errorf("%s: want empty set + error, got %v %v", why, out, err)
		}
	}

	t.Run("group-writable dir", func(t *testing.T) {
		dir, path := mk(t)
		_ = os.Chmod(dir, 0o770)
		refuse(t, "group-writable dir", path)
	})
	t.Run("other-writable dir", func(t *testing.T) {
		dir, path := mk(t)
		_ = os.Chmod(dir, 0o707)
		refuse(t, "other-writable dir", path)
	})
	t.Run("symlinked dir", func(t *testing.T) {
		dir, _ := mk(t)
		link := filepath.Join(filepath.Dir(dir), "link")
		if err := os.Symlink(dir, link); err != nil {
			t.Fatal(err)
		}
		refuse(t, "symlinked dir", filepath.Join(link, FileName))
	})
	t.Run("writable parent", func(t *testing.T) {
		dir, path := mk(t)
		_ = os.Chmod(filepath.Dir(dir), 0o777) // another user could swap the dir
		refuse(t, "writable non-sticky parent", path)
		_ = os.Chmod(filepath.Dir(dir), 0o700)
	})
	t.Run("foreign-owned dir", func(t *testing.T) {
		// /etc is root-owned: not this uid (skip if we ARE root).
		if os.Getuid() == 0 {
			t.Skip("running as root")
		}
		refuse(t, "foreign-owned dir", "/etc/"+FileName)
	})
	t.Run("group-writable file", func(t *testing.T) {
		_, path := mk(t)
		_ = os.Chmod(path, 0o660)
		refuse(t, "group-writable file", path)
	})
	t.Run("non-regular file", func(t *testing.T) {
		dir, path := mk(t)
		_ = os.Remove(path)
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Skip("mkfifo unavailable:", err)
		}
		_ = dir
		refuse(t, "FIFO instead of a file", path) // must not hang either
	})
	t.Run("a directory in place of the file", func(t *testing.T) {
		_, path := mk(t)
		_ = os.Remove(path)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		refuse(t, "directory in place of the file", path)
	})
}
