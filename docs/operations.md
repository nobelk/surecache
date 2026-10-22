# Operations runbook

How to run a service that uses `surecache` safely. The design is in
[design.md](design.md); this page is about what an operator does.

## Administrative writes (PRE-1)

The cache assumes its process is the only writer of the cached data. It cannot see a
write made anywhere else, and while it is trusted nothing makes it re-read the store. A
manual `UPDATE`, a migration, a restore or a second deployment writing the same tables
can leave the cache serving wrong answers indefinitely.

Procedure for any write that does not go through the service:

1. Stop every instance of the service that owns the cache.
2. Make the write.
3. Start the service. `Prime` reloads everything from the store, and the service does
   not serve until it returns `Installed`.

Never run two instances that write the same cached data at the same time.

## Capacity

`Spec.Capacity` bounds the primary map and each `AddIndex` capacity bounds an index.
The cache never evicts.

- At startup, a store that holds more cacheable entities than the capacity makes
  `Prime` return `Failed` with `ErrOverCapacity`; the service must fail startup.
- At runtime, a mutation that would grow a map past its capacity marks the cache stale.
  The mutation itself succeeds. Every later rebuild also fails over capacity, so the
  cache stays stale and every read goes to the store.

Size each capacity to the domain with headroom. If the cacheable set can grow, give
operators a documented way to shrink it (archive, soft-delete) and alert on
`surecache_stale` staying at 1.

## Metrics

With the `promobs` observer (or your own `Observer`):

| Metric | Watch for |
| --- | --- |
| `surecache_stale` | 1 for longer than a few rebuild intervals: the loop cannot rebuild. Check the logs for `surecache_rebuild_failed`. |
| `surecache_unhealthy` | 1: the outage policy distrusts the store (`DistrustAfterProbeFailures` only). |
| `surecache_entities` | Approaching `Spec.Capacity`. |
| `surecache_reads_total{source="cache"}` | The hit share. A low share in steady state means frequent mutations or a stale cache. |
| `surecache_rebuilds_total{result="failed"}` | Rebuilds failing: store errors, timeouts, capacity or contract violations. |
| `surecache_rebuilds_total{result="discarded"}` | Rebuilds racing writes; occasional discards are normal. |
| `surecache_ambiguous_mutations_total` | Writes whose outcome was unknown; each makes the cache stale. |
| `surecache_store_probe_failures_total` | Health probe failures. |

## Logs

Logs are edge-triggered, never per read. Messages are static; attributes carry the
cache name and the cause, never entity contents.

| Message | Level | Meaning | Action |
| --- | --- | --- | --- |
| `surecache_stale` | Warn | The cache stopped trusting its contents (`cause` says why). Reads now go to the store. | None if a recovery follows. Investigate repeated ambiguous mutations. |
| `surecache_rebuild_failed` | Error | The first failed loop rebuild of a stale episode. | Check the cause: store down, fence wait timeout, capacity, contract violation. |
| `surecache_recovered` | Info | A rebuild ended the stale episode (`attempts`, `stale_for`). | None. |

A `cause` of `surecache: store contract violation` or `surecache: index key held by
another entity` means the store broke a precondition (PRE-2, PRE-3 or PRE-4): find the
write that bypassed or misclassified the mutation path.

## Outages

The service chooses an outage policy at build time; there is no default.

`ServeWhileExact`:

- The trusted cache keeps answering through an outage until the first mutation.
- From the first mutation, reads it affects defer, so callers see store errors. When it
  fails ambiguously the cache goes stale; all reads go to the store and fail while it
  is down.
- When the store returns, the loop rebuilds within a rebuild interval plus the rebuild
  budget, and the cache answers again.

`DistrustAfterProbeFailures`:

- Reads defer within about `FailAfter × Interval + Timeout` of an outage, even with no
  writes.
- After the store returns, `RecoverAfter` consecutive probe successes clear the
  distrust. A rebuild never clears it.

Nothing retries a failed write; the service must drive retries.

## Server-side bounds (PostgreSQL)

Budgets bound waits the cache controls; the server and the network need their own
bounds. With `surecachepg`:

- `ConfigurePool` sets `idle_in_transaction_session_timeout`, so an abandoned
  transaction cannot hold the mutation fence forever, and installs TCP keepalive.
- The exclusive fence runs with `lock_timeout` and `statement_timeout`; the snapshot read
  with `statement_timeout` per statement and a statement count limit.
- Call `surecachepg.CheckBudgets(bounds, rebuildBudget)` at startup: the fence wait plus
  the read bound must be less than the rebuild budget.

Defaults: fence wait 1.5 s, read bound 2 s (1 s × 2 statements), rebuild budget 4 s,
prime budget 10 s, idle-in-transaction timeout 5 s, keepalive idle 5 s, interval 2 s,
3 probes.

Each cache uses one advisory-lock key. Advisory locks are scoped to the database:
caches in different databases may share a key; caches in one database must not.
