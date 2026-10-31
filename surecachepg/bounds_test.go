// SPDX-License-Identifier: Apache-2.0

package surecachepg

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestDefaultBounds(t *testing.T) {
	b := DefaultBounds()
	want := Bounds{
		FenceLockTimeout: 1500 * time.Millisecond, FenceStatementTimeout: 1500 * time.Millisecond,
		ReadStatementTimeout: time.Second, MaxReadStatements: 2, IdleInTxTimeout: 5 * time.Second,
		KeepAlive: KeepAlive{Idle: 5 * time.Second, Interval: 2 * time.Second, Count: 3},
	}
	if b != want {
		t.Fatalf("DefaultBounds = %+v", b)
	}
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	// Section 4.6: 1.5 s + 2 s < 4 s, the default rebuild budget.
	if err := CheckBudgets(b, 4*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestBounds_Validate(t *testing.T) {
	cases := []struct {
		name  string
		patch func(*Bounds)
		want  string
	}{
		{"zero fence lock timeout", func(b *Bounds) { b.FenceLockTimeout = 0 }, "FenceLockTimeout"},
		{"negative fence statement timeout", func(b *Bounds) { b.FenceStatementTimeout = -1 }, "FenceStatementTimeout"},
		{"zero read statement timeout", func(b *Bounds) { b.ReadStatementTimeout = 0 }, "ReadStatementTimeout"},
		{"zero idle in transaction timeout", func(b *Bounds) { b.IdleInTxTimeout = 0 }, "IdleInTxTimeout"},
		{"overflowing fence lock timeout", func(b *Bounds) { b.FenceLockTimeout = (math.MaxInt32 + 1) * time.Millisecond }, "FenceLockTimeout"},
		{"overflowing idle timeout", func(b *Bounds) { b.IdleInTxTimeout = time.Duration(math.MaxInt64) }, "IdleInTxTimeout"},
		{"zero max read statements", func(b *Bounds) { b.MaxReadStatements = 0 }, "MaxReadStatements"},
		{"zero keepalive idle", func(b *Bounds) { b.KeepAlive.Idle = 0 }, "KeepAlive.Idle"},
		{"zero keepalive interval", func(b *Bounds) { b.KeepAlive.Interval = 0 }, "KeepAlive.Interval"},
		{"zero keepalive count", func(b *Bounds) { b.KeepAlive.Count = 0 }, "KeepAlive.Count"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := DefaultBounds()
			tc.patch(&b)
			err := b.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want mention of %s", err, tc.want)
			}
		})
	}
	// The largest representable timeout is accepted.
	b := DefaultBounds()
	b.IdleInTxTimeout = math.MaxInt32 * time.Millisecond
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
}

// A positive sub-millisecond duration rounds up to 1 ms, never to 0, which would
// disable a PostgreSQL timeout.
func TestMillis_RoundsUp(t *testing.T) {
	cases := map[time.Duration]int64{
		time.Nanosecond:                          1,
		time.Millisecond:                         1,
		time.Millisecond + time.Nanosecond:       2,
		1500 * time.Millisecond:                  1500,
		1500*time.Millisecond + time.Microsecond: 1501,
	}
	for d, want := range cases {
		if got := millis(d); got != want {
			t.Errorf("millis(%v) = %d, want %d", d, got, want)
		}
	}
}

func TestBounds_FenceWaitAndReadBound(t *testing.T) {
	b := DefaultBounds()
	b.FenceLockTimeout = 2 * time.Second
	b.FenceStatementTimeout = 1500*time.Millisecond + time.Microsecond
	if got := b.FenceWait(); got != 1501*time.Millisecond {
		t.Fatalf("FenceWait = %v", got)
	}
	b.ReadStatementTimeout = 700 * time.Millisecond
	b.MaxReadStatements = 3
	if got := b.ReadBound(); got != 2100*time.Millisecond {
		t.Fatalf("ReadBound = %v", got)
	}
}

// Section 4.6 and plan clarification 13: the aggregate read bound must not wrap.
func TestBounds_ReadBoundOverflow(t *testing.T) {
	cases := []struct {
		name    string
		timeout time.Duration
		count   int
	}{
		{"huge count", math.MaxInt32 * time.Millisecond, math.MaxInt},
		{"huge duration", time.Duration(math.MaxInt64), 1},
		{"duration product", time.Duration(math.MaxInt64 / 2), 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := DefaultBounds()
			b.ReadStatementTimeout = tc.timeout
			b.MaxReadStatements = tc.count
			if got := b.ReadBound(); got != time.Duration(math.MaxInt64) {
				t.Fatalf("ReadBound = %v, want saturation", got)
			}
			if err := b.Validate(); !errors.Is(err, ErrInvalidBounds) {
				t.Fatalf("Validate = %v, want ErrInvalidBounds", err)
			}
		})
	}
}

// Section 4.6: a one-nanosecond increase rounds every statement up by a
// millisecond and can overflow an otherwise representable aggregate.
func TestBounds_ReadBoundRoundingCrossesLimit(t *testing.T) {
	b := DefaultBounds()
	b.ReadStatementTimeout = 10 * time.Second
	b.MaxReadStatements = 922337203
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := b.ReadBound(); got != 9223372030*time.Second {
		t.Fatalf("ReadBound = %v", got)
	}
	b.ReadStatementTimeout++
	if got := b.ReadBound(); got != time.Duration(math.MaxInt64) {
		t.Fatalf("rounded ReadBound = %v, want saturation", got)
	}
	if err := b.Validate(); !errors.Is(err, ErrInvalidBounds) {
		t.Fatalf("Validate rounded overflow = %v", err)
	}
}

// Section 4.6: invalid inputs must not produce a wrapped read bound.
func TestBounds_ReadBoundNonpositive(t *testing.T) {
	for _, patch := range []func(*Bounds){
		func(b *Bounds) { b.ReadStatementTimeout = 0 },
		func(b *Bounds) { b.ReadStatementTimeout = time.Duration(math.MinInt64) },
		func(b *Bounds) { b.MaxReadStatements = 0 },
		func(b *Bounds) { b.MaxReadStatements = math.MinInt },
	} {
		b := DefaultBounds()
		patch(&b)
		if got := b.ReadBound(); got != 0 {
			t.Errorf("ReadBound = %v, want zero for nonpositive input", got)
		}
	}
}

// Section 4.6: even representable individual bounds can overflow when added.
func TestCheckBudgets_LargeBoundary(t *testing.T) {
	b := DefaultBounds()
	b.ReadStatementTimeout = math.MaxInt32 * time.Millisecond
	b.MaxReadStatements = int(int64(math.MaxInt64) / int64(b.ReadStatementTimeout))
	b.FenceLockTimeout = b.ReadStatementTimeout
	b.FenceStatementTimeout = b.ReadStatementTimeout
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := CheckBudgets(b, time.Duration(math.MaxInt64)); !errors.Is(err, ErrBudget) {
		t.Fatalf("CheckBudgets with overflowing sum = %v, want ErrBudget", err)
	}
	b.FenceLockTimeout = time.Millisecond
	budget := b.ReadBound() + b.FenceWait()
	if err := CheckBudgets(b, budget); !errors.Is(err, ErrBudget) {
		t.Fatalf("CheckBudgets at equality = %v, want ErrBudget", err)
	}
	if err := CheckBudgets(b, budget+1); err != nil {
		t.Fatalf("CheckBudgets one nanosecond above = %v", err)
	}
}

func TestCheckBudgets(t *testing.T) {
	cases := []struct {
		name    string
		patch   func(*Bounds)
		budget  time.Duration
		wantErr error
	}{
		{"defaults fit", func(*Bounds) {}, 4 * time.Second, nil},
		{"zero budget", func(*Bounds) {}, 0, ErrBudget},
		{"negative budget", func(*Bounds) {}, -time.Second, ErrBudget},
		{"minimum budget", func(*Bounds) {}, time.Duration(math.MinInt64), ErrBudget},
		{"equal is not less", func(*Bounds) {}, 3500 * time.Millisecond, ErrBudget},
		{"read bound counts every statement", func(b *Bounds) { b.MaxReadStatements = 3 }, 4 * time.Second, ErrBudget},
		{"rounding is applied", func(b *Bounds) {
			b.FenceLockTimeout = 1500*time.Millisecond + time.Nanosecond
			b.FenceStatementTimeout = b.FenceLockTimeout
		}, 3501 * time.Millisecond, ErrBudget},
		{"invalid bounds", func(b *Bounds) { b.MaxReadStatements = 0 }, time.Hour, ErrInvalidBounds},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := DefaultBounds()
			tc.patch(&b)
			err := CheckBudgets(b, tc.budget)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("CheckBudgets = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// Section 4.7: SET LOCAL takes no bind parameters, so values are formatted from the
// validated, rounded integers.
func TestSetLocal_Literal(t *testing.T) {
	got := setLocal("lock_timeout", 1500*time.Millisecond+time.Nanosecond)
	if want := "SET LOCAL lock_timeout = '1501ms'"; got != want {
		t.Fatalf("setLocal = %q, want %q", got, want)
	}
}
