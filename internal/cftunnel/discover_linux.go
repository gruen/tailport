//go:build linux

package cftunnel

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// enumerateCloudflared finds running cloudflared processes by walking /proc and
// reading each PID's NUL-separated /proc/<pid>/cmdline. It matches on the
// executable basename (argv[0] == "cloudflared"), catching both PATH-launched
// and absolute-path launches. Unreadable entries -- permission denied for
// another user's process, or a PID that exits mid-scan -- are silently skipped.
//
// Note: a cloudflared installed under a DIFFERENT binary name (config
// Binary override) won't be enumerated here; that's an accepted v1 limitation,
// since the common case is the stock `cloudflared` name.
func enumerateCloudflared() ([]procInfo, error) {
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
		if len(args) == 0 || !isCloudflaredArgv0(args[0]) {
			continue
		}
		out = append(out, procInfo{pid: pid, args: args})
	}
	return out, nil
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
