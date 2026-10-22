// SPDX-License-Identifier: Apache-2.0

package surecache

import (
	"errors"
	"fmt"
	"testing"
)

func TestAnswer_ZeroValueIsDefer(t *testing.T) {
	var a Answer
	if a != Defer {
		t.Fatalf("zero Answer = %v, want Defer", a)
	}
}

func TestEnums_String(t *testing.T) {
	cases := []struct {
		got  fmt.Stringer
		want string
	}{
		{Defer, "defer"},
		{Found, "found"},
		{Absent, "absent"},
		{Answer(99), "Answer(99)"},
		{Installed, "installed"},
		{Discarded, "discarded"},
		{Failed, "failed"},
		{RebuildResult(99), "RebuildResult(99)"},
		{MethodGet, "get"},
		{MethodLookup, "lookup"},
		{MethodList, "list"},
		{Method(99), "Method(99)"},
		{SourceCache, "cache"},
		{SourceStale, "stale"},
		{SourcePending, "pending"},
		{SourceInFlight, "in_flight"},
		{SourceMiss, "miss"},
		{ReadSource(99), "ReadSource(99)"},
		{TriggerPrime, "prime"},
		{TriggerLoop, "loop"},
		{Trigger(99), "Trigger(99)"},
	}
	for _, tc := range cases {
		if s := tc.got.String(); s != tc.want {
			t.Errorf("String() = %q, want %q", s, tc.want)
		}
	}
}

func TestErrors_Distinct(t *testing.T) {
	all := []error{
		ErrOutcomeUnknown, ErrContract, ErrKeyConflict, ErrOverCapacity, ErrUnsettled,
		ErrForeignIndex, ErrNoIDs, ErrNotPrimed, ErrAlreadyPrimed, ErrAlreadyRunning,
		ErrNoOutagePolicy,
	}
	for i, a := range all {
		for j, b := range all {
			if i != j && errors.Is(a, b) {
				t.Errorf("%v matches %v", a, b)
			}
		}
	}
}
