// SPDX-License-Identifier: Apache-2.0

package surecachepg_test

import (
	"testing"

	"github.com/nobelk/surecache/surecachepg/internal/groupstore"
	"github.com/nobelk/surecache/surecachetest"
)

// Section 11.2: the reference store built from the primitives passes the conformance
// suite. CI runs it against PostgreSQL 17 and 18 and forbids skips.
func TestConformance_Groupstore(t *testing.T) {
	surecachetest.Run(t, groupstore.Harness())
}
