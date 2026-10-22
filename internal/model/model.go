// SPDX-License-Identifier: Apache-2.0

// Package model is the reference model and history checker behind the randomized
// concurrency test (design section 11.1, milestone 07). It replays the store's commit
// log and checks every answer the cache gave against committed state:
//
//   - Linearizability against committed state (SAFE-1, SAFE-3): a Found or Absent answer
//     must be correct for the store's state at some version between the store version
//     read before the call and the one read after it.
//   - Required deferrals (SAFE-2): a read whose whole interval lies inside a mutation's
//     window must defer when it is a hit on an affected id, any index miss, or a List,
//     even though the old value still matches committed state.
//
// The package knows nothing about the cache; tests translate their answers into it.
package model

import (
	"cmp"
	"fmt"
	"reflect"
	"slices"
)

// Answer mirrors the cache's tri-state answer.
type Answer uint8

// Answers.
const (
	Defer Answer = iota
	Found
	Absent
)

// Op is a read operation.
type Op uint8

// Read operations.
const (
	Get Op = iota
	Lookup
	List
)

// Spec describes the entities, for replaying committed state.
type Spec[ID cmp.Ordered, E any] struct {
	Key       func(E) ID
	Cacheable func(E) bool
	// Indexes maps an index name to its key function; keys are compared with ==.
	Indexes map[string]func(E) []any
}

// Commit is one committed transaction: the store version it produced and its changes.
type Commit[ID cmp.Ordered, E any] struct {
	Version uint64
	Puts    []E
	Dels    []ID
}

// Read is one recorded cache read. V0 and V1 are store versions read before and after
// the call; S0 and S1 are logical-clock stamps on the clock windows use.
type Read[ID cmp.Ordered, E any] struct {
	Op     Op
	ID     ID     // Get
	Index  string // Lookup
	Key    any    // Lookup
	Answer Answer
	Value  E    // Get Found
	Holder ID   // Lookup Found
	IDs    []ID // List Found
	V0, V1 uint64
	S0, S1 int64
}

// Window is one mutation's deferral window on the logical clock, from its marks being
// set to their release. A create names no ids.
type Window[ID cmp.Ordered] struct {
	IDs    []ID
	Create bool
	S0, S1 int64
}

// History is everything a run recorded. Base is the store state at Version 0 of the
// commit log (the state before the first recorded commit).
type History[ID cmp.Ordered, E any] struct {
	Base    []E
	BaseVer uint64
	Commits []Commit[ID, E]
	Reads   []Read[ID, E]
	Windows []Window[ID]
}

// Stats counts what the checker saw.
type Stats struct {
	Reads, Deferred, Answered int
}

// Check returns every violation in h and the read statistics.
func Check[ID cmp.Ordered, E any](spec Spec[ID, E], h History[ID, E]) ([]string, Stats) {
	states := replay(spec, h)
	var out []string
	var st Stats
	for i, r := range h.Reads {
		st.Reads++
		if r.Answer == Defer {
			st.Deferred++
			continue
		}
		st.Answered++
		if !states.anyMatches(spec, r) {
			out = append(out, fmt.Sprintf("read %d: %s answer %v matches no committed state in versions [%d, %d]", i, describe(r), r.Answer, r.V0, r.V1))
		}
		for _, w := range h.Windows {
			if r.S0 > w.S0 && r.S1 < w.S1 && mustDefer(r, w) {
				out = append(out, fmt.Sprintf("read %d: %s answered %v inside mutation window [%d, %d] on %v (create=%t)", i, describe(r), r.Answer, w.S0, w.S1, w.IDs, w.Create))
			}
		}
	}
	return out, st
}

func describe[ID cmp.Ordered, E any](r Read[ID, E]) string {
	switch r.Op {
	case Get:
		return fmt.Sprintf("Get(%v)", r.ID)
	case Lookup:
		return fmt.Sprintf("Lookup(%s, %v)", r.Index, r.Key)
	case List:
		return "List()"
	}
	return "unknown op"
}

// mustDefer reports whether SAFE-2 requires r to defer inside w.
func mustDefer[ID cmp.Ordered, E any](r Read[ID, E], w Window[ID]) bool {
	switch r.Op {
	case Get:
		return slices.Contains(w.IDs, r.ID)
	case Lookup:
		return r.Answer == Absent || slices.Contains(w.IDs, r.Holder)
	case List:
		return true
	}
	return false
}

// states holds the committed state at each version from BaseVer on.
type states[ID cmp.Ordered, E any] struct {
	base uint64
	at   []map[ID]E // at[v-base]
}

func replay[ID cmp.Ordered, E any](spec Spec[ID, E], h History[ID, E]) states[ID, E] {
	cur := map[ID]E{}
	for _, e := range h.Base {
		cur[spec.Key(e)] = e
	}
	s := states[ID, E]{base: h.BaseVer, at: []map[ID]E{clone(cur)}}
	commits := slices.Clone(h.Commits)
	slices.SortFunc(commits, func(a, b Commit[ID, E]) int { return cmp.Compare(a.Version, b.Version) })
	for _, c := range commits {
		for uint64(len(s.at)) < c.Version-s.base {
			s.at = append(s.at, s.at[len(s.at)-1]) // a version with no recorded change
		}
		for _, id := range c.Dels {
			delete(cur, id)
		}
		for _, e := range c.Puts {
			cur[spec.Key(e)] = e
		}
		s.at = append(s.at, clone(cur))
	}
	return s
}

func clone[K comparable, V any](m map[K]V) map[K]V {
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (s states[ID, E]) anyMatches(spec Spec[ID, E], r Read[ID, E]) bool {
	last := uint64(len(s.at)) - 1 // at always holds the base state
	for v := r.V0; v <= r.V1; v++ {
		i := v - s.base
		if v < s.base || i > last {
			i = last
		}
		if matches(spec, s.at[i], r) {
			return true
		}
	}
	return false
}

// matches reports whether r's answer is correct for one committed state.
func matches[ID cmp.Ordered, E any](spec Spec[ID, E], state map[ID]E, r Read[ID, E]) bool {
	switch r.Op {
	case Get:
		e, ok := state[r.ID]
		return r.Answer == Found && ok && spec.Cacheable(e) && reflect.DeepEqual(e, r.Value)
	case Lookup:
		holder, ok := holderOf(spec, state, r.Index, r.Key)
		if r.Answer == Absent {
			return !ok
		}
		return ok && holder == r.Holder
	case List:
		var ids []ID
		for id, e := range state {
			if spec.Cacheable(e) {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		return slices.Equal(ids, r.IDs)
	}
	return false
}

func holderOf[ID cmp.Ordered, E any](spec Spec[ID, E], state map[ID]E, index string, key any) (ID, bool) {
	keys := spec.Indexes[index]
	for id, e := range state {
		if spec.Cacheable(e) && slices.Contains(keys(e), key) {
			return id, true
		}
	}
	var zero ID
	return zero, false
}
