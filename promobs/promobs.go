// SPDX-License-Identifier: Apache-2.0

package promobs

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nobelk/surecache"
)

// The closed label sets, in enum order, so a child collector is an array index away.
var (
	methods = []surecache.Method{surecache.MethodGet, surecache.MethodLookup, surecache.MethodList}
	sources = []surecache.ReadSource{
		surecache.SourceCache, surecache.SourceStale, surecache.SourcePending,
		surecache.SourceInFlight, surecache.SourceMiss,
	}
	triggers = []surecache.Trigger{surecache.TriggerPrime, surecache.TriggerLoop}
	results  = []surecache.RebuildResult{surecache.Installed, surecache.Discarded, surecache.Failed}
)

// Option configures New.
type Option func(*options)

type options struct {
	namespace   string
	constLabels prometheus.Labels
}

// WithNamespace prefixes every metric name with namespace and an underscore. The
// default is no prefix, so names start with surecache_.
func WithNamespace(namespace string) Option {
	return func(o *options) { o.namespace = namespace }
}

// WithConstLabels adds constant labels to every metric.
func WithConstLabels(labels prometheus.Labels) Option {
	return func(o *options) { o.constLabels = labels }
}

// Metrics owns the collectors of design section 9. Register it once per registry and
// share it across caches; each cache gets its own Observer, labeled by cache name.
type Metrics struct {
	stale         *prometheus.GaugeVec
	unhealthy     *prometheus.GaugeVec
	entities      *prometheus.GaugeVec
	reads         *prometheus.CounterVec
	rebuilds      *prometheus.CounterVec
	ambiguous     *prometheus.CounterVec
	probeFailures *prometheus.CounterVec
}

// New creates the collectors and registers them with reg. It returns an error, never a
// panic, when a collector is already registered; then it registers nothing.
func New(reg prometheus.Registerer, opts ...Option) (*Metrics, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	gauge := func(name, help string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: o.namespace, Name: name, Help: help, ConstLabels: o.constLabels,
		}, []string{"cache"})
	}
	counter := func(name, help string, labels ...string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: o.namespace, Name: name, Help: help, ConstLabels: o.constLabels,
		}, append([]string{"cache"}, labels...))
	}
	m := &Metrics{
		stale:     gauge("surecache_stale", "1 while the cache is stale, 0 otherwise."),
		unhealthy: gauge("surecache_unhealthy", "1 while the outage policy distrusts the store, 0 otherwise."),
		entities:  gauge("surecache_entities", "Entities held."),
		reads: counter("surecache_reads_total",
			"Cache reads by method and by who answered or why the read deferred.", "method", "source"),
		rebuilds:      counter("surecache_rebuilds_total", "Whole-cache rebuilds by trigger and result.", "trigger", "result"),
		ambiguous:     counter("surecache_ambiguous_mutations_total", "Mutations whose outcome was unknown."),
		probeFailures: counter("surecache_store_probe_failures_total", "Health probe failures under the outage policy."),
	}
	collectors := []prometheus.Collector{m.stale, m.unhealthy, m.entities, m.reads, m.rebuilds, m.ambiguous, m.probeFailures}
	for i, c := range collectors {
		if err := reg.Register(c); err != nil {
			for _, done := range collectors[:i] {
				reg.Unregister(done)
			}
			return nil, errors.Join(errors.New("promobs: registering collectors"), err)
		}
	}
	return m, nil
}

// Observer returns a surecache.Observer for one cache, named by Spec.Name. It creates
// every label combination for that cache at once, so all series exist at zero, and
// resolves every child collector up front: Read is an array index and an increment,
// with no map lookup and no allocation.
func (m *Metrics) Observer(cache string) surecache.Observer {
	o := &observer{
		stale:         m.stale.WithLabelValues(cache),
		unhealthy:     m.unhealthy.WithLabelValues(cache),
		entities:      m.entities.WithLabelValues(cache),
		ambiguous:     m.ambiguous.WithLabelValues(cache),
		probeFailures: m.probeFailures.WithLabelValues(cache),
	}
	for _, method := range methods {
		for _, source := range sources {
			o.reads[method][source] = m.reads.WithLabelValues(cache, method.String(), source.String())
		}
	}
	for _, trigger := range triggers {
		for _, result := range results {
			o.rebuilds[trigger][result] = m.rebuilds.WithLabelValues(cache, trigger.String(), result.String())
		}
	}
	return o
}

// observer adapts one cache's events to the collectors. It embeds NopObserver, so
// events added in later versions are ignored until handled here.
type observer struct {
	surecache.NopObserver
	reads         [3][5]prometheus.Counter // [Method][ReadSource]
	rebuilds      [2][3]prometheus.Counter // [Trigger][RebuildResult]
	stale         prometheus.Gauge
	unhealthy     prometheus.Gauge
	entities      prometheus.Gauge
	ambiguous     prometheus.Counter
	probeFailures prometheus.Counter
}

func (o *observer) Read(m surecache.Method, s surecache.ReadSource) {
	if int(m) < len(o.reads) && int(s) < len(o.reads[m]) {
		o.reads[m][s].Inc()
	}
}

func (o *observer) StaleChanged(stale bool, _ error) { o.stale.Set(boolValue(stale)) }

func (o *observer) UnhealthyChanged(unhealthy bool) { o.unhealthy.Set(boolValue(unhealthy)) }

func (o *observer) Entities(n int) { o.entities.Set(float64(n)) }

func (o *observer) Rebuild(t surecache.Trigger, r surecache.RebuildResult) {
	if int(t) < len(o.rebuilds) && int(r) < len(o.rebuilds[t]) {
		o.rebuilds[t][r].Inc()
	}
}

func (o *observer) AmbiguousMutation() { o.ambiguous.Inc() }

func (o *observer) ProbeFailure() { o.probeFailures.Inc() }

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
