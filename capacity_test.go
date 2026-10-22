// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"errors"
	"testing"
)

// SAFE-8, NFR-1: a refresh that would grow a map past capacity marks stale, the mutation
// still succeeds, every mark is released and the maps are untouched.
func TestRefresh_OverflowMarksStale(t *testing.T) {
	cases := []struct {
		name   string
		cap    fixtureOpt
		before []group
		ids    []string
		after  []group
	}{
		{"primary", withCapacity(1, 10, 10), []group{g("a", "o1")}, []string{"b"}, []group{g("b", "")}},
		{"member", withCapacity(10, 1, 10), []group{g("a", "", "m1")}, []string{"a"}, []group{g("a", "", "m1", "m2")}},
		{"owner", withCapacity(10, 10, 1), []group{g("a", "o1")}, []string{"b"}, []group{g("b", "o2")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.cap)
			f.install(tc.before...)
			if err := f.mutate(t.Context(), tc.ids, set(tc.after...)); err != nil {
				t.Fatalf("Mutate = %v", err)
			}
			if !f.isStale() || !errors.Is(f.staleCause(), ErrOverCapacity) {
				t.Fatalf("stale=%t cause=%v", f.isStale(), f.staleCause())
			}
			if p, n, w := f.marks(); p+n+w != 0 {
				t.Fatalf("marks not released: %d %d %d", p, n, w)
			}
			f.c.mu.RLock()
			defer f.c.mu.RUnlock()
			if f.c.entities.Len() != 1 || len(f.c.directory) != 1 {
				t.Fatalf("primary changed: %d entities", f.c.entities.Len())
			}
			a, _ := f.c.entities.Get("a")
			if a.Owner != tc.before[0].Owner || len(a.Members) != len(tc.before[0].Members) {
				t.Fatalf("a changed: %v", a)
			}
			total := 0
			for _, ix := range f.c.indexes {
				total += ix.size()
			}
			if total != 1 {
				t.Fatalf("index keys = %d, want 1", total)
			}
		})
	}
}

// NFR-1: replacing key A with key B in an index at capacity succeeds.
func TestIndexCapacity_ReplaceAtCapacitySucceeds(t *testing.T) {
	f := newFixture(t, withCapacity(1, 1, 1))
	f.install(g("a", "o1", "A"))
	if err := f.mutate(t.Context(), []string{"a"}, set(g("a", "o2", "B"))); err != nil {
		t.Fatal(err)
	}
	if f.isStale() {
		t.Fatalf("stale: %v", f.staleCause())
	}
	if id, a := f.member("B"); a != Found || id != "a" {
		t.Fatalf("B = %q, %v", id, a)
	}
	if _, a := f.member("A"); a != Absent {
		t.Fatalf("A = %v", a)
	}
	// Re-installing the same keys at capacity is not growth either.
	if err := f.mutate(t.Context(), []string{"a"}, set(g("a", "o2", "B", "B"))); err != nil || f.isStale() {
		t.Fatalf("err=%v stale=%t", err, f.isStale())
	}
}

// NFR-1: true growth past an index's capacity is refused.
func TestIndexCapacity_GrowthPastCapacityRefused(t *testing.T) {
	f := newFixture(t, withCapacity(10, 2, 10))
	f.install(g("a", "", "A"))
	if err := f.mutate(t.Context(), []string{"a"}, set(g("a", "", "A", "B", "C"))); err != nil {
		t.Fatal(err)
	}
	if !f.isStale() || !errors.Is(f.staleCause(), ErrOverCapacity) {
		t.Fatalf("stale=%t cause=%v", f.isStale(), f.staleCause())
	}
}
