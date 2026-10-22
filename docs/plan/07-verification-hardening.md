# Milestone 07: Verification hardening and performance

## Goal

Raise confidence from "each rule has a test" to "random concurrent histories never violate
the safety invariants". Turn coverage and mutation thresholds into hard gates, add fuzzing,
and add the benchmarks and allocation tests that back NFR-3.

## Prerequisites

Milestones 05 and 06. Read design sections 3.1, 3.4 (NFR-3, NFR-5), 11.1 and 12 (last
bullet). Read [README.md](README.md) decisions D6, D7, D8.

## Requirements covered

SAFE-1, -2, -3, -10 end to end under concurrency; NFR-3; section 11.1 (concurrency, clone
isolation, mutation testing floor).

## Deliverables

```
internal/model/            reference model + history checker
model_test.go              randomized model-based concurrency test
fuzz_test.go               FuzzBulkInstallContract, FuzzRefreshSequence
bench_test.go              benchmarks
alloc_test.go              AllocsPerRun assertions
.gremlins.yaml             mutation config (threshold, excluded files)
docs/benchmarks.md         reference results and how to reproduce
Makefile, ci.yml, nightly.yml   gates switched from report-only to enforcing
```

## Design details

### Model-based concurrency checker

The goal is an executable form of SAFE-1 to SAFE-3: every answer the cache gives must
match the store's committed state at some instant during the read.

- **Versioned store.** Extend `memstore` with a commit counter. Every commit increments it
  atomically while holding the store mutex, and records the new state in an append-only
  log (`version -> state delta`).
- **Recording reads.** The test wraps each cache read: it reads the store version `v0`
  before the call and `v1` after. It records `(op, args, answer, v0, v1)`. Under the store
  mutex, the version is a linearization point for commits.
- **Checker.** For each `Found` or `Absent` answer, replay the log and check that some
  version `v` in `[v0, v1]` has a state where the answer is correct: for `Get`, the entity
  value equals the answer (compare with `reflect.DeepEqual` in the test only); for
  `Lookup`, the key holder equals the answer, or is absent for `Absent`; for `List`, the
  sorted cacheable id set equals the answer. `Defer` is always allowed.
- **Required deferrals (SAFE-2).** Committed-state correctness alone cannot catch a
  SAFE-2 bug: before a pending mutation commits, the old cached value still matches the
  store. The test therefore also records each mutation's window `[m0, m1]` with the
  `afterMark` and `beforeRelease` hooks (milestone 03), on the same logical clock as the
  reads, together with its affected ids (or "create"). A read whose interval lies
  entirely inside a window must have deferred if it is a `Get` or `Lookup` hit on an
  affected id, any `Lookup` miss, or any `List`. A non-deferred answer in that case is a
  violation even when it matches committed state. The checker also
  counts how often the cache deferred while trusted, so a cache that defers everything
  cannot pass silently. Set a minimum hit ratio for the idle phases.
- **Workload.** A seeded random driver with N readers and M mutators over a small id and
  key space (high collision rate). Operations: update fields, move a unique key between
  two entities (both ids passed), create, delete, toggle cacheable (with keys cleared, per
  PRE-3). Faults injected at a configurable rate: nothing-committed error, ambiguous with
  commit, ambiguous without commit, abandoned commit released later, `LoadOne` failure,
  `LoadAll` failure, panic in `op`. `Run` is active, so rebuilds interleave.
- **Structural invariant check.** After the run, and at quiescent points while trusted,
  check under the lock (test only): every index entry names an entity that holds the key;
  every key of every cached entity is indexed to that entity; the directory equals the
  sorted key set of `entities`; `pending` and `refreshes` are empty when idle.
- **Budgets.** PR CI runs a short budget (for example 2 seconds wall time, fixed seeds
  plus one random seed printed on failure). Nightly runs a long budget with many random
  seeds. A failing seed is reproducible with `-run TestModel_Linearizable -seed=<n>`.
- **Clone isolation** (`TestModel_CloneIsolation`): readers mutate every returned value;
  the checker must still pass.

### Fuzzing

- `FuzzBulkInstallContract`: decode bytes into a list of entities (ids, keys, cacheable
  flag) with collisions likely. Property: `buildAside` either fails with `ErrContract` or
  `ErrOverCapacity`, or returns maps that satisfy the structural invariant and contain
  exactly the input set.
- `FuzzRefreshSequence`: decode bytes into a single-goroutine sequence of store writes and
  cache mutations, with refresh install orders chosen through the hooks. Property: after
  each step, either the cache is stale, or the structural invariant holds and the cache
  matches the store exactly.
- Seed corpora live in `testdata/fuzz/`. Any crasher found becomes a permanent corpus
  entry.

### Mutation testing

`gremlins` over the root module, excluding `internal/model`, `scripts/` and test helpers.
Efficacy threshold 0.90 (`killed / (killed + lived)`). Equivalent mutants that cannot be
killed are recorded in `.gremlins.yaml` with a one-line reason each; reviewers reject
exclusions without a reason. The tool's exact flag and config names are confirmed when
implementing against the pinned `gremlins` version.

### Coverage gate

Switch `make cover` to enforcing: total at least 95%, and `read.go`, `mutate.go`,
`refresh.go`, `rebuild.go` at 100%. `outage.go` at 100% from milestone 05 is added to the
`-full` list.

### Benchmarks

Entity type: a small struct of value fields (no pointers), so `Clone` is a plain copy and
allocates nothing. A second entity type has a slice field to show realistic clone cost.

| Benchmark | Measures |
| --- | --- |
| `BenchmarkGet/size={1k,100k}` | trusted hit |
| `BenchmarkGetParallel/size=100k` | `b.RunParallel` read scaling, `-cpu 1,4,16` |
| `BenchmarkLookup` | trusted index hit and authoritative miss |
| `BenchmarkList/size={1k,10k}` | directory copy |
| `BenchmarkGetDuringMutations` | parallel reads with one mutator running continuously |
| `BenchmarkMutate` | mutation plus refresh against memstore |
| `BenchmarkRebuild/size={1k,100k}` | build-aside and install |
| `BenchmarkBaselineRWMutexMap` | a plain `RWMutex`-guarded map, for comparison |

Targets, recorded in `docs/benchmarks.md` with the reference machine and Go version:

- `Get` and `Lookup` on the value entity: 0 allocs/op. `List`: 1 alloc/op.
- `Get` within 1.5× of `BenchmarkBaselineRWMutexMap` single-threaded.
- `GetParallel`: report the scaling curve. If throughput at `-cpu 16` is below 4× of
  `-cpu 1`, open an issue to evaluate the copy-on-write read path (D6). Do not implement
  it in v1.

### Allocation tests (hard gate)

`TestGet_AllocsOnlyClone`, `TestLookup_ZeroAllocs`, `TestList_OneAlloc` with
`testing.AllocsPerRun`, using `NopObserver` and a recording observer that does not
allocate. These tests skip under `-race` (the race detector changes allocation counts) and
run in a separate CI step without `-race`.

## Tasks

1. `memstore` version counter and log; checker unit tests with handcrafted histories that
   must pass and must fail (a stale `Get`, a wrong `Absent`, a stale `List`, a `Get` hit
   inside a mutation window that matches committed state, an `Absent` during an
   uncommitted create).
2. Model workload and `TestModel_Linearizable`; `TestModel_CloneIsolation`.
3. Structural invariant checker and its use in the model test and fuzz targets.
4. Fuzz targets and seed corpora; add `make fuzz` to nightly.
5. Benchmarks, allocation tests, `docs/benchmarks.md`.
6. `.gremlins.yaml`; run, kill survivors with new tests, record true equivalents.
7. Flip coverage and mutation gates to enforcing in `Makefile`, `ci.yml`, `nightly.yml`.

## Acceptance

- `TestModel_Linearizable` passes on 1,000 random seeds locally before merge.
- Mutation efficacy ≥ 0.90 on the first enforcing run.
- Coverage gate enforcing and green.
- Each fuzz target has run for at least 30 minutes locally with no failures.
- Allocation tests green; benchmark results recorded.
