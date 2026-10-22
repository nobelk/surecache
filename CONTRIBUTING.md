# Contributing to surecache

Thank you for helping. `surecache` guards safety-critical reads, so the bar for changes is
correctness first, then clarity, then speed. Read [docs/design.md](docs/design.md) before
changing anything under the cache lock.

## Ground rules

- **Test first.** Write a failing test for the requirement, then the code. Each test names
  the requirement it covers in a comment, for example `// SAFE-7`.
- **Lock discipline.** No store call, log call or observer callback while holding the
  cache lock. Events raised under the lock are buffered and emitted after unlock.
- **Panic rule.** Every locked region that can modify state marks the cache stale in a
  deferred handler before it unlocks, so a panic never leaves half-modified maps trusted.
- **Internal invariants.** Section 10.4 of the design lists eleven invariants. A pull
  request that touches the lock, the marks, the generation or the indexes states which
  invariants it affects and which tests show they hold.
- **Dependencies.** The root module (`surecache`, `surecachetest`, `internal/...`) imports
  only the standard library, tests included. `depguard` and `make deps-check` enforce it.
  The `surecachepg` and `promobs` modules may add dependencies only after review.
- **Style.** Every Go file starts with `// SPDX-License-Identifier: Apache-2.0`. Code is
  formatted with `gofumpt` and `goimports`. Every exported identifier has a doc comment.
- **Time.** Library code never calls `time.Sleep`. Time-dependent tests run inside
  `testing/synctest` bubbles.

## Gates

Run these from a clean checkout before opening a pull request:

```sh
make tools   # once: installs pinned golangci-lint, gremlins, benchstat, govulncheck into ./bin
make lint    # golangci-lint in every module
make test    # go test -race -shuffle=on ./... in every module
make cover   # root module coverage gate
```

`make bench`, `make mutate` and `make fuzz` run the benchmark, mutation and fuzz suites.

## PostgreSQL tests

The `surecachepg` tests need a PostgreSQL server and skip when `SURECACHE_PG_DSN` is
unset. Each test creates and drops its own database, because advisory locks are scoped to
the database. Start a local server with Docker and run them:

```sh
make pgtest
```

This starts `compose.yaml` (PostgreSQL 18 on `127.0.0.1:55432`) and runs the tests with
`SURECACHE_PG_DSN=postgres://surecache:surecache@localhost:55432/postgres?sslmode=disable`.

## Commits and pull requests

Use [Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`,
`test:`, `docs:`, `ci:`, `chore:`. Keep pull requests focused and update
`CHANGELOG.md` under "Unreleased".

## Code of conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).
