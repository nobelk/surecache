// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nobelk/surecache/internal/memstore"
	"github.com/nobelk/surecache/internal/model"
)

var (
	modelSeed   = flag.Uint64("seed", 0, "run TestModel_Linearizable with only this seed")
	modelBudget = flag.Duration("model.budget", 300*time.Millisecond, "wall time per model run")
	modelSeeds  = flag.Int("model.seeds", 1, "random seeds per model test, besides the fixed ones")
)

// TestModel_Linearizable drives random concurrent histories (readers, mutators with
// injected faults, the rebuild loop) and checks every answer against the store's commit
// log: SAFE-1 and SAFE-3 through committed-state linearizability, SAFE-2 through required
// deferrals inside mutation windows. A failing seed reproduces with
// -run TestModel_Linearizable -seed=<n>.
func TestModel_Linearizable(t *testing.T) {
	for _, seed := range modelSeedList() {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			runModel(t, seed, false)
		})
	}
}

// TestModel_CloneIsolation is the model test with readers that mutate every value they
// get back (SAFE-10). The checker must still pass.
func TestModel_CloneIsolation(t *testing.T) {
	for _, seed := range modelSeedList() {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			runModel(t, seed, true)
		})
	}
}

func modelSeedList() []uint64 {
	if *modelSeed != 0 {
		return []uint64{*modelSeed}
	}
	seeds := []uint64{1, 2}
	for range *modelSeeds {
		seeds = append(seeds, rand.Uint64()|1)
	}
	return seeds
}

const (
	// minQuietRatio is the share of reads a trusted, idle cache must answer. Gets of ids
	// that do not exist always defer, so it is well below 1.
	minQuietRatio = 0.5
	minQuietReads = 20

	modelIDs     = 8
	modelMembers = 6
	modelOwners  = 4
	modelReaders = 4
	modelWriters = 3
)

func modelID(i int) string     { return fmt.Sprintf("g%d", i) }
func modelMember(i int) string { return fmt.Sprintf("k%d", i) }
func modelOwner(i int) string  { return fmt.Sprintf("o%d", i) }

// modelRun is one randomized run and its recorded history.
type modelRun struct {
	t      *testing.T
	f      *fixture
	clock  atomic.Int64
	mu     sync.Mutex
	reads  []model.Read[string, group]
	wins   []model.Window[string]
	mutate bool // readers mutate returned values

	releaseMu sync.Mutex
	releases  []chan struct{}

	eventsMu sync.Mutex
	events   []stamped

	quiet [][2]int64 // trusted idle windows on the logical clock

	// idle is open during an idle phase, when writers wait, so the loop can restore
	// trust and readers get answers to check; it is closed when the phase ends.
	idle atomic.Pointer[chan struct{}]
}

type windowKey struct{}

func runModel(t *testing.T, seed uint64, mutateReturned bool) {
	t.Logf("seed %d (reproduce with -run %s -seed=%d)", seed, t.Name(), seed)
	m := &modelRun{t: t, mutate: mutateReturned}
	m.f = newFixture(t, concurrent(), withOptions(WithRebuildInterval(2*time.Millisecond), WithObserver(modelObserver{m: m})))
	m.f.c.hooks = &hooks{
		afterMark: func(ctx context.Context, _ any) {
			if w, ok := ctx.Value(windowKey{}).(*model.Window[string]); ok {
				w.S0 = m.clock.Add(1)
			}
		},
		beforeRelease: func(ctx context.Context, _ any) {
			if w, ok := ctx.Value(windowKey{}).(*model.Window[string]); ok {
				w.S1 = m.clock.Add(1)
				m.mu.Lock()
				m.wins = append(m.wins, *w)
				m.mu.Unlock()
			}
		},
	}
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	m.f.prime(m.initialState(rng)...)
	base, baseVer := m.f.store.RecordHistory()

	runCtx, stopRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- m.f.c.Run(runCtx) }()

	deadline := time.Now().Add(*modelBudget)
	var wg sync.WaitGroup
	wg.Go(func() { m.phases(deadline) })
	for i := range modelReaders {
		r := rand.New(rand.NewPCG(seed, uint64(i)))
		wg.Go(func() { m.reader(r, deadline) })
	}
	for i := range modelWriters {
		r := rand.New(rand.NewPCG(seed, uint64(100+i)))
		wg.Go(func() { m.writer(r, deadline) })
	}
	wg.Wait()
	m.releaseAll()

	m.awaitTrusted()
	m.checkQuiescent()
	stopRun()
	if err := <-runDone; err != nil {
		t.Fatalf("Run = %v", err)
	}

	commits := m.f.store.History()
	h := model.History[string, group]{Base: base, BaseVer: baseVer, Reads: m.reads, Windows: m.wins}
	for _, c := range commits {
		h.Commits = append(h.Commits, model.Commit[string, group]{Version: c.Version, Puts: c.Puts, Dels: c.Dels})
	}
	violations, st := model.Check(modelSpec, h)
	for i, v := range violations {
		if i == 20 {
			t.Errorf("... %d more", len(violations)-20)
			break
		}
		t.Error(v)
	}
	if len(violations) > 0 {
		m.diagnose(h)
	}
	m.checkQuietAnswers()
	t.Logf("reads=%d answered=%d deferred=%d commits=%d windows=%d", st.Reads, st.Answered, st.Deferred, len(commits), len(m.wins))
}

var modelSpec = model.Spec[string, group]{
	Key:       groupKey,
	Cacheable: groupCacheable,
	Indexes: map[string]func(group) []any{
		"member": func(e group) []any { return anys(e.Members) },
		"owner":  func(e group) []any { return anys(ownerKeys(e.Owner)) },
	},
}

func anys(keys []string) []any {
	out := make([]any, len(keys))
	for i, k := range keys {
		out[i] = k
	}
	return out
}

// initialState returns entities with unique keys over part of the id space.
func (m *modelRun) initialState(rng *rand.Rand) []group {
	var out []group
	members := rng.Perm(modelMembers)
	owners := rng.Perm(modelOwners)
	for i := range modelIDs / 2 {
		e := g(modelID(i), "")
		if i < len(owners) && rng.IntN(2) == 0 {
			e.Owner = modelOwner(owners[i])
		}
		if i < len(members) {
			e.Members = []string{modelMember(members[i])}
		}
		out = append(out, e)
	}
	return out
}

// reader issues random reads until the deadline and records each with its versions and
// logical-clock stamps.
func (m *modelRun) reader(rng *rand.Rand, deadline time.Time) {
	for time.Now().Before(deadline) {
		r := model.Read[string, group]{S0: m.clock.Add(1), V0: m.f.store.Version()}
		switch rng.IntN(4) {
		case 0, 1:
			r.Op, r.ID = model.Get, modelID(rng.IntN(modelIDs))
			e, a := m.f.c.Get(r.ID)
			r.Answer, r.Value = model.Answer(a), cloneGroup(e)
			if m.mutate && a == Found {
				e.Name = "mutated by reader"
				if len(e.Members) > 0 {
					e.Members[0] = "mutated"
				}
			}
		case 2:
			r.Op = model.Lookup
			var id string
			var a Answer
			if rng.IntN(2) == 0 {
				r.Index, r.Key = "member", modelMember(rng.IntN(modelMembers))
				id, a, _ = Lookup(m.f.c, m.f.byMember, r.Key.(string))
			} else {
				r.Index, r.Key = "owner", modelOwner(rng.IntN(modelOwners))
				id, a, _ = Lookup(m.f.c, m.f.byOwner, r.Key.(string))
			}
			r.Answer, r.Holder = model.Answer(a), id
		case 3:
			r.Op = model.List
			ids, a := m.f.c.List()
			r.Answer, r.IDs = model.Answer(a), slices.Clone(ids)
			if m.mutate && len(ids) > 0 {
				ids[0] = "mutated"
			}
		}
		r.V1 = m.f.store.Version()
		r.S1 = m.clock.Add(1)
		m.mu.Lock()
		m.reads = append(m.reads, r)
		m.mu.Unlock()
	}
}

// writer runs random mutations, classified per PRE-4, with injected faults.
func (m *modelRun) writer(rng *rand.Rand, deadline time.Time) {
	for time.Now().Before(deadline) {
		if idle := m.idle.Load(); idle != nil {
			<-*idle // wait out the idle phase
		}
		m.injectFault(rng)
		m.mutation(rng)
	}
}

// phases alternates active phases (writers run) and idle phases (writers wait).
// phases alternates active phases, when writers run, and idle phases, when they wait.
// An idle phase waits for the loop to restore trust and then records a quiet window on
// the logical clock, in which the cache must answer most reads.
func (m *modelRun) phases(deadline time.Time) {
	wait := func(d time.Duration) {
		timer := time.NewTimer(d)
		<-timer.C
	}
	for time.Now().Before(deadline) {
		wait(3 * time.Millisecond)
		idle := make(chan struct{})
		m.idle.Store(&idle)
		for limit := time.Now().Add(100 * time.Millisecond); !m.trustedAndIdle() && time.Now().Before(limit); {
			wait(200 * time.Microsecond)
		}
		if m.trustedAndIdle() {
			start := m.clock.Add(1)
			wait(5 * time.Millisecond)
			m.mu.Lock()
			m.quiet = append(m.quiet, [2]int64{start, m.clock.Add(1)})
			m.mu.Unlock()
		}
		m.idle.Store(nil)
		close(idle)
	}
}

func (m *modelRun) trustedAndIdle() bool {
	m.f.c.mu.RLock()
	defer m.f.c.mu.RUnlock()
	return !m.f.c.stale && m.f.c.inFlight == 0
}

// checkQuietAnswers fails a run in which the cache never answered while trusted and
// idle: a cache that defers everything must not pass silently.
func (m *modelRun) checkQuietAnswers() {
	var reads, answered int
	for _, r := range m.reads {
		for _, q := range m.quiet {
			if r.S0 > q[0] && r.S1 < q[1] {
				reads++
				if r.Answer != model.Defer {
					answered++
				}
				break
			}
		}
	}
	if reads < minQuietReads {
		m.t.Errorf("only %d reads in %d trusted idle windows; want at least %d", reads, len(m.quiet), minQuietReads)
		return
	}
	if ratio := float64(answered) / float64(reads); ratio < minQuietRatio {
		m.t.Errorf("the trusted, idle cache answered %.1f%% of %d reads, want at least %.0f%%", 100*ratio, reads, 100*minQuietRatio)
	}
}

func (m *modelRun) injectFault(rng *rand.Rand) {
	s := m.f.store
	switch rng.IntN(40) {
	case 0:
		s.InjectWrite(memstore.WriteFault{Kind: memstore.NothingCommitted})
	case 1:
		s.InjectWrite(memstore.WriteFault{Kind: memstore.AmbiguousCommitted})
	case 2:
		s.InjectWrite(memstore.WriteFault{Kind: memstore.AmbiguousNotCommitted})
	case 3:
		release := make(chan struct{})
		m.releaseMu.Lock()
		m.releases = append(m.releases, release)
		m.releaseMu.Unlock()
		s.InjectWrite(memstore.WriteFault{Kind: memstore.AbandonedCommit, Release: release})
		time.AfterFunc(time.Duration(rng.IntN(3000))*time.Microsecond, func() { m.release(release) })
	case 4:
		s.InjectWrite(memstore.WriteFault{Kind: memstore.ContextAfterCommit})
	case 5:
		s.InjectLoadOne(memstore.ReadFault{Kind: memstore.FailRead})
	case 6:
		s.InjectLoadAll(memstore.ReadFault{Kind: memstore.FailRead})
	}
}

func (m *modelRun) release(ch chan struct{}) {
	m.releaseMu.Lock()
	defer m.releaseMu.Unlock()
	if i := slices.Index(m.releases, ch); i >= 0 {
		close(ch)
		m.releases = slices.Delete(m.releases, i, i+1)
	}
}

func (m *modelRun) releaseAll() {
	m.releaseMu.Lock()
	defer m.releaseMu.Unlock()
	for _, ch := range m.releases {
		close(ch)
	}
	m.releases = nil
}

// mutation runs one random mutation through the cache.
func (m *modelRun) mutation(rng *rand.Rand) {
	w := &model.Window[string]{}
	ctx := context.WithValue(context.Background(), windowKey{}, w)
	x, y := modelID(rng.IntN(modelIDs)), modelID(rng.IntN(modelIDs))
	panicking := rng.IntN(60) == 0
	write := func(fn func(tx *txn) error) func() error {
		return func() error {
			err := m.f.store.Write(ctx, fn)
			if panicking {
				panic("op panicked")
			}
			return err
		}
	}
	defer func() {
		if r := recover(); r != nil && r != "op panicked" {
			panic(r)
		}
	}()
	switch rng.IntN(6) {
	case 0: // update a field
		w.IDs = []string{x}
		name := fmt.Sprint("n", rng.IntN(1000))
		_ = m.f.c.Mutate(ctx, w.IDs, write(func(tx *txn) error {
			return tx.Update(x, func(e group) (group, error) { e.Name = name; return e, nil })
		}))
	case 1: // move or assign a unique member key; both ids named
		w.IDs = []string{x, y}
		k := modelMember(rng.IntN(modelMembers))
		_ = m.f.c.Mutate(ctx, w.IDs, write(func(tx *txn) error { return moveMember(tx, k, x, y) }))
	case 2: // set a unique owner
		w.IDs = []string{x}
		o := modelOwner(rng.IntN(modelOwners))
		_ = m.f.c.Mutate(ctx, w.IDs, write(func(tx *txn) error { return setOwner(tx, o, x) }))
	case 3: // create
		w.Create = true
		_, _ = m.f.c.Create(ctx, func() (string, error) {
			return x, write(func(tx *txn) error { return tx.Insert(g(x, "")) })()
		})
	case 4: // delete
		w.IDs = []string{x}
		_ = m.f.c.Mutate(ctx, w.IDs, write(func(tx *txn) error { return tx.Delete(x) }))
	case 5: // toggle cacheable; a non-cacheable entity holds no keys (PRE-3)
		w.IDs = []string{x}
		_ = m.f.c.Mutate(ctx, w.IDs, write(func(tx *txn) error {
			return tx.Update(x, func(e group) (group, error) {
				if e.Deleted = !e.Deleted; e.Deleted {
					e.Owner, e.Members = "", nil
				}
				return e, nil
			})
		}))
	}
}

// holder scans every id in the transaction, so a concurrent change to any of them makes
// the commit conflict: uniqueness holds under concurrency (PRE-2).
func holder(tx *txn, held func(group) bool) (string, bool) {
	for i := range modelIDs {
		if e, ok := tx.Get(modelID(i)); ok && held(e) {
			return e.ID, true
		}
	}
	return "", false
}

func moveMember(tx *txn, k, from, to string) error {
	h, held := holder(tx, func(e group) bool { return slices.Contains(e.Members, k) })
	if held && h != from {
		return memstore.ErrPrecondition
	}
	dst, ok := tx.Get(to)
	if !ok || dst.Deleted {
		return memstore.ErrPrecondition
	}
	if held {
		src, _ := tx.Get(from)
		src.Members = slices.DeleteFunc(src.Members, func(m string) bool { return m == k })
		tx.Put(src)
		if from == to {
			dst = src
		}
	}
	dst.Members = append(dst.Members, k)
	tx.Put(dst)
	return nil
}

func setOwner(tx *txn, o, to string) error {
	if _, held := holder(tx, func(e group) bool { return e.Owner == o }); held {
		return memstore.ErrPrecondition
	}
	return tx.Update(to, func(e group) (group, error) {
		if e.Deleted {
			return e, memstore.ErrPrecondition
		}
		e.Owner = o
		return e, nil
	})
}

// diagnose logs the context of the first violating reads: the value read, the commits
// on the entity, the windows around the read and the cache's state transitions.
func (m *modelRun) diagnose(h model.History[string, group]) {
	t := m.t
	violations, _ := model.Check(modelSpec, h)
	shown := 0
	for _, v := range violations {
		var idx int
		if _, err := fmt.Sscanf(v, "read %d:", &idx); err != nil || shown >= 3 {
			continue
		}
		shown++
		r := h.Reads[idx]
		t.Logf("DIAG read %d: op=%v id=%s key=%v answer=%v value=%+v holder=%s ids=%v V=[%d,%d] S=[%d,%d]",
			idx, r.Op, r.ID, r.Key, r.Answer, r.Value, r.Holder, r.IDs, r.V0, r.V1, r.S0, r.S1)
		for _, c := range h.Commits {
			touched := slices.Contains(c.Dels, r.ID) || slices.ContainsFunc(c.Puts, func(e group) bool {
				return e.ID == r.ID || r.Op != model.Get
			})
			if touched && c.Version+40 > r.V0 && c.Version < r.V1+5 {
				t.Logf("DIAG   commit v%d puts=%+v dels=%v", c.Version, c.Puts, c.Dels)
			}
		}
		for _, w := range h.Windows {
			if w.S1 > r.S0-400 && w.S0 < r.S1+50 {
				t.Logf("DIAG   window S=[%d,%d] ids=%v create=%t", w.S0, w.S1, w.IDs, w.Create)
			}
		}
		m.eventsMu.Lock()
		for _, e := range m.events {
			if e.s > r.S0-400 && e.s < r.S1+50 {
				t.Logf("DIAG   event S=%d %s", e.s, e.what)
			}
		}
		m.eventsMu.Unlock()
	}
}

// stamped is an observer event on the logical clock.
type stamped struct {
	s    int64
	what string
}

// modelObserver stamps state transitions and rebuild results on the logical clock.
type modelObserver struct {
	NopObserver
	m *modelRun
}

func (o modelObserver) add(what string) {
	s := o.m.clock.Add(1)
	o.m.eventsMu.Lock()
	o.m.events = append(o.m.events, stamped{s, what})
	o.m.eventsMu.Unlock()
}

func (o modelObserver) StaleChanged(stale bool, cause error) {
	o.add(fmt.Sprintf("stale=%t cause=%v", stale, cause))
}

func (o modelObserver) Rebuild(tr Trigger, r RebuildResult) {
	o.add(fmt.Sprintf("rebuild %s %s", tr, r))
}

// awaitTrusted waits, in real time, for the loop to rebuild after the run.
func (m *modelRun) awaitTrusted() {
	timeout := time.After(10 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		m.f.c.mu.RLock()
		trusted := !m.f.c.stale && m.f.c.inFlight == 0
		m.f.c.mu.RUnlock()
		if trusted {
			return
		}
		select {
		case <-ticker.C:
		case <-timeout:
			m.t.Fatalf("cache did not become trusted after the run: %v", m.f.staleCause())
		}
	}
}

// checkQuiescent checks the structural invariant and exact agreement with the store on
// the idle, trusted cache, and that the idle cache answers every cacheable read.
func (m *modelRun) checkQuiescent() {
	t := m.t
	if err := checkStructure(m.f.c); err != nil {
		t.Errorf("structural invariant: %v", err)
	}
	if p, n, w := m.f.marks(); p+n+w != 0 {
		t.Errorf("idle cache holds marks: pending=%d inFlight=%d windows=%d", p, n, w)
	}
	state := m.f.store.State()
	var want []string
	for id, e := range state {
		if !groupCacheable(e) {
			continue
		}
		want = append(want, id)
		got, a := m.f.c.Get(id)
		if a != Found || !reflect.DeepEqual(got, e) {
			t.Errorf("idle Get(%s) = %v, %v; store holds %v", id, got, a, e)
		}
	}
	slices.Sort(want)
	if ids, a := m.f.c.List(); a != Found || !slices.Equal(ids, want) {
		t.Errorf("idle List = %v, %v; want %v", ids, a, want)
	}
}

// TestModelHelpers_Uniqueness checks the workload's own transactions keep keys unique,
// so the model test never feeds the cache a PRE-2 violation.
func TestModelHelpers_Uniqueness(t *testing.T) {
	s := newGroupStore()
	s.Seed(g("g0", "o0", "k0"), g("g1", ""), group{ID: "g2", Deleted: true})
	ctx := context.Background()
	run := func(fn func(tx *txn) error) error { return s.Write(ctx, fn) }
	if err := run(func(tx *txn) error { return moveMember(tx, "k0", "g1", "g0") }); !errors.Is(err, memstore.ErrPrecondition) {
		t.Fatalf("move from a non-holder = %v", err)
	}
	if err := run(func(tx *txn) error { return moveMember(tx, "k0", "g0", "g2") }); !errors.Is(err, memstore.ErrPrecondition) {
		t.Fatalf("move to a non-cacheable entity = %v", err)
	}
	if err := run(func(tx *txn) error { return moveMember(tx, "k0", "g0", "g0") }); err != nil {
		t.Fatal(err)
	}
	if err := run(func(tx *txn) error { return moveMember(tx, "k0", "g0", "g1") }); err != nil {
		t.Fatal(err)
	}
	if err := run(func(tx *txn) error { return setOwner(tx, "o0", "g1") }); !errors.Is(err, memstore.ErrPrecondition) {
		t.Fatalf("taken owner = %v", err)
	}
	if err := run(func(tx *txn) error { return setOwner(tx, "o1", "g2") }); !errors.Is(err, memstore.ErrPrecondition) {
		t.Fatalf("owner on a non-cacheable entity = %v", err)
	}
	st := s.State()
	if !slices.Equal(st["g0"].Members, []string{"k0"}) && !slices.Equal(st["g1"].Members, []string{"k0"}) {
		t.Fatalf("state = %v", st)
	}
	if strings.Join(st["g1"].Members, ",") != "k0" || len(st["g0"].Members) != 0 {
		t.Fatalf("state = %v", st)
	}
}
