// SPDX-License-Identifier: Apache-2.0

package surecachepg

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
)

// ErrInvalidBounds reports a bound that would be disabled or out of range on the server.
var ErrInvalidBounds = errors.New("surecachepg: invalid bounds")

// ErrBudget reports bounds whose fence wait plus read bound is not less than the rebuild
// budget (design section 4.6).
var ErrBudget = errors.New("surecachepg: fence wait plus read bound must be less than the rebuild budget")

// KeepAlive configures TCP keepalive on the connection dialer, so a dead peer is
// detected.
type KeepAlive struct {
	Idle     time.Duration // time before the first probe
	Interval time.Duration // time between probes
	Count    int           // unanswered probes before the connection is dropped
}

// Bounds are the server-side and transport bounds of design section 4.6.
type Bounds struct {
	// FenceLockTimeout is lock_timeout in the exclusive fence transaction.
	FenceLockTimeout time.Duration
	// FenceStatementTimeout is statement_timeout in the exclusive fence transaction.
	FenceStatementTimeout time.Duration
	// ReadStatementTimeout is statement_timeout in the snapshot transaction. It bounds
	// each statement, not the transaction.
	ReadStatementTimeout time.Duration
	// MaxReadStatements is how many statements a snapshot callback may run.
	MaxReadStatements int
	// IdleInTxTimeout is idle_in_transaction_session_timeout on every connection, so an
	// abandoned transaction cannot hold the shared fence forever.
	IdleInTxTimeout time.Duration
	// KeepAlive configures the dialer.
	KeepAlive KeepAlive
}

// DefaultBounds returns bounds that satisfy section 4.6 with the default 4 s rebuild
// budget: a 1.5 s fence wait plus a 2 s read bound (1 s per statement, two statements).
func DefaultBounds() Bounds {
	return Bounds{
		FenceLockTimeout:      1500 * time.Millisecond,
		FenceStatementTimeout: 1500 * time.Millisecond,
		ReadStatementTimeout:  time.Second,
		MaxReadStatements:     2,
		IdleInTxTimeout:       5 * time.Second,
		KeepAlive:             KeepAlive{Idle: 5 * time.Second, Interval: 2 * time.Second, Count: 3},
	}
}

// Validate rejects any bound that would be disabled or out of range on the server: every
// duration must be positive and, rounded up to whole milliseconds, at most
// math.MaxInt32 ms; MaxReadStatements and the keepalive values must be positive.
// The aggregate rounded read bound must also fit in a time.Duration.
func (b Bounds) Validate() error {
	var errs []error
	for _, d := range []struct {
		name string
		v    time.Duration
	}{
		{"FenceLockTimeout", b.FenceLockTimeout},
		{"FenceStatementTimeout", b.FenceStatementTimeout},
		{"ReadStatementTimeout", b.ReadStatementTimeout},
		{"IdleInTxTimeout", b.IdleInTxTimeout},
	} {
		switch {
		case d.v <= 0:
			errs = append(errs, fmt.Errorf("%w: %s must be positive", ErrInvalidBounds, d.name))
		case millis(d.v) > math.MaxInt32:
			errs = append(errs, fmt.Errorf("%w: %s exceeds %d ms", ErrInvalidBounds, d.name, math.MaxInt32))
		}
	}
	if b.MaxReadStatements < 1 {
		errs = append(errs, fmt.Errorf("%w: MaxReadStatements must be at least 1", ErrInvalidBounds))
	}
	if b.ReadStatementTimeout > 0 && b.MaxReadStatements > 0 {
		if _, ok := b.checkedReadBound(); !ok {
			errs = append(errs, fmt.Errorf("%w: ReadStatementTimeout times MaxReadStatements exceeds time.Duration", ErrInvalidBounds))
		}
	}
	if b.KeepAlive.Idle <= 0 {
		errs = append(errs, fmt.Errorf("%w: KeepAlive.Idle must be positive", ErrInvalidBounds))
	}
	if b.KeepAlive.Interval <= 0 {
		errs = append(errs, fmt.Errorf("%w: KeepAlive.Interval must be positive", ErrInvalidBounds))
	}
	if b.KeepAlive.Count <= 0 {
		errs = append(errs, fmt.Errorf("%w: KeepAlive.Count must be positive", ErrInvalidBounds))
	}
	return errors.Join(errs...)
}

// FenceWait is the longest the exclusive fence can wait: the smaller of the rounded
// lock and statement timeouts.
func (b Bounds) FenceWait() time.Duration {
	return time.Duration(min(millis(b.FenceLockTimeout), millis(b.FenceStatementTimeout))) * time.Millisecond
}

// ReadBound is the longest the snapshot read can take: the rounded statement timeout
// times MaxReadStatements, since statement_timeout bounds each statement, not the
// transaction. It saturates at math.MaxInt64 if the aggregate cannot be represented
// as a time.Duration; Validate rejects such bounds. A nonpositive timeout or statement
// count returns zero and is also rejected by Validate.
func (b Bounds) ReadBound() time.Duration {
	bound, _ := b.checkedReadBound()
	return bound
}

func (b Bounds) checkedReadBound() (time.Duration, bool) {
	if b.ReadStatementTimeout <= 0 || b.MaxReadStatements <= 0 {
		return 0, false
	}
	ms := millis(b.ReadStatementTimeout)
	// Check before either multiplication, including the conversion to nanoseconds.
	if ms > (math.MaxInt64/int64(time.Millisecond))/int64(b.MaxReadStatements) {
		return time.Duration(math.MaxInt64), false
	}
	return time.Duration(ms*int64(b.MaxReadStatements)) * time.Millisecond, true
}

// CheckBudgets returns an error unless b is valid and FenceWait() + ReadBound() is less
// than rebuildBudget. Call it at startup with the cache's rebuild budget.
func CheckBudgets(b Bounds, rebuildBudget time.Duration) error {
	if err := b.Validate(); err != nil {
		return err
	}
	fence, read := b.FenceWait(), b.ReadBound()
	// Validation makes both bounds positive. Short-circuit before subtracting so
	// even a nonpositive budget cannot underflow, and never form the possibly
	// unrepresentable sum.
	if rebuildBudget <= fence || read >= rebuildBudget-fence {
		return fmt.Errorf("%w: %v + %v >= %v", ErrBudget, fence, read, rebuildBudget)
	}
	return nil
}

// millis rounds d up to whole milliseconds, so a positive sub-millisecond bound becomes
// 1 ms and never 0, which would disable a PostgreSQL timeout.
func millis(d time.Duration) int64 {
	ms := int64(d / time.Millisecond)
	if d%time.Millisecond > 0 {
		ms++ // without overflow, unlike (d + time.Millisecond - 1) near MaxInt64
	}
	return ms
}

// setLocal formats a SET LOCAL statement for a timeout setting. SET LOCAL takes no bind
// parameters, so the value is formatted from the validated, rounded integer, never from
// external input.
func setLocal(name string, d time.Duration) string {
	return "SET LOCAL " + name + " = '" + strconv.FormatInt(millis(d), 10) + "ms'"
}
