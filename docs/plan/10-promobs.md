# Milestone 10: Prometheus observer (`promobs`)

## Goal

Adapt `surecache.Observer` to Prometheus with pre-registered collectors and closed label
sets, so an idle scrape reads zeros rather than absent series.

## Prerequisites

Milestone 06 (final event set). Read design section 9.

## Requirements covered

Section 9 metric table; NFR-7 (the Prometheus dependency stays out of the core module).

## Deliverables

```
promobs/go.mod        requires github.com/prometheus/client_golang, github.com/nobelk/surecache
promobs/promobs.go    Metrics (collectors), New(reg, opts), (*Metrics).Observer(cacheName)
promobs/*_test.go
```

## Design details

```go
// Metrics owns the collectors. Register once per registry; share across caches.
type Metrics struct{ /* vecs */ }

func New(reg prometheus.Registerer, opts ...Option) (*Metrics, error)

// Observer returns an Observer for one cache. It pre-creates every label
// combination for that cache so all series exist at zero.
func (m *Metrics) Observer(cache string) surecache.Observer
```

- Metric names, kinds and labels exactly as section 9. Label values come from the closed
  enums (`Method`, `ReadSource`, `Trigger`, `RebuildResult`) through their `String()`
  methods. The only free label is `cache`, taken from `Spec.Name` by the caller.
- `Observer(cache)` resolves every child collector once, at creation, and stores the
  children in arrays indexed by the enum values. The `Read` hot path is an array index plus
  `Counter.Inc()`, with no map lookup and no allocation.
- Options: `WithNamespace` (default empty, so names start with `surecache_`), and
  `WithConstLabels`.
- The type embeds `surecache.NopObserver`, following the rule from D9.

## Tasks (test-first order)

1. `TestObserver_AllSeriesPresentAtStart`: gather from a fresh registry; every series of
   section 9 exists for the cache. Counters read 0. Before any cache is attached the
   `surecache_stale` gauge reads 0; after `Build` publishes the initial state (milestone
   02) it reads 1, and it stays 1 after a failed `Prime`.
2. One test per method: drive events, then compare with `testutil.ToFloat64` or
   `testutil.CollectAndCompare` against expected exposition text.
3. `TestObserver_ReadZeroAllocs` with `testing.AllocsPerRun` (non-race step).
4. `TestNew_DuplicateRegistration` returns an error, not a panic.
5. Integration: a real cache with memstore, a mutation, a stale episode and a recovery;
   assert counters and gauges.
6. Example `ExampleMetrics_Observer` in godoc.

## Acceptance

- Tests green with `-race`; allocation test green without it.
- The `promobs` module's only direct requirements are `client_golang` and `surecache`.
