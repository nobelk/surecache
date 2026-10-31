// SPDX-License-Identifier: Apache-2.0

package surecachepg

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// ErrStatementLimit is returned by a SnapshotQuerier call past Bounds.MaxReadStatements;
// the statement never reaches the server.
var ErrStatementLimit = errors.New("surecachepg: snapshot read statement limit reached")

// ErrExecMode is returned when a SnapshotQuerier call passes a pgx.QueryExecMode: every
// call runs in the extended protocol, which carries exactly one statement.
var ErrExecMode = errors.New("surecachepg: snapshot reads choose their own exec mode")

// ErrQueryRewriter is returned when a snapshot query passes a pgx.QueryRewriter
// (including pgx.NamedArgs): rewriters receive the raw connection and could run
// uncounted statements. Use positional parameters instead.
var ErrQueryRewriter = errors.New("surecachepg: snapshot reads do not allow query rewriters")

// SnapshotQuerier runs counted, single-statement reads inside the snapshot transaction.
// It deliberately exposes no batch, Exec or connection access: statement_timeout bounds
// each statement, so the read bound holds only if every statement is counted.
// Query preserves pgx.Rows compatibility, but its Conn method always returns nil.
// Query rewriters (including pgx.NamedArgs) are rejected; use positional parameters.
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
		if _, ok := a.(pgx.QueryRewriter); ok {
			return nil, ErrQueryRewriter
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
	rows, err := q.tx.Query(ctx, sql, args...)
	if rows == nil {
		return nil, err
	}
	// pgx can return error-bearing rows alongside an error. Keep them usable
	// through Err(), but never expose the underlying connection.
	return snapshotRows{rows: rows}, err
}

func (q *querier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	args, err := q.take(args)
	if err != nil {
		return errRow{err}
	}
	return snapshotRow{row: q.tx.QueryRow(ctx, sql, args...)}
}

// snapshotRows delegates explicitly: embedding pgx.Rows would promote connection
// access (including any methods added by pgx in the future).
type snapshotRows struct{ rows pgx.Rows }

func (r snapshotRows) Close()                        { r.rows.Close() }
func (r snapshotRows) Err() error                    { return r.rows.Err() }
func (r snapshotRows) CommandTag() pgconn.CommandTag { return r.rows.CommandTag() }
func (r snapshotRows) FieldDescriptions() []pgconn.FieldDescription {
	return r.rows.FieldDescriptions()
}
func (r snapshotRows) Next() bool             { return r.rows.Next() }
func (r snapshotRows) Scan(dest ...any) error { return r.rows.Scan(restrictScanner(dest)...) }
func (r snapshotRows) Values() ([]any, error) { return r.rows.Values() }
func (r snapshotRows) RawValues() [][]byte    { return r.rows.RawValues() }
func (snapshotRows) Conn() *pgx.Conn          { return nil }
func (r snapshotRows) TypeMap() *pgtype.Map   { return r.rows.TypeMap() }

// snapshotRow hides any additional methods of the transaction's concrete Row.
type snapshotRow struct{ row pgx.Row }

func (r snapshotRow) Scan(dest ...any) error { return r.row.Scan(restrictScanner(dest)...) }

// pgx passes its underlying Rows to a sole RowScanner destination. Interpose on
// that callback too, without changing pgx's scan/error/close handling.
func restrictScanner(dest []any) []any {
	if len(dest) == 1 {
		if scanner, ok := dest[0].(pgx.RowScanner); ok {
			return []any{snapshotScanner{scanner: scanner}}
		}
	}
	return dest
}

type snapshotScanner struct{ scanner pgx.RowScanner }

func (s snapshotScanner) ScanRow(rows pgx.Rows) error {
	return s.scanner.ScanRow(snapshotRows{rows: rows})
}

// errRow is a pgx.Row whose Scan reports err.
type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }
