// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestBuild_Validation(t *testing.T) {
	valid := groupSpec(10)
	cases := []struct {
		name    string
		spec    func(*Spec[string, group])
		nilSrc  bool
		index   func(*Builder[string, group])
		opts    []Option
		noPol   bool
		wantErr string
		wantIs  error
	}{
		{name: "empty name", spec: func(s *Spec[string, group]) { s.Name = "" }, wantErr: "Spec.Name is empty"},
		{name: "nil key", spec: func(s *Spec[string, group]) { s.Key = nil }, wantErr: "Spec.Key is nil"},
		{name: "nil clone", spec: func(s *Spec[string, group]) { s.Clone = nil }, wantErr: "Spec.Clone is nil"},
		{name: "nil cacheable", spec: func(s *Spec[string, group]) { s.Cacheable = nil }, wantErr: "Spec.Cacheable is nil"},
		{name: "nil compare", spec: func(s *Spec[string, group]) { s.Compare = nil }, wantErr: "Spec.Compare is nil"},
		{name: "nil committed nothing", spec: func(s *Spec[string, group]) { s.CommittedNothing = nil }, wantErr: "Spec.CommittedNothing is nil"},
		{name: "zero capacity", spec: func(s *Spec[string, group]) { s.Capacity = 0 }, wantErr: "Spec.Capacity must be positive"},
		{name: "negative capacity", spec: func(s *Spec[string, group]) { s.Capacity = -1 }, wantErr: "Spec.Capacity must be positive"},
		{name: "nil source", nilSrc: true, wantErr: "nil Source"},
		{name: "empty index name", index: func(b *Builder[string, group]) { AddIndex(b, "", groupMembers, 1) }, wantErr: "index name is empty"},
		{name: "duplicate index name", index: func(b *Builder[string, group]) {
			AddIndex(b, "x", groupMembers, 1)
			AddIndex(b, "x", groupOwner, 1)
		}, wantErr: `duplicate index name "x"`},
		{name: "nil index keys", index: func(b *Builder[string, group]) { AddIndex[string, group, string](b, "x", nil, 1) }, wantErr: `index "x" has nil keys`},
		{name: "zero index capacity", index: func(b *Builder[string, group]) { AddIndex(b, "x", groupMembers, 0) }, wantErr: `index "x" capacity must be positive`},
		{name: "missing outage policy", noPol: true, wantIs: ErrNoOutagePolicy},
		{name: "zero prime budget", opts: []Option{WithPrimeBudget(0)}, wantErr: "prime budget must be positive"},
		{name: "negative rebuild budget", opts: []Option{WithRebuildBudget(-time.Second)}, wantErr: "rebuild budget must be positive"},
		{name: "zero refresh budget", opts: []Option{WithRefreshBudget(0)}, wantErr: "refresh budget must be positive"},
		{name: "zero rebuild interval", opts: []Option{WithRebuildInterval(0)}, wantErr: "rebuild interval must be positive"},
		{name: "nil tick context", opts: []Option{WithTickContext(nil)}, wantErr: "WithTickContext(nil)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := valid
			if tc.spec != nil {
				tc.spec(&spec)
			}
			var src Source[string, group] = newGroupStore()
			if tc.nilSrc {
				src = nil
			}
			b := NewBuilder(spec, src)
			if tc.index != nil {
				tc.index(b)
			}
			opts := tc.opts
			if !tc.noPol {
				opts = append([]Option{WithOutagePolicy(testPolicy{})}, opts...)
			}
			c, err := b.Build(opts...)
			if err == nil || c != nil {
				t.Fatalf("Build = %v, %v; want error", c, err)
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("err = %v, want errors.Is %v", err, tc.wantIs)
			}
		})
	}
}

func TestBuild_ReportsEveryErrorAtOnce(t *testing.T) {
	spec := groupSpec(0)
	spec.Name = ""
	b := NewBuilder(spec, nil)
	AddIndex(b, "", groupMembers, 0)
	_, err := b.Build(WithPrimeBudget(0))
	for _, want := range []string{"Spec.Name", "Spec.Capacity", "nil Source", "index name is empty", "capacity must be positive", "prime budget"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err missing %q: %v", want, err)
		}
	}
	if !errors.Is(err, ErrNoOutagePolicy) {
		t.Error("err must include ErrNoOutagePolicy")
	}
}

func TestBuild_Twice(t *testing.T) {
	b := NewBuilder(groupSpec(10), Source[string, group](newGroupStore()))
	if _, err := b.Build(WithOutagePolicy(testPolicy{})); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Build(WithOutagePolicy(testPolicy{})); err == nil {
		t.Fatal("second Build must fail")
	}
}

func TestAddIndex_AfterBuildPanics(t *testing.T) {
	b := NewBuilder(groupSpec(10), Source[string, group](newGroupStore()))
	if _, err := b.Build(WithOutagePolicy(testPolicy{})); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "after Build") {
			t.Fatalf("recover = %v", r)
		}
	}()
	AddIndex(b, "late", groupMembers, 1)
}

func TestBuild_OptionsApplied(t *testing.T) {
	tick := func(ctx context.Context) context.Context { return ctx }
	b := NewBuilder(groupSpec(10), Source[string, group](newGroupStore()))
	c, err := b.Build(nil, WithOutagePolicy(testPolicy{}), WithObserver(nil), WithLogger(nil),
		WithTickContext(tick), WithPrimeBudget(time.Second), WithRebuildBudget(2*time.Second),
		WithRefreshBudget(3*time.Second), WithRebuildInterval(4*time.Second), WithCloneOnInstall())
	if err != nil {
		t.Fatal(err)
	}
	cfg := c.cfg
	if cfg.primeBudget != time.Second || cfg.rebuildBudget != 2*time.Second ||
		cfg.refreshBudget != 3*time.Second || cfg.rebuildInterval != 4*time.Second ||
		!cfg.cloneOnInstall || cfg.logger == nil {
		t.Fatalf("cfg = %+v", cfg)
	}
	if _, ok := cfg.observer.(NopObserver); !ok {
		t.Fatalf("nil observer must become NopObserver, got %T", cfg.observer)
	}
}

func TestBuild_Defaults(t *testing.T) {
	cfg := defaultConfig()
	if cfg.primeBudget != 10*time.Second || cfg.rebuildBudget != 4*time.Second ||
		cfg.refreshBudget != 2*time.Second || cfg.rebuildInterval != time.Second || cfg.cloneOnInstall {
		t.Fatalf("defaults = %+v", cfg)
	}
	ctx := context.WithValue(context.Background(), ctxKey{}, 1)
	if cfg.tickContext(ctx) != ctx {
		t.Fatal("default tick context must be the identity")
	}
}

type ctxKey struct{}
