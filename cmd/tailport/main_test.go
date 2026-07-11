package main

import (
	"strings"
	"testing"
)

// TestVersionLine covers the --version output (jtpx). The default build stamps
// "dev"; a release stamps the pkgver via -ldflags -X main.version, and whatever
// that value is must appear verbatim in the line so `tailport --version`
// reports the packaged version.
func TestVersionLine(t *testing.T) {
	if got := versionLine(); got != "tailport "+version {
		t.Errorf("versionLine() = %q, want %q", got, "tailport "+version)
	}
	if !strings.HasPrefix(versionLine(), "tailport ") {
		t.Errorf("versionLine() = %q, want it to start with 'tailport '", versionLine())
	}

	// Simulate a release-time injection: whatever main.version is set to must
	// surface in the printed line.
	orig := version
	t.Cleanup(func() { version = orig })
	version = "0.1.0"
	if got := versionLine(); got != "tailport 0.1.0" {
		t.Errorf("with injected version, versionLine() = %q, want %q", got, "tailport 0.1.0")
	}
}
