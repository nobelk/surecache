// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nobelk/surecache/internal/memstore"
)

// startRun runs the loop in the background; the returned stop cancels it, waits for it
// and fails the test unless Run returned nil.
func (f *fixture) startRun() (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.c.Run(ctx) }()
	synctest.Wait() // the loop is idle on its ticker before the test goes on
	return func() {
		cancel()
		if err := <-done; err != nil {
			f.t.Errorf("Run = %v", err)
		}
	}
}

// tickOnce advances virtual time by one rebuild interval and waits for the loop.
func tickOnce() {
	time.Sleep(defaultRebuildInterval)
	synctest.Wait()
}

// D23, clarification 3: Run requires a successful Prime.
func TestRun_RequiresPrime(t *testing.T) {
	f := newFixture(t)
	if err := f.c.Run(t.Context()); !errors.Is(err, ErrNotPrimed) {
		t.Fatalf("Run = %v", err)
	}
	f.store.InjectLoadAll(memstore.ReadFault{Kind: memstore.FailRead})
	_, _ = f.c.Prime(t.Context())
	if err := f.c.Run(t.Context()); !errors.Is(err, ErrNotPrimed) {
		t.Fatalf("Run after failed Prime = %v", err)
	}
}

// D24: a second concurrent Run is refused; Run may start again after it returns.
func TestRun_SecondConcurrentRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.prime(g("a", ""))
		stop := f.startRun()
		synctest.Wait()
		if err := f.c.Run(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
			t.Fatalf("second Run = %v", err)
		}
		stop()
		stop = f.startRun()
		stop()
	})
}

// FUN-5: the loop rebuilds a stale cache on the next tick.
func TestRun_RebuildsWhenStale(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.prime(g("a", ""))
		stop := f.startRun()
		defer stop()
		f.store.InjectWrite(memstore.WriteFault{Kind: memstore.AmbiguousCommitted})
		_ = f.mutate(context.Background(), []string{"a"}, set(g("a", "o")))
		if !f.isStale() {
			t.Fatal("setup: not stale")
		}
		tickOnce()
		if f.isStale() {
			t.Fatal("loop did not rebuild")
		}
		if e, _ := f.get("a"); e.Owner != "o" {
			t.Fatalf("Get = %v", e)
		}
		if got := f.obs.take(); !slices.Contains(got, "rebuild loop installed") {
			t.Fatalf("events = %v", got)
		}
	})
}

// FUN-5, NFR-2: a trusted cache issues no bulk loads across many ticks.
func TestRun_SkipsWhenTrusted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.prime(g("a", ""))
		stop := f.startRun()
		defer stop()
		for range 50 {
			tickOnce()
		}
		if c := f.store.Calls(); c.LoadAll != 1 {
			t.Fatalf("LoadAll calls = %d", c.LoadAll)
		}
	})
}

// Section 7: a failed loop rebuild stays stale and retries on the next tick.
func TestRun_RetriesAfterFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.prime(g("a", ""))
		stop := f.startRun()
		defer stop()
		f.c.markStale(context.Background(), errors.New("x"))
		f.store.InjectLoadAll(memstore.ReadFault{Kind: memstore.FailRead})
		tickOnce()
		if !f.isStale() {
			t.Fatal("failed rebuild cleared stale")
		}
		tickOnce()
		if f.isStale() {
			t.Fatal("loop did not retry")
		}
	})
}

// A discarded loop rebuild retries on the next tick.
func TestRun_RetriesAfterDiscard(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.prime(g("a", ""))
		var armed atomic.Bool
		f.c.hooks = &hooks{afterLoadAll: func() {
			if armed.CompareAndSwap(true, false) {
				_ = f.mutate(context.Background(), []string{"a"}, set(g("a", "o")))
			}
		}}
		stop := f.startRun()
		defer stop()
		f.c.markStale(context.Background(), errors.New("x"))
		armed.Store(true)
		tickOnce()
		if !f.isStale() {
			t.Fatal("rebuild was not discarded")
		}
		tickOnce()
		if f.isStale() {
			t.Fatal("loop did not retry after discard")
		}
		if e, _ := f.get("a"); e.Owner != "o" {
			t.Fatalf("Get = %v", e)
		}
	})
}

// Run returns nil when its context ends.
func TestRun_ReturnsOnCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.prime(g("a", ""))
		stop := f.startRun()
		tickOnce()
		stop()
	})
}

// Section 6.5: a panic in the store does not escape Run; the cache is stale and
// unlocked, and the next tick installs.
func TestRun_SurvivesStorePanic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.prime(g("a", ""))
		stop := f.startRun()
		defer stop()
		f.c.markStale(context.Background(), errors.New("x"))
		f.store.InjectLoadAll(memstore.ReadFault{Kind: memstore.PanicRead, PanicValue: "store"})
		tickOnce()
		assertStaleAndUnlocked(t, f)
		if got := f.obs.take(); !slices.Contains(got, "rebuild loop failed") {
			t.Fatalf("events = %v", got)
		}
		tickOnce()
		if f.isStale() {
			t.Fatal("next tick did not install")
		}
	})
}

// Section 6.5: a panic in a caller callback during install does not escape Run.
func TestRun_SurvivesCallbackPanic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var armed atomic.Bool
		spec := groupSpec(10)
		spec.Cacheable = func(e group) bool {
			if armed.CompareAndSwap(true, false) {
				panic("cacheable")
			}
			return !e.Deleted
		}
		store := newGroupStore()
		store.Seed(g("a", ""))
		obs := &recorder{}
		c, err := NewBuilder(spec, Source[string, group](store)).Build(WithOutagePolicy(testPolicy{}), WithObserver(obs))
		if err != nil {
			t.Fatal(err)
		}
		if r, err := c.Prime(context.Background()); r != Installed {
			t.Fatal(r, err)
		}
		f := &fixture{t: t, c: c, store: store, obs: obs}
		stop := f.startRun()
		defer stop()
		c.markStale(context.Background(), errors.New("x"))
		armed.Store(true)
		tickOnce()
		assertStaleAndUnlocked(t, f)
		tickOnce()
		if f.isStale() {
			t.Fatal("next tick did not install")
		}
	})
}

// A panicking tick-context hook does not escape Run; the next tick runs.
func TestRun_SurvivesTickContextPanic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		f := newFixture(t, withOptions(WithTickContext(func(ctx context.Context) context.Context {
			if calls.Add(1) == 1 {
				panic("tick")
			}
			return ctx
		})))
		f.prime(g("a", ""))
		stop := f.startRun()
		defer stop()
		tickOnce()
		assertStaleAndUnlocked(t, f)
		tickOnce()
		if calls.Load() != 2 || f.isStale() {
			t.Fatalf("calls=%d stale=%t", calls.Load(), f.isStale())
		}
	})
}

// The tick-context hook runs once per tick and its context reaches the store.
func TestRun_TickContextCalledPerTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		var seen atomic.Int32
		f := newFixture(t,
			withOptions(WithTickContext(func(ctx context.Context) context.Context {
				return context.WithValue(ctx, ctxKey{}, calls.Add(1))
			})),
			withSource(func(s *memstore.Store[string, group]) Source[string, group] {
				return allSpy{s, func(ctx context.Context) {
					if v, ok := ctx.Value(ctxKey{}).(int32); ok {
						seen.Store(v)
					}
				}}
			}))
		f.prime(g("a", ""))
		stop := f.startRun()
		defer stop()
		for range 5 {
			tickOnce()
		}
		f.c.markStale(context.Background(), errors.New("x"))
		tickOnce()
		if n := calls.Load(); n != 6 {
			t.Fatalf("tick context calls = %d", n)
		}
		if v := seen.Load(); v != 6 {
			t.Fatalf("LoadAll saw tick value %v", v)
		}
	})
}

type allSpy struct {
	*memstore.Store[string, group]
	see func(context.Context)
}

func (s allSpy) LoadAll(ctx context.Context) ([]group, error) {
	s.see(ctx)
	return s.Store.LoadAll(ctx)
}

// SAFE-5: after an ambiguous outcome whose commit is still in flight, the loop's bulk
// load waits on the fence and installs state that includes the late commit.
func TestRebuild_WaitsForAbandonedCommit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.prime(g("a", ""))
		stop := f.startRun()
		defer stop()
		release := make(chan struct{})
		f.store.InjectWrite(memstore.WriteFault{Kind: memstore.AbandonedCommit, Release: release})
		if err := f.mutate(context.Background(), []string{"a"}, set(g("a", "late"))); err != memstore.ErrAmbiguous {
			t.Fatalf("Mutate = %v", err)
		}
		tickOnce() // the loop's LoadAll now waits on the exclusive fence
		if c := f.store.Calls(); c.LoadAll != 2 || !f.isStale() {
			t.Fatalf("calls=%+v stale=%t", c, f.isStale())
		}
		close(release)
		synctest.Wait()
		if f.isStale() {
			t.Fatal("rebuild did not install after the late commit")
		}
		if e, _ := f.get("a"); e.Owner != "late" {
			t.Fatalf("Get = %v; the install missed the late commit", e)
		}
	})
}

// NFR-6, section 7: a loop rebuild is bounded by its budget; an exclusive fence that
// cannot be acquired in time fails the rebuild and the cache stays stale.
func TestRebuild_Budget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, withOptions(WithRebuildBudget(3*time.Second)))
		f.prime(g("a", ""))
		stop := f.startRun()
		defer stop()
		release := make(chan struct{})
		f.store.InjectWrite(memstore.WriteFault{Kind: memstore.BlockWrite, Release: release})
		go func() { _ = f.store.Write(context.Background(), set(g("b", ""))) }() // holds the fence
		synctest.Wait()
		f.c.markStale(context.Background(), errors.New("x"))
		start := time.Now()
		time.Sleep(defaultRebuildInterval) // the tick starts its rebuild
		synctest.Wait()
		f.obs.take()
		time.Sleep(3 * time.Second) // the budget expires
		synctest.Wait()
		if got := f.obs.take(); !slices.Contains(got, "rebuild loop failed") {
			t.Fatalf("events after %v = %v", time.Since(start), got)
		}
		if !f.isStale() {
			t.Fatal("cache must stay stale")
		}
		close(release)
	})
}

// NFR-6: the refresh budget holds with the loop running.
func TestRefresh_Budget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.prime(g("a", ""))
		stop := f.startRun()
		defer stop()
		f.store.InjectLoadOne(memstore.ReadFault{Kind: memstore.BlockRead, Release: make(chan struct{})})
		start := time.Now()
		if err := f.mutate(context.Background(), []string{"a"}, set(g("a", "o"))); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d != defaultRefreshBudget {
			t.Fatalf("refresh took %v", d)
		}
		tickOnce()
		if f.isStale() {
			t.Fatal("loop did not recover after the refresh timeout")
		}
	})
}

func assertStaleAndUnlocked(t *testing.T, f *fixture) {
	t.Helper()
	if !f.c.mu.TryLock() {
		t.Fatal("lock held")
	}
	stale := f.c.stale
	f.c.mu.Unlock()
	if !stale {
		t.Fatal("cache not stale")
	}
}
