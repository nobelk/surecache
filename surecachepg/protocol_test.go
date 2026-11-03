// SPDX-License-Identifier: Apache-2.0

package surecachepg_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/nobelk/surecache/surecachepg"
	"github.com/nobelk/surecache/surecachepg/internal/pgtest"
)

// Clarification 13: Parse lock waits and execution share one statement_timeout.
// DescribeExec incorrectly gives each phase a fresh timeout and succeeds here.
func TestReadSnapshot_TimeoutCoversParseAndExecute(t *testing.T) {
	for _, single := range []bool{false, true} {
		for _, disabled := range []bool{false, true} {
			name := map[bool]string{false: "Query", true: "QueryRow"}[single]
			if disabled {
				name += "/cache-disabled"
			}
			t.Run(name, func(t *testing.T) {
				cfg := pgtest.DB(t)
				if disabled {
					cfg.ConnConfig.StatementCacheCapacity = 0
					cfg.ConnConfig.DescriptionCacheCapacity = 0
					cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
				}
				pool := pgtest.Pool(t, cfg)
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				if _, err := pool.Exec(ctx, `CREATE TABLE timeout_phases (n int); INSERT INTO timeout_phases VALUES (1)`); err != nil {
					t.Fatal(err)
				}
				reader, err := pool.Acquire(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Release()
				pid := reader.Conn().PgConn().PID()
				locker, err := pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
					defer stop()
					_ = locker.Rollback(cleanup)
				}()
				if _, err := locker.Exec(ctx, `LOCK TABLE timeout_phases IN ACCESS EXCLUSIVE MODE`); err != nil {
					t.Fatal(err)
				}
				b := surecachepg.DefaultBounds()
				b.ReadStatementTimeout = 2 * time.Second
				b.MaxReadStatements = 1
				done := make(chan struct{})
				var readErr error
				go func() {
					defer close(done)
					readErr = surecachepg.ReadSnapshot(ctx, reader, surecachepg.NewFence(testKey), b,
						func(q surecachepg.SnapshotQuerier) error {
							const sql = `SELECT pg_sleep($1::double precision) FROM timeout_phases`
							if single {
								var ignored any
								return q.QueryRow(ctx, sql, 1.2).Scan(&ignored)
							}
							rows, err := q.Query(ctx, sql, 1.2)
							if err != nil {
								return err
							}
							defer rows.Close()
							for rows.Next() {
							}
							return rows.Err()
						})
				}()
				// Join even if an observation or lock release fails; never release a
				// pooled connection while its query goroutine is still using it.
				defer func() { cancel(); <-done }()
				waitSnapshotEvent(t, ctx, pool, pid, "Lock")
				timer := time.NewTimer(1200 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-done:
					t.Fatalf("read ended before lock release: %v", readErr)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if err := locker.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				// Prove execution started, rather than accepting a lock-only timeout.
				waitSnapshotEvent(t, ctx, pool, pid, "PgSleep")
				select {
				case <-done:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				var pgErr *pgconn.PgError
				if !errors.As(readErr, &pgErr) || pgErr.Code != "57014" {
					t.Fatalf("ReadSnapshot = %v; want PostgreSQL statement timeout (57014)", readErr)
				}
			})
		}
	}
}

// Poll the server, not elapsed query time. Every observation has a bounded context.
func waitSnapshotEvent(t *testing.T, ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, pid uint32, event string,
) {
	t.Helper()
	observe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		err := db.QueryRow(observe, `SELECT EXISTS (
            SELECT 1 FROM pg_stat_activity WHERE pid = $1 AND state = 'active'
            AND query LIKE '%FROM timeout_phases%'
            AND (($2 = 'Lock' AND wait_event_type = 'Lock' AND cardinality(pg_blocking_pids(pid)) > 0)
                 OR wait_event = $2))`, pid, event).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-ticker.C:
		case <-observe.Done():
			t.Fatalf("waiting for backend %d event %s: %v", pid, event, observe.Err())
		}
	}
}
