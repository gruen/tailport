//go:build darwin

package procid

import (
	"os/exec"
	"strconv"
	"strings"
)

// readExe: `ps -o comm= -p <pid>` prints the full executable path on macOS
// (unlike Linux, where comm is the truncated name); the darwin test proves it
// for real against this very process.
func readExe(pid int) string {
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// readCwd: `lsof -a -p <pid> -d cwd -Fn` emits "p<pid>\nfcwd\nn<path>".
func readCwd(pid int) string {
	out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return ""
	}
	return parseLsofCwd(string(out))
}

func parseLsofCwd(out string) string {
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "n") && len(ln) > 1 {
			return ln[1:]
		}
	}
	return ""
}
