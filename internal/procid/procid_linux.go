//go:build linux

package procid

import (
	"os"
	"strconv"
)

func readExe(pid int) string {
	p, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return ""
	}
	return p
}

func readCwd(pid int) string {
	p, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
	if err != nil {
		return ""
	}
	return p
}
