// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"fmt"

	"github.com/nobelk/surecache/internal/boundedmap"
)

// token identifies the builder, and so the cache, an index handle belongs to. It has a
// field so that distinct tokens never share an address.
type token struct{ _ byte }

// Index is a typed handle to a secondary index, valid only for the cache that built it.
// The zero Index belongs to no cache.
type Index[ID comparable, K comparable] struct {
	owner *token
	pos   int
}

// anyIndex is the type-erased view of one secondary index that the cache keeps in a
// slice. Every method takes whole entities, so key values never need boxing.
type anyIndex[ID comparable, E any] interface {
	// label returns the index name, for error messages.
	label() string
	// fresh returns an empty index with the same key function and capacity.
	fresh(hint int) anyIndex[ID, E]
	// insertBulk indexes every key of e under id, failing on a key another entity holds
	// (ErrContract) or on capacity (ErrOverCapacity).
	insertBulk(id ID, e E) error
	// size returns the number of keys held.
	size() int

	// holdsKeys reports whether e yields any key.
	holdsKeys(e E) bool
	// conflicts reports whether a key of e is held by another entity that has no
	// refresh in progress (internal invariant 4).
	conflicts(id ID, e E, refreshes map[ID]*window) bool
	// fits reports whether replacing prev (if had) with e as id stays within capacity.
	fits(id ID, prev E, had bool, e E) bool
	// remove deletes prev's keys that still name id (SAFE-7).
	remove(id ID, prev E)
	// install indexes every key of e to id. The caller checked fits.
	install(id ID, e E)
}

// index is one unique secondary index: key K to the primary key of its one holder.
type index[ID comparable, E any, K comparable] struct {
	name     string
	keys     func(E) []K
	capacity int
	m        *boundedmap.Map[K, ID]
}

func newIndex[ID comparable, E any, K comparable](name string, keys func(E) []K, capacity, hint int) *index[ID, E, K] {
	return &index[ID, E, K]{name: name, keys: keys, capacity: capacity, m: boundedmap.NewSized[K, ID](capacity, hint)}
}

func (ix *index[ID, E, K]) label() string { return ix.name }

func (ix *index[ID, E, K]) fresh(hint int) anyIndex[ID, E] {
	return newIndex[ID, E](ix.name, ix.keys, ix.capacity, hint)
}

func (ix *index[ID, E, K]) insertBulk(id ID, e E) error {
	for _, k := range ix.keys(e) {
		if holder, ok := ix.m.Get(k); ok {
			if holder == id {
				continue // the entity lists the key twice
			}
			return fmt.Errorf("%w: two entities hold one key of index %q", ErrContract, ix.name)
		}
		if !ix.m.Put(k, id) {
			return fmt.Errorf("%w: index %q", ErrOverCapacity, ix.name)
		}
	}
	return nil
}

func (ix *index[ID, E, K]) size() int { return ix.m.Len() }

func (ix *index[ID, E, K]) holdsKeys(e E) bool { return len(ix.keys(e)) > 0 }

func (ix *index[ID, E, K]) conflicts(id ID, e E, refreshes map[ID]*window) bool {
	for _, k := range ix.keys(e) {
		holder, ok := ix.m.Get(k)
		if !ok || holder == id {
			continue
		}
		if w := refreshes[holder]; w == nil || w.active == 0 {
			return true
		}
	}
	return false
}

// fits projects the index size after the refresh: the current size, minus the keys of
// prev that still name id (removal deletes exactly those), plus the distinct incoming
// keys that are absent or name id. An incoming key held by another refreshing entity is
// overwritten in place and does not grow the index.
func (ix *index[ID, E, K]) fits(id ID, prev E, had bool, e E) bool {
	projected := ix.m.Len()
	if had {
		for _, k := range distinct(ix.keys(prev)) {
			if holder, ok := ix.m.Get(k); ok && holder == id {
				projected--
			}
		}
	}
	for _, k := range distinct(ix.keys(e)) {
		if holder, ok := ix.m.Get(k); !ok || holder == id {
			projected++
		}
	}
	return projected <= ix.capacity
}

func (ix *index[ID, E, K]) remove(id ID, prev E) {
	for _, k := range ix.keys(prev) {
		if holder, ok := ix.m.Get(k); ok && holder == id {
			ix.m.Delete(k)
		}
	}
}

func (ix *index[ID, E, K]) install(id ID, e E) {
	for _, k := range ix.keys(e) {
		ix.m.Put(k, id)
	}
}

func distinct[K comparable](keys []K) []K {
	if len(keys) < 2 {
		return keys
	}
	return dedupe(keys)
}
