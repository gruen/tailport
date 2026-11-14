package portscan

import (
	"reflect"
	"testing"
)

// TestResolveTailnetRows pins the pure resolver for the unattributed
// tailnet-IP rows the scanners flag (kata nvv6): served -> tailscaled's proxy
// (dropped, and the whole port if those were its only rows); unserved -> an
// app's own tailnet bind (folded in); serve state unknown -> dropped, exactly
// as before nvv6.
func TestResolveTailnetRows(t *testing.T) {
	only := Port{Number: 18951, UnattribTailnet: true, UnattribHost: "100.81.94.92"}
	withLoop := Port{Number: 18951, Process: "app", Pid: 7, Pid127: 7, BindScope: ScopeLoopback, BindHost: "127.0.0.1", Loopback: true,
		UnattribTailnet: true, UnattribHost: "100.81.94.92"}
	withWild := Port{Number: 18951, BindScope: ScopeWildcard, BindHost: "0.0.0.0",
		UnattribTailnet: true, UnattribHost: "100.81.94.92"}
	plain := Port{Number: 22, Process: "sshd", BindScope: ScopeWildcard}

	tests := []struct {
		name   string
		in     []Port
		served map[int]bool
		known  bool
		want   []Port
	}{
		{"served only-rows port is dropped (dangling stays not-listening)",
			[]Port{only}, map[int]bool{18951: true}, true, []Port{}},
		{"unknown serve state drops it (as before nvv6)",
			[]Port{only}, nil, false, []Port{}},
		{"unserved only-rows port is an app tailnet bind",
			[]Port{only}, map[int]bool{}, true,
			[]Port{{Number: 18951, BindScope: ScopeTailnet, BindHost: "100.81.94.92", No127: true}}},
		{"served mixed with loopback keeps the loopback aggregation untouched",
			[]Port{withLoop}, map[int]bool{18951: true}, true,
			[]Port{{Number: 18951, Process: "app", Pid: 7, Pid127: 7, BindScope: ScopeLoopback, BindHost: "127.0.0.1", Loopback: true}}},
		{"unknown state on mixed port: same as served",
			[]Port{withLoop}, nil, false,
			[]Port{{Number: 18951, Process: "app", Pid: 7, Pid127: 7, BindScope: ScopeLoopback, BindHost: "127.0.0.1", Loopback: true}}},
		{"unserved mixed with loopback widens to tailnet, keeps Loopback and Pid127",
			[]Port{withLoop}, map[int]bool{}, true,
			[]Port{{Number: 18951, Process: "app", Pid: 7, Pid127: 7, BindScope: ScopeTailnet, BindHost: "100.81.94.92", Loopback: true}}},
		{"unserved wildcard stays wildcard (widest wins), host untouched",
			[]Port{withWild}, map[int]bool{}, true,
			[]Port{{Number: 18951, BindScope: ScopeWildcard, BindHost: "0.0.0.0"}}},
		{"unflagged ports pass through and order is kept",
			[]Port{plain, only, {Number: 3000, BindScope: ScopeLoopback}}, map[int]bool{18951: true}, true,
			[]Port{plain, {Number: 3000, BindScope: ScopeLoopback}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := append([]Port(nil), tt.in...)
			got := ResolveTailnetRows(in, tt.served, tt.known)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
			if !reflect.DeepEqual(in, tt.in) {
				t.Errorf("input was modified: %+v", in)
			}
		})
	}
}

func TestIsTailscaleLsofCommand(t *testing.T) {
	for cmd, want := range map[string]bool{
		"tailscale":  true, // tailscaled, lsof's default 9-char truncation
		"tailscaled": true, "Tailscale": true,
		"IPNExtens": true, "IPNExtension": true, "io.tailsc": true,
		"python3": false, "node": false, "tail": false, "": false,
	} {
		if got := isTailscaleLsofCommand(cmd); got != want {
			t.Errorf("isTailscaleLsofCommand(%q) = %v, want %v", cmd, got, want)
		}
	}
}
