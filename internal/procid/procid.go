// Package procid reads the identity of a running program: its executable path
// and working directory (kata p2r5). tailport remembers the identity of the app
// behind a served port so that, when the app stops and later returns, it can
// tell "the same app" from "some other program that grabbed the port".
//
// Executable alone is not enough: interpreters (node, python3, java) share one
// binary across every app, so the working directory is part of the identity.
// Known limitation, deliberately not special-cased: `go run` builds to a fresh
// temp executable path on every run, so it never matches its previous run.
package procid

import "strings"

// Identity is (executable path, working directory). Either field is "" when it
// could not be read (e.g. another user's process).
type Identity struct {
	Exe string
	Cwd string
}

// Known reports whether both halves were read. An identity that is not Known
// never matches anything, so an unreadable process is never auto-shared.
func (i Identity) Known() bool { return i.Exe != "" && i.Cwd != "" }

// Matches reports whether a and b are the same known program.
func (i Identity) Matches(o Identity) bool { return i.Known() && o.Known() && i == o }

// Of reads the identity of pid. pid <= 0 yields the zero (unknown) Identity.
// It is a var-free wrapper over the platform reader so callers can inject.
func Of(pid int) Identity {
	if pid <= 0 {
		return Identity{}
	}
	return Identity{Exe: StripDeleted(readExe(pid)), Cwd: readCwd(pid)}
}

// StripDeleted removes the " (deleted)" suffix Linux appends to /proc/<pid>/exe
// when the binary was replaced on disk after the process started (e.g. a
// rebuild-in-place), so the same program still compares equal.
func StripDeleted(p string) string { return strings.TrimSuffix(p, " (deleted)") }
