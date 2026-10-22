// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nobelk/surecache/internal/memstore"
)

// group is the test entity: a group with a unique owner and unique members. A
// soft-deleted group is not cacheable and, per PRE-3, holds no index keys.
type group struct {
	ID      string
	Name    string
	Owner   string
	Members []string
	Deleted bool
}

func cloneGroup(g group) group {
	g.Members = slices.Clone(g.Members)
	return g
}

func groupKey(g group) string       { return g.ID }
func groupCacheable(g group) bool   { return !g.Deleted }
func groupMembers(g group) []string { return g.Members }
func groupOwner(g group) []string   { return ownerKeys(g.Owner) }
func ownerKeys(owner string) []string {
	if owner == "" {
		return nil
	}
	return []string{owner}
}

func groupSpec(capacity int) Spec[string, group] {
	return Spec[string, group]{
		Name:             "groups",
		Key:              groupKey,
		Clone:            cloneGroup,
		Cacheable:        groupCacheable,
		Compare:          strings.Compare,
		Capacity:         capacity,
		CommittedNothing: memstore.CommittedNothing,
	}
}

func newGroupStore() *memstore.Store[string, group] {
	return memstore.New(memstore.Config[string, group]{
		Key: groupKey, Clone: cloneGroup, Cacheable: groupCacheable,
	})
}

// testPolicy is an outage policy for tests that do not exercise one.
type testPolicy struct{}

func (testPolicy) outagePolicy() {}

// recorder is an Observer that records every event as a string.
type recorder struct {
	NopObserver
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(format string, args ...any) {
	r.mu.Lock()
	r.events = append(r.events, fmt.Sprintf(format, args...))
	r.mu.Unlock()
}

func (r *recorder) Read(m Method, s ReadSource)          { r.add("read %s %s", m, s) }
func (r *recorder) StaleChanged(stale bool, cause error) { r.add("stale %t %v", stale, cause != nil) }
func (r *recorder) UnhealthyChanged(u bool)              { r.add("unhealthy %t", u) }
func (r *recorder) Entities(n int)                       { r.add("entities %d", n) }
func (r *recorder) Rebuild(t Trigger, res RebuildResult) { r.add("rebuild %s %s", t, res) }
func (r *recorder) AmbiguousMutation()                   { r.add("ambiguous") }
func (r *recorder) ProbeFailure()                        { r.add("probe_failure") }

// take returns the events recorded since the last take.
func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

// fixture is a cache over a memstore with two unique indexes.
type fixture struct {
	t        testing.TB
	c        *Cache[string, group]
	store    *memstore.Store[string, group]
	byMember Index[string, string]
	byOwner  Index[string, string]
	obs      *recorder
}

type fixtureConfig struct {
	capacity, memberCap, ownerCap int
	opts                          []Option
}

type fixtureOpt func(*fixtureConfig)

func withCapacity(primary, member, owner int) fixtureOpt {
	return func(c *fixtureConfig) { c.capacity, c.memberCap, c.ownerCap = primary, member, owner }
}

func withOptions(opts ...Option) fixtureOpt {
	return func(c *fixtureConfig) { c.opts = append(c.opts, opts...) }
}

func newFixture(t testing.TB, opts ...fixtureOpt) *fixture {
	t.Helper()
	cfg := fixtureConfig{capacity: 100, memberCap: 100, ownerCap: 100}
	for _, o := range opts {
		o(&cfg)
	}
	f := &fixture{t: t, store: newGroupStore(), obs: &recorder{}}
	b := NewBuilder(groupSpec(cfg.capacity), Source[string, group](f.store))
	f.byMember = AddIndex(b, "member", groupMembers, cfg.memberCap)
	f.byOwner = AddIndex(b, "owner", groupOwner, cfg.ownerCap)
	all := append([]Option{WithOutagePolicy(testPolicy{}), WithObserver(f.obs)}, cfg.opts...)
	c, err := b.Build(all...)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	f.c = c
	f.obs.take()
	return f
}

// probeLock makes every store call fail the test while the cache lock is held. Enable it
// only where the store-calling goroutine is the only cache user at that moment.
func (f *fixture) probeLock() {
	f.store.SetLockProbe(func() bool {
		if f.c.mu.TryLock() {
			f.c.mu.Unlock()
			return false
		}
		return true
	}, func(msg string) { f.t.Error(msg) })
}

// install seeds the store and installs the same entities into the cache as a trusted
// snapshot, bypassing Prime.
func (f *fixture) install(gs ...group) {
	f.t.Helper()
	f.store.Seed(gs...)
	var cacheable []group
	for _, g := range gs {
		if groupCacheable(g) {
			cacheable = append(cacheable, g)
		}
	}
	if err := installForTest(f.c, cacheable); err != nil {
		f.t.Fatalf("install: %v", err)
	}
	f.obs.take()
}

// installForTest builds aside and swaps in a trusted snapshot under the lock with the
// panic rule.
func installForTest[ID comparable, E any](c *Cache[ID, E], all []E) error {
	var err error
	c.withWriteLock(context.Background(), func(*eventBuf) {
		var s *snapshot[ID, E]
		s, err = c.buildAside(all)
		if err != nil {
			c.markStaleLocked(context.Background(), err)
			return
		}
		c.swapInLocked(s)
		c.stale = false
		c.queueStateLocked(stateEvent{kind: stateStale, on: false})
		c.queueStateLocked(stateEvent{kind: stateEntities, n: s.entities.Len()})
	})
	return err
}

func (f *fixture) isStale() bool {
	f.c.mu.RLock()
	defer f.c.mu.RUnlock()
	return f.c.stale
}

func (f *fixture) get(id string) (group, Answer) { return f.c.Get(id) }

func (f *fixture) member(m string) (string, Answer) {
	f.t.Helper()
	id, a, err := Lookup(f.c, f.byMember, m)
	if err != nil {
		f.t.Fatalf("Lookup: %v", err)
	}
	return id, a
}

func (f *fixture) owner(o string) (string, Answer) {
	f.t.Helper()
	id, a, err := Lookup(f.c, f.byOwner, o)
	if err != nil {
		f.t.Fatalf("Lookup: %v", err)
	}
	return id, a
}

func g(id string, owner string, members ...string) group {
	return group{ID: id, Name: "group " + id, Owner: owner, Members: members}
}
