// SPDX-License-Identifier: Apache-2.0

package groupstore

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nobelk/surecache"
	"github.com/nobelk/surecache/surecachepg"
	"github.com/nobelk/surecache/surecachepg/internal/pgtest"
	"github.com/nobelk/surecache/surecachetest"
)

// Conformance capacities.
const (
	ConformanceCapacity       = 10
	ConformanceMemberCapacity = 20
	fenceKey                  = 0x5c4e_c0de
)

// Open creates a fresh database with the schema and returns a store over it.
func Open(t testing.TB, capacity, memberCapacity int) *Store {
	t.Helper()
	cfg := pgtest.DB(t)
	if err := surecachepg.ConfigurePool(cfg, surecachepg.DefaultBounds()); err != nil {
		t.Fatal(err)
	}
	pool := pgtest.Pool(t, cfg)
	if err := Install(context.Background(), pool); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return &Store{
		Pool: pool, Fence: surecachepg.NewFence(fenceKey), Bounds: surecachepg.DefaultBounds(),
		Capacity: capacity, MemberCapacity: memberCapacity,
	}
}

// Harness is the surecachetest harness for the groups store.
func Harness() surecachetest.Harness[string, Group] {
	return surecachetest.Harness[string, Group]{
		Spec: Spec(ConformanceCapacity),
		IndexKeys: map[string]func(Group) []any{
			"member": func(g Group) []any { return anys(MemberKeys(g)) },
			"owner":  func(g Group) []any { return anys(OwnerKeys(g)) },
		},
		NewStore: func(t testing.TB) surecachetest.Store[string, Group] {
			return &conformance{s: Open(t, ConformanceCapacity, ConformanceMemberCapacity)}
		},
	}
}

func anys(keys []string) []any {
	out := make([]any, len(keys))
	for i, k := range keys {
		out[i] = k
	}
	return out
}

// conformance adapts Store to surecachetest.Store.
type conformance struct {
	s *Store
}

var (
	_ surecachetest.Store[string, Group] = (*conformance)(nil)
	_ surecachetest.SnapshotProbe        = (*conformance)(nil)
)

func (c *conformance) Source() surecache.Source[string, Group] { return c.s }
func (c *conformance) SetReadPause(pause func())               { c.s.SetReadPause(pause) }
func (c *conformance) Seed(ctx context.Context, gs []Group) error {
	return c.s.Seed(ctx, gs)
}

// Fixtures cover every shape: cacheable with keys (the first two, with members, so a
// mutation touches both tables), cacheable without keys, and soft-deleted.
func (c *conformance) Fixtures() []Group {
	return []Group{
		{ID: "g1", Name: "alpha", Owner: "o1", Members: []Member{{Name: "m1"}, {Name: "m2"}}},
		{ID: "g2", Name: "beta", Members: []Member{{Name: "m3"}}},
		{ID: "g3", Name: "gamma"},
		{ID: "g4", Name: "archived", Deleted: true},
	}
}

func (c *conformance) NothingCommittedCases() []surecachetest.Case {
	return []surecachetest.Case{
		{Name: "not found", Op: func(ctx context.Context) error { return c.s.Rename(ctx, "zz") }},
		{Name: "group exists", Op: func(ctx context.Context) error {
			return c.s.Insert(ctx, Group{ID: "g1", Name: "dup"})
		}},
		{Name: "member taken", Op: func(ctx context.Context) error { return c.s.AddMember(ctx, "g2", "m1") }},
		{Name: "owner taken", Op: func(ctx context.Context) error { return c.s.SetOwner(ctx, "g2", "o1") }},
		{Name: "owner on deleted group", Op: func(ctx context.Context) error { return c.s.SetOwner(ctx, "g4", "o9") }},
		{Name: "member on deleted group", Op: func(ctx context.Context) error { return c.s.AddMember(ctx, "g4", "m9") }},
		{Name: "soft delete with members", Op: func(ctx context.Context) error {
			return c.s.mutate(ctx, func(tx pgx.Tx) error {
				return exec(ctx, tx, `UPDATE groups SET deleted = true, owner = NULL WHERE id = 'g1'`)
			})
		}},
	}
}

func (c *conformance) AbsentID() string { return "zz" }

func (c *conformance) Dump(ctx context.Context) (any, error) { return c.s.Dump(ctx) }

func (c *conformance) Update(ctx context.Context, id string) (Group, error) {
	open, err := c.begin(ctx, id)
	if err != nil {
		return Group{}, err
	}
	return open.Want(), open.Commit(ctx)
}

func (c *conformance) Delete(ctx context.Context, id string) error { return c.s.Delete(ctx, id) }

func (c *conformance) BeginMutation(ctx context.Context, id string) (surecachetest.OpenMutation[Group], error) {
	return c.begin(ctx, id)
}

func (c *conformance) BeginPairMutation(ctx context.Context, a, b string) (surecachetest.OpenPairMutation[Group], error) {
	return c.begin(ctx, a, b)
}

// begin opens a mutation transaction holding the fence shared, touches every id in both
// tables, and reads the after state inside the transaction.
func (c *conformance) begin(ctx context.Context, ids ...string) (*openTx, error) {
	tx, err := c.s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, unwrap(ctx, err)
	}
	o := &openTx{tx: tx}
	fail := func(err error) (*openTx, error) {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return nil, unwrap(ctx, err)
	}
	if err := c.s.Fence.Shared(ctx, tx); err != nil {
		return fail(err)
	}
	for _, id := range ids {
		if err := Touch(ctx, tx, id); err != nil {
			return fail(err)
		}
		g, err := readGroup(ctx, tx, id, func() {})
		if err != nil {
			return fail(err)
		}
		o.want = append(o.want, g)
	}
	return o, nil
}

type openTx struct {
	tx   pgx.Tx
	want []Group
}

func (o *openTx) Want() Group            { return o.want[0] }
func (o *openTx) WantPair() (a, b Group) { return o.want[0], o.want[1] }
func (o *openTx) Commit(ctx context.Context) error {
	return o.tx.Commit(ctx)
}

func (o *openTx) Rollback(ctx context.Context) error {
	if err := o.tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return err
	}
	return nil
}

func (c *conformance) SeedOverCapacity(ctx context.Context) error {
	gs := make([]Group, 0, ConformanceCapacity+1)
	for i := range ConformanceCapacity + 1 {
		gs = append(gs, Group{ID: fmt.Sprintf("c%02d", i), Name: "over"})
	}
	return c.s.Seed(ctx, gs)
}

func (c *conformance) FenceWaitBound() time.Duration { return c.s.Bounds.FenceWait() }
