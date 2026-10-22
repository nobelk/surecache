// SPDX-License-Identifier: Apache-2.0

// Package surecache is an in-process, write-invalidated cache for safety-critical reads.
//
// It answers hot reads from memory only when it can prove the answer matches the
// authoritative store; otherwise it defers and the caller asks the store. Every write
// stays synchronous to the store. "Sure, or it asks."
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
// DistrustAfterProbeFailures instead makes every read defer within a bounded time of an
// outage, even when nobody writes.
//
// The package is pre-release and its API is unstable. See docs/design.md in the
// repository for the specification.
package surecache
