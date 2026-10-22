# Milestone 01: Repository scaffold

## Goal

Create the three-module repository, lint configuration, CI skeleton and open-source hygiene
files, so that every later milestone lands into working gates.

## Prerequisites

None. Read [README.md](README.md) (decisions D1, D2, D11, D13, D16, D22) and
[docs/design.md](../design.md) section 3.4 (NFR-7).

## Requirements covered

- NFR-7 (minimal dependencies): the root module has no `require` lines, enforced by lint.
- D22 open-source hygiene.

## Deliverables

```
go.mod                                 module github.com/nobelk/surecache, go 1.25
doc.go                                 package surecache, short package doc
go.work                                go 1.25; use ., ./surecachepg, ./promobs
surecachepg/go.mod                     module github.com/nobelk/surecache/surecachepg
surecachepg/doc.go
promobs/go.mod                         module github.com/nobelk/surecache/promobs
promobs/doc.go
.golangci.yml
Makefile
scripts/covercheck/main.go             coverage threshold checker (stdlib only)
compose.yaml                           postgres service for local integration tests
.github/workflows/ci.yml
.github/workflows/nightly.yml
.github/dependabot.yml
.github/ISSUE_TEMPLATE/bug_report.md
.github/ISSUE_TEMPLATE/feature_request.md
.github/pull_request_template.md
CONTRIBUTING.md
SECURITY.md
CODE_OF_CONDUCT.md                     Contributor Covenant 2.1
CHANGELOG.md                           Keep a Changelog format, "Unreleased" section
README.md                              expanded skeleton (status: pre-release)
.gitignore                             stop ignoring go.work and go.work.sum
```

## Tasks

1. **Modules.** Create the root `go.mod` with `go 1.25` and no `require`. Create the two
   nested modules with `go 1.25` and a `require github.com/nobelk/surecache v0.0.0`
   resolved through the workspace. Do not add `replace` directives. Milestone 11 replaces
   the placeholder versions at release time.
2. **Workspace.** Commit `go.work` and `go.work.sum`. Remove both from `.gitignore`. A
   developer then gets a working multi-module build from a plain clone.
3. **SPDX headers.** Every Go file starts with `// SPDX-License-Identifier: Apache-2.0`. Add
   the `goheader` linter to enforce it.
4. **Lint** (`.golangci.yml`, golangci-lint v2 format, `version: "2"`):
   - Linters: `errcheck`, `govet` (enable all analyzers except `fieldalignment`),
     `staticcheck`, `revive` (exported doc comments), `gocritic`, `gosec`, `errorlint`,
     `exhaustive` (switches on `Answer`, `RebuildResult` and the observer enums),
     `unparam`, `unused`, `ineffassign`, `misspell`, `nolintlint` (require a reason),
     `forbidigo`, `depguard`, `goheader`, `testifylint` off (no testify).
   - Formatters: `gofumpt`, `goimports` with local prefix `github.com/nobelk/surecache`.
   - `depguard` rule `core-stdlib-only`: files under the root module, excluding
     `surecachepg/**` and `promobs/**`, may import only the standard library and
     `github.com/nobelk/surecache/...`.
   - `forbidigo`: forbid `fmt.Print*`, `log.Print*` and `time.Sleep` in non-test code;
     forbid `time.Sleep` in tests outside `synctest` bubbles (enforced by review, with the
     lint as a backstop on non-test code).
5. **Makefile** targets, each looping over the three modules where it makes sense:
   - `lint`: `golangci-lint run ./...` per module.
   - `test`: `go test -race -shuffle=on ./...` per module.
   - `cover`: root module `go test -race -coverprofile=coverage.out ./...`, then
     `go run ./scripts/covercheck -profile coverage.out -min 95 -full read.go,mutate.go,refresh.go,rebuild.go`.
     Until milestone 07 the target runs with `-report-only`.
   - `bench`: `go test -run '^$' -bench . -benchmem -count 10 ./...` in the root module.
   - `mutate`: `gremlins unleash` on the root module with the 0.90 efficacy threshold.
   - `fuzz`: loop over fuzz targets with `-fuzztime 5m`.
   - `pgtest`: `docker compose up -d --wait`, then `SURECACHE_PG_DSN=... go test -race ./...`
     in `surecachepg`.
   - `tools`: install pinned versions of golangci-lint, gremlins, benchstat, govulncheck
     into `./bin`. Pin exact versions in the Makefile; check the latest releases when
     implementing.
6. **covercheck.** A small stdlib program that parses a cover profile, prints per-file and
   total statement coverage, fails below `-min`, and fails when any file in `-full` is
   below 100%. Unit-test it with a fixture profile.
7. **CI** (`ci.yml`, on push to `main` and pull requests):
   - `lint`: golangci-lint v2 action, per module.
   - `test`: matrix Go `1.25.x` and `stable`; `make test`. A step runs `go list -m all` in
     the root module with `GOWORK=off` and fails if it prints anything but the root module
     (NFR-7).
   - `cover`: `make cover`, uploads `coverage.out` as an artifact.
   - `postgres`: service `postgres:17` and `postgres:18` (matrix), sets
     `SURECACHE_PG_DSN`, runs `surecachepg` tests with `-race`. Fails if any test skipped
     for a missing DSN (`go test -json` and a check for `"Action":"skip"` on DSN tests).
   - `vuln`: `govulncheck ./...` per module.
   - `bench`: runs benchmarks on the PR head and on `main`, posts `benchstat` output to the
     job summary. Informational only.
   - Pin every action to a full commit SHA; set `permissions: contents: read`.
8. **Nightly** (`nightly.yml`, schedule plus `workflow_dispatch`): `make mutate`,
   `make fuzz`, and the long model-checker run (added in milestone 07). Until those exist
   the jobs are present but call targets that print "not yet implemented" and succeed.
9. **Dependabot**: `gomod` for `/`, `/surecachepg`, `/promobs`; `github-actions` for `/`.
   Weekly.
10. **compose.yaml**: `postgres:18` with a health check, a fixed local port, and a
    superuser that may create databases (tests create one database per test, D16).
11. **OSS files.** `CONTRIBUTING.md` covers the test-first rule, the gates, Conventional
    Commits, the lock discipline and how to run PostgreSQL tests. `SECURITY.md` gives a
    private reporting channel (GitHub security advisories). Issue templates ask for the
    Go version, store and outage policy.
12. **README skeleton.** Name, tagline "Sure, or it asks.", one-paragraph purpose, "status:
    pre-release, API unstable", links to `docs/design.md` and `docs/plan/README.md`.
    Milestone 11 completes it.

## Acceptance

- `make lint`, `make test`, `make cover` (report only) pass in a clean clone.
- CI is green on the scaffold commit, including the `GOWORK=off` dependency check.
- `golangci-lint` fails on a scratch file that imports a non-stdlib package in the root
  module (check manually, then delete the file).
- Every Go file has the SPDX header.
