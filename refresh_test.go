// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nobelk/surecache/internal/memstore"
)

// SAFE-2: pending marks hold until the refresh installs, so no reader sees the
// pre-mutation state in the commit-to-refresh gap.
func TestMutate_PendingHeldUntilRefreshInstalled(t *testing.T) {
	f := newFixture(t, concurrent())
	f.install(g("a", "o", "m1"), g("b", ""))
	gt := newGate()
	f.c.hooks = &hooks{afterLoadOne: onID("a", gt)}
	done := make(chan error)
	go func() { done <- f.mutate(context.Background(), []string{"a"}, set(g("a", "o", "m1", "m2"))) }()
	<-gt.arrived
	// Committed, refresh read but not installed: affected reads defer.
	if _, a := f.get("a"); a != Defer {
		t.Fatalf("Get during refresh = %v", a)
	}
	if _, a := f.member("m1"); a != Defer {
		t.Fatalf("Lookup hit on pending = %v", a)
	}
	if _, a := f.member("m2"); a != Defer {
		t.Fatalf("Lookup miss in flight = %v", a)
	}
	if _, a := f.c.List(); a != Defer {
		t.Fatalf("List in flight = %v", a)
	}
	if _, a := f.get("b"); a != Found {
		t.Fatalf("unaffected Get = %v", a)
	}
	close(gt.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if id, a := f.member("m2"); a != Found || id != "a" {
		t.Fatalf("after install = %q, %v", id, a)
	}
}

// SAFE-2: a create's id is unknown while it runs, so index misses and List defer.
func TestCreate_LookupMissDefersDuringCreate(t *testing.T) {
	f := newFixture(t, concurrent())
	f.install(g("a", "", "m1"))
	gt := newGate()
	f.c.hooks = &hooks{afterOp: gt.block}
	done := make(chan error)
	go func() {
		_, err := f.create(context.Background(), g("n", "", "new"))
		done <- err
	}()
	<-gt.arrived
	if _, a := f.member("new"); a != Defer {
		t.Fatalf("Lookup miss during create = %v", a)
	}
	if _, a := f.c.List(); a != Defer {
		t.Fatalf("List during create = %v", a)
	}
	if id, a := f.member("m1"); a != Found || id != "a" {
		t.Fatalf("unrelated hit = %q, %v", id, a)
	}
	close(gt.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if id, a := f.member("new"); a != Found || id != "n" {
		t.Fatalf("after create = %q, %v", id, a)
	}
}

// Section 4.4: not-found on refresh means the entity is gone; it is removed.
func TestRefresh_DeletedEntityRemoved(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", "o", "m"), g("b", ""))
	if err := f.mutate(t.Context(), []string{"a"}, del("a")); err != nil {
		t.Fatal(err)
	}
	if got := f.obs.take(); len(got) != 1 || got[0] != "entities 1" {
		t.Fatalf("events = %v", got)
	}
	if _, a := f.member("m"); a != Absent {
		t.Fatalf("Lookup = %v", a)
	}
	if _, a := f.owner("o"); a != Absent {
		t.Fatalf("owner = %v", a)
	}
	if ids, _ := f.c.List(); len(ids) != 1 || ids[0] != "b" {
		t.Fatalf("List = %v", ids)
	}
	if f.isStale() {
		t.Fatal("delete keeps trust")
	}
}

// Internal invariant 8: a non-cacheable refresh result is removed.
func TestRefresh_NonCacheableRemoved(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", "o", "m"))
	if err := f.mutate(t.Context(), []string{"a"}, set(group{ID: "a", Deleted: true})); err != nil {
		t.Fatal(err)
	}
	if _, a := f.get("a"); a != Defer {
		t.Fatalf("Get = %v", a)
	}
	if _, a := f.member("m"); a != Absent {
		t.Fatalf("Lookup = %v", a)
	}
	if ids, a := f.c.List(); a != Found || len(ids) != 0 {
		t.Fatalf("List = %v, %v", ids, a)
	}
	// Refreshing a non-cacheable entity the cache never held changes nothing.
	f.obs.take()
	if err := f.mutate(t.Context(), []string{"x"}, set(group{ID: "x", Deleted: true})); err != nil {
		t.Fatal(err)
	}
	if got := f.obs.take(); len(got) != 0 || f.isStale() {
		t.Fatalf("events = %v stale=%t", got, f.isStale())
	}
}

// PRE-3, SAFE-12, internal invariant 8: a non-cacheable entity with index keys marks
// stale.
func TestRefresh_NonCacheableWithKeysMarksStale(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", "o", "m"))
	bad := g("a", "o", "m")
	bad.Deleted = true
	if err := f.mutate(t.Context(), []string{"a"}, set(bad)); err != nil {
		t.Fatal(err)
	}
	if !f.isStale() || !errors.Is(f.staleCause(), ErrContract) {
		t.Fatalf("stale=%t cause=%v", f.isStale(), f.staleCause())
	}
}

// SAFE-12: LoadOne returning another entity's key marks stale.
func TestRefresh_KeyMismatchMarksStale(t *testing.T) {
	f := newFixture(t, withSource(func(s *memstore.Store[string, group]) Source[string, group] {
		return wrongKey{s}
	}))
	f.install(g("a", ""))
	if err := f.mutate(t.Context(), []string{"a"}, set(g("a", "o"))); err != nil {
		t.Fatal(err)
	}
	if !f.isStale() || !errors.Is(f.staleCause(), ErrContract) {
		t.Fatalf("stale=%t cause=%v", f.isStale(), f.staleCause())
	}
}

type wrongKey struct{ *memstore.Store[string, group] }

func (w wrongKey) LoadOne(ctx context.Context, id string) (group, error) {
	e, err := w.Store.LoadOne(ctx, id)
	e.ID = "other"
	return e, err
}

// FUN-6: a failed refresh marks stale.
func TestRefresh_LoadErrorMarksStale(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", ""))
	f.store.InjectLoadOne(memstore.ReadFault{Kind: memstore.FailRead})
	_ = f.mutate(t.Context(), []string{"a"}, set(g("a", "o")))
	if !f.isStale() || !errors.Is(f.staleCause(), memstore.ErrAmbiguous) {
		t.Fatalf("stale=%t cause=%v", f.isStale(), f.staleCause())
	}
}

// Internal invariant 7: once a batch has marked the cache stale it reads no more.
func TestRefresh_StopsReadingOnceStale(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", ""), g("b", ""))
	f.store.InjectLoadOne(memstore.ReadFault{Kind: memstore.FailRead})
	_ = f.mutate(t.Context(), []string{"a", "b"}, set(g("a", "o"), g("b", "p")))
	if n := f.store.Calls().LoadOne; n != 1 {
		t.Fatalf("LoadOne calls = %d, want 1", n)
	}
}

// FUN-8: a refresh failure is never reported as a mutation failure.
func TestMutate_RefreshFailureNotReported(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", ""))
	f.store.InjectLoadOne(memstore.ReadFault{Kind: memstore.FailRead})
	if err := f.mutate(t.Context(), []string{"a"}, set(g("a", "o"))); err != nil {
		t.Fatalf("Mutate = %v", err)
	}
	f.store.InjectLoadOne(memstore.ReadFault{Kind: memstore.FailRead})
	if _, err := f.create(t.Context(), g("n", "")); err != nil {
		t.Fatalf("Create = %v", err)
	}
}

// FUN-6: the refresh runs detached from the caller's cancellation.
func TestRefresh_DetachedFromCallerCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.install(g("a", ""))
		ctx, cancel := context.WithCancel(context.Background())
		f.c.hooks = &hooks{afterOp: cancel}
		if err := f.mutate(ctx, []string{"a"}, set(g("a", "o"))); err != nil {
			t.Fatal(err)
		}
		if f.isStale() {
			t.Fatalf("refresh used the cancelled context: %v", f.staleCause())
		}
		if e, _ := f.get("a"); e.Owner != "o" {
			t.Fatalf("Get = %v", e)
		}
	})
}

// FUN-6: values the caller attached to the context survive the detach.
func TestRefresh_KeepsContextValues(t *testing.T) {
	var seen any
	var deadline bool
	f := newFixture(t, withSource(func(s *memstore.Store[string, group]) Source[string, group] {
		return ctxSpy{s, func(ctx context.Context) {
			seen = ctx.Value(ctxKey{})
			_, deadline = ctx.Deadline()
		}}
	}))
	f.install(g("a", ""))
	ctx := context.WithValue(t.Context(), ctxKey{}, "trace-1")
	if err := f.mutate(ctx, []string{"a"}, set(g("a", "o"))); err != nil {
		t.Fatal(err)
	}
	if seen != "trace-1" || !deadline {
		t.Fatalf("refresh ctx value=%v deadline=%t", seen, deadline)
	}
}

type ctxSpy struct {
	*memstore.Store[string, group]
	see func(context.Context)
}

func (s ctxSpy) LoadOne(ctx context.Context, id string) (group, error) {
	s.see(ctx)
	return s.Store.LoadOne(ctx, id)
}

// FUN-6, NFR-6: a refresh past its budget marks stale; the mutation still succeeds.
func TestRefresh_TimeoutMarksStale(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, withOptions(WithRefreshBudget(3*time.Second)))
		f.install(g("a", ""))
		f.store.InjectLoadOne(memstore.ReadFault{Kind: memstore.BlockRead, Release: make(chan struct{})})
		start := time.Now()
		if err := f.mutate(context.Background(), []string{"a"}, set(g("a", "o"))); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != 3*time.Second {
			t.Fatalf("refresh took %v, want the 3s budget", elapsed)
		}
		if !f.isStale() || !errors.Is(f.staleCause(), context.DeadlineExceeded) {
			t.Fatalf("stale=%t cause=%v", f.isStale(), f.staleCause())
		}
		if p, n, w := f.marks(); p+n+w != 0 {
			t.Fatalf("marks not released: %d %d %d", p, n, w)
		}
	})
}

// Internal invariant 3: marks are released only after settle completes, so the refresh
// has installed before any reader can hit the entity again.
func TestMutate_MarksReleasedAfterSettle(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", "o"))
	checked := false
	f.c.hooks = &hooks{beforeRelease: func(_ context.Context, ids any) {
		f.c.mu.RLock()
		defer f.c.mu.RUnlock()
		e, _ := f.c.entities.Get("a")
		if f.c.pending["a"] != 1 || e.Owner != "o2" || ids.([]string)[0] != "a" {
			t.Errorf("before release: pending=%d cached=%v", f.c.pending["a"], e)
		}
		checked = true
	}}
	if err := f.mutate(t.Context(), []string{"a"}, set(g("a", "o2"))); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("hook did not run")
	}
}

// Internal invariant 10: every window of a batch is open before the first read.
func TestRefresh_WindowsOpenBeforeFirstRead(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", ""), g("b", ""))
	var open int
	f.c.hooks = &hooks{beforeRefresh: func(any) {
		f.c.mu.RLock()
		open = len(f.c.refreshes)
		f.c.mu.RUnlock()
	}}
	if err := f.mutate(t.Context(), []string{"a", "b", "c"}, set(g("a", "x"), g("b", "y"))); err != nil {
		t.Fatal(err)
	}
	if open != 3 || f.store.Calls().LoadOne != 3 {
		t.Fatalf("windows open before the first read = %d", open)
	}
}
