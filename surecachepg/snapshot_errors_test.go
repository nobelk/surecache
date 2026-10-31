// SPDX-License-Identifier: Apache-2.0

package surecachepg_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/nobelk/surecache/surecachepg"
)

func TestSnapshotTestDB_DistinctOrderedTransactions(t *testing.T) {
	ctx := context.Background()
	for _, finish := range []string{"open", "rollback", "commit"} {
		t.Run(finish, func(t *testing.T) {
			readTx := &snapshotTestTx{}
			db := newSnapshotTestDB(readTx)
			fenceTx, err := db.BeginTx(ctx, pgx.TxOptions{})
			if err != nil || fenceTx == readTx {
				t.Fatalf("fence transaction = %v, %v", fenceTx, err)
			}
			switch finish {
			case "rollback":
				err = fenceTx.Rollback(ctx)
			case "commit":
				err = fenceTx.Commit(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
			if finish != "commit" {
				if got != nil || err == nil {
					t.Fatalf("read began without committed fence: %v, %v", got, err)
				}
				return
			}
			if err != nil || got != readTx || readTx.closed {
				t.Fatalf("read transaction = %v, %v; closed=%v", got, err, readTx.closed)
			}
		})
	}
}

func TestReadSnapshot_FenceCommittedBeforeRead(t *testing.T) {
	readTx := &snapshotTestTx{}
	db := newSnapshotTestDB(readTx)
	called := false
	err := surecachepg.ReadSnapshot(context.Background(), db, surecachepg.NewFence(77), surecachepg.DefaultBounds(), func(surecachepg.SnapshotQuerier) error {
		called = true
		if db.begins != 2 || !db.fence.committed || !db.fence.closed || readTx.closed {
			t.Fatal("callback must run in a distinct open read transaction after fence commit")
		}
		return nil
	})
	if err != nil || !called || !readTx.committed {
		t.Fatalf("ReadSnapshot = %v, called=%v, committed=%v", err, called, readTx.committed)
	}
}

func TestSnapshotTestTx_Lifecycle(t *testing.T) {
	ctx := context.Background()
	for _, finish := range []string{"commit", "rollback"} {
		t.Run(finish, func(t *testing.T) {
			tx := &snapshotTestTx{rows: &snapshotTestRows{}, row: &snapshotTestRows{}}
			var err error
			if finish == "commit" {
				err = tx.Commit(ctx)
			} else {
				err = tx.Rollback(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, "SELECT 1"); err != pgx.ErrTxClosed {
				t.Fatalf("Exec after %s = %v", finish, err)
			}
			if rows, err := tx.Query(ctx, "SELECT 1", pgx.QueryExecModeDescribeExec); rows != nil || err != pgx.ErrTxClosed {
				t.Fatalf("Query after %s = %v, %v", finish, rows, err)
			}
			if err := tx.QueryRow(ctx, "SELECT 1").Scan(new(int)); err != pgx.ErrTxClosed {
				t.Fatalf("QueryRow after %s = %v", finish, err)
			}
			if err := tx.Commit(ctx); err != pgx.ErrTxClosed {
				t.Fatalf("Commit after %s = %v", finish, err)
			}
			if err := tx.Rollback(ctx); err != pgx.ErrTxClosed {
				t.Fatalf("Rollback after %s = %v", finish, err)
			}
		})
	}
}

func TestSnapshotTestTx_QueryAbort(t *testing.T) {
	ctx := context.Background()
	queryErr := errors.New("server query failed")
	for _, single := range []bool{false, true} {
		name := "Query"
		if single {
			name = "QueryRow"
		}
		t.Run(name, func(t *testing.T) {
			tx := &snapshotTestTx{err: queryErr}
			if single {
				if err := tx.QueryRow(ctx, "SELECT 1").Scan(new(int)); err != queryErr {
					t.Fatalf("QueryRow = %v", err)
				}
			} else if _, err := tx.Query(ctx, "SELECT 1", pgx.QueryExecModeDescribeExec); err != queryErr {
				t.Fatalf("Query = %v", err)
			}
			assertAborted := func(err error) {
				t.Helper()
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "25P02" {
					t.Fatalf("operation in aborted transaction = %v; want SQLSTATE 25P02", err)
				}
			}
			_, err := tx.Exec(ctx, "SELECT 2")
			assertAborted(err)
			_, err = tx.Query(ctx, "SELECT 2", pgx.QueryExecModeDescribeExec)
			assertAborted(err)
			assertAborted(tx.QueryRow(ctx, "SELECT 2").Scan(new(int)))
			if err := tx.Commit(ctx); err != pgx.ErrTxCommitRollback {
				t.Fatalf("Commit after query failure = %v", err)
			}
			if _, err := tx.Exec(ctx, "SELECT 3"); err != pgx.ErrTxClosed {
				t.Fatalf("Exec after failed commit = %v", err)
			}
		})
	}
}

func TestReadSnapshot_TransactionFailures(t *testing.T) {
	sentinel := errors.New("snapshot failed")
	for _, stage := range []string{"fence", "begin read", "timeout", "callback", "commit", "success"} {
		t.Run(stage, func(t *testing.T) {
			fenceTx := &primitiveTx{}
			readTx := &primitiveTx{err: sentinel}
			db := &primitiveDB{txs: []pgx.Tx{fenceTx, readTx}, err: sentinel}
			switch stage {
			case "fence":
				db.failAt = 1
			case "begin read":
				db.failAt = 2
			case "timeout":
				readTx.execErrAt = 1
			case "commit":
				readTx.commitErr = sentinel
			}
			called := false
			err := surecachepg.ReadSnapshot(context.Background(), db, surecachepg.NewFence(77), surecachepg.DefaultBounds(), func(surecachepg.SnapshotQuerier) error {
				called = true
				if fenceTx.commits != 1 || readTx.execs != 1 {
					t.Fatal("callback ran before fence committed and read timeout was set")
				}
				if stage == "callback" {
					return sentinel
				}
				return nil
			})
			want := sentinel
			if stage == "success" {
				want = nil
			}
			if err != want {
				t.Fatalf("ReadSnapshot = %v, want %v", err, want)
			}
			if called != (stage == "callback" || stage == "commit" || stage == "success") {
				t.Fatalf("callback called = %v", called)
			}
			if stage == "fence" {
				return
			}
			assertPrimitiveCleanup(t, fenceTx, 1)
			if stage == "begin read" {
				return
			}
			if got := db.opts[1]; got.IsoLevel != pgx.RepeatableRead || got.AccessMode != pgx.ReadOnly {
				t.Fatalf("read options = %+v", got)
			}
			commits := 0
			if stage == "commit" || stage == "success" {
				commits = 1
			}
			assertPrimitiveCleanup(t, readTx, commits)
		})
	}
}

func TestReadSnapshot_Cancellation(t *testing.T) {
	t.Run("before begin", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// No database may be touched for an already canceled snapshot.
		if err := surecachepg.ReadSnapshot(ctx, nil, surecachepg.NewFence(77), surecachepg.DefaultBounds(), nil); err != context.Canceled {
			t.Fatalf("ReadSnapshot = %v", err)
		}
	})
	t.Run("during callback", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		fenceTx, readTx := &primitiveTx{}, &primitiveTx{}
		db := &primitiveDB{txs: []pgx.Tx{fenceTx, readTx}}
		err := surecachepg.ReadSnapshot(ctx, db, surecachepg.NewFence(77), surecachepg.DefaultBounds(), func(surecachepg.SnapshotQuerier) error {
			cancel()
			return errors.New("driver cancellation error")
		})
		if err != context.Canceled {
			t.Fatalf("ReadSnapshot = %v", err)
		}
		assertPrimitiveCleanup(t, readTx, 0)
	})
}

func TestReadSnapshot_NilQueryRows(t *testing.T) {
	queryErr := errors.New("query unavailable")
	for _, propagate := range []bool{false, true} {
		name := "swallowed error aborts commit"
		if propagate {
			name = "callback returns query error"
		}
		t.Run(name, func(t *testing.T) {
			tx := &snapshotTestTx{err: queryErr}
			b := surecachepg.DefaultBounds()
			b.MaxReadStatements = 1
			err := surecachepg.ReadSnapshot(context.Background(), newSnapshotTestDB(tx), surecachepg.NewFence(77), b, func(q surecachepg.SnapshotQuerier) error {
				rows, err := q.Query(context.Background(), "SELECT 1")
				if rows != nil || err != queryErr {
					t.Fatalf("Query = %v, %v", rows, err)
				}
				if _, err := q.Query(context.Background(), "SELECT 2"); err != surecachepg.ErrStatementLimit {
					t.Fatalf("failed query did not consume statement allowance: %v", err)
				}
				if propagate {
					return queryErr
				}
				return nil
			})
			want := error(pgx.ErrTxCommitRollback)
			if propagate {
				want = queryErr
			}
			if err != want || tx.calls != 1 || !tx.closed || tx.committed {
				t.Fatalf("ReadSnapshot = %v, want %v; calls=%d closed=%v committed=%v", err, want, tx.calls, tx.closed, tx.committed)
			}
		})
	}
}

// Reject overrides on both APIs even behind result-format options. A rejected
// option is not a statement: the full allowance remains usable afterward.
func TestReadSnapshot_RejectedOptionsPreserveAllowance(t *testing.T) {
	for _, single := range []bool{false, true} {
		for _, tc := range []struct {
			name   string
			option any
			want   error
		}{
			{"exec mode", pgx.QueryExecModeSimpleProtocol, surecachepg.ErrExecMode},
			{"named args", pgx.NamedArgs{"n": 7}, surecachepg.ErrQueryRewriter},
		} {
			api := "Query"
			if single {
				api = "QueryRow"
			}
			t.Run(api+"/"+tc.name, func(t *testing.T) {
				tx := &snapshotTestTx{rows: &snapshotTestRows{}, row: &snapshotTestRows{}}
				b := surecachepg.DefaultBounds()
				b.MaxReadStatements = 1
				err := surecachepg.ReadSnapshot(context.Background(), newSnapshotTestDB(tx), surecachepg.NewFence(77), b, func(q surecachepg.SnapshotQuerier) error {
					args := []any{pgx.QueryResultFormats{0}, tc.option}
					var err error
					if single {
						err = q.QueryRow(context.Background(), "SELECT 7", args...).Scan(new(int))
					} else {
						var rows pgx.Rows
						rows, err = q.Query(context.Background(), "SELECT 7", args...)
						if rows != nil {
							t.Fatal("rejected query returned rows")
						}
					}
					if err != tc.want || tx.calls != 0 {
						t.Fatalf("rejection = %v, calls=%d", err, tx.calls)
					}
					rows, err := q.Query(context.Background(), "SELECT 7")
					if err != nil {
						return err
					}
					rows.Close()
					return nil
				})
				if err != nil || tx.calls != 1 {
					t.Fatalf("ReadSnapshot = %v, calls=%d", err, tx.calls)
				}
			})
		}
	}
}
