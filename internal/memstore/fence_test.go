// SPDX-License-Identifier: Apache-2.0

package memstore

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

func TestFence_SharedHoldersCoexist(t *testing.T) {
	var f Fence
	ctx := context.Background()
	if err := f.RLock(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.RLock(ctx); err != nil {
		t.Fatal(err)
	}
	f.RUnlock()
	f.RUnlock()
}

func TestFence_ExclusiveWaitsForShared(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var f Fence
		ctx := context.Background()
		if err := f.RLock(ctx); err != nil {
			t.Fatal(err)
		}
		got := make(chan error, 1)
		go func() { got <- f.Lock(ctx) }()
		synctest.Wait()
		select {
		case err := <-got:
			t.Fatalf("exclusive granted while shared held: %v", err)
		default:
		}
		f.RUnlock()
		if err := <-got; err != nil {
			t.Fatal(err)
		}
		f.Unlock()
	})
}

func TestFence_WaitingExclusiveBlocksNewShared(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var f Fence
		ctx := context.Background()
		_ = f.RLock(ctx)
		excl := make(chan error, 1)
		go func() { excl <- f.Lock(ctx) }()
		synctest.Wait()
		shared := make(chan error, 1)
		go func() { shared <- f.RLock(ctx) }()
		synctest.Wait()
		select {
		case err := <-shared:
			t.Fatalf("new shared holder overtook a waiting exclusive: %v", err)
		default:
		}
		f.RUnlock()
		if err := <-excl; err != nil {
			t.Fatal(err)
		}
		f.Unlock()
		if err := <-shared; err != nil {
			t.Fatal(err)
		}
		f.RUnlock()
	})
}

func TestFence_ContextEndsWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var f Fence
		_ = f.Lock(context.Background())
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := f.RLock(ctx); err != context.DeadlineExceeded {
			t.Fatalf("RLock = %v, want context.DeadlineExceeded unwrapped", err)
		}
		if err := f.Lock(ctx); err != context.DeadlineExceeded {
			t.Fatalf("Lock = %v, want context.DeadlineExceeded unwrapped", err)
		}
		f.Unlock()
		// The abandoned exclusive wait must not block shared holders.
		if err := f.RLock(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.RUnlock()
	})
}

func TestFence_AlreadyCancelled(t *testing.T) {
	var f Fence
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.RLock(ctx); err != context.Canceled {
		t.Fatalf("RLock = %v", err)
	}
	if err := f.Lock(ctx); err != context.Canceled {
		t.Fatalf("Lock = %v", err)
	}
}
