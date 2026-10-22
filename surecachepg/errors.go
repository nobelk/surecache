// SPDX-License-Identifier: Apache-2.0

package surecachepg

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/nobelk/surecache"
)

// IsNothingCommitted reports whether err is a *pgconn.PgError whose SQLSTATE is one of
// codes, for example "23505" (unique_violation) or "P0001" (raise_exception from a
// precondition check). Use it to build the adapter's surecache.Spec.CommittedNothing.
//
// Call it only on errors from a transaction the adapter rolled back itself, so that the
// error proves nothing committed. Context errors, connection errors, network errors and
// every other error return false: they are ambiguous outcomes (design section 4.2).
func IsNothingCommitted(err error, codes ...string) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && slices.Contains(codes, pgErr.Code)
}

// LimitPlusOne returns capacity+1, the row limit for a bulk-load query: reading one row
// past the capacity detects overflow without reading an unbounded result.
func LimitPlusOne(capacity int) int {
	return capacity + 1
}

// OverCapacity returns an error wrapping surecache.ErrOverCapacity when n rows exceed
// capacity, and nil otherwise. A bulk load that gets an error returns no entities.
func OverCapacity(what string, n, capacity int) error {
	if n <= capacity {
		return nil
	}
	return fmt.Errorf("%w: %s holds more than %d rows", surecache.ErrOverCapacity, what, capacity)
}

// unwrapContext returns ctx.Err() when ctx has ended, so callers can compare context
// errors with ==, and err otherwise.
func unwrapContext(ctx context.Context, err error) error {
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
	}
	return err
}
