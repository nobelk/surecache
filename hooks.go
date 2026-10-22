// SPDX-License-Identifier: Apache-2.0

package surecache

// hooks are test-only interleaving points (D17). Cache.hooks is nil in production and
// set only by tests in this package. Ids are passed as any so the struct is not generic;
// tests type-assert. No hook exists on the read path.
type hooks struct {
	// afterMark runs in guard after the marks are set and the lock released. Together
	// with beforeRelease it brackets the interval in which SAFE-2 requires deferral.
	afterMark func(ids any)
	// beforeRelease runs in guard's cleanup before it takes the lock to release marks.
	beforeRelease func(ids any)
	// afterOp runs between op returning and settle.
	afterOp func()
	// beforeRefresh runs after a batch opened its windows, before the first LoadOne.
	beforeRefresh func(ids any)
	// beforeLoadOne runs before each refresh LoadOne.
	beforeLoadOne func(id any)
	// afterLoadOne runs between LoadOne and the install lock.
	afterLoadOne func(id any)
	// beforeDrain runs before a caller delivers queued state events.
	beforeDrain func()
}
