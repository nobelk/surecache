# surecache implementation plan

This directory is the implementation plan for the first release (`v0.1.0`) of `surecache`,
an open-source Go library that implements the design in [docs/design.md](../design.md).
Section references such as "section 6.3" or "SAFE-7" point into that design document.

Each milestone file is self-contained: it states its goal, prerequisites, the requirements
it covers, the files it creates, the test-first task list and its acceptance gates. A
milestone can be handed to an implementer (human or agent) on its own, together with this
README and the design document.

## Milestones

| # | File | Delivers | Depends on |
| --- | --- | --- | --- |
| 01 | [01-scaffold.md](01-scaffold.md) | modules, workspace, lint, CI skeleton, OSS hygiene | none |
| 02 | [02-core-state-reads.md](02-core-state-reads.md) | public types, `Builder`/`Build`, bounded map, in-memory test store, state, bulk install, `Get`/`Lookup`/`List` | 01 |
| 03 | [03-mutations-refresh.md](03-mutations-refresh.md) | `Mutate`/`Create`, settle, post-commit refresh, refresh windows, index conflict rules, panic rule, test hooks | 02 |
| 04 | [04-prime-rebuild-run.md](04-prime-rebuild-run.md) | `Prime`, fenced rebuild with generation discard, background `Run` loop, budgets | 03 |
| 05 | [05-outage-policy.md](05-outage-policy.md) | `ServeWhileExact`, `DistrustAfterProbeFailures`, health prober, `ObserveStoreResult` | 04 |
| 06 | [06-observability.md](06-observability.md) | edge-triggered `slog` logs, observer contract audit | 04 |
| 07 | [07-verification-hardening.md](07-verification-hardening.md) | model-based concurrency checker, fuzzing, coverage and mutation gates, benchmarks | 05, 06 |
| 08 | [08-conformance-suite.md](08-conformance-suite.md) | `surecachetest` store conformance suite | 02 (final API from 04) |
| 09 | [09-postgres-adapter.md](09-postgres-adapter.md) | `surecachepg` primitives, integration recipes | 08 |
| 10 | [10-promobs.md](10-promobs.md) | `promobs` Prometheus observer | 06 |
| 11 | [11-example-docs-release.md](11-example-docs-release.md) | examples (`examples/groups` module), README, godoc, release `v0.1.0` | 07, 09, 10 |

```
01 ──> 02 ──> 03 ──> 04 ──┬──> 05 ──┐
        │                 │         ├──> 07 ──┐
        │                 └──> 06 ──┤         │
        │                           └──> 10 ──┼──> 11
        └──────────> 08 ──> 09 ───────────────┘
```

05 and 06 can run in parallel after 04. 08 can start once 02 has fixed `Spec` and `Source`;
it must be re-checked against the final API after 04. 10 needs only the `Observer`
interface from 02 and the event set completed in 06.

## Decisions

Settled in the planning session on 2026-10-22.

| ID | Decision |
| --- | --- |
| D1 | Module path `github.com/nobelk/surecache`. License Apache-2.0. |
| D2 | Minimum Go version 1.25 (`testing/synctest` is GA there). `go 1.25` in every `go.mod`. CI matrix: Go 1.25.x and the latest stable release. |
| D3 | `v0.1.0` ships five components: core `surecache`, `surecachetest` conformance suite, `surecachepg` PostgreSQL reference adapter, `promobs` Prometheus observer, runnable examples. |
| D4 | Plan layout: this README plus one self-contained file per milestone. |
| D5 | The design document lives in the repository as `docs/design.md`. |
| D6 | Performance: one `sync.RWMutex` exactly as designed. Benchmarks with stated targets, allocation counts asserted in tests, `benchstat` comparison in CI. A copy-on-write or sharded read path is a post-v1 decision, taken only if benchmarks show contention (section 12). |
| D7 | Test bar for the core module: statement coverage at least 95%, and 100% on `read.go`, `mutate.go`, `refresh.go` and `rebuild.go`; mutation efficacy at least 0.90; `-race` on every test run; `testing/synctest` for every time-dependent test; one deterministic interleaving test per section 11.1 bullet; a randomized model-based concurrency checker; fuzz targets for bulk-load contract checks and refresh sequences. No TLA+ model in v1. |
| D8 | Mutation testing with `gremlins`, efficacy threshold 0.90, in a nightly and manually triggered workflow (not on every pull request). |
| D9 | `Observer` is an interface with one method per event and an embeddable `NopObserver`. Implementations must embed `NopObserver` so that new methods do not break them. |
| D10 | No public `WithClock` option. The library uses package `time` directly and all time-dependent tests run under `testing/synctest`. |
| D11 | GitHub Actions CI: lint, test (race, Go matrix), coverage gate, benchmarks, PostgreSQL integration, `govulncheck`. Mutation testing and long fuzzing run nightly. |
| D12 | The plan is committed directly to `main`. |
| D13 | Multi-module repository. The root module (`surecache`, `surecachetest`, `internal/...`) has no `require` lines. `surecachepg/` and `promobs/` are separate modules. A committed `go.work` ties them together for development. |
| D14 | PostgreSQL driver: pgx v5 (`pgx.Tx`, `pgxpool`). |
| D15 | `surecachepg` ships primitives, not a generic `Source`: fence, snapshot reader, connection configuration, error classifier, budget check. The schema-specific `Source` is the caller's (section 10.1) and is shown in an example. |
| D16 | PostgreSQL tests read the DSN from `SURECACHE_PG_DSN` and skip when it is unset. CI runs a `postgres` service container. `compose.yaml` provides the same for local runs. Every test creates and drops its own database, because advisory locks are scoped to the database (section 4.7). |
| D17 | Interleaving tests use an unexported `hooks` field on `Cache`, nil in production and set only by tests in `package surecache`. Hook points exist on the mutation, refresh and rebuild paths, never on the read path. |
| D18 | `Mutate` with an empty `ids` slice returns `ErrNoIDs` without running `op`. Duplicate ids are removed before marking. |
| D19 | Milestones as listed above. |
| D20 | This README carries a traceability matrix from every requirement to its milestone and tests. |
| D21 | Examples: a stdlib in-memory `example_test.go` in the core package (godoc example), and a full PostgreSQL decorator example in the separate, untagged module `examples/groups` (location refined by clarification 11). |
| D22 | Release tags `v0.1.0`, `surecachepg/v0.1.0`, `promobs/v0.1.0`. No API stability promise before v1. OSS files: `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`, `CHANGELOG.md`, issue and pull request templates, Dependabot. Every Go file starts with `// SPDX-License-Identifier: Apache-2.0`. |
| D23 | `Run` returns `ErrNotPrimed` unless an earlier `Prime` returned `Installed`. |
| D24 | A second `Prime` after `Installed` returns `ErrAlreadyPrimed`. `Prime` may be retried after `Failed` or `Discarded`. A second concurrent `Run` returns `ErrAlreadyRunning`. |
| D25 | Budget defaults from section 4.7: prime 10 s, rebuild 4 s, refresh 2 s, rebuild interval 1 s. `Build` rejects non-positive durations and a health `Timeout` that is not less than its `Interval`. The outage policy has no default. |
| D26 | Exported sentinel errors, all usable with `errors.Is`: `ErrOutcomeUnknown` (wraps the mutation error), `ErrContract`, `ErrKeyConflict`, `ErrOverCapacity`, `ErrUnsettled`, `ErrForeignIndex`, `ErrNoIDs`, `ErrNotPrimed`, `ErrAlreadyPrimed`, `ErrAlreadyRunning`, `ErrNoOutagePolicy`. |

## Spec clarifications and deviations

These refine the design document. Where they conflict with it, this list wins.

1. **No `WithClock`** (D10). Section 10.2 lists it; it is dropped. NFR-5 is met with
   `testing/synctest`.
2. **`Mutate` with no ids** returns `ErrNoIDs` (D18). Section 6.2 does not cover it. Without
   the check, a caller that forgot the ids would get a mutation that only bumps `inFlight`,
   and `Get` would keep serving the entity the op changes.
3. **`Run` requires a successful `Prime`** (D23). Otherwise the loop could install the first
   rebuild in the background, and a caller that ignored a failed `Prime` would start serving.
   This tightens SAFE-9.
4. **Repeat calls** (D24). FUN-5 says no path other than the loop rebuilds after prime, so a
   second successful `Prime` is refused.
5. **Panics in `Mutate` and `Create` propagate.** Section 6.6 says `guard` "recovers outside
   the region". The plan reads that as: the deferred cleanup marks the cache stale, releases
   all marks, and then the caller's panic continues unchanged. The library never swallows a
   panic from the caller's `op`. `Run` is the only place that recovers and continues.
6. **A refresh batch on a stale cache still bumps `generation`** before it returns (section
   6.3 pseudocode). This is how FUN-4's "it still invalidates in-progress rebuilds" is met.
7. **`Observer` shape** (D9) is defined in milestone 02; the design lists only the signals.
8. **Budget defaults** (D25). The design gives the values as examples; the plan makes them
   defaults.
9. **Builder misuse.** `AddIndex` on a builder that has already built panics with a clear
   message (an init-time programmer error). `Build` returns every validation error at once
   through `errors.Join`.
10. **`ObserveStoreResult` counts failures only.** Section 8 says deferred-read errors count
    toward `FailAfter`; it does not say whether deferred-read successes count toward
    `RecoverAfter`. They do not. A success from a caller's read may have come from a
    pooled connection that the probe would not use, and the probe is the one signal the
    policy controls. Successes and non-`Unavailable` errors are ignored entirely.
11. **Example module location.** D21 placed the PostgreSQL decorator example under
    `surecachepg/example/groups`. That example wires `promobs`, which would make the
    `surecachepg` module depend on Prometheus. The example therefore lives in its own
    untagged module, `examples/groups`, released after its three dependencies. The
    schema that `surecachepg`'s own conformance tests need lives in
    `surecachepg/internal/groupstore`.
12. **Ordered state events and initial state.** State events (`StaleChanged`,
    `UnhealthyChanged`, `Entities`) and their logs are queued under the lock and drained
    in order under a separate emitter mutex. Off-lock delivery therefore never reorders
    or drops a transition. Observer callbacks may call read methods but not `Mutate`,
    `Create`, `Prime` or `Run`. `Build` publishes the initial stale state (milestone 02).
13. **PostgreSQL read bound.** `statement_timeout` bounds each statement, not the
    snapshot transaction. `surecachepg` bounds the read as `ReadStatementTimeout ×
    MaxReadStatements`, and the snapshot callback gets a restricted, counted,
    single-statement query interface instead of a `pgx.Tx` (milestone 09).
14. **A refresh batch holds its ids pending.** Found by the model checker in milestone
    07. A refresh that a newer same-entity window discards leaves the entity to the
    newer batch, and its mutation then releases its marks. Section 6.3 assumed the
    newer batch's mutation still marks the entity, but a `Create` marks no id, since
    its id is unknown until `op` returns. A delete of X whose refresh is discarded by a
    re-create of X would then serve the deleted X until the create's refresh installs.
    `refreshBatch` therefore marks every id pending when it opens the windows and
    releases the marks in `endRefresh` (test `TestCreate_RefreshHoldsMarkWhileItOwnsTheWindow`).

## Repository layout (target for v0.1.0)

```
surecache/
├── go.mod                  module github.com/nobelk/surecache, no requires
├── go.work                 use ., ./surecachepg, ./promobs (./examples/groups from milestone 11)
├── doc.go                  package documentation
├── surecache.go            Spec, Source, Answer, RebuildResult
├── builder.go              Builder, AddIndex, Build, validation
├── index.go                Index handle, type-erased index storage
├── options.go              Option and With* functions, defaults
├── errors.go               sentinel errors
├── cache.go                Cache struct and state (section 5.1), markStale
├── read.go                 Get, Lookup, List
├── mutate.go               guard, Mutate, Create, settle
├── refresh.go              refreshBatch, refreshOne, remove/install, conflict check
├── rebuild.go              build-aside, install, Prime, rebuild
├── run.go                  Run loop
├── outage.go               OutagePolicy, HealthPolicy, prober, ObserveStoreResult
├── observer.go             Observer, NopObserver, event enums, event buffer
├── log.go                  edge-triggered slog logging
├── hooks.go                unexported test hooks
├── example_test.go         godoc example on the in-memory store
├── internal/
│   ├── boundedmap/         generic capacity-bounded map, unsynchronized
│   ├── memstore/           in-memory Source with a real fence and fault injection
│   └── model/              reference model and history checker (milestone 07)
├── surecachetest/          exported store conformance suite
├── surecachepg/            separate module: pgx v5 primitives
│   └── internal/           pgtest helpers, groupstore test schema
├── promobs/                separate module: Prometheus observer
├── examples/groups/        separate untagged module: PostgreSQL decorator example (section 10.3)
├── docs/
│   ├── design.md
│   └── plan/
├── .github/                workflows, templates, dependabot
├── .golangci.yml
├── compose.yaml            local PostgreSQL for integration tests
└── Makefile                lint, test, cover, mutate, bench, pgtest
```

## Cross-cutting rules

These apply to every milestone.

- **Test first.** Write the failing test for a requirement, then the code. Each test names
  the requirement it covers in a comment (for example `// SAFE-7`).
- **Implementation checklist.** Section 10.4's eleven internal invariants are binding. A
  pull request that touches the lock, the marks, the generation or the indexes says which
  invariants it affects and which tests show they hold.
- **Lock discipline.** No store call, log call or observer call while holding `c.mu`.
  Events raised under the lock go into a small buffer and are emitted after unlock. State
  events go through the ordered state-event queue (milestone 02).
- **Panic rule.** Every locked region that can modify state uses this shape, so that a panic
  marks stale before the lock is released and no `recover` is needed:

  ```go
  c.mu.Lock()
  done := false
  defer func() {
      if !done {
          c.markStaleLocked(errPanicked)
      }
      c.mu.Unlock()
  }()
  // ... modify state ...
  done = true
  ```

- **Read path budget** (NFR-3). A trusted read does one `RLock`, one map lookup and one
  `Clone`. No allocation besides the clone, no logging, no reflection, no hook checks.
  Milestone 07 asserts this with `testing.AllocsPerRun`.
- **Dependencies.** The root module imports only the standard library, tests included. A
  `depguard` rule enforces it. Other modules may add dependencies only after review.
- **Headers and style.** Every Go file starts with `// SPDX-License-Identifier: Apache-2.0`.
  Code is formatted with `gofumpt` and `goimports`. Every exported identifier has a doc
  comment.
- **Per-milestone gates.** Before a milestone is done, these pass from a clean checkout:
  `make lint`, `make test` (`go test -race ./...` in every module), and `make cover` for the
  root module. The coverage and mutation thresholds become hard gates in milestone 07. Until
  then they are reported only.
- **Commits.** Conventional Commits (`feat:`, `test:`, `docs:`, `ci:`, `chore:`). One
  milestone per branch and pull request, unless a milestone file says otherwise.

## Quality gates (final, from milestone 07 on)

| Gate | Tool | Threshold | Where |
| --- | --- | --- | --- |
| Format | `gofumpt`, `goimports` (through golangci-lint v2 formatters) | no diff | every PR |
| Lint | golangci-lint v2, `.golangci.yml` | zero issues | every PR |
| Tests | `go test -race -shuffle=on ./...` per module | all pass | every PR, Go 1.25.x and stable |
| Coverage | `go test -coverprofile` + `scripts/covercheck` | root module ≥ 95%; 100% on `read.go`, `mutate.go`, `refresh.go`, `rebuild.go` | every PR |
| PostgreSQL | `go test -race ./...` in `surecachepg` with `SURECACHE_PG_DSN` | all pass, none skipped | every PR, PostgreSQL 17 and 18 |
| Vulnerabilities | `govulncheck ./...` per module | none reachable | every PR |
| Benchmarks | `go test -bench` + `benchstat` against `main` | informational; allocation tests are hard gates | every PR |
| Mutation | `gremlins unleash` on the root module | efficacy ≥ 0.90 | nightly, manual |
| Fuzz | `go test -fuzz` per target, 5 minutes each | no new failures | nightly |
| Model checker | long run of the model-based test (`-count`, larger op budget) | no violations | nightly |

## Traceability matrix

Every requirement in the design maps to the milestone that implements it and the tests that
prove it. Test names are the planned names; the implementing milestone may refine them but
must keep this table current.

### Safety invariants (section 3.1)

| ID | Milestone | Tests |
| --- | --- | --- |
| SAFE-1 | 03, 07 | `TestMutate_ReadAfterSuccessSeesNewState`, `TestModel_Linearizable` |
| SAFE-2 | 02, 03, 07 | `TestModel_Linearizable` (required-deferral check), `TestGet_DefersWhilePending`, `TestLookup_MissDefersWhileInFlight`, `TestCreate_LookupMissDefersDuringCreate`, `TestMutate_PendingHeldUntilRefreshInstalled` |
| SAFE-3 | 02 | `TestGet_MissAlwaysDefers`, `TestLookup_AbsentOnlyWhenTrustedAndIdle`, `TestList_DefersWhileInFlight` |
| SAFE-4 | 03, 09 | `TestMutate_AmbiguousErrorMarksStale`, `TestMutate_ContextErrorAfterStoreMarksStale`, `TestAbandonedCommitFenced` |
| SAFE-5 | 03, 04, 09 | `TestMutate_StaleCacheIssuesNoStoreRead`, `TestRebuild_WaitsForAbandonedCommit`, `TestAbandonedCommitFenced` |
| SAFE-6 | 03, 04 | `TestRefresh_OlderSameEntityDiscarded`, `TestRebuild_DiscardedByMidReadCommit`, `TestRebuild_InstallBumpsOpenWindows` |
| SAFE-7 | 03 | `TestRefresh_RemovalKeepsOtherHolder`, `TestRefresh_LaggingValueKeepsNewHolder` |
| SAFE-8 | 02, 04 | `TestInstall_OverCapacityRefused`, `TestPrime_OverCapacityFails`, `TestRefresh_OverflowMarksStale` |
| SAFE-9 | 02, 04 | `TestObserver_InitialStatePublished`, `TestCache_StaleBeforePrime`, `TestPrime_FailureReported`, `TestPrime_TimeoutReported`, `TestRun_RequiresPrime` |
| SAFE-10 | 02, 07 | `TestGet_ReturnsClone`, `TestCloneOnInstall`, `TestModel_CloneIsolation` |
| SAFE-11 | 03 | `TestMutate_PanicMarksStaleAndReleasesMarks`, `TestLockedRegion_CallbackPanicLeavesStaleAndUnlocked` |
| SAFE-12 | 02, 03 | `TestInstall_DuplicateUniqueKeyFails`, `TestInstall_NonCacheableFails`, `TestRefresh_NonCacheableWithKeysMarksStale`, `TestRefresh_KeyMismatchMarksStale`, `FuzzBulkInstallContract` |

### Functional requirements (section 3.2)

| ID | Milestone | Tests |
| --- | --- | --- |
| FUN-1 | 02 | `TestGet_TrustedHit`, `TestLookup_TrustedHit`, `TestList_Trusted` |
| FUN-2 | 02 | `TestReads_StaleDefersAll` |
| FUN-3 | 04 | `TestReads_NeverRebuild` |
| FUN-4 | 03 | `TestMutate_StaleCacheIssuesNoStoreRead`, `TestMutate_StaleStillBumpsGeneration` |
| FUN-5 | 04 | `TestRun_RebuildsWhenStale`, `TestRun_SkipsWhenTrusted`, `TestPrime_SecondCallRefused` |
| FUN-6 | 03 | `TestRefresh_DetachedFromCallerCancel`, `TestRefresh_KeepsContextValues`, `TestRefresh_TimeoutMarksStale` |
| FUN-7 | 03 | `TestMutate_NothingCommittedLeavesCache` |
| FUN-8 | 03 | `TestMutate_ReturnsOpErrorUnchanged`, `TestMutate_RefreshFailureNotReported` |

### Forbidden behaviors (section 3.3)

| Behavior | Milestone | How it is prevented and checked |
| --- | --- | --- |
| Expiry, eviction, dropped writes | 02 | No timer or eviction code exists; `boundedmap` refuses at capacity (`TestBoundedMap_RefusesNewKeyAtCapacity`). |
| Stale-while-error | 02, 05 | Reads never call the store; `TestReads_StaleDefersAll`, outage policy tests. |
| Write-behind | 03 | `op` runs synchronously inside `Mutate`; `TestMutate_OpRunsBeforeReturn`. |
| Coalescing with a pre-mutation load | 02 | No read-through exists; reads only consult memory. Reviewed in 07. |
| Notifications as only invalidation | 03 | Invalidation is driven only by `Mutate`/`Create`. |
| Store call under the lock | 02–05 | In deterministic single-user tests, `memstore` asserts the cache lock is free on every call (lock probe); `TestStoreCalls_NeverUnderLock`. Concurrent tests do not use the probe (see milestone 02). |
| Log or observer under the lock | 06 | `TestObserver_CanReenterCache`, `TestLogger_CanReenterCache` (a reentrant `Get` from the callback would block under the lock; a real-time timeout fails the test). |

### Non-functional requirements (section 3.4)

| ID | Milestone | Tests or gate |
| --- | --- | --- |
| NFR-1 | 02, 03 | `TestBoundedMap_*`, `TestInstall_OverCapacityRefused`, `TestRefresh_OverflowMarksStale`, `TestIndexCapacity_ReplaceAtCapacitySucceeds`, `TestIndexCapacity_GrowthPastCapacityRefused`, `TestPendingMarks_DroppedAtZero`, `TestRefreshWindows_DroppedAtZero` |
| NFR-2 | 02, 04 | `TestReads_NoStoreCallsWhenTrusted`, `TestPrime_SingleBulkLoad` |
| NFR-3 | 07 | `TestGet_AllocsOnlyClone`, `BenchmarkGet`, `BenchmarkGetParallel` |
| NFR-4 | 02, 06 | `TestObserver_*`, `TestEmitter_DeliversInTransitionOrder`, `TestLog_StaleOncePerTransition`, `TestLog_RebuildFailedOncePerEpisode`, `TestLog_RecoveredOncePerEpisode`, `TestLog_NeverPerRead` |
| NFR-5 | 04, 05, 07 | all time-based tests under `synctest`; interleaving tests; conformance suite |
| NFR-6 | 04, 09 | `TestPrime_Budget`, `TestRebuild_Budget`, `TestRefresh_Budget`, `surecachepg.CheckBudgets` tests |
| NFR-7 | 01 | `depguard` rule; root `go.mod` has no `require` |

### Caller preconditions (section 3.5)

| ID | Milestone | Support |
| --- | --- | --- |
| PRE-1 | 11 | Documented in package docs and README with the restart procedure. |
| PRE-2 | 02, 08 | Bulk install rejects duplicates; conformance fixtures check uniqueness. |
| PRE-3 | 03, 08 | Refresh fails safe on violation; conformance predicate and index parity check. |
| PRE-4 | 11 | Decorator example with explicit delegation and a compile-time interface assertion. |

### Store contract (section 4)

| Section | Milestone | Support |
| --- | --- | --- |
| 4.1, 4.2 | 08, 09 | Conformance `NothingCommittedLeavesStoreUnchanged`, `ContextErrorsAreNotNothingCommitted`; `surecachepg.IsNothingCommitted` helper. |
| 4.3 | 08, 09 | Conformance `FenceOrdersBulkLoad`; `surecachepg.Fence`. |
| 4.4 | 08 | Conformance `LoadOneReadsLatestCommit`, `LoadOneKeyMatches`, `LoadOneNotFoundMeansAbsent`, `LoadOneConsistentSnapshot`. |
| 4.5 | 08, 09 | Conformance `LoadAllEachKeyOnce`, `LoadAllCacheableParity`, `LoadAllRefusesOverCapacity`, `LoadAllContextErrorsUnwrapped`, `LoadAllConsistentSnapshot`. |
| 4.6, 4.7 | 09 | `surecachepg` connection configuration, `Bounds.Validate`, `CheckBudgets` (aggregate read bound), fence timeout test, `TestAbandonedCommitFenced`. |

### Internal invariants (section 10.4)

| # | Milestone | Tests |
| --- | --- | --- |
| 1 | 02–06 | `TestStoreCalls_NeverUnderLock`, `TestObserver_CanReenterCache` |
| 2 | 03, 04 | `TestGeneration_BumpedOnRefreshStartMarkStaleCapture` |
| 3 | 03 | `TestCreate_InFlightCountedOnce`, `TestMutate_PanicMarksStaleAndReleasesMarks` |
| 4 | 03 | `TestRefresh_RemovalKeepsOtherHolder`, `TestRefresh_ConflictMarksStaleMapsUntouched`, `TestRefresh_MoveKeyBetweenEntities` |
| 5 | 04 | `TestRebuild_FailedLeavesOldMaps`, `TestRebuild_InstallBumpsOpenWindows` |
| 6 | 04 | `TestStale_OnlyClearedByInstalledRebuild` |
| 7 | 03 | `TestMutate_StaleCacheIssuesNoStoreRead` |
| 8 | 03 | `TestRefresh_NonCacheableRemoved`, `TestRefresh_NonCacheableWithKeysMarksStale` |
| 9 | 02 | `TestLookup_AbsentOnlyWhenTrustedAndIdle`, `TestGet_MissAlwaysDefers` |
| 10 | 03 | `TestRefresh_BatchOpensAllWindowsFirst` |
| 11 | 02–04 | `TestLockedRegion_CallbackPanicLeavesStaleAndUnlocked` |

### Section 11 verification items

| Item | Milestone |
| --- | --- |
| 11.1 unit tests and interleavings | 02–05, 07 |
| 11.1 mutation testing floor 0.90 | 07 |
| 11.2 conformance suite | 08 |
| 11.3 reversible outage recipe | 09 |
| 11.3 restart recipe | 09 |
