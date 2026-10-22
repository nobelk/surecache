# Milestone 04: Prime, rebuild and the background loop

## Goal

Implement `Prime`, the fenced rebuild with generation capture and discard, refresh-window
bumping on install, and the single background rebuilder `Run` with budgets and the
per-tick context hook.

## Prerequisites

Milestone 03. Read design sections 3.1 (SAFE-5, -6, -8, -9), 3.2 (FUN-3, -5), 3.4 (NFR-2,
-5, -6), 6.4, 6.5 and 7. Read [README.md](README.md) decisions D23, D24, D25 and
clarifications 3 and 4.

## Requirements covered

SAFE-5, SAFE-6 (rebuild side), SAFE-8, SAFE-9, FUN-3, FUN-5, NFR-2, NFR-5, NFR-6,
internal invariants 2 (capture), 5, 6.

## Deliverables

```
rebuild.go   Prime, rebuild, install path (uses buildAside/swapInLocked from 02)
run.go       Run, tick loop, panic recovery
*_test.go
```

## Design details

### Prime

```
Prime(ctx):
    lock
      if primed: unlock; return Failed, ErrAlreadyPrimed
      generation++; captured = generation
    unlock
    result, err = rebuild(WithTimeout(ctx, primeBudget), captured, TriggerPrime)
    if result == Installed: primed = true (under the lock, in the install region)
    return result, err
```

Concurrent `Prime` calls are not supported. Document that `Prime` is called once at
startup. Two concurrent calls are still safe: the generation check discards one of them.

### rebuild

As section 6.4. `LoadAll` runs off the lock. The `afterLoadAll` hook sits between the load
and the install lock. Under the lock with the panic rule:

1. A load error marks stale and returns `Failed` with the error.
2. `generation != captured` returns `Discarded, nil`.
3. `buildAside` failure marks stale and returns `Failed`.
4. Otherwise `swapInLocked`, bump every open refresh window, set `stale = false`, record
   `StaleChanged(false, nil)`, `Entities(n)`, `Rebuild(trigger, Installed)`.

A bulk load error is passed through unchanged. Context errors from `LoadAll` are returned
unwrapped (section 4.5), so `errors.Is(err, context.DeadlineExceeded)` works.

### Run

```
Run(ctx):
    lock
      if !primed: return ErrNotPrimed
      if running: return ErrAlreadyRunning
      running = true
    unlock
    defer: running = false
    start the health prober goroutine if the outage policy has one (milestone 05)
    ticker = time.NewTicker(rebuildInterval)
    loop until ctx.Done():
        on tick: c.tick(ctx)
    wait for the prober goroutine; return nil
```

`Run` returns `nil` when `ctx` ends normally. That is the documented contract; callers
`errgroup` it.

`tick` builds `tickCtx = cfg.tickContext(ctx)` and returns early if not stale (read under
`RLock`). Otherwise it captures the generation and calls `rebuild` with
`WithTimeout(tickCtx, rebuildBudget)`. The episode bookkeeping (`failedThisEpisode`)
decides whether milestone 06 logs. The **whole tick** runs inside a helper whose deferred
closure calls `recover()` directly: tick-context construction, the stale check, the
rebuild and the reporting. A panicking `WithTickContext` hook is therefore recovered too. A literal `defer recover()` does not stop a panic,
because `recover` is then not called *by* a deferred function (Go spec, "Handling
panics"). There are two panic sources:

- **Inside a locked region** (a `Key`, `Cacheable`, index `keys` or `Clone` callback
  during install). The region's panic rule has already marked the cache stale and
  released the lock. The recover only keeps the loop alive.
- **Off the lock** (a panicking `LoadAll`). Nothing has marked stale yet. The recovering
  closure takes the lock, calls `markStaleLocked(errPanicked)` wrapping the panic value,
  unlocks, and then emits the buffered events.

Either way the tick reports `Rebuild(TriggerLoop, Failed)` and the next tick retries.
Tests cover both sources.

Ticks do not queue: a rebuild that runs longer than the interval delays the next tick
(standard `time.Ticker` drop behavior). The health prober runs on its own goroutine, so a
slow rebuild never delays health detection (section 6.5).

## Tasks (test-first order)

Time-dependent tests run inside `synctest.Test`.

1. `TestPrime_InstallsAndTrusts`, `TestPrime_SingleBulkLoad` (exactly one `LoadAll`, zero
   `LoadOne`), `TestPrime_SecondCallRefused`, `TestPrime_RetryAfterFailure`.
2. `TestPrime_FailureReported`, `TestPrime_TimeoutReported` (`LoadAll` blocks; prime budget
   expires; `Failed` with an error that is `==` `context.DeadlineExceeded`, not merely
   `errors.Is`), `TestPrime_OverCapacityFails`,
   `TestPrime_ContractViolationFails`.
3. `TestRebuild_DiscardedByMidReadCommit`: `afterLoadAll` hook blocks; a `Mutate` completes
   and bumps the generation; the rebuild returns `Discarded`; cache still stale.
4. `TestRebuild_InstallBumpsOpenWindows`: a refresh that read before the capture is
   discarded after the install.
5. `TestRebuild_WaitsForAbandonedCommit` (SAFE-5): memstore ambiguous error with a delayed
   commit holding the shared fence; the loop's `LoadAll` blocks on the exclusive fence until
   the test releases the commit; the installed state includes the late commit.
6. `TestRebuild_FailedLeavesOldMaps`, `TestStale_OnlyClearedByInstalledRebuild` (try every
   other path: successful mutation on a stale cache, nothing-committed error, refresh).
7. `TestGeneration_BumpedOnRefreshStartMarkStaleCapture` (capture part).
8. Loop: `TestRun_RequiresPrime`, `TestRun_SecondConcurrentRefused`,
   `TestRun_RebuildsWhenStale` (mark stale; advance one interval; installed),
   `TestRun_SkipsWhenTrusted` (no `LoadAll` across many ticks),
   `TestRun_RetriesAfterFailure`, `TestRun_RetriesAfterDiscard`, `TestRun_ReturnsOnCancel`,
   `TestRun_SurvivesStorePanic` (panicking `LoadAll`; the cache is stale and unlocked;
   the next tick installs), `TestRun_SurvivesCallbackPanic` (panicking `Cacheable` during
   install; same assertions), `TestRun_SurvivesTickContextPanic` (the loop continues and
   the next tick runs),
   `TestRun_TickContextCalledPerTick`.
9. Budgets under `synctest`: `TestPrime_Budget`, `TestRebuild_Budget`, `TestRefresh_Budget`
   (the refresh budget is from 03; re-verify with the loop running).
10. `TestReads_NeverRebuild`: on a stale cache, 10,000 reads issue zero `LoadAll` calls.

## Acceptance

- All tests pass with `-race`. No test uses real sleeps; every time-dependent test runs in
  `synctest`.
- `rebuild.go` at 100% statement coverage.
- The section 7 failure-direction table rows for prime, rebuild loop and exclusive fence
  each map to a named test in this milestone (list them in the PR description).
