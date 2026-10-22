// SPDX-License-Identifier: Apache-2.0

// Package promobs adapts the surecache Observer interface to Prometheus collectors with
// closed label sets, so an idle scrape reads zeros rather than absent series.
package promobs
