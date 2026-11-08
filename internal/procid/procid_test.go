package procid

import "testing"

func TestStripDeleted(t *testing.T) {
	for in, want := range map[string]string{
		"/usr/bin/python3.12":           "/usr/bin/python3.12",
		"/usr/bin/python3.12 (deleted)": "/usr/bin/python3.12",
		"/opt/my (deleted)app/bin":      "/opt/my (deleted)app/bin", // only a TRAILING suffix
		"":                              "",
	} {
		if got := StripDeleted(in); got != want {
			t.Errorf("StripDeleted(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIdentityMatching(t *testing.T) {
	a := Identity{Exe: "/usr/bin/node", Cwd: "/srv/a"}
	for name, tc := range map[string]struct {
		other Identity
		want  bool
	}{
		"same":           {Identity{Exe: "/usr/bin/node", Cwd: "/srv/a"}, true},
		"other cwd":      {Identity{Exe: "/usr/bin/node", Cwd: "/srv/b"}, false},
		"other exe":      {Identity{Exe: "/usr/bin/python3", Cwd: "/srv/a"}, false},
		"unknown cwd":    {Identity{Exe: "/usr/bin/node"}, false},
		"unknown exe":    {Identity{Cwd: "/srv/a"}, false},
		"fully unknown":  {Identity{}, false},
		"deleted-suffix": {Identity{Exe: StripDeleted("/usr/bin/node (deleted)"), Cwd: "/srv/a"}, true},
	} {
		if got := a.Matches(tc.other); got != tc.want {
			t.Errorf("%s: Matches = %v, want %v", name, got, tc.want)
		}
	}
	if (Identity{}).Matches(Identity{}) {
		t.Error("two unknown identities must never match")
	}
	if Of(0).Known() || Of(-1).Known() {
		t.Error("a non-positive pid is unknown")
	}
}
