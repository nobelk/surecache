// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"errors"
	"slices"
	"testing"
)

// FUN-1: a trusted cache answers a primary hit from memory.
func TestGet_TrustedHit(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", "o", "m"))
	e, a := f.get("a")
	if a != Found || e.ID != "a" || e.Owner != "o" {
		t.Fatalf("Get = %v, %v", e, a)
	}
	if got := f.obs.take(); !slices.Equal(got, []string{"read get cache"}) {
		t.Fatalf("events = %v", got)
	}
}

// SAFE-3, internal invariant 9: a primary miss always defers, because non-cacheable
// entities exist in the store but not in the cache.
func TestGet_MissAlwaysDefers(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", ""), group{ID: "hidden", Deleted: true})
	for _, id := range []string{"hidden", "never"} {
		if e, a := f.get(id); a != Defer || e.ID != "" {
			t.Fatalf("Get(%s) = %v, %v", id, e, a)
		}
	}
	if got := f.obs.take(); !slices.Equal(got, []string{"read get miss", "read get miss"}) {
		t.Fatalf("events = %v", got)
	}
}

// SAFE-2: a read of an entity with a mutation in flight defers.
func TestGet_DefersWhilePending(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", ""), g("b", ""))
	f.c.pending["a"] = 1
	if _, a := f.get("a"); a != Defer {
		t.Fatalf("pending Get = %v", a)
	}
	if _, a := f.get("b"); a != Found {
		t.Fatalf("unaffected Get = %v", a)
	}
	if got := f.obs.take(); !slices.Equal(got, []string{"read get pending", "read get cache"}) {
		t.Fatalf("events = %v", got)
	}
}

// FUN-2, SAFE-9: a stale cache defers every read.
func TestReads_StaleDefersAll(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", "o", "m"))
	f.c.mu.Lock()
	f.c.markStaleLocked(t.Context(), errors.New("test"))
	f.c.mu.Unlock()
	assertAllDefer(t, f, "stale")
}

// Section 8: an unhealthy cache defers every read even when not stale.
func TestReads_UnhealthyDefersAll(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", "o", "m"))
	f.c.unhealthy = true
	assertAllDefer(t, f, "stale")
}

// SAFE-9: a fresh cache is stale and defers everything.
func TestCache_StaleBeforePrime(t *testing.T) {
	f := newFixture(t)
	f.store.Seed(g("a", "o", "m"))
	assertAllDefer(t, f, "stale")
}

func assertAllDefer(t *testing.T, f *fixture, source string) {
	t.Helper()
	f.obs.take()
	if _, a := f.get("a"); a != Defer {
		t.Errorf("Get = %v", a)
	}
	if _, a := f.member("m"); a != Defer {
		t.Errorf("Lookup hit = %v", a)
	}
	if _, a := f.member("none"); a != Defer {
		t.Errorf("Lookup miss = %v", a)
	}
	if ids, a := f.c.List(); a != Defer || ids != nil {
		t.Errorf("List = %v, %v", ids, a)
	}
	want := []string{"read get " + source, "read lookup " + source, "read lookup " + source, "read list " + source}
	if got := f.obs.take(); !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

// FUN-1: a trusted index hit answers the holder.
func TestLookup_TrustedHit(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", "o", "m"))
	if id, a := f.member("m"); a != Found || id != "a" {
		t.Fatalf("Lookup = %q, %v", id, a)
	}
	if got := f.obs.take(); !slices.Equal(got, []string{"read lookup cache"}) {
		t.Fatalf("events = %v", got)
	}
}

// SAFE-3, internal invariant 9: Absent only while trusted and idle.
func TestLookup_AbsentOnlyWhenTrustedAndIdle(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", "o", "m"))
	if id, a := f.member("other"); a != Absent || id != "" {
		t.Fatalf("idle miss = %q, %v", id, a)
	}
	if got := f.obs.take(); !slices.Equal(got, []string{"read lookup cache"}) {
		t.Fatalf("an authoritative miss reports SourceCache: %v", got)
	}
	f.c.inFlight = 1
	if _, a := f.member("other"); a != Defer {
		t.Fatalf("miss in flight = %v", a)
	}
}

// SAFE-2: an index miss defers while any mutation is in flight; a create's id is unknown.
func TestLookup_MissDefersWhileInFlight(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", "o", "m"))
	f.c.inFlight = 1
	if id, a := f.member("new"); a != Defer || id != "" {
		t.Fatalf("Lookup = %q, %v", id, a)
	}
	if id, a := f.member("m"); a != Found || id != "a" {
		t.Fatalf("a hit on an entity that is not pending answers: %q, %v", id, a)
	}
	if got := f.obs.take(); !slices.Equal(got, []string{"read lookup in_flight", "read lookup cache"}) {
		t.Fatalf("events = %v", got)
	}
}

// SAFE-2: an index hit on a pending entity defers.
func TestLookup_HitDefersWhilePending(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", "o", "m"))
	f.c.pending["a"] = 1
	f.c.inFlight = 1
	if id, a := f.owner("o"); a != Defer || id != "" {
		t.Fatalf("Lookup = %q, %v", id, a)
	}
	if got := f.obs.take(); !slices.Equal(got, []string{"read lookup pending"}) {
		t.Fatalf("events = %v", got)
	}
}

// A handle from another cache defers with an error and never answers Absent.
func TestLookup_ForeignHandle(t *testing.T) {
	f := newFixture(t)
	other := newFixture(t)
	f.install(g("a", "o", "m"))
	for name, idx := range map[string]Index[string, string]{
		"other cache": other.byMember,
		"zero handle": {},
	} {
		id, a, err := Lookup(f.c, idx, "m")
		if !errors.Is(err, ErrForeignIndex) || a != Defer || id != "" {
			t.Errorf("%s: Lookup = %q, %v, %v", name, id, a, err)
		}
	}
}

// FUN-1: a trusted, idle cache answers the directory.
func TestList_Trusted(t *testing.T) {
	f := newFixture(t)
	f.install(g("c", ""), g("a", ""), g("b", ""))
	if ids, a := f.c.List(); a != Found || !slices.Equal(ids, []string{"a", "b", "c"}) {
		t.Fatalf("List = %v, %v", ids, a)
	}
	if got := f.obs.take(); !slices.Equal(got, []string{"read list cache"}) {
		t.Fatalf("events = %v", got)
	}
}

func TestList_EmptyTrusted(t *testing.T) {
	f := newFixture(t)
	f.install()
	if ids, a := f.c.List(); a != Found || ids == nil || len(ids) != 0 {
		t.Fatalf("List = %#v, %v", ids, a)
	}
}

// SAFE-2, SAFE-3: the directory defers while any mutation is in flight.
func TestList_DefersWhileInFlight(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", ""))
	f.c.inFlight = 1
	if ids, a := f.c.List(); a != Defer || ids != nil {
		t.Fatalf("List = %v, %v", ids, a)
	}
	if got := f.obs.take(); !slices.Equal(got, []string{"read list in_flight"}) {
		t.Fatalf("events = %v", got)
	}
}

func TestList_ReturnsDetachedSortedCopy(t *testing.T) {
	f := newFixture(t)
	f.install(g("b", ""), g("a", ""))
	ids, _ := f.c.List()
	ids[0] = "mutated"
	if again, _ := f.c.List(); !slices.Equal(again, []string{"a", "b"}) {
		t.Fatalf("List after caller mutation = %v", again)
	}
}

// SAFE-10: callers get copies.
func TestGet_ReturnsClone(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", "o", "m1", "m2"))
	e, _ := f.get("a")
	e.Members[0] = "mutated"
	again, _ := f.get("a")
	if again.Members[0] != "m1" {
		t.Fatalf("cache state changed through a returned value: %v", again)
	}
}

// NFR-2: a trusted cache issues no store calls on reads.
func TestReads_NoStoreCallsWhenTrusted(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", "o", "m"))
	f.probeLock()
	before := f.store.Calls()
	for range 100 {
		f.get("a")
		f.get("missing")
		f.member("m")
		f.member("none")
		f.c.List()
	}
	if after := f.store.Calls(); after != before {
		t.Fatalf("store calls %+v -> %+v", before, after)
	}
}
