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
