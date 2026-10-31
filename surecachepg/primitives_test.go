// SPDX-License-Identifier: Apache-2.0

package surecachepg_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nobelk/surecache"
	"github.com/nobelk/surecache/surecachepg"
)

func TestCapacityHelpers(t *testing.T) {
	for _, capacity := range []int{0, 1, math.MaxInt - 1} {
		if got := surecachepg.LimitPlusOne(capacity); got != capacity+1 {
			t.Errorf("LimitPlusOne(%d) = %d", capacity, got)
		}
		for _, n := range []int{capacity, capacity + 1} {
			err := surecachepg.OverCapacity("groups", n, capacity)
			if n <= capacity {
				if err != nil {
					t.Errorf("OverCapacity(%d, %d) = %v", n, capacity, err)
				}
			} else if !errors.Is(err, surecache.ErrOverCapacity) || !strings.Contains(err.Error(), "groups") {
				t.Errorf("OverCapacity(%d, %d) = %v", n, capacity, err)
			}
		}
	}
}

func TestIsNothingCommitted_ErrorClassification(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "23505"}
	for _, tc := range []struct {
		name  string
		err   error
		codes []string
		want  bool
	}{
		{"nil", nil, []string{"23505"}, false},
		{"matching", pgErr, []string{"P0001", "23505"}, true},
		{"wrapped", fmt.Errorf("insert: %w", pgErr), []string{"23505"}, true},
		{"different code", pgErr, []string{"P0001"}, false},
		{"no codes", pgErr, nil, false},
		{"connection error", errors.New("connection lost"), []string{"23505"}, false},
		{"canceled takes precedence", errors.Join(pgErr, context.Canceled), []string{"23505"}, false},
		{"deadline takes precedence", errors.Join(pgErr, context.DeadlineExceeded), []string{"23505"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := surecachepg.IsNothingCommitted(tc.err, tc.codes...); got != tc.want {
				t.Fatalf("IsNothingCommitted = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestConfigurePool_WithoutConnection(t *testing.T) {
	for _, nilParams := range []bool{false, true} {
		t.Run(fmt.Sprintf("nilParams=%v", nilParams), func(t *testing.T) {
			cfg, err := pgxpool.ParseConfig("postgres://localhost/test?application_name=retained")
			if err != nil {
				t.Fatal(err)
			}
			if nilParams {
				cfg.ConnConfig.RuntimeParams = nil
			}
			b := surecachepg.DefaultBounds()
			b.IdleInTxTimeout = time.Millisecond + time.Nanosecond
			if err := surecachepg.ConfigurePool(cfg, b); err != nil {
				t.Fatal(err)
			}
			if got := cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"]; got != "2ms" {
				t.Fatalf("idle timeout = %q", got)
			}
			if !nilParams && cfg.ConnConfig.RuntimeParams["application_name"] != "retained" {
				t.Fatal("unrelated connection parameter lost")
			}
			if cfg.ConnConfig.DialFunc == nil {
				t.Fatal("no dialer installed")
			}
		})
	}
	b := surecachepg.DefaultBounds()
	b.KeepAlive.Count = 0
	if err := surecachepg.ConfigurePool(nil, b); !errors.Is(err, surecachepg.ErrInvalidBounds) {
		t.Fatalf("invalid bounds = %v", err)
	}
}

// Only the pgx operations used by the public primitives are implemented. Any
// unexpected database operation panics via the embedded interface.
type primitiveTx struct {
	pgx.Tx
	execErrAt       int
	err             error
	commitErr       error
	execs           int
	commits         int
	rollbacks       int
	rollbackCtxErr  error
	rollbackBounded bool
}

func (tx *primitiveTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	tx.execs++
	if tx.execs == tx.execErrAt {
		return pgconn.CommandTag{}, tx.err
	}
	return pgconn.CommandTag{}, nil
}

func (tx *primitiveTx) Commit(context.Context) error {
	tx.commits++
	return tx.commitErr
}

func (tx *primitiveTx) Rollback(ctx context.Context) error {
	tx.rollbacks++
	tx.rollbackCtxErr = ctx.Err()
	deadline, ok := ctx.Deadline()
	tx.rollbackBounded = ok && time.Until(deadline) > 0 && time.Until(deadline) <= 5*time.Second
	return nil
}

type primitiveDB struct {
	txs    []pgx.Tx
	failAt int
	err    error
	calls  int
	opts   []pgx.TxOptions
}

func (db *primitiveDB) BeginTx(_ context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	db.calls++
	db.opts = append(db.opts, opts)
	if db.calls == db.failAt {
		return nil, db.err
	}
	return db.txs[db.calls-1], nil
}

func assertPrimitiveCleanup(t *testing.T, tx *primitiveTx, commits int) {
	t.Helper()
	if tx.commits != commits || tx.rollbacks != 1 || tx.rollbackCtxErr != nil || !tx.rollbackBounded {
		t.Fatalf("cleanup: commits=%d rollbacks=%d ctxErr=%v bounded=%v", tx.commits, tx.rollbacks, tx.rollbackCtxErr, tx.rollbackBounded)
	}
}

func TestFenceExclusive_Failures(t *testing.T) {
	sentinel := errors.New("database failed")
	for _, stage := range []string{"begin", "lock timeout", "statement timeout", "lock", "commit"} {
		t.Run(stage, func(t *testing.T) {
			tx := &primitiveTx{err: sentinel}
			db := &primitiveDB{txs: []pgx.Tx{tx}, err: sentinel}
			switch stage {
			case "begin":
				db.failAt = 1
			case "lock timeout":
				tx.execErrAt = 1
			case "statement timeout":
				tx.execErrAt = 2
			case "lock":
				tx.execErrAt = 3
			case "commit":
				tx.commitErr = sentinel
			}
			f := surecachepg.NewFence(-77)
			if f.Key() != -77 {
				t.Fatal("fence key changed")
			}
			if err := f.Exclusive(context.Background(), db, surecachepg.DefaultBounds()); err != sentinel {
				t.Fatalf("Exclusive = %v", err)
			}
			if stage == "begin" {
				if tx.rollbacks != 0 {
					t.Fatal("rollback without transaction")
				}
				return
			}
			commits := 0
			if stage == "commit" {
				commits = 1
			}
			assertPrimitiveCleanup(t, tx, commits)
		})
	}
}

func TestWithMutation_FailuresAndSuccess(t *testing.T) {
	sentinel := errors.New("mutation failed")
	for _, stage := range []string{"begin", "fence", "callback", "commit", "success"} {
		t.Run(stage, func(t *testing.T) {
			tx := &primitiveTx{err: sentinel}
			db := &primitiveDB{txs: []pgx.Tx{tx}, err: sentinel}
			if stage == "begin" {
				db.failAt = 1
			}
			if stage == "fence" {
				tx.execErrAt = 1
			}
			if stage == "commit" {
				tx.commitErr = sentinel
			}
			called := false
			err := surecachepg.WithMutation(context.Background(), db, surecachepg.NewFence(77), func(got pgx.Tx) error {
				called = true
				if got != tx || tx.execs != 1 {
					t.Fatal("callback did not receive fenced transaction")
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
				t.Fatalf("WithMutation = %v, want %v", err, want)
			}
			if called != (stage != "begin" && stage != "fence") {
				t.Fatalf("callback called = %v", called)
			}
			if stage == "begin" {
				return
			}
			commits := 0
			if stage == "commit" || stage == "success" {
				commits = 1
			}
			assertPrimitiveCleanup(t, tx, commits)
		})
	}
}

func TestFence_CanceledContextAndCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tx := &primitiveTx{execErrAt: 1, err: errors.New("wrapped cancellation")}
	db := &primitiveDB{txs: []pgx.Tx{tx}}
	if err := surecachepg.NewFence(77).Exclusive(ctx, db, surecachepg.DefaultBounds()); err != context.Canceled {
		t.Fatalf("Exclusive = %v", err)
	}
	assertPrimitiveCleanup(t, tx, 0)
	tx.execs = 0
	if err := surecachepg.NewFence(77).Shared(ctx, tx); err != context.Canceled {
		t.Fatalf("Shared = %v", err)
	}
}
