// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/nobelk/surecache/internal/memstore"
)

// Deterministic interleavings of section 11.1, driven by hooks and channels. Each test
// fixes one order of events; none depends on timing.

// run starts a mutation in the background and returns its result channel.
func (f *fixture) runMutate(ids []string, es ...group) <-chan error {
	done := make(chan error, 1)
	go func() { done <- f.mutate(context.Background(), ids, set(es...)) }()
	return done
}

// nthLoadOne returns a hook that blocks the n-th LoadOne of id (1-based) on gt.
func nthLoadOne(id string, n int32, gt *gate) func(any) {
	var seen atomic.Int32
	return func(v any) {
		if v.(string) == id && seen.Add(1) == n {
			gt.block()
		}
	}
}

func mustNil(t *testing.T, ch <-chan error) {
	t.Helper()
	if err := <-ch; err != nil {
		t.Fatal(err)
	}
}

// SAFE-6: an older refresh of an entity is discarded by a newer one.
func TestRefresh_OlderSameEntityDiscarded(t *testing.T) {
	f := newFixture(t, concurrent())
	f.install(g("x", "o0"))
	gt := newGate()
	f.c.hooks = &hooks{afterLoadOne: nthLoadOne("x", 1, gt)}
	older := f.runMutate([]string{"x"}, g("x", "o1"))
	<-gt.arrived // the older refresh read o1 and stalls before installing
	if err := f.mutate(context.Background(), []string{"x"}, set(g("x", "o2"))); err != nil {
		t.Fatal(err)
	}
	close(gt.release)
	mustNil(t, older)
	if e, a := f.get("x"); a != Found || e.Owner != "o2" {
		t.Fatalf("Get = %v, %v; the older value overwrote the newer", e, a)
	}
	if id, a := f.owner("o2"); a != Found || id != "x" {
		t.Fatalf("owner o2 = %q, %v", id, a)
	}
	if _, a := f.owner("o1"); a != Absent {
		t.Fatalf("owner o1 = %v", a)
	}
}

// SAFE-6: refreshes of different entities do not discard each other.
func TestRefresh_DifferentEntitiesDoNotDiscard(t *testing.T) {
	f := newFixture(t, concurrent())
	f.install(g("x", ""), g("y", ""))
	gt := newGate()
	f.c.hooks = &hooks{afterLoadOne: nthLoadOne("x", 1, gt)}
	slow := f.runMutate([]string{"x"}, g("x", "ox"))
	<-gt.arrived
	if err := f.mutate(context.Background(), []string{"y"}, set(g("y", "oy"))); err != nil {
		t.Fatal(err)
	}
	close(gt.release)
	mustNil(t, slow)
	if e, _ := f.get("x"); e.Owner != "ox" {
		t.Fatalf("x = %v: a refresh of y discarded x's", e)
	}
	if f.isStale() {
		t.Fatal("cache went stale")
	}
}

// SAFE-7, internal invariant 4: while a batch moves K from x to y, y installs first and
// x's cached value still lists K. Re-indexing x must not delete y's entry.
func TestRefresh_LaggingValueKeepsNewHolder(t *testing.T) {
	f := newFixture(t, concurrent())
	f.install(g("x", "", "k"), g("y", ""))
	gt := newGate()
	f.c.hooks = &hooks{afterLoadOne: nthLoadOne("x", 1, gt)}
	done := f.runMutate([]string{"y", "x"}, g("x", ""), g("y", "", "k"))
	<-gt.arrived // y installed with K; x read but not yet re-indexed
	f.c.mu.RLock()
	holder, _ := f.c.indexes[f.byMember.pos].(*index[string, group, string]).m.Get("k")
	lagging, _ := f.c.entities.Get("x")
	f.c.mu.RUnlock()
	if holder != "y" || len(lagging.Members) != 1 {
		t.Fatalf("setup: holder=%q lagging x=%v", holder, lagging)
	}
	close(gt.release)
	mustNil(t, done)
	if id, a := f.member("k"); a != Found || id != "y" {
		t.Fatalf("Lookup(k) = %q, %v", id, a)
	}
	if f.isStale() {
		t.Fatal("cache went stale")
	}
}

// SAFE-7, internal invariant 4: removing x (deleted in the same transaction that moved
// K to y) keeps y's entry.
func TestRefresh_RemovalKeepsOtherHolder(t *testing.T) {
	f := newFixture(t)
	f.install(g("x", "", "k"), g("y", ""))
	err := f.mutate(context.Background(), []string{"y", "x"}, func(tx *txn) error {
		tx.Put(g("y", "", "k"))
		return tx.Delete("x")
	})
	if err != nil {
		t.Fatal(err)
	}
	if id, a := f.member("k"); a != Found || id != "y" {
		t.Fatalf("Lookup(k) = %q, %v", id, a)
	}
	if _, a := f.get("x"); a != Defer {
		t.Fatal("x not removed")
	}
	if f.isStale() {
		t.Fatal("cache went stale")
	}
}

// SAFE-6, SAFE-7: a refresh of x that read K stalls; K then leaves x and later goes to
// y. When the stalled refresh resumes, the entry stays on y, or the cache goes stale;
// it never names x while trusted.
func TestRefresh_StalledInstallAfterKeyTaken(t *testing.T) {
	f := newFixture(t, concurrent())
	f.install(g("x", ""), g("y", ""))
	gt := newGate()
	f.c.hooks = &hooks{afterLoadOne: nthLoadOne("x", 1, gt)}
	stalled := f.runMutate([]string{"x"}, g("x", "", "k"))
	<-gt.arrived
	if err := f.mutate(context.Background(), []string{"x"}, set(g("x", ""))); err != nil {
		t.Fatal(err)
	}
	if err := f.mutate(context.Background(), []string{"y"}, set(g("y", "", "k"))); err != nil {
		t.Fatal(err)
	}
	close(gt.release)
	mustNil(t, stalled)
	id, a := f.member("k")
	switch {
	case a == Found && id == "y":
	case a == Defer && f.isStale():
	default:
		t.Fatalf("Lookup(k) = %q, %v stale=%t", id, a, f.isStale())
	}
}

// SAFE-6: a mutation moves K from x to y while an older refresh of x (holding K) is
// stalled. K ends on y, never unindexed.
func TestRefresh_MoveKeyBetweenEntities(t *testing.T) {
	f := newFixture(t, concurrent())
	f.install(g("x", "", "k"), g("y", ""))
	gt := newGate()
	f.c.hooks = &hooks{afterLoadOne: nthLoadOne("x", 1, gt)}
	older := f.runMutate([]string{"x"}, g("x", "renamed", "k"))
	<-gt.arrived
	if err := f.mutate(context.Background(), []string{"x", "y"}, set(g("x", "renamed"), g("y", "", "k"))); err != nil {
		t.Fatal(err)
	}
	close(gt.release)
	mustNil(t, older)
	if id, a := f.member("k"); a != Found || id != "y" {
		t.Fatalf("Lookup(k) = %q, %v", id, a)
	}
	assertStructure(t, f)
}

// Internal invariant 10: a batch opens every window before its first read. A batch that
// moves K from x to y with y first must discard an older refresh of x that resumes
// after y installed but before the batch reaches x. Opening x's window lazily would let
// that refresh take K back, and the batch's own refresh of x would then unindex K.
func TestRefresh_BatchOpensAllWindowsFirst(t *testing.T) {
	f := newFixture(t, concurrent())
	f.install(g("x", "", "k"), g("y", ""))
	older, batch := newGate(), newGate()
	f.c.hooks = &hooks{
		afterLoadOne:  nthLoadOne("x", 1, older),
		beforeLoadOne: nthLoadOne("x", 2, batch),
	}
	olderDone := f.runMutate([]string{"x"}, g("x", "renamed", "k"))
	<-older.arrived
	batchDone := f.runMutate([]string{"y", "x"}, g("x", "renamed"), g("y", "", "k"))
	<-batch.arrived // y installed with K; the batch is about to read x
	close(older.release)
	mustNil(t, olderDone)
	close(batch.release)
	mustNil(t, batchDone)
	if id, a := f.member("k"); a != Found || id != "y" {
		t.Fatalf("Lookup(k) = %q, %v", id, a)
	}
	assertStructure(t, f)
}

// Internal invariant 4: an incoming key held by another entity with no refresh in
// progress marks stale and leaves the maps untouched.
func TestRefresh_ConflictMarksStaleMapsUntouched(t *testing.T) {
	f := newFixture(t)
	f.install(g("x", "", "a"), g("z", "", "k"))
	// PRE-4 violation: the move from z to x names only x.
	if err := f.mutate(context.Background(), []string{"x"}, set(g("x", "", "a", "k"), g("z", ""))); err != nil {
		t.Fatal(err)
	}
	if !f.isStale() || !errors.Is(f.staleCause(), ErrKeyConflict) {
		t.Fatalf("stale=%t cause=%v", f.isStale(), f.staleCause())
	}
	f.c.mu.RLock()
	defer f.c.mu.RUnlock()
	members := f.c.indexes[f.byMember.pos].(*index[string, group, string]).m
	if h, _ := members.Get("k"); h != "z" {
		t.Fatalf("k moved to %q", h)
	}
	if h, _ := members.Get("a"); h != "x" {
		t.Fatalf("a = %q", h)
	}
	if x, _ := f.c.entities.Get("x"); len(x.Members) != 1 {
		t.Fatalf("x changed: %v", x)
	}
}

// SAFE-11: a panicking create refresh must mark stale before releasing its last
// pending mark, even when an older delete refresh discarded its result.
func TestCreate_RefreshPanicMarksStaleBeforeReleasingPending(t *testing.T) {
	for _, where := range []string{"store", "key"} {
		t.Run(where, func(t *testing.T) {
			f := newFixture(t, concurrent())
			f.install(g("x", "", "old"))
			delRead, createWindow := newGate(), newGate()
			panicValue := errors.New("refresh panic")
			if where == "key" {
				f.c.spec.Key = func(e group) string {
					if slices.Contains(e.Members, "fresh") {
						panic(panicValue)
					}
					return e.ID
				}
			}
			f.c.hooks = &hooks{
				afterLoadOne:  nthLoadOne("x", 1, delRead),
				beforeLoadOne: nthLoadOne("x", 2, createWindow),
				beforeRelease: func(_ context.Context, ids any) {
					if len(ids.([]string)) != 0 {
						return // only inspect the create, before guard cleanup
					}
					if !f.isStale() {
						t.Error("panicking refresh released its marks before marking stale")
					}
					if e, a := f.get("x"); a != Defer {
						t.Errorf("Get during panic cleanup = %v, %v", e, a)
					}
					if id, a := f.member("old"); a != Defer {
						t.Errorf("Lookup during panic cleanup = %q, %v", id, a)
					}
				},
			}
			deleted := make(chan error, 1)
			go func() { deleted <- f.mutate(context.Background(), []string{"x"}, del("x")) }()
			<-delRead.arrived
			if where == "store" {
				f.store.InjectLoadOne(memstore.ReadFault{Kind: memstore.PanicRead, PanicValue: panicValue})
			}
			panicked := make(chan any, 1)
			go func() {
				defer func() { panicked <- recover() }()
				_, _ = f.create(context.Background(), g("x", "", "fresh"))
			}()
			<-createWindow.arrived
			close(delRead.release)
			mustNil(t, deleted)
			close(createWindow.release)
			if got := <-panicked; got != panicValue {
				t.Fatalf("panic = %v, want original %v", got, panicValue)
			}
			if p, n, w := f.marks(); p+n+w != 0 {
				t.Fatalf("marks leaked: pending=%d inFlight=%d windows=%d", p, n, w)
			}
		})
	}
}

// SAFE-11, internal invariant 3: opening refresh windows also delivers queued
// state events. An observer panic there must not leak the batch's marks.
func TestCreate_OpeningRefreshObserverPanicReleasesMarks(t *testing.T) {
	f := newFixture(t)
	f.install(g("x", "", "old"))
	panicValue := errors.New("observer panic")
	f.c.cfg.observer = panicRefreshObserver{panicValue: panicValue}
	f.c.hooks = &hooks{afterOp: func() {
		// Model a state event queued by another operation but not yet delivered.
		f.c.mu.Lock()
		f.c.queueStateLocked(stateEvent{kind: stateEntities, n: 1})
		f.c.mu.Unlock()
	}}
	func() {
		defer func() {
			if got := recover(); got != panicValue {
				t.Errorf("panic = %v, want %v", got, panicValue)
			}
		}()
		_, _ = f.create(t.Context(), g("y", "", "fresh"))
	}()
	if !f.isStale() {
		t.Fatal("observer panic left cache trusted")
	}
	if p, n, w := f.marks(); p+n+w != 0 {
		t.Fatalf("marks leaked: pending=%d inFlight=%d windows=%d", p, n, w)
	}
	f.c.cfg.observer = f.obs
	f.c.hooks = nil
	if result, err := f.c.Prime(t.Context()); result != Installed || err != nil {
		t.Fatalf("Prime after panic = %v, %v", result, err)
	}
	if _, a := f.get("y"); a != Found {
		t.Fatalf("Get after recovery = %v", a)
	}
}

type panicRefreshObserver struct {
	NopObserver
	panicValue any
}

func (o panicRefreshObserver) Entities(int) { panic(o.panicValue) }

// assertStructure checks the structural invariant under the lock: every index entry
// names an entity that holds the key, every key of every entity is indexed to it, and
// the directory is the sorted key set.
func assertStructure(t *testing.T, f *fixture) {
	t.Helper()
	if err := checkStructure(f.c); err != nil {
		t.Fatal(err)
	}
}

// SAFE-1, SAFE-2: a refresh discarded by a newer same-entity refresh relies on the newer
// one's batch to hold the entity pending until it installs. A create names no ids up
// front, so its batch must mark the id itself: a delete of x whose refresh is discarded
// by the re-create of x must not leave the old x readable once the delete releases.
func TestCreate_RefreshHoldsMarkWhileItOwnsTheWindow(t *testing.T) {
	f := newFixture(t, concurrent())
	f.install(g("x", "", "k"))
	delRead, createWindow := newGate(), newGate()
	f.c.hooks = &hooks{
		afterLoadOne:  nthLoadOne("x", 1, delRead),
		beforeLoadOne: nthLoadOne("x", 2, createWindow),
	}
	deleted := make(chan error, 1)
	go func() { deleted <- f.mutate(context.Background(), []string{"x"}, del("x")) }()
	<-delRead.arrived // the delete committed and its refresh read "not found"
	created := make(chan error, 1)
	go func() {
		_, err := f.create(context.Background(), g("x", "", "fresh"))
		created <- err
	}()
	<-createWindow.arrived // the create committed and opened a newer window for x
	close(delRead.release)
	mustNil(t, deleted) // the delete's refresh was discarded; the delete released x
	if e, a := f.get("x"); a != Defer {
		t.Fatalf("Get(x) = %v, %v while the create's refresh owns x; the deleted value leaked", e, a)
	}
	if id, a := f.member("k"); a != Defer {
		t.Fatalf("Lookup(k) = %q, %v; k left with the deleted x", id, a)
	}
	close(createWindow.release)
	mustNil(t, created)
	if e, a := f.get("x"); a != Found || !slices.Equal(e.Members, []string{"fresh"}) {
		t.Fatalf("Get(x) = %v, %v after the create installed", e, a)
	}
	assertStructure(t, f)
}
