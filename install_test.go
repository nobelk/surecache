// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"errors"
	"slices"
	"testing"
)

func TestBuildAside_HappyPathTwoIndexes(t *testing.T) {
	f := newFixture(t)
	f.install(g("b", "ob", "m1", "m2"), g("a", "", "m3"), group{ID: "x", Deleted: true})
	if ids, a := f.c.List(); a != Found || !slices.Equal(ids, []string{"a", "b"}) {
		t.Fatalf("List = %v, %v", ids, a)
	}
	for m, want := range map[string]string{"m1": "b", "m2": "b", "m3": "a"} {
		if id, a := f.member(m); a != Found || id != want {
			t.Errorf("member %s = %q, %v", m, id, a)
		}
	}
	if id, a := f.owner("ob"); a != Found || id != "b" {
		t.Errorf("owner = %q, %v", id, a)
	}
}

// SAFE-8, NFR-1: over capacity the install is refused, for the primary map and for each
// index.
func TestInstall_OverCapacityRefused(t *testing.T) {
	cases := map[string]fixtureOpt{
		"primary": withCapacity(1, 10, 10),
		"member":  withCapacity(10, 1, 10),
		"owner":   withCapacity(10, 10, 1),
	}
	for name, capacity := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, capacity)
			err := installForTest(f.c, []group{g("a", "o1", "m1"), g("b", "o2", "m2")})
			if !errors.Is(err, ErrOverCapacity) {
				t.Fatalf("err = %v", err)
			}
			assertNothingInstalled(t, f)
		})
	}
}

// SAFE-12: a primary key twice in one bulk load fails safe.
func TestInstall_DuplicatePrimaryKeyFails(t *testing.T) {
	f := newFixture(t)
	err := installForTest(f.c, []group{g("a", ""), g("a", "")})
	if !errors.Is(err, ErrContract) {
		t.Fatalf("err = %v", err)
	}
	assertNothingInstalled(t, f)
}

// SAFE-12, PRE-2: two entities holding one unique key fail safe.
func TestInstall_DuplicateUniqueKeyFails(t *testing.T) {
	for name, gs := range map[string][]group{
		"member": {g("a", "", "m"), g("b", "", "m")},
		"owner":  {g("a", "o"), g("b", "o")},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			if err := installForTest(f.c, gs); !errors.Is(err, ErrContract) {
				t.Fatalf("err = %v", err)
			}
			assertNothingInstalled(t, f)
		})
	}
}

// An entity listing one key twice holds it once; that is not a conflict.
func TestInstall_RepeatedKeyOnOneEntity(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", "", "m", "m"))
	if id, a := f.member("m"); a != Found || id != "a" {
		t.Fatalf("member = %q, %v", id, a)
	}
}

// SAFE-12: a bulk load must contain only cacheable entities.
func TestInstall_NonCacheableFails(t *testing.T) {
	f := newFixture(t)
	err := installForTest(f.c, []group{g("a", ""), {ID: "b", Deleted: true}})
	if !errors.Is(err, ErrContract) {
		t.Fatalf("err = %v", err)
	}
	assertNothingInstalled(t, f)
}

// Internal invariant 5: a failed build leaves the live maps untouched.
func TestInstall_FailureLeavesLiveMapsUntouched(t *testing.T) {
	f := newFixture(t)
	f.install(g("a", "o", "m"))
	if err := installForTest(f.c, []group{g("x", "", "k"), g("y", "", "k")}); !errors.Is(err, ErrContract) {
		t.Fatalf("err = %v", err)
	}
	if !f.isStale() {
		t.Fatal("failed install must mark stale")
	}
	f.c.mu.Lock()
	f.c.stale = false
	f.c.mu.Unlock()
	if e, a := f.get("a"); a != Found || e.Owner != "o" {
		t.Fatalf("old entity lost: %v, %v", e, a)
	}
	if _, a := f.member("k"); a != Absent {
		t.Fatalf("aborted install leaked an index entry: %v", a)
	}
}

// SAFE-10: with WithCloneOnInstall the cache does not share the Source's value.
func TestCloneOnInstall(t *testing.T) {
	for _, clone := range []bool{false, true} {
		var opts []fixtureOpt
		if clone {
			opts = append(opts, withOptions(WithCloneOnInstall()))
		}
		f := newFixture(t, opts...)
		src := g("a", "", "m1")
		if err := installForTest(f.c, []group{src}); err != nil {
			t.Fatal(err)
		}
		src.Members[0] = "changed by source"
		e, _ := f.get("a")
		if got := e.Members[0] == "m1"; got != clone {
			t.Errorf("clone=%t: cached member = %q", clone, e.Members[0])
		}
	}
}

// SAFE-11, internal invariant 11: a panicking callback inside the install region leaves
// the cache stale and the lock free.
func TestLockedRegion_CallbackPanicLeavesStaleAndUnlocked(t *testing.T) {
	cases := map[string]func(*Spec[string, group], *bool){
		"Key": func(s *Spec[string, group], armed *bool) {
			s.Key = func(e group) string {
				if *armed {
					panic("key")
				}
				return e.ID
			}
		},
		"Cacheable": func(s *Spec[string, group], armed *bool) {
			s.Cacheable = func(group) bool {
				if *armed {
					panic("cacheable")
				}
				return true
			}
		},
		"index keys": nil,
	}
	for name, patch := range cases {
		t.Run(name, func(t *testing.T) {
			armed := false
			spec := groupSpec(10)
			if patch != nil {
				patch(&spec, &armed)
			}
			b := NewBuilder(spec, Source[string, group](newGroupStore()))
			AddIndex(b, "member", func(e group) []string {
				if armed && patch == nil {
					panic("keys")
				}
				return e.Members
			}, 10)
			c, err := b.Build(WithOutagePolicy(testPolicy{}))
			if err != nil {
				t.Fatal(err)
			}
			if err := installForTest(c, []group{g("a", "", "m")}); err != nil {
				t.Fatal(err)
			}
			armed = true
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("panic must propagate")
					}
				}()
				_ = installForTest(c, []group{g("b", "", "n")})
			}()
			if !c.mu.TryLock() {
				t.Fatal("lock still held after panic")
			}
			stale := c.stale
			c.mu.Unlock()
			if !stale {
				t.Fatal("cache must be stale after a panic in a locked region")
			}
		})
	}
}

func assertNothingInstalled(t *testing.T, f *fixture) {
	t.Helper()
	if !f.isStale() {
		t.Fatal("cache must stay stale")
	}
	f.c.mu.RLock()
	defer f.c.mu.RUnlock()
	if f.c.entities.Len() != 0 || len(f.c.directory) != 0 {
		t.Fatalf("subset installed: %d entities", f.c.entities.Len())
	}
	for _, ix := range f.c.indexes {
		if ix.size() != 0 {
			t.Fatalf("index %s has %d keys", ix.label(), ix.size())
		}
	}
}
