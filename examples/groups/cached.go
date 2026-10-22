// SPDX-License-Identifier: Apache-2.0

package groups

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/nobelk/surecache"
)

// CachedRepo is the cached decorator. It implements Repo by explicit delegation, never
// by embedding the store: a method added to Repo later fails to compile here until
// someone classifies it as a read or a mutation (PRE-4).
type CachedRepo struct {
	store    *Store
	cache    *surecache.Cache[string, Group]
	byMember surecache.Index[string, string]
}

var _ Repo = (*CachedRepo)(nil)

// NewCachedRepo builds the cache over store. The caller primes it and runs it.
func NewCachedRepo(store *Store, opts ...surecache.Option) (*CachedRepo, error) {
	b := surecache.NewBuilder(surecache.Spec[string, Group]{
		Name:             "groups",
		Key:              func(g Group) string { return g.ID },
		Clone:            func(g Group) Group { g.Members = slices.Clone(g.Members); return g },
		Cacheable:        func(Group) bool { return true },
		Compare:          strings.Compare,
		Capacity:         store.capacity,
		CommittedNothing: CommittedNothing,
	}, surecache.Source[string, Group](store))
	r := &CachedRepo{
		store:    store,
		byMember: surecache.AddIndex(b, "member", func(g Group) []string { return g.Members }, store.memberCapacity),
	}
	c, err := b.Build(opts...)
	if err != nil {
		return nil, err
	}
	r.cache = c
	return r, nil
}

// Cache returns the cache, for Prime and Run.
func (r *CachedRepo) Cache() *surecache.Cache[string, Group] { return r.cache }

// Group answers from the cache when it can; Defer goes to the store, always.
func (r *CachedRepo) Group(ctx context.Context, id string) (Group, bool, error) {
	if g, answer := r.cache.Get(id); answer == surecache.Found {
		return g, true, nil
	}
	return r.store.Group(ctx, id)
}

// GroupForMember answers from the member index. Absent is authoritative; Defer and an
// error go to the store, never to "no group".
func (r *CachedRepo) GroupForMember(ctx context.Context, member string) (string, bool, error) {
	id, answer, err := surecache.Lookup(r.cache, r.byMember, member)
	switch {
	case err != nil || answer == surecache.Defer:
		if err != nil {
			slog.WarnContext(ctx, "groups: index lookup failed", "err", err)
		}
		return r.store.GroupForMember(ctx, member)
	case answer == surecache.Absent:
		return "", false, nil
	}
	return id, true, nil
}

// Groups answers from the directory when it can.
func (r *CachedRepo) Groups(ctx context.Context) ([]string, error) {
	if ids, answer := r.cache.List(); answer == surecache.Found {
		return ids, nil
	}
	return r.store.Groups(ctx)
}

// CreateGroup inserts one previously nonexistent group: Create, not Mutate.
func (r *CachedRepo) CreateGroup(ctx context.Context, id, name string) error {
	_, err := r.cache.Create(ctx, func() (string, error) { return id, r.store.CreateGroup(ctx, id, name) })
	return err
}

// AddMember changes one group.
func (r *CachedRepo) AddMember(ctx context.Context, group, member string) error {
	return r.cache.Mutate(ctx, []string{group}, func() error { return r.store.AddMember(ctx, group, member) })
}

// MoveMember changes two groups, so it names both.
func (r *CachedRepo) MoveMember(ctx context.Context, member, from, to string) error {
	return r.cache.Mutate(ctx, []string{from, to}, func() error { return r.store.MoveMember(ctx, member, from, to) })
}

// DeleteGroup changes one group.
func (r *CachedRepo) DeleteGroup(ctx context.Context, id string) error {
	return r.cache.Mutate(ctx, []string{id}, func() error { return r.store.DeleteGroup(ctx, id) })
}
