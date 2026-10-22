// SPDX-License-Identifier: Apache-2.0

package surecachetest

import (
	"context"
	"testing"
	"time"

	"github.com/nobelk/surecache"
)

// Harness describes a store adapter to the suite.
type Harness[ID comparable, E any] struct {
	// Spec is the spec the cache uses with this store; the suite uses Key, Cacheable
	// and CommittedNothing.
	Spec surecache.Spec[ID, E]
	// IndexKeys lists the key function of every index, with keys type-erased for
	// comparison.
	IndexKeys map[string]func(E) []any
	// NewStore returns an empty, isolated store. Register cleanup with t.Cleanup.
	NewStore func(t testing.TB) Store[ID, E]
	// Equal compares two entities. Nil means reflect.DeepEqual.
	Equal func(a, b E) bool
}

// Store is one isolated store instance under test.
type Store[ID comparable, E any] interface {
	// Source returns the adapter's surecache.Source over this store.
	Source() surecache.Source[ID, E]
	// Seed writes fixtures directly, including non-cacheable ones.
	Seed(ctx context.Context, fixtures []E) error
	// Fixtures returns the fixtures to seed. They must cover every entity shape:
	// cacheable with index keys, cacheable without index keys, and non-cacheable.
	// Every fixture is mutable by Update and BeginMutation, and at least two are
	// cacheable for BeginPairMutation.
	Fixtures() []E
	// NothingCommittedCases returns one operation per declared nothing-committed
	// error. Each runs against the seeded fixtures, must return that error, and must
	// change nothing.
	NothingCommittedCases() []Case
	// AbsentID returns an id that no fixture uses and that is never written.
	AbsentID() ID
	// Dump returns the full store state for equality checks with reflect.DeepEqual.
	Dump(ctx context.Context) (any, error)

	// Update changes one existing entity through the adapter's real mutation path
	// (shared fence held) and returns the entity as it must read afterwards.
	Update(ctx context.Context, id ID) (want E, err error)
	// Delete removes one existing entity through the real mutation path.
	Delete(ctx context.Context, id ID) error
	// BeginMutation starts the same change as Update on id and returns before COMMIT,
	// with the shared fence held. The change must touch every component of the
	// entity, so a mixed snapshot is detectable.
	BeginMutation(ctx context.Context, id ID) (OpenMutation[E], error)
	// BeginPairMutation starts one transaction that changes two distinct entities a
	// and b in every component, and returns before COMMIT with the shared fence held.
	BeginPairMutation(ctx context.Context, a, b ID) (OpenPairMutation[E], error)

	// SeedOverCapacity writes Spec.Capacity+1 cacheable entities.
	SeedOverCapacity(ctx context.Context) error
	// FenceWaitBound is the adapter's exclusive-fence wait limit.
	FenceWaitBound() time.Duration
}

// OpenMutation is a mutation transaction that has not committed yet.
type OpenMutation[E any] interface {
	// Want is the entity as it must read after Commit.
	Want() E
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// OpenPairMutation is an open transaction over two entities.
type OpenPairMutation[E any] interface {
	OpenMutation[E]
	// WantPair returns both entities as they must read after Commit.
	WantPair() (a, b E)
}

// SnapshotProbe is implemented by stores whose LoadOne or LoadAll runs more than one
// statement. SetReadPause installs a function the store calls between consecutive
// statements inside LoadOne and LoadAll (test builds only); nil removes it. The suite
// uses it to commit a mutation between two statements of one load.
type SnapshotProbe interface {
	SetReadPause(pause func())
}

// Case is one nothing-committed operation.
type Case struct {
	Name string
	Op   func(ctx context.Context) error
}
