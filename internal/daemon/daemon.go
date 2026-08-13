// Package daemon holds the shared skeleton both labd and kbased run:
// config-driven startup, Postgres connection, an HTTP server with
// /healthz, and clean shutdown on SIGINT/SIGTERM.
package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bio4554/lab/internal/migrate"
)

// Options configures one daemon instance.
type Options struct {
	// Name is the daemon name used in logs ("labd", "kbased").
	Name string
	// Version is reported by /healthz.
	Version string
	// Addr is the HTTP listen address.
	Addr string
	// DSN is the Postgres connection string.
	DSN string
	// Stream is the migration stream whose currency /healthz reports.
	Stream migrate.Stream
	// Logger receives structured logs.
	Logger *slog.Logger
	// Routes, when non-nil, mounts the daemon's API routes on mux with
	// access to the shared pool. /healthz is mounted either way.
	Routes func(pool *pgxpool.Pool, mux *http.ServeMux)
}

type healthResponse struct {
	Version       string `json:"version"`
	SchemaCurrent bool   `json:"schema_current"`
}

// Run starts the daemon and blocks until SIGINT/SIGTERM (or ctx
// cancellation), then drains the HTTP server and closes the pools.
func Run(ctx context.Context, opts Options) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log := opts.Logger

	pool, err := pgxpool.New(ctx, opts.DSN)
	if err != nil {
		return fmt.Errorf("postgres pool: %w", err)
	}
	defer pool.Close()

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = pool.Ping(pingCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("postgres ping: %w", err)
	}
	log.Info("connected to postgres")

	// Separate database/sql handle for goose-based schema checks.
	db, err := sql.Open("pgx", opts.DSN)
	if err != nil {
		return fmt.Errorf("postgres (database/sql): %w", err)
	}
	defer db.Close()

	// Startup self-check: warn on stale schema, never auto-migrate.
	current, err := opts.Stream.Current(ctx, db)
	switch {
	case err != nil:
		log.Warn("schema currency check failed", "stream", opts.Stream.Name, "error", err)
	case !current:
		log.Warn("schema is not current; run migrations",
			"stream", opts.Stream.Name, "hint", "make migrate-"+opts.Stream.Name)
	default:
		log.Info("schema is current", "stream", opts.Stream.Name)
	}

	mux := http.NewServeMux()
	if opts.Routes != nil {
		opts.Routes(pool, mux)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		checkCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		current, err := opts.Stream.Current(checkCtx, db)
		if err != nil {
			log.Warn("schema currency check failed", "stream", opts.Stream.Name, "error", err)
			current = false
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(healthResponse{
			Version:       opts.Version,
			SchemaCurrent: current,
		}); err != nil {
			log.Warn("healthz write failed", "error", err)
		}
	})

	// Bind with the bounded busy-address retry so a daemon restarted
	// while its predecessor drains wins the port instead of exiting.
	ln, err := Listen(ctx, opts.Addr, BindRetryWindow, log)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: opts.Addr, Handler: mux}
	errCh := make(chan error, 1)
	go func() {
		log.Info("http listening", "addr", opts.Addr)
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	if err := <-errCh; err != nil {
		return fmt.Errorf("http server: %w", err)
	}
	log.Info("shutdown complete")
	return nil
}
