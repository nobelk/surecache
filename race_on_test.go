// SPDX-License-Identifier: Apache-2.0

//go:build race

package surecache

// raceEnabled reports whether the race detector is on; it changes allocation counts.
const raceEnabled = true
