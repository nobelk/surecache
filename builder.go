// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"errors"
	"fmt"
)

// Builder registers secondary indexes before the cache is built, so the index set is
// closed: every refresh and rebuild maintains all of them.
type Builder[ID comparable, E any] struct {
	spec    Spec[ID, E]
	src     Source[ID, E]
	owner   *token
	indexes []anyIndex[ID, E]
	names   map[string]bool
	errs    []error
	built   bool
}

// NewBuilder starts a cache over src for entities described by spec. Problems are
// recorded and returned together by Build.
func NewBuilder[ID comparable, E any](spec Spec[ID, E], src Source[ID, E]) *Builder[ID, E] {
	b := &Builder[ID, E]{spec: spec, src: src, owner: &token{}, names: map[string]bool{}}
	if src == nil {
		b.errs = append(b.errs, errors.New("surecache: nil Source"))
	}
	return b
}

// AddIndex registers a unique secondary index (PRE-2). keys returns every key an entity
// holds; a non-cacheable entity must hold none (PRE-3). capacity bounds the index; the
// cache refuses on overflow and never evicts.
//
// Problems are recorded on the builder and returned by Build. Calling AddIndex after
// Build is a programming error and panics.
func AddIndex[ID comparable, E any, K comparable](b *Builder[ID, E], name string, keys func(E) []K, capacity int) Index[ID, K] {
	if b.built {
		panic("surecache: AddIndex called after Build")
	}
	switch {
	case name == "":
		b.errs = append(b.errs, errors.New("surecache: index name is empty"))
	case b.names[name]:
		b.errs = append(b.errs, fmt.Errorf("surecache: duplicate index name %q", name))
	}
	if keys == nil {
		b.errs = append(b.errs, fmt.Errorf("surecache: index %q has nil keys", name))
	}
	if capacity <= 0 {
		b.errs = append(b.errs, fmt.Errorf("surecache: index %q capacity must be positive", name))
	}
	b.names[name] = true
	b.indexes = append(b.indexes, newIndex[ID, E](name, keys, capacity, 0))
	return Index[ID, K]{owner: b.owner, pos: len(b.indexes) - 1}
}

// Build validates the spec, the indexes and the options, and returns a stale cache that
// serves nothing until Prime returns Installed. WithOutagePolicy is required; there is no
// default (design section 8). All validation errors are returned at once.
func (b *Builder[ID, E]) Build(opts ...Option) (*Cache[ID, E], error) {
	if b.built {
		return nil, errors.New("surecache: Build called twice")
	}
	cfg := defaultConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	errs := append(append([]error{}, b.errs...), validateSpec(b.spec), cfg.validate())
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	b.built = true
	c := newCache(b.spec, b.src, cfg, b.owner, b.indexes)
	// Publish the initial state, so an observer reports stale before prime (SAFE-9).
	c.mu.Lock()
	c.queueStateLocked(stateEvent{kind: stateStale, on: true, initial: true})
	c.queueStateLocked(stateEvent{kind: stateEntities, n: 0})
	c.mu.Unlock()
	c.drain()
	return c, nil
}

func validateSpec[ID comparable, E any](s Spec[ID, E]) error {
	var errs []error
	if s.Name == "" {
		errs = append(errs, errors.New("surecache: Spec.Name is empty"))
	}
	if s.Key == nil {
		errs = append(errs, errors.New("surecache: Spec.Key is nil"))
	}
	if s.Clone == nil {
		errs = append(errs, errors.New("surecache: Spec.Clone is nil"))
	}
	if s.Cacheable == nil {
		errs = append(errs, errors.New("surecache: Spec.Cacheable is nil"))
	}
	if s.Compare == nil {
		errs = append(errs, errors.New("surecache: Spec.Compare is nil"))
	}
	if s.CommittedNothing == nil {
		errs = append(errs, errors.New("surecache: Spec.CommittedNothing is nil"))
	}
	if s.Capacity <= 0 {
		errs = append(errs, errors.New("surecache: Spec.Capacity must be positive"))
	}
	return errors.Join(errs...)
}
