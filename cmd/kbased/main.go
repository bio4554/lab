// Command kbased is the kbase daemon. Phase 0 skeleton: config,
// Postgres, /healthz, clean shutdown.
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
	})).With("daemon", "kbased")
	slog.SetDefault(log)

	err = daemon.Run(context.Background(), daemon.Options{
		Name:    "kbased",
		Version: version,
		Addr:    cfg.Kbased.ListenAddr,
		DSN:     cfg.PostgresDSN,
		Stream:  migrate.Kbase,
		Logger:  log,
	})
	if err != nil {
		log.Error("kbased exited", "error", err)
		os.Exit(1)
	}
}
