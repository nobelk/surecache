// SPDX-License-Identifier: Apache-2.0

package surecache_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nobelk/surecache"
)

// Group is the example entity: a group with a unique set of members.
type Group struct {
	ID      string
	Members []string
}

// errNotFound and errMemberTaken are the store's nothing-committed errors: it returns
// them only when it changed nothing.
var (
	errNotFound    = errors.New("not found")
	errMemberTaken = errors.New("member belongs to another group")
)

// store is a tiny authoritative store. A real one is a database; the fence is its
// reader-writer lock: mutations hold it shared, the bulk load exclusively.
type store struct {
	fence  sync.RWMutex
	mu     sync.Mutex
	groups map[string]Group
}

func newStore(groups ...Group) *store {
	s := &store{groups: map[string]Group{}}
	for _, g := range groups {
		s.groups[g.ID] = g
	}
	return s
}

func (s *store) LoadOne(_ context.Context, id string) (Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[id]
	if !ok {
		return Group{}, errNotFound
	}
	g.Members = slices.Clone(g.Members)
	return g, nil
}

func (s *store) NotFound(err error) bool { return errors.Is(err, errNotFound) }

func (s *store) LoadAll(context.Context) ([]Group, error) {
	s.fence.Lock() // every earlier mutation has finished, and none runs during the read
	defer s.fence.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Group
	for _, g := range s.groups {
		g.Members = slices.Clone(g.Members)
		out = append(out, g)
	}
	return out, nil
}

// AddMember adds m to group g, checking uniqueness in the same atomic step.
func (s *store) AddMember(g, m string) error {
	s.fence.RLock()
	defer s.fence.RUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	grp, ok := s.groups[g]
	if !ok {
		return errNotFound
	}
	for _, other := range s.groups {
		if slices.Contains(other.Members, m) {
			return errMemberTaken
		}
	}
	grp.Members = append(slices.Clone(grp.Members), m)
	s.groups[g] = grp
	return nil
}

// MoveMember moves m from one group to another in one atomic step.
func (s *store) MoveMember(m, from, to string) error {
	s.fence.RLock()
	defer s.fence.RUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	src, ok1 := s.groups[from]
	dst, ok2 := s.groups[to]
	if !ok1 || !ok2 || !slices.Contains(src.Members, m) {
		return errNotFound
	}
	src.Members = slices.DeleteFunc(slices.Clone(src.Members), func(x string) bool { return x == m })
	dst.Members = append(slices.Clone(dst.Members), m)
	s.groups[from], s.groups[to] = src, dst
	return nil
}

// GroupOf answers "which group holds member m" from the store.
func (s *store) GroupOf(m string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, g := range s.groups {
		if slices.Contains(g.Members, m) {
			return id, true
		}
	}
	return "", false
}

// build returns a cache over s with a unique index from member to group.
func build(s *store) (*surecache.Cache[string, Group], surecache.Index[string, string]) {
	b := surecache.NewBuilder(surecache.Spec[string, Group]{
		Name:      "groups",
		Key:       func(g Group) string { return g.ID },
		Clone:     func(g Group) Group { g.Members = slices.Clone(g.Members); return g },
		Cacheable: func(Group) bool { return true },
		Compare:   strings.Compare,
		Capacity:  1000,
		CommittedNothing: func(err error) bool {
			return errors.Is(err, errNotFound) || errors.Is(err, errMemberTaken)
		},
	}, surecache.Source[string, Group](s))
	byMember := surecache.AddIndex(b, "member", func(g Group) []string { return g.Members }, 10_000)
	c, err := b.Build(surecache.WithOutagePolicy(surecache.ServeWhileExact()))
	if err != nil {
		panic(err)
	}
	return c, byMember
}

// Build the cache, prime it, run the rebuild loop, read, and mutate through it.
func Example() {
	s := newStore(Group{ID: "red", Members: []string{"ann"}}, Group{ID: "blue"})
	cache, byMember := build(s)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if r, err := cache.Prime(ctx); r != surecache.Installed {
		panic(fmt.Sprint("startup failed: ", r, err)) // never serve an unprimed cache
	}
	go func() { _ = cache.Run(ctx) }()

	if g, a := cache.Get("red"); a == surecache.Found {
		fmt.Println("red:", g.Members)
	}
	err := cache.Mutate(ctx, []string{"blue"}, func() error { return s.AddMember("blue", "bob") })
	fmt.Println("add bob:", err)
	id, a, _ := surecache.Lookup(cache, byMember, "bob")
	fmt.Println("bob is in:", id, a)
	// Output:
	// red: [ann]
	// add bob: <nil>
	// bob is in: blue found
}

// Map Defer to a store call, always. Absent is an authoritative negative; Defer never is.
func ExampleLookup() {
	s := newStore(Group{ID: "red", Members: []string{"ann"}})
	cache, byMember := build(s)
	groupOf := func(m string) (string, bool) {
		id, answer, err := surecache.Lookup(cache, byMember, m)
		switch {
		case err != nil || answer == surecache.Defer:
			return s.GroupOf(m) // ask the store
		case answer == surecache.Absent:
			return "", false
		}
		return id, true
	}
	fmt.Println(groupOf("ann")) // stale before Prime: answered by the store
	if r, _ := cache.Prime(context.Background()); r != surecache.Installed {
		panic(r)
	}
	fmt.Println(groupOf("ann")) // answered by the cache
	fmt.Println(groupOf("zed")) // authoritative: nobody holds zed
	// Output:
	// red true
	// red true
	//  false
}

// A mutation names every entity it can change: a move between two groups names both.
func ExampleCache_Mutate() {
	s := newStore(Group{ID: "red", Members: []string{"ann"}}, Group{ID: "blue"})
	cache, byMember := build(s)
	if r, _ := cache.Prime(context.Background()); r != surecache.Installed {
		panic(r)
	}
	err := cache.Mutate(context.Background(), []string{"red", "blue"}, func() error {
		return s.MoveMember("ann", "red", "blue")
	})
	fmt.Println("move:", err)
	id, _, _ := surecache.Lookup(cache, byMember, "ann")
	fmt.Println("ann is in:", id)
	// A nothing-committed error leaves the cache as is; any other error makes it stale.
	err = cache.Mutate(context.Background(), []string{"red"}, func() error { return s.AddMember("red", "ann") })
	fmt.Println("add ann again:", err)
	if _, a := cache.Get("red"); a == surecache.Found {
		fmt.Println("still trusted")
	}
	// Output:
	// move: <nil>
	// ann is in: blue
	// add ann again: member belongs to another group
	// still trusted
}

// Branch on the result of Prime, never on its error alone, and fail startup on anything
// but Installed.
func ExampleCache_Prime() {
	cache, _ := build(newStore(Group{ID: "red"}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	switch result, err := cache.Prime(ctx); result {
	case surecache.Installed:
		fmt.Println("ready")
	case surecache.Discarded:
		fmt.Println("a write landed during the load; retry Prime or fail startup")
	case surecache.Failed:
		fmt.Println("startup failed:", err)
	}
	// Output: ready
}
