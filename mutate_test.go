// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/nobelk/surecache/internal/memstore"
)

// D18: Mutate with no ids runs nothing.
func TestMutate_NoIDs(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", ""))
	ran := false
	for _, ids := range [][]string{nil, {}} {
		err := f.c.Mutate(t.Context(), ids, func() error { ran = true; return nil })
		if !errors.Is(err, ErrNoIDs) || ran {
			t.Fatalf("Mutate(%v) = %v, ran=%t", ids, err, ran)
		}
	}
}

// D18, internal invariant 3: duplicate ids are marked once.
func TestMutate_DedupesIDs(t *testing.T) {
	for _, n := range []int{3, 20} { // the linear scan and the map path
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			f := newFixture(t)
			f.install(g("a", ""), g("b", ""))
			ids := []string{"a", "b"}
			for i := range n {
				ids = append(ids, fmt.Sprint("x", i%3), "a")
			}
			var pending map[string]int
			f.c.hooks = &hooks{afterMark: func(any) { pending = maps(f.c) }}
			if err := f.c.Mutate(t.Context(), ids, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			for id, cnt := range pending {
				if cnt != 1 {
					t.Errorf("pending[%s] = %d, want 1", id, cnt)
				}
			}
			if len(pending) != 5 {
				t.Errorf("pending = %v", pending)
			}
		})
	}
}

func maps(c *Cache[string, group]) map[string]int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := map[string]int{}
	for k, v := range c.pending {
		out[k] = v
	}
	return out
}

// Internal invariant 3: a create counts in flight exactly once and marks no id pending.
func TestCreate_InFlightCountedOnce(t *testing.T) {
	f := newFixture(t)
	f.install()
	var pending, inFlight int
	f.c.hooks = &hooks{afterMark: func(ids any) {
		if ids.([]string) != nil {
			t.Errorf("create marked ids %v", ids)
		}
		pending, inFlight, _ = f.marks()
	}}
	id, err := f.create(t.Context(), g("n", "", "m"))
	if err != nil || id != "n" {
		t.Fatalf("Create = %q, %v", id, err)
	}
	if pending != 0 || inFlight != 1 {
		t.Fatalf("during create pending=%d inFlight=%d", pending, inFlight)
	}
}

// NFR-1: pending marks are dropped at zero.
func TestPendingMarks_DroppedAtZero(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", ""))
	_ = f.mutate(t.Context(), []string{"a", "b"}, set(g("a", "o")))
	_ = f.mutate(t.Context(), []string{"a"}, func(*memstore.Tx[string, group]) error { return memstore.ErrPrecondition })
	if p, n, _ := f.marks(); p != 0 || n != 0 {
		t.Fatalf("pending=%d inFlight=%d after mutations", p, n)
	}
}

// NFR-1: refresh windows are dropped when no refresh is active.
func TestRefreshWindows_DroppedAtZero(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", ""))
	_ = f.mutate(t.Context(), []string{"a", "b"}, set(g("a", "o"), g("b", "")))
	if _, _, w := f.marks(); w != 0 {
		t.Fatalf("windows = %d after refresh", w)
	}
}

// FUN-8: the caller receives op's error unchanged.
func TestMutate_ReturnsOpErrorUnchanged(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", ""))
	opErr := fmt.Errorf("wrapped: %w", context.Canceled)
	for _, want := range []error{nil, memstore.ErrNotFound, errors.New("driver"), opErr} {
		if got := f.c.Mutate(t.Context(), []string{"a"}, func() error { return want }); got != want {
			t.Fatalf("Mutate = %v, want %v", got, want)
		}
		id, got := f.c.Create(t.Context(), func() (string, error) { return "z", want })
		if got != want || id != "z" {
			t.Fatalf("Create = %q, %v, want z, %v", id, got, want)
		}
	}
}

// FUN-7: a nothing-committed error leaves the cache as is and refreshes nothing.
func TestMutate_NothingCommittedLeavesCache(t *testing.T) {
	for _, nc := range []error{memstore.ErrNotFound, memstore.ErrExists, memstore.ErrPrecondition, memstore.ErrConflict} {
		t.Run(nc.Error(), func(t *testing.T) {
			f := newFixture(t)
			f.install(g("a", "o", "m"))
			f.store.InjectWrite(memstore.WriteFault{Kind: memstore.NothingCommitted, Err: nc})
			if err := f.mutate(t.Context(), []string{"a"}, set(g("a", "o2"))); err != nc {
				t.Fatalf("err = %v", err)
			}
			if f.isStale() {
				t.Fatal("nothing-committed error marked stale")
			}
			if e, a := f.get("a"); a != Found || e.Owner != "o" {
				t.Fatalf("Get = %v, %v", e, a)
			}
			if f.store.Calls().LoadOne != 0 {
				t.Fatal("nothing-committed error refreshed")
			}
		})
	}
}

// SAFE-4: an ambiguous outcome marks the whole cache stale at once.
func TestMutate_AmbiguousErrorMarksStale(t *testing.T) {
	for _, kind := range []memstore.WriteFaultKind{memstore.AmbiguousCommitted, memstore.AmbiguousNotCommitted} {
		f := newFixture(t)
		f.install(g("a", "o"), g("b", ""))
		var cause error
		f.obs.take()
		f.store.InjectWrite(memstore.WriteFault{Kind: kind})
		err := f.mutate(t.Context(), []string{"a"}, set(g("a", "o2")))
		if err != memstore.ErrAmbiguous {
			t.Fatalf("err = %v", err)
		}
		if !f.isStale() {
			t.Fatal("ambiguous outcome must mark stale")
		}
		cause = f.staleCause()
		if !errors.Is(cause, ErrOutcomeUnknown) || !errors.Is(cause, memstore.ErrAmbiguous) {
			t.Fatalf("cause = %v", cause)
		}
		if _, a := f.get("b"); a != Defer {
			t.Fatal("whole cache must defer")
		}
		if f.store.Calls().LoadOne != 0 {
			t.Fatal("SAFE-5: an ambiguous outcome must not refresh one entity")
		}
	}
}

// SAFE-4: a context error after the call reached the store is ambiguous.
func TestMutate_ContextErrorAfterStoreMarksStale(t *testing.T) {
	for _, ctxErr := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(ctxErr.Error(), func(t *testing.T) {
			f := newFixture(t)
			f.install(g("a", ""))
			f.store.InjectWrite(memstore.WriteFault{Kind: memstore.ContextAfterCommit, Err: ctxErr})
			if err := f.mutate(t.Context(), []string{"a"}, set(g("a", "o"))); err != ctxErr {
				t.Fatalf("err = %v", err)
			}
			if !f.isStale() || !errors.Is(f.staleCause(), ctxErr) {
				t.Fatalf("stale=%t cause=%v", f.isStale(), f.staleCause())
			}
		})
	}
}

// Section 9: every ambiguous mutation is counted, also on an already stale cache.
func TestMutate_AmbiguousCountsObserved(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", ""))
	for range 2 {
		f.store.InjectWrite(memstore.WriteFault{Kind: memstore.AmbiguousCommitted})
		_ = f.mutate(t.Context(), []string{"a"}, set(g("a", "o")))
	}
	got := f.obs.take()
	want := []string{"ambiguous", "stale true true", "ambiguous"}
	if !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// SAFE-1: after a mutation returns success, a read sees the new state.
func TestMutate_ReadAfterSuccessSeesNewState(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", "o", "m1"), g("b", ""))
	if err := f.mutate(t.Context(), []string{"a"}, set(g("a", "o2", "m2"))); err != nil {
		t.Fatal(err)
	}
	if e, a := f.get("a"); a != Found || e.Owner != "o2" {
		t.Fatalf("Get = %v, %v", e, a)
	}
	if _, a := f.member("m1"); a != Absent {
		t.Fatalf("old key = %v", a)
	}
	if id, a := f.member("m2"); a != Found || id != "a" {
		t.Fatalf("new key = %q, %v", id, a)
	}
	if id, a := f.owner("o2"); a != Found || id != "a" {
		t.Fatalf("owner = %q, %v", id, a)
	}
	if _, err := f.create(t.Context(), g("c", "", "m3")); err != nil {
		t.Fatal(err)
	}
	if ids, _ := f.c.List(); !slices.Equal(ids, []string{"a", "b", "c"}) {
		t.Fatalf("List = %v", ids)
	}
	if f.isStale() {
		t.Fatal("successful mutations keep the cache trusted")
	}
}

// Section 3.3: no write-behind; the store holds the change when Mutate returns.
func TestMutate_OpRunsBeforeReturn(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", ""))
	if err := f.mutate(t.Context(), []string{"a"}, set(g("a", "o"))); err != nil {
		t.Fatal(err)
	}
	if e, _ := f.store.Get("a"); e.Owner != "o" {
		t.Fatalf("store = %v", e)
	}
}

// FUN-4, internal invariant 7: a mutation on a stale cache reads nothing for the cache.
func TestMutate_StaleCacheIssuesNoStoreRead(t *testing.T) {
	f := newFixture(t)
	f.store.Seed(g("a", ""))
	if err := f.mutate(t.Context(), []string{"a"}, set(g("a", "o"))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.create(t.Context(), g("b", "")); err != nil {
		t.Fatal(err)
	}
	if n := f.store.Calls().LoadOne; n != 0 {
		t.Fatalf("LoadOne calls = %d", n)
	}
	if !f.isStale() {
		t.Fatal("a mutation never clears stale")
	}
}

// FUN-4, clarification 6: a refresh batch on a stale cache still bumps the generation,
// so an in-progress rebuild discards.
func TestMutate_StaleStillBumpsGeneration(t *testing.T) {
	f := newFixture(t)
	before := f.generation()
	if err := f.c.Mutate(t.Context(), []string{"a"}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if f.generation() <= before {
		t.Fatal("generation not bumped")
	}
}

// SAFE-11, clarification 5: a panic in op marks stale, releases every mark, and the
// caller sees the same panic.
func TestMutate_PanicMarksStaleAndReleasesMarks(t *testing.T) {
	type boom struct{ n int }
	for _, create := range []bool{false, true} {
		f := newFixture(t)
		f.install(g("a", ""))
		func() {
			defer func() {
				if r := recover(); r != (boom{7}) {
					t.Fatalf("recover = %v", r)
				}
			}()
			if create {
				_, _ = f.c.Create(t.Context(), func() (string, error) { panic(boom{7}) })
			} else {
				_ = f.c.Mutate(t.Context(), []string{"a"}, func() error { panic(boom{7}) })
			}
		}()
		if p, n, w := f.marks(); p != 0 || n != 0 || w != 0 {
			t.Fatalf("pending=%d inFlight=%d windows=%d", p, n, w)
		}
		if !f.isStale() || !errors.Is(f.staleCause(), ErrUnsettled) {
			t.Fatalf("stale=%t cause=%v", f.isStale(), f.staleCause())
		}
	}
}

// Internal invariant 2: refresh start and mark-stale bump the generation.
func TestGeneration_BumpedOnRefreshStartMarkStaleCapture(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", ""))
	g0 := f.generation()
	if err := f.mutate(t.Context(), []string{"a"}, set(g("a", "o"))); err != nil {
		t.Fatal(err)
	}
	g1 := f.generation()
	if g1 <= g0 {
		t.Fatal("refresh start did not bump the generation")
	}
	f.c.mu.Lock()
	f.c.markStaleLocked(t.Context(), errors.New("x"))
	f.c.markStaleLocked(t.Context(), errors.New("again"))
	f.c.mu.Unlock()
	if g2 := f.generation(); g2 != g1+2 {
		t.Fatalf("mark-stale must bump on every call: %d -> %d", g1, g2)
	}
}

// Internal invariant 1: no store call under the cache lock, across every path in this
// milestone. The fixture's lock probe fails the test on a violation.
func TestStoreCalls_NeverUnderLock(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", "o", "m"), g("b", ""))
	ctx := t.Context()
	_ = f.mutate(ctx, []string{"a"}, set(g("a", "o2", "m2")))
	_, _ = f.create(ctx, g("c", "", "n"))
	_ = f.mutate(ctx, []string{"b"}, del("b"))
	_ = f.mutate(ctx, []string{"a"}, func(*memstore.Tx[string, group]) error { return memstore.ErrPrecondition })
	f.store.InjectLoadOne(memstore.ReadFault{Kind: memstore.FailRead})
	_ = f.mutate(ctx, []string{"a"}, set(g("a", "o3")))
	_ = f.mutate(ctx, []string{"a"}, set(g("a", "o4")))
	if n := f.store.Calls(); n.LoadOne < 3 || n.Write < 6 {
		t.Fatalf("store calls = %+v", n)
	}
}

func (f *fixture) generation() uint64 {
	f.c.mu.RLock()
	defer f.c.mu.RUnlock()
	return f.c.generation
}
