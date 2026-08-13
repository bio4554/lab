// Command labd is the lab daemon. Phase 6: it hosts the agent drivers
// and serves the two HTTP APIs — the client API (localhost; projects,
// agents, turns, events + SSE) and the agent API (bearer-token auth,
// reachable from containers via host.docker.internal).
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bio4554/lab/internal/config"
	"github.com/bio4554/lab/internal/labd/api"
	"github.com/bio4554/lab/internal/labd/claude"
	"github.com/bio4554/lab/internal/labd/gitrepo"
	"github.com/bio4554/lab/internal/labd/runtime"
	"github.com/bio4554/lab/internal/labd/store"
	"github.com/bio4554/lab/internal/migrate"
)

// version is stamped via -ldflags at build time.
var version = "dev"

func main() {
	configPath := flag.String("config", config.DefaultPath, "path to lab.toml")
	flag.Parse()

	cfg, err := config.Load(*configPath, *configPath != config.DefaultPath)
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: cfg.SlogLevel(),
	})).With("daemon", "labd")
	slog.SetDefault(log)

	if err := run(cfg, log); err != nil {
		log.Error("labd exited", "error", err)
		os.Exit(1)
	}
}

// agentAPIURL derives the URL under which containers reach the agent
// API: host.docker.internal plus the configured listen port.
func agentAPIURL(addr string) (string, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("agent_api_addr %q: %w", addr, err)
	}
	return "http://host.docker.internal:" + port, nil
}

func run(cfg config.Config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.PostgresDSN)
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
	db, err := sql.Open("pgx", cfg.PostgresDSN)
	if err != nil {
		return fmt.Errorf("postgres (database/sql): %w", err)
	}
	defer db.Close()
	if current, err := migrate.Lab.Current(ctx, db); err != nil {
		log.Warn("schema currency check failed", "error", err)
	} else if !current {
		log.Warn("schema is not current; run migrations", "hint", "make migrate-lab")
	}

	st := store.New(pool)
	git := gitrepo.NewManager(cfg.DataDir)
	rt, err := runtime.New()
	if err != nil {
		return err
	}
	defer rt.Close()

	apiURL, err := agentAPIURL(cfg.Labd.AgentAPIAddr)
	if err != nil {
		return err
	}
	wake := claude.NewWakeHub()
	driver := claude.New(claude.Options{
		Store:       st,
		Git:         git,
		Runtime:     rt,
		Builder:     runtime.NewBuilder(rt, runtime.BuilderOptions{}),
		Logger:      log,
		AgentAPIURL: apiURL,
		TurnWake:    wake.Chan,
	})
	manager := claude.NewManager(driver, log)

	// The LISTEN fan-out: lab_events wakes SSE subscribers, lab_turns
	// wakes hosted drivers. Runs until shutdown; the drivers' 1s poll
	// and the SSE keepalive cover any listener downtime.
	hubCtx, stopHub := context.WithCancel(context.Background())
	defer stopHub()
	hub := api.NewHub(pool, wake.Wake, log)
	go hub.Run(hubCtx)

	// Simple crash recovery (full reconciliation is Phase 12): restart
	// drivers for agents that were running when the last daemon died.
	active, err := st.ActiveAgents(ctx)
	if err != nil {
		return err
	}
	for _, a := range active {
		log.Info("restarting driver for active agent", "agent", a.Name, "state", a.State)
		if err := manager.Start(a.ID); err != nil {
			log.Error("restarting driver", "agent", a.Name, "error", err)
		}
	}

	clientSrv := &api.ClientServer{
		Store: st, Pool: pool, Git: git,
		Driver: driver, Manager: manager, Hub: hub,
		Version: version, Log: log,
	}
	agentSrv := &api.AgentServer{Store: st, Manager: manager, Log: log}

	clientMux := http.NewServeMux()
	clientMux.Handle("/", clientSrv.Handler())
	clientMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		checkCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		current, err := migrate.Lab.Current(checkCtx, db)
		if err != nil {
			log.Warn("schema currency check failed", "error", err)
			current = false
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"version": version, "schema_current": current,
		})
	})

	// Request contexts hang off srvCtx so long-lived SSE handlers exit
	// when shutdown begins; Server.Shutdown alone would wait on them
	// forever.
	srvCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	baseCtx := func(net.Listener) context.Context { return srvCtx }
	servers := []*http.Server{
		{Addr: cfg.Labd.ClientAPIAddr, Handler: clientMux, BaseContext: baseCtx},
		{Addr: cfg.Labd.AgentAPIAddr, Handler: agentSrv.Handler(), BaseContext: baseCtx},
	}
	names := []string{"client API", "agent API"}

	errCh := make(chan error, len(servers))
	for i, srv := range servers {
		go func() {
			log.Info("http listening", "api", names[i], "addr", srv.Addr)
			if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s server: %w", names[i], err)
				return
			}
			errCh <- nil
		}()
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// Shutdown order: drivers first (each gets the pump's grace-drain),
	// then the HTTP servers, then the LISTEN connection.
	log.Info("shutting down: stopping agent drivers")
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	manager.StopAll(stopCtx)
	cancel()

	log.Info("shutting down: draining http servers")
	cancelRequests()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var shutdownErr error
	for i, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("%s shutdown: %w", names[i], err))
		}
		if err := <-errCh; err != nil {
			shutdownErr = errors.Join(shutdownErr, err)
		}
	}
	stopHub()
	if shutdownErr != nil {
		return shutdownErr
	}
	log.Info("shutdown complete")
	return nil
}
