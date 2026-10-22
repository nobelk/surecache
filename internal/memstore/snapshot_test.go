// SPDX-License-Identifier: Apache-2.0

package memstore

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// part is a two-part entity: Head and Tail live in different "tables".
type part struct {
	ID   string
	Head int
	Tail int
}

func newPartStore(mixed bool) *Store[string, part] {
	return New(Config[string, part]{
		Key:           func(p part) string { return p.ID },
		Clone:         func(p part) part { return p },
		Cacheable:     func(part) bool { return true },
		Join:          func(head, tail part) part { head.Tail = tail.Tail; return head },
		MixedSnapshot: mixed,
	})
}

func bump(s *Store[string, part], ids ...string) {
	_ = s.Write(context.Background(), func(tx *Tx[string, part]) error {
		for _, id := range ids {
			if err := tx.Update(id, func(p part) (part, error) { p.Head++; p.Tail++; return p, nil }); err != nil {
				return err
			}
		}
		return nil
	})
}

func TestTwoPart_ConsistentSnapshot(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		s := newPartStore(mixed)
		s.Seed(part{ID: "a"}, part{ID: "b"})
		pauses := 0
		s.SetReadPause(func() {
			pauses++
			bump(s, "a", "b")
		})
		one, err := s.LoadOne(context.Background(), "a")
		if err != nil {
			t.Fatal(err)
		}
		if (one.Head != one.Tail) != mixed {
			t.Fatalf("mixed=%t LoadOne = %+v", mixed, one)
		}
		all, err := s.LoadAll(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range all {
			if (p.Head != p.Tail) != mixed {
				t.Fatalf("mixed=%t LoadAll = %+v", mixed, all)
			}
		}
		if pauses != 2 {
			t.Fatalf("pauses = %d", pauses)
		}
	}
}

// A two-part entity deleted between the statements of a mixed read is not found.
func TestTwoPart_MixedDelete(t *testing.T) {
	s := newPartStore(true)
	s.Seed(part{ID: "a"})
	s.SetReadPause(func() {
		_ = s.Write(context.Background(), func(tx *Tx[string, part]) error { return tx.Delete("a") })
	})
	if _, err := s.LoadOne(context.Background(), "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LoadOne = %v", err)
	}
	s.Seed(part{ID: "a"})
	if all, err := s.LoadAll(context.Background()); err != nil || len(all) != 0 {
		t.Fatalf("LoadAll = %v, %v", all, err)
	}
}

// The exclusive fence wait is bounded by Config.FenceWait.
func TestStore_FenceWaitBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Config[string, item]{
			Key: func(i item) string { return i.ID }, Clone: func(i item) item { return i },
			Cacheable: func(item) bool { return true }, FenceWait: time.Second,
		})
		release := make(chan struct{})
		s.InjectWrite(WriteFault{Kind: BlockWrite, Release: release})
		go func() { _ = s.Write(context.Background(), putX) }()
		synctest.Wait()
		start := time.Now()
		if _, err := s.LoadAll(context.Background()); !errors.Is(err, ErrFenceTimeout) {
			t.Fatalf("LoadAll = %v", err)
		}
		if time.Since(start) != time.Second {
			t.Fatalf("waited %v", time.Since(start))
		}
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(100*time.Millisecond, cancel)
		if _, err := s.LoadAll(ctx); err != context.Canceled {
			t.Fatalf("cancelled LoadAll = %v", err)
		}
		close(release)
	})
}
