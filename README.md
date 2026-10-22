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

## Quick start

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
	return fmt.Errorf("prime: %v: %w", r, err)
}
go func() { _ = cache.Run(ctx) }() // the single background rebuilder

// Found may drive the action. Anything else asks the store; it is not "no such group".
if g, a := cache.Get("red"); a == surecache.Found {
	fmt.Println("red:", g.Members)
}

// Writes: name every entity the write can change.
err = cache.Mutate(ctx, []string{"blue"}, func() error { return store.AddMember("blue", "bob") })

// Absent is authoritative ("bob is in no group"). Defer asks the store, always.
id, a, err := surecache.Lookup(cache, byMember, "bob")
switch {
case err != nil || a == surecache.Defer:
	id, _, err = store.GroupOf("bob") // ask the store; never treat Defer as absent
case a == surecache.Absent:
	// trusted negative: do not act as if bob were in a group
default:
	fmt.Println("bob is in:", id)
}
```

The runnable versions are `Example` and `ExampleLookup` in
[example_test.go](example_test.go). For a PostgreSQL service with a cached repository
decorator, Prometheus metrics and graceful shutdown, see
[examples/groups](examples/groups).

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
