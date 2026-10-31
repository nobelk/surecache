// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"

	"github.com/nobelk/surecache/examples/groups"
	"github.com/nobelk/surecache/promobs"
	"github.com/nobelk/surecache/surecachepg"
)

// Unused repository operations panic rather than silently satisfying a request.
type memberRepo struct {
	groups.Repo
	lookup func(context.Context, string) (string, bool, error)
}

func (r memberRepo) GroupForMember(ctx context.Context, member string) (string, bool, error) {
	return r.lookup(ctx, member)
}

func TestHandlerMember(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "request")
	repo := memberRepo{lookup: func(got context.Context, member string) (string, bool, error) {
		if got.Value(contextKey{}) != "request" || member != "ann" {
			t.Fatalf("lookup lost request context or member: %v, %q", got, member)
		}
		return "red", true, nil
	}}
	response := httptest.NewRecorder()
	newHandler(repo, prometheus.NewRegistry()).ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/member/ann", nil).WithContext(ctx))
	if response.Code != http.StatusOK || response.Body.String() != "{\"group\":\"red\"}\n" || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("response = %d %v %q", response.Code, response.Header(), response.Body.String())
	}
}

func TestHandlerErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ok     bool
		err    error
		status int
		body   string
	}{
		{"absent", false, nil, http.StatusNotFound, "404 page not found\n"},
		{"store error", false, errors.New("secret database details"), http.StatusServiceUnavailable, "store unavailable\n"},
		{"error wins over result", true, errors.New("secret"), http.StatusServiceUnavailable, "store unavailable\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := memberRepo{lookup: func(context.Context, string) (string, bool, error) {
				return "red", tc.ok, tc.err
			}}
			response := httptest.NewRecorder()
			newHandler(repo, prometheus.NewRegistry()).ServeHTTP(response, httptest.NewRequest("GET", "/member/ann", nil))
			if response.Code != tc.status || response.Body.String() != tc.body {
				t.Fatalf("response = %d %q", response.Code, response.Body.String())
			}
		})
	}
}

func TestHandlerMetricsAndRouting(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics, err := promobs.New(reg)
	if err != nil {
		t.Fatal(err)
	}
	metrics.Observer("groups")
	handler := newHandler(memberRepo{}, reg)
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/metrics", http.StatusOK},
		{"POST", "/member/ann", http.StatusMethodNotAllowed},
		{"POST", "/metrics", http.StatusMethodNotAllowed},
		{"GET", "/unknown", http.StatusNotFound},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
			if response.Code != tc.status {
				t.Fatalf("status = %d", response.Code)
			}
			if tc.path == "/metrics" && tc.method == "GET" && !strings.Contains(response.Body.String(), `surecache_stale{cache="groups"}`) {
				t.Fatalf("metrics missing groups collector: %s", response.Body.String())
			}
		})
	}
}

// exerciseBlockedRequest always releases and joins the handler, server and client,
// including when an assertion fails before the normal shutdown path.
func exerciseBlockedRequest(t *testing.T, lookup func(context.Context, <-chan struct{}), check func(*http.Server, context.CancelFunc, func(), <-chan error)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	g, gctx := errgroup.WithContext(ctx)
	release := make(chan struct{})
	var releaseOnce sync.Once
	started, handlerDone := make(chan struct{}), make(chan struct{})
	repo := memberRepo{lookup: func(ctx context.Context, _ string) (string, bool, error) {
		defer close(handlerDone)
		close(started)
		lookup(ctx, release)
		<-release
		return "", false, ctx.Err()
	}}
	srv := newHTTPServer(gctx, newHandler(repo, prometheus.NewRegistry()))
	listener := testListener(t)
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(listener) }()
	client := &http.Client{Timeout: 5 * time.Second}
	clientDone := make(chan error, 1)
	clientResult := make(chan error, 1)
	go func() {
		response, err := client.Get("http://" + listener.Addr().String() + "/member/ann")
		if response != nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		clientResult <- err
		clientDone <- err
	}()
	// Closing the parent context isn't the only cancellation source: exercise
	// errgroup failure, as happens when the cache worker fails in production.
	stop := func() {
		g.Go(func() error { return errors.New("service worker stopped") })
		_ = g.Wait()
	}
	t.Cleanup(func() {
		cancel()
		releaseOnce.Do(func() { close(release) })
		_ = srv.Close()
		client.CloseIdleConnections()
		for name, done := range map[string]<-chan error{"server": serveDone, "client": clientDone} {
			select {
			case err := <-done:
				if name == "server" && err != nil && !errors.Is(err, http.ErrServerClosed) {
					t.Errorf("server exit: %v", err)
				}
			case <-time.After(6 * time.Second):
				t.Errorf("%s did not exit", name)
			}
		}
		select {
		case <-handlerDone:
		case <-time.After(time.Second):
			t.Error("handler did not exit")
		}
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not enter fallback")
	}
	check(srv, stop, func() { releaseOnce.Do(func() { close(release) }) }, clientResult)
}

func TestActiveFallbackCanceledByServiceFailure(t *testing.T) {
	canceled := make(chan struct{})
	exerciseBlockedRequest(t, func(ctx context.Context, release <-chan struct{}) {
		select {
		case <-ctx.Done():
			close(canceled)
		case <-release:
		}
	}, func(srv *http.Server, stop context.CancelFunc, release func(), _ <-chan error) {
		stop()
		select {
		case <-canceled:
		case <-time.After(time.Second):
			t.Fatal("fallback request context did not inherit errgroup cancellation")
		}
		release()
		if err := shutdownHTTPServer(context.Background(), srv, time.Second); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	})
}

func TestShutdownForceClosesActiveRequestAfterGrace(t *testing.T) {
	exerciseBlockedRequest(t, func(context.Context, <-chan struct{}) {}, func(srv *http.Server, stop context.CancelFunc, _ func(), result <-chan error) {
		stop()
		// The handler deliberately ignores cancellation and stays blocked until
		// cleanup releases it. Shutdown alone cannot close this connection.
		if err := shutdownHTTPServer(context.Background(), srv, 20*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown = %v, want grace deadline", err)
		}
		select {
		case err := <-result:
			if err == nil {
				t.Fatal("blocked request unexpectedly completed successfully")
			}
		case <-time.After(time.Second):
			t.Fatal("active network request was not force-closed")
		}
	})
}

func TestRunInvalidDatabaseURLClosesListener(t *testing.T) {
	t.Setenv("DATABASE_URL", "://invalid")
	listener := testListener(t)
	if err := runWithListener(context.Background(), listener); err == nil {
		t.Fatal("invalid database URL accepted")
	}
	assertListenerClosed(t, listener)
}

func TestRunHTTPAndCancellation(t *testing.T) {
	pool, dsn := isolatedDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, groups.Schema); err != nil {
		t.Fatal(err)
	}
	store := groups.NewStore(pool, surecachepg.NewFence(fenceKey), surecachepg.DefaultBounds(), 100, 100)
	if err := store.CreateGroup(ctx, "red", "Red"); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMember(ctx, "red", "ann"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_URL", dsn)
	listener := testListener(t)
	done := make(chan error, 1)
	go func() { done <- runWithListener(ctx, listener) }()
	// Cleanup joins the service even if an assertion aborts the test.
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("service exit: %v", err)
			}
		case <-time.After(12 * time.Second):
			t.Error("service did not shut down")
		}
		assertListenerClosed(t, listener)
	})
	client := &http.Client{Timeout: 500 * time.Millisecond}
	defer client.CloseIdleConnections()
	baseURL := "http://" + listener.Addr().String()
	readyCtx, readyCancel := context.WithTimeout(ctx, 8*time.Second)
	defer readyCancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(readyCtx, "GET", baseURL+"/member/ann", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr != nil || response.StatusCode != http.StatusOK || string(body) != "{\"group\":\"red\"}\n" {
				t.Fatalf("member response = %d %q, %v", response.StatusCode, body, readErr)
			}
			break
		}
		select {
		case <-readyCtx.Done():
			t.Fatalf("service not ready: %v", err)
		case <-ticker.C:
		}
	}
	for _, tc := range []struct {
		path   string
		status int
		body   string
	}{
		{"/member/nobody", http.StatusNotFound, "404 page not found"},
		{"/metrics", http.StatusOK, `surecache_stale{cache="groups"} 0`},
	} {
		req, err := http.NewRequestWithContext(ctx, "GET", baseURL+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil || response.StatusCode != tc.status || !strings.Contains(string(body), tc.body) {
			t.Fatalf("%s response = %d %q, %v", tc.path, response.StatusCode, body, readErr)
		}
	}
	// The cleanup cancels, requires a clean exit and checks the listener is closed.
	cancel()
}

func TestRunFailsClosedWithoutSchema(t *testing.T) {
	_, dsn := isolatedDB(t)
	t.Setenv("DATABASE_URL", dsn)
	listener := testListener(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := runWithListener(ctx, listener)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42P01" || !strings.HasPrefix(err.Error(), "prime:") {
		t.Fatalf("startup without schema = %v, want prime undefined-table error", err)
	}
	assertListenerClosed(t, listener)
}

func TestRunServeFailureStopsCache(t *testing.T) {
	pool, dsn := isolatedDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, groups.Schema); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_URL", dsn)
	listener := testListener(t)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runWithListener(ctx, listener); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("serve on closed listener = %v", err)
	}
}

// isolatedDB follows the example's DSN contract, with bounded setup and teardown.
// Each test gets a database, not just a schema: advisory fences are database-wide.
func isolatedDB(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	dsn := os.Getenv("SURECACHE_PG_DSN")
	if dsn == "" {
		t.Skip("SURECACHE_PG_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if closeErr := admin.Close(closeCtx); closeErr != nil {
			t.Error(closeErr)
		}
	})
	name := "groups_command_" + strings.ToLower(rand.Text())
	quotedName := pgx.Identifier{name}.Sanitize()
	if _, createErr := admin.Exec(ctx, "CREATE DATABASE "+quotedName); createErr != nil {
		t.Fatal(createErr)
	}
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		if _, dropErr := admin.Exec(dropCtx, "DROP DATABASE IF EXISTS "+quotedName+" WITH (FORCE)"); dropErr != nil {
			t.Error(dropErr)
		}
	})
	// ConnString preserves the original DSN, so change the DSN itself rather
	// than only mutating ConnConfig.Database before passing it to the service.
	isolatedDSN := dsn + " dbname=" + name
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, parseErr := url.Parse(dsn)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		u.Path, u.RawPath = "/"+name, ""
		query := u.Query()
		query.Del("dbname")
		u.RawQuery = query.Encode()
		isolatedDSN = u.String()
	}
	cfg, err := pgxpool.ParseConfig(isolatedDSN)
	if err != nil {
		t.Fatal(err)
	}
	if configureErr := surecachepg.ConfigurePool(cfg, surecachepg.DefaultBounds()); configureErr != nil {
		t.Fatal(configureErr)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, isolatedDSN
}

func testListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func assertListenerClosed(t *testing.T, listener net.Listener) {
	t.Helper()
	// A deadline makes a missed close fail promptly, without a sleep or a leaked Accept.
	if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(time.Second)); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	conn, err := listener.Accept()
	if conn != nil {
		_ = conn.Close()
	}
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("listener not closed: %v", err)
	}
}
