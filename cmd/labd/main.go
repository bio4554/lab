// Command labd is the lab daemon. Phase 0 skeleton: config, Postgres,
// /healthz on the client API address, clean shutdown.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	"github.com/bio4554/lab/internal/config"
	"github.com/bio4554/lab/internal/daemon"
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

	// Phase 0: labd serves only the client API address; the agent API
	// (cfg.Labd.AgentAPIAddr) arrives with Phase 6.
	err = daemon.Run(context.Background(), daemon.Options{
		Name:    "labd",
		Version: version,
		Addr:    cfg.Labd.ClientAPIAddr,
		DSN:     cfg.PostgresDSN,
		Stream:  migrate.Lab,
		Logger:  log,
	})
	if err != nil {
		log.Error("labd exited", "error", err)
		os.Exit(1)
	}
}
