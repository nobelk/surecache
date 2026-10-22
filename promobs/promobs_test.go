// SPDX-License-Identifier: Apache-2.0

package promobs_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/nobelk/surecache"
	"github.com/nobelk/surecache/promobs"
)

func newMetrics(t *testing.T, opts ...promobs.Option) (*promobs.Metrics, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m, err := promobs.New(reg, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return m, reg
}

// series counts the series of each metric family.
func series(t *testing.T, reg *prometheus.Registry) map[string]int {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			out[mf.GetName()]++
			if v := m.GetCounter().GetValue() + m.GetGauge().GetValue(); v != 0 {
				t.Errorf("%s %v = %v at start, want 0", mf.GetName(), m.GetLabel(), v)
			}
		}
	}
	return out
}

// Section 9: every series exists at zero as soon as an observer is created, so an idle
// scrape reads zeros rather than absent series.
func TestObserver_AllSeriesPresentAtStart(t *testing.T) {
	m, reg := newMetrics(t)
	m.Observer("groups")
	want := map[string]int{
		"surecache_stale":                      1,
		"surecache_unhealthy":                  1,
		"surecache_entities":                   1,
		"surecache_reads_total":                3 * 5,
		"surecache_rebuilds_total":             2 * 3,
		"surecache_ambiguous_mutations_total":  1,
		"surecache_store_probe_failures_total": 1,
	}
	got := series(t, reg)
	for name, n := range want {
		if got[name] != n {
			t.Errorf("%s has %d series, want %d", name, got[name], n)
		}
	}
	if len(got) != len(want) {
		t.Errorf("families = %v", got)
	}
}

// SAFE-9: Build publishes the initial stale state, and a failed Prime keeps it.
func TestObserver_StaleGaugeFollowsCache(t *testing.T) {
	m, reg := newMetrics(t)
	obs := m.Observer("groups")
	stale := func() float64 { return value(t, reg, "surecache_stale", map[string]string{"cache": "groups"}) }
	if v := stale(); v != 0 {
		t.Fatalf("stale before Build = %v", v)
	}
	src := &mapSource{fail: errors.New("down")}
	c := buildCache(t, src, obs)
	if v := stale(); v != 1 {
		t.Fatalf("stale after Build = %v, want 1", v)
	}
	if r, _ := c.Prime(context.Background()); r != surecache.Failed {
		t.Fatalf("Prime = %v", r)
	}
	if v := stale(); v != 1 {
		t.Fatalf("stale after a failed Prime = %v, want 1", v)
	}
}

func TestObserver_Read(t *testing.T) {
	m, reg := newMetrics(t)
	obs := m.Observer("groups")
	obs.Read(surecache.MethodGet, surecache.SourceCache)
	obs.Read(surecache.MethodGet, surecache.SourceCache)
	obs.Read(surecache.MethodLookup, surecache.SourceInFlight)
	obs.Read(surecache.MethodList, surecache.SourceStale)
	obs.Read(surecache.Method(99), surecache.SourceCache) // unknown values are ignored
	obs.Read(surecache.MethodGet, surecache.ReadSource(99))
	expect(t, reg, "surecache_reads_total", `
		# HELP surecache_reads_total Cache reads by method and by who answered or why the read deferred.
		# TYPE surecache_reads_total counter
		surecache_reads_total{cache="groups",method="get",source="cache"} 2
		surecache_reads_total{cache="groups",method="get",source="in_flight"} 0
		surecache_reads_total{cache="groups",method="get",source="miss"} 0
		surecache_reads_total{cache="groups",method="get",source="pending"} 0
		surecache_reads_total{cache="groups",method="get",source="stale"} 0
		surecache_reads_total{cache="groups",method="list",source="cache"} 0
		surecache_reads_total{cache="groups",method="list",source="in_flight"} 0
		surecache_reads_total{cache="groups",method="list",source="miss"} 0
		surecache_reads_total{cache="groups",method="list",source="pending"} 0
		surecache_reads_total{cache="groups",method="list",source="stale"} 1
		surecache_reads_total{cache="groups",method="lookup",source="cache"} 0
		surecache_reads_total{cache="groups",method="lookup",source="in_flight"} 1
		surecache_reads_total{cache="groups",method="lookup",source="miss"} 0
		surecache_reads_total{cache="groups",method="lookup",source="pending"} 0
		surecache_reads_total{cache="groups",method="lookup",source="stale"} 0
	`)
}

func TestObserver_StateGauges(t *testing.T) {
	m, reg := newMetrics(t)
	obs := m.Observer("groups")
	obs.StaleChanged(true, errors.New("x"))
	obs.UnhealthyChanged(true)
	obs.Entities(42)
	for name, want := range map[string]float64{"surecache_stale": 1, "surecache_unhealthy": 1, "surecache_entities": 42} {
		if v := value(t, reg, name, map[string]string{"cache": "groups"}); v != want {
			t.Errorf("%s = %v, want %v", name, v, want)
		}
	}
	obs.StaleChanged(false, nil)
	obs.UnhealthyChanged(false)
	for _, name := range []string{"surecache_stale", "surecache_unhealthy"} {
		if v := value(t, reg, name, map[string]string{"cache": "groups"}); v != 0 {
			t.Errorf("%s = %v after clearing", name, v)
		}
	}
}

func TestObserver_Rebuild(t *testing.T) {
	m, reg := newMetrics(t)
	obs := m.Observer("groups")
	obs.Rebuild(surecache.TriggerPrime, surecache.Installed)
	obs.Rebuild(surecache.TriggerLoop, surecache.Failed)
	obs.Rebuild(surecache.TriggerLoop, surecache.Failed)
	obs.Rebuild(surecache.Trigger(9), surecache.Failed)
	obs.Rebuild(surecache.TriggerLoop, surecache.RebuildResult(9))
	expect(t, reg, "surecache_rebuilds_total", `
		# HELP surecache_rebuilds_total Whole-cache rebuilds by trigger and result.
		# TYPE surecache_rebuilds_total counter
		surecache_rebuilds_total{cache="groups",result="discarded",trigger="loop"} 0
		surecache_rebuilds_total{cache="groups",result="discarded",trigger="prime"} 0
		surecache_rebuilds_total{cache="groups",result="failed",trigger="loop"} 2
		surecache_rebuilds_total{cache="groups",result="failed",trigger="prime"} 0
		surecache_rebuilds_total{cache="groups",result="installed",trigger="loop"} 0
		surecache_rebuilds_total{cache="groups",result="installed",trigger="prime"} 1
	`)
}

func TestObserver_Counters(t *testing.T) {
	m, reg := newMetrics(t)
	obs := m.Observer("groups")
	obs.AmbiguousMutation()
	obs.ProbeFailure()
	obs.ProbeFailure()
	expect(t, reg, "surecache_ambiguous_mutations_total", `
		# HELP surecache_ambiguous_mutations_total Mutations whose outcome was unknown.
		# TYPE surecache_ambiguous_mutations_total counter
		surecache_ambiguous_mutations_total{cache="groups"} 1
	`)
	expect(t, reg, "surecache_store_probe_failures_total", `
		# HELP surecache_store_probe_failures_total Health probe failures under the outage policy.
		# TYPE surecache_store_probe_failures_total counter
		surecache_store_probe_failures_total{cache="groups"} 2
	`)
}

// One Metrics serves several caches, each with its own label.
func TestObserver_SharedAcrossCaches(t *testing.T) {
	m, reg := newMetrics(t)
	m.Observer("a").AmbiguousMutation()
	m.Observer("b")
	expect(t, reg, "surecache_ambiguous_mutations_total", `
		# HELP surecache_ambiguous_mutations_total Mutations whose outcome was unknown.
		# TYPE surecache_ambiguous_mutations_total counter
		surecache_ambiguous_mutations_total{cache="a"} 1
		surecache_ambiguous_mutations_total{cache="b"} 0
	`)
}

func TestNew_Options(t *testing.T) {
	_, reg := newMetrics(t, promobs.WithNamespace("app"), promobs.WithConstLabels(prometheus.Labels{"region": "eu"}))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if !strings.HasPrefix(mf.GetName(), "app_surecache_") {
			t.Errorf("name %s lacks the namespace", mf.GetName())
		}
	}
	m, reg := newMetrics(t, promobs.WithConstLabels(prometheus.Labels{"region": "eu"}))
	m.Observer("groups")
	mfs, _ = reg.Gather()
	if l := mfs[0].GetMetric()[0].GetLabel(); !strings.Contains(l[len(l)-1].String(), "eu") && !strings.Contains(l[0].String(), "eu") {
		t.Errorf("labels = %v", l)
	}
}

// Registering twice on one registry returns an error, never a panic.
func TestNew_DuplicateRegistration(t *testing.T) {
	reg := prometheus.NewRegistry()
	if _, err := promobs.New(reg); err != nil {
		t.Fatal(err)
	}
	if _, err := promobs.New(reg); err == nil {
		t.Fatal("second New on one registry succeeded")
	}
	// A failed New registers nothing, so the first set stays intact and usable.
	if _, err := reg.Gather(); err != nil {
		t.Fatal(err)
	}
}

// NFR-3: the Read hot path is an array index and an increment; it allocates nothing.
func TestObserver_ReadZeroAllocs(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not meaningful under -race")
	}
	m, _ := newMetrics(t)
	obs := m.Observer("groups")
	if n := testing.AllocsPerRun(1000, func() { obs.Read(surecache.MethodGet, surecache.SourceCache) }); n != 0 {
		t.Fatalf("Read allocs = %v", n)
	}
}

// End to end: a real cache, a mutation, a stale episode and a recovery.
func TestObserver_Integration(t *testing.T) {
	m, reg := newMetrics(t)
	src := &mapSource{data: map[string]string{"a": "1", "b": "2"}}
	c := buildCache(t, src, m.Observer("groups"))
	if r, err := c.Prime(context.Background()); r != surecache.Installed {
		t.Fatal(r, err)
	}
	c.Get("a")
	c.Get("zz")
	_ = c.Mutate(context.Background(), []string{"a"}, func() error { src.set("a", "3"); return nil })
	_ = c.Mutate(context.Background(), []string{"b"}, func() error { return errors.New("connection lost") })
	c.List()
	stale := value(t, reg, "surecache_stale", map[string]string{"cache": "groups"})
	if stale != 1 {
		t.Fatalf("stale = %v after an ambiguous mutation", stale)
	}
	// The loop recovers.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	for value(t, reg, "surecache_stale", map[string]string{"cache": "groups"}) != 0 {
		<-time.After(waitInterval)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if v := value(t, reg, "surecache_entities", map[string]string{"cache": "groups"}); v != 2 {
		t.Fatalf("entities = %v", v)
	}
	expect(t, reg, "surecache_ambiguous_mutations_total", `
		# HELP surecache_ambiguous_mutations_total Mutations whose outcome was unknown.
		# TYPE surecache_ambiguous_mutations_total counter
		surecache_ambiguous_mutations_total{cache="groups"} 1
	`)
	n, err := testutil.GatherAndCount(reg, "surecache_rebuilds_total")
	if err != nil || n != 6 {
		t.Fatalf("rebuild series = %d, %v", n, err)
	}
	if v := value(t, reg, "surecache_reads_total", map[string]string{"method": "get", "source": "miss"}); v != 1 {
		t.Fatalf("get miss reads = %v", v)
	}
	if v := value(t, reg, "surecache_rebuilds_total", map[string]string{"trigger": "loop", "result": "installed"}); v < 1 {
		t.Fatalf("loop installs = %v", v)
	}
}

func expect(t *testing.T, reg *prometheus.Registry, name, text string) {
	t.Helper()
	if err := testutil.GatherAndCompare(reg, strings.NewReader(dedent(text)), name); err != nil {
		t.Fatal(err)
	}
}

func dedent(s string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// waitInterval is the rebuild interval of the integration cache.
const waitInterval = 5 * time.Millisecond

func value(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	metrics:
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if want, ok := labels[l.GetName()]; ok && want != l.GetValue() {
					continue metrics
				}
			}
			return m.GetCounter().GetValue() + m.GetGauge().GetValue()
		}
	}
	t.Fatalf("no %s %v", name, labels)
	return 0
}

// mapSource is a minimal Source over a map; fail makes every call fail.
type mapSource struct {
	mu   sync.Mutex
	data map[string]string
	fail error
}

type entry struct{ ID, V string }

var errNotFound = errors.New("not found")

func (s *mapSource) set(id, v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[id] = v
}

func (s *mapSource) LoadOne(_ context.Context, id string) (entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return entry{}, s.fail
	}
	v, ok := s.data[id]
	if !ok {
		return entry{}, errNotFound
	}
	return entry{id, v}, nil
}

func (s *mapSource) NotFound(err error) bool { return errors.Is(err, errNotFound) }

func (s *mapSource) LoadAll(context.Context) ([]entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return nil, s.fail
	}
	var out []entry
	for id, v := range s.data {
		out = append(out, entry{id, v})
	}
	return out, nil
}

func buildCache(t *testing.T, src *mapSource, obs surecache.Observer) *surecache.Cache[string, entry] {
	t.Helper()
	c, err := surecache.NewBuilder(surecache.Spec[string, entry]{
		Name:             "groups",
		Key:              func(e entry) string { return e.ID },
		Clone:            func(e entry) entry { return e },
		Cacheable:        func(entry) bool { return true },
		Compare:          strings.Compare,
		Capacity:         10,
		CommittedNothing: func(error) bool { return false },
	}, surecache.Source[string, entry](src)).Build(
		surecache.WithOutagePolicy(surecache.ServeWhileExact()),
		surecache.WithObserver(obs),
		surecache.WithRebuildInterval(waitInterval),
	)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
