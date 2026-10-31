// SPDX-License-Identifier: Apache-2.0

package surecachepg_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nobelk/surecache/surecachepg"
)

// Clarification 13: query results must not bypass the counted snapshot interface.
func TestReadSnapshot_RowsHideConnection(t *testing.T) {
	underlying := &snapshotTestRows{conn: new(pgx.Conn)}
	tx := &snapshotTestTx{rows: underlying}
	err := surecachepg.ReadSnapshot(context.Background(), snapshotTestDB{tx}, surecachepg.NewFence(77), surecachepg.DefaultBounds(),
		func(q surecachepg.SnapshotQuerier) error {
			rows, err := q.Query(context.Background(), "SELECT 1")
			if err != nil {
				return err
			}
			if rows.Conn() != nil {
				t.Error("snapshot rows expose the connection, allowing uncounted statements")
			}
			assertSnapshotResultOpaque(t, rows)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
}

// Clarification 13: hiding the connection preserves the remaining pgx.Rows API.
func TestReadSnapshot_RowsCompatibility(t *testing.T) {
	readErr := errors.New("read failed")
	underlying := &snapshotTestRows{readErr: readErr, typeMap: pgtype.NewMap()}
	tx := &snapshotTestTx{rows: underlying}
	err := surecachepg.ReadSnapshot(context.Background(), snapshotTestDB{tx}, surecachepg.NewFence(77), surecachepg.DefaultBounds(),
		func(q surecachepg.SnapshotQuerier) error {
			rows, err := q.Query(context.Background(), "SELECT 7")
			if err != nil {
				return err
			}
			if !rows.Next() {
				t.Fatal("Next returned false")
			}
			var got int
			if scanErr := rows.Scan(&got); scanErr != nil || got != 7 {
				t.Fatalf("Scan = %d, %v", got, scanErr)
			}
			values, err := rows.Values()
			if err != nil || !reflect.DeepEqual(values, []any{7}) {
				t.Fatalf("Values = %v, %v", values, err)
			}
			if !reflect.DeepEqual(rows.RawValues(), [][]byte{[]byte("7")}) {
				t.Fatal("RawValues changed")
			}
			if fields := rows.FieldDescriptions(); len(fields) != 1 || fields[0].Name != "n" {
				t.Fatalf("FieldDescriptions = %v", fields)
			}
			if rows.TypeMap() != underlying.typeMap {
				t.Fatal("TypeMap changed")
			}
			rows.Close()
			if !underlying.closed || rows.Next() {
				t.Fatal("Close was not delegated")
			}
			if rows.CommandTag().String() != "SELECT 1" || rows.Err() != readErr {
				t.Fatal("CommandTag or Err changed")
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
}

// Clarification 13: failed queries and exhausted budgets never expose raw results.
func TestReadSnapshot_QueryErrorsAndLimit(t *testing.T) {
	queryErr := errors.New("query failed")
	tx := &snapshotTestTx{err: queryErr, rows: &snapshotTestRows{readErr: queryErr, conn: new(pgx.Conn)}}
	b := surecachepg.DefaultBounds()
	b.MaxReadStatements = 1
	err := surecachepg.ReadSnapshot(context.Background(), snapshotTestDB{tx}, surecachepg.NewFence(77), b,
		func(q surecachepg.SnapshotQuerier) error {
			rows, err := q.Query(context.Background(), "SELECT 1")
			if rows == nil || err != queryErr {
				t.Fatalf("Query = %v, %v; want restricted error-bearing rows", rows, err)
			}
			if rows.Err() != queryErr || rows.Conn() != nil {
				t.Fatalf("error rows: Err = %v, Conn = %v", rows.Err(), rows.Conn())
			}
			rows.Close()
			if rows, err := q.Query(context.Background(), "SELECT 2"); rows != nil || err != surecachepg.ErrStatementLimit {
				t.Fatalf("Query past limit = %v, %v", rows, err)
			}
			if err := q.QueryRow(context.Background(), "SELECT 3").Scan(new(int)); err != surecachepg.ErrStatementLimit {
				t.Fatalf("QueryRow past limit = %v", err)
			}
			return nil
		})
	if err != nil || tx.calls != 1 {
		t.Fatalf("ReadSnapshot = %v; query calls = %d", err, tx.calls)
	}
}

// Clarification 13: even a custom transaction's concrete Row must stay opaque.
func TestReadSnapshot_RowHidesConnection(t *testing.T) {
	underlying := &snapshotTestRows{conn: new(pgx.Conn)}
	tx := &snapshotTestTx{row: underlying}
	err := surecachepg.ReadSnapshot(context.Background(), snapshotTestDB{tx}, surecachepg.NewFence(77), surecachepg.DefaultBounds(),
		func(q surecachepg.SnapshotQuerier) error {
			row := q.QueryRow(context.Background(), "SELECT 7")
			if _, ok := row.(interface{ Conn() *pgx.Conn }); ok {
				t.Error("snapshot row exposes connection access")
			}
			assertSnapshotResultOpaque(t, row)
			var got int
			if err := row.Scan(&got); err != nil || got != 7 {
				t.Fatalf("Scan = %d, %v", got, err)
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
}

// Clarification 13: custom scanner callbacks must also receive restricted rows.
func TestReadSnapshot_ScannerHidesConnection(t *testing.T) {
	for _, single := range []bool{false, true} {
		t.Run(map[bool]string{false: "Query", true: "QueryRow"}[single], func(t *testing.T) {
			underlying := &snapshotTestRows{conn: new(pgx.Conn)}
			tx := &snapshotTestTx{rows: underlying, row: underlying}
			err := surecachepg.ReadSnapshot(context.Background(), snapshotTestDB{tx}, surecachepg.NewFence(77), surecachepg.DefaultBounds(),
				func(q surecachepg.SnapshotQuerier) error {
					scanner := &snapshotTestScanner{t: t}
					if single {
						return q.QueryRow(context.Background(), "SELECT 7").Scan(scanner)
					}
					rows, err := q.Query(context.Background(), "SELECT 7")
					if err != nil {
						return err
					}
					defer rows.Close()
					if !rows.Next() {
						t.Fatal("no row")
					}
					return rows.Scan(scanner)
				})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Clarification 13: exercise pgx's actual RowScanner dispatch when PostgreSQL is available.
func TestReadSnapshot_RealRowsHideConnection(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	err := surecachepg.ReadSnapshot(ctx, pool, surecachepg.NewFence(77), surecachepg.DefaultBounds(),
		func(q surecachepg.SnapshotQuerier) error {
			rows, err := q.Query(ctx, "SELECT 7")
			if err != nil {
				return err
			}
			if rows.Conn() != nil {
				t.Error("Query exposes connection")
			}
			if !rows.Next() {
				rows.Close()
				t.Fatalf("Next = false; Err = %v", rows.Err())
			}
			err = rows.Scan(&snapshotTestScanner{t: t})
			rows.Close()
			if err != nil {
				return err
			}
			return q.QueryRow(ctx, "SELECT 7").Scan(&snapshotTestScanner{t: t})
		})
	if err != nil {
		t.Fatal(err)
	}
}

// Clarification 13: pgx rewriters receive the raw connection before execution.
// Reject them even when result-format options precede them, for both query APIs.
func TestReadSnapshot_QueryRewriterRejected(t *testing.T) {
	for _, single := range []bool{false, true} {
		for _, formats := range []bool{false, true} {
			t.Run(map[bool]string{false: "Query", true: "QueryRow"}[single]+map[bool]string{false: "", true: "/formats"}[formats], func(t *testing.T) {
				tx := &snapshotTestTx{rows: &snapshotTestRows{}, row: &snapshotTestRows{}}
				err := surecachepg.ReadSnapshot(context.Background(), snapshotTestDB{tx}, surecachepg.NewFence(77), surecachepg.DefaultBounds(),
					func(q surecachepg.SnapshotQuerier) error {
						args := []any{snapshotTestRewriter{}}
						if formats {
							args = append([]any{pgx.QueryResultFormats{0}, pgx.QueryResultFormatsByOID{23: 0}}, args...)
						}
						if single {
							return q.QueryRow(context.Background(), "SELECT 7", args...).Scan(new(int))
						}
						_, err := q.Query(context.Background(), "SELECT 7", args...)
						return err
					})
				if !errors.Is(err, surecachepg.ErrQueryRewriter) || tx.calls != 0 {
					t.Fatalf("rewriter query = %v, calls=%d; want rejected before transaction access", err, tx.calls)
				}
			})
		}
	}
}

type snapshotTestRewriter struct{}

func (snapshotTestRewriter) RewriteQuery(context.Context, *pgx.Conn, string, []any) (string, []any, error) {
	panic("snapshot must not invoke connection-bearing rewriters")
}

type snapshotTestScanner struct{ t *testing.T }

func (s *snapshotTestScanner) ScanRow(rows pgx.Rows) error {
	if rows.Conn() != nil {
		s.t.Error("scanner received the underlying connection")
	}
	assertSnapshotResultOpaque(s.t, rows)
	var got int
	if err := rows.Scan(&got); err != nil || got != 7 {
		s.t.Fatalf("scanner Scan = %d, %v", got, err)
	}
	return nil
}

// Opaque results expose neither exported fields nor promoted escape-hatch methods.
func assertSnapshotResultOpaque(t *testing.T, result any) {
	t.Helper()
	if _, ok := result.(interface{ UnderlyingConnection() *pgx.Conn }); ok {
		t.Error("snapshot result promotes an underlying escape hatch")
	}
	typ := reflect.TypeOf(result)
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() == reflect.Struct {
		for i := range typ.NumField() {
			if field := typ.Field(i); field.IsExported() {
				t.Errorf("snapshot result exposes field %s", field.Name)
			}
		}
	}
}

type snapshotTestDB struct{ tx pgx.Tx }

func (db snapshotTestDB) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return db.tx, nil
}

type snapshotTestTx struct {
	pgx.Tx // Unused transaction operations panic rather than silently succeeding.
	rows   pgx.Rows
	row    pgx.Row
	err    error
	calls  int
}

func (*snapshotTestTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (*snapshotTestTx) Commit(context.Context) error   { return nil }
func (*snapshotTestTx) Rollback(context.Context) error { return nil }
func (tx *snapshotTestTx) Query(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
	tx.calls++
	if len(args) == 0 || args[0] != pgx.QueryExecModeDescribeExec {
		return nil, errors.New("extended protocol not forced")
	}
	return tx.rows, tx.err
}

func (tx *snapshotTestTx) QueryRow(context.Context, string, ...any) pgx.Row {
	tx.calls++
	return tx.row
}

type snapshotTestRows struct {
	conn    *pgx.Conn
	closed  bool
	readErr error
	typeMap *pgtype.Map
}

func (r *snapshotTestRows) Close()                      { r.closed = true }
func (r *snapshotTestRows) Err() error                  { return r.readErr }
func (*snapshotTestRows) CommandTag() pgconn.CommandTag { return pgconn.NewCommandTag("SELECT 1") }
func (*snapshotTestRows) FieldDescriptions() []pgconn.FieldDescription {
	return []pgconn.FieldDescription{{Name: "n"}}
}
func (r *snapshotTestRows) Next() bool { return !r.closed }
func (r *snapshotTestRows) Scan(dest ...any) error {
	if len(dest) == 1 {
		if scanner, ok := dest[0].(pgx.RowScanner); ok {
			return scanner.ScanRow(r)
		}
	}
	*dest[0].(*int) = 7
	return nil
}
func (*snapshotTestRows) Values() ([]any, error)            { return []any{7}, nil }
func (*snapshotTestRows) RawValues() [][]byte               { return [][]byte{[]byte("7")} }
func (r *snapshotTestRows) TypeMap() *pgtype.Map            { return r.typeMap }
func (r *snapshotTestRows) Conn() *pgx.Conn                 { return r.conn }
func (r *snapshotTestRows) UnderlyingConnection() *pgx.Conn { return r.conn }
