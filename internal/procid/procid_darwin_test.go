//go:build darwin

package procid

import "testing"

func TestParseLsofCwd(t *testing.T) {
	if got := parseLsofCwd("p123\nfcwd\nn/Users/x/my app\n"); got != "/Users/x/my app" {
		t.Errorf("parseLsofCwd = %q", got)
	}
	if got := parseLsofCwd("p123\n"); got != "" {
		t.Errorf("no n line should be unknown, got %q", got)
	}
}
