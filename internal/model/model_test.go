// SPDX-License-Identifier: Apache-2.0

package model

import (
	"strings"
	"testing"
)

type ent struct {
	ID   string
	Key  string
	Name string
	Gone bool
}

var spec = Spec[string, ent]{
	Key:       func(e ent) string { return e.ID },
	Cacheable: func(e ent) bool { return !e.Gone },
	Indexes: map[string]func(ent) []any{
		"key": func(e ent) []any {
			if e.Key == "" {
				return nil
			}
			return []any{e.Key}
		},
	},
}

// history: version 1 holds a(k, "v1"); version 2 renames a to "v2"; version 3 moves k
// from a to b.
func history() History[string, ent] {
	return History[string, ent]{
		Base:    []ent{{ID: "a", Key: "k", Name: "v1"}},
		BaseVer: 1,
		Commits: []Commit[string, ent]{
			{Version: 2, Puts: []ent{{ID: "a", Key: "k", Name: "v2"}}},
			{Version: 3, Puts: []ent{{ID: "a", Name: "v2"}, {ID: "b", Key: "k"}}},
		},
	}
}

func check(t *testing.T, reads []Read[string, ent], windows []Window[string]) []string {
	t.Helper()
	h := history()
	h.Reads, h.Windows = reads, windows
	v, _ := Check(spec, h)
	return v
}

func TestCheck_Passes(t *testing.T) {
	reads := []Read[string, ent]{
		{Op: Get, ID: "a", Answer: Found, Value: ent{ID: "a", Key: "k", Name: "v1"}, V0: 1, V1: 1},
		{Op: Get, ID: "a", Answer: Found, Value: ent{ID: "a", Key: "k", Name: "v1"}, V0: 1, V1: 3}, // overlaps the change
		{Op: Get, ID: "zz", Answer: Defer, V0: 1, V1: 3},
		{Op: Lookup, Index: "key", Key: "k", Answer: Found, Holder: "b", V0: 3, V1: 3},
		{Op: Lookup, Index: "key", Key: "none", Answer: Absent, V0: 1, V1: 3},
		{Op: List, Answer: Found, IDs: []string{"a", "b"}, V0: 3, V1: 3},
		{Op: List, Answer: Found, IDs: []string{"a"}, V0: 2, V1: 2},
		{Op: Get, ID: "a", Answer: Found, Value: ent{ID: "a", Key: "k", Name: "v1"}, S0: 1, S1: 10, V0: 1, V1: 1}, // starts before the window
	}
	windows := []Window[string]{{IDs: []string{"a"}, S0: 5, S1: 20}}
	if v := check(t, reads, windows); len(v) != 0 {
		t.Fatalf("violations: %v", v)
	}
}

func TestCheck_Fails(t *testing.T) {
	cases := []struct {
		name   string
		read   Read[string, ent]
		window *Window[string]
		want   string
	}{
		{"stale Get", Read[string, ent]{Op: Get, ID: "a", Answer: Found, Value: ent{ID: "a", Key: "k", Name: "v1"}, V0: 2, V1: 3}, nil, "matches no committed state"},
		{"Get of a non-cacheable value", Read[string, ent]{Op: Get, ID: "a", Answer: Found, Value: ent{ID: "a", Gone: true}, V0: 1, V1: 3}, nil, "matches no committed state"},
		{"wrong Absent", Read[string, ent]{Op: Lookup, Index: "key", Key: "k", Answer: Absent, V0: 1, V1: 3}, nil, "matches no committed state"},
		{"stale holder", Read[string, ent]{Op: Lookup, Index: "key", Key: "k", Answer: Found, Holder: "a", V0: 3, V1: 3}, nil, "matches no committed state"},
		{"stale List", Read[string, ent]{Op: List, Answer: Found, IDs: []string{"a"}, V0: 3, V1: 3}, nil, "matches no committed state"},
		{
			"Get hit inside a mutation window",
			Read[string, ent]{Op: Get, ID: "a", Answer: Found, Value: ent{ID: "a", Key: "k", Name: "v1"}, V0: 1, V1: 1, S0: 6, S1: 7},
			&Window[string]{IDs: []string{"a"}, S0: 5, S1: 8}, "inside mutation window",
		},
		{
			"Lookup hit on an affected holder inside a window",
			Read[string, ent]{Op: Lookup, Index: "key", Key: "k", Answer: Found, Holder: "a", V0: 1, V1: 1, S0: 6, S1: 7},
			&Window[string]{IDs: []string{"a", "b"}, S0: 5, S1: 8}, "inside mutation window",
		},
		{
			"Absent during an uncommitted create",
			Read[string, ent]{Op: Lookup, Index: "key", Key: "new", Answer: Absent, V0: 3, V1: 3, S0: 6, S1: 7},
			&Window[string]{Create: true, S0: 5, S1: 8}, "inside mutation window",
		},
		{
			"List inside any window",
			Read[string, ent]{Op: List, Answer: Found, IDs: []string{"a", "b"}, V0: 3, V1: 3, S0: 6, S1: 7},
			&Window[string]{IDs: []string{"zz"}, S0: 5, S1: 8}, "inside mutation window",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var windows []Window[string]
			if tc.window != nil {
				windows = append(windows, *tc.window)
			}
			v := check(t, []Read[string, ent]{tc.read}, windows)
			if len(v) != 1 || !strings.Contains(v[0], tc.want) {
				t.Fatalf("violations = %v, want one containing %q", v, tc.want)
			}
		})
	}
}

// An unaffected Get hit inside a window is allowed.
func TestCheck_UnaffectedHitInsideWindow(t *testing.T) {
	r := Read[string, ent]{Op: Get, ID: "a", Answer: Found, Value: ent{ID: "a", Key: "k", Name: "v1"}, V0: 1, V1: 1, S0: 6, S1: 7}
	if v := check(t, []Read[string, ent]{r}, []Window[string]{{IDs: []string{"b"}, S0: 5, S1: 8}}); len(v) != 0 {
		t.Fatalf("violations = %v", v)
	}
}

func TestCheck_Stats(t *testing.T) {
	h := history()
	h.Reads = []Read[string, ent]{
		{Op: Get, ID: "a", Answer: Defer},
		{Op: List, Answer: Found, IDs: []string{"a"}, V0: 1, V1: 1},
	}
	_, st := Check(spec, h)
	if st != (Stats{Reads: 2, Deferred: 1, Answered: 1}) {
		t.Fatalf("stats = %+v", st)
	}
}

// Versions past the last commit read the latest state.
func TestCheck_VersionPastLog(t *testing.T) {
	r := Read[string, ent]{Op: List, Answer: Found, IDs: []string{"a", "b"}, V0: 9, V1: 9}
	if v := check(t, []Read[string, ent]{r}, nil); len(v) != 0 {
		t.Fatalf("violations = %v", v)
	}
}

// A commit log with a gap carries the previous state across it.
func TestReplay_FillsGaps(t *testing.T) {
	h := History[string, ent]{
		BaseVer: 1,
		Commits: []Commit[string, ent]{{Version: 4, Puts: []ent{{ID: "a"}}}},
	}
	h.Reads = []Read[string, ent]{
		{Op: List, Answer: Found, IDs: nil, V0: 2, V1: 3},
		{Op: Get, ID: "a", Answer: Found, Value: ent{ID: "a"}, V0: 4, V1: 4},
	}
	if v, _ := Check(spec, h); len(v) != 0 {
		t.Fatalf("violations = %v", v)
	}
}

func TestDescribe(t *testing.T) {
	if describe(Read[string, ent]{Op: Op(9)}) != "unknown op" {
		t.Fatal("unknown op")
	}
	if mustDefer(Read[string, ent]{Op: Op(9)}, Window[string]{}) {
		t.Fatal("unknown op must not require deferral")
	}
	if matches(spec, nil, Read[string, ent]{Op: Op(9)}) {
		t.Fatal("unknown op must not match")
	}
}
