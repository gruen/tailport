// Package clip copies text to the system clipboard with two mechanisms, in
// keeping with tailport's zero-required-dependency posture:
//
//   - OSC 52 (primary): a terminal escape sequence, pure Go with no external
//     binary. Crucially it sets the clipboard of the user's LOCAL terminal
//     even when tailport is running over SSH on a fleet host -- the common
//     case for this tool.
//   - A local clipboard helper (best-effort): pbcopy on macOS, or wl-copy /
//     xclip / xsel on Linux, used ONLY when present. It is never required;
//     absence is silently ignored.
//
// OSC 52 is fire-and-forget -- terminals give no success signal and some don't
// support it -- so callers should phrase their confirmation as "copied"
// without claiming certainty.
package clip

import (
	"encoding/base64"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// OSC52 returns the OSC 52 escape sequence that sets the terminal clipboard to
// s. It is pure and side-effect-free so it can be unit-tested; Copy is what
// actually emits it.
func OSC52(s string) string {
	enc := base64.StdEncoding.EncodeToString([]byte(s))
	// ESC ] 52 ; c ; <base64> BEL -- selection "c" is the clipboard.
	return "\x1b]52;c;" + enc + "\a"
}

// Copy sets the clipboard to s on a best-effort basis: it always emits OSC 52
// to the controlling terminal, and additionally pipes s to a local clipboard
// helper when one is on PATH. It never returns an error -- OSC 52 can't be
// confirmed and the helper is optional -- so the UI pairs it with a toast.
func Copy(s string) {
	writeOSC52(s)
	if name, args := helper(); name != "" {
		pipeTo(name, args, s) // best-effort; errors/absence ignored
	}
}

// writeOSC52 sends the sequence to the real terminal (/dev/tty) so it isn't
// swallowed by an output redirect or interleaved into the TUI's own stdout
// frame; it falls back to stdout if /dev/tty can't be opened.
func writeOSC52(s string) {
	seq := OSC52(s)
	if f, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
		f.WriteString(seq)
		f.Close()
		return
	}
	os.Stdout.WriteString(seq)
}

// helper returns a local clipboard command (name + args) if one is available,
// or "" when none is. macOS: pbcopy. Linux: wl-copy (Wayland), then xclip /
// xsel (X11), in that order of preference.
func helper() (string, []string) {
	type cand struct {
		name string
		args []string
	}
	var cands []cand
	if runtime.GOOS == "darwin" {
		cands = []cand{{"pbcopy", nil}}
	} else {
		cands = []cand{
			{"wl-copy", nil},
			{"xclip", []string{"-selection", "clipboard"}},
			{"xsel", []string{"--clipboard", "--input"}},
		}
	}
	for _, c := range cands {
		if _, err := exec.LookPath(c.name); err == nil {
			return c.name, c.args
		}
	}
	return "", nil
}

func pipeTo(name string, args []string, s string) {
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(s)
	cmd.Run() // best-effort
}
