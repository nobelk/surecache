// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"context"
	"strconv"
)

// Spec describes the cached entity. Every field is required; Build rejects a missing one.
type Spec[ID comparable, E any] struct {
	// Name identifies the cache in logs and metric labels.
	Name string
	// Key returns an entity's primary key.
	Key func(E) ID
	// Clone returns a deep copy. Every value the cache serves is a clone (SAFE-10).
	Clone func(E) E
	// Cacheable reports whether an entity is held. A non-cacheable entity (for example a
	// soft-deleted row) still exists in the store and must yield no index keys (PRE-3).
	Cacheable func(E) bool
	// Compare is a total order on primary keys; List returns ids in this order.
	Compare func(a, b ID) int
	// Capacity bounds the primary map. The cache refuses on overflow and never evicts.
	Capacity int
	// CommittedNothing reports whether a mutation error guarantees that no change
	// committed. Any other error is an ambiguous outcome and marks the cache stale.
	CommittedNothing func(error) bool
}

// Source is the store side the cache needs. Both reads go to the authoritative writer,
// never to a replica.
type Source[ID comparable, E any] interface {
	// LoadOne returns a consistent snapshot of id, even when it is not Cacheable. A
	// successful result satisfies Key(e) == id.
	LoadOne(ctx context.Context, id ID) (E, error)
	// NotFound reports whether a LoadOne error means the entity authoritatively does not
	// exist.
	NotFound(err error) bool
	// LoadAll returns every Cacheable entity once, from one snapshot ordered after every
	// earlier mutation (the fence), or an over-capacity error; never a subset. Context
	// errors are returned unwrapped.
	LoadAll(ctx context.Context) ([]E, error)
}

// Answer is a tri-state lookup result. Defer means "ask the store", never "absent".
type Answer uint8

const (
	// Defer means the cache cannot vouch for an answer; the caller must ask the store.
	// It is the zero value, so an unset Answer never reads as Found or Absent.
	Defer Answer = iota
	// Found means the cache answered from memory.
	Found
	// Absent is an authoritative negative: the cache is trusted and no mutation is in
	// flight.
	Absent
)

// String returns "defer", "found" or "absent".
func (a Answer) String() string {
	switch a {
	case Defer:
		return "defer"
	case Found:
		return "found"
	case Absent:
		return "absent"
	}
	return "Answer(" + strconv.Itoa(int(a)) + ")"
}

// RebuildResult tells a caller whether trust was actually restored. Branch on it, never
// on a nil error alone: Discarded comes with a nil error and leaves the cache stale.
type RebuildResult uint8

const (
	// Installed means the bulk load replaced the cache and the cache is trusted.
	Installed RebuildResult = iota
	// Discarded means newer state committed while the bulk load read; the cache stays
	// stale.
	Discarded
	// Failed means the bulk load or its contract checks failed; the cache stays stale.
	Failed
)

// String returns "installed", "discarded" or "failed".
func (r RebuildResult) String() string {
	switch r {
	case Installed:
		return "installed"
	case Discarded:
		return "discarded"
	case Failed:
		return "failed"
	}
	return "RebuildResult(" + strconv.Itoa(int(r)) + ")"
}
