// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nobelk/surecache/internal/memstore"
)

// logRecord is one captured log record.
type logRecord struct {
	level slog.Level
	msg   string
	attrs map[string]slog.Value
	ctxV  any // the ctxKey value of the context passed to the logger
	text  string
}

// logSink is a slog.Handler that records every record.
type logSink struct {
	mu      sync.Mutex
	records []logRecord
	onLog   func()
}

func (h *logSink) Enabled(context.Context, slog.Level) bool { return true }
func (h *logSink) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *logSink) WithGroup(string) slog.Handler            { return h }

func (h *logSink) Handle(ctx context.Context, r slog.Record) error {
	rec := logRecord{level: r.Level, msg: r.Message, attrs: map[string]slog.Value{}, ctxV: ctx.Value(ctxKey{})}
	var text strings.Builder
	text.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value
		text.WriteString(" " + a.Key + "=" + a.Value.String())
		return true
	})
	rec.text = text.String()
	h.mu.Lock()
	h.records = append(h.records, rec)
	onLog := h.onLog
	h.mu.Unlock()
	if onLog != nil {
		onLog()
	}
	return nil
}

func (h *logSink) take() []logRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.records
	h.records = nil
	return out
}

func messages(rs []logRecord) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.msg
	}
	return out
}

// logFixture is a fixture whose logger records into the returned sink.
func logFixture(t *testing.T, opts ...fixtureOpt) (*fixture, *logSink) {
	t.Helper()
	sink := &logSink{}
	f := newFixture(t, append(opts, withOptions(WithLogger(slog.New(sink))))...)
	sink.take()
	return f, sink
}

// NFR-4: two ambiguous mutations in a row log one surecache_stale.
func TestLog_StaleOncePerTransition(t *testing.T) {
	f, sink := logFixture(t)
	f.prime(g("a", ""))
	for range 2 {
		f.store.InjectWrite(memstore.WriteFault{Kind: memstore.AmbiguousCommitted})
		_ = f.mutate(context.Background(), []string{"a"}, set(g("a", "o")))
	}
	rs := sink.take()
	if !slices.Equal(messages(rs), []string{"surecache_stale"}) {
		t.Fatalf("logs = %v", messages(rs))
	}
	r := rs[0]
	if r.level != slog.LevelWarn || r.attrs["cache"].String() != "groups" ||
		!strings.Contains(r.attrs["cause"].String(), "outcome unknown") {
		t.Fatalf("record = %+v", r)
	}
}

// Startup is not a trusted-to-stale transition: no surecache_stale, and a failed prime
// does not log surecache_rebuild_failed.
func TestLog_NothingAtStartupOrFailedPrime(t *testing.T) {
	f, sink := logFixture(t)
	f.store.InjectLoadAll(memstore.ReadFault{Kind: memstore.FailRead})
	_, _ = f.c.Prime(context.Background())
	f.prime(g("a", ""))
	if rs := sink.take(); len(rs) != 0 {
		t.Fatalf("logs = %v", messages(rs))
	}
}

// NFR-4: one surecache_rebuild_failed per stale episode, however many ticks fail.
func TestLog_RebuildFailedOncePerEpisode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, sink := logFixture(t)
		f.prime(g("a", ""))
		stop := f.startRun()
		defer stop()
		episode := func() {
			f.store.SetDown(memstore.ErrAmbiguous)
			f.c.markStale(context.Background(), errors.New("x"))
			for range 5 {
				tickOnce()
			}
			f.store.SetDown(nil)
			tickOnce()
		}
		episode()
		rs := sink.take()
		want := []string{"surecache_stale", "surecache_rebuild_failed", "surecache_recovered"}
		if !slices.Equal(messages(rs), want) {
			t.Fatalf("logs = %v, want %v", messages(rs), want)
		}
		if rs[1].level != slog.LevelError || rs[1].attrs["attempt"].Int64() != 1 ||
			rs[1].attrs["cause"].String() != memstore.ErrAmbiguous.Error() {
			t.Fatalf("rebuild_failed = %+v", rs[1])
		}
		episode()
		if got := messages(sink.take()); !slices.Equal(got, want) {
			t.Fatalf("second episode logs = %v", got)
		}
	})
}

// NFR-4: surecache_recovered once per episode, with the attempts and the stale time.
func TestLog_RecoveredOncePerEpisode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, sink := logFixture(t)
		f.prime(g("a", ""))
		stop := f.startRun()
		defer stop()
		f.store.InjectLoadAll(
			memstore.ReadFault{Kind: memstore.FailRead},
			memstore.ReadFault{Kind: memstore.FailRead},
		)
		f.c.markStale(context.Background(), errors.New("x"))
		for range 5 {
			tickOnce()
		}
		rs := sink.take()
		var rec *logRecord
		for i := range rs {
			if rs[i].msg == "surecache_recovered" {
				if rec != nil {
					t.Fatal("recovered logged twice")
				}
				rec = &rs[i]
			}
		}
		if rec == nil || rec.level != slog.LevelInfo {
			t.Fatalf("logs = %v", messages(rs))
		}
		if rec.attrs["attempts"].Int64() != 3 || rec.attrs["stale_for"].Duration() != 3*time.Second {
			t.Fatalf("attrs = %v", rec.attrs)
		}
	})
}

// NFR-4: reads never log.
func TestLog_NeverPerRead(t *testing.T) {
	f, sink := logFixture(t)
	f.prime(g("a", "o", "m"))
	read := func() {
		for range 10_000 {
			f.get("a")
			f.get("missing")
			f.member("m")
			f.member("none")
			f.c.List()
		}
	}
	read()
	f.c.markStale(context.Background(), errors.New("x"))
	sink.take()
	read()
	if rs := sink.take(); len(rs) != 0 {
		t.Fatalf("reads logged %d records", len(rs))
	}
}

// Section 9: handlers get the mutation's context on surecache_stale and the tick
// context on loop logs, so they can attach trace ids.
func TestLog_ContextPropagated(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, sink := logFixture(t, withOptions(WithTickContext(func(ctx context.Context) context.Context {
			return context.WithValue(ctx, ctxKey{}, "tick")
		})))
		f.prime(g("a", ""))
		stop := f.startRun()
		defer stop()
		f.store.InjectWrite(memstore.WriteFault{Kind: memstore.AmbiguousCommitted})
		_ = f.mutate(context.WithValue(context.Background(), ctxKey{}, "mutation"), []string{"a"}, set(g("a", "o")))
		f.store.InjectLoadAll(memstore.ReadFault{Kind: memstore.FailRead})
		tickOnce()
		tickOnce()
		got := map[string]any{}
		for _, r := range sink.take() {
			got[r.msg] = r.ctxV
		}
		want := map[string]any{"surecache_stale": "mutation", "surecache_rebuild_failed": "tick", "surecache_recovered": "tick"}
		for msg, v := range want {
			if got[msg] != v {
				t.Errorf("%s ctx value = %v, want %v", msg, got[msg], v)
			}
		}
	})
}

// Section 9: log records never carry entity contents or ids.
func TestLog_NoEntityContents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const secret = "SENTINEL-entity-content"
		f, sink := logFixture(t)
		f.prime(group{ID: secret + "-id", Name: secret, Owner: secret + "-owner", Members: []string{secret}})
		stop := f.startRun()
		defer stop()
		f.store.InjectWrite(memstore.WriteFault{Kind: memstore.AmbiguousCommitted})
		_ = f.mutate(context.Background(), []string{secret + "-id"}, set(group{ID: secret + "-id", Name: secret + "2"}))
		f.store.InjectLoadAll(memstore.ReadFault{Kind: memstore.FailRead})
		tickOnce()
		tickOnce()
		rs := sink.take()
		if len(rs) != 3 {
			t.Fatalf("logs = %v", messages(rs))
		}
		for _, r := range rs {
			if strings.Contains(r.text, "SENTINEL") {
				t.Fatalf("record leaks entity contents: %s", r.text)
			}
		}
	})
}

// reenterWithin calls Get and List on a separate goroutine and waits with a real-time
// timeout. If the caller held the cache lock, the reads would block and the timeout
// would fire instead of the test hanging.
func reenterWithin(t *testing.T, c *Cache[string, group]) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Get("a")
		c.List()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("reentrant read blocked: callback ran under the cache lock")
	}
}

// Section 3.3, internal invariant 1: observer callbacks run off the lock. Not under
// synctest: blocking on a mutex is not durably blocking in a bubble.
func TestObserver_CanReenterCache(t *testing.T) {
	obs := &reenterObserver{t: t}
	store := newGroupStore()
	store.Seed(g("a", ""))
	c, err := NewBuilder(groupSpec(10), Source[string, group](store)).
		Build(WithOutagePolicy(ServeWhileExact()), WithObserver(obs))
	if err != nil {
		t.Fatal(err)
	}
	obs.c = c
	if r, err := c.Prime(context.Background()); r != Installed {
		t.Fatal(r, err)
	}
	store.InjectWrite(memstore.WriteFault{Kind: memstore.AmbiguousCommitted})
	_ = c.Mutate(context.Background(), []string{"a"}, func() error {
		return store.Write(context.Background(), set(g("a", "o")))
	})
	c.Get("a")
	if obs.state.Load() == 0 || obs.counter.Load() == 0 {
		t.Fatalf("state=%d counter=%d reentries", obs.state.Load(), obs.counter.Load())
	}
}

type reenterObserver struct {
	NopObserver
	t              *testing.T
	c              *Cache[string, group]
	state, counter atomic.Int32
	inRead         atomic.Bool
}

func (o *reenterObserver) StaleChanged(bool, error) {
	if o.c != nil {
		reenterWithin(o.t, o.c)
		o.state.Add(1)
	}
}

func (o *reenterObserver) Rebuild(Trigger, RebuildResult) { o.counterReenter() }
func (o *reenterObserver) AmbiguousMutation()             { o.counterReenter() }

func (o *reenterObserver) Read(Method, ReadSource) {
	if o.inRead.CompareAndSwap(false, true) { // the reentrant reads report Read too
		o.counterReenter()
		o.inRead.Store(false)
	}
}

func (o *reenterObserver) counterReenter() {
	if o.c != nil {
		reenterWithin(o.t, o.c)
		o.counter.Add(1)
	}
}

// Section 3.3, internal invariant 1: log calls run off the lock.
func TestLogger_CanReenterCache(t *testing.T) {
	synctestFree := func(f *fixture, sink *logSink) {
		var n atomic.Int32
		sink.mu.Lock()
		sink.onLog = func() {
			reenterWithin(t, f.c)
			n.Add(1)
		}
		sink.mu.Unlock()
		f.store.InjectWrite(memstore.WriteFault{Kind: memstore.AmbiguousCommitted})
		_ = f.mutate(context.Background(), []string{"a"}, set(g("a", "o")))
		f.store.InjectLoadAll(memstore.ReadFault{Kind: memstore.FailRead})
		f.c.tick(context.Background()) // rebuild_failed: a direct log
		f.c.tick(context.Background()) // recovered: a state-event log
		if n.Load() != 3 {
			t.Fatalf("reentrant logs = %d, want 3", n.Load())
		}
	}
	f, sink := logFixture(t, concurrent())
	f.prime(g("a", ""))
	synctestFree(f, sink)
}

// NFR-4, section 9: every scenario raises exactly its events, in order.
func TestObserver_EventPerSignal(t *testing.T) {
	cases := []struct {
		name string
		run  func(f *fixture)
		want []string
	}{
		{"prime installed", func(f *fixture) {
			f.store.Seed(g("a", ""), g("b", ""))
			_, _ = f.c.Prime(context.Background())
		}, []string{"stale false false", "entities 2", "rebuild prime installed"}},
		{"prime failed", func(f *fixture) {
			f.store.InjectLoadAll(memstore.ReadFault{Kind: memstore.FailRead})
			_, _ = f.c.Prime(context.Background())
		}, []string{"rebuild prime failed"}},
		{"ambiguous mutation", func(f *fixture) {
			f.prime(g("a", ""))
			f.store.InjectWrite(memstore.WriteFault{Kind: memstore.AmbiguousCommitted})
			_ = f.mutate(context.Background(), []string{"a"}, set(g("a", "o")))
		}, []string{"ambiguous", "stale true true"}},
		{"refresh adds an entity", func(f *fixture) {
			f.prime(g("a", ""))
			_, _ = f.create(context.Background(), g("b", ""))
		}, []string{"entities 2"}},
		{"refresh removes an entity", func(f *fixture) {
			f.prime(g("a", ""), g("b", ""))
			_ = f.mutate(context.Background(), []string{"b"}, del("b"))
		}, []string{"entities 1"}},
		{"refresh updates an entity", func(f *fixture) {
			f.prime(g("a", ""))
			_ = f.mutate(context.Background(), []string{"a"}, set(g("a", "o")))
		}, nil},
		{"loop rebuild installed", func(f *fixture) {
			f.prime(g("a", ""))
			f.c.markStale(context.Background(), errors.New("x"))
			f.obs.take()
			f.c.tick(context.Background())
		}, []string{"stale false false", "entities 1", "rebuild loop installed"}},
		{"loop rebuild discarded", func(f *fixture) {
			f.prime(g("a", ""))
			f.c.markStale(context.Background(), errors.New("x"))
			f.obs.take()
			f.c.hooks = &hooks{afterLoadAll: func() { f.c.markStale(context.Background(), errors.New("y")) }}
			f.c.tick(context.Background())
		}, []string{"rebuild loop discarded"}},
		{"reads", func(f *fixture) {
			f.prime(g("a", "o"))
			f.get("a")
			f.owner("o")
			f.c.List()
		}, []string{"read get cache", "read lookup cache", "read list cache"}},
		{"deferred-read failure", func(f *fixture) {
			f.prime(g("a", ""))
			f.c.ObserveStoreResult(memstore.ErrAmbiguous)
		}, []string{"probe_failure", "unhealthy true"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, withPolicy(func() OutagePolicy {
				return DistrustAfterProbeFailures(healthPolicy(func(context.Context) error { return nil }, 1, 1))
			}))
			tc.run(f)
			if got := f.obs.take(); !slices.Equal(got, tc.want) {
				t.Fatalf("events = %v, want %v", got, tc.want)
			}
		})
	}
}

// D9: an observer that embeds NopObserver and overrides one method compiles and
// receives only that event.
func TestObserver_NopEmbedding(t *testing.T) {
	obs := &onlyAmbiguous{}
	store := newGroupStore()
	store.Seed(g("a", ""))
	c, err := NewBuilder(groupSpec(10), Source[string, group](store)).
		Build(WithOutagePolicy(ServeWhileExact()), WithObserver(obs))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Prime(context.Background())
	c.Get("a")
	_ = c.Mutate(context.Background(), []string{"a"}, func() error { return errors.New("lost") })
	if obs.n != 1 {
		t.Fatalf("AmbiguousMutation calls = %d", obs.n)
	}
}

type onlyAmbiguous struct {
	NopObserver
	n int
}

func (o *onlyAmbiguous) AmbiguousMutation() { o.n++ }
