// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"context"
	"fmt"
	"slices"
)

// Mutate runs op with ids marked pending, settles the cache on op's error, and returns
// that error unchanged (FUN-8). ids must name every entity op can change (PRE-4); a move
// between two entities names both. An empty ids returns ErrNoIDs without running op.
//
// While op runs and until its post-commit refresh installs, reads of the marked entities,
// index misses and List defer (SAFE-2). On success the cache re-reads every id under a
// context detached from ctx's cancellation (values survive) and bounded by the refresh
// budget; a refresh failure marks the cache stale but is not reported (FUN-6). A
// nothing-committed error (Spec.CommittedNothing) leaves the cache as is (FUN-7). Any
// other error, including a context error, marks the whole cache stale (SAFE-4).
//
// If op, or a Spec callback during the refresh, panics, the cache is marked stale, every
// mark is released and the panic continues unchanged (SAFE-11).
func (c *Cache[ID, E]) Mutate(ctx context.Context, ids []ID, op func() error) error {
	if len(ids) == 0 {
		return ErrNoIDs
	}
	ids = dedupe(ids)
	var err error
	c.guard(ctx, ids, func() {
		err = op()
		c.afterOpHook()
		c.settle(ctx, ids, err)
	})
	return err
}

// Create is for an operation that inserts exactly one previously nonexistent entity and
// modifies no existing entity; never use it for an upsert (PRE-4). It runs op under the
// in-flight count, so index misses and List defer while it runs, refreshes the returned
// id on success, and returns op's results unchanged. Settlement and panics follow Mutate.
func (c *Cache[ID, E]) Create(ctx context.Context, op func() (ID, error)) (ID, error) {
	var id ID
	var err error
	c.guard(ctx, nil, func() {
		id, err = op()
		c.afterOpHook()
		var ids []ID
		if err == nil {
			ids = []ID{id}
		}
		c.settle(ctx, ids, err)
	})
	return id, err
}

func (c *Cache[ID, E]) afterOpHook() {
	if h := c.hooks; h != nil && h.afterOp != nil {
		h.afterOp()
	}
}

// guard marks ids pending and counts one in-flight mutation (internal invariant 3), runs
// apply, and releases the marks only after apply returns, so a refresh installs before a
// reader can hit the entity again. If apply does not finish (a panic), the cache is
// marked stale before the marks are released (SAFE-11).
func (c *Cache[ID, E]) guard(ctx context.Context, ids []ID, apply func()) {
	c.mu.Lock()
	for _, id := range ids {
		c.pending[id]++
	}
	c.inFlight++
	c.mu.Unlock()
	if h := c.hooks; h != nil && h.afterMark != nil {
		h.afterMark(ctx, ids)
	}

	settled := false
	defer func() {
		if h := c.hooks; h != nil && h.beforeRelease != nil {
			h.beforeRelease(ctx, ids)
		}
		var buf eventBuf
		c.mu.Lock()
		if !settled {
			c.markStaleLocked(ctx, ErrUnsettled)
		}
		for _, id := range ids {
			if c.pending[id]--; c.pending[id] <= 0 {
				delete(c.pending, id)
			}
		}
		c.inFlight--
		c.mu.Unlock()
		c.emit(&buf)
	}()
	apply()
	settled = true
}

// settle applies a mutation's outcome: refresh on success, nothing on a nothing-committed
// error, mark stale on anything else.
func (c *Cache[ID, E]) settle(ctx context.Context, ids []ID, err error) {
	switch {
	case err == nil:
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.refreshBudget)
		defer cancel()
		c.refreshBatch(rctx, ids)
	case c.spec.CommittedNothing(err):
		// FUN-7: the cached copy is still exact.
	default:
		cause := fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)
		c.withWriteLock(ctx, func(buf *eventBuf) {
			buf.add(counterEvent{kind: counterAmbiguous})
			c.markStaleLocked(ctx, cause)
		})
	}
}

// dedupe removes duplicate ids, keeping first occurrences in order. A linear scan beats a
// map for the few ids a mutation usually names.
func dedupe[ID comparable](ids []ID) []ID {
	out := make([]ID, 0, len(ids))
	if len(ids) <= 8 {
		for _, id := range ids {
			if !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
		return out
	}
	seen := make(map[ID]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; !dup {
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}
