// SPDX-License-Identifier: Apache-2.0

package model_test

import (
	"reflect"
	"testing"

	"github.com/nobelk/surecache/internal/model"
)

func TestCheckReplaysDeletesInVersionOrder(t *testing.T) {
	type entity struct{ ID, Name string }
	spec := model.Spec[string, entity]{
		Key:       func(e entity) string { return e.ID },
		Cacheable: func(entity) bool { return true },
	}
	commits := []model.Commit[string, entity]{
		{Version: 5, Puts: []entity{{ID: "a", Name: "recreated"}}},
		{Version: 3, Dels: []string{"a", "missing"}},
	}
	h := model.History[string, entity]{
		BaseVer: 1,
		Base:    []entity{{ID: "a", Name: "original"}},
		Commits: commits,
		Reads: []model.Read[string, entity]{
			{Op: model.Get, ID: "a", Answer: model.Found, Value: entity{ID: "a", Name: "original"}, V0: 2, V1: 2},
			{Op: model.List, Answer: model.Found, V0: 3, V1: 3},
			{Op: model.List, Answer: model.Found, V0: 4, V1: 4},
			{Op: model.Get, ID: "a", Answer: model.Found, Value: entity{ID: "a", Name: "recreated"}, V0: 5, V1: 5},
		},
	}
	violations, stats := model.Check(spec, h)
	if len(violations) != 0 {
		t.Fatalf("violations = %v", violations)
	}
	if stats != (model.Stats{Reads: 4, Answered: 4}) {
		t.Fatalf("stats = %+v", stats)
	}
	if commits[0].Version != 5 || commits[1].Version != 3 {
		t.Fatal("Check reordered caller's commit log")
	}

	// Deletion must also reject an answer that retains the old entity.
	h.Reads = []model.Read[string, entity]{
		{Op: model.List, Answer: model.Found, IDs: []string{"a"}, V0: 3, V1: 4},
	}
	violations, stats = model.Check(spec, h)
	if len(violations) != 1 || stats.Answered != 1 {
		t.Fatalf("deleted entity accepted: %v, %+v", violations, stats)
	}
	if !reflect.DeepEqual(h.Base, []entity{{ID: "a", Name: "original"}}) {
		t.Fatal("Check changed the base state")
	}
}
