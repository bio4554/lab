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
	"github.com/bio4554/lab/internal/daemon"
	"github.com/bio4554/lab/internal/kbclient"
	"github.com/bio4554/lab/internal/labd/api"
	"github.com/bio4554/lab/internal/labd/budget"
	"github.com/bio4554/lab/internal/labd/claude"
	"github.com/bio4554/lab/internal/labd/creds"
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

	// The credential vault: refuses to start on a key file readable
	// beyond its owner (the error says what to chmod).
	vault, err := creds.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	gate := &budget.Gate{St: st, Log: log}

	apiURL, err := agentAPIURL(cfg.Labd.AgentAPIAddr)
	if err != nil {
		return err
	}
	// kbase wiring: with an admin token configured, every container
	// create registers the agent as a kbase principal and mints its
	// project-scoped token. Without one, agents simply run without
	// kbase access (the driver also degrades gracefully when kbased is
	// down at create time).
	var kbase claude.KBaseTokenSource
	if cfg.Kbased.AdminToken != "" {
		kbase = &kbclient.TokenProvisioner{
			Admin:        kbclient.New(cfg.Kbased.URL, cfg.Kbased.AdminToken),
			AgentBaseURL: cfg.Kbased.KbaseURLForAgents,
		}
	} else {
		log.Warn("no [kbased] admin_token configured: agents start without kbase access",
			"hint", "set admin_token in lab.toml or LAB_KBASED_ADMIN_TOKEN")
	}

	builderOpts := runtime.BuilderOptions{Logger: log}
	if cfg.Labd.ClaudeStubPath != "" {
		// e2e test mode: agent images carry a scripted claude stand-in
		// instead of the real CLI. Loud on purpose — never intended for
		// real deployments.
		log.Warn("running with a claude stub; agents are NOT real Claude Code",
			"stub", cfg.Labd.ClaudeStubPath)
		builderOpts.ClaudeStubPath = cfg.Labd.ClaudeStubPath
	}

	wake := claude.NewWakeHub()
	driver := claude.New(claude.Options{
		Store:       st,
		Git:         git,
		Runtime:     rt,
		Builder:     runtime.NewBuilder(rt, builderOpts),
		Creds:       creds.NewSource(st, vault, log),
		Logger:      log,
		AgentAPIURL: apiURL,
		TurnGate:    gate,
		KBase:       kbase,
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

	// Crash-recovery reconciliation: before serving traffic, sweep
	// container reality against DB state — remove containers whose
	// agents are gone, fix stale container ids, error turns stuck
	// running (their pump died with the old daemon), and reset working
	// agents to idle. A failed sweep (e.g. Docker down) degrades to a
	// warning: the daemon can still serve the API and repair on its
	// next boot.
	sweeper := &claude.Sweeper{St: st, Rt: rt, Log: log}
	if err := sweeper.Sweep(ctx); err != nil {
		log.Warn("boot reconciliation sweep incomplete", "error", err)
	}

	// Then restart drivers for agents that were active when the last
	// daemon died; provisioning replaces their containers as usual.
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

	// Credential sweep: expire past-expiry credentials and release
	// passed rate-limit holds (resuming their paused agents). Lazy
	// checks on use catch both sooner; this keeps listings honest and
	// resumes agents with empty queues.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-hubCtx.Done():
				return
			case <-ticker.C:
				sweepCtx, cancel := context.WithTimeout(hubCtx, 10*time.Second)
				if n, err := st.ExpireCredentials(sweepCtx, time.Now()); err != nil {
					log.Warn("credential expiry sweep", "error", err)
				} else if n > 0 {
					log.Info("credentials expired", "count", n)
				}
				if n, err := st.ReleaseExpiredLimits(sweepCtx, time.Now()); err != nil {
					log.Warn("rate-limit release sweep", "error", err)
				} else if n > 0 {
					log.Info("rate-limit holds released; agents resumed", "credentials", n)
				}
				cancel()
			}
		}
	}()

	clientSrv := &api.ClientServer{
		Store: st, Pool: pool, Git: git,
		Driver: driver, Manager: manager, Hub: hub,
		Vault: vault, Gate: gate,
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

	// Bind both listeners up front, retrying while a draining
	// predecessor still holds the ports (a labd restarted mid-drain
	// used to exit immediately with "address already in use").
	listeners := make([]net.Listener, len(servers))
	for i, srv := range servers {
		ln, err := daemon.Listen(ctx, srv.Addr, daemon.BindRetryWindow, log.With("api", names[i]))
		if err != nil {
			for _, l := range listeners[:i] {
				l.Close()
			}
			return fmt.Errorf("%s: %w", names[i], err)
		}
		listeners[i] = ln
	}

	errCh := make(chan error, len(servers))
	for i, srv := range servers {
		go func() {
			log.Info("http listening", "api", names[i], "addr", srv.Addr)
			if err := srv.Serve(listeners[i]); !errors.Is(err, http.ErrServerClosed) {
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
