// SPDX-License-Identifier: Apache-2.0

// Package groupstore is a test-only PostgreSQL store built with the surecachepg
// primitives: groups and their members in two tables, so a load spans two statements.
// The schema enforces the caller preconditions: a member belongs to at most one group
// and an owner to at most one group (PRE-2), and a soft-deleted group holds no owner and
// no members (PRE-3).
package groupstore

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nobelk/surecache"
	"github.com/nobelk/surecache/surecachepg"
)

// Schema creates the two tables. The composite foreign key cascades a group's deleted
// flag into its members, where a CHECK rejects it: soft-deleting a group that still has
// members fails, so a non-cacheable group can never hold a member key.
const Schema = `
CREATE TABLE groups (
	id      text PRIMARY KEY,
	name    text NOT NULL,
	owner   text UNIQUE,
	deleted boolean NOT NULL DEFAULT false,
	rev     integer NOT NULL DEFAULT 0,
	UNIQUE (id, deleted),
	CHECK (NOT deleted OR owner IS NULL)
);
CREATE TABLE members (
	member        text PRIMARY KEY,
	group_id      text NOT NULL,
	group_deleted boolean NOT NULL DEFAULT false CHECK (NOT group_deleted),
	rev           integer NOT NULL DEFAULT 0,
	FOREIGN KEY (group_id, group_deleted) REFERENCES groups (id, deleted)
		ON UPDATE CASCADE ON DELETE CASCADE
);`

// Nothing-committed sentinels, each returned only from a rolled-back transaction.
var (
	ErrNotFound     = errors.New("groupstore: not found")
	ErrPrecondition = errors.New("groupstore: precondition failed")
)

// committedCodes are the SQLSTATEs a rolled-back mutation may fail with: unique,
// foreign-key and check violations.
var committedCodes = []string{"23505", "23503", "23514"}

// CommittedNothing is the store's surecache.Spec.CommittedNothing classifier.
func CommittedNothing(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, ErrPrecondition) ||
		surecachepg.IsNothingCommitted(err, committedCodes...)
}

// Member is one row of the members table.
type Member struct {
	Name string
	Rev  int
}

// Group is the cached entity: a groups row and its members, sorted by name.
type Group struct {
	ID      string
	Name    string
	Owner   string
	Deleted bool
	Rev     int
	Members []Member
}

// Clone returns a deep copy.
func Clone(g Group) Group {
	g.Members = slices.Clone(g.Members)
	return g
}

// Cacheable reports whether g is held: soft-deleted groups are not.
func Cacheable(g Group) bool { return !g.Deleted }

// MemberKeys returns the member index keys.
func MemberKeys(g Group) []string {
	out := make([]string, len(g.Members))
	for i, m := range g.Members {
		out[i] = m.Name
	}
	return out
}

// OwnerKeys returns the owner index key, if any.
func OwnerKeys(g Group) []string {
	if g.Owner == "" {
		return nil
	}
	return []string{g.Owner}
}

// Spec returns the cache spec for groups.
func Spec(capacity int) surecache.Spec[string, Group] {
	return surecache.Spec[string, Group]{
		Name:             "groups",
		Key:              func(g Group) string { return g.ID },
		Clone:            Clone,
		Cacheable:        Cacheable,
		Compare:          strings.Compare,
		Capacity:         capacity,
		CommittedNothing: CommittedNothing,
	}
}

// Store is the groups store.
type Store struct {
	Pool           *pgxpool.Pool
	Fence          surecachepg.Fence
	Bounds         surecachepg.Bounds
	Capacity       int // groups LoadAll may return
	MemberCapacity int // member rows LoadAll may return

	pause atomic.Pointer[func()]
}

// Install creates the schema.
func Install(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, Schema)
	return err
}

// SetReadPause installs a function called between the two statements of LoadOne and
// LoadAll (test builds only); nil removes it.
func (s *Store) SetReadPause(pause func()) {
	if pause == nil {
		s.pause.Store(nil)
		return
	}
	s.pause.Store(&pause)
}

func (s *Store) betweenStatements() {
	if p := s.pause.Load(); p != nil {
		(*p)()
	}
}

// querier is the read interface LoadOne and LoadAll share.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// LoadOne reads one group, cacheable or not, from one REPEATABLE READ snapshot.
func (s *Store) LoadOne(ctx context.Context, id string) (Group, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Group{}, unwrap(ctx, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	g, err := readGroup(ctx, tx, id, s.betweenStatements)
	if err != nil {
		return Group{}, unwrap(ctx, err)
	}
	return g, unwrap(ctx, tx.Commit(ctx))
}

// readGroup reads one group in two statements, calling between in between.
func readGroup(ctx context.Context, q querier, id string, between func()) (Group, error) {
	var g Group
	var owner *string
	err := q.QueryRow(ctx, `SELECT id, name, owner, deleted, rev FROM groups WHERE id = $1`, id).
		Scan(&g.ID, &g.Name, &owner, &g.Deleted, &g.Rev)
	if errors.Is(err, pgx.ErrNoRows) {
		return Group{}, ErrNotFound
	}
	if err != nil {
		return Group{}, err
	}
	if owner != nil {
		g.Owner = *owner
	}
	between()
	rows, err := q.Query(ctx, `SELECT member, rev FROM members WHERE group_id = $1 ORDER BY member`, id)
	if err != nil {
		return Group{}, err
	}
	g.Members, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Member, error) {
		var m Member
		return m, r.Scan(&m.Name, &m.Rev)
	})
	if len(g.Members) == 0 {
		g.Members = nil // a group without members reads the same from LoadOne and LoadAll
	}
	return g, err
}

// NotFound reports whether a LoadOne error means the group does not exist.
func (s *Store) NotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// LoadAll reads every cacheable group from one snapshot taken after the fence. It reads
// one row past each capacity and refuses with no groups on overflow.
func (s *Store) LoadAll(ctx context.Context) ([]Group, error) {
	var out []Group
	err := surecachepg.ReadSnapshot(ctx, s.Pool, s.Fence, s.Bounds, func(q surecachepg.SnapshotQuerier) error {
		rows, err := q.Query(ctx, `SELECT id, name, owner, deleted, rev FROM groups
			WHERE NOT deleted ORDER BY id LIMIT $1`, surecachepg.LimitPlusOne(s.Capacity))
		if err != nil {
			return err
		}
		groups, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Group, error) {
			var g Group
			var owner *string
			serr := r.Scan(&g.ID, &g.Name, &owner, &g.Deleted, &g.Rev)
			if owner != nil {
				g.Owner = *owner
			}
			return g, serr
		})
		if err != nil {
			return err
		}
		if cerr := surecachepg.OverCapacity("groups", len(groups), s.Capacity); cerr != nil {
			return cerr
		}
		s.betweenStatements()
		rows, err = q.Query(ctx, `SELECT m.group_id, m.member, m.rev FROM members m
			JOIN groups g ON g.id = m.group_id WHERE NOT g.deleted
			ORDER BY m.member LIMIT $1`, surecachepg.LimitPlusOne(s.MemberCapacity))
		if err != nil {
			return err
		}
		type row struct {
			group string
			m     Member
		}
		members, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
			var x row
			return x, r.Scan(&x.group, &x.m.Name, &x.m.Rev)
		})
		if err != nil {
			return err
		}
		if err := surecachepg.OverCapacity("members", len(members), s.MemberCapacity); err != nil {
			return err
		}
		byID := make(map[string]int, len(groups))
		for i, g := range groups {
			byID[g.ID] = i
		}
		for _, x := range members {
			if i, ok := byID[x.group]; ok {
				groups[i].Members = append(groups[i].Members, x.m)
			}
		}
		out = groups
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func unwrap(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// mutate runs fn through surecachepg.WithMutation with the shared fence held.
func (s *Store) mutate(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return surecachepg.WithMutation(ctx, s.Pool, s.Fence, fn)
}

// exec runs one statement and returns ErrNotFound when it changed no row.
func exec(ctx context.Context, tx pgx.Tx, sql string, args ...any) error {
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Insert creates a group with its members.
func (s *Store) Insert(ctx context.Context, g Group) error {
	return s.mutate(ctx, func(tx pgx.Tx) error { return insert(ctx, tx, g) })
}

func insert(ctx context.Context, tx pgx.Tx, g Group) error {
	var owner *string
	if g.Owner != "" {
		owner = &g.Owner
	}
	if _, err := tx.Exec(ctx, `INSERT INTO groups (id, name, owner, deleted, rev) VALUES ($1, $2, $3, $4, $5)`,
		g.ID, g.Name, owner, g.Deleted, g.Rev); err != nil {
		return err
	}
	for _, m := range g.Members {
		if _, err := tx.Exec(ctx, `INSERT INTO members (member, group_id, rev) VALUES ($1, $2, $3)`,
			m.Name, g.ID, m.Rev); err != nil {
			return err
		}
	}
	return nil
}

// Seed inserts groups directly, in one transaction, without the fence.
func (s *Store) Seed(ctx context.Context, gs []Group) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	for _, g := range gs {
		if err := insert(ctx, tx, g); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Touch renames a group and bumps the revision of the group and of every member, so
// both tables change.
func Touch(ctx context.Context, tx pgx.Tx, id string) error {
	if err := exec(ctx, tx, `UPDATE groups SET name = name || '+', rev = rev + 1 WHERE id = $1`, id); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE members SET rev = rev + 1 WHERE group_id = $1`, id)
	return err
}

// Rename bumps a group through Touch.
func (s *Store) Rename(ctx context.Context, id string) error {
	return s.mutate(ctx, func(tx pgx.Tx) error { return Touch(ctx, tx, id) })
}

// AddMember adds a member to a cacheable group.
func (s *Store) AddMember(ctx context.Context, group, member string) error {
	return s.mutate(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO members (member, group_id) VALUES ($1, $2)`, member, group)
		return err
	})
}

// MoveMember moves a member between groups. Pass both ids to Mutate.
func (s *Store) MoveMember(ctx context.Context, member, from, to string) error {
	return s.mutate(ctx, func(tx pgx.Tx) error {
		return exec(ctx, tx, `UPDATE members SET group_id = $3 WHERE member = $1 AND group_id = $2`, member, from, to)
	})
}

// SetOwner sets a cacheable group's owner.
func (s *Store) SetOwner(ctx context.Context, id, owner string) error {
	return s.mutate(ctx, func(tx pgx.Tx) error {
		err := exec(ctx, tx, `UPDATE groups SET owner = $2 WHERE id = $1 AND NOT deleted`, id, owner)
		if errors.Is(err, ErrNotFound) {
			return ErrPrecondition
		}
		return err
	})
}

// SoftDelete marks a group deleted, clearing its owner and members (PRE-3).
func (s *Store) SoftDelete(ctx context.Context, id string) error {
	return s.mutate(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM members WHERE group_id = $1`, id); err != nil {
			return err
		}
		return exec(ctx, tx, `UPDATE groups SET deleted = true, owner = NULL WHERE id = $1`, id)
	})
}

// Delete removes a group and its members.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.mutate(ctx, func(tx pgx.Tx) error { return exec(ctx, tx, `DELETE FROM groups WHERE id = $1`, id) })
}

// Dump returns every row of both tables, ordered, for equality checks.
func (s *Store) Dump(ctx context.Context) (any, error) {
	var dump [][]any
	for _, sql := range []string{
		`SELECT id, name, owner, deleted, rev FROM groups ORDER BY id`,
		`SELECT member, group_id, group_deleted, rev FROM members ORDER BY member`,
	} {
		rows, err := s.Pool.Query(ctx, sql)
		if err != nil {
			return nil, err
		}
		vals, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) ([]any, error) { return r.Values() })
		if err != nil {
			return nil, err
		}
		dump = append(dump, []any{vals})
	}
	return dump, nil
}
