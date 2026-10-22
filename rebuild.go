// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"fmt"
	"slices"

	"github.com/nobelk/surecache/internal/boundedmap"
)

// snapshot is a complete set of maps built aside from a bulk load, ready to swap in.
type snapshot[ID comparable, E any] struct {
	entities  *boundedmap.Map[ID, E]
	indexes   []anyIndex[ID, E]
	directory []ID
}

// buildAside builds new maps from a bulk load without touching live state. It installs
// every entity or nothing (SAFE-8): it fails with ErrOverCapacity when a map refuses a
// key, and with ErrContract for a duplicate primary key, a non-cacheable entity, or two
// entities holding one unique key (SAFE-12, PRE-2). Caller callbacks run here, so the
// caller runs it under the panic rule.
func (c *Cache[ID, E]) buildAside(all []E) (*snapshot[ID, E], error) {
	s := &snapshot[ID, E]{
		entities:  boundedmap.NewSized[ID, E](c.spec.Capacity, len(all)),
		indexes:   make([]anyIndex[ID, E], len(c.indexes)),
		directory: make([]ID, 0, min(len(all), c.spec.Capacity)),
	}
	for i, ix := range c.indexes {
		s.indexes[i] = ix.fresh(len(all))
	}
	for _, e := range all {
		if !c.spec.Cacheable(e) {
			return nil, fmt.Errorf("%w: bulk load returned a non-cacheable entity", ErrContract)
		}
		id := c.spec.Key(e)
		if _, dup := s.entities.Get(id); dup {
			return nil, fmt.Errorf("%w: bulk load returned a primary key twice", ErrContract)
		}
		if c.cfg.cloneOnInstall {
			e = c.spec.Clone(e)
		}
		if !s.entities.Put(id, e) {
			return nil, fmt.Errorf("%w: primary map", ErrOverCapacity)
		}
		for _, ix := range s.indexes {
			if err := ix.insertBulk(id, e); err != nil {
				return nil, err
			}
		}
		s.directory = append(s.directory, id)
	}
	slices.SortFunc(s.directory, c.spec.Compare)
	return s, nil
}

// swapInLocked replaces the live maps with s. The caller holds c.mu for writing.
func (c *Cache[ID, E]) swapInLocked(s *snapshot[ID, E]) {
	c.entities = s.entities
	c.indexes = s.indexes
	c.directory = s.directory
}
