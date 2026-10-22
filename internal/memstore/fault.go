// SPDX-License-Identifier: Apache-2.0

package memstore

import (
	"context"
	"errors"
	"sync"
)

// ErrAmbiguous is the default injected ambiguous error, standing in for a lost
// connection: the change may or may not have committed.
var ErrAmbiguous = errors.New("memstore: connection lost")

// WriteFaultKind selects the scripted outcome of one Write.
type WriteFaultKind int

// Write fault kinds. Every outcome is deterministic.
const (
	// NothingCommitted commits nothing and returns Err (default ErrPrecondition).
	NothingCommitted WriteFaultKind = iota + 1
	// AmbiguousCommitted commits and returns Err (default ErrAmbiguous).
	AmbiguousCommitted
	// AmbiguousNotCommitted commits nothing and returns Err (default ErrAmbiguous).
	AmbiguousNotCommitted
	// AbandonedCommit returns Err (default ErrAmbiguous) at once but keeps holding the
	// fence shared, and commits when Release is closed: a COMMIT the client gave up on.
	AbandonedCommit
	// ContextAfterCommit commits and returns Err (default context.Canceled).
	ContextAfterCommit
	// PanicWrite panics with PanicValue inside the transaction; nothing commits.
	PanicWrite
	// BlockWrite blocks inside the transaction, holding the fence shared, until Release
	// is closed and then commits normally; if ctx ends first it commits nothing and
	// returns ctx.Err().
	BlockWrite
)

// WriteFault scripts one Write call.
type WriteFault struct {
	Kind       WriteFaultKind
	Err        error
	Release    <-chan struct{}
	PanicValue any
}

// ReadFaultKind selects the scripted outcome of one LoadOne or LoadAll.
type ReadFaultKind int

// Read fault kinds.
const (
	// FailRead returns Err (default ErrAmbiguous).
	FailRead ReadFaultKind = iota + 1
	// BlockRead blocks until Release is closed and then reads normally; if ctx ends
	// first it returns ctx.Err().
	BlockRead
	// PanicRead panics with PanicValue.
	PanicRead
)

// ReadFault scripts one LoadOne or LoadAll call.
type ReadFault struct {
	Kind       ReadFaultKind
	Err        error
	Release    <-chan struct{}
	PanicValue any
}

// InjectWrite queues scripted outcomes; each Write consumes the next one.
func (s *Store[ID, E]) InjectWrite(faults ...WriteFault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeFaults = append(s.writeFaults, faults...)
}

// InjectLoadOne queues scripted outcomes; each LoadOne consumes the next one.
func (s *Store[ID, E]) InjectLoadOne(faults ...ReadFault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadOneFaults = append(s.loadOneFaults, faults...)
}

// InjectLoadAll queues scripted outcomes; each LoadAll consumes the next one.
func (s *Store[ID, E]) InjectLoadAll(faults ...ReadFault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadAllFaults = append(s.loadAllFaults, faults...)
}

func pop[F any](mu sync.Locker, q *[]F) (F, bool) {
	mu.Lock()
	defer mu.Unlock()
	var f F
	if len(*q) == 0 {
		return f, false
	}
	f, *q = (*q)[0], (*q)[1:]
	return f, true
}

// applyReadFault runs a scripted read outcome. It returns a non-nil error to return to
// the caller, or nil to proceed with the read.
func applyReadFault(ctx context.Context, f ReadFault) error {
	switch f.Kind {
	case FailRead:
		return orDefault(f.Err, ErrAmbiguous)
	case BlockRead:
		select {
		case <-f.Release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	case PanicRead:
		panic(f.PanicValue)
	}
	return nil
}

func orDefault(err, def error) error {
	if err != nil {
		return err
	}
	return def
}
