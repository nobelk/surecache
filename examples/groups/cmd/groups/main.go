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
	if result, err := repo.Cache().Prime(ctx); result != surecache.Installed {
		return fmt.Errorf("prime: %v: %w", result, err)
	}

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
	srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return repo.Cache().Run(gctx) })
	g.Go(func() error {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(gctx), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	})
	return g.Wait()
}
