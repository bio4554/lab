// Command migrate applies goose migrations for one of the two
// independent streams (lab or kbase) against the configured Postgres.
// It is a dev tool driven by make migrate-lab / make migrate-kbase.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bio4554/lab/internal/config"
	"github.com/bio4554/lab/internal/migrate"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run() error {
	streamName := flag.String("stream", "", "migration stream: lab or kbase")
	configPath := flag.String("config", config.DefaultPath, "path to lab.toml")
	flag.Parse()

	command := flag.Arg(0)
	if command == "" {
		command = "up"
	}

	stream, err := migrate.ByName(*streamName)
	if err != nil {
		return err
	}
	cfg, err := config.Load(*configPath, *configPath != config.DefaultPath)
	if err != nil {
		return err
	}

	db, err := sql.Open("pgx", cfg.PostgresDSN)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	switch command {
	case "up":
		applied, err := stream.Up(ctx, db)
		if err != nil {
			return err
		}
		if applied > 0 {
			fmt.Printf("stream %s: applied %d migration(s)\n", stream.Name, applied)
		} else {
			fmt.Printf("stream %s: up to date\n", stream.Name)
		}
	case "down":
		if err := stream.Down(ctx, db); err != nil {
			return err
		}
		fmt.Printf("stream %s: rolled back one migration\n", stream.Name)
	case "status":
		current, err := stream.Current(ctx, db)
		if err != nil {
			return err
		}
		fmt.Printf("stream %s: current=%v\n", stream.Name, current)
	default:
		return fmt.Errorf("unknown command %q (want up, down, or status)", command)
	}
	return nil
}
