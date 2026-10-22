// SPDX-License-Identifier: Apache-2.0

package surecachetest

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nobelk/surecache"
	"github.com/nobelk/surecache/internal/memstore"
)

// team is a two-part entity: the head (Name, Lead, Hidden) and the tail (Members, Rev)
// are read by two statements, so a mixed snapshot is detectable.
type team struct {
	ID      string
	Name    string
	Lead    string
	Hidden  bool
	Members []string
	Rev     int
}

func cloneTeam(e team) team { e.Members = slices.Clone(e.Members); return e }

func joinTeam(head, tail team) team {
	head.Members, head.Rev = slices.Clone(tail.Members), tail.Rev
	return head
}

func leadKeys(e team) []any {
	if e.Lead == "" {
		return nil
	}
	return []any{e.Lead}
}

func memberKeys(e team) []any {
	out := make([]any, len(e.Members))
	for i, m := range e.Members {
		out[i] = m
	}
	return out
}

// bump changes both parts of a team.
func bump(e team) team {
	e = cloneTeam(e)
	e.Name += "+"
	e.Rev++
	return e
}

const teamCapacity = 10

var teamSpec = surecache.Spec[string, team]{
	Name:             "teams",
	Key:              func(e team) string { return e.ID },
	Clone:            cloneTeam,
	Cacheable:        func(e team) bool { return !e.Hidden },
	Compare:          strings.Compare,
	Capacity:         teamCapacity,
	CommittedNothing: memstore.CommittedNothing,
}

// variant breaks one part of the memstore harness, to prove a check catches it.
type variant struct {
	mixedSnapshot   bool                                                               // two-part reads without a common snapshot
	unboundedFence  bool                                                               // the fence wait never times out
	source          func(*memstore.Store[string, team]) surecache.Source[string, team] // wraps the Source
	partialCase     bool                                                               // a nothing-committed case that committed
	keyedHidden     bool                                                               // a non-cacheable fixture holds an index key
	exclusiveWrites bool                                                               // mutations block each other
}

func harness(v variant) Harness[string, team] {
	return Harness[string, team]{
		Spec:      teamSpec,
		IndexKeys: map[string]func(team) []any{"lead": leadKeys, "member": memberKeys},
		NewStore: func(testing.TB) Store[string, team] {
			fenceWait := 500 * time.Millisecond
			if v.unboundedFence {
				fenceWait = 0
			}
			s := memstore.New(memstore.Config[string, team]{
				Key: teamSpec.Key, Clone: cloneTeam, Cacheable: teamSpec.Cacheable,
				Capacity: teamCapacity, FenceWait: fenceWait,
				Join: joinTeam, MixedSnapshot: v.mixedSnapshot,
			})
			return &memStore{s: s, v: v, writeSlot: make(chan struct{}, 1)}
		},
	}
}

// memStore adapts internal/memstore to the conformance harness.
type memStore struct {
	s         *memstore.Store[string, team]
	v         variant
	writeSlot chan struct{} // exclusiveWrites: held by an open mutation
}

var _ SnapshotProbe = (*memStore)(nil)

func (m *memStore) Source() surecache.Source[string, team] {
	if m.v.source != nil {
		return m.v.source(m.s)
	}
	return m.s
}

func (m *memStore) SetReadPause(pause func()) { m.s.SetReadPause(pause) }

func (m *memStore) Seed(_ context.Context, fixtures []team) error {
	m.s.Seed(fixtures...)
	return nil
}

func (m *memStore) Fixtures() []team {
	hidden := team{ID: "t4", Name: "archived", Hidden: true}
	if m.v.keyedHidden {
		hidden.Lead = "L9"
	}
	return []team{
		{ID: "t1", Name: "alpha", Lead: "L1", Members: []string{"m1", "m2"}},
		{ID: "t2", Name: "beta", Members: []string{"m3"}},
		{ID: "t3", Name: "gamma"},
		hidden,
	}
}

func (m *memStore) NothingCommittedCases() []Case {
	write := func(fn func(tx *memstore.Tx[string, team]) error) func(context.Context) error {
		return func(ctx context.Context) error { return m.s.Write(ctx, fn) }
	}
	cases := []Case{
		{"not found", write(func(tx *memstore.Tx[string, team]) error {
			return tx.Update("zz", func(e team) (team, error) { return e, nil })
		})},
		{"exists", write(func(tx *memstore.Tx[string, team]) error { return tx.Insert(team{ID: "t1"}) })},
		{"precondition", write(func(tx *memstore.Tx[string, team]) error {
			return tx.Update("t1", func(e team) (team, error) { return e, memstore.ErrPrecondition })
		})},
	}
	if m.v.partialCase {
		cases = append(cases, Case{"partial", func(ctx context.Context) error {
			_ = m.s.Write(ctx, func(tx *memstore.Tx[string, team]) error {
				return tx.Update("t1", func(e team) (team, error) { return bump(e), nil })
			})
			return memstore.ErrPrecondition
		}})
	}
	return cases
}

func (m *memStore) AbsentID() string { return "zz" }

func (m *memStore) Dump(context.Context) (any, error) { return m.s.State(), nil }

func (m *memStore) Update(ctx context.Context, id string) (team, error) {
	var want team
	err := m.s.Write(ctx, func(tx *memstore.Tx[string, team]) error {
		return tx.Update(id, func(e team) (team, error) { want = bump(e); return want, nil })
	})
	return want, err
}

func (m *memStore) Delete(ctx context.Context, id string) error {
	return m.s.Write(ctx, func(tx *memstore.Tx[string, team]) error { return tx.Delete(id) })
}

func (m *memStore) BeginMutation(ctx context.Context, id string) (OpenMutation[team], error) {
	return m.begin(ctx, id)
}

func (m *memStore) BeginPairMutation(ctx context.Context, a, b string) (OpenPairMutation[team], error) {
	return m.begin(ctx, a, b)
}

var errRollback = errors.New("rollback")

// begin runs a Write that stages a bump of every id and then waits, holding the fence
// shared, for Commit or Rollback.
func (m *memStore) begin(ctx context.Context, ids ...string) (*openTx, error) {
	if m.v.exclusiveWrites {
		select {
		case m.writeSlot <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	o := &openTx{decide: make(chan bool), done: make(chan error, 1), staged: make(chan struct{})}
	go func() {
		o.done <- m.s.Write(ctx, func(tx *memstore.Tx[string, team]) error {
			for _, id := range ids {
				if err := tx.Update(id, func(e team) (team, error) {
					w := bump(e)
					o.want = append(o.want, w)
					return w, nil
				}); err != nil {
					return err
				}
			}
			close(o.staged)
			if !<-o.decide {
				return errRollback
			}
			return nil
		})
		if m.v.exclusiveWrites {
			<-m.writeSlot
		}
	}()
	select {
	case <-o.staged:
		return o, nil
	case err := <-o.done:
		return nil, err
	}
}

type openTx struct {
	want   []team
	decide chan bool
	done   chan error
	staged chan struct{}
}

func (o *openTx) Want() team            { return o.want[0] }
func (o *openTx) WantPair() (a, b team) { return o.want[0], o.want[1] }

func (o *openTx) Commit(context.Context) error {
	o.decide <- true
	return <-o.done
}

func (o *openTx) Rollback(context.Context) error {
	select {
	case o.decide <- false:
		if err := <-o.done; !errors.Is(err, errRollback) {
			return err
		}
	default: // already finished
	}
	return nil
}

func (m *memStore) SeedOverCapacity(context.Context) error {
	for i := range teamCapacity + 1 {
		m.s.Seed(team{ID: fmt.Sprintf("c%02d", i)})
	}
	return nil
}

func (m *memStore) FenceWaitBound() time.Duration { return 500 * time.Millisecond }

// The suite passes against the in-memory store.
func TestConformance_Memstore(t *testing.T) {
	Run(t, harness(variant{}))
}

// recorder is a testing.TB that records failures instead of failing the real test.
type recorder struct {
	testing.TB
	mu      sync.Mutex
	failed  bool
	skipped bool
	log     []string
}

func (r *recorder) Helper() {}

func (r *recorder) record(failed bool, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = r.failed || failed
	r.log = append(r.log, fmt.Sprint(args...))
}

func (r *recorder) Error(args ...any)                 { r.record(true, args...) }
func (r *recorder) Errorf(format string, args ...any) { r.record(true, fmt.Sprintf(format, args...)) }
func (r *recorder) Fatal(args ...any)                 { r.record(true, args...); runtime.Goexit() }
func (r *recorder) Fatalf(format string, args ...any) {
	r.record(true, fmt.Sprintf(format, args...))
	runtime.Goexit()
}
func (r *recorder) Fail()    { r.record(true) }
func (r *recorder) FailNow() { r.record(true); runtime.Goexit() }
func (r *recorder) Failed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failed
}
func (r *recorder) Log(args ...any)                 { r.record(false, args...) }
func (r *recorder) Logf(format string, args ...any) { r.record(false, fmt.Sprintf(format, args...)) }
func (r *recorder) Skip(...any)                     { r.skipped = true; runtime.Goexit() }
func (r *recorder) Skipf(string, ...any)            { r.skipped = true; runtime.Goexit() }
func (r *recorder) SkipNow()                        { r.skipped = true; runtime.Goexit() }

// runRecorded runs fn on its own goroutine with a recorder, so Fatal can stop it.
func runRecorded(t *testing.T, fn func(testing.TB)) *recorder {
	r := &recorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(r)
	}()
	<-done
	return r
}

// brokenSource wraps the memstore Source to break one contract rule.
type brokenSource struct {
	*memstore.Store[string, team]
	loadOne  func(ctx context.Context, id string) (team, error)
	loadAll  func(ctx context.Context) ([]team, error)
	notFound func(error) bool
}

func (b brokenSource) LoadOne(ctx context.Context, id string) (team, error) {
	if b.loadOne != nil {
		return b.loadOne(ctx, id)
	}
	return b.Store.LoadOne(ctx, id)
}

func (b brokenSource) LoadAll(ctx context.Context) ([]team, error) {
	if b.loadAll != nil {
		return b.loadAll(ctx)
	}
	return b.Store.LoadAll(ctx)
}

func (b brokenSource) NotFound(err error) bool {
	if b.notFound != nil {
		return b.notFound(err)
	}
	return b.Store.NotFound(err)
}

func wrap(fn func(s *memstore.Store[string, team]) brokenSource) variant {
	return variant{source: func(s *memstore.Store[string, team]) surecache.Source[string, team] { return fn(s) }}
}

// Each check fails against a store that breaks the rule it checks.
func TestSuite_CatchesBrokenStores(t *testing.T) {
	cases := []struct {
		check   string
		variant variant
		spec    func(*surecache.Spec[string, team])
	}{
		{check: "NothingCommittedLeavesStoreUnchanged/partial", variant: variant{partialCase: true}},
		{check: "ContextErrorsAreNotNothingCommitted", spec: func(s *surecache.Spec[string, team]) {
			s.CommittedNothing = func(err error) bool {
				return memstore.CommittedNothing(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
			}
		}},
		{check: "LoadOneReadsLatestCommit", variant: wrap(func(s *memstore.Store[string, team]) brokenSource {
			stale := map[string]team{}
			for id, e := range s.State() {
				stale[id] = e
			}
			return brokenSource{Store: s, loadOne: func(context.Context, string) (team, error) { return stale["t1"], nil }}
		})},
		{check: "LoadOneKeyMatches", variant: wrap(func(s *memstore.Store[string, team]) brokenSource {
			return brokenSource{Store: s, loadOne: func(ctx context.Context, id string) (team, error) {
				e, err := s.LoadOne(ctx, id)
				e.ID += "x"
				return e, err
			}}
		})},
		{check: "LoadOneNotFoundMeansAbsent", variant: wrap(func(s *memstore.Store[string, team]) brokenSource {
			return brokenSource{Store: s, notFound: func(error) bool { return false }}
		})},
		{check: "LoadOneConsistentSnapshot", variant: variant{mixedSnapshot: true}},
		{check: "LoadAllConsistentSnapshot", variant: variant{mixedSnapshot: true}},
		{check: "LoadOneNonCacheable", variant: wrap(func(s *memstore.Store[string, team]) brokenSource {
			return brokenSource{Store: s, loadOne: func(ctx context.Context, id string) (team, error) {
				e, err := s.LoadOne(ctx, id)
				if err == nil && e.Hidden {
					return team{}, memstore.ErrNotFound
				}
				return e, err
			}}
		})},
		{check: "LoadAllEachKeyOnce", variant: wrap(func(s *memstore.Store[string, team]) brokenSource {
			return brokenSource{Store: s, loadAll: func(ctx context.Context) ([]team, error) {
				all, err := s.LoadAll(ctx)
				return append(all, all[0]), err
			}}
		})},
		{check: "LoadAllCacheableParity", variant: wrap(func(s *memstore.Store[string, team]) brokenSource {
			return brokenSource{Store: s, loadAll: func(ctx context.Context) ([]team, error) {
				all, err := s.LoadAll(ctx)
				hidden, _ := s.Get("t4")
				return append(all, hidden), err
			}}
		})},
		{check: "IndexParity", variant: variant{keyedHidden: true}},
		{check: "LoadAllRefusesOverCapacity", variant: wrap(func(s *memstore.Store[string, team]) brokenSource {
			return brokenSource{Store: s, loadAll: func(ctx context.Context) ([]team, error) {
				all, err := s.LoadAll(ctx)
				if errors.Is(err, memstore.ErrOverCapacity) {
					var subset []team
					for _, e := range s.State() {
						subset = append(subset, e)
					}
					return subset[:teamCapacity], nil
				}
				return all, err
			}}
		})},
		{check: "LoadAllContextErrorsUnwrapped", variant: wrap(func(s *memstore.Store[string, team]) brokenSource {
			return brokenSource{Store: s, loadAll: func(ctx context.Context) ([]team, error) {
				all, err := s.LoadAll(ctx)
				if err != nil {
					return nil, fmt.Errorf("load: %w", err)
				}
				return all, nil
			}}
		})},
		{check: "FenceOrdersBulkLoad/commit", variant: wrap(func(s *memstore.Store[string, team]) brokenSource {
			return brokenSource{Store: s, loadAll: func(context.Context) ([]team, error) {
				var all []team
				for _, e := range s.State() {
					if !e.Hidden {
						all = append(all, e)
					}
				}
				return all, nil // no fence
			}}
		})},
		{check: "FenceWaitTimesOut", variant: variant{unboundedFence: true}},
		{check: "MutationsDoNotBlockEachOther", variant: variant{exclusiveWrites: true}},
	}
	for _, tc := range cases {
		t.Run(tc.check, func(t *testing.T) {
			h := harness(tc.variant)
			if tc.spec != nil {
				tc.spec(&h.Spec)
			}
			s := suite[string, team]{h: h}
			fn := s.byName(tc.check)
			if name, ok := strings.CutPrefix(tc.check, "NothingCommittedLeavesStoreUnchanged/"); ok {
				fn = func(tb testing.TB) { s.nothingCommitted(tb, name) }
			}
			if fn == nil {
				t.Fatalf("no check %q", tc.check)
			}
			r := runRecorded(t, fn)
			if !r.Failed() || r.skipped {
				t.Fatalf("check passed against a broken store (skipped=%t): %v", r.skipped, r.log)
			}
			t.Logf("caught: %v", r.log)
		})
	}
}

// A store without SnapshotProbe skips the snapshot checks with the documented reason.
func TestSuite_SkipsSnapshotChecksWithoutProbe(t *testing.T) {
	h := harness(variant{})
	newStore := h.NewStore
	h.NewStore = func(tb testing.TB) Store[string, team] { return noProbe{newStore(tb)} }
	s := suite[string, team]{h: h}
	for _, name := range []string{"LoadOneConsistentSnapshot", "LoadAllConsistentSnapshot"} {
		r := runRecorded(t, s.byName(name))
		if !r.skipped || r.Failed() {
			t.Fatalf("%s: skipped=%t failed=%t %v", name, r.skipped, r.Failed(), r.log)
		}
	}
}

type noProbe struct{ Store[string, team] }
