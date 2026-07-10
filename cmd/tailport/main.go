// Command tailport is a TUI for toggling tailscale serve (tailnet-only,
// plain HTTP) on and off per locally listening port.
package main

import (
	"fmt"
	"os"

	"github.com/gruen/tailport/internal/config"
	"github.com/gruen/tailport/internal/ui"
)

func main() {
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
