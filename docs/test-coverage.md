# Test coverage and verification

Measured on this branch with Go 1.27.3, the race detector, and a temporary PostgreSQL
18.6 instance. PostgreSQL tests were enabled with `SURECACHE_PG_DSN`; their results
are not the misleading low-coverage totals produced when database tests skip.
Coverage is statement coverage, not proof that every interleaving or failure is tested.

## Results

| Scope | Coverage |
| --- | ---: |
| Root module, including conformance suite and internal/tool packages | **98.6%** |
| Core `surecache` package | **100%** |
| `surecachetest` | **95.5%** |
| `internal/boundedmap` | **100%** |
| `internal/memstore` | **99.6%** |
| `internal/model` | **100%** |
| `scripts/covercheck` | **97.6%** |
| `surecachepg` public package | **100%** |
| `surecachepg` module, including internal PostgreSQL fixtures/proxy/helpers | **92.7%** |
| `promobs` module | **100%** |
| `examples/groups` module, including HTTP command | **88.4%** |
| Example repository/decorator package, excluding command | **93.6%** |

The root module improved from 95.5% to 98.6%; `surecachetest` from 87.2% to 95.5%;
`promobs` from 97.4% to 100%. With PostgreSQL enabled, the example module improved
from 56.8% to 88.4%. The core package was already at 100%; new tests strengthen
behavioral coverage rather than increasing its percentage.

## Gaps addressed

- Conformance-suite diagnostics for broken adapters, load/seed/update errors,
  malformed fixture sets, and custom entity comparison.
- In-memory store transaction/rejection behavior, model history replay, and coverage
  checker malformed-input/error handling.
- PostgreSQL fence, snapshot, callback and transaction failures; canceled-context
  cleanup; statement allowances and query-option rejection. Transaction test doubles
  now enforce separate fence/read transactions, closed transactions, and aborted
  transaction behavior instead of allowing impossible successful commits.
- Reference-store member moves, rollback on invalid moves, and soft deletion with
  predicate/index parity and key reuse, against real PostgreSQL.
- Prometheus partial-registration rollback and observer signal combinations. Allocation
  assertions now run without the race detector in both core and `promobs` via `make alloc`.
- Example decorator pass-through hits, misses and errors; rejected writes preserving
  trust and committed state; entity/index overflow refusing partial results.
- HTTP member responses, metrics, startup failure, cancellation, listener ownership,
  and an active request during shutdown. Requests inherit service cancellation;
  shutdown force-closes connections if the grace budget expires.
- The actual README quick-start block is compiled against the in-memory example store,
  preventing API/signature drift in documentation.

## Reproduce

From the repository root, with a PostgreSQL DSN whose user can create/drop databases:

```sh
export SURECACHE_PG_DSN='postgres://surecache:surecache@localhost:55432/postgres?sslmode=disable'
make test        # all four modules, -race -shuffle=on
make alloc       # allocation assertions must run without -race
make cover       # root coverage gate: >=95%, selected safety files at 100%
make cover-all   # every module, -race -count=1 -coverpkg=./..., module-local coverage.out

# The public PostgreSQL package's >=90% acceptance gate (also enforced in CI):
go run ./scripts/covercheck -profile surecachepg/coverage.out \
  -module github.com/nobelk/surecache/surecachepg -exclude internal/ -min 90
```

For PostgreSQL setup, see [README.md](../README.md#postgresql-integration-tests).
`make cover-all` reports all packages in each module, including internal helpers and
commands. Use the `go tool cover -func=coverage.out` total, not an average or sum of
individual `go test` percentages. Generated profiles are ignored by Git.

## Remaining limits

- Some conformance transaction/snapshot failure diagnostics and test infrastructure
  setup/teardown failures remain uncovered. These include reference-store SQL error
  handling and connection/proxy setup failures; coverage does not claim every possible
  database or network failure has been injected.
- The example command's process entry point, OS-signal wrapper, fixed-address bind
  failure and several defensive initialization branches are not all instrumented by
  unit tests. Handler behavior and the actual service lifecycle are exercised through
  an ephemeral listener against PostgreSQL; the production binary is also smoke-tested.
- A handler that ignores cancellation can keep doing application work after its network
  connection is force-closed. Application/store operations must honor their contexts;
  the example cannot forcibly terminate arbitrary goroutines or uncooperative drivers.
- PostgreSQL 18 was tested locally. CI covers PostgreSQL 17 and 18, and Go 1.25.x and
  stable. Mutation efficacy and long nightly fuzz/model budgets are separate quality
  gates; these percentages do not imply those expensive runs were performed locally.
