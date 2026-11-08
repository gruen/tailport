// Package pausedserve persists the "paused serves" memory (kata p2r5): a serve
// forward tailport removed because its app stopped, remembered together with
// the identity (executable + working directory) of the program that was behind
// it, so a relaunched tailport can still resume it when the same app returns.
//
// The file lives in tailport's state dir (not config.yaml: it is machine-local
// runtime memory, not preference), mode 0600, written atomically, and opened
// without following symlinks (the same hygiene as cftunnel's state files,
// via cftunnel.EnsureStateDir).
package pausedserve

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/gruen/tailport/internal/cftunnel"
)

// MaxAge is how long a paused entry is kept: it expires 7 days after pausing.
const MaxAge = 7 * 24 * time.Hour

// FileName is the paused-serves file inside the state dir.
const FileName = "paused-serves.json"

// Entry is one paused serve.
type Entry struct {
	Port     int       `json:"port"`
	Exe      string    `json:"exe"`
	Cwd      string    `json:"cwd"`
	PausedAt time.Time `json:"paused_at"`
}

// Expired reports whether e is older than MaxAge at now.
func (e Entry) Expired(now time.Time) bool { return now.Sub(e.PausedAt) > MaxAge }

type fileFormat struct {
	Version int     `json:"version"`
	Paused  []Entry `json:"paused"`
}

// Path resolves the paused-serves file location in tailport's state dir.
func Path() (string, error) {
	dir, err := cftunnel.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// Load reads path, dropping expired entries (and any malformed ones: no port,
// no identity). A missing file is an empty set with no error. A corrupt file is
// an EMPTY set plus a non-nil error, so the caller can say so once and carry
// on; the bad file is left in place until the next Save overwrites it.
//
// This file drives an AUTOMATIC serve, so nothing in it is trusted until the
// place it came from is: the state dir must be a real directory this uid owns,
// not group/other-writable (and not reached through a symlink), and the file
// itself -- opened with O_NOFOLLOW and judged by fstat on the OPEN descriptor,
// not the path -- a regular file this uid owns, not group/other-writable.
// Any failure is an empty set plus an error: another user who can write the
// directory can never plant an entry that matches a listener.
func Load(path string, now time.Time) (map[int]Entry, error) {
	out := map[int]Entry{}
	if err := cftunnel.ValidateStateDir(filepath.Dir(path)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return out, fmt.Errorf("paused serves ignored: %w", err)
	}
	// O_NONBLOCK so opening a planted FIFO cannot hang; fstat then rejects it.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("paused serves ignored: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return out, fmt.Errorf("paused serves ignored: %w", err)
	}
	if err := cftunnel.ValidateOwnedFile(path, info); err != nil {
		return out, fmt.Errorf("paused serves ignored: %w", err)
	}
	var ff fileFormat
	if err := json.NewDecoder(f).Decode(&ff); err != nil {
		return map[int]Entry{}, fmt.Errorf("paused serves file %s is corrupt (%v); starting with none", path, err)
	}
	for _, e := range ff.Paused {
		if e.Port <= 0 || e.Port > 65535 || e.Exe == "" || e.Cwd == "" || e.Expired(now) {
			continue
		}
		out[e.Port] = e
	}
	return out, nil
}

// Save writes entries atomically: a 0600 temp file in the state dir (created
// O_EXCL|O_NOFOLLOW) renamed over path, so a reader never sees a torn file and
// a symlink planted at path is replaced rather than written through.
func Save(path string, entries map[int]Entry) error {
	dir := filepath.Dir(path)
	if err := cftunnel.EnsureStateDir(dir); err != nil {
		return err
	}
	ff := fileFormat{Version: 1}
	for _, e := range entries {
		ff.Paused = append(ff.Paused, e)
	}
	sort.Slice(ff.Paused, func(i, j int) bool { return ff.Paused[i].Port < ff.Paused[j].Port })
	data, err := json.MarshalIndent(ff, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, FileName+".tmp-*") // 0600, O_EXCL
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(name, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
