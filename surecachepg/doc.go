// SPDX-License-Identifier: Apache-2.0

// Package surecachepg provides PostgreSQL (pgx v5) primitives for writing a correct
// surecache store adapter: the advisory-lock fence, the fenced snapshot reader,
// connection bounds and a nothing-committed error classifier.
package surecachepg
