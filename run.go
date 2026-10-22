// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Run is the single background rebuilder (FUN-5). Every rebuild interval it checks the
// cache and, if stale, rebuilds it under the rebuild budget; a failed or discarded
// rebuild retries on the next tick. It also drives the outage policy's health probe on
// its own schedule. Run returns nil when ctx ends, after its goroutines have exited.
//
// Run returns ErrNotPrimed unless an earlier Prime returned Installed, and
// ErrAlreadyRunning while another Run is active. A panic in the store or in a Spec
// callback does not escape Run: the cache is marked stale and the next tick retries.
func (c *Cache[ID, E]) Run(ctx context.Context) error {
	c.mu.Lock()
	switch {
	case !c.primed:
		c.mu.Unlock()
		return ErrNotPrimed
	case c.running:
		c.mu.Unlock()
		return ErrAlreadyRunning
	}
	c.running = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
	}()

	var wg sync.WaitGroup
	defer wg.Wait()
	c.startProber(ctx, &wg)

	ticker := time.NewTicker(c.cfg.rebuildInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.tick(ctx)
		}
	}
}

// tick runs one loop iteration. The deferred closure recovers a panic from any part of
// it: the tick-context hook, the store, or a callback. A panic inside a locked region
// has already marked the cache stale and released the lock; any other panic is marked
// stale here, so the cache never stays trusted across an unexplained failure.
func (c *Cache[ID, E]) tick(ctx context.Context) {
	tickCtx := ctx
	rebuilding := false
	result, err := Failed, error(nil)
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v", errPanicked, r)
			c.markStale(tickCtx, err)
		}
		if rebuilding && result == Failed {
			c.noteLoopFailure(tickCtx, err)
		}
	}()
	tickCtx = c.cfg.tickContext(ctx)

	c.mu.Lock()
	if !c.stale {
		c.mu.Unlock()
		return
	}
	c.generation++
	captured := c.generation
	c.episode.attempts++
	c.mu.Unlock()

	rctx, cancel := context.WithTimeout(tickCtx, c.cfg.rebuildBudget)
	defer cancel()
	rebuilding = true
	result, err = c.rebuild(rctx, captured, TriggerLoop)
}
