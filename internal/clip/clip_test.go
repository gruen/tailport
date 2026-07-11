package clip

import (
	"encoding/base64"
	"strings"
	"testing"
)

// TestOSC52 covers the escape-sequence builder: ESC ] 52 ; c ; <base64> BEL,
// with the payload being the base64 of the input.
func TestOSC52(t *testing.T) {
	got := OSC52("http://host:8080")
	if !strings.HasPrefix(got, "\x1b]52;c;") {
		t.Errorf("OSC52 should start with the OSC 52 introducer; got %q", got)
	}
	if !strings.HasSuffix(got, "\a") {
		t.Errorf("OSC52 should end with BEL; got %q", got)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(got, "\x1b]52;c;"), "\a")
	dec, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("payload is not valid base64: %v", err)
	}
	if string(dec) != "http://host:8080" {
		t.Errorf("decoded payload = %q, want the original string", string(dec))
	}
}

func TestOSC52Empty(t *testing.T) {
	if got := OSC52(""); got != "\x1b]52;c;\a" {
		t.Errorf("OSC52(\"\") = %q", got)
	}
}
