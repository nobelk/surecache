// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// SAFE-9: Build publishes the initial stale state, so an observer attached at Build
// reports stale before prime.
func TestObserver_InitialStatePublished(t *testing.T) {
	obs := &recorder{}
	b := NewBuilder(groupSpec(10), Source[string, group](newGroupStore()))
	if _, err := b.Build(WithOutagePolicy(testPolicy{}), WithObserver(obs)); err != nil {
		t.Fatal(err)
	}
	if got := obs.take(); !slices.Equal(got, []string{"stale true false", "entities 0"}) {
		t.Fatalf("events = %v", got)
	}
}

// NFR-4: state events reach the observer in transition order even when the goroutine
// holding the later transition reaches delivery first.
func TestEmitter_DeliversInTransitionOrder(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", ""))

	first := true
	var mu sync.Mutex
	staleWaiting, releaseStale := make(chan struct{}), make(chan struct{})
	f.c.hooks = &hooks{beforeDrain: func() {
		mu.Lock()
		isFirst := first
		first = false
		mu.Unlock()
		if isFirst { // the stale transition's goroutine stalls before delivery
			close(staleWaiting)
			<-releaseStale
		}
	}}

	staleDone := make(chan struct{})
	go func() {
		defer close(staleDone)
		f.c.withWriteLock(t.Context(), func(*eventBuf) {
			f.c.markStaleLocked(t.Context(), errors.New("boom"))
		})
	}()
	<-staleWaiting
	// The later transition: trust returns, and this goroutine delivers first.
	if err := installForTest(f.c, []group{g("a", ""), g("b", "")}); err != nil {
		t.Fatal(err)
	}
	close(releaseStale)
	<-staleDone

	want := []string{"stale true true", "stale false false", "entities 2"}
	if got := f.obs.take(); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// Observer callbacks may call read methods: state events are delivered off the cache
// lock. A deadlock would trip the real-time timeout instead of hanging.
func TestEmitter_ReadFromCallbackAllowed(t *testing.T) {
	obs := &reentrant{}
	b := NewBuilder(groupSpec(10), Source[string, group](newGroupStore()))
	c, err := b.Build(WithOutagePolicy(testPolicy{}), WithObserver(obs))
	if err != nil {
		t.Fatal(err)
	}
	obs.c = c
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := installForTest(c, []group{g("a", "")}); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("observer callback blocked: called under the cache lock")
	}
	if obs.reads == 0 {
		t.Fatal("callback did not read")
	}
}

type reentrant struct {
	NopObserver
	c     *Cache[string, group]
	reads int
}

func (r *reentrant) StaleChanged(bool, error) {
	if r.c != nil {
		r.c.Get("a")
		r.c.List()
		r.reads++
	}
}

func TestEventBuf_OverflowsToSlice(t *testing.T) {
	var buf eventBuf
	for range 6 {
		buf.add(counterEvent{kind: counterAmbiguous})
	}
	if buf.n != 4 || len(buf.extra) != 2 {
		t.Fatalf("n=%d extra=%d", buf.n, len(buf.extra))
	}
	obs := &recorder{}
	c := &Cache[string, group]{cfg: config{observer: obs}}
	buf.add(counterEvent{kind: counterRebuild, trigger: TriggerLoop, result: Failed})
	buf.add(counterEvent{kind: counterProbeFailure})
	c.emit(&buf)
	got := obs.take()
	if len(got) != 8 || got[6] != "rebuild loop failed" || got[7] != "probe_failure" {
		t.Fatalf("events = %v", got)
	}
}

func TestNopObserver_AllMethods(*testing.T) {
	var o Observer = NopObserver{}
	o.Read(MethodGet, SourceCache)
	o.StaleChanged(true, nil)
	o.UnhealthyChanged(true)
	o.Entities(1)
	o.Rebuild(TriggerPrime, Installed)
	o.AmbiguousMutation()
	o.ProbeFailure()
}

func TestDeliverState_Unhealthy(t *testing.T) {
	obs := &recorder{}
	c := &Cache[string, group]{cfg: config{observer: obs}}
	c.deliverState(stateEvent{kind: stateUnhealthy, on: true})
	if got := obs.take(); !slices.Equal(got, []string{"unhealthy true"}) {
		t.Fatalf("events = %v", got)
	}
}
