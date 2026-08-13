// Command kbased is the kbase daemon: the bearer-token knowledge-base
// API (entries, versions, recall) plus the admin API labd uses to
// register principals and mint tokens, on the shared daemon skeleton
// (config, Postgres, /healthz, clean shutdown).
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bio4554/lab/internal/config"
	"github.com/bio4554/lab/internal/daemon"
	"github.com/bio4554/lab/internal/kbased"
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
	})).With("daemon", "kbased")
	slog.SetDefault(log)

	if cfg.Kbased.AdminToken == "" {
		log.Warn("no admin_token configured: the admin API is disabled and labd cannot mint agent kbase tokens",
			"hint", "set [kbased] admin_token in lab.toml or LAB_KBASED_ADMIN_TOKEN")
	}

	err = daemon.Run(context.Background(), daemon.Options{
		Name:    "kbased",
		Version: version,
		Addr:    cfg.Kbased.ListenAddr,
		DSN:     cfg.PostgresDSN,
		Stream:  migrate.Kbase,
		Logger:  log,
		Routes: func(pool *pgxpool.Pool, mux *http.ServeMux) {
			srv := &kbased.Server{
				Store:      kbased.NewStore(pool),
				AdminToken: cfg.Kbased.AdminToken,
				Log:        log,
			}
			h := srv.Handler()
			mux.Handle("/v1/", h)
			mux.Handle("/admin/v1/", h)
		},
	})
	if err != nil {
		log.Error("kbased exited", "error", err)
		os.Exit(1)
	}
}
