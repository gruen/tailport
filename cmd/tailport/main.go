// Command tailport is a TUI for toggling tailscale serve (tailnet-only,
// plain HTTP) on and off per locally listening port.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/gruen/tailport/internal/config"
	"github.com/gruen/tailport/internal/ui"
)

// version is the build version, injected at release time via the linker:
// -ldflags "-X main.version=<pkgver>" (see .github/workflows/build.yml and the
// AUR PKGBUILDs). It stays "dev" for plain `go build`/`go run` so an
// unversioned local build is self-evident.
var version = "dev"

// versionLine is the string printed by --version. Kept tiny and pure so it can
// be asserted in a test without standing up the whole TUI.
func versionLine() string {
	return "tailport " + version
}

func main() {
	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.BoolVar(&showVersion, "v", false, "print version and exit (shorthand)")
	flag.Parse()
	if showVersion {
		fmt.Println(versionLine())
		return
	}

	if err := config.WriteDefault(); err != nil {
		fmt.Fprintln(os.Stderr, "tailport: writing default config:", err)
		os.Exit(1)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tailport: loading config:", err)
		os.Exit(1)
	}
	if err := ui.Run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "tailport:", err)
		os.Exit(1)
	}
}
