// SPDX-License-Identifier: Apache-2.0

package surecachepg_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nobelk/surecache"
	"github.com/nobelk/surecache/surecachepg"
	"github.com/nobelk/surecache/surecachepg/internal/groupstore"
	"github.com/nobelk/surecache/surecachepg/internal/pgtest"
)

const (
	fenceKey   = 0x5c4e
	barrierKey = 4242
)

// env is a database with the groups schema, a proxy in front of it, a store whose pool
// goes through the proxy, and a direct pool for setup and observation.
type env struct {
	direct *pgxpool.Pool
	proxy  *pgtest.Proxy
	store  *groupstore.Store
}

func newEnv(t *testing.T) *env {
	t.Helper()
	cfg := pgtest.DB(t)
	b := surecachepg.DefaultBounds()
	if err := surecachepg.ConfigurePool(cfg, b); err != nil {
		t.Fatal(err)
	}
	direct := pgtest.Pool(t, cfg)
	if err := groupstore.Install(context.Background(), direct); err != nil {
		t.Fatal(err)
	}
	proxy := pgtest.NewProxy(t, cfg.ConnConfig.Host, cfg.ConnConfig.Port)
	via := pgtest.Via(cfg, proxy)
	via.ConnConfig.ConnectTimeout = time.Second
	return &env{
		direct: direct,
		proxy:  proxy,
		store: &groupstore.Store{
			Pool: pgtest.Pool(t, via), Fence: surecachepg.NewFence(fenceKey), Bounds: b,
			Capacity: 100, MemberCapacity: 100,
		},
	}
}

// groupCache is a cache over the store with member and owner indexes.
type groupCache struct {
	c        *surecache.Cache[string, groupstore.Group]
	byMember surecache.Index[string, string]
	byOwner  surecache.Index[string, string]
}

func newGroupCache(t *testing.T, s *groupstore.Store, opts ...surecache.Option) *groupCache {
	t.Helper()
	if err := surecachepg.CheckBudgets(s.Bounds, 4*time.Second); err != nil {
		t.Fatal(err)
	}
	b := surecache.NewBuilder(groupstore.Spec(s.Capacity), surecache.Source[string, groupstore.Group](s))
	gc := &groupCache{
		byMember: surecache.AddIndex(b, "member", groupstore.MemberKeys, s.MemberCapacity),
		byOwner:  surecache.AddIndex(b, "owner", groupstore.OwnerKeys, s.Capacity),
	}
	c, err := b.Build(append([]surecache.Option{surecache.WithRebuildInterval(200 * time.Millisecond)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	gc.c = c
	return gc
}

func (gc *groupCache) primeAndRun(t *testing.T) {
	t.Helper()
	if r, err := gc.c.Prime(context.Background()); r != surecache.Installed {
		t.Fatalf("Prime = %v, %v", r, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gc.c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run = %v", err)
		}
	})
}

// eventually polls cond in real time until it holds or the timeout passes.
func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		<-ticker.C
	}
}

// advisoryWaiters counts backends waiting, not granted, on an advisory lock key.
func advisoryWaiters(t *testing.T, pool *pgxpool.Pool, key int64, mode string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_locks
		WHERE locktype = 'advisory' AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
		AND objid = $1 AND mode = $2 AND NOT granted`, key, mode).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// SAFE-4, SAFE-5: a COMMIT the client abandoned still lands on the server. The cache
// goes stale on the ambiguous error, and the loop's rebuild waits on the fence until the
// late commit finishes, then installs state that includes it. A server-side barrier
// holds the COMMIT, so the test cannot pass without an overlapping late commit.
func TestAbandonedCommitFenced(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.direct.Exec(ctx, `
		CREATE FUNCTION barrier() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_advisory_lock(4242); PERFORM pg_advisory_unlock(4242); RETURN NULL; END $$;
		CREATE CONSTRAINT TRIGGER members_barrier AFTER INSERT ON members
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION barrier();`); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Seed(ctx, []groupstore.Group{{ID: "g1", Name: "alpha"}}); err != nil {
		t.Fatal(err)
	}
	// 1. The test session holds the barrier.
	barrier, err := e.direct.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Release()
	if _, err = barrier.Exec(ctx, `SELECT pg_advisory_lock($1)`, barrierKey); err != nil {
		t.Fatal(err)
	}
	gc := newGroupCache(t, e.store, surecache.WithOutagePolicy(surecache.ServeWhileExact()))
	gc.primeAndRun(t)

	// 2. The mutation's COMMIT blocks server-side in the trigger, holding the shared fence.
	mutated := make(chan error, 1)
	go func() {
		mutated <- gc.c.Mutate(ctx, []string{"g1"}, func() error { return e.store.AddMember(ctx, "g1", "late") })
	}()
	eventually(t, 5*time.Second, "the COMMIT to block on the barrier", func() bool {
		return advisoryWaiters(t, e.direct, barrierKey, "ExclusiveLock") == 1
	})
	var pid int
	if err = e.direct.QueryRow(ctx, `SELECT pid FROM pg_locks WHERE locktype = 'advisory' AND objid = $1
		AND NOT granted`, barrierKey).Scan(&pid); err != nil {
		t.Fatal(err)
	}

	// 3. The client loses the connection: an ambiguous error, and the cache goes stale.
	e.proxy.Down()
	err = <-mutated
	if err == nil || groupstore.CommittedNothing(err) {
		t.Fatalf("Mutate = %v; want an ambiguous error", err)
	}
	if _, a := gc.c.List(); a != surecache.Defer {
		t.Fatal("cache still trusted after an ambiguous outcome")
	}

	// 4. The abandoned backend is still active and still holds the shared fence.
	var state string
	if err := e.direct.QueryRow(ctx, `SELECT state FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&state); err != nil || state != "active" {
		t.Fatalf("mutation backend state = %q, %v", state, err)
	}
	var shared int
	if err := e.direct.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND pid = $1
		AND objid = $2 AND mode = 'ShareLock' AND granted`, pid, fenceKey).Scan(&shared); err != nil || shared != 1 {
		t.Fatalf("shared fence held = %d, %v", shared, err)
	}

	// 5. The network returns; the loop's exclusive fence request waits behind the
	// abandoned commit.
	e.proxy.Up()
	eventually(t, e.store.Bounds.FenceWait(), "the rebuild to wait on the exclusive fence", func() bool {
		return advisoryWaiters(t, e.direct, fenceKey, "ExclusiveLock") == 1
	})

	// 6. The late COMMIT lands; the rebuild installs state that includes it.
	if _, err := barrier.Exec(ctx, `SELECT pg_advisory_unlock($1)`, barrierKey); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the rebuild to install", func() bool {
		_, a := gc.c.List()
		return a == surecache.Found
	})
	g, a := gc.c.Get("g1")
	if a != surecache.Found || !slices.Equal(groupstore.MemberKeys(g), []string{"late"}) {
		t.Fatalf("Get(g1) = %v, %v; the install missed the late commit", g, a)
	}
	if id, a, _ := surecache.Lookup(gc.c, gc.byMember, "late"); a != surecache.Found || id != "g1" {
		t.Fatalf("Lookup(late) = %q, %v", id, a)
	}
}

// Section 11.3, reversible outage: prime through the proxy, take it down, assert the
// configured outage policy, bring it back, and assert recovery through the loop.
func TestRecipe_ReversibleOutage(t *testing.T) {
	t.Run("ServeWhileExact", func(t *testing.T) {
		e := newEnv(t)
		ctx := context.Background()
		if err := e.store.Seed(ctx, []groupstore.Group{{ID: "g1", Name: "alpha", Owner: "o1"}}); err != nil {
			t.Fatal(err)
		}
		gc := newGroupCache(t, e.store, surecache.WithOutagePolicy(surecache.ServeWhileExact()))
		gc.primeAndRun(t)

		e.proxy.Down()
		time.Sleep(500 * time.Millisecond) // real time: the store is really down
		if _, a := gc.c.Get("g1"); a != surecache.Found {
			t.Fatal("a trusted cache stopped answering in an outage under ServeWhileExact")
		}
		err := gc.c.Mutate(ctx, []string{"g1"}, func() error { return e.store.Rename(ctx, "g1") })
		if err == nil || groupstore.CommittedNothing(err) {
			t.Fatalf("Mutate during the outage = %v; want an ambiguous error", err)
		}
		if _, a := gc.c.Get("g1"); a != surecache.Defer {
			t.Fatal("the cache answered after an ambiguous outcome")
		}
		time.Sleep(600 * time.Millisecond) // several failing loop ticks
		if _, a := gc.c.List(); a != surecache.Defer {
			t.Fatal("rebuilt while the store was down")
		}

		e.proxy.Up()
		eventually(t, 10*time.Second, "recovery through the loop", func() bool {
			_, a := gc.c.Get("g1")
			return a == surecache.Found
		})
	})
	t.Run("DistrustAfterProbeFailures", func(t *testing.T) {
		e := newEnv(t)
		ctx := context.Background()
		if err := e.store.Seed(ctx, []groupstore.Group{{ID: "g1", Name: "alpha"}}); err != nil {
			t.Fatal(err)
		}
		gc := newGroupCache(t, e.store, surecache.WithOutagePolicy(surecache.DistrustAfterProbeFailures(surecache.HealthPolicy{
			Probe:        e.store.Pool.Ping,
			Interval:     200 * time.Millisecond,
			Timeout:      150 * time.Millisecond,
			Unavailable:  func(error) bool { return true },
			FailAfter:    2,
			RecoverAfter: 2,
		})))
		gc.primeAndRun(t)
		if _, a := gc.c.Get("g1"); a != surecache.Found {
			t.Fatal("not trusted after prime")
		}

		e.proxy.Down()
		eventually(t, 5*time.Second, "reads to defer once the probe fails", func() bool {
			_, a := gc.c.Get("g1")
			return a == surecache.Defer
		})

		e.proxy.Up()
		eventually(t, 10*time.Second, "RecoverAfter probe successes", func() bool {
			_, a := gc.c.Get("g1")
			return a == surecache.Found
		})
	})
}

// Section 11.3, restart: a fresh cache primes over a store holding every entity shape,
// including cacheable groups without index keys, and matches the store exactly.
func TestRecipe_Restart(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	shapes := []groupstore.Group{
		{ID: "g1", Name: "keys", Owner: "o1", Members: []groupstore.Member{{Name: "m1"}, {Name: "m2"}}},
		{ID: "g2", Name: "members only", Members: []groupstore.Member{{Name: "m3"}}},
		{ID: "g3", Name: "owner only", Owner: "o3"},
		{ID: "g4", Name: "no keys"},
		{ID: "g5", Name: "soft deleted", Deleted: true},
	}
	if err := e.store.Seed(ctx, shapes); err != nil {
		t.Fatal(err)
	}
	gc := newGroupCache(t, e.store, surecache.WithOutagePolicy(surecache.ServeWhileExact()))
	if r, err := gc.c.Prime(ctx); r != surecache.Installed {
		t.Fatalf("Prime = %v, %v", r, err)
	}
	var want []string
	for _, s := range shapes {
		stored, err := e.store.LoadOne(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		got, a := gc.c.Get(s.ID)
		if !groupstore.Cacheable(stored) {
			if a != surecache.Defer {
				t.Fatalf("Get(%s) of a non-cacheable group = %v", s.ID, a)
			}
			continue
		}
		want = append(want, s.ID)
		if a != surecache.Found || !slices.Equal(groupstore.MemberKeys(got), groupstore.MemberKeys(stored)) ||
			got.Owner != stored.Owner || got.Name != stored.Name {
			t.Fatalf("Get(%s) = %v, %v; store holds %v", s.ID, got, a, stored)
		}
		for _, m := range groupstore.MemberKeys(stored) {
			if id, a, _ := surecache.Lookup(gc.c, gc.byMember, m); a != surecache.Found || id != s.ID {
				t.Fatalf("Lookup(member %s) = %q, %v", m, id, a)
			}
		}
		for _, o := range groupstore.OwnerKeys(stored) {
			if id, a, _ := surecache.Lookup(gc.c, gc.byOwner, o); a != surecache.Found || id != s.ID {
				t.Fatalf("Lookup(owner %s) = %q, %v", o, id, a)
			}
		}
	}
	if ids, a := gc.c.List(); a != surecache.Found || !slices.Equal(ids, want) {
		t.Fatalf("List = %v, %v; want %v", ids, a, want)
	}
	if _, a, _ := surecache.Lookup(gc.c, gc.byMember, "nobody"); a != surecache.Absent {
		t.Fatalf("Lookup(nobody) = %v", a)
	}
	if _, err := e.store.LoadOne(ctx, "missing"); !errors.Is(err, groupstore.ErrNotFound) {
		t.Fatalf("LoadOne(missing) = %v", err)
	}
}
