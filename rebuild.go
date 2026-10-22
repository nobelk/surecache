// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"context"
	"fmt"
	"slices"

	"github.com/nobelk/surecache/internal/boundedmap"
)

// Prime runs the first rebuild, bounded by the prime budget. Anything but Installed must
// fail startup (SAFE-9): Discarded comes with a nil error and leaves the cache stale, so
// branch on the result, never on the error alone. Context errors are returned unwrapped.
//
// Call Prime once at startup, before Run. After an Installed result a second call returns
// Failed and ErrAlreadyPrimed; after Failed or Discarded it may be retried. Concurrent
// calls are not supported, though they are safe: the generation check discards all but
// the last.
func (c *Cache[ID, E]) Prime(ctx context.Context) (RebuildResult, error) {
	c.mu.Lock()
	if c.primed {
		c.mu.Unlock()
		return Failed, ErrAlreadyPrimed
	}
	c.generation++
	captured := c.generation
	c.mu.Unlock()
	pctx, cancel := context.WithTimeout(ctx, c.cfg.primeBudget)
	defer cancel()
	return c.rebuild(pctx, captured, TriggerPrime)
}

// rebuild replaces the whole cache from a fenced bulk load (design section 6.4). It
// installs only if the generation is unchanged since captured; otherwise newer state
// committed while it read and it discards. The Rebuild event is reported after unlock,
// also when a callback panics.
func (c *Cache[ID, E]) rebuild(ctx context.Context, captured uint64, trigger Trigger) (RebuildResult, error) {
	result := Failed
	defer func() { c.cfg.observer.Rebuild(trigger, result) }()
	all, err := c.src.LoadAll(ctx)
	if h := c.hooks; h != nil && h.afterLoadAll != nil {
		h.afterLoadAll()
	}
	c.withWriteLock(ctx, func(*eventBuf) {
		if err != nil {
			c.markStaleLocked(ctx, err)
			return
		}
		if c.generation != captured {
			result = Discarded
			return
		}
		var s *snapshot[ID, E]
		if s, err = c.buildAside(all); err != nil {
			c.markStaleLocked(ctx, err)
			return
		}
		c.installSnapshotLocked(ctx, s)
		if trigger == TriggerPrime {
			c.primed = true
		}
		result = Installed
	})
	return result, err
}

// installSnapshotLocked swaps in s and restores trust. Every open refresh window is
// bumped, so a refresh that read before the capture discards: the snapshot already holds
// its commit (internal invariant 5).
func (c *Cache[ID, E]) installSnapshotLocked(ctx context.Context, s *snapshot[ID, E]) {
	c.swapInLocked(s)
	for _, w := range c.refreshes {
		w.gen++
	}
	if c.stale {
		c.stale = false
		c.queueStateLocked(stateEvent{ctx: ctx, kind: stateStale, on: false})
	}
	c.queueStateLocked(stateEvent{ctx: ctx, kind: stateEntities, n: s.entities.Len()})
}

// snapshot is a complete set of maps built aside from a bulk load, ready to swap in.
type snapshot[ID comparable, E any] struct {
	entities  *boundedmap.Map[ID, E]
	indexes   []anyIndex[ID, E]
	directory []ID
}

// buildAside builds new maps from a bulk load without touching live state. It installs
// every entity or nothing (SAFE-8): it fails with ErrOverCapacity when a map refuses a
// key, and with ErrContract for a duplicate primary key, a non-cacheable entity, or two
// entities holding one unique key (SAFE-12, PRE-2). Caller callbacks run here, so the
// caller runs it under the panic rule.
func (c *Cache[ID, E]) buildAside(all []E) (*snapshot[ID, E], error) {
	s := &snapshot[ID, E]{
		entities:  boundedmap.NewSized[ID, E](c.spec.Capacity, len(all)),
		indexes:   make([]anyIndex[ID, E], len(c.indexes)),
		directory: make([]ID, 0, min(len(all), c.spec.Capacity)),
	}
	for i, ix := range c.indexes {
		s.indexes[i] = ix.fresh(len(all))
	}
	for _, e := range all {
		if !c.spec.Cacheable(e) {
			return nil, fmt.Errorf("%w: bulk load returned a non-cacheable entity", ErrContract)
		}
		id := c.spec.Key(e)
		if _, dup := s.entities.Get(id); dup {
			return nil, fmt.Errorf("%w: bulk load returned a primary key twice", ErrContract)
		}
		if c.cfg.cloneOnInstall {
			e = c.spec.Clone(e)
		}
		if !s.entities.Put(id, e) {
			return nil, fmt.Errorf("%w: primary map", ErrOverCapacity)
		}
		for _, ix := range s.indexes {
			if err := ix.insertBulk(id, e); err != nil {
				return nil, err
			}
		}
		s.directory = append(s.directory, id)
	}
	slices.SortFunc(s.directory, c.spec.Compare)
	return s, nil
}

// swapInLocked replaces the live maps with s. The caller holds c.mu for writing.
func (c *Cache[ID, E]) swapInLocked(s *snapshot[ID, E]) {
	c.entities = s.entities
	c.indexes = s.indexes
	c.directory = s.directory
}
