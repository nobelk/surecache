// SPDX-License-Identifier: Apache-2.0

package surecachepg_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/nobelk/surecache/surecachepg/internal/groupstore"
)

func groupstoreEdgeShapes() []groupstore.Group {
	return []groupstore.Group{
		{ID: "g1", Name: "source", Owner: "o1", Rev: 3, Members: []groupstore.Member{{Name: "m1", Rev: 7}, {Name: "m2", Rev: 8}}},
		{ID: "g2", Name: "target", Owner: "o2", Members: []groupstore.Member{{Name: "m3", Rev: 9}}},
		{ID: "g3", Name: "no keys"},
		{ID: "g4", Name: "archived", Deleted: true},
	}
}

// Check both read interfaces: LoadOne retains non-cacheable entities, whereas
// LoadAll must omit them. Full equality also catches unintended revision changes.
func assertGroupstoreEdgeState(t *testing.T, ctx context.Context, s *groupstore.Store, want []groupstore.Group) {
	t.Helper()
	var cacheable []groupstore.Group
	for _, g := range want {
		got, err := s.LoadOne(ctx, g.ID)
		if err != nil || !reflect.DeepEqual(got, g) {
			t.Fatalf("LoadOne(%q) = %+v, %v; want %+v", g.ID, got, err, g)
		}
		if groupstore.Cacheable(g) {
			cacheable = append(cacheable, g)
		}
	}
	got, err := s.LoadAll(ctx)
	if err != nil || !reflect.DeepEqual(got, cacheable) {
		t.Fatalf("LoadAll = %+v, %v; want %+v", got, err, cacheable)
	}
}

func TestGroupstoreMoveMember(t *testing.T) {
	s := groupstore.Open(t, 10, 20)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	want := groupstoreEdgeShapes()
	if err := s.Seed(ctx, want); err != nil {
		t.Fatal(err)
	}
	assertGroupstoreEdgeState(t, ctx, s, want)

	if err := s.MoveMember(ctx, "m1", "g1", "g2"); err != nil {
		t.Fatalf("MoveMember = %v", err)
	}
	want[0].Members = []groupstore.Member{{Name: "m2", Rev: 8}}
	want[1].Members = []groupstore.Member{{Name: "m1", Rev: 7}, {Name: "m3", Rev: 9}}
	assertGroupstoreEdgeState(t, ctx, s, want)
}

func TestGroupstoreMoveMemberNothingCommitted(t *testing.T) {
	for _, tc := range []struct {
		name, member, from, to string
		wantNotFound           bool
	}{
		{name: "wrong source", member: "m1", from: "g2", to: "g3", wantNotFound: true},
		{name: "missing target", member: "m1", from: "g1", to: "missing"},
		{name: "deleted target", member: "m1", from: "g1", to: "g4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := groupstore.Open(t, 10, 20)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			want := groupstoreEdgeShapes()
			if err := s.Seed(ctx, want); err != nil {
				t.Fatal(err)
			}
			before, err := s.Dump(ctx)
			if err != nil {
				t.Fatal(err)
			}
			err = s.MoveMember(ctx, tc.member, tc.from, tc.to)
			if err == nil || !groupstore.CommittedNothing(err) {
				t.Fatalf("MoveMember = %v; want a nothing-committed error", err)
			}
			if tc.wantNotFound && !errors.Is(err, groupstore.ErrNotFound) {
				t.Fatalf("MoveMember = %v; want ErrNotFound", err)
			}
			after, err := s.Dump(ctx)
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("Dump after failed move = %v, %v; before = %v", after, err, before)
			}
			assertGroupstoreEdgeState(t, ctx, s, want)
		})
	}
}

func TestGroupstoreSoftDelete(t *testing.T) {
	for _, id := range []string{"g1", "g3"} {
		t.Run(id, func(t *testing.T) {
			s := groupstore.Open(t, 10, 20)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			want := groupstoreEdgeShapes()
			if err := s.Seed(ctx, want); err != nil {
				t.Fatal(err)
			}
			assertGroupstoreEdgeState(t, ctx, s, want)
			if err := s.SoftDelete(ctx, id); err != nil {
				t.Fatalf("SoftDelete(%q) = %v", id, err)
			}
			for i := range want {
				if want[i].ID == id {
					want[i].Deleted = true
					want[i].Owner = ""
					want[i].Members = nil
				}
			}
			assertGroupstoreEdgeState(t, ctx, s, want)

			if id == "g1" {
				// Reusing every cleared unique key proves no stale owner/member
				// rows survived the deletion, not merely that reads hide them.
				replacement := groupstore.Group{ID: "g5", Name: "replacement", Owner: "o1", Members: []groupstore.Member{{Name: "m1"}, {Name: "m2"}}}
				if err := s.Insert(ctx, replacement); err != nil {
					t.Fatalf("reuse deleted group's keys: %v", err)
				}
				want = append(want, replacement)
				assertGroupstoreEdgeState(t, ctx, s, want)
			}
		})
	}
}

func TestGroupstoreSoftDeleteMissing(t *testing.T) {
	s := groupstore.Open(t, 10, 20)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	want := groupstoreEdgeShapes()
	if err := s.Seed(ctx, want); err != nil {
		t.Fatal(err)
	}
	before, err := s.Dump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = s.SoftDelete(ctx, "missing")
	if !errors.Is(err, groupstore.ErrNotFound) || !groupstore.CommittedNothing(err) {
		t.Fatalf("SoftDelete(missing) = %v; want nothing-committed ErrNotFound", err)
	}
	after, err := s.Dump(ctx)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("Dump after missing delete = %v, %v; before = %v", after, err, before)
	}
	assertGroupstoreEdgeState(t, ctx, s, want)
}
