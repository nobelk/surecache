// SPDX-License-Identifier: Apache-2.0

package surecache

// hooks are test-only interleaving points (D17). Cache.hooks is nil in production and
// set only by tests in this package. No hook exists on the read path.
type hooks struct {
	// beforeDrain runs before a caller delivers queued state events.
	beforeDrain func()
}
