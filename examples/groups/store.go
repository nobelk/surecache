// SPDX-License-Identifier: Apache-2.0

// Package groups is a runnable example of the decorator pattern of design section 10.3:
// a PostgreSQL repository of groups and members, and a cached decorator that implements
// the same Repo interface, delegates every method explicitly, and routes reads through
// surecache and writes through Mutate or Create.
package groups

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nobelk/surecache/surecachepg"
)

// Repo is the application's repository interface. Both the store and the cached
// decorator implement it.
type Repo interface {
	// Group returns a group, or false when it does not exist.
	Group(ctx context.Context, id string) (Group, bool, error)
	// GroupForMember returns the group a member belongs to, or false.
	GroupForMember(ctx context.Context, member string) (string, bool, error)
	// Groups returns every group id, sorted.
	Groups(ctx context.Context) ([]string, error)
	// CreateGroup inserts a new, empty group.
	CreateGroup(ctx context.Context, id, name string) error
	// AddMember adds a member that belongs to no group yet.
	AddMember(ctx context.Context, group, member string) error
	// MoveMember moves a member from one group to another.
	MoveMember(ctx context.Context, member, from, to string) error
	// DeleteGroup deletes a group and its members.
	DeleteGroup(ctx context.Context, id string) error
}

// Group is a group and its members, sorted.
type Group struct {
	ID      string
	Name    string
	Members []string
}

// The store's nothing-committed errors: each is returned only from a transaction that
// rolled back.
var (
	ErrNotFound    = errors.New("groups: not found")
	ErrExists      = errors.New("groups: already exists")
	ErrMemberTaken = errors.New("groups: member belongs to another group")
)

// CommittedNothing classifies the store's errors for surecache.Spec.CommittedNothing.
func CommittedNothing(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, ErrExists) || errors.Is(err, ErrMemberTaken)
}

// Schema creates the tables. members.member is the primary key, so a member belongs to
// at most one group (PRE-2).
const Schema = `
CREATE TABLE groups (
	id   text PRIMARY KEY,
	name text NOT NULL
);
CREATE TABLE members (
	member   text PRIMARY KEY,
	group_id text NOT NULL REFERENCES groups (id) ON DELETE CASCADE
);`

// Store is the PostgreSQL repository. Every write holds the cache's fence shared, and
// LoadAll reads after taking it exclusively, so it also serves as the cache's Source.
type Store struct {
	pool           *pgxpool.Pool
	fence          surecachepg.Fence
	bounds         surecachepg.Bounds
	capacity       int
	memberCapacity int
}

var _ Repo = (*Store)(nil)

// NewStore returns a store over pool. capacity and memberCapacity bound the groups and
// the members the cache holds.
func NewStore(pool *pgxpool.Pool, fence surecachepg.Fence, bounds surecachepg.Bounds, capacity, memberCapacity int) *Store {
	return &Store{pool: pool, fence: fence, bounds: bounds, capacity: capacity, memberCapacity: memberCapacity}
}

// Group reads a group from the store.
func (s *Store) Group(ctx context.Context, id string) (Group, bool, error) {
	g, err := s.LoadOne(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return Group{}, false, nil
	}
	return g, err == nil, err
}

// GroupForMember reads a member's group from the store.
func (s *Store) GroupForMember(ctx context.Context, member string) (string, bool, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT group_id FROM members WHERE member = $1`, member).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// Groups reads every group id from the store.
func (s *Store) Groups(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM groups ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// CreateGroup inserts a group.
func (s *Store) CreateGroup(ctx context.Context, id, name string) error {
	return s.mutate(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO groups (id, name) VALUES ($1, $2)`, id, name)
		if surecachepg.IsNothingCommitted(err, "23505") {
			return ErrExists
		}
		return err
	})
}

// AddMember adds a member to a group.
func (s *Store) AddMember(ctx context.Context, group, member string) error {
	return s.mutate(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO members (member, group_id) VALUES ($1, $2)`, member, group)
		switch {
		case surecachepg.IsNothingCommitted(err, "23505"):
			return ErrMemberTaken
		case surecachepg.IsNothingCommitted(err, "23503"):
			return ErrNotFound
		}
		return err
	})
}

// MoveMember moves a member between groups.
func (s *Store) MoveMember(ctx context.Context, member, from, to string) error {
	return s.mutate(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE members SET group_id = $3 WHERE member = $1 AND group_id = $2`, member, from, to)
		switch {
		case surecachepg.IsNothingCommitted(err, "23503"):
			return ErrNotFound
		case err != nil:
			return err
		case tag.RowsAffected() == 0:
			return ErrNotFound
		}
		return nil
	})
}

// DeleteGroup deletes a group and its members.
func (s *Store) DeleteGroup(ctx context.Context, id string) error {
	return s.mutate(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM groups WHERE id = $1`, id)
		if err == nil && tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return err
	})
}

func (s *Store) mutate(ctx context.Context, fn func(pgx.Tx) error) error {
	return surecachepg.WithMutation(ctx, s.pool, s.fence, fn)
}

// LoadOne reads one group in one REPEATABLE READ snapshot (surecache.Source).
func (s *Store) LoadOne(ctx context.Context, id string) (Group, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Group{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	g := Group{ID: id}
	if err = tx.QueryRow(ctx, `SELECT name FROM groups WHERE id = $1`, id).Scan(&g.Name); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Group{}, ErrNotFound
		}
		return Group{}, err
	}
	rows, err := tx.Query(ctx, `SELECT member FROM members WHERE group_id = $1 ORDER BY member`, id)
	if err != nil {
		return Group{}, err
	}
	if g.Members, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return Group{}, err
	}
	if len(g.Members) == 0 {
		g.Members = nil // as LoadAll returns it
	}
	return g, tx.Commit(ctx)
}

// NotFound reports whether a LoadOne error means the group does not exist.
func (s *Store) NotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// LoadAll reads every group after the fence, from one snapshot (surecache.Source).
func (s *Store) LoadAll(ctx context.Context) ([]Group, error) {
	var out []Group
	err := surecachepg.ReadSnapshot(ctx, s.pool, s.fence, s.bounds, func(q surecachepg.SnapshotQuerier) error {
		rows, err := q.Query(ctx, `SELECT id, name FROM groups ORDER BY id LIMIT $1`, surecachepg.LimitPlusOne(s.capacity))
		if err != nil {
			return err
		}
		groups, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Group, error) {
			var g Group
			return g, r.Scan(&g.ID, &g.Name)
		})
		if err != nil {
			return err
		}
		if cerr := surecachepg.OverCapacity("groups", len(groups), s.capacity); cerr != nil {
			return cerr
		}
		rows, err = q.Query(ctx, `SELECT group_id, member FROM members ORDER BY member LIMIT $1`,
			surecachepg.LimitPlusOne(s.memberCapacity))
		if err != nil {
			return err
		}
		type row struct{ group, member string }
		members, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
			var x row
			return x, r.Scan(&x.group, &x.member)
		})
		if err != nil {
			return err
		}
		if cerr := surecachepg.OverCapacity("members", len(members), s.memberCapacity); cerr != nil {
			return cerr
		}
		byID := make(map[string]int, len(groups))
		for i, g := range groups {
			byID[g.ID] = i
		}
		for _, m := range members {
			if i, ok := byID[m.group]; ok {
				groups[i].Members = append(groups[i].Members, m.member)
			}
		}
		out = groups
		return nil
	})
	return out, err
}
