package pausedserve

import (
	"os"
	"path/filepath"
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
