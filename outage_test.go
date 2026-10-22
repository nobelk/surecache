// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nobelk/surecache/internal/memstore"
)

// healthPolicy returns a valid probe policy over the store's Ping: 1 s interval, 300 ms
// timeout, memstore.ErrAmbiguous counts as unavailable.
func healthPolicy(probe func(context.Context) error, failAfter, recoverAfter int) HealthPolicy {
	return HealthPolicy{
		Probe:        probe,
		Interval:     time.Second,
		Timeout:      300 * time.Millisecond,
		Unavailable:  func(err error) bool { return errors.Is(err, memstore.ErrAmbiguous) },
		FailAfter:    failAfter,
		RecoverAfter: recoverAfter,
	}
}

// distrustFixture builds a cache whose probe pings the store through probe.
func distrustFixture(t *testing.T, failAfter, recoverAfter int, probe func(*memstore.Store[string, group]) func(context.Context) error) *fixture {
	t.Helper()
	var store *memstore.Store[string, group]
	// The prober and the loop use the cache at once, so the lock probe stays off.
	f := newFixture(t, concurrent(), withSource(func(s *memstore.Store[string, group]) Source[string, group] {
		store = s
		return s
	}), withPolicy(func() OutagePolicy {
		return DistrustAfterProbeFailures(healthPolicy(func(ctx context.Context) error {
			return probe(store)(ctx)
		}, failAfter, recoverAfter))
	}))
	return f
}

func ping(s *memstore.Store[string, group]) func(context.Context) error { return s.Ping }

func (f *fixture) isUnhealthy() bool {
	f.c.mu.RLock()
	defer f.c.mu.RUnlock()
	return f.c.unhealthy
}

func TestBuild_HealthPolicyValidation(t *testing.T) {
	probe := func(context.Context) error { return nil }
	valid := healthPolicy(probe, 2, 2)
	cases := []struct {
		name  string
		patch func(*HealthPolicy)
		want  string
	}{
		{"nil probe", func(p *HealthPolicy) { p.Probe = nil }, "Probe is nil"},
		{"nil unavailable", func(p *HealthPolicy) { p.Unavailable = nil }, "Unavailable is nil"},
		{"zero interval", func(p *HealthPolicy) { p.Interval = 0 }, "Interval must be positive"},
		{"zero timeout", func(p *HealthPolicy) { p.Timeout = 0 }, "Timeout must be positive"},
		{"timeout equals interval", func(p *HealthPolicy) { p.Timeout = p.Interval }, "Timeout must be less than Interval"},
		{"timeout above interval", func(p *HealthPolicy) { p.Timeout = 2 * p.Interval }, "Timeout must be less than Interval"},
		{"zero fail after", func(p *HealthPolicy) { p.FailAfter = 0 }, "FailAfter must be at least 1"},
		{"zero recover after", func(p *HealthPolicy) { p.RecoverAfter = 0 }, "RecoverAfter must be at least 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := valid
			tc.patch(&p)
			_, err := NewBuilder(groupSpec(10), Source[string, group](newGroupStore())).
				Build(WithOutagePolicy(DistrustAfterProbeFailures(p)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	t.Run("all at once", func(t *testing.T) {
		_, err := NewBuilder(groupSpec(10), Source[string, group](newGroupStore())).
			Build(WithOutagePolicy(DistrustAfterProbeFailures(HealthPolicy{})))
		for _, want := range []string{"Probe", "Unavailable", "Interval", "Timeout", "FailAfter", "RecoverAfter"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, missing %s", err, want)
			}
		}
	})
	t.Run("valid", func(t *testing.T) {
		if _, err := NewBuilder(groupSpec(10), Source[string, group](newGroupStore())).
			Build(WithOutagePolicy(DistrustAfterProbeFailures(valid))); err != nil {
			t.Fatal(err)
		}
	})
}

// Section 8: under ServeWhileExact a trusted cache keeps answering from memory while the
// store is dead, issuing no store calls.
func TestServeWhileExact_TrustedWithDeadStore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.prime(g("a", "o", "m"))
		stop := f.startRun()
		defer stop()
		f.store.SetDown(memstore.ErrAmbiguous)
		before := f.store.Calls()
		for range 5 {
			tickOnce()
			if e, a := f.get("a"); a != Found || e.Owner != "o" {
				t.Fatalf("Get = %v, %v", e, a)
			}
			if _, a := f.member("m"); a != Found {
				t.Fatal("Lookup deferred")
			}
			if _, a := f.c.List(); a != Found {
				t.Fatal("List deferred")
			}
		}
		if after := f.store.Calls(); after != before {
			t.Fatalf("store calls %+v -> %+v", before, after)
		}
	})
}

// Section 8: the first mutation in an outage defers the reads it affects; its ambiguous
// failure makes the cache stale until a rebuild succeeds after the store returns.
func TestServeWhileExact_FirstMutationDefersThenStale(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, concurrent())
		f.prime(g("a", "o", "m"), g("b", ""))
		stop := f.startRun()
		defer stop()

		release := make(chan struct{})
		f.store.InjectWrite(memstore.WriteFault{Kind: memstore.BlockWrite, Release: release})
		done := make(chan error, 1)
		go func() {
			done <- f.c.Mutate(context.Background(), []string{"a"}, func() error {
				err := f.store.Write(context.Background(), set(g("a", "o2")))
				if err == nil {
					err = memstore.ErrAmbiguous // the connection died during COMMIT
				}
				return err
			})
		}()
		synctest.Wait()
		if _, a := f.get("a"); a != Defer {
			t.Fatalf("affected Get = %v", a)
		}
		if _, a := f.c.List(); a != Defer {
			t.Fatalf("List = %v", a)
		}
		if _, a := f.get("b"); a != Found {
			t.Fatalf("unaffected Get = %v", a)
		}
		f.store.SetDown(memstore.ErrAmbiguous)
		close(release)
		if err := <-done; err != memstore.ErrAmbiguous {
			t.Fatalf("Mutate = %v", err)
		}
		if _, a := f.get("b"); a != Defer {
			t.Fatal("stale cache must defer everything")
		}
		for range 3 {
			tickOnce()
			if !f.isStale() {
				t.Fatal("rebuilt while the store was dead")
			}
		}
		f.store.SetDown(nil)
		tickOnce()
		if f.isStale() {
			t.Fatal("no rebuild after the store returned")
		}
		if e, _ := f.get("a"); e.Owner != "o2" {
			t.Fatalf("Get = %v", e)
		}
	})
}

// Section 8: exactly FailAfter consecutive failures set unhealthy; reads then defer.
func TestDistrust_SetsUnhealthyAfterExactlyFailAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := distrustFixture(t, 3, 1, ping)
		f.prime(g("a", ""))
		stop := f.startRun()
		defer stop()
		f.store.SetDown(memstore.ErrAmbiguous)
		for i := range 2 {
			tickOnce()
			if _, a := f.get("a"); a != Found {
				t.Fatalf("after %d failures Get = %v", i+1, a)
			}
		}
		tickOnce()
		if !f.isUnhealthy() {
			t.Fatal("not unhealthy after FailAfter failures")
		}
		if f.isStale() {
			t.Fatal("unhealthy must not set stale")
		}
		if _, a := f.get("a"); a != Defer {
			t.Fatalf("Get = %v", a)
		}
		got := f.obs.take()
		if n := countOf(got, "probe_failure"); n != 3 || !slices.Contains(got, "unhealthy true") {
			t.Fatalf("events = %v", got)
		}
	})
}

// Section 8: detection takes at most FailAfter × Interval + Timeout of virtual time.
func TestDistrust_DetectionBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hung := make(chan struct{})
		var dead atomic.Bool
		f := distrustFixture(t, 3, 1, func(*memstore.Store[string, group]) func(context.Context) error {
			return func(ctx context.Context) error {
				if !dead.Load() {
					return nil
				}
				select { // a hung store: the probe returns only at its deadline
				case <-hung:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		})
		f.prime(g("a", ""))
		stop := f.startRun()
		defer stop()
		dead.Store(true)
		time.Sleep(3*time.Second + 300*time.Millisecond - time.Nanosecond)
		synctest.Wait()
		if f.isUnhealthy() {
			t.Fatal("unhealthy before the third probe timed out")
		}
		time.Sleep(time.Nanosecond) // FailAfter × Interval + Timeout
		synctest.Wait()
		if !f.isUnhealthy() {
			t.Fatal("not unhealthy at the detection bound")
		}
	})
}

// Section 8: a probe that exceeds its timeout counts as a failure.
func TestDistrust_ProbeTimeoutCountsAsFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := distrustFixture(t, 1, 1, func(*memstore.Store[string, group]) func(context.Context) error {
			return func(context.Context) error {
				time.Sleep(500 * time.Millisecond) // ignores ctx, then reports success
				return nil
			}
		})
		f.prime(g("a", ""))
		stop := f.startRun()
		defer stop()
		time.Sleep(time.Second + 500*time.Millisecond)
		synctest.Wait()
		if !f.isUnhealthy() {
			t.Fatal("a probe past its timeout did not count as a failure")
		}
	})
}

// Section 8: errors Unavailable rejects never count.
func TestDistrust_NonUnavailableErrorsIgnored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := distrustFixture(t, 1, 1, func(*memstore.Store[string, group]) func(context.Context) error {
			return func(context.Context) error { return memstore.ErrNotFound }
		})
		f.prime(g("a", ""))
		stop := f.startRun()
		defer stop()
		for range 5 {
			tickOnce()
		}
		if f.isUnhealthy() || countOf(f.obs.take(), "probe_failure") != 0 {
			t.Fatal("a business error counted as a probe failure")
		}
	})
}

// scriptedProbe returns a probe that answers from results in order, then nil.
func scriptedProbe(results ...error) func(*memstore.Store[string, group]) func(context.Context) error {
	var i atomic.Int32
	return func(*memstore.Store[string, group]) func(context.Context) error {
		return func(context.Context) error {
			n := int(i.Add(1)) - 1
			if n < len(results) {
				return results[n]
			}
			return nil
		}
	}
}

// Section 8: a success resets the failure streak.
func TestDistrust_SuccessResetsFailStreak(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		down := memstore.ErrAmbiguous
		f := distrustFixture(t, 2, 1, scriptedProbe(down, nil, down, nil, down))
		f.prime(g("a", ""))
		stop := f.startRun()
		defer stop()
		for range 6 {
			tickOnce()
		}
		if f.isUnhealthy() {
			t.Fatal("non-consecutive failures set unhealthy")
		}
	})
}

// Section 8: a failure resets the success streak.
func TestDistrust_FailureResetsOkStreak(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		down := memstore.ErrAmbiguous
		f := distrustFixture(t, 1, 2, scriptedProbe(down, nil, down, nil, down, nil, nil))
		f.prime(g("a", ""))
		stop := f.startRun()
		defer stop()
		for range 6 {
			tickOnce()
			if !f.isUnhealthy() {
				t.Fatal("non-consecutive successes cleared unhealthy")
			}
		}
		tickOnce()
		if f.isUnhealthy() {
			t.Fatal("RecoverAfter consecutive successes did not clear unhealthy")
		}
	})
}

// Section 8, two gates: a rebuild clears stale but not unhealthy; only RecoverAfter
// probe successes clear it.
func TestDistrust_RebuildDoesNotClearUnhealthy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := distrustFixture(t, 1, 2, ping)
		f.prime(g("a", ""))
		stop := f.startRun()
		defer stop()
		f.store.SetDown(memstore.ErrAmbiguous)
		_ = f.mutate(context.Background(), []string{"a"}, set(g("a", "o")))
		tickOnce()
		if !f.isStale() || !f.isUnhealthy() {
			t.Fatalf("stale=%t unhealthy=%t", f.isStale(), f.isUnhealthy())
		}
		f.store.SetDown(nil)
		f.c.mu.Lock()
		f.c.generation++
		captured := f.c.generation
		f.c.mu.Unlock()
		if r, _ := f.c.rebuild(context.Background(), captured, TriggerLoop); r != Installed {
			t.Fatalf("rebuild = %v", r)
		}
		if f.isStale() || !f.isUnhealthy() {
			t.Fatalf("after rebuild stale=%t unhealthy=%t", f.isStale(), f.isUnhealthy())
		}
		if _, a := f.get("a"); a != Defer {
			t.Fatal("reads must defer while unhealthy")
		}
		tickOnce()
		if !f.isUnhealthy() {
			t.Fatal("one success cleared unhealthy; RecoverAfter is 2")
		}
		tickOnce()
		if f.isUnhealthy() {
			t.Fatal("RecoverAfter successes did not clear unhealthy")
		}
		if _, a := f.get("a"); a != Found {
			t.Fatal("reads must answer once both gates clear")
		}
		if got := f.obs.take(); !slices.Contains(got, "unhealthy false") {
			t.Fatalf("events = %v", got)
		}
	})
}

// Section 8, clarification 10: deferred-read errors count toward FailAfter only when
// Unavailable accepts them.
func TestObserveStoreResult_CountsUnavailableOnly(t *testing.T) {
	f := distrustFixture(t, 2, 1, ping)
	f.prime(g("a", ""))
	f.c.ObserveStoreResult(memstore.ErrNotFound)
	f.c.ObserveStoreResult(memstore.ErrAmbiguous)
	if f.isUnhealthy() {
		t.Fatal("one failure set unhealthy")
	}
	f.c.ObserveStoreResult(memstore.ErrAmbiguous)
	if !f.isUnhealthy() {
		t.Fatal("FailAfter deferred-read failures did not set unhealthy")
	}
	if got := f.obs.take(); countOf(got, "probe_failure") != 2 {
		t.Fatalf("events = %v", got)
	}
}

// Section 8: ObserveStoreResult is a no-op under ServeWhileExact.
func TestObserveStoreResult_NoOpUnderServeWhileExact(t *testing.T) {
	f := newFixture(t)
	f.prime(g("a", ""))
	for range 10 {
		f.c.ObserveStoreResult(memstore.ErrAmbiguous)
	}
	if f.isUnhealthy() || len(f.obs.take()) != 0 {
		t.Fatal("ObserveStoreResult acted under ServeWhileExact")
	}
}

// Clarification 10: deferred-read successes neither recover nor reset the failure
// streak.
func TestObserveStoreResult_SuccessDoesNotRecover(t *testing.T) {
	f := distrustFixture(t, 2, 1, ping)
	f.prime(g("a", ""))
	f.c.ObserveStoreResult(memstore.ErrAmbiguous)
	f.c.ObserveStoreResult(nil)
	f.c.ObserveStoreResult(memstore.ErrAmbiguous)
	if !f.isUnhealthy() {
		t.Fatal("a deferred-read success reset the failure streak")
	}
	for range 5 {
		f.c.ObserveStoreResult(nil)
	}
	if !f.isUnhealthy() {
		t.Fatal("deferred-read successes cleared unhealthy")
	}
}

// Section 6.5: a rebuild blocked on a slow store never delays health detection.
func TestProber_NotDelayedBySlowRebuild(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := distrustFixture(t, 3, 1, ping)
		f.prime(g("a", ""))
		stop := f.startRun()
		defer stop()
		release := make(chan struct{})
		f.store.InjectLoadAll(memstore.ReadFault{Kind: memstore.BlockRead, Release: release})
		f.c.markStale(context.Background(), errors.New("x"))
		tickOnce() // the rebuild now blocks in LoadAll
		f.store.SetDown(memstore.ErrAmbiguous)
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if !f.isUnhealthy() {
			t.Fatal("probes were delayed by the blocked rebuild")
		}
		close(release)
	})
}

// Run waits for the prober before returning.
func TestRun_WaitsForProber(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls, after atomic.Int32
		var returned atomic.Bool
		f := distrustFixture(t, 1, 1, func(*memstore.Store[string, group]) func(context.Context) error {
			return func(context.Context) error {
				calls.Add(1)
				if returned.Load() {
					after.Add(1)
				}
				return nil
			}
		})
		f.prime(g("a", ""))
		stop := f.startRun()
		tickOnce()
		tickOnce()
		stop()
		returned.Store(true)
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if calls.Load() != 2 || after.Load() != 0 {
			t.Fatalf("calls=%d after return=%d", calls.Load(), after.Load())
		}
	})
}

// A panicking probe counts as a failure and does not stop the prober.
func TestProber_PanicCountsAsFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := distrustFixture(t, 2, 1, func(*memstore.Store[string, group]) func(context.Context) error {
			return func(context.Context) error { panic("probe") }
		})
		f.prime(g("a", ""))
		stop := f.startRun()
		defer stop()
		tickOnce()
		tickOnce()
		if !f.isUnhealthy() {
			t.Fatal("panicking probes did not count")
		}
	})
}

func countOf(events []string, want string) int {
	n := 0
	for _, e := range events {
		if e == want {
			n++
		}
	}
	return n
}

// A probe cut short because Run is stopping is not a failure.
func TestProber_StopIgnoresInFlightProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := distrustFixture(t, 1, 1, func(*memstore.Store[string, group]) func(context.Context) error {
			return func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			}
		})
		f.prime(g("a", ""))
		stop := f.startRun()
		time.Sleep(1100 * time.Millisecond) // a probe is in flight
		synctest.Wait()
		stop()
		if f.isUnhealthy() || countOf(f.obs.take(), "probe_failure") != 0 {
			t.Fatal("the interrupted probe counted as a failure")
		}
	})
}
