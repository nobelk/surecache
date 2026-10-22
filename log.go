// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"context"
	"log/slog"
	"time"
)

// Edge-triggered log messages (design section 9). Messages are static; attributes carry
// the cache name, the cause and counts, never entity contents or ids.
const (
	logStale         = "surecache_stale"
	logRebuildFailed = "surecache_rebuild_failed"
	logRecovered     = "surecache_recovered"
)

// episode tracks one stale episode: from a trusted-to-stale transition to the rebuild
// that installs. It lives under c.mu and is reset when a rebuild installs.
type episode struct {
	start        time.Time // zero while no episode is open
	attempts     int       // loop rebuild attempts in this episode
	failedLogged bool      // surecache_rebuild_failed already logged
}

// beginEpisodeLocked opens an episode on a trusted-to-stale transition.
func (c *Cache[ID, E]) beginEpisodeLocked() {
	c.episode = episode{start: time.Now()}
}

// endEpisodeLocked closes the open episode, if any, and returns the recovery facts for
// surecache_recovered.
func (c *Cache[ID, E]) endEpisodeLocked() (attempts int, staleFor time.Duration, ok bool) {
	ep := c.episode
	c.episode = episode{}
	if ep.start.IsZero() {
		return 0, 0, false
	}
	return ep.attempts, time.Since(ep.start), true
}

// noteLoopFailure logs surecache_rebuild_failed for the first failed loop rebuild of a
// stale episode. A Failed rebuild always carries a non-nil cause.
func (c *Cache[ID, E]) noteLoopFailure(ctx context.Context, cause error) {
	c.mu.Lock()
	first := !c.episode.failedLogged && !c.episode.start.IsZero()
	if first {
		c.episode.failedLogged = true
	}
	attempt := c.episode.attempts
	c.mu.Unlock()
	if !first {
		return
	}
	c.cfg.logger.LogAttrs(ctx, slog.LevelError, logRebuildFailed,
		slog.String("cache", c.spec.Name),
		slog.String("cause", cause.Error()),
		slog.Int("attempt", attempt))
}

// logState writes the log record that belongs to a state event, if any. It runs in
// drain, off the lock and in transition order. A transition to stale always carries a
// non-nil cause.
func (c *Cache[ID, E]) logState(ev stateEvent) {
	if ev.kind != stateStale || ev.initial {
		return
	}
	ctx := ev.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	switch {
	case ev.on:
		c.cfg.logger.LogAttrs(ctx, slog.LevelWarn, logStale,
			slog.String("cache", c.spec.Name),
			slog.String("cause", ev.cause.Error()))
	case ev.recovered:
		c.cfg.logger.LogAttrs(ctx, slog.LevelInfo, logRecovered,
			slog.String("cache", c.spec.Name),
			slog.Int("attempts", ev.attempts),
			slog.Duration("stale_for", ev.staleFor))
	}
}
