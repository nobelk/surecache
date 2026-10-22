# Milestone 09: PostgreSQL reference adapter (`surecachepg`)

## Goal

Ship pgx v5 primitives that make a correct PostgreSQL `Source` straightforward to write:
the advisory-lock fence, the fenced snapshot reader, connection configuration with
server-side and transport bounds, a nothing-committed error classifier and a budget check.
Prove them with the conformance suite against a real PostgreSQL, and implement the two
integration recipes of section 11.3.

## Prerequisites

Milestone 08. Read design sections 4.3, 4.6, 4.7, 7, 11.3 and 12. Read
[README.md](README.md) decisions D13 to D16 and clarification 11.

## Requirements covered

Section 4.3, 4.6, 4.7; NFR-6 budget check; section 11.3 recipes.

## Deliverables

```
surecachepg/go.mod          requires github.com/jackc/pgx/v5, github.com/nobelk/surecache
surecachepg/fence.go        Fence: Shared, Exclusive, WithMutation
surecachepg/snapshot.go     ReadSnapshot with REPEATABLE READ READ ONLY
surecachepg/conn.go         ConfigurePool(cfg *pgxpool.Config, Bounds)
surecachepg/errors.go       IsNothingCommitted for SQLSTATE codes
surecachepg/bounds.go       Bounds, DefaultBounds, Validate, CheckBudgets
surecachepg/internal/pgtest/      per-test database, DSN handling, TCP proxy
surecachepg/internal/groupstore/  test-only two-table schema and Source
surecachepg/*_test.go
```

The adopter-facing decorator example is a separate module, `examples/groups`, built in
milestone 11 (clarification 11). `surecachepg` itself never imports `promobs`.

## Design details

### Bounds and budget check

```go
type Bounds struct {
    FenceLockTimeout      time.Duration // lock_timeout in the exclusive fence tx
    FenceStatementTimeout time.Duration // statement_timeout in the exclusive fence tx
    ReadStatementTimeout  time.Duration // statement_timeout in the snapshot tx
    MaxReadStatements     int           // statements the snapshot callback may run
    IdleInTxTimeout       time.Duration // idle_in_transaction_session_timeout
    KeepAlive             KeepAlive     // TCP keepalive: idle, interval, count
}

func DefaultBounds() Bounds

// Validate rejects any bound that would be disabled or out of range on the server.
func (b Bounds) Validate() error

// CheckBudgets returns an error unless FenceWait() + ReadBound() < rebuildBudget.
func CheckBudgets(b Bounds, rebuildBudget time.Duration) error

// FenceWait is min(FenceLockTimeout, FenceStatementTimeout).
func (b Bounds) FenceWait() time.Duration

// ReadBound is ReadStatementTimeout × MaxReadStatements.
func (b Bounds) ReadBound() time.Duration
```

`statement_timeout` applies to each statement, not to the transaction. A snapshot callback
that runs several queries can therefore exceed one statement's timeout in total.
`ReadBound` multiplies by `MaxReadStatements`. The bound holds only if the callback
cannot run uncounted statements, so `ReadSnapshot` does **not** hand the callback a
`pgx.Tx` (which exposes `Conn()`, `SendBatch`, `Exec` and simple-protocol execution).
The callback gets a restricted interface instead:

```go
// SnapshotQuerier runs counted, single-statement reads inside the snapshot tx.
type SnapshotQuerier interface {
    Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
    QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}
```

Each call counts as one statement; the call past `MaxReadStatements` fails without
reaching the server. Every call forces the extended protocol
(`pgx.QueryExecModeCacheDescribe` prepended to the arguments), so one call cannot carry
several statements, and an argument that selects another exec mode is rejected. There is
no batch or connection access.

PostgreSQL 17's `transaction_timeout` was considered as the aggregate bound and rejected
for v0.1: it terminates the whole session rather than cancelling the statement, and it
would raise the minimum server version.

Defaults, all set explicitly (they satisfy section 4.6 with the default 4 s rebuild
budget, since 1.5 s + 2 s < 4 s):

| Bound | Default |
| --- | --- |
| `FenceLockTimeout` | 1.5 s |
| `FenceStatementTimeout` | 1.5 s |
| `ReadStatementTimeout` | 1 s |
| `MaxReadStatements` | 2 |
| `IdleInTxTimeout` | 5 s |
| `KeepAlive` | idle 5 s, interval 2 s, count 3 |

`Validate` rules: every duration is positive; durations are converted to milliseconds by
**rounding up** (`(d + time.Millisecond - 1) / time.Millisecond`), so a positive
sub-millisecond value becomes 1 ms and never 0 (0 disables a PostgreSQL timeout); the
converted value is at most `math.MaxInt32` ms (the server's range for these settings);
`MaxReadStatements >= 1`; keepalive values are positive. `CheckBudgets` calls `Validate`
first and uses the rounded values.

### Fence

```go
type Fence struct{ key int64 }

func NewFence(key int64) Fence

// Shared takes the shared fence as the first statement of a mutation tx.
func (f Fence) Shared(ctx context.Context, tx pgx.Tx) error

// Exclusive acquires and releases the exclusive fence in its own READ COMMITTED tx
// (SET LOCAL lock_timeout, SET LOCAL statement_timeout, pg_advisory_xact_lock, COMMIT).
func (f Fence) Exclusive(ctx context.Context, db Beginner, b Bounds) error

// WithMutation begins a tx, takes the shared fence, runs fn and commits.
func WithMutation(ctx context.Context, db Beginner, f Fence, fn func(pgx.Tx) error) error
```

`SET LOCAL` does not accept bind parameters. Its values are formatted from the validated,
rounded millisecond integers with `strconv.FormatInt` plus the `ms` unit, never from
external input (section 4.7 note). `WithMutation` keeps single-statement writes inside an
explicit transaction, as section 4.7 requires.

### Snapshot reader

`ReadSnapshot(ctx, db, fence, bounds, fn func(SnapshotQuerier) error) error`: calls
`fence.Exclusive`, then begins `REPEATABLE READ READ ONLY`, sets the statement timeout,
runs `fn` with a `SnapshotQuerier` bound to the transaction, and commits. `LimitPlusOne(capacity int)`
and an over-capacity helper that wraps `surecache.ErrOverCapacity` standardize the "read
one row past capacity" pattern. Context errors are returned unwrapped (`==` comparable).

### Error classification

`IsNothingCommitted(err error, codes ...string) bool`: true only for a `*pgconn.PgError`
whose SQLSTATE is in the caller's list (for example `23505` unique violation, or `P0001`
raised by a precondition check). The doc comment states the adapter must call it only on
errors from a transaction it rolled back itself. Context errors, `pgconn.ConnectError`,
network errors and every other error return false. Not-found on a mutation is the
adapter's own sentinel when zero rows were affected and the transaction rolled back.

### Connection configuration

`ConfigurePool(cfg *pgxpool.Config, b Bounds) error` validates `b`, sets
`idle_in_transaction_session_timeout` in `RuntimeParams`, and installs a `net.Dialer`
with `KeepAliveConfig` (Go 1.23+). It changes only the config; the caller owns the pool.

### Test infrastructure

- `pgtest.DB(t)`: reads `SURECACHE_PG_DSN`, `t.Skip`s when unset, creates a uniquely named
  database, returns a pool for it, and drops it in `t.Cleanup`. Advisory locks are per
  database, so tests never interfere.
- `pgtest.Proxy`: a stdlib TCP proxy with `Down()` (reset all connections, refuse new
  ones) and `Up()`. It never pauses silently (section 11.3 explains why).
- `groupstore`: groups and members in two tables, a soft-delete column, a unique
  member-to-group constraint, a `Source` built with the primitives, and the
  `surecachetest` harness including `SnapshotProbe` (pause between the group and member
  queries).

## Tasks (test-first order)

1. `Bounds.Validate` table test (zero, negative, sub-millisecond rounding, overflow);
   `CheckBudgets` table test; `SET LOCAL` literal formatting test.
2. Fence: `TestFence_SharedDoesNotBlockShared`, `TestFence_ExclusiveWaitsForShared`,
   `TestFence_ExclusiveTimesOut` (lock_timeout honored), `TestFence_ExclusiveAfterRollback`.
3. `TestReadSnapshot_StatementLimitEnforced`: a callback running
   `MaxReadStatements + 1` statements fails before the extra statement reaches the
   server. `TestReadSnapshot_MultiStatementRejected`: `"SELECT 1; SELECT 2"` in one
   `Query` fails. `TestReadSnapshot_ExecModeOverrideRejected`: passing
   `pgx.QueryExecModeSimpleProtocol` as an argument fails.
4. `groupstore` and its harness. Run `surecachetest.Run` against it. Every subtest passes,
   none skipped (CI enforces no skips).
5. Error classification: a unique violation inside a rolled-back tx is nothing-committed;
   a context cancel during COMMIT is not; a killed backend (`pg_terminate_backend`) is not.
6. `TestAbandonedCommitFenced` (SAFE-4, SAFE-5). The test controls the commit with a
   server-side barrier, so it cannot pass without an overlapping late commit:
   1. Install a `DEFERRABLE INITIALLY DEFERRED` constraint trigger on the members table
      that calls `pg_advisory_lock(<barrier key>)` and then unlocks it. The test session
      holds that advisory lock first.
   2. Through `pgtest.Proxy`, run a mutation through `Cache.Mutate`. Its COMMIT blocks
      server-side in the trigger, still holding the shared fence.
   3. Call `Proxy.Down()`. The client sees a network error. Assert that `Mutate` returned
      that error and the cache is stale (ambiguous outcome).
   4. Assert from `pg_stat_activity` that the mutation backend is still active, and from
      `pg_locks` that it still holds the shared fence.
   5. `Proxy.Up()`. Let `Run` tick. Assert from `pg_locks` that the rebuild's exclusive
      fence request is waiting (not granted) while the barrier is held. The test holds
      the barrier for less than `FenceWait()`.
   6. Release the barrier. The late COMMIT lands; the rebuild acquires the fence, reads,
      and installs. Assert the installed state **includes** the late commit.
7. Recipe `TestRecipe_ReversibleOutage`, for both outage policies: prime through the
   proxy; `Down()`; assert the policy's behavior (section 8); `Up()`; assert recovery
   through `Run`.
8. Recipe `TestRecipe_Restart`: seed every entity shape, including cacheable entities with
   no index keys; a fresh cache primes and matches the store exactly.
9. `TestConfigurePool_SetsBounds`: `SHOW idle_in_transaction_session_timeout` on a pooled
   connection.

## Acceptance

- `make pgtest` green against PostgreSQL 17 and 18 in CI, with zero skipped tests.
- `surecachepg` has 90%+ coverage (it is mostly integration code).
- `surecachepg/go.mod` directly requires only `pgx/v5` and `surecache`.
- `go list -m all` in the root module with `GOWORK=off` still shows no dependencies.
