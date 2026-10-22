// SPDX-License-Identifier: Apache-2.0

package promobs_test

import (
	"fmt"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nobelk/surecache"
	"github.com/nobelk/surecache/promobs"
)

// Register the collectors once, then give each cache its own observer, labeled with the
// cache's Spec.Name.
func ExampleMetrics_Observer() {
	reg := prometheus.NewRegistry()
	metrics, err := promobs.New(reg)
	if err != nil {
		panic(err)
	}
	src := &mapSource{data: map[string]string{"a": "1"}}
	_, err = surecache.NewBuilder(surecache.Spec[string, entry]{
		Name:             "groups",
		Key:              func(e entry) string { return e.ID },
		Clone:            func(e entry) entry { return e },
		Cacheable:        func(entry) bool { return true },
		Compare:          strings.Compare,
		Capacity:         100,
		CommittedNothing: func(error) bool { return false },
	}, surecache.Source[string, entry](src)).Build(
		surecache.WithOutagePolicy(surecache.ServeWhileExact()),
		surecache.WithObserver(metrics.Observer("groups")),
	)
	if err != nil {
		panic(err)
	}
	mfs, _ := reg.Gather()
	for _, mf := range mfs {
		if mf.GetName() == "surecache_stale" {
			fmt.Println("stale before prime:", mf.GetMetric()[0].GetGauge().GetValue())
		}
	}
	// Output: stale before prime: 1
}
