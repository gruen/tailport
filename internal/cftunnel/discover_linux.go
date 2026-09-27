//go:build linux

package cftunnel

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// enumerateCloudflared finds running cloudflared processes by walking /proc and
// reading each PID's NUL-separated /proc/<pid>/cmdline. It matches on the
// executable basename (argv[0] against binName -- the configured Client.binary(),
// "cloudflared" by default), catching both PATH-launched and absolute-path
// launches, AND a configured Binary override (roborev carryover, kata aprt).
// Unreadable entries -- permission denied for another user's process, or a PID
// that exits mid-scan -- are silently skipped.
//
// Each entry also carries the process's owner UID (S2(a), audit finding 2),
// read via os.Stat on the /proc/<pid> directory itself (its owner is set by
// the kernel to the process's EFFECTIVE uid, not its real uid, though the two
// only ever differ for a setuid binary) rather than any file inside it -- so
// it works regardless of what the process's cmdline permissions happen to
// be. A PID whose UID can't be read (raced past exit) is skipped, same as an
// unreadable cmdline.
func enumerateCloudflared(binName string) ([]procInfo, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []procInfo
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a /proc/<pid> dir
		}
		data, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil || len(data) == 0 {
			continue
		}
		args := splitNUL(data)
		if len(args) == 0 || !isCloudflaredArgv0(args[0], binName) {
			continue
		}
		uid, err := procUID(pid)
		if err != nil {
			continue
		}
		out = append(out, procInfo{pid: pid, uid: uid, args: args})
	}
	return out, nil
}

// procUID reads pid's owner UID via os.Stat on its /proc/<pid> directory,
// which the kernel always sets to the process's own EFFECTIVE UID (not its
// real UID, though the two coincide except for a setuid binary).
func procUID(pid int) (int, error) {
	info, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid)))
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, os.ErrInvalid
	}
	return int(st.Uid), nil
}

// splitNUL splits /proc/<pid>/cmdline (argv joined and terminated by NUL bytes)
// into its argument vector, dropping the trailing empty element the terminating
// NUL produces.
func splitNUL(data []byte) []string {
	raw := strings.Split(string(data), "\x00")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
