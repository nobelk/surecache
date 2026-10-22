// SPDX-License-Identifier: Apache-2.0

package memstore

import (
	"context"
	"sync"
)

// Fence is a reader-writer lock whose acquisition honors a context, standing in for the
// store-side mutation fence of design section 4.3: mutations hold it shared, a bulk load
// takes it exclusively. A waiting exclusive request blocks new shared holders, as
// PostgreSQL's lock queue does, so a bulk load cannot starve. The zero Fence is unlocked.
type Fence struct {
	mu             sync.Mutex
	shared         int
	exclusive      bool
	waitingWriters int
	changed        chan struct{} // closed and replaced on every release
}

// RLock acquires the fence in shared mode. It returns ctx.Err() unwrapped if ctx ends
// first, including when ctx is already done.
func (f *Fence) RLock(ctx context.Context) error {
	return f.acquire(ctx, func() bool {
		if f.exclusive || f.waitingWriters > 0 {
			return false
		}
		f.shared++
		return true
	})
}

// RUnlock releases a shared hold.
func (f *Fence) RUnlock() {
	f.mu.Lock()
	f.shared--
	f.broadcastLocked()
	f.mu.Unlock()
}

// Lock acquires the fence exclusively. It returns ctx.Err() unwrapped if ctx ends first.
func (f *Fence) Lock(ctx context.Context) error {
	f.mu.Lock()
	f.waitingWriters++
	f.mu.Unlock()
	err := f.acquire(ctx, func() bool {
		if f.exclusive || f.shared > 0 {
			return false
		}
		f.exclusive = true
		return true
	})
	f.mu.Lock()
	f.waitingWriters--
	f.broadcastLocked() // an abandoned exclusive wait may unblock shared holders
	f.mu.Unlock()
	return err
}

// Unlock releases the exclusive hold.
func (f *Fence) Unlock() {
	f.mu.Lock()
	f.exclusive = false
	f.broadcastLocked()
	f.mu.Unlock()
}

// acquire waits until try, called under f.mu, succeeds or ctx ends.
func (f *Fence) acquire(ctx context.Context, try func() bool) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		f.mu.Lock()
		if try() {
			f.mu.Unlock()
			return nil
		}
		ch := f.changedLocked()
		f.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (f *Fence) changedLocked() chan struct{} {
	if f.changed == nil {
		f.changed = make(chan struct{})
	}
	return f.changed
}

func (f *Fence) broadcastLocked() {
	if f.changed != nil {
		close(f.changed)
		f.changed = nil
	}
}
