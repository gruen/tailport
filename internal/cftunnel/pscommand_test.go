package cftunnel

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// TestSplitPSCommand is the regression test for the MEDIUM roborev carryover
// (kata aprt): a space-form --logfile path containing a space (e.g. a spaced
// $HOME or $XDG_STATE_HOME) must survive splitPSCommand as ONE argv element,
// not be torn apart the way a naive strings.Fields would. Moved here (out of
// discover_darwin_test.go, which carried a darwin build tag) so this pure
// parsing logic is compiled and run on Linux CI too, not only indirectly on a
// darwin runner (kata nc1j).
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
			name:    "named tunnel: tunnel-level flags (spaced logfile) before run, name trails after --url",
			command: "cloudflared tunnel --metrics 127.0.0.1:1 --logfile /Users/Jane Doe/.local/state/tailport/cftunnel-8080-app.example.com.log --no-autoupdate run --url http://localhost:8080 web",
			want: []string{"cloudflared", "tunnel", "--metrics", "127.0.0.1:1",
				"--logfile", "/Users/Jane Doe/.local/state/tailport/cftunnel-8080-app.example.com.log", "--no-autoupdate",
				"run", "--url", "http://localhost:8080", "web"},
		},
		{
			name:    "equals-form logfile is untouched by the extraction (no space-form match)",
			command: "cloudflared tunnel --url http://localhost:9000 --logfile=/x/cftunnel-9000.log",
			want:    []string{"cloudflared", "tunnel", "--url", "http://localhost:9000", "--logfile=/x/cftunnel-9000.log"},
		},
		{
			// kata nc1j (hermetic --config fix): a spaced --config value,
			// immediately followed by --url, must survive as ONE arg too.
			name:    "spaced state dir -- config value preserved as ONE arg, followed by --url",
			command: "cloudflared tunnel --config /Users/Jane Doe/.local/state/tailport/cftunnel-config.yml --url http://localhost:3000 --metrics 127.0.0.1:1 --logfile /home/u/.local/state/tailport/cftunnel-3000.log --no-autoupdate",
			want: []string{"cloudflared", "tunnel", "--config", "/Users/Jane Doe/.local/state/tailport/cftunnel-config.yml",
				"--url", "http://localhost:3000", "--metrics", "127.0.0.1:1",
				"--logfile", "/home/u/.local/state/tailport/cftunnel-3000.log", "--no-autoupdate"},
		},
		{
			// Both --config AND --logfile spaced in the SAME command line --
			// each must be protected independently without corrupting the
			// other (named order: --config precedes --metrics here).
			name:    "named tunnel: BOTH --config and --logfile spaced in the same line",
			command: "cloudflared tunnel --config /Users/Jane Doe/.local/state/tailport/cftunnel-config.yml --metrics 127.0.0.1:1 --logfile /Users/Jane Doe/.local/state/tailport/cftunnel-8080-app.example.com.log --no-autoupdate run --url http://localhost:8080 web",
			want: []string{"cloudflared", "tunnel", "--config", "/Users/Jane Doe/.local/state/tailport/cftunnel-config.yml",
				"--metrics", "127.0.0.1:1",
				"--logfile", "/Users/Jane Doe/.local/state/tailport/cftunnel-8080-app.example.com.log", "--no-autoupdate",
				"run", "--url", "http://localhost:8080", "web"},
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
// OWNED tunnel as foreign (roborev carryover, kata aprt). Moved here alongside
// TestSplitPSCommand for the same reason: no build tag, so it runs on Linux
// CI too.
func TestParseRunningSpacedLogfilePath(t *testing.T) {
	args := splitPSCommand("cloudflared tunnel --url http://localhost:3000 --metrics 127.0.0.1:20941 --logfile /Users/Jane Doe/.local/state/tailport/cftunnel-3000.log --no-autoupdate")
	r, ok := parseRunning(123, os.Getuid(), args)
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

// TestSplitPSCommandRoundTripsBuildArgs pins that splitPSCommand's parsing
// survives a full round trip through buildArgs's OWN argv shape -- quick and
// named, with plain, --config-only-spaced, --logfile-only-spaced, and
// BOTH-spaced state-dir paths (kata nc1j's hermetic --config fix put a
// SECOND space-containing value on the line) -- so a future reshuffle of
// buildArgs's flag order can't silently break macOS's ps-based discovery
// without failing here first.
func TestSplitPSCommandRoundTripsBuildArgs(t *testing.T) {
	const (
		unspacedDir = "/home/u/.local/state/tailport"
		spacedDir   = "/Users/Jane Doe/.local/state/tailport"
	)
	cases := []struct {
		name       string
		spec       Spec
		logfile    string
		configPath string
	}{
		{
			name:       "quick, unspaced config and logfile",
			spec:       Spec{Port: 3000, Mode: ModeQuick, MetricsPort: 20941},
			logfile:    unspacedDir + "/cftunnel-3000.log",
			configPath: unspacedDir + "/cftunnel-config.yml",
		},
		{
			name:       "quick, spaced config only",
			spec:       Spec{Port: 3000, Mode: ModeQuick, MetricsPort: 20941},
			logfile:    unspacedDir + "/cftunnel-3000.log",
			configPath: spacedDir + "/cftunnel-config.yml",
		},
		{
			name:       "quick, spaced logfile only",
			spec:       Spec{Port: 3000, Mode: ModeQuick, MetricsPort: 20941},
			logfile:    spacedDir + "/cftunnel-3000.log",
			configPath: unspacedDir + "/cftunnel-config.yml",
		},
		{
			name:       "quick, BOTH config and logfile spaced",
			spec:       Spec{Port: 3000, Mode: ModeQuick, MetricsPort: 20941},
			logfile:    spacedDir + "/cftunnel-3000.log",
			configPath: spacedDir + "/cftunnel-config.yml",
		},
		{
			name:       "named, unspaced config and logfile",
			spec:       Spec{Port: 8080, Mode: ModeNamed, TunnelName: "web", MetricsPort: 20942},
			logfile:    unspacedDir + "/cftunnel-8080-app.example.com.log",
			configPath: unspacedDir + "/cftunnel-config.yml",
		},
		{
			name:       "named, BOTH config and logfile spaced",
			spec:       Spec{Port: 8080, Mode: ModeNamed, TunnelName: "web", MetricsPort: 20942},
			logfile:    spacedDir + "/cftunnel-8080-app.example.com.log",
			configPath: spacedDir + "/cftunnel-config.yml",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Simulate how this argv would be reconstructed by macOS's
			// `ps -o command=` -- argv[0] (the binary name) followed by the
			// space-joined arguments, exactly as splitPSCommand's caller
			// (enumerateCloudflared) hands it a single space-joined string.
			want := append([]string{"cloudflared"}, buildArgs(tc.spec, tc.logfile, tc.configPath)...)
			command := strings.Join(want, " ")
			got := splitPSCommand(command)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("splitPSCommand round trip:\n got %q\nwant %q", got, want)
			}
		})
	}
}

// TestSplitPidUidCommand pins splitPidUidCommand's parsing of a
// `ps -axww -o pid=,uid=,command=` line (S2(a), audit finding 2): ps
// right-pads/aligns the two numeric columns with spaces, and this must
// tolerate that padding on either column independently.
func TestSplitPidUidCommand(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		wantPid     int
		wantUID     int
		wantCommand string
		wantOK      bool
	}{
		{
			name:        "single-space separated",
			line:        "1234 501 /usr/bin/cloudflared tunnel --url http://localhost:3000",
			wantPid:     1234,
			wantUID:     501,
			wantCommand: "/usr/bin/cloudflared tunnel --url http://localhost:3000",
			wantOK:      true,
		},
		{
			name:        "ps-style right-aligned padding on both columns",
			line:        "   1234    0 /usr/bin/cloudflared tunnel --url http://localhost:3000",
			wantPid:     1234,
			wantUID:     0,
			wantCommand: "/usr/bin/cloudflared tunnel --url http://localhost:3000",
			wantOK:      true,
		},
		{
			name:   "no uid or command column",
			line:   "1234",
			wantOK: false,
		},
		{
			name:   "no command column",
			line:   "1234 501",
			wantOK: false,
		},
		{
			name:   "non-numeric pid",
			line:   "abc 501 command",
			wantOK: false,
		},
		{
			name:   "non-numeric uid",
			line:   "1234 abc command",
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pid, uid, command, ok := splitPidUidCommand(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if pid != tt.wantPid || uid != tt.wantUID || command != tt.wantCommand {
				t.Errorf("got (%d, %d, %q), want (%d, %d, %q)", pid, uid, command, tt.wantPid, tt.wantUID, tt.wantCommand)
			}
		})
	}
}
