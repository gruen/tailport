// Package portscan enumerates locally listening TCP ports.
package portscan

// Port is a locally listening TCP port, optionally attributed to a process.
type Port struct {
	Number  int
	Process string // best-effort; empty if not resolvable (e.g. owned by another user)
}
