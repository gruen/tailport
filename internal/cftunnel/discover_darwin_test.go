//go:build darwin

package cftunnel

import (
	"reflect"
	"testing"
)

// TestSplitPSCommand is the regression test for the MEDIUM roborev carryover
// (kata aprt): a space-form --logfile path containing a space (e.g. a spaced
// $HOME or $XDG_STATE_HOME) must survive splitPSCommand as ONE argv element,
// not be torn apart the way a naive strings.Fields would.
func TestSplitPSCommand(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    []string
	}{
		{
			name:    "no --logfile flag at all -- plain split",
			command: "cloudflared tunnel --url http://localhost:3000",
			want:    []string{"cloudflared", "tunnel", "--url", "http://localhost:3000"},
		},
		{
			name:    "space-free logfile path -- plain split already correct",
			command: "cloudflared tunnel --url http://localhost:3000 --metrics 127.0.0.1:1 --logfile /home/u/.local/state/tailport/cftunnel-3000.log --no-autoupdate",
			want: []string{"cloudflared", "tunnel", "--url", "http://localhost:3000", "--metrics", "127.0.0.1:1",
				"--logfile", "/home/u/.local/state/tailport/cftunnel-3000.log", "--no-autoupdate"},
		},
		{
			name:    "spaced state dir -- logfile value preserved as ONE arg, followed by another flag",
			command: "cloudflared tunnel --url http://localhost:3000 --metrics 127.0.0.1:1 --logfile /Users/Jane Doe/.local/state/tailport/cftunnel-3000.log --no-autoupdate",
			want: []string{"cloudflared", "tunnel", "--url", "http://localhost:3000", "--metrics", "127.0.0.1:1",
				"--logfile", "/Users/Jane Doe/.local/state/tailport/cftunnel-3000.log", "--no-autoupdate"},
		},
		{
			name:    "spaced state dir, logfile is the LAST token (no trailing flag)",
			command: "cloudflared tunnel --url http://localhost:3000 --logfile /Users/Jane Doe/.local/state/tailport/cftunnel-3000.log",
			want: []string{"cloudflared", "tunnel", "--url", "http://localhost:3000",
				"--logfile", "/Users/Jane Doe/.local/state/tailport/cftunnel-3000.log"},
		},
		{
			name:    "named tunnel: spaced logfile with hostname suffix, tunnel name trails after",
			command: "cloudflared tunnel run --url http://localhost:8080 --logfile /Users/Jane Doe/.local/state/tailport/cftunnel-8080-app.example.com.log --no-autoupdate web",
			want: []string{"cloudflared", "tunnel", "run", "--url", "http://localhost:8080",
				"--logfile", "/Users/Jane Doe/.local/state/tailport/cftunnel-8080-app.example.com.log", "--no-autoupdate", "web"},
		},
		{
			name:    "equals-form logfile is untouched by the extraction (no space-form match)",
			command: "cloudflared tunnel --url http://localhost:9000 --logfile=/x/cftunnel-9000.log",
			want:    []string{"cloudflared", "tunnel", "--url", "http://localhost:9000", "--logfile=/x/cftunnel-9000.log"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitPSCommand(tt.command)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("splitPSCommand(%q) =\n  %q\nwant\n  %q", tt.command, got, tt.want)
			}
		})
	}
}

// TestParseRunningSpacedLogfilePath pins the darwin enumerator's output shape
// end-to-end through parseRunning: a spaced state dir must not misclassify an
// OWNED tunnel as foreign (roborev carryover, kata aprt).
func TestParseRunningSpacedLogfilePath(t *testing.T) {
	args := splitPSCommand("cloudflared tunnel --url http://localhost:3000 --metrics 127.0.0.1:20941 --logfile /Users/Jane Doe/.local/state/tailport/cftunnel-3000.log --no-autoupdate")
	r, ok := parseRunning(123, args)
	if !ok {
		t.Fatal("parseRunning should recognize a tunnel invocation")
	}
	if !r.Owned {
		t.Error("a spaced state-dir logfile path must still be recognized as OWNED")
	}
	if r.Port != 3000 {
		t.Errorf("port = %d, want 3000", r.Port)
	}
}
