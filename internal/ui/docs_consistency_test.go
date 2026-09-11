package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadmeKeybindingsMatchKeymap is the automated doc-drift guard for the one
// structured, deterministic doc surface worth automating: the README's
// keybinding table. It pins that table against the app's canonical key registry
// (keyMap.groups(), the same source the bottom-bar legend and "?" overlay draw
// from), so a key added, renamed, or removed in the app without updating the
// README -- or a README row for a key that no longer exists -- fails in CI
// before merge instead of drifting until roborev or a user notices. See the
// "Docs stay honest" rule in AGENTS.md. (Prose behavioral claims are covered by
// per-claim pinning tests + review, not by parsing prose here.)
func TestReadmeKeybindingsMatchKeymap(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		// Only skip when the source tree genuinely isn't present (an unusual
		// out-of-tree build); in the repo and in CI, README is right there.
		t.Skipf("README.md not found from %s: %v", mustCwd(t), err)
	}
	documented := readmeKeyTable(string(readme))
	if len(documented) == 0 {
		t.Fatal("parsed no keys from the README '| Key | Action |' table -- the table moved or its format changed; update readmeKeyTable")
	}

	// The app's canonical registry: every binding's display key (Help().Key)
	// plus its alias keys (Keys()), flattened from groups().
	displayKeys := map[string]string{} // display key -> its description, for error text
	allAppKeys := map[string]bool{}
	for _, g := range newKeyMap().groups() {
		for _, b := range g.bindings {
			displayKeys[b.Help().Key] = b.Help().Desc
			allAppKeys[b.Help().Key] = true
			for _, k := range b.Keys() {
				allAppKeys[k] = true
			}
		}
	}

	// 1. Every action key the app advertises must be documented in the README.
	for dk, desc := range displayKeys {
		if !documented[dk] {
			t.Errorf("key %q (%q) is in keyMap.groups() but NOT in the README keybinding table -- doc drift; document it or drop the binding", dk, desc)
		}
	}

	// 2. Every key the README documents must be a real binding, OR one of the
	//    two-level navigation keys handled directly in Update (not in the
	//    keyMap struct). A README row for anything else is a removed key or a
	//    typo.
	navKeys := map[string]bool{
		"↑": true, "↓": true, "j": true, "k": true,
		"Shift+↑": true, "Shift+↓": true, "J": true, "K": true,
	}
	for dk := range documented {
		if !allAppKeys[dk] && !navKeys[dk] {
			t.Errorf("README keybinding table documents %q, which is neither a real binding nor a known navigation key -- doc drift (a removed/renamed key, or add it to navKeys if it's genuinely handled in Update)", dk)
		}
	}
}

func mustCwd(t *testing.T) string {
	t.Helper()
	cwd, _ := os.Getwd()
	return cwd
}

// readmeKeyTable extracts the set of backtick-wrapped keys from the first cell
// of the README's "| Key | Action |" markdown table. It reads only that one
// table (bounded by the header and the first non-table line) and pulls keys
// only from the first (Key) column, so backticks elsewhere in the doc -- or in
// the Action prose -- are ignored.
func readmeKeyTable(readme string) map[string]bool {
	keys := map[string]bool{}
	inTable := false
	for _, ln := range strings.Split(readme, "\n") {
		t := strings.TrimSpace(ln)
		if !inTable {
			if strings.HasPrefix(t, "| Key ") && strings.Contains(t, "Action") {
				inTable = true
			}
			continue
		}
		if !strings.HasPrefix(t, "|") {
			break // table ended
		}
		if strings.HasPrefix(t, "| ---") || strings.HasPrefix(t, "|---") {
			continue // header separator row
		}
		// First cell: text between the first and second pipe.
		inner := strings.TrimPrefix(t, "|")
		cell := inner
		if i := strings.Index(inner, "|"); i >= 0 {
			cell = inner[:i]
		}
		for _, k := range backtickTokens(cell) {
			keys[k] = true
		}
	}
	return keys
}

// backtickTokens returns every `-quoted substring in s, in order.
func backtickTokens(s string) []string {
	var out []string
	for {
		i := strings.Index(s, "`")
		if i < 0 {
			break
		}
		rest := s[i+1:]
		j := strings.Index(rest, "`")
		if j < 0 {
			break
		}
		out = append(out, rest[:j])
		s = rest[j+1:]
	}
	return out
}
