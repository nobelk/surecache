// SPDX-License-Identifier: Apache-2.0

package surecachepg

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// ErrStatementLimit is returned by a SnapshotQuerier call past Bounds.MaxReadStatements;
// the statement never reaches the server.
var ErrStatementLimit = errors.New("surecachepg: snapshot read statement limit reached")

// ErrExecMode is returned when a SnapshotQuerier call passes a pgx.QueryExecMode: every
// call runs in the extended protocol, which carries exactly one statement.
var ErrExecMode = errors.New("surecachepg: snapshot reads choose their own exec mode")

// SnapshotQuerier runs counted, single-statement reads inside the snapshot transaction.
// It deliberately exposes no batch, Exec or connection access: statement_timeout bounds
// each statement, so the read bound holds only if every statement is counted.
type SnapshotQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ReadSnapshot runs a fenced, consistent bulk read (design section 4.3): it acquires
// the fence exclusively in its own transaction, so every earlier mutation has committed
// or rolled back, then runs fn in a REPEATABLE READ, READ ONLY transaction whose
// statements are each bounded by b.ReadStatementTimeout, at most b.MaxReadStatements of
// them. Context errors are returned unwrapped, so callers can compare them with ==.
func ReadSnapshot(ctx context.Context, db Beginner, f Fence, b Bounds, fn func(SnapshotQuerier) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.Exclusive(ctx, db, b); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return unwrapContext(ctx, err)
	}
	defer rollback(ctx, tx)
	if _, err := tx.Exec(ctx, setLocal("statement_timeout", b.ReadStatementTimeout)); err != nil {
		return unwrapContext(ctx, err)
	}
	if err := fn(&querier{tx: tx, left: b.MaxReadStatements}); err != nil {
		return unwrapContext(ctx, err)
	}
	return unwrapContext(ctx, tx.Commit(ctx))
}

// querier counts statements and forces the extended protocol.
type querier struct {
	tx   pgx.Tx
	left int
}

func (q *querier) take(args []any) ([]any, error) {
	for _, a := range args {
		if _, ok := a.(pgx.QueryExecMode); ok {
			return nil, ErrExecMode
		}
	}
	if q.left <= 0 {
		return nil, ErrStatementLimit
	}
	q.left--
	return append([]any{pgx.QueryExecModeDescribeExec}, args...), nil
}

func (q *querier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	args, err := q.take(args)
	if err != nil {
		return nil, err
	}
	return q.tx.Query(ctx, sql, args...)
}

func (q *querier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	args, err := q.take(args)
	if err != nil {
		return errRow{err}
	}
	return q.tx.QueryRow(ctx, sql, args...)
}

// errRow is a pgx.Row whose Scan reports err.
type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }
