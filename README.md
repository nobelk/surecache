# surecache

**Sure, or it asks.**

`surecache` is an in-process, read-mostly Go cache for systems where a wrong answer is
worse than a slow one. It sits in front of an authoritative store, answers hot reads
from memory only when it can prove the answer matches the store's committed state, and
otherwise returns `Defer` so the caller asks the store. Writes stay synchronous to the
store. The cache never expires, never evicts, and never serves cached data because the
store failed.

Most caches trade a little staleness for hit rate. `surecache` makes the opposite trade:
correctness first, availability through outages second, speed third.

> **Status: pre-1.0.** The API may change before v1.0.0.

## Use case

One process owns a bounded set of groups and their members. A sweep asks, several times
a second, which group a member belongs to before it acts. A person occasionally adds or
moves a member. A stale membership, or a false "not a member," takes the wrong action:
a safety step skipped, a record dropped, an authorization granted to the wrong group.

A TTL cache can do that. It may still serve the pre-write membership after the write has
returned, and a failed refresh can keep the last value because the store is down.
`surecache` answers from memory only while it can prove the answer matches committed
state:

- `Found` is the current entity. `Absent`, from a secondary-index lookup, means the
  member is in no group. Either may drive the action.
- `Defer` means "ask the store." It is never "not a member." Map it to a store call.

Writes stay on the store, through `Mutate` or `Create`, and name every entity they can
change. A move between two groups names both. With `ServeWhileExact`, trusted reads keep
succeeding while the database is down, until a write whose outcome is unknown makes the
whole cache stale. Then every read defers until a fenced rebuild restores trust.

The same story, as a PostgreSQL repository decorator, is
[examples/groups](examples/groups). The quick start below is the in-memory version.

### Other useful cases

These all require a **bounded domain owned by one writer process**, not a shared
cache for independent writers. Every write must pass through that process's cache
mutation path, name all affected entities, and obey the store/fence contracts in
[the design](docs/design.md). An advisory fence alone does not make multiple writer
processes safe.

- **Authorization and revocation:** one service owns a bounded set of role groups.
  Removing a principal changes its group; moving it names both groups. Later lookups
  must not grant from the pre-revocation state. `Defer` requires an authoritative read;
  if that fails, deny or return unavailable, never grant from a remembered value.
- **Routing ownership:** one controller owns a bounded assignment of workers to
  partitions. A move invalidates both owners while it commits. A trusted negative
  can mean "unassigned"; `Defer` cannot. If the store cannot answer, pause routing or
  return unavailable rather than picking an owner. This is not a distributed lease
  or a replacement for fencing work performed by downstream workers.
- **Safety sweeps:** a single service repeatedly checks which group owns a device
  before scheduling an action. A trusted membership or trusted negative can inform
  the check; on `Defer`, consult the store. If it cannot answer, follow a domain-defined
  fail-safe response: postponing a required protective action may itself be unsafe.
  Safety actions need an independent fallback that does not depend on the store.
  A read is not an atomic check-and-act transaction: protect actions that can race
  later writes separately.

Choose the outage policy deliberately. `ServeWhileExact` may keep answering trusted
reads during an outage; it does not promise store reachability. If decisions must
stop when the store is unreachable, consider `DistrustAfterProbeFailures` with a
probe (distrust is delayed by its thresholds), or require a live store check for the
operation. Neither policy permits bypass writes or makes `Defer` an absence.

## Guarantees

In plain words (the precise statements are in [docs/design.md](docs/design.md), section 3.1):

1. After a write returns success, no later read returns the old state (SAFE-1).
2. While a write is in flight, every read it can affect defers (SAFE-2).
3. "Absent" is only answered when the cache is trusted and no write is in flight; a
   primary-key miss always defers (SAFE-3).
4. A write whose outcome is unknown (including a timeout) makes the whole cache stale
   at once (SAFE-4), and only a fenced rebuild restores trust (SAFE-5).
5. An older read never overwrites newer state (SAFE-6), and re-indexing one entity
   never deletes another entity's index entry (SAFE-7).
6. A rebuild installs everything or nothing; over capacity the cache refuses and stays
   stale (SAFE-8).
7. The cache starts stale and serves nothing until its first successful prime (SAFE-9).
8. Callers get deep copies (SAFE-10).
9. A panic during a write leaves the cache stale, never half-updated (SAFE-11).
10. A store result that breaks the store contract makes the cache stale instead of
    installing it (SAFE-12).

## When to use it

- Reads vastly outnumber writes.
- The working set is bounded and fits in memory.
- One process is the only writer of the cached data.
- A stale or missing answer has real cost, as in the membership sweep above.

## When not to use it

- Several processes write the same data: the cache cannot see their writes.
- The data set is unbounded or larger than memory: the cache never evicts.
- Bounded staleness is acceptable: a TTL cache is simpler.

## Install

```sh
go get github.com/nobelk/surecache@latest
# PostgreSQL (pgx v5) primitives and the Prometheus observer are separate modules:
go get github.com/nobelk/surecache/surecachepg@latest
go get github.com/nobelk/surecache/promobs@latest
```

Requires Go 1.25 or later. The core module depends only on the standard library.

## Build and test from Git

Install Go **1.25+**, Git and Make. Docker Compose is needed only for the local
PostgreSQL workflow below; optional developer tools are installed separately.

```sh
git clone https://github.com/nobelk/surecache.git
cd surecache
# go.work includes four modules; root ./... alone does not traverse them.
for m in . surecachepg promobs examples/groups; do
  (cd "$m" && go build ./...) || exit 1
done
# Unit tests (plus any integration tests enabled by your environment):
for m in . surecachepg promobs examples/groups; do
  (cd "$m" && go test -timeout 180s ./...) || exit 1
done
make test    # all four modules, -race -shuffle=on (requires race/CGO toolchain)
make alloc   # allocation assertions without -race; they skip under -race
```

PostgreSQL integration tests in `surecachepg` and `examples/groups` **skip when
`SURECACHE_PG_DSN` is unset**. A passing unit/race run without that variable does not
validate PostgreSQL behavior. Root tests include a compile check of the quickstart
against the in-memory store in `example_test.go`, with no third-party dependencies.

### PostgreSQL integration tests

From the repository root, using Docker Compose's disposable PostgreSQL 18 instance
on `127.0.0.1:55432`:

```sh
docker compose up -d --wait
export SURECACHE_PG_DSN='postgres://surecache:surecache@localhost:55432/postgres?sslmode=disable'
(cd surecachepg && go test -race -count=1 -timeout 180s ./...)
(cd examples/groups && go test -race -count=1 -timeout 180s ./...)
# Or: make pgtest (starts Compose and supplies its default DSN).
# For another server: make pgtest PG_DSN='postgres://...'
docker compose down
```

Tests create and drop isolated databases; the DSN user needs permission to do both
(`CREATEDB`). `make pgtest` leaves Compose running: shut it down yourself. The local
Compose data directory is tmpfs, so its data is disposable, not durable storage.

### Run the groups HTTP example

The command does **not** create tables or seed data. With no other example instance
running, initialize a fresh local database first. This schema matches `groups.Schema`
in [examples/groups/store.go](examples/groups/store.go):

```sh
docker compose up -d --wait
docker compose exec -T postgres psql -U surecache -d postgres -v ON_ERROR_STOP=1 <<'SQL'
CREATE TABLE groups (
  id text PRIMARY KEY,
  name text NOT NULL
);
CREATE TABLE members (
  member text PRIMARY KEY,
  group_id text NOT NULL REFERENCES groups (id) ON DELETE CASCADE
);
INSERT INTO groups (id, name) VALUES ('red', 'Red'), ('blue', 'Blue');
INSERT INTO members (member, group_id) VALUES ('ann', 'red');
SQL
# Build a binary so Ctrl-C signals the service directly.
(cd examples/groups && go build -o /tmp/surecache-groups ./cmd/groups)
DATABASE_URL='postgres://surecache:surecache@localhost:55432/postgres?sslmode=disable' /tmp/surecache-groups
```

Leave the service in the foreground. In another terminal:

```sh
curl --fail-with-body http://localhost:8080/member/ann  # {"group":"red"}
curl -i http://localhost:8080/member/nobody           # 404
curl --fail-with-body http://localhost:8080/metrics
```

`DATABASE_URL` configures the service; `SURECACHE_PG_DSN` configures tests. The server
listens on `:8080` and exposes only member reads and metrics, not HTTP write endpoints.
The repository decorator demonstrates writes through `Mutate`/`Create`.

Stop the service with **Ctrl-C** (SIGINT; SIGTERM also triggers graceful shutdown),
then run `docker compose down` from the repository root and remove
`/tmp/surecache-groups` if desired. Stop the service before any SQL edits or reseeding,
then restart it so `Prime` reloads the store. The schema commands above are for a fresh
database, not an idempotent migration. Never run overlapping writer instances.

### Optional developer checks

```sh
make tools                 # pinned tools into ./bin; see Makefile for versions
make lint                  # all four modules; needs golangci-lint
make deps-check            # root module must have no external dependencies
make cover                 # root-only race coverage + configured covercheck gate
make cover-all             # report every module; set SURECACHE_PG_DSN for DB coverage
make bench                 # root benchmarks, -benchmem, 10 repetitions
make fuzz FUZZTIME=10s      # each of the two root fuzz targets, no -race
make model MODEL_BUDGET=2s MODEL_SEEDS=5  # root model checks with -race
make mutate                # root mutation testing; needs gremlins
make vuln                  # all four modules; needs govulncheck
```

Allocation checks are separate from race tests. `make cover` checks the thresholds
configured in `Makefile`; it does not measure all four modules. `make cover-all` writes
`coverage.out` in each module and reports coverage across that module, including its
internal helpers and commands. Run it with `SURECACHE_PG_DSN` set for a complete
PostgreSQL report. CI also enforces at least 90% coverage on the public `surecachepg`
package (excluding internal test helpers). See [the coverage report](docs/test-coverage.md)
for measured results and remaining gaps. Fuzz/model/mutation
runs can be expensive; raise their budgets deliberately. Tool releases may require a
newer Go toolchain than the library's minimum. See [CONTRIBUTING.md](CONTRIBUTING.md)
for contributor gates.

## Quick start

This is a function body taking `ctx context.Context` and the in-memory `store` from
[example_test.go](example_test.go), with that file's imports (`context`, `errors`,
`fmt`, `slices`, `strings`, and `surecache`). The store's `GroupOf` returns **two**
values, `(string, bool)`; real repository reads must also propagate store errors.

```go
// A Source reads the authoritative store: LoadOne, NotFound and a fenced LoadAll.
b := surecache.NewBuilder(surecache.Spec[string, Group]{
	Name:      "groups",
	Key:       func(g Group) string { return g.ID },
	Clone:     func(g Group) Group { g.Members = slices.Clone(g.Members); return g },
	Cacheable: func(Group) bool { return true },
	Compare:   strings.Compare,
	Capacity:  1000,
	CommittedNothing: func(err error) bool {
		return errors.Is(err, errNotFound) || errors.Is(err, errMemberTaken)
	},
}, surecache.Source[string, Group](store))
byMember := surecache.AddIndex(b, "member", func(g Group) []string { return g.Members }, 10_000)
// ServeWhileExact: keep answering trusted reads while the store is down.
cache, err := b.Build(surecache.WithOutagePolicy(surecache.ServeWhileExact()))
if err != nil {
	return err
}

// Never serve an unprimed cache: anything but Installed fails startup.
if r, err := cache.Prime(ctx); r != surecache.Installed {
	return fmt.Errorf("prime: %v: %v", r, err)
}
ctx, cancel := context.WithCancel(ctx)
done := make(chan error, 1)
go func() { done <- cache.Run(ctx) }() // the single background rebuilder
defer func() { cancel(); <-done }() // join it when this function returns

// Primary-key misses defer: ask the store, never infer absence from the cache.
g, a := cache.Get("red")
if a != surecache.Found {
	g, err = store.LoadOne(ctx, "red")
	if err != nil {
		return err // includes not-found; this example expects red to exist
	}
}
fmt.Println("red:", g.Members)

// Writes: name every entity the write can change.
if err := cache.Mutate(ctx, []string{"blue"}, func() error { return store.AddMember("blue", "bob") }); err != nil {
	return err
}

// Absent is authoritative ("bob is in no group"). Defer asks the store, always.
id, a, err := surecache.Lookup(cache, byMember, "bob")
var ok bool
switch {
case err != nil || a == surecache.Defer:
	id, ok = store.GroupOf("bob") // two results; never treat Defer as absent
case a == surecache.Absent:
	ok = false // trusted negative: do not act as if bob were in a group
default:
	ok = true
}
if ok {
	fmt.Println("bob is in:", id)
}
```

The runnable versions are `Example` and `ExampleLookup` in
[example_test.go](example_test.go). For a PostgreSQL service with a cached repository
decorator, Prometheus metrics and graceful shutdown, see
[examples/groups](examples/groups). Run the in-memory examples and the README compile
regression without PostgreSQL:

```sh
go test -run '^(Example|ExampleLookup|ExampleCache_Mutate|ExampleCache_Prime|TestREADMEQuickstartCompiles)$' -count=1 -timeout 90s .
```

## Documentation

- [Design and guarantees](docs/design.md)
- [Operations runbook](docs/operations.md): administrative writes, capacity, metrics,
  logs, outages and server-side bounds
- [Store conformance suite](surecachetest) (`surecachetest`): run it against your store
  adapter
- [PostgreSQL adapter primitives](surecachepg) (`surecachepg`): the fence, the fenced
  snapshot reader, connection bounds and error classification
- [Prometheus observer](promobs) (`promobs`)
- [Benchmarks](docs/benchmarks.md)
- [Implementation plan](docs/plan/README.md)

## Performance

On an AMD Ryzen 9 5900HX with Go 1.27: a trusted `Get` of a value entity takes about
38 ns with zero allocations, `Lookup` about 14 ns with zero allocations, and `List` one
allocation for the returned copy. Every read takes one `sync.RWMutex` read lock, which
limits scaling across many cores; see [docs/benchmarks.md](docs/benchmarks.md).

## Stability

`surecache` is pre-1.0: the API may change in any minor release. The three modules are
versioned together: `v0.1.0`, `surecachepg/v0.1.0`, `promobs/v0.1.0`.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and the [code of conduct](CODE_OF_CONDUCT.md).
Report security issues privately as described in [SECURITY.md](SECURITY.md).

## License

Apache-2.0. See [LICENSE](LICENSE).
