// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"context"
	"errors"
	"sync"
	"time"
)

// OutagePolicy decides how the cache behaves while the store is unavailable (design
// section 8). Whether answering from a trusted cache during an outage is safe depends on
// the caller's domain, so there is no default and Build requires one. The interface is
// sealed: use ServeWhileExact or DistrustAfterProbeFailures.
type OutagePolicy interface {
	// health returns the probe policy, or nil when there is none.
	health() *HealthPolicy
	validate() error
}

// ServeWhileExact keeps a trusted cache answering through an outage until the first
// mutation starts. From then on reads that mutation affects (and List) defer, so callers
// see store errors; when it fails ambiguously the cache goes stale until a rebuild
// succeeds after the store returns. Use it when acting on known-good data during an
// outage is the safe direction.
func ServeWhileExact() OutagePolicy { return serveWhileExact{} }

type serveWhileExact struct{}

func (serveWhileExact) health() *HealthPolicy { return nil }
func (serveWhileExact) validate() error       { return nil }

// DistrustAfterProbeFailures runs p.Probe on its own schedule and sets the cache
// unhealthy after p.FailAfter consecutive failures, so every read defers within about
// FailAfter × Interval + Timeout of an outage even when nobody writes. Only
// p.RecoverAfter consecutive probe successes clear it; a rebuild never does. Use it when
// acting on data the system cannot currently confirm is the unsafe direction.
func DistrustAfterProbeFailures(p HealthPolicy) OutagePolicy { return distrust{p: p} }

type distrust struct{ p HealthPolicy }

func (d distrust) health() *HealthPolicy { return &d.p }
func (d distrust) validate() error       { return d.p.validate() }

// HealthPolicy configures DistrustAfterProbeFailures.
type HealthPolicy struct {
	// Probe checks the store, for example with a short round trip to the writer. It
	// runs off the cache lock under a context bounded by Timeout.
	Probe func(ctx context.Context) error
	// Interval is the probe schedule, independent of rebuilds.
	Interval time.Duration
	// Timeout bounds each probe and must be less than Interval. A probe that exceeds it
	// counts as a failure.
	Timeout time.Duration
	// Unavailable reports which probe errors count as failures. Other errors (business
	// errors such as not-found) are ignored.
	Unavailable func(error) bool
	// FailAfter is the number of consecutive failures that sets unhealthy.
	FailAfter int
	// RecoverAfter is the number of consecutive probe successes that clears it.
	RecoverAfter int
}

func (p *HealthPolicy) validate() error {
	var errs []error
	if p.Probe == nil {
		errs = append(errs, errors.New("surecache: HealthPolicy.Probe is nil"))
	}
	if p.Unavailable == nil {
		errs = append(errs, errors.New("surecache: HealthPolicy.Unavailable is nil"))
	}
	if p.Interval <= 0 {
		errs = append(errs, errors.New("surecache: HealthPolicy.Interval must be positive"))
	}
	switch {
	case p.Timeout <= 0:
		errs = append(errs, errors.New("surecache: HealthPolicy.Timeout must be positive"))
	case p.Timeout >= p.Interval:
		errs = append(errs, errors.New("surecache: HealthPolicy.Timeout must be less than Interval"))
	}
	if p.FailAfter < 1 {
		errs = append(errs, errors.New("surecache: HealthPolicy.FailAfter must be at least 1"))
	}
	if p.RecoverAfter < 1 {
		errs = append(errs, errors.New("surecache: HealthPolicy.RecoverAfter must be at least 1"))
	}
	return errors.Join(errs...)
}

// ObserveStoreResult feeds the outcome of a deferred read's own store call to the health
// policy. Under DistrustAfterProbeFailures an error for which Unavailable returns true
// counts toward FailAfter exactly like a probe failure; successes and other errors are
// ignored, and only probe successes clear unhealthy. Under ServeWhileExact it does
// nothing.
func (c *Cache[ID, E]) ObserveStoreResult(err error) {
	hp := c.cfg.outage.health()
	if hp == nil || err == nil || !hp.Unavailable(err) {
		return
	}
	c.recordHealth(context.Background(), hp, probeFailure)
}

// startProber starts the health prober goroutine when the outage policy has one.
func (c *Cache[ID, E]) startProber(ctx context.Context, wg *sync.WaitGroup) {
	hp := c.cfg.outage.health()
	if hp == nil {
		return
	}
	wg.Go(func() {
		ticker := time.NewTicker(hp.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.recordHealth(ctx, hp, c.probe(ctx, hp))
			}
		}
	})
}

// probeOutcome classifies one probe.
type probeOutcome uint8

const (
	probeIgnored probeOutcome = iota // an error Unavailable rejects; streaks unchanged
	probeSuccess
	probeFailure
)

// probe runs one probe off the lock. A probe that exceeds its timeout fails whatever it
// returns, and so does a panicking probe or Unavailable. A probe cut short because Run is
// stopping is ignored.
func (c *Cache[ID, E]) probe(ctx context.Context, hp *HealthPolicy) (outcome probeOutcome) {
	pctx, cancel := context.WithTimeout(ctx, hp.Timeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			outcome = probeFailure
		}
	}()
	err := hp.Probe(pctx)
	switch {
	case ctx.Err() != nil:
		return probeIgnored
	case pctx.Err() != nil:
		return probeFailure
	case err == nil:
		return probeSuccess
	case hp.Unavailable(err):
		return probeFailure
	}
	return probeIgnored
}

// recordHealth applies one probe or deferred-read outcome to the health streaks under
// the lock (design section 8, Counting).
func (c *Cache[ID, E]) recordHealth(ctx context.Context, hp *HealthPolicy, outcome probeOutcome) {
	if outcome == probeIgnored {
		return
	}
	c.withWriteLock(ctx, func(buf *eventBuf) {
		if outcome == probeSuccess {
			c.okStreak++
			c.failStreak = 0
			if c.unhealthy && c.okStreak >= hp.RecoverAfter {
				c.unhealthy = false
				c.queueStateLocked(stateEvent{ctx: ctx, kind: stateUnhealthy, on: false})
			}
			return
		}
		c.failStreak++
		c.okStreak = 0
		buf.add(counterEvent{kind: counterProbeFailure})
		if !c.unhealthy && c.failStreak >= hp.FailAfter {
			c.unhealthy = true
			c.queueStateLocked(stateEvent{ctx: ctx, kind: stateUnhealthy, on: true})
		}
	})
}
