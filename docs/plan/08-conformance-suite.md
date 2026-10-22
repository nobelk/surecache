# Milestone 08: Store conformance suite (`surecachetest`)

## Goal

Ship an exported, stdlib-only test suite that any store adapter runs to prove it meets the
store contract of section 4. The suite runs against `internal/memstore` in this
repository, and against the PostgreSQL reference adapter in milestone 09.

## Prerequisites

Milestone 02 (for `Spec` and `Source`). Re-check against the final API after milestone 04.
Read design sections 4 and 11.2.

## Requirements covered

Section 4.1 to 4.5 as testable properties; section 11.2 in full; PRE-2 and PRE-3 parity.

## Deliverables

```
surecachetest/doc.go
surecachetest/harness.go     Harness type the adapter fills in
surecachetest/suite.go       Run(t, h) and the individual checks
surecachetest/memstore_test.go   runs the suite against internal/memstore
```

## Design details

### Harness

The suite cannot know the adapter's schema or mutation API, so the adapter describes them.
Planned shape; finalize it while implementing, keeping it stdlib-only:

```go
type Harness[ID comparable, E any] struct {
    Spec surecache.Spec[ID, E]
    // IndexKeys lists the key functions of every index, type-erased for comparison.
    IndexKeys map[string]func(E) []any

    // NewStore returns an empty, isolated store and its Source. Cleanup via t.Cleanup.
    NewStore func(t testing.TB) Store[ID, E]
}

type Store[ID comparable, E any] interface {
    Source() surecache.Source[ID, E]
    // Seed writes fixtures directly, including non-cacheable ones. It must cover
    // every entity shape: cacheable with and without index keys, non-cacheable.
    Seed(ctx context.Context, fixtures []E) error
    Fixtures() []E
    // NothingCommittedCases returns one operation per declared nothing-committed
    // error. Each must return that error and change nothing.
    NothingCommittedCases() []Case
    // AbsentID returns an id that no fixture uses and that is never written.
    AbsentID() ID
    // Dump returns the full store state for equality checks.
    Dump(ctx context.Context) (any, error)

    // Update changes one existing entity through the adapter's real mutation path
    // (shared fence held) and returns the entity as it must read afterwards.
    Update(ctx context.Context, id ID) (want E, err error)
    // Delete removes one existing entity through the real mutation path.
    Delete(ctx context.Context, id ID) error
    // BeginMutation starts the same change as Update on id and returns before COMMIT,
    // with the shared fence held. The change must touch every component table of
    // the entity, so a mixed snapshot is detectable.
    BeginMutation(ctx context.Context, id ID) (OpenMutation, error)
    // BeginPairMutation starts one transaction that changes two distinct entities a
    // and b, and returns before COMMIT with the shared fence held. Want() returns the
    // after state of a; WantPair() of OpenPairMutation returns both.
    BeginPairMutation(ctx context.Context, a, b ID) (OpenPairMutation, error)

    // SeedOverCapacity writes Spec.Capacity+1 cacheable entities.
    SeedOverCapacity(ctx context.Context) error
    // FenceWaitBound is the adapter's exclusive-fence wait limit.
    FenceWaitBound() time.Duration
}

type OpenMutation interface {
    // Want is the entity as it must read after Commit.
    Want() E
    Commit(ctx context.Context) error
    Rollback(ctx context.Context) error
}

type OpenPairMutation interface {
    OpenMutation
    WantPair() (a, b E)
}

// SnapshotProbe is implemented by stores whose LoadOne or LoadAll runs more than one
// statement. SetReadPause installs a function the adapter calls between consecutive
// statements inside LoadOne and LoadAll (test builds only). The suite uses it to
// commit a mutation between two statements of one load.
type SnapshotProbe interface {
    SetReadPause(pause func())
}

type Case struct {
    Name string
    Op   func(ctx context.Context) error
}
```

A `Store` that does not implement `SnapshotProbe` must document that `LoadOne` and the
**entire** `LoadAll` each run exactly one statement. Only then may the snapshot subtests
`t.Skip`, with that reason. A store whose `LoadAll` runs one statement per entity or per
table must implement the probe. The
PostgreSQL reference adapter spans two tables, implements the probe, and its CI forbids
skips.

### Checks

Each check is a `t.Run` subtest with a stable name, so adapters can report and skip
precisely:

| Subtest | Property |
| --- | --- |
| `NothingCommittedLeavesStoreUnchanged/<case>` | `Dump` equal before and after; `Spec.CommittedNothing(err)` is true |
| `ContextErrorsAreNotNothingCommitted` | `Update` with an already-cancelled context, and with a context cancelled while the op waits on the fence, returns an error for which `CommittedNothing` is false |
| `LoadOneReadsLatestCommit` | after `Update` returns `want`, the next `LoadOne` equals `want` |
| `LoadOneKeyMatches` | `Key(LoadOne(id)) == id` for every fixture |
| `LoadOneNotFoundMeansAbsent` | `NotFound(err)` for `AbsentID()`, and for an id after `Delete` |
| `LoadOneConsistentSnapshot` | through `SetReadPause`, commit an open mutation between component reads; the result equals either the whole before state or `Want()`, never a mix |
| `LoadAllConsistentSnapshot` | `BeginPairMutation(a, b)`; start `LoadAll`; at the first read pause, commit the pair; the result must contain both old values or both `WantPair()` values, never one of each. Repeat with the commit at every pause point the adapter reports. Per-entity consistent snapshots that are not one whole-store snapshot fail this check. |
| `LoadOneNonCacheable` | `LoadOne` returns non-cacheable fixtures |
| `LoadAllEachKeyOnce` | no duplicate primary keys |
| `LoadAllCacheableParity` | the `LoadAll` set equals `{f in Fixtures : Spec.Cacheable(f)}` |
| `IndexParity` | no non-cacheable fixture yields an index key (PRE-3); no two cacheable fixtures share a key (PRE-2) |
| `LoadAllRefusesOverCapacity` | an error and zero entities, never a subset |
| `LoadAllContextErrorsUnwrapped` | the returned error is `==` `context.Canceled` for an already-cancelled context and for cancellation while waiting on the fence, and `==` `context.DeadlineExceeded` for an expired deadline. `errors.Is` is not enough, because it accepts wrapped errors. |
| `FenceOrdersBulkLoad/commit` | with an open mutation, `LoadAll` blocks; after commit it returns the new state |
| `FenceOrdersBulkLoad/rollback` | as above; after rollback it returns the old state |
| `FenceWaitTimesOut` | with a mutation held open past the bound, `LoadAll` fails within `FenceWaitBound + slack` |
| `MutationsDoNotBlockEachOther` | two open mutations on different entities coexist (shared fence) |

The fence checks use real time with generous slack (adapters talk to real databases).
They are the only real-time tests in the repository. Mark them clearly and keep their
total runtime under a few seconds.

## Tasks (test-first order)

1. Write the memstore harness first (`memstore_test.go`), then each check against it.
2. Add a deliberately broken memstore variant per check (for example `LoadAll` returning a
   subset over capacity, a nothing-committed error after a partial write, a two-part
   entity whose `LoadOne` reads its parts without a common snapshot, a wrapped context
   error) and assert that the check fails. For the snapshot checks, `memstore` gains a
   two-part entity mode and implements `SnapshotProbe`. Run checks through a fake `testing.TB` that records failures. This
   proves the suite catches each violation.
3. Package docs: how to write a harness, the time cost, and which subtests need a real
   isolated database.

## Acceptance

- The suite passes against `memstore` and fails against each broken variant.
- `surecachetest` imports only the standard library and `surecache`.
- The API is reviewed against milestone 09's needs before 09 starts. Adjust the harness
  here, not in 09.
