// SPDX-License-Identifier: Apache-2.0

package surecache

// Reads take one read lock, do one map lookup and clone after unlocking; installed
// entities are immutable, because an install replaces a map value and never mutates it.
// Every read reports exactly one Read event, after unlocking.

// Get returns a clone of the entity and Found, or the zero value and Defer. It never
// returns Absent: primary absence is not authoritative, because non-cacheable entities
// exist in the store but not in the cache (SAFE-3).
func (c *Cache[ID, E]) Get(id ID) (E, Answer) {
	c.mu.RLock()
	e, hit := c.entities.Get(id)
	var src ReadSource
	switch {
	case c.stale || c.unhealthy:
		src = SourceStale
	case len(c.pending) > 0 && c.pending[id] > 0: // skip the lookup when idle
		src = SourcePending
	case !hit:
		src = SourceMiss
	default:
		src = SourceCache
	}
	c.mu.RUnlock()
	c.cfg.observer.Read(MethodGet, src)
	if src != SourceCache {
		var zero E
		return zero, Defer
	}
	return c.spec.Clone(e), Found
}

// Lookup answers a secondary-key query: the holder's id and Found, Absent (an
// authoritative negative: the cache is trusted and no mutation is in flight), or Defer.
// A handle from another cache returns Defer and ErrForeignIndex; it never answers Absent.
func Lookup[ID comparable, E any, K comparable](c *Cache[ID, E], idx Index[ID, K], key K) (ID, Answer, error) {
	var zero ID
	if idx.owner == nil || idx.owner != c.owner {
		return zero, Defer, ErrForeignIndex
	}
	c.mu.RLock()
	id, hit := c.indexes[idx.pos].(*index[ID, E, K]).m.Get(key)
	var src ReadSource
	answer := Defer
	switch {
	case c.stale || c.unhealthy:
		src = SourceStale
	case hit && len(c.pending) > 0 && c.pending[id] > 0:
		src = SourcePending
	case !hit && c.inFlight > 0:
		// A create or update in flight may be adding the key (SAFE-2).
		src = SourceInFlight
	case hit:
		src, answer = SourceCache, Found
	default:
		src, answer = SourceCache, Absent
	}
	c.mu.RUnlock()
	c.cfg.observer.Read(MethodLookup, src)
	if answer != Found {
		id = zero
	}
	return id, answer, nil
}

// List returns a detached copy of the directory, sorted by Spec.Compare, and Found, or
// nil and Defer. It defers while any mutation is in flight, since a create may be adding
// an id the cache cannot see yet (SAFE-2, SAFE-3).
func (c *Cache[ID, E]) List() ([]ID, Answer) {
	c.mu.RLock()
	var ids []ID
	var src ReadSource
	switch {
	case c.stale || c.unhealthy:
		src = SourceStale
	case c.inFlight > 0:
		src = SourceInFlight
	default:
		src = SourceCache
		ids = make([]ID, len(c.directory))
		copy(ids, c.directory)
	}
	c.mu.RUnlock()
	c.cfg.observer.Read(MethodList, src)
	if src != SourceCache {
		return nil, Defer
	}
	return ids, Found
}
