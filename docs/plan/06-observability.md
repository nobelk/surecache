# Milestone 06: Observability

## Goal

Add the edge-triggered `slog` logs from section 9 and audit the observer contract: every
event in the section 9 signal table is emitted exactly where it should be, never under the
lock, and never per read in logs.

## Prerequisites

Milestone 04. Read design sections 3.3 (last two bullets), 3.4 (NFR-4), 6.6 and 9. Can run
in parallel with milestone 05. Events from milestone 05 (`UnhealthyChanged`,
`ProbeFailure`) are audited here if 05 has landed, or in 05's own tests otherwise.

## Requirements covered

NFR-4, the "no log or observer call under the lock" forbidden behavior, internal invariant
1.

## Deliverables

```
log.go          edge-triggered logging
observer.go     final event emission points, docs on the embedding rule
*_test.go
```

## Design details

### Logs

| Message | Level | When | Attributes |
| --- | --- | --- | --- |
| `surecache_stale` | Warn | every trusted to stale transition | `cache`, `cause` (error string) |
| `surecache_rebuild_failed` | Error | first `Failed` loop rebuild in a stale episode | `cache`, `cause`, `attempt` |
| `surecache_recovered` | Info | an installed rebuild that ends a stale episode | `cache`, `attempts`, `stale_for` (duration) |

- Message strings are constants. Attributes never include entity contents or ids.
- The context passed to the logger is the mutation's `ctx` for stale transitions raised by
  a mutation, and `tickCtx` for loop events, so handlers can attach trace ids
  (`logger.LogAttrs(ctx, ...)`).
- Episode state (`episodeStart`, `episodeAttempts`, `episodeFailedLogged`) lives under the
  cache lock and is reset when a rebuild installs.
- `surecache_stale` and `surecache_recovered` are state-event logs. They go through the
  ordered state-event queue from milestone 02, so every transition is logged, in order,
  and logs and observer state never disagree.
- A prime failure is reported to the caller through the return value and does not log
  `surecache_rebuild_failed`. Prime is not a loop rebuild.
- `slog.LogAttrs` with pre-built attributes, so the disabled-handler path allocates nothing
  measurable. Logs are never on the read path.

### Event emission audit

Map every signal of section 9 to its call site and test:

| Signal | Observer method | Raised by |
| --- | --- | --- |
| `surecache_stale` | `StaleChanged` | `markStaleLocked` transition, rebuild install |
| `surecache_unhealthy` | `UnhealthyChanged` | health state transitions (05) |
| `surecache_entities` | `Entities` | rebuild install, refresh install or remove |
| `surecache_reads_total` | `Read` | every `Get`, `Lookup`, `List` |
| `surecache_rebuilds_total` | `Rebuild` | prime and loop, every result |
| `surecache_ambiguous_mutations_total` | `AmbiguousMutation` | settle |
| `surecache_store_probe_failures_total` | `ProbeFailure` | prober and `ObserveStoreResult` (05) |

## Tasks (test-first order)

1. A test logger: `slog.New` with a handler that records records, plus a counter of calls.
2. `TestLog_StaleOncePerTransition`: two ambiguous mutations in a row produce one
   `surecache_stale`.
3. `TestLog_RebuildFailedOncePerEpisode`: under `synctest`, five failing ticks produce one
   Error. Recover, go stale again, fail: one more Error.
4. `TestLog_RecoveredOncePerEpisode` with `attempts` and `stale_for` attributes.
5. `TestLog_NeverPerRead`: 10,000 reads of every kind produce zero log records.
6. `TestLog_ContextPropagated`: a handler reads a value from the context; it sees the
   mutation's value on `surecache_stale` and the tick context's value on loop logs.
7. `TestLog_NoEntityContents`: an entity with a sentinel string field; no record contains
   it.
8. `TestObserver_CanReenterCache` and `TestLogger_CanReenterCache`: the callback calls
   `c.Get` and `c.List` on a separate goroutine and waits for it with a real-time
   `select` timeout (5 seconds). If the callback ran under the cache lock, the reentrant
   read would block, the timeout would fire, and the test would fail instead of hanging.
   These tests do **not** use `synctest`: blocking on `sync.Mutex` or `sync.RWMutex` is
   not durably blocking inside a bubble, so a deadlock would hang rather than be detected.
   Cover state events (which go through the emitter) and counter events separately.
9. `TestObserver_EventPerSignal`: a table driving each scenario above and asserting the
   exact event sequence.
10. `TestObserver_NopEmbedding`: a type that embeds `NopObserver` and overrides one method
    compiles and receives only that event. This shows the forward-compatibility pattern.

## Acceptance

- All tests pass with `-race`.
- `go doc surecache.Observer` explains the embedding rule and that methods are called off
  the lock, possibly concurrently, and must not block.
