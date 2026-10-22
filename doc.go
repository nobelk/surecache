// SPDX-License-Identifier: Apache-2.0

// Package surecache is an in-process, write-invalidated cache for safety-critical reads.
//
// It answers hot reads from memory only when it can prove the answer matches the
// authoritative store; otherwise it defers and the caller asks the store. Every write
// stays synchronous to the store. "Sure, or it asks."
//
// The package is pre-release and its API is unstable. See docs/design.md in the
// repository for the specification.
package surecache
