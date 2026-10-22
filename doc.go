// SPDX-License-Identifier: Apache-2.0

// Package surecache is an in-process, write-invalidated cache for safety-critical reads:
// "Sure, or it asks."
//
// It sits in front of an authoritative store (typically a SQL database), answers hot
// reads from memory only when it can prove the answer matches the store's committed
// state, and otherwise defers so the caller asks the store. Every write stays
// synchronous to the store. The cache never expires, never evicts and never serves
// cached data because the store failed.
//
// Use it when reads vastly outnumber writes, the working set is bounded and fits in
// memory, one process is the only writer of the cached data, and a stale or missing
// answer has real cost. Do not use it when several processes write the same data, when
// the data set is unbounded, or when bounded staleness is acceptable: a TTL cache is
// simpler then.
//
// # Answers
//
// Every read returns an Answer. Found means the cache answered from memory. Absent, from
// Lookup only, is an authoritative negative: the cache is trusted and no mutation is in
// flight. Defer means "ask the store", never "absent": map it to a store call, always.
// Get never answers Absent, because entities the Spec does not hold (non-cacheable ones)
// still exist in the store.
//
// # Lifecycle
//
// NewBuilder takes a Spec and a Source; AddIndex registers unique secondary indexes;
// Build requires an OutagePolicy and returns a cache that starts stale and serves
// nothing. Call Prime once at startup and fail startup unless it returns Installed
// (branch on the RebuildResult, never on the error alone: Discarded has a nil error).
// Then run Run in the background for the cache's lifetime: it is the only rebuilder of
// a stale cache, and it drives the outage policy's health probe.
//
// # Mutations
//
// Route every store write that can change cached data through Mutate or Create (PRE-4).
// Mutate names every entity the write can change; a move between two entities names
// both. Create is only for a write that inserts exactly one previously nonexistent
// entity and modifies no existing one; an upsert uses Mutate. The caller's result is
// returned unchanged. A nothing-committed error (Spec.CommittedNothing) leaves the cache
// as is; success re-reads the named entities; any other error, including a context
// error, makes the whole cache stale until the loop rebuilds it.
//
// Wrap the application's repository in a decorator that delegates every method
// explicitly, never by embedding, so a method added later fails to compile until it is
// classified as a read or a mutation. See the examples/groups module in the repository.
//
// # Preconditions the caller guarantees
//
//   - PRE-1 Single writer. The owning process is the only writer of the cached data and
//     every write goes through Mutate or Create. A write from anywhere else can leave a
//     trusted cache wrong indefinitely. Stop the process before administrative writes
//     and restart it afterwards, so Prime reloads.
//   - PRE-2 Unique index keys. Each secondary key is held by at most one entity in
//     committed state, enforced by the store.
//   - PRE-3 Non-cacheable entities hold no index keys, preferably enforced by schema
//     constraints and checked by the surecachetest conformance fixtures.
//   - PRE-4 Complete mutation classification, as above.
//
// # Outage policies
//
// Whether a trusted cache should keep answering while the store is down depends on the
// caller's domain, so Build requires an explicit OutagePolicy. Callers that choose
// ServeWhileExact must accept these consequences:
//
//   - Any action whose prerequisite read is served by the trusted cache still proceeds
//     during an outage, even if a later store write in the same flow then fails.
//   - Once the cache is stale, actions whose prerequisite reads defer will fail those
//     reads and not proceed. If those actions are safety actions, the domain needs an
//     independent fallback that does not depend on the store.
//   - Nothing retries a failed write. Callers that need retries must drive them,
//     typically from a periodic sweep whose next successful read retries the action.
//
// DistrustAfterProbeFailures instead makes every read defer within about
// FailAfter × Interval + Timeout of an outage, even when nobody writes; feed failures
// seen on deferred reads to ObserveStoreResult.
//
// # Residual risks
//
//   - Budgets are not hard deadlines. A driver that does not watch the context at every
//     step, or a peer that acknowledges TCP but stops answering, can hold a call past a
//     budget. Configure server-side timeouts and TCP keepalive (package surecachepg).
//   - The single-writer precondition is unchecked.
//   - Capacity is enforced, not structural: if the cacheable set outgrows the capacity,
//     the cache refuses and stays stale.
//   - Every read takes one sync.RWMutex read lock; contention under many cores is
//     measured in docs/benchmarks.md.
//
// The package is pre-1.0: its API may change. See docs/design.md in the repository for
// the specification and docs/operations.md for the runbook.
package surecache
