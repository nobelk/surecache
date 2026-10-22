// SPDX-License-Identifier: Apache-2.0

package memstore

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func putX(tx *Tx[string, item]) error {
	tx.Put(item{ID: "x"})
	return nil
}

func TestWriteFaults(t *testing.T) {
	custom := errors.New("custom")
	cases := []struct {
		name       string
		fault      WriteFault
		wantErr    error
		wantCommit bool
	}{
		{"nothing committed", WriteFault{Kind: NothingCommitted}, ErrPrecondition, false},
		{"nothing committed custom", WriteFault{Kind: NothingCommitted, Err: ErrExists}, ErrExists, false},
		{"ambiguous committed", WriteFault{Kind: AmbiguousCommitted}, ErrAmbiguous, true},
		{"ambiguous not committed", WriteFault{Kind: AmbiguousNotCommitted, Err: custom}, custom, false},
		{"context after commit", WriteFault{Kind: ContextAfterCommit}, context.Canceled, true},
		{"deadline after commit", WriteFault{Kind: ContextAfterCommit, Err: context.DeadlineExceeded}, context.DeadlineExceeded, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(0)
			s.InjectWrite(tc.fault)
			err := s.Write(context.Background(), putX)
			if err != tc.wantErr {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if _, ok := s.Get("x"); ok != tc.wantCommit {
				t.Fatalf("committed = %t, want %t", ok, tc.wantCommit)
			}
			// Faults are consumed one per call.
			if err := s.Write(context.Background(), putX); err != nil {
				t.Fatalf("second write: %v", err)
			}
		})
	}
}

func TestWriteFault_FnErrorWins(t *testing.T) {
	s := newStore(0)
	s.InjectWrite(WriteFault{Kind: AmbiguousCommitted})
	if err := s.Write(context.Background(), func(*Tx[string, item]) error { return ErrPrecondition }); err != ErrPrecondition {
		t.Fatalf("err = %v", err)
	}
}

func TestWriteFault_Panic(t *testing.T) {
	s := newStore(0)
	s.InjectWrite(WriteFault{Kind: PanicWrite, PanicValue: "boom"})
	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Fatalf("recover = %v", r)
			}
		}()
		_ = s.Write(context.Background(), putX)
	}()
	if _, ok := s.Get("x"); ok {
		t.Fatal("panicking write committed")
	}
	// The fence was released during unwinding.
	if _, err := s.LoadAll(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// SAFE-5: an abandoned commit returns an ambiguous error at once, keeps holding the
// shared fence, and commits later.
func TestWriteFault_AbandonedCommit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newStore(0)
		release := make(chan struct{})
		s.InjectWrite(WriteFault{Kind: AbandonedCommit, Release: release})
		if err := s.Write(context.Background(), putX); err != ErrAmbiguous {
			t.Fatalf("err = %v", err)
		}
		if _, ok := s.Get("x"); ok {
			t.Fatal("committed before release")
		}
		loaded := make(chan []item, 1)
		go func() {
			all, _ := s.LoadAll(context.Background())
			loaded <- all
		}()
		synctest.Wait()
		select {
		case <-loaded:
			t.Fatal("LoadAll passed the fence while the abandoned commit held it")
		default:
		}
		close(release)
		if all := <-loaded; len(all) != 1 {
			t.Fatalf("LoadAll = %v, want the late commit", all)
		}
	})
}

func TestWriteFault_Block(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newStore(0)
		release := make(chan struct{})
		s.InjectWrite(WriteFault{Kind: BlockWrite, Release: release}, WriteFault{Kind: BlockWrite, Release: make(chan struct{})})
		done := make(chan error, 1)
		go func() { done <- s.Write(context.Background(), putX) }()
		synctest.Wait()
		if _, ok := s.Get("x"); ok {
			t.Fatal("committed while blocked")
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Write(ctx, func(tx *Tx[string, item]) error { return tx.Delete("x") }); err != context.DeadlineExceeded {
			t.Fatalf("err = %v", err)
		}
		if _, ok := s.Get("x"); !ok {
			t.Fatal("timed-out blocked write committed")
		}
	})
}

func TestReadFaults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newStore(0)
		s.Seed(item{ID: "a"})
		ctx := context.Background()
		custom := errors.New("custom")
		s.InjectLoadOne(ReadFault{Kind: FailRead}, ReadFault{Kind: FailRead, Err: custom})
		if _, err := s.LoadOne(ctx, "a"); err != ErrAmbiguous {
			t.Fatalf("err = %v", err)
		}
		if _, err := s.LoadOne(ctx, "a"); err != custom {
			t.Fatalf("err = %v", err)
		}
		release := make(chan struct{})
		s.InjectLoadAll(ReadFault{Kind: BlockRead, Release: release})
		got := make(chan int, 1)
		go func() {
			all, _ := s.LoadAll(ctx)
			got <- len(all)
		}()
		synctest.Wait()
		close(release)
		if n := <-got; n != 1 {
			t.Fatalf("LoadAll after release = %d", n)
		}
		tctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		s.InjectLoadOne(ReadFault{Kind: BlockRead, Release: make(chan struct{})})
		if _, err := s.LoadOne(tctx, "a"); err != context.DeadlineExceeded {
			t.Fatalf("blocked LoadOne = %v", err)
		}
		s.InjectLoadAll(ReadFault{Kind: PanicRead, PanicValue: 7})
		defer func() {
			if r := recover(); r != 7 {
				t.Fatalf("recover = %v", r)
			}
		}()
		_, _ = s.LoadAll(ctx)
	})
}
