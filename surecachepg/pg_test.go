// SPDX-License-Identifier: Apache-2.0

package surecachepg_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nobelk/surecache/surecachepg"
	"github.com/nobelk/surecache/surecachepg/internal/pgtest"
)

const testKey = 77

// shortBounds keeps fence waits short in tests.
func shortBounds() surecachepg.Bounds {
	b := surecachepg.DefaultBounds()
	b.FenceLockTimeout = 300 * time.Millisecond
	b.FenceStatementTimeout = 300 * time.Millisecond
	return b
}

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg := pgtest.DB(t)
	if err := surecachepg.ConfigurePool(cfg, surecachepg.DefaultBounds()); err != nil {
		t.Fatal(err)
	}
	return pgtest.Pool(t, cfg)
}

// openShared begins a transaction holding the fence shared.
func openShared(t *testing.T, pool *pgxpool.Pool, f surecachepg.Fence) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if err := f.Shared(ctx, tx); err != nil {
		t.Fatal(err)
	}
	return tx
}

// Section 4.3: mutations hold the fence shared and never block each other.
func TestFence_SharedDoesNotBlockShared(t *testing.T) {
	pool := newPool(t)
	f := surecachepg.NewFence(testKey)
	openShared(t, pool, f)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := f.Shared(ctx, tx); err != nil {
		t.Fatalf("second shared holder blocked: %v", err)
	}
}

// Section 4.3: the exclusive fence waits for every open shared holder.
func TestFence_ExclusiveWaitsForShared(t *testing.T) {
	pool := newPool(t)
	f := surecachepg.NewFence(testKey)
	tx := openShared(t, pool, f)
	done := make(chan error, 1)
	go func() { done <- f.Exclusive(context.Background(), pool, surecachepg.DefaultBounds()) }()
	select {
	case err := <-done:
		t.Fatalf("exclusive granted while shared held: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Exclusive after commit: %v", err)
	}
}

// Section 4.6: lock_timeout bounds the exclusive wait on the server.
func TestFence_ExclusiveTimesOut(t *testing.T) {
	pool := newPool(t)
	f := surecachepg.NewFence(testKey)
	openShared(t, pool, f)
	start := time.Now()
	err := f.Exclusive(context.Background(), pool, shortBounds())
	if err == nil {
		t.Fatal("Exclusive succeeded while shared held")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Exclusive waited %v; bound is 300ms", d)
	}
}

// The exclusive fence is granted once a shared holder rolls back.
func TestFence_ExclusiveAfterRollback(t *testing.T) {
	pool := newPool(t)
	f := surecachepg.NewFence(testKey)
	tx := openShared(t, pool, f)
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.Exclusive(context.Background(), pool, shortBounds()); err != nil {
		t.Fatal(err)
	}
	if f.Key() != testKey {
		t.Fatal("Key")
	}
}

func TestFence_ExclusiveRejectsInvalidBounds(t *testing.T) {
	b := surecachepg.DefaultBounds()
	b.MaxReadStatements = 0
	err := surecachepg.NewFence(testKey).Exclusive(context.Background(), nil, b)
	if !errors.Is(err, surecachepg.ErrInvalidBounds) {
		t.Fatalf("Exclusive = %v", err)
	}
}

// WithMutation commits on nil, rolls back and returns fn's error unchanged otherwise.
func TestWithMutation(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE TABLE t (v int PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	f := surecachepg.NewFence(testKey)
	if err := surecachepg.WithMutation(ctx, pool, f, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO t VALUES (1)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("precondition")
	err := surecachepg.WithMutation(ctx, pool, f, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO t VALUES (2)`); err != nil {
			return err
		}
		return sentinel
	})
	if err != sentinel { // fn's error must come back unchanged
		t.Fatalf("WithMutation = %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM t`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d, %v; the failed mutation committed", n, err)
	}
	// The fence is free again: both transactions released it.
	if err := f.Exclusive(ctx, pool, shortBounds()); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := surecachepg.WithMutation(cancelled, pool, f, func(pgx.Tx) error { return nil }); err == nil {
		t.Fatal("WithMutation with a cancelled context succeeded")
	}
}

// ReadSnapshot fails a statement past MaxReadStatements before it reaches the server.
func TestReadSnapshot_StatementLimitEnforced(t *testing.T) {
	pool := newPool(t)
	b := surecachepg.DefaultBounds()
	b.MaxReadStatements = 2
	start := time.Now()
	err := surecachepg.ReadSnapshot(context.Background(), pool, surecachepg.NewFence(testKey), b,
		func(q surecachepg.SnapshotQuerier) error {
			for range 2 {
				var one int
				if qerr := q.QueryRow(context.Background(), "SELECT 1").Scan(&one); qerr != nil {
					return qerr
				}
			}
			var x any
			return q.QueryRow(context.Background(), "SELECT pg_sleep(5)").Scan(&x)
		})
	if !errors.Is(err, surecachepg.ErrStatementLimit) {
		t.Fatalf("ReadSnapshot = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the extra statement reached the server (took %v)", d)
	}
	err = surecachepg.ReadSnapshot(context.Background(), pool, surecachepg.NewFence(testKey), surecachepg.DefaultBounds(),
		func(q surecachepg.SnapshotQuerier) error {
			for range 3 {
				rows, qerr := q.Query(context.Background(), "SELECT 1")
				if qerr != nil {
					return qerr
				}
				rows.Close()
			}
			return nil
		})
	if !errors.Is(err, surecachepg.ErrStatementLimit) {
		t.Fatalf("Query past the limit = %v", err)
	}
}

// A single call cannot carry several statements.
func TestReadSnapshot_MultiStatementRejected(t *testing.T) {
	pool := newPool(t)
	err := surecachepg.ReadSnapshot(context.Background(), pool, surecachepg.NewFence(testKey), surecachepg.DefaultBounds(),
		func(q surecachepg.SnapshotQuerier) error {
			rows, err := q.Query(context.Background(), "SELECT 1; SELECT 2")
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() { // drain to surface the error
			}
			return rows.Err()
		})
	if err == nil {
		t.Fatal("a multi-statement query ran in the snapshot")
	}
}

// An argument that selects another exec mode is rejected.
func TestReadSnapshot_ExecModeOverrideRejected(t *testing.T) {
	pool := newPool(t)
	err := surecachepg.ReadSnapshot(context.Background(), pool, surecachepg.NewFence(testKey), surecachepg.DefaultBounds(),
		func(q surecachepg.SnapshotQuerier) error {
			if _, err := q.Query(context.Background(), "SELECT 1", pgx.QueryExecModeSimpleProtocol); err != nil {
				return err
			}
			var x int
			return q.QueryRow(context.Background(), "SELECT $1::int", pgx.QueryExecModeExec, 1).Scan(&x)
		})
	if !errors.Is(err, surecachepg.ErrExecMode) {
		t.Fatalf("ReadSnapshot = %v", err)
	}
}

// The snapshot is read only, and context errors come back unwrapped.
func TestReadSnapshot_ReadOnlyAndContext(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	f := surecachepg.NewFence(testKey)
	err := surecachepg.ReadSnapshot(ctx, pool, f, surecachepg.DefaultBounds(), func(q surecachepg.SnapshotQuerier) error {
		rows, err := q.Query(ctx, "CREATE TABLE x (v int)")
		if err != nil {
			return err
		}
		rows.Close()
		return rows.Err()
	})
	if err == nil {
		t.Fatal("a write ran in the read-only snapshot")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := surecachepg.ReadSnapshot(cancelled, pool, f, surecachepg.DefaultBounds(), func(surecachepg.SnapshotQuerier) error {
		return nil
	}); err != context.Canceled { // the contract requires ==
		t.Fatalf("ReadSnapshot(cancelled) = %v", err)
	}
	tx := openShared(t, pool, f)
	defer func() { _ = tx.Rollback(ctx) }()
	waiting, cancelWaiting := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancelWaiting()
	if err := surecachepg.ReadSnapshot(waiting, pool, f, surecachepg.DefaultBounds(), func(surecachepg.SnapshotQuerier) error {
		return nil
	}); err != context.DeadlineExceeded { // the contract requires ==
		t.Fatalf("ReadSnapshot waiting on the fence = %v", err)
	}
}

// Section 4.2: only listed SQLSTATEs from a rolled-back transaction are
// nothing-committed; context and connection errors never are.
func TestIsNothingCommitted(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE TABLE u (v int PRIMARY KEY); INSERT INTO u VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	f := surecachepg.NewFence(testKey)
	err := surecachepg.WithMutation(ctx, pool, f, func(tx pgx.Tx) error {
		_, xerr := tx.Exec(ctx, `INSERT INTO u VALUES (1)`)
		return xerr
	})
	if !surecachepg.IsNothingCommitted(err, "23505") {
		t.Fatalf("unique violation = %v, want nothing-committed", err)
	}
	if surecachepg.IsNothingCommitted(err, "23503") {
		t.Fatal("an unlisted SQLSTATE counted")
	}

	// A context cancelled while COMMIT waits is ambiguous.
	release := lockRow(t, pool)
	cctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	err = surecachepg.WithMutation(cctx, pool, f, func(tx pgx.Tx) error {
		_, xerr := tx.Exec(cctx, `UPDATE u SET v = 2 WHERE v = 1`)
		return xerr
	})
	cancel()
	release()
	if err == nil || surecachepg.IsNothingCommitted(err, "23505", "57014") {
		t.Fatalf("cancelled mutation = %v; want an ambiguous error", err)
	}

	// A killed backend is ambiguous.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var pid int
	if err = conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO u VALUES (3)`)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err == nil || surecachepg.IsNothingCommitted(err, "23505") {
		t.Fatalf("terminated backend = %v; want an ambiguous error", err)
	}
	if surecachepg.IsNothingCommitted(context.Canceled, "57014") || surecachepg.IsNothingCommitted(nil) {
		t.Fatal("a context error or nil counted")
	}
}

// lockRow holds a row lock on u in another transaction until release.
func lockRow(t *testing.T, pool *pgxpool.Pool) (release func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT * FROM u WHERE v = 1 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	return func() { _ = tx.Rollback(ctx) }
}

// Section 4.6: ConfigurePool sets idle_in_transaction_session_timeout and keepalive.
func TestConfigurePool_SetsBounds(t *testing.T) {
	cfg := pgtest.DB(t)
	b := surecachepg.DefaultBounds()
	b.IdleInTxTimeout = 7 * time.Second
	if err := surecachepg.ConfigurePool(cfg, b); err != nil {
		t.Fatal(err)
	}
	if cfg.ConnConfig.DialFunc == nil {
		t.Fatal("no keepalive dialer installed")
	}
	pool := pgtest.Pool(t, cfg)
	var v string
	if err := pool.QueryRow(context.Background(), `SHOW idle_in_transaction_session_timeout`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != "7s" {
		t.Fatalf("idle_in_transaction_session_timeout = %q", v)
	}
	b.MaxReadStatements = 0
	if err := surecachepg.ConfigurePool(cfg, b); !errors.Is(err, surecachepg.ErrInvalidBounds) {
		t.Fatalf("ConfigurePool(invalid) = %v", err)
	}
}

// Snapshot reads need no statement or description cache, so any valid pool works.
func TestReadSnapshot_WithoutDescriptionCache(t *testing.T) {
	cfg := pgtest.DB(t)
	cfg.ConnConfig.DescriptionCacheCapacity = 0
	pool := pgtest.Pool(t, cfg)
	var got int
	err := surecachepg.ReadSnapshot(context.Background(), pool, surecachepg.NewFence(testKey), surecachepg.DefaultBounds(),
		func(q surecachepg.SnapshotQuerier) error {
			return q.QueryRow(context.Background(), "SELECT $1::int", 7).Scan(&got)
		})
	if err != nil || got != 7 {
		t.Fatalf("ReadSnapshot = %v, %d", err, got)
	}
}
