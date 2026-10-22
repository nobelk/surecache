// SPDX-License-Identifier: Apache-2.0

package surecachetest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// Real-time bounds for the fence checks. Adapters talk to real databases, so the slack
// is generous; these are the only real-time tests in the repository.
const (
	// blockWindow is how long a check waits to see that a load is still blocked.
	blockWindow = 300 * time.Millisecond
	// slack is added to every real-time bound.
	slack = 2 * time.Second
)

// Run runs every conformance check against h as subtests with stable names, so an
// adapter can report and skip precisely.
func Run[ID comparable, E any](t *testing.T, h Harness[ID, E]) {
	t.Helper()
	s := suite[ID, E]{h: h}
	t.Run("NothingCommittedLeavesStoreUnchanged", func(t *testing.T) {
		for _, tc := range s.seeded(t).NothingCommittedCases() {
			t.Run(tc.Name, func(t *testing.T) { s.nothingCommitted(t, tc.Name) })
		}
	})
	for _, c := range s.checks() {
		if len(c.subs) == 0 {
			t.Run(c.name, func(t *testing.T) { c.fn(t) })
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			for _, sub := range c.subs {
				t.Run(sub.name, func(t *testing.T) { sub.fn(t) })
			}
		})
	}
}

type check struct {
	name string
	fn   func(t testing.TB)
	subs []check
}

type suite[ID comparable, E any] struct {
	h Harness[ID, E]
}

func (s suite[ID, E]) checks() []check {
	return []check{
		{name: "ContextErrorsAreNotNothingCommitted", fn: s.contextErrorsAreNotNothingCommitted},
		{name: "LoadOneReadsLatestCommit", fn: s.loadOneReadsLatestCommit},
		{name: "LoadOneKeyMatches", fn: s.loadOneKeyMatches},
		{name: "LoadOneNotFoundMeansAbsent", fn: s.loadOneNotFoundMeansAbsent},
		{name: "LoadOneConsistentSnapshot", fn: s.loadOneConsistentSnapshot},
		{name: "LoadAllConsistentSnapshot", fn: s.loadAllConsistentSnapshot},
		{name: "LoadOneNonCacheable", fn: s.loadOneNonCacheable},
		{name: "LoadAllEachKeyOnce", fn: s.loadAllEachKeyOnce},
		{name: "LoadAllCacheableParity", fn: s.loadAllCacheableParity},
		{name: "IndexParity", fn: s.indexParity},
		{name: "LoadAllRefusesOverCapacity", fn: s.loadAllRefusesOverCapacity},
		{name: "LoadAllContextErrorsUnwrapped", fn: s.loadAllContextErrorsUnwrapped},
		{name: "FenceOrdersBulkLoad", subs: []check{
			{name: "commit", fn: func(t testing.TB) { s.fenceOrdersBulkLoad(t, true) }},
			{name: "rollback", fn: func(t testing.TB) { s.fenceOrdersBulkLoad(t, false) }},
		}},
		{name: "FenceWaitTimesOut", fn: s.fenceWaitTimesOut},
		{name: "MutationsDoNotBlockEachOther", fn: s.mutationsDoNotBlockEachOther},
	}
}

// byName returns the check with the given name; a sub-check is named "parent/sub".
func (s suite[ID, E]) byName(name string) func(t testing.TB) {
	for _, c := range s.checks() {
		if c.name == name {
			return c.fn
		}
		for _, sub := range c.subs {
			if c.name+"/"+sub.name == name {
				return sub.fn
			}
		}
	}
	return nil
}

func (s suite[ID, E]) equal(a, b E) bool {
	if s.h.Equal != nil {
		return s.h.Equal(a, b)
	}
	return reflect.DeepEqual(a, b)
}

// seeded returns a fresh store holding the fixtures.
func (s suite[ID, E]) seeded(t testing.TB) Store[ID, E] {
	t.Helper()
	st := s.h.NewStore(t)
	if err := st.Seed(context.Background(), st.Fixtures()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	return st
}

// cacheableIDs returns the ids of the cacheable fixtures, failing below n.
func (s suite[ID, E]) cacheableIDs(t testing.TB, st Store[ID, E], n int) []ID {
	t.Helper()
	var ids []ID
	for _, f := range st.Fixtures() {
		if s.h.Spec.Cacheable(f) {
			ids = append(ids, s.h.Spec.Key(f))
		}
	}
	if len(ids) < n {
		t.Fatalf("the fixtures need at least %d cacheable entities, have %d", n, len(ids))
	}
	return ids
}

func ctxTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// Section 4.2: each declared nothing-committed error leaves the store unchanged.
func (s suite[ID, E]) nothingCommitted(t testing.TB, name string) {
	st := s.seeded(t)
	var op func(context.Context) error
	for _, tc := range st.NothingCommittedCases() {
		if tc.Name == name {
			op = tc.Op
		}
	}
	if op == nil {
		t.Fatalf("no nothing-committed case %q", name)
	}
	ctx := context.Background()
	before, err := st.Dump(ctx)
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	opErr := op(ctx)
	if opErr == nil || !s.h.Spec.CommittedNothing(opErr) {
		t.Fatalf("case returned %v; want an error for which CommittedNothing is true", opErr)
	}
	after, err := st.Dump(ctx)
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("store changed after a nothing-committed error:\nbefore %v\nafter  %v", before, after)
	}
}

// Section 4.2: a context error is never a nothing-committed error, whether the context
// ended before the call or while the mutation waited on the fence.
func (s suite[ID, E]) contextErrorsAreNotNothingCommitted(t testing.TB) {
	st := s.seeded(t)
	ids := s.cacheableIDs(t, st, 2)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.Update(cancelled, ids[0]); err == nil || s.h.Spec.CommittedNothing(err) {
		t.Errorf("Update with a cancelled context = %v; want an error that is not nothing-committed", err)
	}

	// An open mutation holds the fence shared; a waiting bulk load queues for it
	// exclusively, and a new mutation then waits behind that request.
	open, err := st.BeginMutation(context.Background(), ids[0])
	if err != nil {
		t.Fatalf("BeginMutation: %v", err)
	}
	loadCtx, cancelLoad := ctxTimeout(st.FenceWaitBound() + slack)
	defer cancelLoad()
	loaded := make(chan struct{})
	go func() {
		defer close(loaded)
		_, _ = st.Source().LoadAll(loadCtx)
	}()
	// Nothing shows when the bulk load has queued, so retry until an Update is cut
	// short; an Update that got through before the queue formed committed normally.
	quarter := min(st.FenceWaitBound()/4, blockWindow)
	observed := false
	for deadline := time.Now().Add(st.FenceWaitBound() / 2); !observed && time.Now().Before(deadline); {
		upCtx, cancelUp := ctxTimeout(quarter)
		_, err = st.Update(upCtx, ids[1])
		cancelUp()
		if err != nil {
			observed = true
			if s.h.Spec.CommittedNothing(err) {
				t.Errorf("Update cancelled while waiting on the fence = %v; want an error that is not nothing-committed", err)
			}
		}
	}
	if rerr := open.Rollback(context.Background()); rerr != nil {
		t.Errorf("Rollback: %v", rerr)
	}
	<-loaded
	if !observed {
		t.Skip("no Update ever waited on the fence behind the bulk load; the store does not queue shared requests behind an exclusive one")
	}
}

// Section 4.4: LoadOne includes every commit completed before the call.
func (s suite[ID, E]) loadOneReadsLatestCommit(t testing.TB) {
	st := s.seeded(t)
	ctx := context.Background()
	for _, id := range s.cacheableIDs(t, st, 1) {
		want, err := st.Update(ctx, id)
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		got, err := st.Source().LoadOne(ctx, id)
		if err != nil || !s.equal(got, want) {
			t.Fatalf("LoadOne after Update = %v, %v; want %v", got, err, want)
		}
	}
}

// Section 4.4: a successful LoadOne satisfies Key(e) == id.
func (s suite[ID, E]) loadOneKeyMatches(t testing.TB) {
	st := s.seeded(t)
	for _, f := range st.Fixtures() {
		id := s.h.Spec.Key(f)
		got, err := st.Source().LoadOne(context.Background(), id)
		if err != nil {
			t.Fatalf("LoadOne(%v): %v", id, err)
		}
		if k := s.h.Spec.Key(got); k != id {
			t.Fatalf("LoadOne(%v) returned key %v", id, k)
		}
	}
}

// Section 4.4: not-found means the entity authoritatively does not exist.
func (s suite[ID, E]) loadOneNotFoundMeansAbsent(t testing.TB) {
	st := s.seeded(t)
	src := st.Source()
	ctx := context.Background()
	if _, err := src.LoadOne(ctx, st.AbsentID()); err == nil || !src.NotFound(err) {
		t.Errorf("LoadOne(absent) = %v; want a not-found error", err)
	}
	id := s.cacheableIDs(t, st, 1)[0]
	if err := st.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := src.LoadOne(ctx, id); err == nil || !src.NotFound(err) {
		t.Errorf("LoadOne after Delete = %v; want a not-found error", err)
	}
	if src.NotFound(context.Canceled) {
		t.Errorf("NotFound(context.Canceled) is true; a context error never means absent")
	}
}

// probe returns the store's SnapshotProbe, or skips when the store documents that its
// loads are single statements.
func probe[ID comparable, E any](t testing.TB, st Store[ID, E]) SnapshotProbe {
	t.Helper()
	p, ok := st.(SnapshotProbe)
	if !ok {
		t.Skip("store does not implement SnapshotProbe; it must document that LoadOne and LoadAll each run exactly one statement")
	}
	return p
}

// Section 4.4: LoadOne is one consistent snapshot. A mutation that commits at any
// statement boundary yields the whole before state or the whole after state.
func (s suite[ID, E]) loadOneConsistentSnapshot(t testing.TB) {
	st := s.seeded(t)
	p := probe(t, st)
	pauses := 0
	p.SetReadPause(func() { pauses++ })
	if _, err := st.Source().LoadOne(context.Background(), s.cacheableIDs(t, st, 1)[0]); err != nil {
		t.Fatalf("LoadOne: %v", err)
	}
	p.SetReadPause(nil)
	for at := 1; at <= pauses; at++ {
		s.loadOneCommitAt(t, at)
	}
}

func (s suite[ID, E]) loadOneCommitAt(t testing.TB, at int) {
	st := s.seeded(t)
	p := probe(t, st)
	ctx := context.Background()
	id := s.cacheableIDs(t, st, 1)[0]
	before, err := st.Source().LoadOne(ctx, id)
	if err != nil {
		t.Fatalf("LoadOne: %v", err)
	}
	open, err := st.BeginMutation(ctx, id)
	if err != nil {
		t.Fatalf("BeginMutation: %v", err)
	}
	defer func() { _ = open.Rollback(ctx) }()
	n := 0
	p.SetReadPause(func() {
		if n++; n == at {
			if cerr := open.Commit(ctx); cerr != nil {
				t.Errorf("Commit: %v", cerr)
			}
		}
	})
	defer p.SetReadPause(nil)
	got, err := st.Source().LoadOne(ctx, id)
	if err != nil {
		t.Fatalf("LoadOne: %v", err)
	}
	if !s.equal(got, before) && !s.equal(got, open.Want()) {
		t.Fatalf("commit at pause %d: LoadOne mixed two snapshots: got %v, before %v, after %v", at, got, before, open.Want())
	}
}

// Section 4.5: LoadAll is one whole-store snapshot. A pair mutation that commits at any
// statement boundary of the read yields both old values or both new ones, never one of
// each. The pair begins and commits inside the pause: the bulk load has already passed
// the fence, so the pair's shared fence is free.
func (s suite[ID, E]) loadAllConsistentSnapshot(t testing.TB) {
	st := s.seeded(t)
	p := probe(t, st)
	pauses := 0
	p.SetReadPause(func() { pauses++ })
	if _, err := st.Source().LoadAll(context.Background()); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	p.SetReadPause(nil)
	for at := 1; at <= pauses; at++ {
		s.loadAllPairAt(t, at)
	}
}

func (s suite[ID, E]) loadAllPairAt(t testing.TB, at int) {
	st := s.seeded(t)
	p := probe(t, st)
	ctx := context.Background()
	ids := s.cacheableIDs(t, st, 2)
	a, b := ids[0], ids[1]
	beforeA, errA := st.Source().LoadOne(ctx, a)
	beforeB, errB := st.Source().LoadOne(ctx, b)
	if errA != nil || errB != nil {
		t.Fatalf("LoadOne: %v, %v", errA, errB)
	}
	var wantA, wantB E
	n := 0
	p.SetReadPause(func() {
		if n++; n != at {
			return
		}
		pair, err := st.BeginPairMutation(ctx, a, b)
		if err != nil {
			t.Errorf("BeginPairMutation: %v", err)
			return
		}
		wantA, wantB = pair.WantPair()
		if err := pair.Commit(ctx); err != nil {
			t.Errorf("Commit: %v", err)
		}
	})
	defer p.SetReadPause(nil)
	all, err := st.Source().LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	var gotA, gotB E
	for _, e := range all {
		switch s.h.Spec.Key(e) {
		case a:
			gotA = e
		case b:
			gotB = e
		}
	}
	old := s.equal(gotA, beforeA) && s.equal(gotB, beforeB)
	updated := s.equal(gotA, wantA) && s.equal(gotB, wantB)
	if !old && !updated {
		t.Fatalf("commit at pause %d: LoadAll mixed snapshots: a=%v b=%v; before %v %v; after %v %v",
			at, gotA, gotB, beforeA, beforeB, wantA, wantB)
	}
}

// Section 4.4: LoadOne returns non-cacheable entities too.
func (s suite[ID, E]) loadOneNonCacheable(t testing.TB) {
	st := s.seeded(t)
	for _, f := range st.Fixtures() {
		if s.h.Spec.Cacheable(f) {
			continue
		}
		got, err := st.Source().LoadOne(context.Background(), s.h.Spec.Key(f))
		if err != nil || !s.equal(got, f) {
			t.Fatalf("LoadOne(non-cacheable %v) = %v, %v", s.h.Spec.Key(f), got, err)
		}
	}
}

// Section 4.5: LoadAll returns each primary key exactly once.
func (s suite[ID, E]) loadAllEachKeyOnce(t testing.TB) {
	st := s.seeded(t)
	all, err := st.Source().LoadAll(context.Background())
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	seen := map[ID]bool{}
	for _, e := range all {
		id := s.h.Spec.Key(e)
		if seen[id] {
			t.Fatalf("LoadAll returned %v twice", id)
		}
		seen[id] = true
	}
}

// Section 4.5: LoadAll returns exactly the cacheable fixtures (predicate parity).
func (s suite[ID, E]) loadAllCacheableParity(t testing.TB) {
	st := s.seeded(t)
	all, err := st.Source().LoadAll(context.Background())
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	want := map[ID]E{}
	for _, f := range st.Fixtures() {
		if s.h.Spec.Cacheable(f) {
			want[s.h.Spec.Key(f)] = f
		}
	}
	if len(all) != len(want) {
		t.Fatalf("LoadAll returned %d entities, want %d cacheable fixtures", len(all), len(want))
	}
	for _, e := range all {
		f, ok := want[s.h.Spec.Key(e)]
		if !ok || !s.h.Spec.Cacheable(e) || !s.equal(e, f) {
			t.Fatalf("LoadAll returned %v, not a cacheable fixture", e)
		}
	}
}

// PRE-2, PRE-3: no non-cacheable fixture yields an index key, no two cacheable fixtures
// share a key, and the fixtures cover every entity shape.
func (s suite[ID, E]) indexParity(t testing.TB) {
	st := s.h.NewStore(t)
	var withKeys, withoutKeys, nonCacheable bool
	for name, keys := range s.h.IndexKeys {
		holders := map[any]ID{}
		for _, f := range st.Fixtures() {
			ks := keys(f)
			if !s.h.Spec.Cacheable(f) {
				if len(ks) > 0 {
					t.Errorf("non-cacheable fixture %v holds keys %v of index %s (PRE-3)", s.h.Spec.Key(f), ks, name)
				}
				continue
			}
			for _, k := range ks {
				if h, dup := holders[k]; dup && h != s.h.Spec.Key(f) {
					t.Errorf("key %v of index %s held by %v and %v (PRE-2)", k, name, h, s.h.Spec.Key(f))
				}
				holders[k] = s.h.Spec.Key(f)
			}
		}
	}
	for _, f := range st.Fixtures() {
		switch {
		case !s.h.Spec.Cacheable(f):
			nonCacheable = true
		case s.holdsKeys(f):
			withKeys = true
		default:
			withoutKeys = true
		}
	}
	if !withKeys || !withoutKeys || !nonCacheable {
		t.Errorf("fixtures miss an entity shape: cacheable with keys=%t, cacheable without keys=%t, non-cacheable=%t",
			withKeys, withoutKeys, nonCacheable)
	}
}

func (s suite[ID, E]) holdsKeys(e E) bool {
	for _, keys := range s.h.IndexKeys {
		if len(keys(e)) > 0 {
			return true
		}
	}
	return false
}

// SAFE-8, section 4.5: over capacity LoadAll fails with no entities, never a subset.
func (s suite[ID, E]) loadAllRefusesOverCapacity(t testing.TB) {
	st := s.h.NewStore(t)
	if err := st.SeedOverCapacity(context.Background()); err != nil {
		t.Fatalf("SeedOverCapacity: %v", err)
	}
	all, err := st.Source().LoadAll(context.Background())
	if err == nil || len(all) != 0 {
		t.Fatalf("LoadAll over capacity = %d entities, %v; want an error and none", len(all), err)
	}
}

// Section 4.5: context errors come back unwrapped (==, not merely errors.Is).
func (s suite[ID, E]) loadAllContextErrorsUnwrapped(t testing.TB) {
	st := s.seeded(t)
	src := st.Source()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if all, err := src.LoadAll(cancelled); err != context.Canceled || len(all) != 0 { //nolint:errorlint // the contract requires ==
		t.Errorf("LoadAll(cancelled) = %d, %v; want == context.Canceled", len(all), err)
	}
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	if all, err := src.LoadAll(expired); err != context.DeadlineExceeded || len(all) != 0 { //nolint:errorlint // the contract requires ==
		t.Errorf("LoadAll(expired) = %d, %v; want == context.DeadlineExceeded", len(all), err)
	}
	open, err := st.BeginMutation(context.Background(), s.cacheableIDs(t, st, 1)[0])
	if err != nil {
		t.Fatalf("BeginMutation: %v", err)
	}
	defer func() { _ = open.Rollback(context.Background()) }()
	waiting, cancelWaiting := context.WithCancel(context.Background())
	time.AfterFunc(min(st.FenceWaitBound()/3, blockWindow), cancelWaiting)
	if all, err := src.LoadAll(waiting); err != context.Canceled || len(all) != 0 { //nolint:errorlint // the contract requires ==
		t.Errorf("LoadAll cancelled while waiting on the fence = %d, %v; want == context.Canceled", len(all), err)
	}
}

// Section 4.3: an open mutation blocks LoadAll, which then returns the committed change,
// or the unchanged state after a rollback.
func (s suite[ID, E]) fenceOrdersBulkLoad(t testing.TB, commit bool) {
	st := s.seeded(t)
	ctx := context.Background()
	id := s.cacheableIDs(t, st, 1)[0]
	before, err := st.Source().LoadOne(ctx, id)
	if err != nil {
		t.Fatalf("LoadOne: %v", err)
	}
	open, err := st.BeginMutation(ctx, id)
	if err != nil {
		t.Fatalf("BeginMutation: %v", err)
	}
	type result struct {
		all []E
		err error
	}
	done := make(chan result, 1)
	go func() {
		all, lerr := st.Source().LoadAll(ctx)
		done <- result{all, lerr}
	}()
	select {
	case r := <-done:
		_ = open.Rollback(ctx)
		t.Fatalf("LoadAll returned while a mutation held the fence: %d entities, %v", len(r.all), r.err)
	case <-time.After(min(blockWindow, st.FenceWaitBound()/3)):
	}
	want := before
	if commit {
		want = open.Want()
		err = open.Commit(ctx)
	} else {
		err = open.Rollback(ctx)
	}
	if err != nil {
		t.Fatalf("finishing the mutation: %v", err)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("LoadAll: %v", r.err)
	}
	for _, e := range r.all {
		if s.h.Spec.Key(e) == id {
			if !s.equal(e, want) {
				t.Fatalf("LoadAll after the mutation finished returned %v, want %v", e, want)
			}
			return
		}
	}
	t.Fatalf("LoadAll lost %v", id)
}

// Section 4.6: with a mutation held open past the bound, LoadAll fails within it.
func (s suite[ID, E]) fenceWaitTimesOut(t testing.TB) {
	st := s.seeded(t)
	ctx := context.Background()
	open, err := st.BeginMutation(ctx, s.cacheableIDs(t, st, 1)[0])
	if err != nil {
		t.Fatalf("BeginMutation: %v", err)
	}
	defer func() { _ = open.Rollback(ctx) }()
	bound := st.FenceWaitBound() + slack
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := st.Source().LoadAll(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("LoadAll succeeded while a mutation held the fence")
		}
		if errors.Is(err, context.Canceled) {
			t.Fatalf("LoadAll returned %v without a cancelled context", err)
		}
	case <-time.After(bound):
		t.Fatalf("LoadAll still waiting after %v; the fence wait bound is %v", time.Since(start), st.FenceWaitBound())
	}
}

// Section 4.3: mutations hold the fence shared, so two open mutations coexist.
func (s suite[ID, E]) mutationsDoNotBlockEachOther(t testing.TB) {
	st := s.seeded(t)
	ctx := context.Background()
	ids := s.cacheableIDs(t, st, 2)
	first, err := st.BeginMutation(ctx, ids[0])
	if err != nil {
		t.Fatalf("BeginMutation: %v", err)
	}
	defer func() { _ = first.Rollback(ctx) }()
	secondCtx, cancel := ctxTimeout(slack)
	defer cancel()
	second, err := st.BeginMutation(secondCtx, ids[1])
	if err != nil {
		t.Fatalf("a second mutation could not start while the first was open: %v", err)
	}
	if err := second.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}
