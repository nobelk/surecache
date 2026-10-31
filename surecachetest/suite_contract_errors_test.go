// SPDX-License-Identifier: Apache-2.0

package surecachetest

import (
	"context"
	"testing"

	"github.com/nobelk/surecache/internal/memstore"
)

func TestSuite_RejectsInvalidResults(t *testing.T) {
	cases := []struct {
		name, check, want string
		source            func(*memstore.Store[string, team]) brokenSource
	}{
		{"not-found-after-delete", "LoadOneNotFoundMeansAbsent", "LoadOne after Delete", func(s *memstore.Store[string, team]) brokenSource {
			return brokenSource{Store: s, loadOne: func(ctx context.Context, id string) (team, error) {
				e, err := s.LoadOne(ctx, id)
				if id == "t1" && err != nil {
					return team{ID: id}, nil
				}
				return e, err
			}}
		}},
		{"context-is-not-absence", "LoadOneNotFoundMeansAbsent", "NotFound(context.Canceled) is true", func(s *memstore.Store[string, team]) brokenSource {
			return brokenSource{Store: s, notFound: func(err error) bool { return err == context.Canceled || s.NotFound(err) }}
		}},
		{"wrong-entity-with-right-count", "LoadAllCacheableParity", "not a cacheable fixture", func(s *memstore.Store[string, team]) brokenSource {
			return brokenSource{Store: s, loadAll: func(ctx context.Context) ([]team, error) {
				all, err := s.LoadAll(ctx)
				if len(all) > 0 {
					all[0].Name = "corrupt"
				}
				return all, err
			}}
		}},
		{"fence-succeeds", "FenceWaitTimesOut", "LoadAll succeeded while a mutation held the fence", func(s *memstore.Store[string, team]) brokenSource {
			return brokenSource{Store: s, loadAll: func(context.Context) ([]team, error) { return nil, nil }}
		}},
		{"fence-spurious-cancellation", "FenceWaitTimesOut", "without a cancelled context", func(s *memstore.Store[string, team]) brokenSource {
			return brokenSource{Store: s, loadAll: func(context.Context) ([]team, error) { return nil, context.Canceled }}
		}},
		{"fenced-load-fails", "FenceOrdersBulkLoad/commit", "LoadAll: adapter diagnostic failure", func(s *memstore.Store[string, team]) brokenSource {
			return brokenSource{Store: s, loadAll: func(ctx context.Context) ([]team, error) {
				if _, err := s.LoadAll(ctx); err != nil {
					return nil, err
				}
				return nil, errDiagnostic
			}}
		}},
		{"fenced-load-loses-entity", "FenceOrdersBulkLoad/commit", "LoadAll lost t1", func(s *memstore.Store[string, team]) brokenSource {
			return brokenSource{Store: s, loadAll: func(ctx context.Context) ([]team, error) {
				all, err := s.LoadAll(ctx)
				out := make([]team, 0, len(all))
				for _, e := range all {
					if e.ID != "t1" {
						out = append(out, e)
					}
				}
				return out, err
			}}
		}},
		{"fenced-load-stale", "FenceOrdersBulkLoad/commit", "LoadAll after the mutation finished returned", func(s *memstore.Store[string, team]) brokenSource {
			before, _ := s.Get("t1")
			return brokenSource{Store: s, loadAll: func(ctx context.Context) ([]team, error) {
				all, err := s.LoadAll(ctx)
				for i := range all {
					if all[i].ID == "t1" {
						all[i] = before
					}
				}
				return all, err
			}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := harness(wrap(tc.source))
			requireDiagnostic(t, (suite[string, team]{h: h}).byName(tc.check), tc.want)
		})
	}
}

func TestSuite_RejectsInvalidIndexFixtures(t *testing.T) {
	t.Run("duplicate-key", func(t *testing.T) {
		h := harness(variant{})
		h.IndexKeys = map[string]func(team) []any{"duplicate": func(e team) []any {
			if e.ID == "t1" || e.ID == "t2" {
				return []any{"shared"}
			}
			return nil
		}}
		requireDiagnostic(t, (suite[string, team]{h: h}).indexParity, "key shared of index duplicate held by t1 and t2 (PRE-2)")
	})
	t.Run("missing-shape", func(t *testing.T) {
		h := harness(variant{})
		h.IndexKeys = nil
		requireDiagnostic(t, (suite[string, team]{h: h}).indexParity, "fixtures miss an entity shape: cacheable with keys=false, cacheable without keys=true, non-cacheable=true")
	})
}
