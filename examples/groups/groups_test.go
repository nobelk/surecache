// SPDX-License-Identifier: Apache-2.0

package groups_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nobelk/surecache"
	"github.com/nobelk/surecache/examples/groups"
	"github.com/nobelk/surecache/surecachepg"
)

// newDB creates a database for one test from SURECACHE_PG_DSN, the same contract as
// the surecachepg tests, and drops it afterwards. It skips when the DSN is unset.
func newDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SURECACHE_PG_DSN")
	if dsn == "" {
		t.Skip("SURECACHE_PG_DSN is not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(ctx) }()
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	name := "groups_example_" + hex.EncodeToString(b)
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn, cerr := pgx.Connect(ctx, dsn)
		if cerr != nil {
			t.Error(cerr)
			return
		}
		defer func() { _ = conn.Close(ctx) }()
		if _, cerr = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); cerr != nil {
			t.Error(cerr)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	if err = surecachepg.ConfigurePool(cfg, surecachepg.DefaultBounds()); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err = pool.Exec(ctx, groups.Schema); err != nil {
		t.Fatal(err)
	}
	return pool
}

// The decorator end to end: reads come from the cache and match the store, writes go
// through Mutate and Create, and Defer always reaches the store.
func TestCachedRepo_EndToEnd(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()
	store := groups.NewStore(pool, surecachepg.NewFence(7), surecachepg.DefaultBounds(), 100, 1000)
	repo, err := groups.NewCachedRepo(store, surecache.WithOutagePolicy(surecache.ServeWhileExact()),
		surecache.WithRebuildInterval(50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}

	// Before Prime every read defers to the store.
	if err := store.CreateGroup(ctx, "red", "Red"); err != nil {
		t.Fatal(err)
	}
	if ids, err := repo.Groups(ctx); err != nil || !slices.Equal(ids, []string{"red"}) {
		t.Fatalf("Groups before Prime = %v, %v", ids, err)
	}
	if r, err := repo.Cache().Prime(ctx); r != surecache.Installed {
		t.Fatalf("Prime = %v, %v", r, err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- repo.Cache().Run(runCtx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	steps := []struct {
		name string
		op   func() error
		want error
	}{
		{"create blue", func() error { return repo.CreateGroup(ctx, "blue", "Blue") }, nil},
		{"create blue again", func() error { return repo.CreateGroup(ctx, "blue", "Blue") }, groups.ErrExists},
		{"add ann to red", func() error { return repo.AddMember(ctx, "red", "ann") }, nil},
		{"add ann to blue", func() error { return repo.AddMember(ctx, "blue", "ann") }, groups.ErrMemberTaken},
		{"move ann", func() error { return repo.MoveMember(ctx, "ann", "red", "blue") }, nil},
		{"add bob to a missing group", func() error { return repo.AddMember(ctx, "green", "bob") }, groups.ErrNotFound},
	}
	for _, s := range steps {
		if err := s.op(); !errors.Is(err, s.want) {
			t.Fatalf("%s = %v, want %v", s.name, err, s.want)
		}
	}
	// Nothing-committed errors leave the cache trusted, so these come from memory.
	if _, a := repo.Cache().Get("blue"); a != surecache.Found {
		t.Fatalf("Get(blue) = %v; the cache should be trusted", a)
	}
	assertAgree(t, repo, store, []string{"ann", "bob"})

	if err := repo.DeleteGroup(ctx, "blue"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := repo.GroupForMember(ctx, "ann"); ok {
		t.Fatal("ann still has a group after its group was deleted")
	}
	assertAgree(t, repo, store, []string{"ann"})
}

// assertAgree checks that the decorator and the store give the same answers.
func assertAgree(t *testing.T, cached, store groups.Repo, members []string) {
	t.Helper()
	ctx := context.Background()
	want, err := store.Groups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := cached.Groups(ctx)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("Groups = %v, %v; store %v", got, err, want)
	}
	for _, id := range want {
		g1, ok1, err1 := cached.Group(ctx, id)
		g2, ok2, err2 := store.Group(ctx, id)
		if err1 != nil || err2 != nil || ok1 != ok2 || g1.Name != g2.Name || !slices.Equal(g1.Members, g2.Members) {
			t.Fatalf("Group(%s) = %v %t %v; store %v %t %v", id, g1, ok1, err1, g2, ok2, err2)
		}
	}
	for _, m := range members {
		id1, ok1, err1 := cached.GroupForMember(ctx, m)
		id2, ok2, err2 := store.GroupForMember(ctx, m)
		if err1 != nil || err2 != nil || id1 != id2 || ok1 != ok2 {
			t.Fatalf("GroupForMember(%s) = %q %t %v; store %q %t %v", m, id1, ok1, err1, id2, ok2, err2)
		}
	}
}
