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
