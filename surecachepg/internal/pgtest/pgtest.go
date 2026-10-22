// SPDX-License-Identifier: Apache-2.0

// Package pgtest provides PostgreSQL test infrastructure: a fresh database per test and
// a TCP proxy that simulates an outage by resetting connections.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvDSN names the environment variable holding the DSN of a server on which the tests
// may create and drop databases.
const EnvDSN = "SURECACHE_PG_DSN"

// DSN returns the server DSN, or skips the test when it is unset.
func DSN(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv(EnvDSN)
	if dsn == "" {
		t.Skipf("%s is not set; run `make pgtest`", EnvDSN)
	}
	return dsn
}

// DB creates a uniquely named database, returns a pool config for it, and drops it in
// t.Cleanup. Advisory locks are scoped to the database, so tests that hold the fence
// never interfere with each other (D16).
func DB(t testing.TB) *pgxpool.Config {
	t.Helper()
	dsn := DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	name := "surecache_" + randomSuffix(t)
	if _, cerr := admin.Exec(ctx, "CREATE DATABASE "+name); cerr != nil {
		t.Fatalf("create database: %v", cerr)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		conn, derr := pgx.Connect(cctx, dsn)
		if derr != nil {
			t.Errorf("drop database: %v", derr)
			return
		}
		defer func() { _ = conn.Close(context.Background()) }()
		if _, derr = conn.Exec(cctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); derr != nil {
			t.Errorf("drop database: %v", derr)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.ConnConfig.Database = name
	return cfg
}

// Pool opens a pool for cfg and closes it in t.Cleanup.
func Pool(t testing.TB, cfg *pgxpool.Config) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Via returns a copy of cfg that connects through the proxy.
func Via(cfg *pgxpool.Config, p *Proxy) *pgxpool.Config {
	c := cfg.Copy()
	host, port, _ := net.SplitHostPort(p.Addr())
	c.ConnConfig.Host = host
	n, _ := strconv.ParseUint(port, 10, 16)
	c.ConnConfig.Port = uint16(n)
	c.ConnConfig.Fallbacks = nil
	return c
}

func randomSuffix(t testing.TB) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
