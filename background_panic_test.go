// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nobelk/surecache/internal/memstore"
)

type faultObserver struct {
	NopObserver
	stale        func(bool, error)
	entities     func(int)
	unhealthy    func(bool)
	probeFailure func()
	rebuild      func(Trigger, RebuildResult)
}

func (o faultObserver) StaleChanged(v bool, err error) {
	if o.stale != nil {
		o.stale(v, err)
	}
}

func (o faultObserver) Entities(n int) {
	if o.entities != nil {
		o.entities(n)
	}
}

func (o faultObserver) UnhealthyChanged(v bool) {
	if o.unhealthy != nil {
		o.unhealthy(v)
	}
}

func (o faultObserver) ProbeFailure() {
	if o.probeFailure != nil {
		o.probeFailure()
	}
}

func (o faultObserver) Rebuild(t Trigger, r RebuildResult) {
	if o.rebuild != nil {
		o.rebuild(t, r)
	}
}

type faultLogHandler struct{ handle func(slog.Record) }

func (faultLogHandler) Enabled(context.Context, slog.Level) bool        { return true }
func (h faultLogHandler) Handle(_ context.Context, r slog.Record) error { h.handle(r); return nil }
func (h faultLogHandler) WithAttrs([]slog.Attr) slog.Handler            { return h }
func (h faultLogHandler) WithGroup(string) slog.Handler                 { return h }

func catchPanic(fn func()) (value any) {
	defer func() { value = recover() }()
	fn()
	return nil
}

// Clarification 12: a failing consumer must not discard subsequent state events,
// or prevent an independent consumer from observing the same transition.
func TestEmitter_PanicAttemptsRemainingConsumersInOrder(t *testing.T) {
	for _, failing := range []string{"observer", "logger"} {
		t.Run(failing, func(t *testing.T) {
			f := newFixture(t)
			first := errors.New("first consumer panic")
			var delivered []string
			f.c.cfg.observer = faultObserver{
				stale: func(bool, error) {
					delivered = append(delivered, "stale")
					if failing == "observer" {
						panic(first)
					}
				},
				entities: func(int) { delivered = append(delivered, "entities") },
			}
			f.c.cfg.logger = slog.New(faultLogHandler{handle: func(slog.Record) {
				delivered = append(delivered, "log")
				if failing == "logger" {
					panic(first)
				}
			}})
			got := catchPanic(func() {
				f.c.withWriteLock(t.Context(), func(*eventBuf) {
					f.c.queueStateLocked(stateEvent{kind: stateStale, on: true, cause: errors.New("stale")})
					f.c.queueStateLocked(stateEvent{kind: stateEntities, n: 2})
				})
			})
			if got != first {
				t.Fatalf("panic = %v, want original %v", got, first)
			}
			if !slices.Equal(delivered, []string{"log", "stale", "entities"}) {
				t.Fatalf("delivery = %v; independent and subsequent consumers were lost", delivered)
			}
			f.c.mu.RLock()
			remaining := len(f.c.stateQueue)
			f.c.mu.RUnlock()
			if remaining != 0 {
				t.Fatalf("undispatched events = %d", remaining)
			}
		})
	}
}

func TestEmitter_CounterPanicStillDrainsState(t *testing.T) {
	f := newFixture(t)
	first := errors.New("counter panic")
	var delivered []string
	f.c.cfg.observer = faultObserver{
		rebuild:      func(Trigger, RebuildResult) { delivered = append(delivered, "rebuild"); panic(first) },
		probeFailure: func() { delivered = append(delivered, "probe") },
		entities:     func(int) { delivered = append(delivered, "entities"); panic("later panic") },
	}
	got := catchPanic(func() {
		f.c.withWriteLock(t.Context(), func(buf *eventBuf) {
			buf.add(counterEvent{kind: counterRebuild})
			buf.add(counterEvent{kind: counterProbeFailure})
			f.c.queueStateLocked(stateEvent{kind: stateEntities, n: 1})
		})
	})
	if got != first || !slices.Equal(delivered, []string{"rebuild", "probe", "entities"}) {
		t.Fatalf("panic=%v delivery=%v", got, delivered)
	}
}

// Section 6.5: callback failures must not escape the prober goroutine, even if
// reporting the failure also panics. Subsequent scheduled probes still execute.
func TestHealth_ObserverPanicsDoNotStopRun(t *testing.T) {
	for _, failing := range []string{"counter", "state", "both"} {
		t.Run(failing, func(t *testing.T) {
			testHealthObserverPanic(t, failing)
		})
	}
}

func testHealthObserverPanic(t *testing.T, failing string) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		var probes, failures, unhealthy, stale atomic.Int32
		hp := healthPolicy(func(context.Context) error { probes.Add(1); return memstore.ErrAmbiguous }, 1, 1)
		f := newFixture(t, concurrent(), withPolicy(func() OutagePolicy { return DistrustAfterProbeFailures(hp) }), withOptions(WithRebuildInterval(10*time.Second)))
		f.prime(g("a", ""))
		f.c.cfg.observer = faultObserver{
			probeFailure: func() {
				failures.Add(1)
				if failing != "state" {
					panic("probe observer panic")
				}
			},
			unhealthy: func(bool) {
				unhealthy.Add(1)
				if failing != "counter" {
					panic("health observer panic")
				}
			},
			stale: func(bool, error) { stale.Add(1); panic("failure-report observer panic") },
		}
		stop := f.startRun()
		defer stop()
		tickOnce()
		tickOnce()
		if probes.Load() != 2 || failures.Load() != 2 || unhealthy.Load() != 1 || stale.Load() != 1 {
			t.Fatalf("probes=%d failures=%d unhealthy=%d stale=%d", probes.Load(), failures.Load(), unhealthy.Load(), stale.Load())
		}
		if !f.isStale() || !f.isUnhealthy() {
			t.Fatal("callback failure did not fail closed")
		}
		if _, a := f.get("a"); a != Defer {
			t.Fatalf("Get = %v", a)
		}
	})
}

// Deferred event consumers must not replace the panic already unwinding Prime.
func TestPrime_FirstConsumerPanicPreserved(t *testing.T) {
	f := newFixture(t)
	f.store.Seed(g("a", ""))
	first := errors.New("state observer panic")
	var delivered []string
	f.c.cfg.observer = faultObserver{
		stale:    func(bool, error) { delivered = append(delivered, "state"); panic(first) },
		entities: func(int) { delivered = append(delivered, "entities") },
		rebuild:  func(Trigger, RebuildResult) { delivered = append(delivered, "rebuild"); panic("later counter panic") },
	}
	got := catchPanic(func() { _, _ = f.c.Prime(t.Context()) })
	if got != first || !slices.Equal(delivered, []string{"state", "entities", "rebuild"}) {
		t.Fatalf("panic=%v delivery=%v", got, delivered)
	}
}

func TestMutate_OriginalPanicSurvivesReporting(t *testing.T) {
	for _, where := range []string{"op", "key", "cacheable"} {
		t.Run(where, func(t *testing.T) {
			f := newFixture(t)
			f.install(g("a", ""))
			first := errors.New("original panic")
			f.c.cfg.logger = slog.New(faultLogHandler{handle: func(slog.Record) { panic("reporting panic") }})
			if where == "key" {
				f.c.spec.Key = func(group) string { panic(first) }
			}
			if where == "cacheable" {
				f.c.spec.Cacheable = func(group) bool { panic(first) }
			}
			got := catchPanic(func() {
				_ = f.c.Mutate(t.Context(), []string{"a"}, func() error {
					if where == "op" {
						panic(first)
					}
					return f.store.Write(t.Context(), set(g("a", "new")))
				})
			})
			if got != first {
				t.Fatalf("panic=%v, want original %v", got, first)
			}
			if !f.isStale() {
				t.Fatal("panic did not mark stale")
			}
			if p, n, w := f.marks(); p+n+w != 0 {
				t.Fatalf("marks leaked: %d %d %d", p, n, w)
			}
		})
	}
}

// A rebuild may have installed before a counter observer panics. Run must
// distrust that iteration, deliver its remaining state events, and retry later.
func TestRun_RebuildObserverPanicRetries(t *testing.T) {
	f := newFixture(t)
	f.prime(g("a", ""))
	f.c.markStale(t.Context(), errors.New("setup"))
	var rebuilt, entities, distrusted int
	f.c.cfg.observer = faultObserver{
		rebuild:  func(Trigger, RebuildResult) { rebuilt++; panic("rebuild observer panic") },
		entities: func(int) { entities++ },
		stale: func(on bool, _ error) {
			if on {
				distrusted++
				panic("recovery observer panic")
			}
		},
	}
	if got := catchPanic(func() { f.c.tick(t.Context()) }); got != nil {
		t.Fatalf("background panic escaped: %v", got)
	}
	if !f.isStale() || rebuilt != 1 || entities != 1 || distrusted != 1 {
		t.Fatalf("stale=%v rebuilt=%d entities=%d distrusted=%d", f.isStale(), rebuilt, entities, distrusted)
	}
	f.c.cfg.observer = f.obs
	f.c.tick(t.Context())
	if _, answer := f.get("a"); answer != Found {
		t.Fatalf("retry did not recover: %v", answer)
	}
}

func TestRun_RecoveryReportingPanicContained(t *testing.T) {
	for _, failing := range []string{"stale log", "failure log"} {
		t.Run(failing, func(t *testing.T) {
			f := newFixture(t)
			f.prime(g("a", ""))
			if failing == "stale log" {
				var ticks int
				f.c.cfg.tickContext = func(ctx context.Context) context.Context {
					ticks++
					if ticks == 1 {
						panic("tick context panic")
					}
					return ctx
				}
			} else {
				f.c.markStale(t.Context(), errors.New("setup"))
				f.store.InjectLoadAll(memstore.ReadFault{Kind: memstore.FailRead})
			}
			var reports int
			f.c.cfg.logger = slog.New(faultLogHandler{handle: func(r slog.Record) {
				if r.Level >= slog.LevelWarn {
					reports++
					panic("reporting logger panic")
				}
			}})
			if got := catchPanic(func() { f.c.tick(t.Context()) }); got != nil {
				t.Fatalf("background panic escaped: %v", got)
			}
			if !f.isStale() || reports == 0 {
				t.Fatalf("stale=%v reports=%d", f.isStale(), reports)
			}
			if got := catchPanic(func() { f.c.tick(t.Context()) }); got != nil {
				t.Fatalf("retry panic escaped: %v", got)
			}
			if _, a := f.get("a"); a != Found {
				t.Fatalf("next tick did not recover: %v", a)
			}
		})
	}
}
