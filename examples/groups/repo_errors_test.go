// SPDX-License-Identifier: Apache-2.0

package groups_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/nobelk/surecache"
	"github.com/nobelk/surecache/examples/groups"
	"github.com/nobelk/surecache/surecachepg"
)

func TestCachedRepo_FallbackReads(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()
	store := groups.NewStore(pool, surecachepg.NewFence(7), surecachepg.DefaultBounds(), 10, 10)
	if err := store.CreateGroup(ctx, "red", "Red"); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMember(ctx, "red", "ann"); err != nil {
		t.Fatal(err)
	}
	repo, err := groups.NewCachedRepo(store, surecache.WithOutagePolicy(surecache.ServeWhileExact()))
	if err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T) {
		t.Helper()
		if _, answer := repo.Cache().Get("red"); answer != surecache.Defer {
			t.Fatalf("cache Get = %v, want Defer", answer)
		}
		g, ok, err := repo.Group(ctx, "red")
		if err != nil || !ok || g.ID != "red" || g.Name != "Red" || !slices.Equal(g.Members, []string{"ann"}) {
			t.Fatalf("Group hit = %+v, %v, %v", g, ok, err)
		}
		if g, ok, err := repo.Group(ctx, "missing"); err != nil || ok || !reflect.DeepEqual(g, groups.Group{}) {
			t.Fatalf("Group miss = %+v, %v, %v", g, ok, err)
		}
		if id, ok, err := repo.GroupForMember(ctx, "ann"); err != nil || !ok || id != "red" {
			t.Fatalf("member hit = %q, %v, %v", id, ok, err)
		}
		if id, ok, err := repo.GroupForMember(ctx, "missing"); err != nil || ok || id != "" {
			t.Fatalf("member miss = %q, %v, %v", id, ok, err)
		}
		if ids, err := repo.Groups(ctx); err != nil || !slices.Equal(ids, []string{"red"}) {
			t.Fatalf("Groups = %v, %v", ids, err)
		}
	}
	t.Run("unprimed", check)
	if result, err := repo.Cache().Prime(ctx); result != surecache.Installed || err != nil {
		t.Fatalf("Prime = %v, %v", result, err)
	}
	// An unclassified write failure invalidates the snapshot. All subsequent reads
	// must reach the store; no direct store writes occur after Prime.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := repo.AddMember(canceled, "red", "bob"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled AddMember = %v", err)
	}
	t.Run("stale", check)
	if _, ok, err := store.GroupForMember(ctx, "bob"); err != nil || ok {
		t.Fatalf("canceled write persisted: %v, %v", ok, err)
	}
}

func TestCachedRepo_InvalidBuilderOption(t *testing.T) {
	// Build validates options without touching the database.
	store := groups.NewStore(nil, surecachepg.NewFence(7), surecachepg.DefaultBounds(), 10, 10)
	repo, err := groups.NewCachedRepo(store)
	if repo != nil || !errors.Is(err, surecache.ErrNoOutagePolicy) {
		t.Fatalf("NewCachedRepo without required policy = %v, %v", repo, err)
	}
	repo, err = groups.NewCachedRepo(store,
		surecache.WithOutagePolicy(surecache.ServeWhileExact()),
		surecache.WithRebuildInterval(0))
	if repo != nil || err == nil {
		t.Fatalf("NewCachedRepo with zero rebuild interval = %v, %v", repo, err)
	}
}

func TestCachedRepo_InvalidMutationsRollBack(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()
	store := groups.NewStore(pool, surecachepg.NewFence(7), surecachepg.DefaultBounds(), 10, 10)
	for _, id := range []string{"red", "blue"} {
		if err := store.CreateGroup(ctx, id, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AddMember(ctx, "red", "ann"); err != nil {
		t.Fatal(err)
	}
	repo, err := groups.NewCachedRepo(store, surecache.WithOutagePolicy(surecache.ServeWhileExact()))
	if err != nil {
		t.Fatal(err)
	}
	if result, primeErr := repo.Cache().Prime(ctx); result != surecache.Installed || primeErr != nil {
		t.Fatalf("Prime = %v, %v", result, primeErr)
	}
	before, err := store.LoadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		op   func() error
	}{
		{"missing member", func() error { return repo.MoveMember(ctx, "nobody", "red", "blue") }},
		{"wrong source group", func() error { return repo.MoveMember(ctx, "ann", "blue", "red") }},
		{"missing target group", func() error { return repo.MoveMember(ctx, "ann", "red", "missing") }},
		{"delete missing group", func() error { return repo.DeleteGroup(ctx, "missing") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.op(); !errors.Is(err, groups.ErrNotFound) || !groups.CommittedNothing(err) {
				t.Fatalf("mutation = %v, want classified ErrNotFound", err)
			}
			after, err := store.LoadAll(ctx)
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("database changed after rejected mutation: %+v, %v; before %+v", after, err, before)
			}
			for _, id := range []string{"red", "blue"} {
				if _, answer := repo.Cache().Get(id); answer != surecache.Found {
					t.Fatalf("cache Get(%q) = %v; rejected mutation should preserve trust", id, answer)
				}
			}
			assertAgree(t, repo, store, []string{"ann", "nobody"})
		})
	}
}

func TestStore_LoadAllOverCapacityReturnsNoSubset(t *testing.T) {
	for _, tc := range []struct {
		name              string
		capacity, members int
	}{
		{"entities", 1, 10},
		{"member index", 10, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newDB(t)
			ctx := context.Background()
			store := groups.NewStore(pool, surecachepg.NewFence(7), surecachepg.DefaultBounds(), tc.capacity, tc.members)
			for _, id := range []string{"red", "blue"} {
				if err := store.CreateGroup(ctx, id, id); err != nil {
					t.Fatal(err)
				}
				if err := store.AddMember(ctx, id, id+"-member"); err != nil {
					t.Fatal(err)
				}
			}
			if all, err := store.LoadAll(ctx); !errors.Is(err, surecache.ErrOverCapacity) || all != nil {
				t.Fatalf("LoadAll = %+v, %v; want nil, ErrOverCapacity", all, err)
			}
			repo, err := groups.NewCachedRepo(store, surecache.WithOutagePolicy(surecache.ServeWhileExact()))
			if err != nil {
				t.Fatal(err)
			}
			if result, err := repo.Cache().Prime(ctx); result != surecache.Failed || !errors.Is(err, surecache.ErrOverCapacity) {
				t.Fatalf("Prime = %v, %v", result, err)
			}
			if ids, answer := repo.Cache().List(); answer != surecache.Defer || len(ids) != 0 {
				t.Fatalf("failed Prime exposed directory subset: %v, %v", ids, answer)
			}
			for _, id := range []string{"red", "blue"} {
				if _, answer := repo.Cache().Get(id); answer != surecache.Defer {
					t.Fatalf("failed Prime exposed entity %q: %v", id, answer)
				}
			}
			assertAgree(t, repo, store, []string{"red-member", "blue-member", "missing"})
		})
	}
}

func TestRepo_DatabaseErrors(t *testing.T) {
	for _, failure := range []string{"canceled", "closed pool"} {
		t.Run(failure, func(t *testing.T) {
			pool := newDB(t)
			store := groups.NewStore(pool, surecachepg.NewFence(7), surecachepg.DefaultBounds(), 10, 10)
			repo, err := groups.NewCachedRepo(store, surecache.WithOutagePolicy(surecache.ServeWhileExact()))
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if failure == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			} else {
				pool.Close()
			}
			checkError := func(t *testing.T, err error) {
				t.Helper()
				if err == nil || groups.CommittedNothing(err) || (failure == "canceled" && !errors.Is(err, context.Canceled)) {
					t.Fatalf("database failure = %v", err)
				}
			}
			// The unprimed decorator must propagate fallback failures, not report absence.
			for name, r := range map[string]groups.Repo{"store": store, "decorator": repo} {
				t.Run(name, func(t *testing.T) {
					g, ok, readErr := r.Group(ctx, "red")
					checkError(t, readErr)
					if ok || !reflect.DeepEqual(g, groups.Group{}) {
						t.Fatalf("Group error returned value: %+v, %v", g, ok)
					}
					id, ok, readErr := r.GroupForMember(ctx, "ann")
					checkError(t, readErr)
					if ok || id != "" {
						t.Fatalf("member error returned value: %q, %v", id, ok)
					}
					ids, readErr := r.Groups(ctx)
					checkError(t, readErr)
					if len(ids) != 0 {
						t.Fatalf("Groups error returned values: %v", ids)
					}
					checkError(t, r.CreateGroup(ctx, "red", "Red"))
					checkError(t, r.AddMember(ctx, "red", "ann"))
					checkError(t, r.MoveMember(ctx, "ann", "red", "blue"))
					checkError(t, r.DeleteGroup(ctx, "red"))
				})
			}
			all, err := store.LoadAll(ctx)
			checkError(t, err)
			if all != nil {
				t.Fatalf("LoadAll error returned subset: %+v", all)
			}
		})
	}
}
