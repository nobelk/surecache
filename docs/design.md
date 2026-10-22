# surecache: Design of a Write-Invalidated Cache for Safety-Critical Reads

- **Status**: draft design for an open-source Go library
- **Name**: `surecache` (it answers only when it is sure; otherwise it asks the store)
- **Tagline**: "Sure, or it asks."
- **Module**: `github.com/nobelk/surecache`
- **Date**: 2026-10-21
- **Implementation plan**: [docs/plan/README.md](plan/README.md). Where the plan records a
  clarification or deviation (plan section "Spec clarifications and deviations"), the plan
  wins.

## 1. Purpose

`surecache` is an in-process, read-mostly cache for systems where a wrong answer is worse
than a slow one. It sits in front of an authoritative store (typically a SQL database),
answers hot reads from memory, and keeps every write synchronous to the store.

Most cache libraries optimize for hit rate and memory, and accept short windows of
staleness. `surecache` makes the opposite trade:

1. **Never return an answer the store has moved past.** Not after a confirmed write, not
   while a write is in flight, and not after a write whose outcome is unknown.
2. **Keep answering correctly while the store is unavailable**, for as long as the cache
   can prove it is still exact (subject to an explicit outage policy, section 8).
3. **Be fast**, as a distant third priority.

This document states the guarantees, the assumptions they rest on, what the backing store
must provide, the algorithms that meet each guarantee, a proposed public API, and the
verification an implementation must pass. It is intended as the specification for the
library's first release.

### 1.1 When to use it

- Reads vastly outnumber writes (for example periodic sweeps at several hertz, and writes
  paced by humans).
- The working set is bounded and fits in memory.
- One process is the only writer of the cached data (section 3.5).
- A stale or missing answer has real cost: a safety action skipped, data silently dropped,
  an authorization granted wrongly.

### 1.2 When not to use it

- Several processes write the same data. The library cannot see their writes.
- The data set is unbounded or larger than memory. `surecache` never evicts.
- Bounded staleness is acceptable. A conventional TTL cache is simpler.

## 2. Concepts and terms

| Term | Meaning |
| --- | --- |
| **Store** | The authoritative source of truth. The cache never answers for it when in doubt. |
| **Entity** | One cached value, identified by a **primary key** (`ID`). |
| **Cacheable** | A caller-supplied predicate. Only cacheable entities are held. A non-cacheable entity (for example a soft-deleted row) still exists in the store. |
| **Secondary index** | A map from a secondary key (`K`) to the one entity that currently holds it. Example: "which group contains member M", "which group does user U own". |
| **Directory** | The sorted list of all cacheable primary keys. |
| **Mutation** | Any write to the store that can change a cached entity, an index key, or the directory. |
| **Nothing-committed error** | An error the store guarantees was returned with no change committed (a rejected precondition, a rolled-back transaction). |
| **Ambiguous outcome** | Any other mutation error. The change may have committed, or may still commit. |
| **Fence** | A store-side mechanism that orders a bulk read after every earlier mutation (section 4.3). |
| **Bulk load** | One consistent read of every cacheable entity, taken after the fence. |
| **Trusted / stale** | Trusted: the cache is known complete and exact for committed state. Stale: it is not, and every read goes to the store. |
| **Pending** | An entity with a mutation in flight. Reads about it go to the store. |
| **In flight** | The count of all mutations in progress, including creates whose id is not yet known. |
| **Refresh** | Re-reading one entity from the store after its mutation committed. |
| **Rebuild** | Replacing the whole cache from a bulk load. The only way to go from stale to trusted. |
| **Prime** | The first rebuild, at startup. |
| **Defer** | The cache declines to answer; the caller asks the store. Defer never means "absent". |

## 3. Requirements

"MUST" items are safety or correctness invariants. An implementation that violates one is
rejected regardless of performance.

### 3.1 Safety invariants

- **SAFE-1 No stale answer after a confirmed commit.** Once a mutation has returned
  success to its caller, no read that starts afterwards may return the pre-mutation state,
  whether presence, absence, field values, index membership or directory membership.
- **SAFE-2 No stale answer while a mutation is in flight.** From the start of a mutation
  until its post-commit refresh installs (or the cache goes stale), every read the
  mutation can affect MUST defer. A create's id is unknown until it returns, so during any
  in-flight mutation an index lookup that misses MUST defer rather than answer "absent".
- **SAFE-3 Absence is authoritative only when the cache is trusted for that question.**
  A primary-key miss MUST always defer, because non-cacheable entities exist in the store
  but not in the cache. An index miss or the directory may be answered authoritatively
  only while the cache is trusted and no mutation is in flight.
- **SAFE-4 An ambiguous outcome destroys trust.** A mutation that returns anything other
  than success or a nothing-committed error MUST mark the whole cache stale at once. This
  includes context cancellation and deadline errors once the call reached the store: many
  drivers return the context error in place of a COMMIT the server applied, or is still
  applying on a backend the client abandoned.
- **SAFE-5 A single-entity read cannot restore trust.** After an ambiguous outcome, the
  write may still be committing on an abandoned backend, and its id may be unknown (an
  ambiguous create). Only a rebuild whose snapshot is ordered after every earlier write
  (the fence) may restore trust.
- **SAFE-6 No older read installed over newer state.** A refresh or rebuild that read
  before a later commit MUST NOT install its result over that commit. A race detector
  cannot find this hazard; it needs explicit ordering (section 6).
- **SAFE-7 An index removal never deletes another entity's entry.** When the cache removes
  an index entry while re-indexing entity X, it removes the entry only if the entry still
  names X. A stalled refresh must not delete an entry a later commit moved to entity Y.
- **SAFE-8 Never a subset.** A rebuild installs every cacheable entity or nothing. Over
  capacity, the cache refuses and stays stale (at runtime) or fails startup (at prime). It
  never evicts and never installs a partial set.
- **SAFE-9 Fail closed at startup.** The cache starts stale and never serves before its
  first successful prime. A failed, timed-out or discarded prime is reported to the caller,
  who is expected to fail startup.
- **SAFE-10 Callers get copies.** Every served entity is a deep copy. No caller can mutate
  shared cache state.
- **SAFE-11 Panics leave the cache stale.** If a mutation unwinds by panic after reaching
  the store, its outcome is unknown, so the cache goes stale before releasing its marks.
- **SAFE-12 Contract violations fail safe.** If a store result breaks a stated
  precondition (a non-cacheable entity that still yields index keys, two entities claiming
  one unique key in one bulk load), the cache goes stale rather than install it.

### 3.2 Functional requirements

- **FUN-1 Reads from memory when trusted.** Primary lookups, index lookups and the
  directory are answered from memory when the cache is trusted and the deferral rules
  (section 6.1) allow.
- **FUN-2 Stale means pass-through.** A stale cache defers every read. The cost and
  behavior equal having no cache, a known-good baseline.
- **FUN-3 Never rebuild on the read path.** A read never triggers a rebuild, even when
  stale. Rebuilding inline would park a hot-path worker on a full load and create a
  serialized stampede against a store that is already failing.
- **FUN-4 Never rebuild on the write path.** A mutation on a stale cache refreshes nothing
  and rebuilds nothing, so a user-facing write never waits on the exclusive fence behind
  other writes. It still invalidates in-progress rebuilds (section 6.3).
- **FUN-5 One rebuilder.** A single background loop rebuilds a stale cache on a fixed
  interval under a budget. No other code path rebuilds after prime.
- **FUN-6 Post-commit refresh.** After a successful mutation, the cache re-reads the
  affected entity and installs it, under a context detached from the caller's
  cancellation and bounded by a refresh budget. Values the caller attached to the context
  (trace or correlation ids) survive the detach. A failed or timed-out refresh marks the
  cache stale.
- **FUN-7 Nothing-committed errors leave the cache as is.** The cached copy is still exact,
  so nothing is refreshed.
- **FUN-8 Caller results are never rewritten.** The mutation's own result is returned
  unchanged. A refresh failure after a successful mutation marks the cache stale but is
  never reported as a mutation failure.

### 3.3 Forbidden behaviors

An implementation MUST NOT:

- expire entries on a timer, evict on size (LRU, LFU, admission policies), or drop writes
  under pressure;
- serve cached data when the store errors ("stale-while-error");
- buffer writes and commit later (write-behind);
- coalesce a read with an in-flight load that started before a mutation (classic
  singleflight read-through), since that load can return pre-mutation state;
- rely on store notifications (LISTEN/NOTIFY, change streams) as the only invalidation
  source, since they arrive after commit and can be lost across reconnects;
- call the store while holding the cache lock;
- emit logs or observer callbacks while holding the cache lock.

### 3.4 Non-functional requirements

- **NFR-1 Bounded memory.** The primary map, every index and the directory have a stated
  capacity with refuse-on-overflow. Keys come from store results, never from untrusted
  input. Internal bookkeeping (pending marks, refresh windows) is bounded by concurrent
  mutations and drops entries at zero.
- **NFR-2 Zero store calls in steady state.** While trusted and with no mutations, reads
  issue no store calls. Startup issues one bulk load (two short transactions for the
  PostgreSQL reference) regardless of history.
- **NFR-3 Cheap reads.** A trusted read is one read-lock acquisition, one map lookup and
  one clone. No reflection, no per-read allocation beyond the clone, no per-read logging.
- **NFR-4 Observable.** State transitions and read sources are reported through an
  observer interface (section 9). Logs are edge-triggered, never per read.
- **NFR-5 Deterministic tests.** Every timer and deadline is injectable or runs correctly
  under virtual time (`testing/synctest`). The library ships deterministic interleaving
  tests and a store conformance suite (section 11).
- **NFR-6 Bounded budgets.** Prime, rebuild and refresh each run under a configurable
  budget. Budgets bound waits the cache controls; see section 12 for what they cannot
  bound.
- **NFR-7 Minimal dependencies.** The core package depends only on the Go standard library
  (`log/slog` for logging). Metric exporters live in optional subpackages.

### 3.5 Preconditions the caller guarantees

- **PRE-1 Single writer.** The process that owns the cache is the only writer of the
  cached data, and every write goes through the cache's mutation API. A write from
  anywhere else can leave a trusted cache wrong indefinitely: the rebuild loop only runs
  while the cache is stale, and the library cannot detect the write. Stop the process
  before administrative writes and restart (re-prime) afterwards.
- **PRE-2 Unique index keys.** In committed state, each secondary key is held by at most
  one entity, enforced by the store.
- **PRE-3 Non-cacheable entities hold no index keys.** Otherwise an index "absent" answer
  would be wrong for a key held by a non-cacheable entity. This is a store adapter
  guarantee the cache cannot fully enforce: `LoadAll` excludes non-cacheable rows, so a
  rebuild never sees them. A refresh detects a violation it encounters and fails safe
  (SAFE-12). Adapters MUST verify predicate and index parity, preferably through schema
  constraints and the conformance fixtures (section 11.2).
- **PRE-4 Complete mutation classification.** Every store operation that can change
  cached data is routed through `Mutate` or `Create` with the right ids. `Create` is only
  for an operation that inserts exactly one previously nonexistent entity and modifies no
  existing entity. Upserts, and any operation that can change an existing entity, MUST
  use `Mutate` with every affected id. Section 10.3 shows how to make the compiler help.

## 4. What the store must provide

The cache is only as correct as the store's contract. A store adapter MUST provide all of
the following. Section 4.7 describes the PostgreSQL reference adapter.

### 4.1 Atomic mutations

Each mutation is one transaction. Preconditions (uniqueness, ownership, existence) are
checked inside the same atomic operation as the write, never check-then-act.

### 4.2 A closed list of nothing-committed errors

The adapter documents a closed set of errors, each returned only when the store committed
nothing: a statement that changed no row, or a transaction that rolled back. Typical
members: not found, already exists, precondition failed, unique key held elsewhere.

Everything else is ambiguous: driver errors, network errors, unknown errors, and context
errors once the call reached the store. The adapter must never map a context error or a
driver error onto a nothing-committed error, and never return a nothing-committed error
after a partial commit. The caller passes the classifier to the cache
(`Spec.CommittedNothing`).

The library ships a conformance test (section 11.2) that checks, for each listed error,
that the store state is unchanged after the error is returned.

### 4.3 A mutation fence

The store provides a way to order a bulk read after every earlier mutation, including a
mutation whose client gave up during COMMIT. Requirements:

- Every mutation holds a **shared** lock on one fence for the whole of its transaction.
  Shared mode means mutations never block each other.
- The bulk load acquires the fence **exclusively**, in a separate transaction that
  commits before the read begins. The exclusive grant proves that no mutation transaction
  is still open, so every earlier mutation has committed or rolled back.
- The read then runs in its own transaction, so its snapshot follows all of them.
- Read-only paths that write nothing take no fence.

A row lock (`SELECT ... FOR UPDATE`) is not a substitute: it cannot fence an ambiguous
create, whose id the cache never learned. One fence covers every case.

### 4.4 Single-entity read

`LoadOne(ctx, id)` MUST return a detached, internally consistent snapshot of the entity
(an entity spread over several tables is read in one transaction), from the authoritative
writer, including every commit completed before the call. A read replica does not
qualify. Its not-found error MUST mean the entity authoritatively does not exist, and a
successful result MUST satisfy `Key(entity) == id`.

### 4.5 One consistent bulk load

`LoadAll(ctx)` returns every cacheable entity from one consistent snapshot taken after the
fence, from the authoritative writer, with each primary key exactly once.

- **Capacity refusal.** It reads one row past the capacity to detect overflow and returns
  an over-capacity error with no entities. It never returns a subset.
- **Context errors** are returned unwrapped, so the caller can tell cancellation apart.
- **Predicate parity.** The store-side filter for "cacheable" MUST be the exact equivalent
  of `Spec.Cacheable`. The conformance suite checks parity on fixtures.

### 4.6 Server-side and transport bounds

A context deadline alone cannot always stop a call to a hung store (section 12). The
adapter SHOULD add bounds the server enforces:

- a lock wait timeout and statement timeout on the fence transaction;
- a statement timeout on the bulk-read transaction;
- an idle-in-transaction timeout on every connection, so an abandoned transaction cannot
  hold the shared fence forever;
- TCP keepalive on the connection dialer, so a dead peer is detected.

The fence wait plus the read bound MUST be less than the rebuild budget. Enforce it with a
compile-time or startup check.

### 4.7 PostgreSQL reference adapter

| Requirement | PostgreSQL mechanism |
| --- | --- |
| Fence key | One fixed 64-bit advisory-lock key per cache |
| Shared fence in a mutation | `SELECT pg_advisory_xact_lock_shared($key)` as the first statement of the transaction. Single-statement writes run in an explicit transaction so they can take it. |
| Exclusive fence | Separate READ COMMITTED transaction: `SET LOCAL lock_timeout`, `SET LOCAL statement_timeout`, then `SELECT pg_advisory_xact_lock($key)`, then COMMIT |
| Consistent read | REPEATABLE READ, READ ONLY transaction with `SET LOCAL statement_timeout`; `LIMIT capacity + 1` on each query |
| Abandoned transactions | `idle_in_transaction_session_timeout` as a connection parameter |
| Dead peers | Dialer with TCP keepalive (for example idle 5 s, interval 2 s, 3 probes) |

Notes:

- `SET LOCAL` does not accept bind parameters. Format the value from a constant as a
  literal; never from input.
- Advisory locks are scoped to the database, not the schema. Tests that hold the fence
  open need their own database.
- Example budgets that satisfy section 4.6: fence wait 1.5 s, read bound 2 s, rebuild
  budget 4 s, prime budget 10 s.

A store without shared and exclusive advisory locks needs an equivalent reader-writer
barrier with the same semantics.

## 5. State model

### 5.1 State

One `sync.RWMutex` guards all state. No store call and no observer callback runs under
it.

| Field | Purpose |
| --- | --- |
| `entities` | bounded map, primary key to entity |
| `indexes[i]` | bounded map per secondary index, key to primary key |
| `directory` | sorted primary keys |
| `stale` | snapshot distrust: true means not known complete. Starts true. Cleared only by an installed rebuild. |
| `unhealthy` | health distrust from the outage policy (section 8). Starts false. Cleared only by the policy's recovery rule, never by a rebuild. Every read defers while either `stale` or `unhealthy` is true; "trusted" below means both are false. |
| `pending` | map primary key to count of in-flight mutations on it |
| `inFlight` | count of all in-flight mutations, creates included |
| `generation` | global counter. Bumped by every refresh start, every mark-stale and every rebuild capture. A rebuild installs only if it is unchanged since capture. |
| `refreshes` | map primary key to `{gen, active}`, the per-entity refresh window. Orders same-entity refreshes. Dropped when `active` reaches zero. |

The bounded map refuses a new key at capacity and stores nothing; overwriting an existing
key always succeeds. It has no background goroutine and no expiry.

### 5.2 Trust states

```
                Prime / rebuild installed
     +--------+  (generation unchanged)   +-----------+
     | STALE  | ------------------------> |  TRUSTED  |
     | (start)| <------------------------ |           |
     +--------+   ambiguous mutation      +-----------+
          ^       refresh failed/timed out      |
          |       capacity overflow             | successful mutation:
          |       mutation panicked             | pending -> refresh -> install
          |       contract violation            v
          +----- rebuild failed or      (still TRUSTED)
                 discarded (stays STALE)
```

- STALE: reads defer; mutations run but refresh nothing and rebuild nothing; only an
  installed rebuild leaves STALE.
- TRUSTED: reads answer from memory except as section 6.1 requires.

## 6. Algorithms

`lock` is the write lock and `rlock` the read lock. Pseudocode omits metrics except where
they matter.

### 6.1 Reads

```
Get(id):
    rlock
      if stale              -> Defer
      elif pending[id] > 0  -> Defer
      elif id not in entities -> Defer          # primary absence never authoritative
      else e = entities[id]
    runlock
    return Clone(e), Found

Lookup(index, key):
    rlock
      id, hit = indexes[index][key]
      if stale                        -> Defer
      elif hit and pending[id] > 0    -> Defer
      elif not hit and inFlight > 0   -> Defer  # a create or update may be adding key
      elif hit                        -> Found(id)
      else                            -> Absent # authoritative negative
    runlock

List():
    rlock
      if stale or inFlight > 0 -> Defer
      else ids = copy(directory)
    runlock
    return ids
```

Why a hit and a miss defer differently: a hit names an id the cache can check for pending.
A miss names no id, and the mutation that would add the key may be a create whose id is
not yet known, so any in-flight mutation must defer it.

The clone runs after the read lock is released. This is safe only because installed
entities are immutable: an install replaces the map value and never mutates the old one,
and the `Source` must not retain or mutate a value after returning it (or the caller
enables `WithCloneOnInstall`). An implementation that cannot guarantee this clones under
the lock.

For brevity the pseudocode writes `stale`; every read condition means `stale or
unhealthy`.

### 6.2 Mutations

```
guard(ctx, apply, ids...):
    lock
    for id in ids:
        pending[id]++
    inFlight++                  # exactly once per mutation, creates included
    unlock
    settled = false
    defer:
        lock
        if not settled: markStaleLocked(errUnsettled)     # SAFE-11
        for id in ids: pending[id]--, delete at 0
        inFlight--
        unlock
        emit deferred logs and observer events
    apply()
    settled = true

Mutate(ctx, ids, op):
    guard(ctx, func(){ err = op(); settle(ctx, ids, err) }, ids...)
    return err                  # unchanged, FUN-8

Create(ctx, op):
    guard(ctx, func(){ id, err = op(); settle(ctx, [id] if err == nil, err) })
    return id, err              # no pending id: covered by inFlight alone

settle(ctx, ids, err):
    if err == nil:
        rctx = WithTimeout(WithoutCancel(ctx), refreshBudget)
        refreshBatch(rctx, ids)
    elif CommittedNothing(err):
        nothing                 # FUN-7
    else:
        count ambiguous
        markStale(errOutcomeUnknown wrapping err)          # SAFE-4
```

Pending marks are released only after settle returns, so the refresh has installed (or
the cache went stale) before any reader can hit the cache for that entity again. This
closes the commit-to-refresh gap (SAFE-2). Without it, a reader in that gap could act on a
list of members that omits one just added.

A mutation that touches several entities (for example moving a member between two groups)
passes every affected id.

### 6.3 Post-commit refresh

```
refreshBatch(ctx, ids):
    lock
      generation++                      # a rebuild reading now must discard
      if stale: unlock; return          # FUN-4
      for id in ids:                    # open every window before any read
          captured[id] = beginRefreshLocked(id)   # window.gen++, window.active++
          pending[id]++                 # the batch holds the entity (plan clarification 14)
    unlock
    defer for id in ids: endRefresh(id) # active--, pending--, delete each at 0

    for id in ids:
        refreshOne(ctx, id, captured[id])

refreshOne(ctx, id, captured):
    e, err = Source.LoadOne(ctx, id)    # off-lock
    if err is not-found: e = nil        # entity authoritatively deleted (section 4.4)
    elif err: markStale(err); return
    elif Key(e) != id: markStale(errContract); return      # SAFE-12

    lock (with the panic rule of section 6.6)
      if refreshes[id].gen != captured: unlock; return   # newer same-entity refresh owns it
      if e != nil and not Cacheable(e) and len(keys(e)) > 0:
          markStaleLocked(errContract); unlock; return     # PRE-3, SAFE-12
      if e != nil and Cacheable(e):
          for each index i, for k in keys_i(e):          # conflict check before any change
              holder = indexes[i][k]
              if holder exists and holder != id and refreshes[holder] has no active holder:
                  markStaleLocked(errKeyConflict); unlock; return
      removeLocked(id)                  # deindex with the current-holder check (SAFE-7)
      if e != nil and Cacheable(e):
          installLocked(id, e)          # index keys, then entity, then directory
            on capacity: markStaleLocked; return
    unlock

removeLocked(id):
    if prev = entities[id]:
        for each index i, for k in keys_i(prev):
            if indexes[i][k] == id: delete indexes[i][k]    # current-holder check
        delete entities[id]; remove id from directory
```

The `LoadOne` not-found error is part of the store contract (`Source.NotFound`), so a
deleted entity is removed rather than treated as a failure.

Why every window of a batch opens before any read: a mutation that moves key K from X to
Y names both. If X's window opened only when the batch reached X, an older refresh of X
(from an earlier mutation, holding K) could install after Y's refresh had indexed K,
overwrite K back to X, and then the batch's own refresh of X would remove K, leaving K
unindexed while Y holds it. With all windows opened up front, that older refresh of X is
discarded at its window check.

Why the insert-side conflict check: removal protects a key's current holder (SAFE-7), and
insertion needs the matching rule. An incoming key that already names another entity Z
may overwrite Z's entry only while Z has a refresh in progress, which happens exactly
when one mutation moved the key between entities in the same batch, and Z's own refresh
will then re-index Z (and its removal will skip K, which no longer names Z). In any other
case the incoming value is older than Z's installed state, or Z's state is wrong; the
cache cannot tell which, so it goes stale and leaves the conflict to a fenced rebuild.
All checks run before any map changes, so an aborted install leaves the maps untouched.

Why two counters:

- The **global generation** guards whole rebuilds. A rebuild that read before any later
  refresh start or mark-stale cannot know whether its snapshot includes that commit, so it
  discards.
- The **per-entity window** guards single-entity refreshes. Only a newer refresh of the
  same entity discards an older one. A commit on another entity says nothing about this
  read, so it never discards it. If one global counter guarded refreshes too, overlapping
  mutations on different entities would keep discarding each other and drive the cache
  stale.

Why the current-holder check: an entity's cached value can lag behind the index while a
discarded refresh is in flight. Without the check, re-indexing X could delete an index
entry that a later install of Y wrote.

### 6.4 Prime and rebuild

```
Prime(ctx):
    lock; generation++; captured = generation; unlock
    return rebuild(WithTimeout(ctx, primeBudget), captured)

rebuild(ctx, captured) -> (RebuildResult, error):
    all, err = Source.LoadAll(ctx)           # fence, then consistent read; off-lock
    lock
      if err: markStaleLocked(err); return Failed, err
      if generation != captured: return Discarded, nil     # newer state landed mid-read
      build new entities, indexes, directory from all
        on capacity, duplicate primary key, duplicate unique key, or non-cacheable entity:
            markStaleLocked; return Failed, err             # SAFE-8, SAFE-12
      # caller callbacks (Key, Cacheable, index keys) run here; see panic rule in 6.6
      swap them in
      for window in refreshes: window.gen++  # open refreshes began before capture
      stale = false
    unlock
    return Installed, nil
```

Notes:

- `Discarded` leaves the cache stale and returns no error. Callers MUST branch on the
  result, never on a nil error alone. `Prime` returning anything but `Installed` must fail
  startup.
- Bumping every open refresh window at install makes any refresh that read before the
  rebuild's capture discard itself, because the install already contains its commit.
- The new maps are built aside and swapped in, so a failed rebuild never leaves a
  half-cleared cache. (The cache is stale on failure regardless, so this is defense in
  depth.)

### 6.5 Background rebuild loop

```
Run(ctx):
    every rebuildInterval (injected scheduler):
        tickCtx = TickContext(ctx)           # caller hook, e.g. a fresh trace id per tick
        if not stale: continue
        lock; generation++; captured = generation; unlock
        result, err = rebuild(WithTimeout(tickCtx, rebuildBudget), captured)
        Failed, first in this stale episode -> Error log "surecache_rebuild_failed"
        Installed                           -> Info log  "surecache_recovered"
        Discarded                           -> no log; retry next tick
```

`Run` returns when `ctx` ends, and the owner waits for it. A panic in the store or in a
caller callback does not escape `Run`: the locked region marks the cache stale and
releases its lock during unwinding (section 6.6), `Run` recovers outside it, reports
after unlocking, and continues on the next tick.

`Run` also drives the outage policy's health probe (section 8) on its own schedule, so a
rebuild blocked on a slow store never delays health detection.

### 6.6 Locking rules

- One lock guards all state. The library's own bounded maps are unsynchronized and used
  only under that lock, so reads take exactly one lock.
- No store call, log call or observer callback under the lock. Transitions are recorded
  under the lock and emitted after release.
- **Panic rule.** Every locked region that can modify cache state installs a deferred
  handler that, on panic, marks the cache stale while still holding the lock, then
  unlocks and lets the panic continue. Readers therefore never see half-modified maps
  without the stale flag, and a panic in a caller callback (`Key`, `Clone`, `Cacheable`,
  index key functions) can never leave the lock held. Logging and observer reporting
  happen after unlocking; `Run` and `guard` recover outside the region.
- Entities are cloned on the way out (SAFE-10). Entities from the store are assumed to be
  detached copies; the library also clones on install when `WithCloneOnInstall` is set.

## 7. Failure direction

How the cache reacts to each kind of store failure, and which way that pushes safety.

| Event | On error | On timeout | Resulting state | Why |
| --- | --- | --- | --- | --- |
| Read, trusted cache | n/a | n/a | answered from memory | Exact for committed state under PRE-1 and the fence. |
| Read, deferred | caller's store call fails | same | caller handles | The cache never converts a store failure into an answer. |
| Mutation returns success, refresh fails | mark stale | mark stale (refresh budget) | stale | Never serve state the store may have moved past. |
| Mutation returns a nothing-committed error | no change | n/a | unchanged | Contract says the cached copy is exact. |
| Mutation returns anything else | mark stale at once | mark stale at once | stale | Commit may have landed or may still land. |
| Mutation's shared fence acquire fails | mutation returns the error; not on the allowlist, so mark stale | same | stale | Likely nothing written, but the cache cannot prove it. |
| Mutation on a stale cache | no refresh, no rebuild | n/a | stale | One entity cannot restore trust; writes must not wait on the fence. |
| Rebuild loop bulk load | stay stale, Error once per episode | stay stale | stale | Retry next tick. |
| Prime bulk load | return Failed | return Failed (prime budget) | stale; caller fails startup | Never serve an unprimed cache. |
| Exclusive fence acquire | rebuild fails | lock timeout | stale | Trust only after the fence is held. |

Callers on a safety-critical path should classify each read the same way: a read that
establishes the set of targets to act on may abort when deferred and failing; a read that
enriches one target should degrade that target toward its safe state, never suppress the
action for the rest.

## 8. Outage policy

A trusted cache can keep answering while the store is down, because nothing has changed
since it became trusted. Whether that is desirable is a property of the caller's domain,
not of the cache, so the library requires an explicit choice. There is no default.

- **`ServeWhileExact`.** The cache stays trusted through an outage until the first
  mutation starts. From that moment, reads that mutation affects defer (and the directory
  defers), so callers see store errors. When the mutation fails ambiguously, the cache
  goes stale and stays stale until a rebuild succeeds after the store returns. Use this
  when continuing to act on known-good data during an outage is the safe direction.
- **`DistrustAfterProbeFailures(HealthPolicy)`.** A health probe runs on its own
  schedule. After enough consecutive failures the cache sets `unhealthy`, so callers see
  the outage within a bounded time even when nobody writes. Use this when acting on data
  the system cannot currently confirm is the unsafe direction (for example, when
  downstream devices have their own fail-safe that should take over).

```go
type HealthPolicy struct {
    Probe       func(ctx context.Context) error // e.g. a short round trip to the writer
    Interval    time.Duration                   // probe schedule, independent of rebuilds
    Timeout     time.Duration                   // per-probe bound, < Interval
    Unavailable func(error) bool                // which probe errors count as failures
    FailAfter   int                             // consecutive failures that set unhealthy
    RecoverAfter int                            // consecutive successes that clear it
}
```

Rules:

- **Two gates.** `unhealthy` is separate from `stale`. A successful rebuild clears only
  `stale`; only `RecoverAfter` consecutive probe successes clear `unhealthy`. Reads need
  both clear. This stops a rebuild from resuming cached answers while the probe still
  reports the store as unavailable.
- **Counting.** A success resets the failure count and a failure resets the success
  count. Only errors for which `Unavailable` returns true count as failures; a probe that
  exceeds `Timeout` counts as a failure.
- **Bound.** Detection takes at most about `FailAfter x Interval + Timeout`, provided the
  probe call honors its timeout (section 12).
- **Deferred reads.** The caller may feed outcomes it observes on deferred reads through
  `ObserveStoreResult(err)`. Only errors for which `Unavailable` returns true count, and
  they count toward `FailAfter` exactly like probe failures. Business errors such as
  not-found never count.

Consequences to document for users of `ServeWhileExact`:

- Any action whose prerequisite read is served by the trusted cache still proceeds during
  an outage, even if a later store write in the same flow then fails.
- Once the cache is stale, actions whose prerequisite reads defer will fail those reads
  and not proceed. If those actions are safety actions, the domain needs an independent
  fallback that does not depend on the store.
- Nothing retries a failed write. Callers that need retries must drive them, typically
  from a periodic sweep whose next successful read retries the action.

## 9. Observability

The core package exposes an `Observer` interface; `nil` means no observation. An optional
`surecache/promobs` subpackage adapts it to Prometheus with pre-registered collectors and
closed label sets, so idle scrapes read zeros rather than absent series.

| Signal | Kind | Labels | Meaning |
| --- | --- | --- | --- |
| `surecache_stale` | gauge | `cache` | 1 while stale, 0 otherwise |
| `surecache_unhealthy` | gauge | `cache` | 1 while the health policy distrusts the store, 0 otherwise |
| `surecache_entities` | gauge | `cache` | entities held |
| `surecache_reads_total` | counter | `cache`, `method` (`get`, `lookup`, `list`), `source` (`cache`, `stale`, `pending`, `in_flight`, `miss`) | who answered, or why the read deferred |
| `surecache_rebuilds_total` | counter | `cache`, `trigger` (`prime`, `loop`), `result` (`installed`, `discarded`, `failed`) | whole rebuilds |
| `surecache_ambiguous_mutations_total` | counter | `cache` | mutations whose outcome was unknown |
| `surecache_store_probe_failures_total` | counter | `cache` | health probe failures (outage policy) |

Logs go through an injected `*slog.Logger`, with the context passed so handlers can attach
trace ids.

| Message | Level | Trigger | Edge rule |
| --- | --- | --- | --- |
| `surecache_stale` | Warn | entering stale, with the cause | once per transition |
| `surecache_rebuild_failed` | Error | first failed loop rebuild in a stale episode | once per episode |
| `surecache_recovered` | Info | a rebuild installs after a stale episode | once per episode |

Log messages are static strings. Attributes carry the cache name, the cause and counts,
never entity contents.

## 10. Public API (proposed)

### 10.1 What the library owns and what the caller owns

| Library owns | Caller owns |
| --- | --- |
| trust state, generation, refresh windows, pending and in-flight marks | the store, its transactions and its fence |
| deferral rules for primary, index and directory reads | the nothing-committed error classifier |
| current-holder removal for every index | which operations are mutations and which ids they touch |
| bounded maps with refuse-on-overflow | capacity values |
| rebuild install, discard, capacity and contract checks | the bulk loader and its consistency guarantee |
| the background rebuild loop and outage policy mechanics | the outage policy choice and the budgets |
| observer events and edge logs | the observer, logger and metric registry |

### 10.2 Interfaces

```go
package surecache

// Spec describes the cached entity. Every field is required; Build rejects a missing one.
type Spec[ID comparable, E any] struct {
    Name      string             // used in logs and metric labels
    Key       func(E) ID         // primary key
    Clone     func(E) E          // deep copy; every served value is a clone
    Cacheable func(E) bool       // false: not held; must yield no index keys (PRE-3)
    Compare   func(a, b ID) int  // total order for List
    Capacity  int                // primary-map cap; refuse on overflow, never evict
    // CommittedNothing reports whether a mutation error guarantees no change committed.
    CommittedNothing func(error) bool
}

// Source is the store side the cache needs; both reads go to the authoritative writer.
type Source[ID comparable, E any] interface {
    // LoadOne returns a consistent snapshot of id, even when it is not Cacheable.
    LoadOne(ctx context.Context, id ID) (E, error)
    // NotFound reports whether a LoadOne error means the entity authoritatively does not exist.
    NotFound(err error) bool
    // LoadAll returns every Cacheable entity once, from one snapshot ordered after every
    // earlier mutation (the fence), or an over-capacity error; never a subset.
    LoadAll(ctx context.Context) ([]E, error)
}

// Index is a typed handle to a secondary index, valid only for the cache that built it.
type Index[ID comparable, K comparable] struct{ /* opaque */ }

// Builder registers secondary indexes before the cache is built, so the set is closed.
type Builder[ID comparable, E any] struct{ /* ... */ }

func NewBuilder[ID comparable, E any](spec Spec[ID, E], src Source[ID, E]) *Builder[ID, E]

// AddIndex registers a unique index (PRE-2); keys returns every key an entity holds.
func AddIndex[ID comparable, E any, K comparable](b *Builder[ID, E], name string,
    keys func(E) []K, capacity int) Index[ID, K]

// Build requires an OutagePolicy option; there is no default (section 8).
func (b *Builder[ID, E]) Build(opts ...Option) (*Cache[ID, E], error)

// Answer is a tri-state lookup result. Defer means "ask the store", never "absent".
type Answer uint8

const (
    Defer Answer = iota
    Found
    Absent // authoritative negative: trusted, no mutation in flight
)

// RebuildResult tells a caller whether trust was actually restored.
type RebuildResult uint8

const (
    Installed RebuildResult = iota
    Discarded // newer state committed mid-read; still stale
    Failed
)

type Cache[ID comparable, E any] struct{ /* section 5.1 */ }

// Prime runs the first rebuild; anything but Installed must fail startup.
func (c *Cache[ID, E]) Prime(ctx context.Context) (RebuildResult, error)

// Get returns a clone and Found, or Defer. It never returns Absent (SAFE-3).
func (c *Cache[ID, E]) Get(id ID) (E, Answer)

// Lookup answers a secondary-key query. A handle from another cache returns Defer and
// an error; it never answers Absent.
func Lookup[ID comparable, E any, K comparable](c *Cache[ID, E], idx Index[ID, K],
    key K) (ID, Answer, error)

// List returns a detached, Compare-sorted copy of the directory and Found, or Defer.
func (c *Cache[ID, E]) List() ([]ID, Answer)

// Mutate runs op with ids marked pending, settles the cache on op's error, and returns
// that error unchanged. ids must name every entity op can change.
func (c *Cache[ID, E]) Mutate(ctx context.Context, ids []ID, op func() error) error

// Create is for inserting one previously nonexistent entity only (never an upsert). It
// runs op under the in-flight count, refreshes the returned id on success, and returns
// op's results unchanged.
func (c *Cache[ID, E]) Create(ctx context.Context, op func() (ID, error)) (ID, error)

// ObserveStoreResult feeds a deferred read's outcome to the health policy, if one is set.
func (c *Cache[ID, E]) ObserveStoreResult(err error)

// Run is the single background rebuilder and health prober; it returns when ctx ends.
func (c *Cache[ID, E]) Run(ctx context.Context) error
```

Options: `WithOutagePolicy` (required), `WithObserver`, `WithLogger`, `WithClock`
(scheduler and timers, for tests), `WithTickContext` (per-tick context hook),
`WithRefreshBudget`, `WithRebuildBudget`, `WithRebuildInterval`, `WithPrimeBudget`,
`WithCloneOnInstall`.

Design notes:

- **Generics.** Go methods cannot take their own type parameters, so `AddIndex` and
  `Lookup` are package-level functions. The typed `Index[ID, K]` handle makes a wrong key
  type a compile error rather than a runtime miss that could read as "absent".
- **Closed index set.** Indexes are fixed at `Build`, so rebuild and refresh always
  maintain all of them, and the current-holder rule applies to every index.
- **Injected `Key` and `Clone`.** A method constraint whose `Clone` returns an interface
  would not fit ordinary value types. Functions avoid that.
- **Cacheable.** Lets one store hold rows the cache deliberately excludes (soft-deleted,
  archived, empty) without a separate table.

### 10.3 Wrapping a repository: the decorator pattern

The intended integration is a decorator that implements the application's own repository
interface, delegates to the real store, and routes through the cache:

```go
type CachedRepo struct {
    store Repo                      // the real store adapter
    cache *surecache.Cache[GroupID, Group]
    byMember surecache.Index[GroupID, MemberID]
}

var _ Repo = (*CachedRepo)(nil)

func (r *CachedRepo) GroupForMember(ctx context.Context, m MemberID) (GroupID, bool, error) {
    id, answer, err := surecache.Lookup(r.cache, r.byMember, m)
    switch {
    case err != nil || answer == surecache.Defer:
        return r.store.GroupForMember(ctx, m)
    case answer == surecache.Absent:
        return "", false, nil
    }
    return id, true, nil
}

func (r *CachedRepo) AddMember(ctx context.Context, g GroupID, m MemberID) error {
    return r.cache.Mutate(ctx, []GroupID{g}, func() error { return r.store.AddMember(ctx, g, m) })
}
```

Rules for the decorator:

- **Delegate every method explicitly; never embed the store.** With embedding, a method
  added to the repository later passes straight through, and if it writes, the cache
  misses its invalidation silently. With explicit delegation, the new method fails to
  compile until someone classifies it as a read or a mutation (PRE-4).
- **Pass every affected id to `Mutate`.** A move between two groups names both.
- **Map `Defer` to a store call, always.** Never to an empty result.

### 10.4 Internal invariants (implementation checklist)

1. No store call, log or observer callback under the lock.
2. `generation++` on every refresh start, mark-stale and rebuild capture.
3. `inFlight++` exactly once per mutation, creates included; pending and in-flight marks
   released only after settle completes, including on panic (which marks stale first).
   Every refresh batch marks its ids pending while its windows are open, so an entity
   whose older refresh was discarded defers until the newer batch installs.
4. Every index removal compares the current holder before deleting, and every index
   insertion that would overwrite another entity's entry is allowed only while that
   entity has a refresh in progress; otherwise the cache goes stale with the maps
   untouched.
5. Rebuild builds aside and installs whole, or not at all; refresh windows bumped on
   install.
6. Stale is sticky until a rebuild returns `Installed`; nothing else clears it.
7. A mutation on a stale cache never reads the store for the cache's sake.
8. A refresh installs only a cacheable entity; a non-cacheable result is removed; a
   non-cacheable result with index keys marks stale.
9. `Absent` and directory answers only while trusted with no mutation in flight; primary
   misses always defer.
10. A refresh batch opens every window before its first read.
11. A panic inside a modifying locked region marks stale before the lock is released.

## 11. Verification

### 11.1 Library unit tests (hermetic, `-race`, virtual time)

- Every read source (cache, stale, pending, in-flight, miss) for `Get`, `Lookup`, `List`.
- Each nothing-committed error leaves the cache unchanged; each other error, including
  context errors after the store call, marks stale and counts ambiguous.
- Refresh failure and refresh timeout mark stale; the mutation still returns success.
- A mutation on a stale cache issues no store read.
- A panicking store call marks stale once and releases all marks.
- Deterministic interleavings, driven by test hooks between the store call and install:
  - an older same-entity refresh is discarded by a newer one;
  - refreshes of different entities do not discard each other;
  - a rebuild is discarded by a commit that lands mid-read;
  - a lagging value does not delete an index entry another entity now holds;
  - a stalled refresh installs after another entity took the same unique key; the index
    entry stays on the new holder, or the cache goes stale;
  - a mutation moves key K from X to Y while an older refresh of X is stalled; K ends on
    Y, never unindexed;
  - a panicking callback inside a locked region leaves the cache stale and the lock free.
- Capacity: prime over the cap fails; runtime overflow marks stale; no subset installed.
- Contract violations: duplicate unique key in a bulk load; non-cacheable entity with
  index keys. Both fail safe.
- Budgets and the rebuild interval, under `testing/synctest`.
- Outage policies: `ServeWhileExact` stays trusted with a dead store until the first
  mutation; `DistrustAfterProbeFailures` sets `unhealthy` after exactly `FailAfter`
  failures, a rebuild does not clear it, and only `RecoverAfter` successes do.
- Concurrency: N parallel readers and mutators; invariants hold; race detector clean.
- Clone isolation: mutating a returned value never changes the cache.
- Mutation testing on the core package with a floor of at least 0.90
  (`killed / (killed + survived)`).

### 11.2 Store conformance suite (`surecachetest`)

Exported so every store adapter can run it:

- each declared nothing-committed error leaves the store unchanged;
- `LoadOne` returns every commit completed before the call, a consistent multi-table
  snapshot, and `Key(entity) == id`; its not-found means the entity does not exist;
- `LoadAll` returns each primary key exactly once;
- predicate and index parity: no non-cacheable fixture row yields an index key;
- `LoadAll` returns exactly the cacheable set, in parity with `Spec.Cacheable`;
- `LoadAll` refuses over capacity with no entities;
- context errors are returned unwrapped;
- **fence ordering:** a mutation transaction held open blocks `LoadAll`, which then
  returns the committed change, or the unchanged state after a rollback;
- the fence wait times out within its bound.

### 11.3 Integration recipes (documented, run by adopters)

- **Reversible outage:** route the store connection through an in-test TCP proxy that
  resets connections while down. Prime, take the proxy down, assert the configured outage
  policy, bring it back, assert recovery through the loop. (A silent network pause is not
  used, because many drivers cannot bound one.)
- **Restart:** a fresh cache primes over a store holding every entity shape, including
  ones that are cacheable but have no index keys.

## 12. Residual risks and limits

- **Budgets are not hard deadlines.** Some drivers do not watch the context at every step
  (for example, sending BEGIN before checking it, or cancelling through a second
  connection). A peer that still acknowledges TCP but stops answering can hold a call past
  a budget. Server-side timeouts bound waits on the server, and keepalive bounds a dead
  network only after its probes. Prefer drivers that honor the context throughout.
- **The single-writer precondition is unchecked.** Out-of-band writes can leave a trusted
  cache wrong indefinitely. Stop the process before administrative writes and restart it
  afterwards.
- **Capacity is enforced, not structural.** If the cacheable set can grow without bound in
  the domain, the cache will eventually refuse and stay stale. Size the cap to the domain,
  and give operators a documented way to shrink the cacheable set.
- **Lock contention is unmeasured.** A copy-on-write snapshot behind `atomic.Pointer`
  would give lock-free reads, at the cost of reworking the pending, generation and window
  logic. Consider it only if benchmarks show contention.

## 13. Alternatives considered and rejected

| Alternative | Why rejected |
| --- | --- |
| TTL or LRU caches (ristretto, otter, theine, golang-lru, bigcache, freecache) | Evict or drop on their own schedule (SAFE-8). Solve none of the ordering rules. |
| Read-through with request coalescing (sturdyc, groupcache, singleflight) | A coalesced load can return pre-mutation state; stale-on-error is the unsafe direction; groupcache has no invalidation. |
| Cache tables inside the database (unlogged tables, SQL-backed cache stores) | Every lookup becomes a store call, defeating NFR-2 and outage tolerance; usually TTL-based. |
| LISTEN/NOTIFY invalidation | Delivered after commit, so in-flight deferral is still needed; no guarantee across reconnect, so a fenced rebuild is still needed; adds a dedicated connection and reconnect logic and removes none. |
| Logical replication change stream | Slots, snapshot handoff, position tracking and lag management; no benefit under a single writer. |
| Two stacked caches over one store | Duplicate invalidation bookkeeping; the two views can disagree mid-mutation. |
| Refresh one entity after an ambiguous error | Can read before an abandoned COMMIT lands (SAFE-5). |
| Row locks (`SELECT ... FOR UPDATE`) as the fence | Cannot fence an ambiguous create whose id is unknown. |
| Shared network cache (Redis and similar) | A new network dependency and failure mode for a single-process deployment. |
| Write-behind | Loses durable state on a crash. |
