// SPDX-License-Identifier: Apache-2.0

// Package boundedmap provides a capacity-bounded map that refuses new keys at capacity
// instead of evicting. It has no background goroutine and no expiry, and it is not safe
// for concurrent use; the cache guards it with its own lock.
package boundedmap

import "iter"

// Map is a Go map with a fixed capacity. A Put of a new key at capacity stores nothing
// and reports false; overwriting an existing key always succeeds.
type Map[K comparable, V any] struct {
	m        map[K]V
	capacity int
}

// New returns an empty map that holds at most capacity keys.
func New[K comparable, V any](capacity int) *Map[K, V] {
	return &Map[K, V]{m: map[K]V{}, capacity: capacity}
}

// NewSized is New with a size hint for the underlying map. The hint is clamped to the
// capacity, so a large hint never allocates past what the map may hold.
func NewSized[K comparable, V any](capacity, hint int) *Map[K, V] {
	hint = max(0, min(hint, capacity))
	return &Map[K, V]{m: make(map[K]V, hint), capacity: capacity}
}

// Put stores v under k. It reports false, storing nothing, when k is new and the map is
// at capacity.
func (b *Map[K, V]) Put(k K, v V) bool {
	if _, exists := b.m[k]; !exists && len(b.m) >= b.capacity {
		return false
	}
	b.m[k] = v
	return true
}

// Get returns the value stored under k.
func (b *Map[K, V]) Get(k K) (V, bool) {
	v, ok := b.m[k]
	return v, ok
}

// Delete removes k, freeing its slot. Deleting an absent key does nothing.
func (b *Map[K, V]) Delete(k K) {
	delete(b.m, k)
}

// Len returns the number of keys held.
func (b *Map[K, V]) Len() int {
	return len(b.m)
}

// Cap returns the capacity.
func (b *Map[K, V]) Cap() int {
	return b.capacity
}

// All iterates over every key and value in unspecified order.
func (b *Map[K, V]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		for k, v := range b.m {
			if !yield(k, v) {
				return
			}
		}
	}
}
