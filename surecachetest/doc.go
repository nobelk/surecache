// SPDX-License-Identifier: Apache-2.0

// Package surecachetest is the store conformance suite (design section 11.2). Every
// store adapter runs it to show that it meets the store contract of section 4: the
// nothing-committed errors commit nothing, LoadOne reads the latest commit as one
// consistent snapshot, LoadAll returns exactly the cacheable set once from one snapshot
// ordered after every earlier mutation (the fence), refuses over capacity with no
// entities, and returns context errors unwrapped.
//
// # Writing a harness
//
// The suite cannot know the adapter's schema or mutation API, so the adapter describes
// them through a Harness: the Spec the cache will use, the index key functions, and a
// NewStore function that returns an empty, isolated Store. The Store seeds fixtures,
// runs the adapter's real mutation path (holding the shared fence), and exposes open,
// uncommitted mutations so the suite can probe the fence and snapshot isolation. Run
// the suite from an ordinary test:
//
//	func TestConformance(t *testing.T) {
//		surecachetest.Run(t, surecachetest.Harness[ID, Entity]{
//			Spec:      spec,
//			IndexKeys: map[string]func(Entity) []any{"member": memberKeys},
//			NewStore:  newIsolatedStore,
//		})
//	}
//
// A Store whose LoadOne or LoadAll runs more than one statement must implement
// SnapshotProbe, so the suite can commit between two statements of one load. A Store
// that does not implement it must document that LoadOne and the entire LoadAll each run
// exactly one statement; only then do the snapshot checks skip.
//
// # Cost and isolation
//
// Most checks are fast. The fence checks (FenceOrdersBulkLoad, FenceWaitTimesOut,
// MutationsDoNotBlockEachOther, and the fence-wait cases of the context checks) use
// real time with generous slack, because adapters talk to real databases; together they
// take a few seconds plus FenceWaitBound. Every check that holds the fence open needs
// its own isolated store: for PostgreSQL, whose advisory locks are scoped to the
// database, NewStore must return a store over its own database.
package surecachetest
