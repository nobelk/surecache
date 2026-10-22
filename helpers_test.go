// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nobelk/surecache/internal/boundedmap"
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

// recorder is an Observer that records every event as a string.
type recorder struct {
	NopObserver
	mu     sync.Mutex
	events []string
	cause  error // the cause of the latest transition to stale
}

func (r *recorder) add(format string, args ...any) {
	r.mu.Lock()
	r.events = append(r.events, fmt.Sprintf(format, args...))
	r.mu.Unlock()
}

func (r *recorder) Read(m Method, s ReadSource) { r.add("read %s %s", m, s) }
func (r *recorder) StaleChanged(stale bool, cause error) {
	r.mu.Lock()
	if stale {
		r.cause = cause
	}
	r.mu.Unlock()
	r.add("stale %t %v", stale, cause != nil)
}
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
	source                        func(*memstore.Store[string, group]) Source[string, group]
	policy                        func() OutagePolicy
	noProbe                       bool
}

type fixtureOpt func(*fixtureConfig)

func withCapacity(primary, member, owner int) fixtureOpt {
	return func(c *fixtureConfig) { c.capacity, c.memberCap, c.ownerCap = primary, member, owner }
}

func withOptions(opts ...Option) fixtureOpt {
	return func(c *fixtureConfig) { c.opts = append(c.opts, opts...) }
}

// withSource wraps the memstore in a custom Source.
func withSource(wrap func(*memstore.Store[string, group]) Source[string, group]) fixtureOpt {
	return func(c *fixtureConfig) { c.source = wrap }
}

// withPolicy sets the outage policy; the default is ServeWhileExact. It is a function so
// the policy can close over the fixture's store.
func withPolicy(p func() OutagePolicy) fixtureOpt {
	return func(c *fixtureConfig) { c.policy = p }
}

// concurrent disables the lock probe, for tests where several goroutines use the cache
// at once: an unrelated reader holding RLock would make the probe report a correct
// store call.
func concurrent() fixtureOpt {
	return func(c *fixtureConfig) { c.noProbe = true }
}

func newFixture(t testing.TB, opts ...fixtureOpt) *fixture {
	t.Helper()
	cfg := fixtureConfig{capacity: 100, memberCap: 100, ownerCap: 100}
	for _, o := range opts {
		o(&cfg)
	}
	f := &fixture{t: t, store: newGroupStore(), obs: &recorder{}}
	var src Source[string, group] = f.store
	if cfg.source != nil {
		src = cfg.source(f.store)
	}
	b := NewBuilder(groupSpec(cfg.capacity), src)
	f.byMember = AddIndex(b, "member", groupMembers, cfg.memberCap)
	f.byOwner = AddIndex(b, "owner", groupOwner, cfg.ownerCap)
	policy := ServeWhileExact()
	if cfg.policy != nil {
		policy = cfg.policy()
	}
	all := append([]Option{WithOutagePolicy(policy), WithObserver(f.obs)}, cfg.opts...)
	c, err := b.Build(all...)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	f.c = c
	f.obs.take()
	if !cfg.noProbe {
		f.probeLock()
	}
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

// staleCause returns the cause of the latest transition to stale.
func (f *fixture) staleCause() error {
	f.obs.mu.Lock()
	defer f.obs.mu.Unlock()
	return f.obs.cause
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

// mutate runs a store transaction through Mutate with the given ids.
func (f *fixture) mutate(ctx context.Context, ids []string, fn func(tx *memstore.Tx[string, group]) error) error {
	return f.c.Mutate(ctx, ids, func() error { return f.store.Write(ctx, fn) })
}

// create inserts e through Create.
func (f *fixture) create(ctx context.Context, e group) (string, error) {
	return f.c.Create(ctx, func() (string, error) {
		err := f.store.Write(ctx, func(tx *memstore.Tx[string, group]) error { return tx.Insert(e) })
		return e.ID, err
	})
}

// set replaces whole entities in one transaction.
func set(es ...group) func(tx *memstore.Tx[string, group]) error {
	return func(tx *memstore.Tx[string, group]) error {
		for _, e := range es {
			tx.Put(e)
		}
		return nil
	}
}

// del deletes entities in one transaction.
func del(ids ...string) func(tx *memstore.Tx[string, group]) error {
	return func(tx *memstore.Tx[string, group]) error {
		for _, id := range ids {
			if err := tx.Delete(id); err != nil {
				return err
			}
		}
		return nil
	}
}

// marks returns the pending map size and the in-flight count.
func (f *fixture) marks() (pending, inFlight, windows int) {
	f.c.mu.RLock()
	defer f.c.mu.RUnlock()
	return len(f.c.pending), f.c.inFlight, len(f.c.refreshes)
}

// gate blocks one hook call until released.
type gate struct {
	arrived chan struct{}
	release chan struct{}
}

func newGate() *gate {
	return &gate{arrived: make(chan struct{}), release: make(chan struct{})}
}

func (g *gate) block() {
	close(g.arrived)
	<-g.release
}

// onID returns a hook that blocks on gt the first time it sees id, and passes others.
func onID(id string, gt *gate) func(any) {
	var once sync.Once
	return func(v any) {
		if v.(string) == id {
			once.Do(gt.block)
		}
	}
}

func g(id string, owner string, members ...string) group {
	return group{ID: id, Name: "group " + id, Owner: owner, Members: members}
}

type txn = memstore.Tx[string, group]

// checkStructure verifies the structural invariant under the read lock: every index
// entry names an entity that holds the key, every key of every cached entity is indexed
// to that entity, and the directory is the sorted key set of the entities.
func checkStructure[ID comparable, E any](c *Cache[ID, E]) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, ix := range c.indexes {
		if err := ix.(interface {
			check(*boundedmap.Map[ID, E]) error
		}).check(c.entities); err != nil {
			return err
		}
	}
	if len(c.directory) != c.entities.Len() {
		return fmt.Errorf("directory has %d ids, entities %d", len(c.directory), c.entities.Len())
	}
	for i, id := range c.directory {
		if _, ok := c.entities.Get(id); !ok {
			return fmt.Errorf("directory id %v not cached", id)
		}
		if i > 0 && c.spec.Compare(c.directory[i-1], id) >= 0 {
			return fmt.Errorf("directory not strictly sorted at %d", i)
		}
	}
	return nil
}

func (ix *index[ID, E, K]) check(entities *boundedmap.Map[ID, E]) error {
	for k, holder := range ix.m.All() {
		e, ok := entities.Get(holder)
		if !ok || !slices.Contains(ix.keys(e), k) {
			return fmt.Errorf("index %s: key %v names %v, which does not hold it", ix.name, k, holder)
		}
	}
	for id, e := range entities.All() {
		for _, k := range ix.keys(e) {
			if holder, ok := ix.m.Get(k); !ok || holder != id {
				return fmt.Errorf("index %s: key %v of %v indexed to %v (present=%t)", ix.name, k, id, holder, ok)
			}
		}
	}
	return nil
}
