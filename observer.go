// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"context"
	"strconv"
	"time"
)

// Observer receives the cache's signals (design section 9). The cache calls every method
// off its lock, possibly from several goroutines at once, so implementations must be safe
// for concurrent use and must not block.
//
// State events (StaleChanged, UnhealthyChanged, Entities) are delivered one at a time in
// the order the transitions happened. Counter events (Read, Rebuild, AmbiguousMutation,
// ProbeFailure) are delivered without ordering guarantees.
//
// A callback may call the cache's read methods (Get, Lookup, List). It must not call
// Mutate, Create, Prime or Run, which deliver state events themselves and would deadlock.
//
// Implementations must embed NopObserver, so that methods added in later versions do not
// break them.
type Observer interface {
	// Read reports who answered a read, or why it deferred.
	Read(m Method, s ReadSource)
	// StaleChanged reports a change of the stale flag. cause is nil when trust returns
	// and for the initial stale state published by Build.
	StaleChanged(stale bool, cause error)
	// UnhealthyChanged reports a change of the outage policy's health distrust.
	UnhealthyChanged(unhealthy bool)
	// Entities reports the number of entities held.
	Entities(n int)
	// Rebuild reports the result of a prime or loop rebuild.
	Rebuild(t Trigger, r RebuildResult)
	// AmbiguousMutation reports a mutation whose outcome is unknown.
	AmbiguousMutation()
	// ProbeFailure reports a health probe failure or a counted deferred-read failure.
	ProbeFailure()
}

// NopObserver implements Observer with no-ops. Observer implementations must embed it.
type NopObserver struct{}

var _ Observer = NopObserver{}

// Read does nothing.
func (NopObserver) Read(Method, ReadSource) {}

// StaleChanged does nothing.
func (NopObserver) StaleChanged(bool, error) {}

// UnhealthyChanged does nothing.
func (NopObserver) UnhealthyChanged(bool) {}

// Entities does nothing.
func (NopObserver) Entities(int) {}

// Rebuild does nothing.
func (NopObserver) Rebuild(Trigger, RebuildResult) {}

// AmbiguousMutation does nothing.
func (NopObserver) AmbiguousMutation() {}

// ProbeFailure does nothing.
func (NopObserver) ProbeFailure() {}

// Method names a read method.
type Method uint8

const (
	// MethodGet is Cache.Get.
	MethodGet Method = iota
	// MethodLookup is Lookup.
	MethodLookup
	// MethodList is Cache.List.
	MethodList
)

// String returns the metric label value: "get", "lookup" or "list".
func (m Method) String() string {
	switch m {
	case MethodGet:
		return "get"
	case MethodLookup:
		return "lookup"
	case MethodList:
		return "list"
	}
	return "Method(" + strconv.Itoa(int(m)) + ")"
}

// ReadSource says who answered a read, or why it deferred.
type ReadSource uint8

const (
	// SourceCache means the cache answered (Found, or an authoritative Absent).
	SourceCache ReadSource = iota
	// SourceStale means the read deferred because the cache is stale or unhealthy.
	SourceStale
	// SourcePending means the read deferred because its entity has a mutation in flight.
	SourcePending
	// SourceInFlight means an index miss or List deferred because a mutation is in flight.
	SourceInFlight
	// SourceMiss means a primary-key miss deferred; primary absence is never
	// authoritative.
	SourceMiss
)

// String returns the metric label value: "cache", "stale", "pending", "in_flight" or
// "miss".
func (s ReadSource) String() string {
	switch s {
	case SourceCache:
		return "cache"
	case SourceStale:
		return "stale"
	case SourcePending:
		return "pending"
	case SourceInFlight:
		return "in_flight"
	case SourceMiss:
		return "miss"
	}
	return "ReadSource(" + strconv.Itoa(int(s)) + ")"
}

// Trigger says what started a rebuild.
type Trigger uint8

const (
	// TriggerPrime is the startup rebuild run by Prime.
	TriggerPrime Trigger = iota
	// TriggerLoop is a rebuild run by the background loop in Run.
	TriggerLoop
)

// String returns the metric label value: "prime" or "loop".
func (t Trigger) String() string {
	switch t {
	case TriggerPrime:
		return "prime"
	case TriggerLoop:
		return "loop"
	}
	return "Trigger(" + strconv.Itoa(int(t)) + ")"
}

// counterKind names an order-insensitive event.
type counterKind uint8

const (
	counterRebuild counterKind = iota
	counterAmbiguous
	counterProbeFailure
)

// counterEvent is an order-insensitive event raised under the lock and delivered after
// unlock.
type counterEvent struct {
	kind    counterKind
	trigger Trigger
	result  RebuildResult
}

// eventBuf collects counter events raised under the lock. It is a value on the caller's
// stack; the fixed array covers every path without allocating.
type eventBuf struct {
	fixed [4]counterEvent
	n     int
	extra []counterEvent
}

func (b *eventBuf) add(ev counterEvent) {
	if b.n < len(b.fixed) {
		b.fixed[b.n] = ev
		b.n++
		return
	}
	b.extra = append(b.extra, ev)
}

// stateKind names an ordered state event.
type stateKind uint8

const (
	stateStale stateKind = iota
	stateUnhealthy
	stateEntities
)

// stateEvent is a state transition, queued under the lock in transition order and
// delivered by drain. ctx is the context of the operation that caused it, for logging.
type stateEvent struct {
	ctx     context.Context
	cause   error
	kind    stateKind
	on      bool
	initial bool // Build's initial state: published, not logged
	n       int

	// recovered marks the stale-to-trusted transition that ends a stale episode;
	// attempts and staleFor feed surecache_recovered.
	recovered bool
	attempts  int
	staleFor  time.Duration
}

// queueStateLocked appends a state event. The caller holds c.mu for writing.
func (c *Cache[ID, E]) queueStateLocked(ev stateEvent) {
	c.stateQueue = append(c.stateQueue, ev)
}

// emit delivers buffered counter events, then drains the state queue. The caller must not
// hold c.mu.
func (c *Cache[ID, E]) emit(buf *eventBuf) {
	for i := range buf.n {
		c.deliverCounter(buf.fixed[i])
	}
	for _, ev := range buf.extra {
		c.deliverCounter(ev)
	}
	c.drain()
}

func (c *Cache[ID, E]) deliverCounter(ev counterEvent) {
	switch ev.kind {
	case counterRebuild:
		c.cfg.observer.Rebuild(ev.trigger, ev.result)
	case counterAmbiguous:
		c.cfg.observer.AmbiguousMutation()
	case counterProbeFailure:
		c.cfg.observer.ProbeFailure()
	}
}

// drain delivers queued state events in transition order. emitMu admits one deliverer at
// a time; it loops until the queue is empty, so an event queued by a concurrent caller
// while this one delivers is never left behind.
func (c *Cache[ID, E]) drain() {
	if h := c.hooks; h != nil && h.beforeDrain != nil {
		h.beforeDrain()
	}
	c.emitMu.Lock()
	defer c.emitMu.Unlock()
	for {
		c.mu.Lock()
		batch := c.stateQueue
		c.stateQueue = nil
		c.mu.Unlock()
		if len(batch) == 0 {
			return
		}
		for _, ev := range batch {
			c.deliverState(ev)
		}
	}
}

func (c *Cache[ID, E]) deliverState(ev stateEvent) {
	c.logState(ev)
	switch ev.kind {
	case stateStale:
		c.cfg.observer.StaleChanged(ev.on, ev.cause)
	case stateUnhealthy:
		c.cfg.observer.UnhealthyChanged(ev.on)
	case stateEntities:
		c.cfg.observer.Entities(ev.n)
	}
}
