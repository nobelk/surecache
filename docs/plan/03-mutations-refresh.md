# Milestone 03: Mutations and post-commit refresh

## Goal

Implement `Mutate`, `Create`, the `guard` with pending and in-flight marks, outcome
settlement, the post-commit refresh with per-entity windows, the index current-holder and
conflict rules, and the panic rule on every modifying region. Add test hooks and fault
injection, and write the deterministic interleaving tests for the refresh path.

## Prerequisites

Milestone 02. Read design sections 3.1 (SAFE-1, -2, -4, -5, -6, -7, -11, -12), 3.2 (FUN-4,
-6, -7, -8), 3.5 (PRE-3, PRE-4), 6.2, 6.3, 6.6 and 10.4. Read [README.md](README.md)
decisions D17, D18 and clarifications 2, 5 and 6.

## Requirements covered

SAFE-1, SAFE-2, SAFE-4, SAFE-5 (no single-entity recovery), SAFE-6 (same-entity
ordering), SAFE-7, SAFE-11, SAFE-12 (refresh side), FUN-4, FUN-6, FUN-7, FUN-8, NFR-1
(marks and windows), internal invariants 2, 3, 4, 7, 8, 10, 11.

## Deliverables

```
mutate.go       guard, Mutate, Create, settle
refresh.go      refreshBatch, refreshOne, beginRefreshLocked, endRefresh,
                removeLocked, conflictLocked, installLocked
cache.go        markStale / markStaleLocked (bumps generation), cause wrapping
hooks.go        hooks struct and call sites
internal/memstore/   fault injection
*_test.go
```

## Design details

### guard, Mutate, Create

As section 6.2:

- `Mutate(ctx, ids, op)`: `len(ids) == 0` returns `ErrNoIDs` without running `op`.
  Duplicates are removed (`slices.Sort` is not possible on a `comparable` ID, so dedupe
  with a small map for more than 8 ids and a linear scan below that).
- `guard` increments `pending[id]` for each id and `inFlight` once, under one write lock.
- The deferred cleanup takes the lock, marks stale with `ErrUnsettled` if `settled` is
  false, decrements marks (deleting at zero), decrements `inFlight`, unlocks, then emits
  buffered events. If `op` panicked, the panic continues after cleanup (clarification 5).
- `Create(ctx, op)`: no pending ids; on success refreshes `[id]`.
- `settle`: success runs `refreshBatch` under
  `context.WithTimeout(context.WithoutCancel(ctx), cfg.refreshBudget)`; a nothing-committed
  error does nothing; any other error emits `AmbiguousMutation` and marks stale with an
  error that wraps both `ErrOutcomeUnknown` and the op error (`fmt.Errorf("%w: %w", ...)`).
- The caller always receives `op`'s own error unchanged (FUN-8), including when it wraps a
  context error.

### Refresh

As section 6.3, exactly:

1. Under the lock: `generation++`; if stale, return (FUN-4, clarification 6); otherwise
   open every window of the batch (`refreshes[id].gen++`, `.active++`) and capture each
   `gen`.
2. Deferred: `endRefresh` for every id (`active--`, delete at zero).
3. For each id, `refreshOne`: `LoadOne` off the lock. `NotFound` means delete. Any other
   error marks stale. `Key(e) != id` marks stale with `ErrContract`.
4. Under the lock with the panic rule: discard if the window moved. Non-cacheable with
   index keys marks stale (`ErrContract`). For a cacheable entity, run the conflict check
   for every key of every index **before** any change: a key held by another entity whose
   window has no active refresh marks stale with `ErrKeyConflict` and leaves the maps
   untouched. Then `removeLocked(id)` (current-holder check per key), then
   `installLocked(id, e)`: index keys first, then the entity, then the directory (binary
   insert with `slices.BinarySearchFunc` and `Compare`). A capacity refusal marks stale
   with `ErrOverCapacity`.

Capacity pre-check: before modifying anything, compute the **projected occupancy** of the
primary map and of each index after the whole refresh, and refuse only if a projection
exceeds its capacity. For each index, projected = current length − (keys of the old value
that `removeLocked` will actually delete, that is, keys still naming `id`) + (distinct
incoming keys not already present after that removal). Incoming keys are deduplicated
first. The primary map grows by one only when `id` is new. So replacing key A with key B
in an index at capacity succeeds, and only true growth past capacity is refused. A refusal
marks stale with `ErrOverCapacity` and leaves the maps untouched. The install then cannot
fail halfway.

### markStale

`markStaleLocked(cause)` bumps `generation` on every call. It sets `stale` and records a
`StaleChanged(true, cause)` event only on the transition. Logging is milestone 06; this
milestone records the event only.

### Test hooks (D17)

```go
type hooks struct {
    afterLoadOne   func(id any)   // between LoadOne and the install lock
    beforeRefresh  func(ids any)  // after windows open, before the first LoadOne
    afterLoadAll   func()         // between LoadAll and the install lock (used in 04)
    afterOp        func()         // between op returning and settle
    afterMark      func(ids any)  // in guard, after marks are set and the lock released
    beforeRelease  func(ids any)  // in guard cleanup, before taking the lock to release marks
}
```

`afterMark` and `beforeRelease` bracket the interval in which SAFE-2 requires deferral.
The model checker in milestone 07 uses them to timestamp mutation windows.

`Cache.hooks` is a `*hooks`, nil in production. Call sites are `if h := c.hooks; h != nil
&& h.afterLoadOne != nil { ... }`. Hooks use `any` for ids so the struct is not generic;
tests type-assert. No hook exists on the read path.

### memstore fault injection

Per-call scripted outcomes, chosen by a test: return a nothing-committed error; return an
ambiguous error with the change committed; return an ambiguous error with the change not
committed; return an ambiguous error and commit later while still holding the shared fence
(an abandoned COMMIT, released by the test); return a context error after committing;
panic; block until released. The same for `LoadOne` and `LoadAll` failures. Every injected
outcome is deterministic. No randomness lives in memstore; the model test in 07 drives it.

## Tasks (test-first order)

1. Marks and counting: `TestMutate_NoIDs`, `TestMutate_DedupesIDs`,
   `TestCreate_InFlightCountedOnce`, `TestPendingMarks_DroppedAtZero`,
   `TestRefreshWindows_DroppedAtZero`.
2. Outcomes: `TestMutate_ReturnsOpErrorUnchanged`, `TestMutate_NothingCommittedLeavesCache`
   (one subtest per memstore nothing-committed error), `TestMutate_AmbiguousErrorMarksStale`,
   `TestMutate_ContextErrorAfterStoreMarksStale` (`context.Canceled` and
   `context.DeadlineExceeded`), `TestMutate_AmbiguousCountsObserved`.
3. Refresh: `TestMutate_ReadAfterSuccessSeesNewState`,
   `TestMutate_PendingHeldUntilRefreshInstalled` (hook blocks in `afterLoadOne`; a
   concurrent `Get` defers), `TestCreate_LookupMissDefersDuringCreate`,
   `TestRefresh_DeletedEntityRemoved`, `TestRefresh_NonCacheableRemoved`,
   `TestRefresh_NonCacheableWithKeysMarksStale`, `TestRefresh_KeyMismatchMarksStale`,
   `TestRefresh_LoadErrorMarksStale`, `TestMutate_RefreshFailureNotReported`.
4. Context handling under `synctest`: `TestRefresh_DetachedFromCallerCancel` (cancel the
   caller ctx after `op` returns; refresh still installs), `TestRefresh_KeepsContextValues`,
   `TestRefresh_TimeoutMarksStale` (`LoadOne` blocks past the refresh budget).
5. Stale path: `TestMutate_StaleCacheIssuesNoStoreRead` (memstore `LoadOne` count stays 0),
   `TestMutate_StaleStillBumpsGeneration`.
6. Panics: `TestMutate_PanicMarksStaleAndReleasesMarks` (panic in `op`; caller sees the
   same panic value; `pending` and `inFlight` are zero; cache stale),
   `TestLockedRegion_CallbackPanicLeavesStaleAndUnlocked` (panic in `Cacheable` and index
   `keys` during refresh install).
7. Interleavings (section 11.1), each fully deterministic with hooks and channels:
   - `TestRefresh_OlderSameEntityDiscarded`;
   - `TestRefresh_DifferentEntitiesDoNotDiscard`;
   - `TestRefresh_LaggingValueKeepsNewHolder` (SAFE-7);
   - `TestRefresh_RemovalKeepsOtherHolder`;
   - `TestRefresh_StalledInstallAfterKeyTaken`: the entry stays on the new holder, or the
     cache goes stale; assert one of the two and never "entry on old holder while trusted";
   - `TestRefresh_MoveKeyBetweenEntities`: mutation moves K from X to Y while an older
     refresh of X is stalled; K ends on Y;
   - `TestRefresh_BatchOpensAllWindowsFirst`: the variant of the previous test that fails
     if windows open lazily (verify by temporarily breaking the code during development);
   - `TestRefresh_ConflictMarksStaleMapsUntouched`.
8. `TestGeneration_BumpedOnRefreshStartMarkStaleCapture` (refresh start and mark-stale
   parts; the capture part lands in 04).
9. Capacity on the refresh path (SAFE-8, NFR-1): `TestRefresh_OverflowMarksStale` with
   subtests for the primary map and for each index. Each asserts the mutation still
   returns success, the cache is stale, all marks are released and the maps are
   untouched. `TestIndexCapacity_ReplaceAtCapacitySucceeds` (index at capacity 1, entity
   moves from key A to key B, cache stays trusted) and
   `TestIndexCapacity_GrowthPastCapacityRefused`.
10. `TestMutate_OpRunsBeforeReturn` (no write-behind): the store shows the change at the
    moment `Mutate` returns success.
11. `TestStoreCalls_NeverUnderLock`: the memstore lock probe (milestone 02) is enabled
    in every deterministic single-user test of this milestone, that is, tests where the
    goroutine calling the store is the only cache user at that moment. It is never
    enabled in concurrent tests, where an unrelated reader holding `RLock` would make
    `TryLock` fail for a correct implementation.

## Acceptance

- All tests pass with `-race -count=50` for the interleaving tests (they must be
  deterministic, so 50 runs is a smoke test for accidental timing dependence).
- `mutate.go` and `refresh.go` at 100% statement coverage.
- Each of section 10.4 invariants 2, 3, 4, 7, 8, 10 and 11 is named in at least one test
  comment.
