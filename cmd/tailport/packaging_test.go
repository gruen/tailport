package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// packagingField returns the first capture group of re over the file at
// path (relative to the repo root), failing the test if it doesn't match.
func packagingField(t *testing.T, path string, re *regexp.Regexp) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", path))
	if err != nil {
		t.Fatal(err)
	}
	m := re.FindSubmatch(b)
	if m == nil {
		t.Fatalf("%s: no match for %v", path, re)
	}
	return string(m[1])
}

// TestPackagingDescriptionsAgree pins the one-line description (kata jhvm):
// the brew formula's desc, both PKGBUILDs' pkgdesc (tailport-bin appends
// " (prebuilt binary)") and both .SRCINFOs must be the same sentence, so a
// future edit can't update one packaging surface and strand the others.
// Homebrew's audit also requires desc to be at most 80 characters and not to
// end with a period.
func TestPackagingDescriptionsAgree(t *testing.T) {
	const binSuffix = " (prebuilt binary)"
	brew := packagingField(t, "packaging/brew/tailport.rb", regexp.MustCompile(`(?m)^\s*desc "([^"]*)"`))
	pkg := regexp.MustCompile(`(?m)^pkgdesc="([^"]*)"`)
	srcinfo := regexp.MustCompile(`(?m)^\s*pkgdesc = (.*)$`)

	got := map[string]string{
		"aur/tailport/PKGBUILD":     packagingField(t, "packaging/aur/tailport/PKGBUILD", pkg),
		"aur/tailport/.SRCINFO":     packagingField(t, "packaging/aur/tailport/.SRCINFO", srcinfo),
		"aur/tailport-bin/PKGBUILD": packagingField(t, "packaging/aur/tailport-bin/PKGBUILD", pkg),
		"aur/tailport-bin/.SRCINFO": packagingField(t, "packaging/aur/tailport-bin/.SRCINFO", srcinfo),
	}
	for name, d := range got {
		if strings.Contains(name, "-bin") {
			if !strings.HasSuffix(d, binSuffix) {
				t.Errorf("%s pkgdesc %q lacks the %q suffix", name, d, binSuffix)
			}
			d = strings.TrimSuffix(d, binSuffix)
		}
		if d != brew {
			t.Errorf("%s description %q != brew desc %q", name, d, brew)
		}
	}

	if n := utf8.RuneCountInString(brew); n > 80 {
		t.Errorf("brew desc is %d chars, Homebrew's audit allows at most 80: %q", n, brew)
	}
	if strings.HasSuffix(brew, ".") {
		t.Errorf("brew desc must not end with a period (brew audit): %q", brew)
	}
}
