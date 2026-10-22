// SPDX-License-Identifier: Apache-2.0

package memstore

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
)

type item struct {
	ID   string
	Tags []string
	Hide bool
}

func newStore(capacity int) *Store[string, item] {
	return New(Config[string, item]{
		Key:       func(i item) string { return i.ID },
		Clone:     func(i item) item { i.Tags = slices.Clone(i.Tags); return i },
		Cacheable: func(i item) bool { return !i.Hide },
		Capacity:  capacity,
	})
}

func TestStore_LoadOne(t *testing.T) {
	s := newStore(0)
	s.Seed(item{ID: "a", Tags: []string{"x"}})
	got, err := s.LoadOne(context.Background(), "a")
	if err != nil || got.ID != "a" {
		t.Fatalf("LoadOne = %v, %v", got, err)
	}
	got.Tags[0] = "mutated"
	if again, _ := s.LoadOne(context.Background(), "a"); again.Tags[0] != "x" {
		t.Fatal("LoadOne must return a detached copy")
	}
	if _, err := s.LoadOne(context.Background(), "b"); !s.NotFound(err) || !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent LoadOne = %v", err)
	}
	if s.Calls().LoadOne != 3 {
		t.Fatalf("calls = %+v", s.Calls())
	}
}

func TestStore_LoadAllFiltersAndRefusesOverCapacity(t *testing.T) {
	s := newStore(2)
	s.Seed(item{ID: "a"}, item{ID: "b", Hide: true}, item{ID: "c"})
	all, err := s.LoadAll(context.Background())
	if err != nil || len(all) != 2 {
		t.Fatalf("LoadAll = %v, %v", all, err)
	}
	s.Seed(item{ID: "d"})
	all, err = s.LoadAll(context.Background())
	if !errors.Is(err, ErrOverCapacity) || all != nil {
		t.Fatalf("over capacity LoadAll = %v, %v", all, err)
	}
}

func TestStore_LoadAllContextErrorsUnwrapped(t *testing.T) {
	s := newStore(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.LoadAll(ctx); err != context.Canceled {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.LoadOne(ctx, "a"); err != context.Canceled {
		t.Fatalf("err = %v", err)
	}
}

func TestStore_WriteCommitsOrNothing(t *testing.T) {
	s := newStore(0)
	ctx := context.Background()
	s.Seed(item{ID: "a"})
	err := s.Write(ctx, func(tx *Tx[string, item]) error {
		tx.Put(item{ID: "b"})
		return tx.Insert(item{ID: "a"})
	})
	if !errors.Is(err, ErrExists) || !CommittedNothing(err) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := s.Get("b"); ok {
		t.Fatal("failed write must commit nothing")
	}
	err = s.Write(ctx, func(tx *Tx[string, item]) error {
		if ierr := tx.Insert(item{ID: "b"}); ierr != nil {
			return ierr
		}
		if uerr := tx.Update("a", func(i item) (item, error) { i.Tags = []string{"t"}; return i, nil }); uerr != nil {
			return uerr
		}
		if _, ok := tx.Get("b"); !ok {
			t.Error("tx must see its own writes")
		}
		return tx.Delete("b")
	})
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Get("a"); len(a.Tags) != 1 {
		t.Fatalf("a = %v", a)
	}
	if _, ok := s.Get("b"); ok {
		t.Fatal("b deleted in the same tx")
	}
	for _, op := range []func(*Tx[string, item]) error{
		func(tx *Tx[string, item]) error { return tx.Delete("zz") },
		func(tx *Tx[string, item]) error {
			return tx.Update("zz", func(i item) (item, error) { return i, nil })
		},
		func(tx *Tx[string, item]) error {
			return tx.Update("a", func(i item) (item, error) { return i, ErrPrecondition })
		},
	} {
		if err := s.Write(ctx, op); !CommittedNothing(err) {
			t.Fatalf("err = %v, want nothing-committed", err)
		}
	}
	if CommittedNothing(errors.New("other")) || CommittedNothing(context.Canceled) {
		t.Fatal("only the declared errors are nothing-committed")
	}
	if len(s.State()) != 1 {
		t.Fatalf("state = %v", s.State())
	}
}

func TestStore_LoadAllWaitsForOpenMutation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newStore(0)
		ctx := context.Background()
		inside, release := make(chan struct{}), make(chan struct{})
		go func() {
			_ = s.Write(ctx, func(tx *Tx[string, item]) error {
				tx.Put(item{ID: "late"})
				close(inside)
				<-release
				return nil
			})
		}()
		<-inside
		done := make(chan []item, 1)
		go func() {
			all, _ := s.LoadAll(ctx)
			done <- all
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("LoadAll did not wait for the open mutation")
		default:
		}
		close(release)
		if all := <-done; len(all) != 1 || all[0].ID != "late" {
			t.Fatalf("LoadAll = %v, want the late commit", all)
		}
	})
}

func TestStore_LockProbe(t *testing.T) {
	s := newStore(0)
	var reports []string
	held := true
	s.SetLockProbe(func() bool { return held }, func(msg string) { reports = append(reports, msg) })
	ctx := context.Background()
	_, _ = s.LoadOne(ctx, "a")
	_, _ = s.LoadAll(ctx)
	_ = s.Write(ctx, func(*Tx[string, item]) error { return nil })
	if len(reports) != 3 {
		t.Fatalf("reports = %v", reports)
	}
	held = false
	_, _ = s.LoadOne(ctx, "a")
	if len(reports) != 3 {
		t.Fatal("probe reported while the lock was free")
	}
}

// Design 4.1: preconditions are checked atomically with the write. Two transactions that
// insert the same id concurrently never both succeed.
func TestStore_ConcurrentConflictCommitsNothing(t *testing.T) {
	s := newStore(0)
	ctx := context.Background()
	inA, releaseA := make(chan struct{}), make(chan struct{})
	errA := make(chan error, 1)
	go func() {
		errA <- s.Write(ctx, func(tx *Tx[string, item]) error {
			if err := tx.Insert(item{ID: "x", Tags: []string{"a"}}); err != nil {
				return err
			}
			close(inA)
			<-releaseA
			return nil
		})
	}()
	<-inA
	errB := s.Write(ctx, func(tx *Tx[string, item]) error {
		return tx.Insert(item{ID: "x", Tags: []string{"b"}})
	})
	close(releaseA)
	if errB != nil {
		t.Fatalf("first committer: %v", errB)
	}
	if err := <-errA; !errors.Is(err, ErrConflict) || !CommittedNothing(err) {
		t.Fatalf("second committer = %v, want ErrConflict", err)
	}
	if x, _ := s.Get("x"); x.Tags[0] != "b" {
		t.Fatalf("x = %v, want the first commit kept", x)
	}
	// A blind Put of a concurrently changed id conflicts too.
	inA, releaseA = make(chan struct{}), make(chan struct{})
	go func() {
		errA <- s.Write(ctx, func(tx *Tx[string, item]) error {
			tx.Put(item{ID: "x", Tags: []string{"c"}})
			close(inA)
			<-releaseA
			return nil
		})
	}()
	<-inA
	if err := s.Write(ctx, func(tx *Tx[string, item]) error { return tx.Delete("x") }); err != nil {
		t.Fatal(err)
	}
	close(releaseA)
	if err := <-errA; !errors.Is(err, ErrConflict) {
		t.Fatalf("blind put = %v", err)
	}
}
