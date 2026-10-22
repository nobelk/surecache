// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"testing"
)

// The allocation tests back NFR-3: a trusted read is one read lock, one map lookup and
// one clone. They skip under -race, which changes allocation counts; CI runs them in a
// separate step without it.

func skipUnderRace(t *testing.T) {
	t.Helper()
	if raceEnabled {
		t.Skip("allocation counts are not meaningful under -race")
	}
}

// NFR-3: Get allocates nothing beyond the clone: zero for a value entity, the clone's
// own allocation for an entity with a slice.
func TestGet_AllocsOnlyClone(t *testing.T) {
	skipUnderRace(t)
	for _, obs := range []Observer{NopObserver{}, &countingObserver{}} {
		pc := newPointCache(t, 100, WithObserver(obs))
		if n := testing.AllocsPerRun(1000, func() { sinkPoint, _ = pc.c.Get(7) }); n != 0 {
			t.Errorf("%T: Get allocs = %v, want 0", obs, n)
		}
	}
	c := newTaggedCache(t, 100)
	if n := testing.AllocsPerRun(1000, func() { _, _ = c.Get(7) }); n != 1 {
		t.Errorf("Get on a slice entity allocs = %v, want 1 (the clone)", n)
	}
}

// NFR-3: Lookup allocates nothing, for a hit and for an authoritative miss.
func TestLookup_ZeroAllocs(t *testing.T) {
	skipUnderRace(t)
	pc := newPointCache(t, 100, WithObserver(&countingObserver{}))
	for _, key := range []int64{14, 15} {
		if n := testing.AllocsPerRun(1000, func() { _, _, _ = Lookup(pc.c, pc.byX, key) }); n != 0 {
			t.Errorf("Lookup(%d) allocs = %v, want 0", key, n)
		}
	}
}

// NFR-3: List allocates exactly the returned copy.
func TestList_OneAlloc(t *testing.T) {
	skipUnderRace(t)
	pc := newPointCache(t, 100, WithObserver(&countingObserver{}))
	if n := testing.AllocsPerRun(1000, func() { _, _ = pc.c.List() }); n != 1 {
		t.Errorf("List allocs = %v, want 1", n)
	}
}
