# Milestone 02: Core types, state and reads

## Goal

Define the public API surface from section 10.2, the internal state from section 5.1, the
bounded map, the build-aside bulk install, and the three read operations with their
deferral rules. At the end of this milestone a cache can be installed from a slice of
entities (through an internal test helper) and answers reads correctly.

## Prerequisites

Milestone 01. Read design sections 2, 3.1 (SAFE-2, -3, -8, -10, -12), 3.2 (FUN-1, -2), 5,
6.1, 6.6 and 10.2. Read [README.md](README.md) decisions D9, D18, D25, D26 and
clarifications 7 and 9.

## Requirements covered

SAFE-2 (read side), SAFE-3, SAFE-8 (bulk install), SAFE-10, SAFE-12 (bulk install),
FUN-1, FUN-2, NFR-1 (maps), NFR-3 (shape; asserted in 07), internal invariants 9 and 11
(install region).

## Deliverables

```
surecache.go          Spec, Source, Answer, RebuildResult (+ String methods)
errors.go             all sentinel errors from D26
options.go            Option, With* functions, defaults (D25)
observer.go           Observer, NopObserver, Method, ReadSource, Trigger enums, event buffer
builder.go            Builder, NewBuilder, AddIndex, Build, validation
index.go              Index[ID, K] handle; internal index[ID, E, K]; anyIndex[ID, E]
cache.go              Cache struct and fields from section 5.1; markStaleLocked; withWriteLock
rebuild.go            buildAside (entities -> new maps, contract checks), swapInLocked
read.go               Get, Lookup, List
internal/boundedmap/  Map[K comparable, V any]
internal/memstore/    in-memory Source with fence (baseline; fault injection grows in 03)
*_test.go
```

## Design details

### Public types

As section 10.2, with these specifics:

- `Answer` and `RebuildResult` have `String()` methods. `Defer` is the zero value of
  `Answer`, so a zero `Answer` never reads as `Found` or `Absent`.
- `Spec` validation in `Build`: `Name` non-empty; `Key`, `Clone`, `Cacheable`, `Compare`,
  `CommittedNothing` non-nil; `Capacity > 0`. `NewBuilder` with a nil `Source` records an
  error.
- `AddIndex(b, name, keys, capacity)`: `name` non-empty and unique within the builder,
  `keys` non-nil, `capacity > 0`. Errors are recorded on the builder and returned by
  `Build` through `errors.Join`. Calling `AddIndex` on a built builder panics
  (clarification 9). `Build` on a built builder returns an error.
- `Build` requires `WithOutagePolicy` (`ErrNoOutagePolicy` otherwise). The outage policy
  type is declared here as a sealed interface with an unexported method; its two
  constructors arrive in milestone 05. For tests in this milestone use an internal
  `testPolicy`.

### Options

`WithOutagePolicy`, `WithObserver`, `WithLogger`, `WithTickContext`, `WithRefreshBudget`,
`WithRebuildBudget`, `WithRebuildInterval`, `WithPrimeBudget`, `WithCloneOnInstall`.
Defaults: prime 10 s, rebuild 4 s, refresh 2 s, interval 1 s, logger
`slog.New(slog.DiscardHandler)`, observer `NopObserver{}`, tick context identity.
Non-positive durations are `Build` errors. Options are applied to a private `config`
struct; `Build` validates the result.

### Index storage

`Index[ID, K]` is a small value holding a pointer to the owning builder's identity token
and the index position. The cache stores indexes as `[]anyIndex[ID, E]`. `anyIndex` has
the unexported methods the refresh and rebuild paths need (`keysOf`, `holder`, `put`,
`deleteIfHolder`, `fresh`, `len`). `Lookup` checks the token (`ErrForeignIndex` with
`Defer` on mismatch), then type-asserts `c.indexes[pos]` to `*index[ID, E, K]`. The
assertion is a cheap interface check, not reflection.

### Bounded map

`internal/boundedmap.Map[K, V]`: wraps a Go map with a capacity. `Put` of a new key at
capacity returns false and stores nothing; `Put` of an existing key always succeeds.
`Get`, `Delete`, `Len`, `All` (an `iter.Seq2`). Not safe for concurrent use. No goroutine,
no expiry.

### State and the event buffer

`Cache` holds the fields of section 5.1 plus `cfg`, `indexes`, `observer`, `logger`,
`primed` and `running` (used from milestone 04). Events raised while holding the lock are
appended to a short per-call buffer, a small array value on the stack (for example
`[4]event`), and emitted after unlock. Reads emit exactly one `Read(method, source)` call
after `RUnlock`.

**Ordered delivery of state events.** Off-lock delivery can reorder events from two
callers: a delayed `StaleChanged(false)` could arrive after a newer `StaleChanged(true)`
and leave a gauge permanently wrong. Events therefore come in two classes:

- **Counter events** (`Read`, `Rebuild`, `AmbiguousMutation`, `ProbeFailure`) are
  order-insensitive and are delivered directly after unlock.
- **State events** (`StaleChanged`, `UnhealthyChanged`, `Entities`) and their log
  records are appended, while `c.mu` is held, to a FIFO queue owned by the cache. The
  queue order is therefore the order in which the transitions happened. After unlock, the
  caller drains the queue under a separate `emitMu`:

  ```
  drain():
      emitMu.Lock()
      loop:
          c.mu.Lock(); batch = c.stateQueue; c.stateQueue = nil; c.mu.Unlock()
          if batch empty: break
          deliver every event in batch, in order, to the observer and the logger
      emitMu.Unlock()
  ```

  No event is ever dropped, and every observer and log consumer sees state transitions
  in the order they happened. No callback runs under `c.mu`. The queue holds only
  transitions not yet delivered, so its length is bounded by the number of concurrent
  state-changing operations.

Consequence, documented on `Observer`: callbacks may call the cache's read methods
(`Get`, `Lookup`, `List`), which raise only counter events. Callbacks must not call
`Mutate`, `Create`, `Prime` or `Run`, because those drain state events and would deadlock
on `emitMu`.

**Initial state.** `Build` publishes the initial state through the emitter after
construction: `StaleChanged(true, nil)` and `Entities(0)`. An observer attached at `Build`
therefore reports stale before prime and after a failed prime. The initial event does not
produce a `surecache_stale` log, because startup is not a trusted-to-stale transition.

### Observer (D9)

```go
type Observer interface {
    Read(m Method, s ReadSource)
    StaleChanged(stale bool, cause error)
    UnhealthyChanged(unhealthy bool)
    Entities(n int)
    Rebuild(t Trigger, r RebuildResult)
    AmbiguousMutation()
    ProbeFailure()
}

// NopObserver implements Observer with no-ops. Implementations must embed it.
type NopObserver struct{}
```

`Method` is `MethodGet`, `MethodLookup`, `MethodList`. `ReadSource` is `SourceCache`,
`SourceStale`, `SourcePending`, `SourceInFlight`, `SourceMiss`. A miss that answers
`Absent` reports `SourceCache`, because the cache answered; `SourceMiss` is a primary-key
miss that defers. `SourceStale` covers both `stale` and `unhealthy`. `Trigger` is
`TriggerPrime`, `TriggerLoop`.

### Bulk install (build-aside)

`buildAside(all []E) (*snapshot, error)` builds new primary map, index maps and a sorted
directory without touching live state. It fails with:

- `ErrOverCapacity` if the primary map or any index refuses a key;
- `ErrContract` for a duplicate primary key, an entity for which `Cacheable` is false, or
  two entities holding one unique index key (SAFE-12, PRE-2).

Callbacks (`Key`, `Cacheable`, index `keys`, `Clone` when `WithCloneOnInstall`) run inside
this function. The function runs under the write lock in milestone 04, so it follows the
panic rule. `swapInLocked(snap)` replaces the maps. In this milestone an internal test
helper `installForTest(c, all)` calls both under the lock and clears `stale`.

### Reads

As section 6.1, with every `stale` meaning `stale || unhealthy`:

- `Get`: defer on stale, pending, or miss. A hit returns `Clone(e)` after `RUnlock`
  (entities are immutable once installed).
- `Lookup`: foreign handle returns `Defer` and `ErrForeignIndex`. A hit with a pending id
  defers; a miss with `inFlight > 0` defers; a hit answers `Found`; a miss answers
  `Absent`.
- `List`: defer on stale or `inFlight > 0`; otherwise a fresh copy of the directory (one
  allocation of exactly `len(directory)`).

### memstore (baseline)

`internal/memstore.Store[ID, E]` implements `surecache.Source` over a map guarded by its
own mutex. It has a reader-writer **fence** with context-aware acquisition (a channel-based
RW lock, since `sync.RWMutex` cannot time out). Mutations hold it shared, `LoadAll` takes
it exclusively before reading. It counts calls per method. It declares the nothing-committed
errors `ErrNotFound`, `ErrExists`, `ErrPrecondition` and a `CommittedNothing` classifier.
It accepts an optional lock probe `func() bool` that reports whether the cache lock is
held; every store method fails the test if the probe returns true. Milestone 03 adds fault
injection.

To detect the cache lock without exporting it, tests in `package surecache` install the
probe as `func() bool { if c.mu.TryLock() { c.mu.Unlock(); return false }; return true }`.
A failed `TryLock` shows only that *some* goroutine holds the lock, not that the
goroutine calling the store does. The probe is therefore enabled only in deterministic
tests where the store-calling goroutine is the only cache user at that moment. Concurrent
tests never enable it.

## Tasks (test-first order)

1. `boundedmap`: `TestBoundedMap_RefusesNewKeyAtCapacity`,
   `TestBoundedMap_OverwriteAtCapacitySucceeds`, `TestBoundedMap_DeleteFreesSlot`.
2. Errors, enums, `String()` methods; exhaustive-switch lint passes.
3. `Builder`/`Build` validation table test: every missing `Spec` field, bad capacities,
   duplicate index names, missing outage policy, bad durations, built-twice,
   `AddIndex`-after-build panic.
4. `buildAside`: happy path with two indexes; `TestInstall_OverCapacityRefused` (primary
   and per-index); `TestInstall_DuplicatePrimaryKeyFails`;
   `TestInstall_DuplicateUniqueKeyFails`; `TestInstall_NonCacheableFails`;
   `TestInstall_FailureLeavesLiveMapsUntouched`; `TestCloneOnInstall`.
5. Panic rule for the install region: `TestLockedRegion_CallbackPanicLeavesStaleAndUnlocked`
   with a panicking `Key`, `Cacheable` and index `keys`. After the panic, `TryLock`
   succeeds and the cache is stale.
6. Reads, one table test per method covering every `ReadSource`:
   `TestGet_TrustedHit`, `TestGet_MissAlwaysDefers`, `TestGet_DefersWhilePending` (set
   `pending` directly), `TestReads_StaleDefersAll`, `TestReads_UnhealthyDefersAll`,
   `TestLookup_TrustedHit`, `TestLookup_AbsentOnlyWhenTrustedAndIdle`,
   `TestLookup_MissDefersWhileInFlight`, `TestLookup_HitDefersWhilePending`,
   `TestLookup_ForeignHandle`, `TestList_Trusted`, `TestList_DefersWhileInFlight`,
   `TestList_ReturnsDetachedSortedCopy`.
7. `TestGet_ReturnsClone`: mutate the returned value; a second `Get` is unchanged.
8. `TestReads_NoStoreCallsWhenTrusted` and `TestCache_StaleBeforePrime` (a fresh cache
   defers everything).
9. Observer: a recording observer asserts one `Read` event per read with the right
   `ReadSource`.
10. Emitter: `TestObserver_InitialStatePublished` (after `Build`, the observer has seen
    `StaleChanged(true)` and `Entities(0)`), `TestEmitter_DeliversInTransitionOrder` (two
    goroutines raise stale then recovered transitions; the goroutine holding the later
    transition reaches `drain` first through a hook; the observer and the logger still
    see both events, in transition order), and `TestEmitter_ReadFromCallbackAllowed`.

## Acceptance

- All tests above pass with `-race`.
- Lint clean; every exported identifier documented.
- `go doc` for the package reads as the API in section 10.2 plus the D26 errors, minus
  `WithClock`.
- Coverage of `read.go` is 100%.
