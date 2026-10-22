# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). The
`surecachepg` and `promobs` modules are versioned with their own tag prefixes.

## [Unreleased]

### Added

- Repository scaffold: root, `surecachepg` and `promobs` modules with a committed Go
  workspace, lint configuration, CI and nightly workflows, and open-source project files.
- Core types (`Spec`, `Source`, `Answer`, `RebuildResult`, sentinel errors), `Builder`
  with closed index registration and validation, options with budget defaults, the
  `Observer` interface with ordered state-event delivery, the capacity-bounded map, the
  build-aside bulk install and the `Get`, `Lookup` and `List` reads with their deferral
  rules.
- `Mutate` and `Create` with pending and in-flight marks, outcome settlement, the
  post-commit refresh with per-entity refresh windows, current-holder index removal,
  the insert-side conflict check, capacity projection on refresh, and the panic rule on
  every modifying region. `internal/memstore` gains deterministic fault injection.
- `Prime` with the prime budget and the `ErrAlreadyPrimed` rule, the fenced rebuild with
  generation capture, discard and refresh-window bumping on install, and the background
  `Run` loop with the rebuild budget, the per-tick context hook and panic recovery.
- Outage policies `ServeWhileExact` and `DistrustAfterProbeFailures` with `HealthPolicy`
  validation, the `unhealthy` read gate, the health prober driven by `Run`, and
  `ObserveStoreResult` for deferred-read failures.
- Edge-triggered `slog` logs `surecache_stale`, `surecache_rebuild_failed` and
  `surecache_recovered`, delivered off the lock and in transition order with the
  caller's or the tick's context, plus an audit of every observer signal.
- A randomized model-based concurrency checker that verifies every answer against the
  store's commit log and the required deferrals inside mutation windows, fuzz targets
  for the bulk-install contract and refresh sequences, benchmarks with allocation gates,
  mutation testing with `gremlins`, and enforcing coverage and mutation gates.
- `surecachetest`, the exported store conformance suite, with a self-test that proves
  each check fails against a store that breaks its rule.
- `surecachepg`, the PostgreSQL (pgx v5) primitives: the advisory-lock fence, the fenced
  snapshot reader with a counted single-statement querier, connection bounds,
  `CheckBudgets` and `IsNothingCommitted`, proven by the conformance suite, an
  abandoned-commit test and the outage and restart recipes against PostgreSQL 17 and 18.
- `promobs`, the Prometheus observer with pre-registered collectors and closed label
  sets.
- Godoc examples, the `examples/groups` PostgreSQL decorator module, complete package
  documentation, the README and the operations runbook.
