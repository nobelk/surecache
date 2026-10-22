// SPDX-License-Identifier: Apache-2.0

// Package memstore is an in-memory store for tests. It implements surecache.Source
// structurally (it cannot import surecache, whose in-package tests import it), with a real
// mutation fence, atomic transactions, a closed list of nothing-committed errors, call
// counters and an optional probe that reports store calls made under the cache lock.
package memstore

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// Nothing-committed errors. Each is returned only when the store committed nothing.
var (
	ErrNotFound     = errors.New("memstore: not found")
	ErrExists       = errors.New("memstore: already exists")
	ErrPrecondition = errors.New("memstore: precondition failed")
	// ErrConflict means a concurrent transaction committed a change to an entity this
	// one read or wrote; this one rolled back (like a serialization failure).
	ErrConflict = errors.New("memstore: concurrent update conflict")
)

// ErrOverCapacity is returned by LoadAll when more cacheable entities exist than the
// configured capacity.
var ErrOverCapacity = errors.New("memstore: over capacity")

// CommittedNothing is the classifier for surecache.Spec.CommittedNothing: true only for
// the declared nothing-committed errors. Context and other errors are ambiguous.
func CommittedNothing(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, ErrExists) ||
		errors.Is(err, ErrPrecondition) || errors.Is(err, ErrConflict)
}

// Config describes the entities a Store holds.
type Config[ID comparable, E any] struct {
	Key       func(E) ID
	Clone     func(E) E
	Cacheable func(E) bool
	// Capacity makes LoadAll refuse when more cacheable entities exist. Zero means no
	// limit.
	Capacity int
}

// Calls counts store calls per method.
type Calls struct {
	LoadOne, LoadAll, Write int64
}

// Store is an in-memory authoritative store.
type Store[ID comparable, E any] struct {
	cfg   Config[ID, E]
	fence Fence

	mu   sync.Mutex
	data map[ID]E
	// version is the commit counter; touched records the version of the last commit
	// that changed each id, so a transaction can detect a concurrent change at commit.
	version uint64
	touched map[ID]uint64

	writeFaults   []WriteFault
	loadOneFaults []ReadFault
	loadAllFaults []ReadFault

	loadOne, loadAll, writes atomic.Int64

	probe  func() bool
	report func(string)
}

// New returns an empty store.
func New[ID comparable, E any](cfg Config[ID, E]) *Store[ID, E] {
	return &Store[ID, E]{cfg: cfg, data: map[ID]E{}, touched: map[ID]uint64{}}
}

// SetLockProbe installs a probe that reports whether the cache lock is held; every store
// call reports a violation through report while it does. Set it before the store is used
// concurrently, and only in tests where the store-calling goroutine is the only cache
// user at that moment.
func (s *Store[ID, E]) SetLockProbe(held func() bool, report func(msg string)) {
	s.probe, s.report = held, report
}

func (s *Store[ID, E]) checkProbe(method string) {
	if s.probe != nil && s.probe() {
		s.report("memstore: " + method + " called while the cache lock is held")
	}
}

// Seed writes entities directly, without the fence. Use it for test setup only.
func (s *Store[ID, E]) Seed(es ...E) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version++
	for _, e := range es {
		id := s.cfg.Key(e)
		s.data[id] = s.cfg.Clone(e)
		s.touched[id] = s.version
	}
}

// Get returns a copy of id's committed state, without counting a call.
func (s *Store[ID, E]) Get(id ID) (E, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data[id]
	if ok {
		e = s.cfg.Clone(e)
	}
	return e, ok
}

// State returns a copy of the whole committed state.
func (s *Store[ID, E]) State() map[ID]E {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[ID]E, len(s.data))
	for id, e := range s.data {
		out[id] = s.cfg.Clone(e)
	}
	return out
}

// Calls returns the call counters.
func (s *Store[ID, E]) Calls() Calls {
	return Calls{LoadOne: s.loadOne.Load(), LoadAll: s.loadAll.Load(), Write: s.writes.Load()}
}

// LoadOne returns a detached copy of id, or ErrNotFound.
func (s *Store[ID, E]) LoadOne(ctx context.Context, id ID) (E, error) {
	s.loadOne.Add(1)
	s.checkProbe("LoadOne")
	var zero E
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if f, ok := pop(&s.mu, &s.loadOneFaults); ok {
		if err := applyReadFault(ctx, f); err != nil {
			return zero, err
		}
	}
	e, ok := s.Get(id)
	if !ok {
		return zero, ErrNotFound
	}
	return e, nil
}

// NotFound reports whether a LoadOne error means the entity does not exist.
func (s *Store[ID, E]) NotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}

// LoadAll takes the fence exclusively and releases it (every earlier mutation has then
// committed or rolled back), then returns every cacheable entity from one snapshot.
// Over capacity it returns ErrOverCapacity and no entities. Context errors are returned
// unwrapped.
func (s *Store[ID, E]) LoadAll(ctx context.Context) ([]E, error) {
	s.loadAll.Add(1)
	s.checkProbe("LoadAll")
	if f, ok := pop(&s.mu, &s.loadAllFaults); ok {
		if err := applyReadFault(ctx, f); err != nil {
			return nil, err
		}
	}
	if err := s.fence.Lock(ctx); err != nil {
		return nil, err
	}
	s.fence.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []E
	for _, e := range s.data {
		if s.cfg.Cacheable(e) {
			out = append(out, s.cfg.Clone(e))
		}
	}
	if s.cfg.Capacity > 0 && len(out) > s.cfg.Capacity {
		return nil, ErrOverCapacity
	}
	return out, nil
}

// Write runs fn as one atomic transaction holding the fence shared. If fn returns an
// error nothing commits and the error is returned unchanged; otherwise every staged
// change commits at once. Transactions run concurrently; one that read or wrote an
// entity another transaction changed since commits nothing and returns ErrConflict.
func (s *Store[ID, E]) Write(ctx context.Context, fn func(tx *Tx[ID, E]) error) error {
	s.writes.Add(1)
	s.checkProbe("Write")
	fault, _ := pop(&s.mu, &s.writeFaults)
	if err := s.fence.RLock(ctx); err != nil {
		return err
	}
	held := true
	defer func() {
		if held {
			s.fence.RUnlock()
		}
	}()
	tx := newTx(s)
	if err := fn(tx); err != nil {
		return err
	}
	switch fault.Kind {
	case NothingCommitted:
		return orDefault(fault.Err, ErrPrecondition)
	case AmbiguousCommitted:
		if err := s.commit(tx); err != nil {
			return err
		}
		return orDefault(fault.Err, ErrAmbiguous)
	case AmbiguousNotCommitted:
		return orDefault(fault.Err, ErrAmbiguous)
	case AbandonedCommit:
		held = false
		go func() {
			<-fault.Release
			_ = s.commit(tx)
			s.fence.RUnlock()
		}()
		return orDefault(fault.Err, ErrAmbiguous)
	case ContextAfterCommit:
		if err := s.commit(tx); err != nil {
			return err
		}
		return orDefault(fault.Err, context.Canceled)
	case PanicWrite:
		panic(fault.PanicValue)
	case BlockWrite:
		select {
		case <-fault.Release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.commit(tx)
}

func (s *Store[ID, E]) commit(tx *Tx[ID, E]) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, seen := range tx.seen {
		if s.touched[id] != seen {
			return ErrConflict
		}
	}
	s.version++
	for id := range tx.dels {
		delete(s.data, id)
		s.touched[id] = s.version
	}
	for id, e := range tx.puts {
		s.data[id] = e
		s.touched[id] = s.version
	}
	return nil
}

// Tx stages the changes of one Write.
type Tx[ID comparable, E any] struct {
	s    *Store[ID, E]
	puts map[ID]E
	dels map[ID]bool
	seen map[ID]uint64 // version of each id when this transaction first touched it
}

func newTx[ID comparable, E any](s *Store[ID, E]) *Tx[ID, E] {
	return &Tx[ID, E]{s: s, puts: map[ID]E{}, dels: map[ID]bool{}, seen: map[ID]uint64{}}
}

// Get returns id as this transaction sees it.
func (tx *Tx[ID, E]) Get(id ID) (E, bool) {
	if e, ok := tx.puts[id]; ok {
		return tx.s.cfg.Clone(e), true
	}
	if tx.dels[id] {
		var zero E
		return zero, false
	}
	return tx.readCommitted(id)
}

// readCommitted reads id's committed state and records the version it saw.
func (tx *Tx[ID, E]) readCommitted(id ID) (E, bool) {
	tx.s.mu.Lock()
	defer tx.s.mu.Unlock()
	if _, ok := tx.seen[id]; !ok {
		tx.seen[id] = tx.s.touched[id]
	}
	e, ok := tx.s.data[id]
	if ok {
		e = tx.s.cfg.Clone(e)
	}
	return e, ok
}

// Put stages an upsert.
func (tx *Tx[ID, E]) Put(e E) {
	id := tx.s.cfg.Key(e)
	tx.readCommitted(id) // a blind write still conflicts with a concurrent change
	delete(tx.dels, id)
	tx.puts[id] = tx.s.cfg.Clone(e)
}

// Insert stages an insert, or returns ErrExists.
func (tx *Tx[ID, E]) Insert(e E) error {
	if _, ok := tx.Get(tx.s.cfg.Key(e)); ok {
		return ErrExists
	}
	tx.Put(e)
	return nil
}

// Update stages fn applied to id, or returns ErrNotFound, or fn's error.
func (tx *Tx[ID, E]) Update(id ID, fn func(E) (E, error)) error {
	e, ok := tx.Get(id)
	if !ok {
		return ErrNotFound
	}
	e, err := fn(e)
	if err != nil {
		return err
	}
	tx.Put(e)
	return nil
}

// Delete stages a delete, or returns ErrNotFound.
func (tx *Tx[ID, E]) Delete(id ID) error {
	if _, ok := tx.Get(id); !ok {
		return ErrNotFound
	}
	delete(tx.puts, id)
	tx.dels[id] = true
	return nil
}
