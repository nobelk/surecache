// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Budget defaults (design section 4.7).
const (
	defaultPrimeBudget     = 10 * time.Second
	defaultRebuildBudget   = 4 * time.Second
	defaultRefreshBudget   = 2 * time.Second
	defaultRebuildInterval = 1 * time.Second
)

// Option configures a cache at Build.
type Option func(*config)

// OutagePolicy decides how the cache behaves while the store is unavailable (design
// section 8). It is sealed; there is no default, and Build requires one.
type OutagePolicy interface {
	outagePolicy()
}

type config struct {
	outage          OutagePolicy
	observer        Observer
	logger          *slog.Logger
	tickContext     func(context.Context) context.Context
	primeBudget     time.Duration
	rebuildBudget   time.Duration
	refreshBudget   time.Duration
	rebuildInterval time.Duration
	cloneOnInstall  bool
}

func defaultConfig() config {
	return config{
		observer:        NopObserver{},
		logger:          slog.New(slog.DiscardHandler),
		tickContext:     func(ctx context.Context) context.Context { return ctx },
		primeBudget:     defaultPrimeBudget,
		rebuildBudget:   defaultRebuildBudget,
		refreshBudget:   defaultRefreshBudget,
		rebuildInterval: defaultRebuildInterval,
	}
}

func (cfg *config) validate() error {
	var errs []error
	if cfg.outage == nil {
		errs = append(errs, ErrNoOutagePolicy)
	}
	if cfg.tickContext == nil {
		errs = append(errs, errors.New("surecache: WithTickContext(nil)"))
	}
	for _, d := range []struct {
		name string
		v    time.Duration
	}{
		{"prime budget", cfg.primeBudget},
		{"rebuild budget", cfg.rebuildBudget},
		{"refresh budget", cfg.refreshBudget},
		{"rebuild interval", cfg.rebuildInterval},
	} {
		if d.v <= 0 {
			errs = append(errs, errors.New("surecache: "+d.name+" must be positive"))
		}
	}
	return errors.Join(errs...)
}

// WithOutagePolicy sets the required outage policy.
func WithOutagePolicy(p OutagePolicy) Option {
	return func(c *config) { c.outage = p }
}

// WithObserver sets the observer. nil means no observation.
func WithObserver(o Observer) Option {
	return func(c *config) {
		if o == nil {
			o = NopObserver{}
		}
		c.observer = o
	}
}

// WithLogger sets the logger for the edge-triggered logs. nil discards them.
func WithLogger(l *slog.Logger) Option {
	return func(c *config) {
		if l == nil {
			l = slog.New(slog.DiscardHandler)
		}
		c.logger = l
	}
}

// WithTickContext sets a hook that derives each rebuild-loop tick's context from the
// context passed to Run, for example to attach a fresh trace id per tick.
func WithTickContext(f func(context.Context) context.Context) Option {
	return func(c *config) { c.tickContext = f }
}

// WithPrimeBudget bounds Prime. Default 10 s.
func WithPrimeBudget(d time.Duration) Option {
	return func(c *config) { c.primeBudget = d }
}

// WithRebuildBudget bounds each loop rebuild. Default 4 s. It must exceed the store's
// fence wait plus its read bound (design section 4.6).
func WithRebuildBudget(d time.Duration) Option {
	return func(c *config) { c.rebuildBudget = d }
}

// WithRefreshBudget bounds each post-commit refresh. Default 2 s.
func WithRefreshBudget(d time.Duration) Option {
	return func(c *config) { c.refreshBudget = d }
}

// WithRebuildInterval sets how often the loop in Run checks for staleness. Default 1 s.
func WithRebuildInterval(d time.Duration) Option {
	return func(c *config) { c.rebuildInterval = d }
}

// WithCloneOnInstall makes the cache clone every entity it installs. Use it when the
// Source may retain or mutate a value after returning it.
func WithCloneOnInstall() Option {
	return func(c *config) { c.cloneOnInstall = true }
}
