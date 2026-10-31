// SPDX-License-Identifier: Apache-2.0

package surecachetest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nobelk/surecache/internal/memstore"
)

var errDiagnostic = errors.New("adapter diagnostic failure")

// diagnosticStore preserves the real adapter except for the operation under test.
// In particular, successful operations still exercise the normal transaction path.
type diagnosticStore struct {
	*memStore
	fail  string
	dumps int
}

func (s *diagnosticStore) Seed(ctx context.Context, fixtures []team) error {
	if s.fail == "seed" {
		return errDiagnostic
	}
	return s.memStore.Seed(ctx, fixtures)
}

func (s *diagnosticStore) Fixtures() []team {
	if s.fail == "fixtures" {
		return nil
	}
	return s.memStore.Fixtures()
}

func (s *diagnosticStore) Dump(ctx context.Context) (any, error) {
	s.dumps++
	if s.fail == "dump-before" || s.fail == "dump-after" && s.dumps == 2 {
		return nil, errDiagnostic
	}
	return s.memStore.Dump(ctx)
}

func (s *diagnosticStore) NothingCommittedCases() []Case {
	switch s.fail {
	case "missing-case":
		return nil
	case "successful-case":
		return []Case{{Name: "not found", Op: func(context.Context) error { return nil }}}
	case "ambiguous-case":
		return []Case{{Name: "not found", Op: func(context.Context) error { return errDiagnostic }}}
	}
	return s.memStore.NothingCommittedCases()
}

func (s *diagnosticStore) Update(ctx context.Context, id string) (team, error) {
	if s.fail == "update" {
		return team{}, errDiagnostic
	}
	return s.memStore.Update(ctx, id)
}

func (s *diagnosticStore) Delete(ctx context.Context, id string) error {
	if s.fail == "delete" {
		return errDiagnostic
	}
	return s.memStore.Delete(ctx, id)
}

func (s *diagnosticStore) BeginMutation(ctx context.Context, id string) (OpenMutation[team], error) {
	if s.fail == "begin" {
		return nil, errDiagnostic
	}
	return s.memStore.BeginMutation(ctx, id)
}

func (s *diagnosticStore) SeedOverCapacity(ctx context.Context) error {
	if s.fail == "capacity" {
		return errDiagnostic
	}
	return s.memStore.SeedOverCapacity(ctx)
}

func requireDiagnostic(t *testing.T, fn func(testing.TB), want string) {
	t.Helper()
	r := runRecorded(t, fn)
	if !r.Failed() || r.skipped || !strings.Contains(strings.Join(r.log, "\n"), want) {
		t.Fatalf("want failure containing %q, got failed=%t skipped=%t logs=%v", want, r.Failed(), r.skipped, r.log)
	}
}

func TestSuite_ReportsAdapterErrors(t *testing.T) {
	cases := []struct{ check, fail, want string }{
		{"LoadOneKeyMatches", "seed", "Seed: adapter diagnostic failure"},
		{"LoadOneReadsLatestCommit", "fixtures", "the fixtures need at least 1 cacheable entities, have 0"},
		{"NothingCommitted", "missing-case", `no nothing-committed case "not found"`},
		{"NothingCommitted", "dump-before", "Dump: adapter diagnostic failure"},
		{"NothingCommitted", "dump-after", "Dump: adapter diagnostic failure"},
		{"NothingCommitted", "successful-case", "case returned <nil>; want an error for which CommittedNothing is true"},
		{"NothingCommitted", "ambiguous-case", "case returned adapter diagnostic failure; want an error for which CommittedNothing is true"},
		{"LoadOneReadsLatestCommit", "update", "Update: adapter diagnostic failure"},
		{"LoadOneNotFoundMeansAbsent", "delete", "Delete: adapter diagnostic failure"},
		{"LoadAllRefusesOverCapacity", "capacity", "SeedOverCapacity: adapter diagnostic failure"},
	}
	for _, check := range []string{"ContextErrorsAreNotNothingCommitted", "LoadAllContextErrorsUnwrapped", "FenceOrdersBulkLoad/commit", "FenceWaitTimesOut", "MutationsDoNotBlockEachOther"} {
		cases = append(cases, struct{ check, fail, want string }{check, "begin", "BeginMutation: adapter diagnostic failure"})
	}
	for _, tc := range cases {
		t.Run(tc.check+"/"+tc.fail, func(t *testing.T) {
			h := harness(variant{})
			newStore := h.NewStore
			h.NewStore = func(tb testing.TB) Store[string, team] {
				return &diagnosticStore{memStore: newStore(tb).(*memStore), fail: tc.fail}
			}
			s := suite[string, team]{h: h}
			fn := s.byName(tc.check)
			if tc.check == "NothingCommitted" {
				fn = func(tb testing.TB) { s.nothingCommitted(tb, "not found") }
			}
			requireDiagnostic(t, fn, tc.want)
		})
	}
}

func TestSuite_ReportsLoadErrors(t *testing.T) {
	for _, check := range []string{"LoadOneKeyMatches", "LoadOneConsistentSnapshot", "LoadAllConsistentSnapshot", "LoadAllEachKeyOnce", "LoadAllCacheableParity"} {
		t.Run(check, func(t *testing.T) {
			h := harness(wrap(func(s *memstore.Store[string, team]) brokenSource {
				return brokenSource{
					Store:   s,
					loadOne: func(context.Context, string) (team, error) { return team{}, errDiagnostic },
					loadAll: func(context.Context) ([]team, error) { return nil, errDiagnostic },
				}
			}))
			requireDiagnostic(t, (suite[string, team]{h: h}).byName(check), "adapter diagnostic failure")
		})
	}
}

func TestSuite_CustomEquality(t *testing.T) {
	h := harness(variant{})
	calls := 0
	h.Equal = func(a, b team) bool {
		calls++
		// Deliberately compare a domain identity rather than the whole representation.
		return a.ID == b.ID && a.Rev == b.Rev
	}
	s := suite[string, team]{h: h}
	s.loadOneReadsLatestCommit(t)
	if calls != 3 {
		t.Fatalf("Equal called %d times, want one comparison per cacheable fixture (3)", calls)
	}
	// The custom comparator must also control rejection, not just successful checks.
	h.Equal = func(team, team) bool { return false }
	requireDiagnostic(t, (suite[string, team]{h: h}).loadOneReadsLatestCommit, "LoadOne after Update")
}
