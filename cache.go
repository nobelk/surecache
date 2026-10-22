// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"context"
	"sync"

	"github.com/nobelk/surecache/internal/boundedmap"
)

// Cache is a write-invalidated cache in front of an authoritative store. It answers reads
// from memory only when it can prove the answer matches committed state; otherwise reads
// return Defer and the caller asks the store.
//
// A Cache starts stale. Call Prime once at startup and fail startup unless it returns
// Installed, then run Run in the background. Route every store write through Mutate or
// Create.
type Cache[ID comparable, E any] struct {
	spec  Spec[ID, E]
	src   Source[ID, E]
	cfg   config
	owner *token
	hooks *hooks // nil in production (D17)

	// mu guards every field below. No store call, log or observer callback runs under
	// it (design section 6.6).
	mu        sync.RWMutex
	entities  *boundedmap.Map[ID, E]
	indexes   []anyIndex[ID, E]
	directory []ID // sorted by spec.Compare
	// stale is snapshot distrust; only an installed rebuild clears it.
	stale bool
	// unhealthy is the outage policy's health distrust; only its recovery rule clears it.
	unhealthy  bool
	pending    map[ID]int
	inFlight   int
	generation uint64
	refreshes  map[ID]*window
	stateQueue []stateEvent
	failStreak int // consecutive health failures (outage policy)
	okStreak   int // consecutive probe successes (outage policy)
	episode    episode
	primed     bool // an earlier Prime returned Installed
	running    bool // a Run is active

	// emitMu admits one deliverer of state events at a time (see drain).
	emitMu sync.Mutex
}

func newCache[ID comparable, E any](spec Spec[ID, E], src Source[ID, E], cfg config, owner *token, indexes []anyIndex[ID, E]) *Cache[ID, E] {
	return &Cache[ID, E]{
		spec:      spec,
		src:       src,
		cfg:       cfg,
		owner:     owner,
		entities:  boundedmap.New[ID, E](spec.Capacity),
		indexes:   indexes,
		stale:     true,
		pending:   map[ID]int{},
		refreshes: map[ID]*window{},
	}
}

// markStaleLocked records snapshot distrust. It bumps the generation on every call, so a
// rebuild that captured before it discards (internal invariant 2), and queues a
// StaleChanged event only on the transition. The caller holds c.mu for writing.
func (c *Cache[ID, E]) markStaleLocked(ctx context.Context, cause error) {
	c.generation++
	if c.stale {
		return
	}
	c.stale = true
	c.beginEpisodeLocked()
	c.queueStateLocked(stateEvent{ctx: ctx, kind: stateStale, on: true, cause: cause})
}

// withWriteLock runs fn under the write lock with the panic rule (internal invariant
// 11): if fn panics, the cache is marked stale before the lock is released, so no reader
// sees half-modified maps without the stale flag. Events fn buffers are emitted after
// unlock, also when the panic continues.
func (c *Cache[ID, E]) withWriteLock(ctx context.Context, fn func(buf *eventBuf)) {
	var buf eventBuf
	c.mu.Lock()
	done := false
	defer func() {
		if !done {
			c.markStaleLocked(ctx, errPanicked)
		}
		c.mu.Unlock()
		c.emit(&buf)
	}()
	fn(&buf)
	done = true
}
