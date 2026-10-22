// SPDX-License-Identifier: Apache-2.0

package surecache

import "errors"

// Sentinel errors. Every error the cache returns or reports as a stale cause matches one
// of these, or the store's own error, through errors.Is.
var (
	// ErrOutcomeUnknown marks a mutation whose outcome is ambiguous: it may have
	// committed, or may still commit. The stale cause wraps it and the mutation error.
	ErrOutcomeUnknown = errors.New("surecache: mutation outcome unknown")
	// ErrContract reports a store result that breaks the store contract (SAFE-12).
	ErrContract = errors.New("surecache: store contract violation")
	// ErrKeyConflict reports a refresh whose index key is held by another entity that has
	// no refresh in progress.
	ErrKeyConflict = errors.New("surecache: index key held by another entity")
	// ErrOverCapacity reports that the primary map or an index would exceed its capacity.
	ErrOverCapacity = errors.New("surecache: over capacity")
	// ErrUnsettled is the stale cause when a mutation unwound before its outcome settled,
	// for example by panic (SAFE-11).
	ErrUnsettled = errors.New("surecache: mutation did not settle")
	// ErrForeignIndex is returned by Lookup for an index handle from another cache.
	ErrForeignIndex = errors.New("surecache: index handle belongs to another cache")
	// ErrNoIDs is returned by Mutate when ids is empty; op is not run.
	ErrNoIDs = errors.New("surecache: mutation names no ids")
	// ErrNotPrimed is returned by Run unless an earlier Prime returned Installed.
	ErrNotPrimed = errors.New("surecache: cache not primed")
	// ErrAlreadyPrimed is returned by Prime after an earlier Prime returned Installed.
	ErrAlreadyPrimed = errors.New("surecache: cache already primed")
	// ErrAlreadyRunning is returned by Run while another Run is active.
	ErrAlreadyRunning = errors.New("surecache: Run already active")
	// ErrNoOutagePolicy is returned by Build without WithOutagePolicy; there is no
	// default.
	ErrNoOutagePolicy = errors.New("surecache: no outage policy")
)

// errPanicked is the stale cause when a locked region unwinds by panic.
var errPanicked = errors.New("surecache: panic in a locked region")
