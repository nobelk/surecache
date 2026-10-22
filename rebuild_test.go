// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nobelk/surecache/internal/memstore"
)

// prime seeds the store and primes the cache, failing the test unless it installs.
func (f *fixture) prime(gs ...group) {
	f.t.Helper()
	f.store.Seed(gs...)
	if r, err := f.c.Prime(context.Background()); r != Installed || err != nil {
		f.t.Fatalf("Prime = %v, %v", r, err)
	}
	f.obs.take()
}

// SAFE-9: Prime installs and the cache becomes trusted.
func TestPrime_InstallsAndTrusts(t *testing.T) {
	f := newFixture(t)
	f.store.Seed(g("a", "o", "m"), group{ID: "x", Deleted: true})
	r, err := f.c.Prime(t.Context())
	if r != Installed || err != nil {
		t.Fatalf("Prime = %v, %v", r, err)
	}
	if e, a := f.get("a"); a != Found || e.Owner != "o" {
		t.Fatalf("Get = %v, %v", e, a)
	}
	want := []string{"stale false false", "entities 1", "rebuild prime installed", "read get cache"}
	if got := f.obs.take(); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// NFR-2: startup issues exactly one bulk load and no single reads.
func TestPrime_SingleBulkLoad(t *testing.T) {
	f := newFixture(t)
	f.prime(g("a", ""), g("b", ""))
	if c := f.store.Calls(); c.LoadAll != 1 || c.LoadOne != 0 {
		t.Fatalf("calls = %+v", c)
	}
}

// FUN-5, D24: a second Prime after Installed is refused.
func TestPrime_SecondCallRefused(t *testing.T) {
	f := newFixture(t)
	f.prime(g("a", ""))
	r, err := f.c.Prime(t.Context())
	if r != Failed || !errors.Is(err, ErrAlreadyPrimed) {
		t.Fatalf("Prime = %v, %v", r, err)
	}
	if f.store.Calls().LoadAll != 1 || f.isStale() {
		t.Fatal("refused Prime must not load or change trust")
	}
}

// D24: Prime may be retried after Failed or Discarded.
func TestPrime_RetryAfterFailure(t *testing.T) {
	f := newFixture(t)
	f.store.Seed(g("a", ""))
	f.store.InjectLoadAll(memstore.ReadFault{Kind: memstore.FailRead})
	if r, err := f.c.Prime(t.Context()); r != Failed || err == nil {
		t.Fatalf("Prime = %v, %v", r, err)
	}
	f.c.hooks = &hooks{afterLoadAll: func() {
		_ = f.c.Mutate(context.Background(), []string{"a"}, func() error { return nil })
		f.c.hooks = nil
	}}
	if r, err := f.c.Prime(t.Context()); r != Discarded || err != nil {
		t.Fatalf("Prime = %v, %v", r, err)
	}
	if r, err := f.c.Prime(t.Context()); r != Installed || err != nil {
		t.Fatalf("Prime = %v, %v", r, err)
	}
}

// SAFE-9: a failed prime is reported and leaves the cache stale.
func TestPrime_FailureReported(t *testing.T) {
	f := newFixture(t)
	f.store.InjectLoadAll(memstore.ReadFault{Kind: memstore.FailRead})
	r, err := f.c.Prime(t.Context())
	if r != Failed || err != memstore.ErrAmbiguous {
		t.Fatalf("Prime = %v, %v; want the load error unchanged", r, err)
	}
	if !f.isStale() {
		t.Fatal("failed prime must leave the cache stale")
	}
	if got := f.obs.take(); !slices.Equal(got, []string{"rebuild prime failed"}) {
		t.Fatalf("events = %v", got)
	}
}

// SAFE-9, NFR-6: a prime past its budget fails with the context error unwrapped.
func TestPrime_TimeoutReported(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, withOptions(WithPrimeBudget(5*time.Second)))
		f.store.InjectLoadAll(memstore.ReadFault{Kind: memstore.BlockRead, Release: make(chan struct{})})
		start := time.Now()
		r, err := f.c.Prime(context.Background())
		if r != Failed || err != context.DeadlineExceeded {
			t.Fatalf("Prime = %v, %v; want == context.DeadlineExceeded", r, err)
		}
		if time.Since(start) != 5*time.Second {
			t.Fatalf("prime took %v", time.Since(start))
		}
		if !f.isStale() {
			t.Fatal("timed-out prime must leave the cache stale")
		}
	})
}

// NFR-6: the prime budget bounds Prime.
func TestPrime_Budget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.store.InjectLoadAll(memstore.ReadFault{Kind: memstore.BlockRead, Release: make(chan struct{})})
		start := time.Now()
		_, _ = f.c.Prime(context.Background())
		if d := time.Since(start); d != defaultPrimeBudget {
			t.Fatalf("Prime took %v, want the default budget", d)
		}
	})
}

// SAFE-8: a prime over capacity fails and installs nothing.
func TestPrime_OverCapacityFails(t *testing.T) {
	f := newFixture(t, withCapacity(1, 10, 10))
	f.store.Seed(g("a", ""), g("b", ""))
	r, err := f.c.Prime(t.Context())
	if r != Failed || !errors.Is(err, ErrOverCapacity) {
		t.Fatalf("Prime = %v, %v", r, err)
	}
	assertNothingInstalled(t, f)
}

// SAFE-12: a bulk load that breaks the contract fails prime.
func TestPrime_ContractViolationFails(t *testing.T) {
	f := newFixture(t)
	f.store.Seed(g("a", "", "m"), g("b", "", "m"))
	r, err := f.c.Prime(t.Context())
	if r != Failed || !errors.Is(err, ErrContract) {
		t.Fatalf("Prime = %v, %v", r, err)
	}
	assertNothingInstalled(t, f)
}

// SAFE-6: a rebuild whose snapshot may predate a commit that landed mid-read discards.
func TestRebuild_DiscardedByMidReadCommit(t *testing.T) {
	f := newFixture(t)
	f.store.Seed(g("a", ""))
	f.c.hooks = &hooks{afterLoadAll: func() {
		if err := f.mutate(context.Background(), []string{"a"}, set(g("a", "o"))); err != nil {
			t.Error(err)
		}
	}}
	r, err := f.c.Prime(t.Context())
	if r != Discarded || err != nil {
		t.Fatalf("Prime = %v, %v", r, err)
	}
	if !f.isStale() {
		t.Fatal("a discarded rebuild leaves the cache stale")
	}
	if got := f.obs.take(); !slices.Equal(got, []string{"rebuild prime discarded"}) {
		t.Fatalf("events = %v", got)
	}
}

// SAFE-6, internal invariant 5: an install bumps every open refresh window, so a
// refresh that read before the rebuild's capture cannot install over it.
func TestRebuild_InstallBumpsOpenWindows(t *testing.T) {
	f := newFixture(t, concurrent())
	f.prime(g("x", "o0"))
	gt := newGate()
	f.c.hooks = &hooks{afterLoadOne: onID("x", gt)}
	old := f.runMutate([]string{"x"}, g("x", "o1"))
	<-gt.arrived // the refresh read o1 and stalls
	// An ambiguous write commits o2 and marks the cache stale.
	f.store.InjectWrite(memstore.WriteFault{Kind: memstore.AmbiguousCommitted})
	_ = f.mutate(context.Background(), []string{"x"}, set(g("x", "o2")))
	// A rebuild captures after it and installs o2.
	f.c.mu.Lock()
	f.c.generation++
	captured := f.c.generation
	f.c.mu.Unlock()
	if r, err := f.c.rebuild(context.Background(), captured, TriggerLoop); r != Installed {
		t.Fatalf("rebuild = %v, %v", r, err)
	}
	close(gt.release)
	mustNil(t, old)
	if e, a := f.get("x"); a != Found || e.Owner != "o2" {
		t.Fatalf("Get = %v, %v: the stalled refresh installed over the rebuild", e, a)
	}
}

// Internal invariant 5: a failed rebuild leaves the old maps in place (still stale).
func TestRebuild_FailedLeavesOldMaps(t *testing.T) {
	f := newFixture(t)
	f.prime(g("a", "o", "m"))
	f.c.markStale(t.Context(), errors.New("x"))
	f.store.Seed(g("b", "", "m"))
	f.c.mu.Lock()
	f.c.generation++
	captured := f.c.generation
	f.c.mu.Unlock()
	if r, err := f.c.rebuild(t.Context(), captured, TriggerLoop); r != Failed || !errors.Is(err, ErrContract) {
		t.Fatalf("rebuild = %v, %v", r, err)
	}
	f.c.mu.RLock()
	defer f.c.mu.RUnlock()
	if e, ok := f.c.entities.Get("a"); !ok || e.Owner != "o" || f.c.entities.Len() != 1 {
		t.Fatal("old maps replaced by a failed rebuild")
	}
}

// Internal invariant 6: stale is sticky until a rebuild returns Installed.
func TestStale_OnlyClearedByInstalledRebuild(t *testing.T) {
	f := newFixture(t)
	f.prime(g("a", ""))
	f.c.markStale(t.Context(), errors.New("x"))
	ctx := t.Context()
	_ = f.mutate(ctx, []string{"a"}, set(g("a", "o")))
	_ = f.mutate(ctx, []string{"a"}, func(*txn) error { return memstore.ErrPrecondition })
	_, _ = f.create(ctx, g("b", ""))
	f.c.refreshBatch(ctx, []string{"a"})
	if r, _ := f.c.Prime(ctx); r != Failed {
		t.Fatal("Prime after Installed must be refused")
	}
	if !f.isStale() {
		t.Fatal("stale cleared without an installed rebuild")
	}
	f.c.mu.Lock()
	f.c.generation++
	captured := f.c.generation
	f.c.mu.Unlock()
	if r, _ := f.c.rebuild(ctx, captured, TriggerLoop); r != Installed || f.isStale() {
		t.Fatalf("rebuild = %v stale=%t", r, f.isStale())
	}
}

// FUN-3: reads never rebuild, even on a stale cache.
func TestReads_NeverRebuild(t *testing.T) {
	f := newFixture(t)
	f.store.Seed(g("a", "o", "m"))
	for range 10_000 {
		f.get("a")
		f.member("m")
		f.c.List()
	}
	if c := f.store.Calls(); c.LoadAll != 0 || c.LoadOne != 0 {
		t.Fatalf("calls = %+v", c)
	}
}
