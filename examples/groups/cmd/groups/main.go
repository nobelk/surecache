// SPDX-License-Identifier: Apache-2.0

// Command groups runs the cached groups repository behind a small HTTP API, with
// Prometheus metrics and graceful shutdown.
//
//	DATABASE_URL=postgres://... go run ./cmd/groups
//	curl localhost:8080/member/ann
//	curl localhost:8080/metrics
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"

	"github.com/nobelk/surecache"
	"github.com/nobelk/surecache/examples/groups"
	"github.com/nobelk/surecache/promobs"
	"github.com/nobelk/surecache/surecachepg"
)

// fenceKey is this cache's advisory-lock key; caches sharing a database need distinct
// keys.
const fenceKey = 0x67726f757073

func main() {
	if err := run(); err != nil {
		slog.Error("groups: exiting", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runWithListener(ctx, nil)
}

// runWithListener owns listener, including on startup failure. A nil listener uses
// the production address, bound only after the cache has successfully primed.
func runWithListener(ctx context.Context, listener net.Listener) error {
	defer func() {
		if listener != nil {
			_ = listener.Close()
		}
	}()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Server-side and transport bounds, checked against the rebuild budget.
	bounds := surecachepg.DefaultBounds()
	if err := surecachepg.CheckBudgets(bounds, 4*time.Second); err != nil {
		return err
	}
	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	if err = surecachepg.ConfigurePool(cfg, bounds); err != nil {
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	reg := prometheus.NewRegistry()
	metrics, err := promobs.New(reg)
	if err != nil {
		return err
	}
	store := groups.NewStore(pool, surecachepg.NewFence(fenceKey), bounds, 10_000, 100_000)
	repo, err := groups.NewCachedRepo(store,
		surecache.WithOutagePolicy(surecache.ServeWhileExact()),
		surecache.WithObserver(metrics.Observer("groups")),
		surecache.WithLogger(logger),
	)
	if err != nil {
		return err
	}

	// Never serve an unprimed cache: anything but Installed fails startup.
	if result, primeErr := repo.Cache().Prime(ctx); result != surecache.Installed {
		return fmt.Errorf("prime: %v: %w", result, primeErr)
	}

	if listener == nil {
		listener, err = net.Listen("tcp", ":8080") // #nosec G102 -- This example deliberately serves HTTP on all interfaces.
		if err != nil {
			return err
		}
	}
	g, gctx := errgroup.WithContext(ctx)
	srv := newHTTPServer(gctx, newHandler(repo, reg))
	g.Go(func() error { return repo.Cache().Run(gctx) })
	g.Go(func() error {
		if err := srv.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		return shutdownHTTPServer(gctx, srv, 10*time.Second)
	})
	return g.Wait()
}

func newHTTPServer(ctx context.Context, handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
}

func shutdownHTTPServer(ctx context.Context, srv *http.Server, grace time.Duration) error {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// Shutdown leaves active connections open on timeout. Close them so
		// network requests cannot outlive teardown (and retain pool resources).
		return errors.Join(err, srv.Close())
	}
	return nil
}

func newHandler(repo groups.Repo, reg prometheus.Gatherer) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /member/{member}", func(w http.ResponseWriter, r *http.Request) {
		id, ok, err := repo.GroupForMember(r.Context(), r.PathValue("member"))
		switch {
		case err != nil:
			http.Error(w, "store unavailable", http.StatusServiceUnavailable)
		case !ok:
			http.NotFound(w, r)
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"group": id})
		}
	})
	return mux
}
