// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"cmp"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nobelk/surecache/internal/memstore"
)

// point is a value entity: no pointers, so Clone is a plain copy and allocates nothing.
type point struct {
	ID      int64
	X, Y, Z int64
	Tag     uint64
}

// tagged has a slice field, to show a realistic clone cost.
type tagged struct {
	ID   int64
	Tags []int64
}

func pointKey(p point) int64        { return p.ID }
func pointClone(p point) point      { return p }
func pointCacheable(point) bool     { return true }
func pointX(p point) []int64        { return []int64{p.X} }
func taggedKey(e tagged) int64      { return e.ID }
func taggedClone(e tagged) tagged   { e.Tags = append([]int64(nil), e.Tags...); return e }
func taggedCacheable(tagged) bool   { return true }
func committedNothing(e error) bool { return memstore.CommittedNothing(e) }

// countingObserver counts reads without allocating.
type countingObserver struct {
	NopObserver
	reads atomic.Int64
}

func (o *countingObserver) Read(Method, ReadSource) { o.reads.Add(1) }

// pointCache is a primed cache of n points with a unique index on X.
type pointCache struct {
	c     *Cache[int64, point]
	byX   Index[int64, int64]
	store *memstore.Store[int64, point]
}

func newPointCache(tb testing.TB, n int, opts ...Option) *pointCache {
	tb.Helper()
	store := memstore.New(memstore.Config[int64, point]{Key: pointKey, Clone: pointClone, Cacheable: pointCacheable})
	for i := range int64(n) {
		store.Seed(point{ID: i, X: i * 2})
	}
	b := NewBuilder(Spec[int64, point]{
		Name: "points", Key: pointKey, Clone: pointClone, Cacheable: pointCacheable,
		Compare: cmp.Compare[int64], Capacity: n + 1, CommittedNothing: committedNothing,
	}, Source[int64, point](store))
	byX := AddIndex(b, "x", pointX, n+1)
	c, err := b.Build(append([]Option{WithOutagePolicy(ServeWhileExact())}, opts...)...)
	if err != nil {
		tb.Fatal(err)
	}
	if r, err := c.Prime(context.Background()); r != Installed {
		tb.Fatal(r, err)
	}
	return &pointCache{c: c, byX: byX, store: store}
}

func newTaggedCache(tb testing.TB, n int) *Cache[int64, tagged] {
	tb.Helper()
	store := memstore.New(memstore.Config[int64, tagged]{Key: taggedKey, Clone: taggedClone, Cacheable: taggedCacheable})
	for i := range int64(n) {
		store.Seed(tagged{ID: i, Tags: []int64{i, i + 1, i + 2, i + 3}})
	}
	c, err := NewBuilder(Spec[int64, tagged]{
		Name: "tagged", Key: taggedKey, Clone: taggedClone, Cacheable: taggedCacheable,
		Compare: cmp.Compare[int64], Capacity: n, CommittedNothing: committedNothing,
	}, Source[int64, tagged](store)).Build(WithOutagePolicy(ServeWhileExact()))
	if err != nil {
		tb.Fatal(err)
	}
	if r, err := c.Prime(context.Background()); r != Installed {
		tb.Fatal(r, err)
	}
	return c
}

var sinkPoint point

func BenchmarkGet(b *testing.B) {
	for _, n := range []int{1_000, 100_000} {
		b.Run(fmt.Sprintf("size=%d", n), func(b *testing.B) {
			pc := newPointCache(b, n)
			b.ReportAllocs()
			i := int64(0)
			for b.Loop() {
				sinkPoint, _ = pc.c.Get(i)
				if i++; i == int64(n) {
					i = 0
				}
			}
		})
	}
}

func BenchmarkGetTagged(b *testing.B) {
	c := newTaggedCache(b, 1_000)
	b.ReportAllocs()
	i := int64(0)
	for b.Loop() {
		e, _ := c.Get(i % 1_000)
		_ = e
		i++
	}
}

func BenchmarkGetParallel(b *testing.B) {
	const n = 100_000
	b.Run(fmt.Sprintf("size=%d", n), func(b *testing.B) {
		pc := newPointCache(b, n)
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			i := int64(0)
			for pb.Next() {
				p, _ := pc.c.Get(i % n)
				_ = p
				i += 7
			}
		})
	})
}

func BenchmarkLookup(b *testing.B) {
	pc := newPointCache(b, 1_000)
	for _, tc := range []struct {
		name string
		key  func(i int64) int64
	}{
		{"hit", func(i int64) int64 { return (i % 1_000) * 2 }},
		{"miss", func(i int64) int64 { return (i%1_000)*2 + 1 }},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			i := int64(0)
			for b.Loop() {
				_, _, _ = Lookup(pc.c, pc.byX, tc.key(i))
				i++
			}
		})
	}
}

func BenchmarkList(b *testing.B) {
	for _, n := range []int{1_000, 10_000} {
		b.Run(fmt.Sprintf("size=%d", n), func(b *testing.B) {
			pc := newPointCache(b, n)
			b.ReportAllocs()
			for b.Loop() {
				ids, _ := pc.c.List()
				_ = ids
			}
		})
	}
}

// BenchmarkGetDuringMutations measures parallel reads while one mutator runs.
func BenchmarkGetDuringMutations(b *testing.B) {
	const n = 10_000
	pc := newPointCache(b, n)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := int64(0); ctx.Err() == nil; i = (i + 1) % n {
			_ = pc.c.Mutate(ctx, []int64{i}, func() error {
				return pc.store.Write(ctx, func(tx *memstore.Tx[int64, point]) error {
					return tx.Update(i, func(p point) (point, error) { p.Y++; return p, nil })
				})
			})
		}
	})
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := int64(0)
		for pb.Next() {
			p, _ := pc.c.Get(i % n)
			_ = p
			i += 13
		}
	})
	b.StopTimer()
	cancel()
	wg.Wait()
}

func BenchmarkMutate(b *testing.B) {
	const n = 1_000
	pc := newPointCache(b, n)
	ctx := context.Background()
	b.ReportAllocs()
	i := int64(0)
	for b.Loop() {
		id := i % n
		_ = pc.c.Mutate(ctx, []int64{id}, func() error {
			return pc.store.Write(ctx, func(tx *memstore.Tx[int64, point]) error {
				return tx.Update(id, func(p point) (point, error) { p.Y++; return p, nil })
			})
		})
		i++
	}
}

func BenchmarkRebuild(b *testing.B) {
	for _, n := range []int{1_000, 100_000} {
		b.Run(fmt.Sprintf("size=%d", n), func(b *testing.B) {
			pc := newPointCache(b, n)
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				pc.c.mu.Lock()
				pc.c.generation++
				captured := pc.c.generation
				pc.c.mu.Unlock()
				if r, err := pc.c.rebuild(ctx, captured, TriggerLoop); r != Installed {
					b.Fatal(r, err)
				}
			}
		})
	}
}

// BenchmarkBaselineRWMutexMap is a plain RWMutex-guarded map, for comparison with Get.
func BenchmarkBaselineRWMutexMap(b *testing.B) {
	const n = 1_000
	var mu sync.RWMutex
	m := make(map[int64]point, n)
	for i := range int64(n) {
		m[i] = point{ID: i}
	}
	b.ReportAllocs()
	i := int64(0)
	for b.Loop() {
		mu.RLock()
		sinkPoint = m[i]
		mu.RUnlock()
		if i++; i == n {
			i = 0
		}
	}
}
