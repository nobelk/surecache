# Milestone 11: Examples, documentation and release v0.1.0

## Goal

Make the library adoptable: runnable examples, complete package documentation and README,
documented preconditions and operational procedures, and the first tagged release of all
three modules.

## Prerequisites

Milestones 07, 09 and 10. Read design sections 1, 3.5, 7, 8, 10.3 and 12.

## Requirements covered

PRE-1 to PRE-4 (documented and demonstrated), D21, D22, section 10.3.

## Deliverables

```
example_test.go                      godoc examples on an in-memory store
examples/groups/                     separate, untagged module: PostgreSQL decorator example
examples/groups/go.mod               requires surecache, surecachepg, promobs
README.md                            complete
doc.go                               complete package documentation
docs/operations.md                   runbook: administrative writes, capacity, outages
CHANGELOG.md                         v0.1.0 entry
```

## Tasks

1. **Godoc examples** (`example_test.go`, package `surecache_test`, stdlib only). Use a
   small in-memory store written in the example file itself. `internal/memstore` is not
   importable from an external test package's documentation in a way readers can copy.
   - `Example`: build, prime, run, read, mutate.
   - `ExampleLookup`: tri-state handling, mapping `Defer` to a store call.
   - `ExampleCache_Mutate`: passing every affected id for a move between two groups.
   - `ExampleCache_Prime`: branching on `RebuildResult`, failing startup on anything but
     `Installed`.
2. **PostgreSQL decorator** (`examples/groups`, its own module, added to `go.work`),
   following section 10.3. It may copy the schema from `surecachepg/internal/groupstore`
   but cannot import it:
   - a `Repo` interface, a store implementation with the fence and `WithMutation`, a
     `CachedRepo` decorator with `var _ Repo = (*CachedRepo)(nil)` and explicit delegation
     of every method (no embedding);
   - a `main` package under `cmd/` that wires `Prime`, `Run` under an `errgroup`,
     graceful shutdown and `promobs`;
   - a test that runs the decorator end to end against a per-test database. `pgtest` is
     internal to `surecachepg`, so the example carries its own small helper with the
     same `SURECACHE_PG_DSN` contract.
3. **Package documentation** (`doc.go`): purpose and when not to use it (section 1); the
   tri-state `Answer` and the rule "`Defer` means ask the store, never absent"; the
   lifecycle (`Build`, `Prime`, `Run`); `Mutate` versus `Create` (PRE-4); the outage
   policies and their consequences (section 8); preconditions PRE-1 to PRE-4; residual
   risks (section 12).
4. **README**: tagline; a 30-second pitch; a guarantees list (SAFE-1 to SAFE-12 in plain
   words); when to use it and when not; install (`go get`); a quick start that mirrors
   `Example`; links to the design, the operations runbook, the conformance suite and the
   PostgreSQL adapter; the stability statement (pre-1.0, API may change); the benchmark
   summary from `docs/benchmarks.md`; contributing and license.
5. **Operations runbook** (`docs/operations.md`): stop the process before administrative
   writes and restart after (PRE-1); sizing capacity and what happens at overflow; reading
   the metrics and the three log messages; outage behavior per policy; the server-side
   bounds to configure.
6. **API review.** Run `go doc -all` for each package and review against section 10.2 and
   the README decisions. Check that `apidiff` (from `golang.org/x/exp`, run as a tool,
   not a dependency) shows nothing unintended. Fix names before tagging; this is the
   cheapest moment.
7. **Release sequence.** Dependencies are released before their dependents. Each module
   is tidied and tested with `GOWORK=off`, so it resolves only published versions:
   1. Update `CHANGELOG.md` with the `v0.1.0` entry.
   2. Tag the root module `v0.1.0` and push the tag.
   3. In `promobs/go.mod` and `surecachepg/go.mod`, require
      `github.com/nobelk/surecache v0.1.0`. In each, run `GOWORK=off go mod tidy` and
      `GOWORK=off go test -race ./...`. Commit.
   4. Tag `promobs/v0.1.0` and `surecachepg/v0.1.0` (independent of each other); push the
      tags.
   5. In `examples/groups/go.mod`, require all three `v0.1.0` versions. Run
      `GOWORK=off go mod tidy` and `GOWORK=off go test ./...`. Commit. The example module
      is not tagged.
   6. Create a GitHub release for `v0.1.0` with the changelog entry.
   7. Verify from a clean directory outside the repository:
      `go get github.com/nobelk/surecache@v0.1.0` and the same for both submodules; build
      the quick start. Request the modules on `pkg.go.dev`.

## Acceptance

- All CI gates green on the release commit, including nightly mutation and fuzz jobs run
  manually on that commit.
- All three tags resolvable through the Go module proxy.
- The quick start in the README compiles as written (it is copied from `Example`, which
  `go test` runs).
