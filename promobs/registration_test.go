// SPDX-License-Identifier: Apache-2.0

package promobs_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nobelk/surecache"
	"github.com/nobelk/surecache/promobs"
)

// A late collision must undo earlier registrations without removing the collector
// that caused the collision, or leaving the registry unusable for a retry.
func TestNew_PartialRegistrationRollback(t *testing.T) {
	for _, name := range []string{"surecache_entities", "surecache_store_probe_failures_total"} {
		t.Run(name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			// Obtain an identically described collector through the public constructor.
			capture := &capturingRegisterer{Registry: prometheus.NewRegistry()}
			m, err := promobs.New(capture)
			if err != nil {
				t.Fatal(err)
			}
			m.Observer("existing")
			var collision prometheus.Collector
			for _, c := range capture.collectors {
				isolated := prometheus.NewRegistry()
				isolated.MustRegister(c)
				families, gatherErr := isolated.Gather()
				if gatherErr != nil {
					t.Fatal(gatherErr)
				}
				if len(families) == 1 && families[0].GetName() == name {
					collision = c
				}
			}
			if collision == nil {
				t.Fatalf("collector %s not found", name)
			}
			reg.MustRegister(collision)
			got, err := promobs.New(reg)
			var duplicate prometheus.AlreadyRegisteredError
			if got != nil || !errors.As(err, &duplicate) {
				t.Fatalf("New = %v, %v; want nil and preserved duplicate error", got, err)
			}
			families, err := reg.Gather()
			if err != nil || len(families) != 1 || families[0].GetName() != name {
				t.Fatalf("registry after failure = %v, %v", families, err)
			}
			if !reg.Unregister(collision) {
				t.Fatal("failed New removed the existing collector")
			}
			retry, err := promobs.New(reg)
			if err != nil {
				t.Fatalf("retry after removing collision: %v", err)
			}
			retry.Observer("retry")
			if got := series(t, reg); len(got) != 7 {
				t.Fatalf("retry metric families = %v", got)
			}
		})
	}
}

type capturingRegisterer struct {
	*prometheus.Registry
	collectors []prometheus.Collector
}

func (r *capturingRegisterer) Register(c prometheus.Collector) error {
	if err := r.Registry.Register(c); err != nil {
		return err
	}
	r.collectors = append(r.collectors, c)
	return nil
}

// User-supplied registerers can fail for reasons other than duplicate names.
func TestNew_RegistererErrorPreserved(t *testing.T) {
	sentinel := errors.New("registry unavailable")
	reg := failingRegisterer{Registerer: prometheus.NewRegistry(), err: sentinel}
	m, err := promobs.New(reg)
	if m != nil || !errors.Is(err, sentinel) {
		t.Fatalf("New = %v, %v", m, err)
	}
}

type failingRegisterer struct {
	prometheus.Registerer
	err error
}

func (r failingRegisterer) Register(prometheus.Collector) error {
	return fmt.Errorf("register: %w", r.err)
}

// Every valid enum combination is independent; unknown enum values must not
// create new time series or corrupt the last valid array entry.
func TestObserver_AllEnumCombinations(t *testing.T) {
	m, reg := newMetrics(t)
	o := m.Observer("enums")
	for _, method := range []surecache.Method{surecache.MethodGet, surecache.MethodLookup, surecache.MethodList} {
		for _, source := range []surecache.ReadSource{surecache.SourceCache, surecache.SourceStale, surecache.SourcePending, surecache.SourceInFlight, surecache.SourceMiss} {
			o.Read(method, source)
		}
	}
	for _, trigger := range []surecache.Trigger{surecache.TriggerPrime, surecache.TriggerLoop} {
		for _, result := range []surecache.RebuildResult{surecache.Installed, surecache.Discarded, surecache.Failed} {
			o.Rebuild(trigger, result)
		}
	}
	o.Read(surecache.Method(255), surecache.ReadSource(255))
	o.Rebuild(surecache.Trigger(255), surecache.RebuildResult(255))
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "surecache_reads_total" && family.GetName() != "surecache_rebuilds_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			if got := metric.GetCounter().GetValue(); got != 1 {
				t.Errorf("%s %v = %v, want 1", family.GetName(), metric.GetLabel(), got)
			}
		}
	}
}
