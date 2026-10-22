// SPDX-License-Identifier: Apache-2.0

package boundedmap

import (
	"maps"
	"testing"
)

// NFR-1, SAFE-8: a new key at capacity is refused and nothing is stored.
func TestBoundedMap_RefusesNewKeyAtCapacity(t *testing.T) {
	m := New[string, int](2)
	if !m.Put("a", 1) || !m.Put("b", 2) {
		t.Fatal("puts below capacity must succeed")
	}
	if m.Put("c", 3) {
		t.Fatal("put of a new key at capacity must be refused")
	}
	if _, ok := m.Get("c"); ok {
		t.Fatal("refused key must not be stored")
	}
	if m.Len() != 2 || m.Cap() != 2 {
		t.Fatalf("len=%d cap=%d, want 2 and 2", m.Len(), m.Cap())
	}
}

// NFR-1: overwriting an existing key always succeeds, even at capacity.
func TestBoundedMap_OverwriteAtCapacitySucceeds(t *testing.T) {
	m := New[string, int](1)
	m.Put("a", 1)
	if !m.Put("a", 2) {
		t.Fatal("overwrite at capacity must succeed")
	}
	if v, _ := m.Get("a"); v != 2 {
		t.Fatalf("a = %d, want 2", v)
	}
}

func TestBoundedMap_DeleteFreesSlot(t *testing.T) {
	m := New[string, int](1)
	m.Put("a", 1)
	m.Delete("a")
	m.Delete("absent")
	if !m.Put("b", 2) {
		t.Fatal("delete must free a slot")
	}
	if _, ok := m.Get("a"); ok {
		t.Fatal("deleted key still present")
	}
}

func TestBoundedMap_All(t *testing.T) {
	m := NewSized[string, int](3, 100)
	m.Put("a", 1)
	m.Put("b", 2)
	got := maps.Collect(m.All())
	if len(got) != 2 || got["a"] != 1 || got["b"] != 2 {
		t.Fatalf("All = %v", got)
	}
	for range m.All() {
		break // early exit must be honored
	}
}

func TestBoundedMap_NonPositiveCapacityRefusesAll(t *testing.T) {
	if New[string, int](0).Put("a", 1) {
		t.Fatal("capacity 0 must refuse every key")
	}
}
