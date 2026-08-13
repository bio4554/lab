// Command lab is the TUI client for labd (Phase 7): project/agent
// management, live transcripts over SSE, session history, and usage
// rollups. It reads the same lab.toml as the daemons (client API
// address), honors LAB_* env overrides, and -addr wins over both.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bio4554/lab/internal/config"
	"github.com/bio4554/lab/internal/labclient"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "lab:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", config.DefaultPath, "path to lab.toml")
	addr := flag.String("addr", "", "labd client API address (overrides config)")
	flag.Parse()

	cfg, err := config.Load(*configPath, *configPath != config.DefaultPath)
	if err != nil {
		return err
	}
	apiAddr := cfg.Labd.ClientAPIAddr
	if *addr != "" {
		apiAddr = *addr
	}

	// One context bounds the whole client: SIGINT/SIGTERM cancel it,
	// which makes bubbletea's Run return (restoring the terminal) and
	// ends the SSE stream goroutine; quitting from inside the TUI
	// cancels it via the deferred stop, with the same effect.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client := labclient.New(apiAddr)
	program := tea.NewProgram(newModel(ctx, client, apiAddr), tea.WithAltScreen(), tea.WithContext(ctx))
	_, err = program.Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil // signal-driven exit is a clean exit
	}
	return err
}
