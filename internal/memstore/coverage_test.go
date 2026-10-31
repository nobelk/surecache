// SPDX-License-Identifier: Apache-2.0

package memstore_test

import (
	"context"
	"slices"
	"testing"

	"github.com/nobelk/surecache/internal/memstore"
)

type record struct {
	ID     string
	Values []string
}

func testStore() *memstore.Store[string, record] {
	return memstore.New(memstore.Config[string, record]{
		Key:       func(r record) string { return r.ID },
		Clone:     func(r record) record { r.Values = slices.Clone(r.Values); return r },
		Cacheable: func(record) bool { return true },
	})
}

func TestTransactionSeesDeleteAndReinsert(t *testing.T) {
	s := testStore()
	s.Seed(record{ID: "a", Values: []string{"original"}})
	err := s.Write(context.Background(), func(tx *memstore.Tx[string, record]) error {
		if err := tx.Delete("a"); err != nil {
			return err
		}
		if got, ok := tx.Get("a"); ok || got.ID != "" || got.Values != nil {
			t.Fatalf("Get after Delete = %+v, %t", got, ok)
		}
		if err := tx.Delete("a"); err != memstore.ErrNotFound {
			t.Fatalf("repeated Delete = %v", err)
		}
		if err := tx.Insert(record{ID: "a", Values: []string{"replacement"}}); err != nil {
			return err
		}
		got, ok := tx.Get("a")
		if !ok || !slices.Equal(got.Values, []string{"replacement"}) {
			t.Fatalf("Get after Insert = %+v, %t", got, ok)
		}
		got.Values[0] = "mutated"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get("a")
	if !ok || !slices.Equal(got.Values, []string{"replacement"}) {
		t.Fatalf("committed record = %+v, %t", got, ok)
	}
}

func TestCanceledWriteDoesNotInvokeCallback(t *testing.T) {
	s := testStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.Write(ctx, func(*memstore.Tx[string, record]) error {
		t.Fatal("canceled Write invoked callback")
		return nil
	})
	if err != context.Canceled {
		t.Fatalf("Write = %v", err)
	}
	if s.Version() != 0 || len(s.State()) != 0 {
		t.Fatal("canceled write changed state")
	}
	if _, err := s.LoadAll(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestInjectedCommitOutcomesPreserveConflict(t *testing.T) {
	for _, kind := range []memstore.WriteFaultKind{memstore.AmbiguousCommitted, memstore.ContextAfterCommit} {
		t.Run(map[memstore.WriteFaultKind]string{memstore.AmbiguousCommitted: "ambiguous", memstore.ContextAfterCommit: "context"}[kind], func(t *testing.T) {
			s := testStore()
			s.Seed(record{ID: "a", Values: []string{"original"}})
			s.InjectWrite(memstore.WriteFault{Kind: kind})
			err := s.Write(context.Background(), func(tx *memstore.Tx[string, record]) error {
				tx.Put(record{ID: "a", Values: []string{"loser"}})
				tx.Put(record{ID: "b"})
				// Commit an overlapping transaction after the outer transaction staged its writes.
				return s.Write(context.Background(), func(other *memstore.Tx[string, record]) error {
					other.Put(record{ID: "a", Values: []string{"winner"}})
					return nil
				})
			})
			if err != memstore.ErrConflict || !memstore.CommittedNothing(err) {
				t.Fatalf("Write = %v, want nothing-committed conflict", err)
			}
			got, ok := s.Get("a")
			if !ok || !slices.Equal(got.Values, []string{"winner"}) {
				t.Fatalf("winner = %+v, %t", got, ok)
			}
			if _, ok := s.Get("b"); ok {
				t.Fatal("conflicting write partially committed")
			}
			if s.Version() != 2 {
				t.Fatalf("version = %d, want 2", s.Version())
			}
			if _, err := s.LoadAll(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLoadAllReadFailureIsConsumed(t *testing.T) {
	s := testStore()
	s.Seed(record{ID: "a"})
	s.InjectLoadAll(memstore.ReadFault{Kind: memstore.FailRead})
	if got, err := s.LoadAll(context.Background()); got != nil || err != memstore.ErrAmbiguous {
		t.Fatalf("failed LoadAll = %v, %v", got, err)
	}
	if got, err := s.LoadAll(context.Background()); err != nil || len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("next LoadAll = %v, %v", got, err)
	}
}
