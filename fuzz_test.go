// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/nobelk/surecache/internal/memstore"
)

// decodeGroups turns bytes into entities, three bytes each, over a small id and key
// space so that collisions are likely.
func decodeGroups(data []byte) []group {
	var out []group
	for i := 0; i+2 < len(data); i += 3 {
		e := group{ID: fmt.Sprint("g", data[i]%8), Deleted: data[i+1]&0x80 != 0}
		if o := data[i+1] % 5; o != 0 {
			e.Owner = fmt.Sprint("o", o)
		}
		if m := data[i+2] % 7; m != 0 {
			e.Members = append(e.Members, fmt.Sprint("k", m))
		}
		if m := data[i+2] / 64; m != 0 {
			e.Members = append(e.Members, fmt.Sprint("k", m))
		}
		out = append(out, e)
	}
	return out
}

// FuzzBulkInstallContract: buildAside either fails with ErrContract or ErrOverCapacity,
// or returns maps that satisfy the structural invariant and hold exactly the input set
// (SAFE-8, SAFE-12). A valid input that fits must install.
func FuzzBulkInstallContract(f *testing.F) {
	f.Add([]byte{0, 1, 1, 1, 2, 2})
	f.Add([]byte{0, 1, 1, 0, 2, 2})                                     // duplicate primary key
	f.Add([]byte{0, 1, 1, 1, 2, 1})                                     // duplicate member key
	f.Add([]byte{0, 0x81, 0})                                           // non-cacheable
	f.Add([]byte{0, 0, 0, 1, 0, 0, 2, 0, 0, 3, 0, 0, 4, 0, 0, 5, 0, 0}) // over capacity
	f.Fuzz(func(t *testing.T, data []byte) {
		const capacity, memberCap, ownerCap = 5, 4, 3
		fx := newFixture(t, withCapacity(capacity, memberCap, ownerCap))
		all := decodeGroups(data)
		snap, err := fx.c.buildAside(all)
		valid, fits := classify(all, capacity, memberCap, ownerCap)
		if err != nil {
			if !errors.Is(err, ErrContract) && !errors.Is(err, ErrOverCapacity) {
				t.Fatalf("unexpected error %v", err)
			}
			if valid && fits {
				t.Fatalf("valid input that fits was refused: %v", err)
			}
			if valid && !errors.Is(err, ErrOverCapacity) {
				t.Fatalf("valid input refused with %v", err)
			}
			return
		}
		if !valid || !fits {
			t.Fatalf("invalid input installed (valid=%t fits=%t): %v", valid, fits, all)
		}
		fx.c.mu.Lock()
		fx.c.swapInLocked(snap)
		fx.c.stale = false
		fx.c.mu.Unlock()
		if err := checkStructure(fx.c); err != nil {
			t.Fatal(err)
		}
		if fx.c.entities.Len() != len(all) {
			t.Fatalf("installed %d of %d", fx.c.entities.Len(), len(all))
		}
		for _, e := range all {
			if got, a := fx.c.Get(e.ID); a != Found || !reflect.DeepEqual(got, e) {
				t.Fatalf("Get(%s) = %v, %v", e.ID, got, a)
			}
		}
	})
}

// classify reports whether a bulk load keeps the contract and whether it fits.
func classify(all []group, capacity, memberCap, ownerCap int) (valid, fits bool) {
	ids, members, owners := map[string]bool{}, map[string]bool{}, map[string]bool{}
	valid = true
	for _, e := range all {
		if e.Deleted || ids[e.ID] {
			valid = false
		}
		ids[e.ID] = true
		for _, k := range distinct(e.Members) {
			if members[k] {
				valid = false
			}
			members[k] = true
		}
		for _, k := range ownerKeys(e.Owner) {
			if owners[k] {
				valid = false
			}
			owners[k] = true
		}
	}
	return valid, len(ids) <= capacity && len(members) <= memberCap && len(owners) <= ownerCap
}

// FuzzRefreshSequence drives one goroutine through store writes and cache mutations. A
// flag on an op runs the next op inside its refresh, between LoadOne and the install,
// which chooses the refresh install order. After each step the cache is stale, or it
// satisfies the structural invariant and matches the store exactly.
func FuzzRefreshSequence(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8})
	f.Add([]byte{0x81, 0x12, 0x23, 0x34, 0x45})
	f.Add([]byte{0x91, 0x01, 0x92, 0x02, 0x08, 0x13})
	f.Fuzz(func(t *testing.T, data []byte) {
		fx := newFixture(t, concurrent(), withCapacity(6, 5, 4))
		fx.prime(g("g0", "o1", "k1"), g("g1", "", "k2"), g("g2", "o2"))
		ops := data
		var step func(nested bool)
		step = func(nested bool) {
			if len(ops) < 2 {
				return
			}
			op, arg := ops[0], ops[1]
			ops = ops[2:]
			if op&0x80 != 0 && !nested { // run the next op inside this op's refresh
				var done bool
				fx.c.hooks = &hooks{afterLoadOne: func(any) {
					if !done {
						done = true
						fx.c.hooks = nil
						step(true)
					}
				}}
			}
			fuzzOp(fx, op&0x7f, arg)
			fx.c.hooks = nil
		}
		for len(ops) >= 2 {
			step(false)
			assertMatchesStore(t, fx)
		}
	})
}

// fuzzOp runs one cache mutation chosen by op over ids and keys chosen by arg.
func fuzzOp(fx *fixture, op, arg byte) {
	ctx := context.Background()
	x, y := fmt.Sprint("g", arg%5), fmt.Sprint("g", arg/5%5)
	k, o := fmt.Sprint("k", arg%4), fmt.Sprint("o", arg%3)
	write := func(fn func(tx *txn) error) func() error {
		return func() error { return fx.store.Write(ctx, fn) }
	}
	switch op % 9 {
	case 0:
		_ = fx.c.Mutate(ctx, []string{x}, write(func(tx *txn) error {
			return tx.Update(x, func(e group) (group, error) { e.Name = fmt.Sprint(arg); return e, nil })
		}))
	case 1:
		_ = fx.c.Mutate(ctx, []string{x, y}, write(func(tx *txn) error { return moveMember(tx, k, x, y) }))
	case 2:
		_ = fx.c.Mutate(ctx, []string{x}, write(func(tx *txn) error { return setOwner(tx, o, x) }))
	case 3:
		_, _ = fx.c.Create(ctx, func() (string, error) {
			return x, write(func(tx *txn) error { return tx.Insert(g(x, "")) })()
		})
	case 4:
		_ = fx.c.Mutate(ctx, []string{x}, write(func(tx *txn) error { return tx.Delete(x) }))
	case 5:
		_ = fx.c.Mutate(ctx, []string{x}, write(func(tx *txn) error {
			return tx.Update(x, func(e group) (group, error) {
				if e.Deleted = !e.Deleted; e.Deleted {
					e.Owner, e.Members = "", nil
				}
				return e, nil
			})
		}))
	case 6:
		fx.store.InjectWrite(memstore.WriteFault{Kind: memstore.NothingCommitted})
		_ = fx.c.Mutate(ctx, []string{x}, write(func(tx *txn) error { return tx.Delete(x) }))
	case 7:
		fx.store.InjectWrite(memstore.WriteFault{Kind: memstore.AmbiguousCommitted})
		_ = fx.c.Mutate(ctx, []string{x}, write(func(tx *txn) error { return tx.Delete(x) }))
	case 8:
		fx.c.tick(ctx) // the loop's rebuild, if stale
	}
}

// assertMatchesStore checks that the cache is stale, or that it satisfies the
// structural invariant and holds exactly the store's cacheable state.
func assertMatchesStore(t *testing.T, fx *fixture) {
	t.Helper()
	if fx.isStale() {
		return
	}
	if err := checkStructure(fx.c); err != nil {
		t.Fatal(err)
	}
	var want []string
	for id, e := range fx.store.State() {
		if !groupCacheable(e) {
			continue
		}
		want = append(want, id)
		if got, a := fx.c.Get(id); a != Found || !reflect.DeepEqual(got, e) {
			t.Fatalf("Get(%s) = %v, %v; store holds %v", id, got, a, e)
		}
	}
	slices.Sort(want)
	if ids, _ := fx.c.List(); !slices.Equal(ids, want) {
		t.Fatalf("List = %v, want %v", ids, want)
	}
}
