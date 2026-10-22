// SPDX-License-Identifier: Apache-2.0

package surecachepg

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Beginner starts transactions. *pgxpool.Pool and *pgx.Conn implement it.
type Beginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Fence is the mutation fence of design section 4.3 on one transaction-scoped advisory
// lock key per cache: every mutation holds it shared for its whole transaction, and a
// bulk load acquires it exclusively in a separate transaction that commits before the
// read begins. Advisory locks are scoped to the database, not the schema.
type Fence struct {
	key int64
}

// NewFence returns the fence on one fixed 64-bit advisory-lock key.
func NewFence(key int64) Fence {
	return Fence{key: key}
}

// Key returns the advisory-lock key.
func (f Fence) Key() int64 {
	return f.key
}

// Shared takes the fence shared. Run it as the first statement of every mutation
// transaction; the lock is released when the transaction ends.
func (f Fence) Shared(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock_shared($1)", f.key)
	return unwrapContext(ctx, err)
}

// Exclusive acquires and releases the fence exclusively in its own READ COMMITTED
// transaction, bounded on the server by b's fence lock and statement timeouts. When it
// returns nil, no mutation transaction that began before it is still open.
func (f Fence) Exclusive(ctx context.Context, db Beginner, b Bounds) error {
	if err := b.Validate(); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return unwrapContext(ctx, err)
	}
	defer rollback(ctx, tx)
	for _, stmt := range []string{
		setLocal("lock_timeout", b.FenceLockTimeout),
		setLocal("statement_timeout", b.FenceStatementTimeout),
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return unwrapContext(ctx, err)
		}
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", f.key); err != nil {
		return unwrapContext(ctx, err)
	}
	return unwrapContext(ctx, tx.Commit(ctx))
}

// WithMutation runs fn in one transaction that holds f shared from its first statement,
// so even a single-statement write orders before every later bulk load (design section
// 4.7). It commits when fn returns nil and rolls back otherwise. fn's error is returned
// unchanged, so the adapter's own nothing-committed sentinels reach
// Spec.CommittedNothing; begin, fence and commit errors are returned as they are and are
// ambiguous.
func WithMutation(ctx context.Context, db Beginner, f Fence, fn func(pgx.Tx) error) error {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if err := f.Shared(ctx, tx); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// rollbackTimeout bounds the rollback that runs after the caller's context ended.
const rollbackTimeout = 5 * time.Second

// rollback ends tx if it is still open; after a commit it does nothing. It runs even
// when ctx has ended, so a cancelled caller never leaves the fence held. If the rollback
// fails, pgx closes the connection, which ends the transaction on the server.
func rollback(ctx context.Context, tx pgx.Tx) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	_ = tx.Rollback(rctx)
}
