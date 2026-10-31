// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"context"
	"fmt"
	"slices"
)

// window is one entity's refresh window. gen orders same-entity refreshes: only the
// newest may install. active counts refreshes in progress; the window is dropped at zero.
type window struct {
	gen    uint64
	active int
}

// refreshBatch re-reads ids after their mutation committed (design section 6.3). It bumps
// the generation so an in-progress rebuild discards, does nothing more on a stale cache
// (FUN-4), and otherwise opens every window before the first read (internal invariant
// 10).
//
// The batch also marks every id pending while its windows are open. An older refresh
// that a newer window discards leaves the entity to this batch, so the entity must
// defer until this batch installs. Mutate's ids are pending already; Create's id is not,
// because it is unknown until op returns.
func (c *Cache[ID, E]) refreshBatch(ctx context.Context, ids []ID) {
	captured := make([]uint64, len(ids))
	stale, opened, completed := false, false, false
	// Register before opening windows: withWriteLock emits queued events on its
	// way out, and an observer panic must not strand batch-owned marks.
	defer func() {
		if opened {
			c.endRefresh(ctx, ids, completed)
		}
	}()
	c.withWriteLock(ctx, func(*eventBuf) {
		c.generation++
		if stale = c.stale; stale {
			return
		}
		for i, id := range ids {
			captured[i] = c.beginRefreshLocked(id)
			c.pending[id]++
		}
		opened = true
	})
	if stale {
		return
	}
	if h := c.hooks; h != nil && h.beforeRefresh != nil {
		h.beforeRefresh(ids)
	}
	for i, id := range ids {
		c.refreshOne(ctx, id, captured[i])
	}
	completed = true
}

func (c *Cache[ID, E]) beginRefreshLocked(id ID) uint64 {
	w := c.refreshes[id]
	if w == nil {
		w = &window{}
		c.refreshes[id] = w
	}
	w.gen++
	w.active++
	return w.gen
}

func (c *Cache[ID, E]) endRefresh(ctx context.Context, ids []ID, completed bool) {
	c.withWriteLock(ctx, func(*eventBuf) {
		// A create has no guard-owned pending id. On panic, these may be the last
		// marks protecting an old value left by a discarded refresh. Distrust it
		// atomically before releasing any marks (SAFE-11).
		if !completed {
			c.markStaleLocked(ctx, ErrUnsettled)
		}
		for _, id := range ids {
			if w := c.refreshes[id]; w != nil {
				if w.active--; w.active <= 0 {
					delete(c.refreshes, id)
				}
			}
			if c.pending[id]--; c.pending[id] <= 0 {
				delete(c.pending, id)
			}
		}
	})
}

// refreshOne reads id off the lock and installs the result unless a newer same-entity
// refresh owns the window (SAFE-6).
func (c *Cache[ID, E]) refreshOne(ctx context.Context, id ID, captured uint64) {
	c.mu.RLock()
	stale := c.stale
	c.mu.RUnlock()
	if stale {
		return // internal invariant 7: no store read for a stale cache
	}
	if h := c.hooks; h != nil && h.beforeLoadOne != nil {
		h.beforeLoadOne(id)
	}
	e, err := c.src.LoadOne(ctx, id)
	if h := c.hooks; h != nil && h.afterLoadOne != nil {
		h.afterLoadOne(id)
	}
	present := err == nil
	switch {
	case present:
		if c.spec.Key(e) != id {
			c.markStale(ctx, fmt.Errorf("%w: LoadOne returned another entity", ErrContract))
			return
		}
	case c.src.NotFound(err):
		// The entity authoritatively does not exist (section 4.4): remove it.
	default:
		c.markStale(ctx, fmt.Errorf("surecache: refresh: %w", err))
		return
	}
	c.withWriteLock(ctx, func(*eventBuf) {
		c.installRefreshLocked(ctx, id, e, present, captured)
	})
}

// installRefreshLocked applies one refresh result. Every check runs before any map
// changes, so a refused install leaves the maps untouched. The caller holds c.mu under
// the panic rule.
func (c *Cache[ID, E]) installRefreshLocked(ctx context.Context, id ID, e E, present bool, captured uint64) {
	if c.stale || c.refreshes[id].gen != captured {
		return // a newer same-entity refresh, or a rebuild, owns this entity now
	}
	cacheable := present && c.spec.Cacheable(e)
	if present && !cacheable && slices.ContainsFunc(c.indexes, func(ix anyIndex[ID, E]) bool { return ix.holdsKeys(e) }) {
		c.markStaleLocked(ctx, fmt.Errorf("%w: non-cacheable entity holds index keys", ErrContract))
		return
	}
	prev, had := c.entities.Get(id)
	if cacheable {
		if c.cfg.cloneOnInstall {
			e = c.spec.Clone(e)
		}
		if err := c.checkInstallLocked(id, prev, had, e); err != nil {
			c.markStaleLocked(ctx, err)
			return
		}
	}
	if had {
		c.removeLocked(id, prev)
	}
	if cacheable {
		c.installLocked(id, e)
	}
	if had != cacheable {
		c.queueStateLocked(stateEvent{kind: stateEntities, n: c.entities.Len()})
	}
}

// checkInstallLocked runs the insert-side conflict check and the capacity projection for
// installing e as id. An incoming key held by another entity is allowed only while that
// entity has a refresh in progress, which re-indexes it (internal invariant 4).
func (c *Cache[ID, E]) checkInstallLocked(id ID, prev E, had bool, e E) error {
	for _, ix := range c.indexes {
		if ix.conflicts(id, e, c.refreshes) {
			return fmt.Errorf("%w: index %q", ErrKeyConflict, ix.label())
		}
	}
	if !had && c.entities.Len() >= c.entities.Cap() {
		return fmt.Errorf("%w: primary map", ErrOverCapacity)
	}
	for _, ix := range c.indexes {
		if !ix.fits(id, prev, had, e) {
			return fmt.Errorf("%w: index %q", ErrOverCapacity, ix.label())
		}
	}
	return nil
}

// removeLocked de-indexes prev with the current-holder check (SAFE-7) and removes id
// from the primary map and the directory.
func (c *Cache[ID, E]) removeLocked(id ID, prev E) {
	for _, ix := range c.indexes {
		ix.remove(id, prev)
	}
	c.entities.Delete(id)
	if i, ok := slices.BinarySearchFunc(c.directory, id, c.spec.Compare); ok {
		c.directory = slices.Delete(c.directory, i, i+1)
	}
}

// installLocked indexes e, then stores it, then adds id to the directory. The caller
// checked capacity, so no put is refused.
func (c *Cache[ID, E]) installLocked(id ID, e E) {
	for _, ix := range c.indexes {
		ix.install(id, e)
	}
	c.entities.Put(id, e)
	if i, ok := slices.BinarySearchFunc(c.directory, id, c.spec.Compare); !ok {
		c.directory = slices.Insert(c.directory, i, id)
	}
}

// markStale marks the cache stale from outside the lock.
func (c *Cache[ID, E]) markStale(ctx context.Context, cause error) {
	c.withWriteLock(ctx, func(*eventBuf) { c.markStaleLocked(ctx, cause) })
}
