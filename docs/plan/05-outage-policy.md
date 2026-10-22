# Milestone 05: Outage policy

## Goal

Implement the two outage policies from section 8, the `unhealthy` gate, the health prober
driven by `Run`, and `ObserveStoreResult`.

## Prerequisites

Milestone 04. Read design section 8 and section 6.5's last paragraph. Can run in parallel
with milestone 06.

## Requirements covered

Section 8 in full, the `unhealthy` field of section 5.1, NFR-5 (prober timing under
`synctest`), the "stale-while-error" forbidden behavior (section 3.3).

## Deliverables

```
outage.go   OutagePolicy (sealed), ServeWhileExact, DistrustAfterProbeFailures,
            HealthPolicy, validation, prober, ObserveStoreResult
*_test.go
```

## Design details

### API

```go
// OutagePolicy is sealed; use ServeWhileExact or DistrustAfterProbeFailures.
type OutagePolicy interface{ outagePolicy() }

func ServeWhileExact() OutagePolicy
func DistrustAfterProbeFailures(p HealthPolicy) OutagePolicy
```

`HealthPolicy` as in section 8. `Build` validates: `Probe` and `Unavailable` non-nil;
`Interval > 0`; `0 < Timeout < Interval`; `FailAfter >= 1`; `RecoverAfter >= 1`. A
`DistrustAfterProbeFailures` value built from an invalid `HealthPolicy` makes `Build`
return the joined validation errors.

### Health state

Under the cache lock: `unhealthy bool`, `failStreak int`, `okStreak int`.
`recordProbe(err)`:

- `err == nil`: `okStreak++`, `failStreak = 0`. If `unhealthy` and
  `okStreak >= RecoverAfter`, clear `unhealthy` and record `UnhealthyChanged(false)`.
- `Unavailable(err)` (or the probe timed out): `failStreak++`, `okStreak = 0`, emit
  `ProbeFailure`. If not `unhealthy` and `failStreak >= FailAfter`, set `unhealthy` and
  record `UnhealthyChanged(true)`.
- Any other error: no change to either streak (business errors never count).

`unhealthy` does not set `stale` and does not bump the generation. A rebuild never clears
`unhealthy` (two gates). The cache can be not stale and unhealthy, and all reads defer.

### Prober

Started by `Run` (milestone 04 left the hook) as a goroutine with its own `time.Ticker` at
`Interval`. Each probe runs under `context.WithTimeout(ctx, Timeout)`. A probe that returns
after its deadline with any error counts as a failure. `Run` waits for the prober goroutine
before returning. The probe runs off the lock, and so does the observer and log emission.

### ObserveStoreResult

`ObserveStoreResult(err)` is a no-op under `ServeWhileExact`. Under
`DistrustAfterProbeFailures`, errors for which `Unavailable` returns true count toward
`FailAfter` exactly like probe failures. Successes (`err == nil`) and all other errors are
ignored: they do **not** count toward `RecoverAfter` and do not reset the failure streak.
Only the prober's own successes clear `unhealthy` (README clarification 10).

### ServeWhileExact

No prober. No extra state. Behavior is exactly milestones 02 to 04: trusted until the
first mutation, then pending/in-flight deferral, then stale on the ambiguous outcome. The
tests in this milestone prove that sequence end to end against a "dead" memstore.

## Tasks (test-first order)

All timing tests run in `synctest`.

1. `Build` validation table for `HealthPolicy`.
2. `TestServeWhileExact_TrustedWithDeadStore`: prime; make every memstore call fail with an
   ambiguous error; reads keep answering from memory; no store calls.
3. `TestServeWhileExact_FirstMutationDefersThenStale`: start a mutation (blocked); affected
   `Get` and `List` defer, unaffected `Get` still answers; release with an ambiguous error;
   everything defers; the loop retries and stays stale while the store is dead; revive the
   store; the next tick installs.
4. `TestDistrust_SetsUnhealthyAfterExactlyFailAfter`: `FailAfter-1` failures keep reads
   answering; the `FailAfter`-th sets `unhealthy`.
5. `TestDistrust_DetectionBound`: time from store death to `unhealthy` is at most
   `FailAfter × Interval + Timeout` of virtual time.
6. `TestDistrust_ProbeTimeoutCountsAsFailure` (probe blocks past `Timeout`).
7. `TestDistrust_NonUnavailableErrorsIgnored`.
8. `TestDistrust_SuccessResetsFailStreak`, `TestDistrust_FailureResetsOkStreak`.
9. `TestDistrust_RebuildDoesNotClearUnhealthy`: stale and unhealthy; the rebuild installs;
   reads still defer; `RecoverAfter` probe successes clear it.
10. `TestObserveStoreResult_CountsUnavailableOnly`,
    `TestObserveStoreResult_NoOpUnderServeWhileExact`,
    `TestObserveStoreResult_SuccessDoesNotRecover`.
11. `TestProber_NotDelayedBySlowRebuild`: a `LoadAll` blocks for many intervals; probes
    still run on schedule and set `unhealthy`.
12. `TestRun_WaitsForProber`: after `Run` returns, the prober goroutine has exited (use a
    probe that records calls; no calls after return).

## Acceptance

- All tests pass with `-race` under `synctest`.
- Package docs carry the three "consequences to document" from section 8 verbatim in
  substance.
- `outage.go` at 100% statement coverage.
