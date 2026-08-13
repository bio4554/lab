// Package config loads the shared lab.toml configuration for labd and
// kbased, with LAB_-prefixed environment variable overrides.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// DefaultPath is the config file both daemons look for when no explicit
// -config flag is given. A missing file at this path is not an error.
const DefaultPath = "lab.toml"

// Config is the root of lab.toml, shared by both daemons.
type Config struct {
	// PostgresDSN is the connection string for the shared Postgres
	// instance (both the lab and kbase schemas live in this database).
	PostgresDSN string `toml:"postgres_dsn"`
	// DataDir is the root under which labd keeps per-project state
	// (bare clones, worktrees, ...).
	DataDir string `toml:"data_dir"`
	// LogLevel is one of debug, info, warn, error.
	LogLevel string `toml:"log_level"`

	Labd   Labd   `toml:"labd"`
	Kbased Kbased `toml:"kbased"`
}

// Labd holds labd-specific settings.
type Labd struct {
	// ClientAPIAddr is the listen address for the client (TUI) API.
	ClientAPIAddr string `toml:"client_api_addr"`
	// AgentAPIAddr is the listen address for the agent-facing API,
	// reachable from containers via host.docker.internal.
	AgentAPIAddr string `toml:"agent_api_addr"`
	// ClaudeStubPath, when set, is a host binary baked into agent
	// images as `claude` in place of the real Claude Code CLI (the
	// install layer is skipped). Test knob for the credential-free e2e
	// suite; never set it in real deployments. Image tags are
	// content-addressed over the stub, so stub images never alias real
	// ones.
	ClaudeStubPath string `toml:"claude_stub_path"`
}

// Kbased holds kbased-specific settings (some consumed by labd, which
// is a kbased client).
type Kbased struct {
	ListenAddr string `toml:"listen_addr"`
	// URL is where labd reaches kbased from the host. Defaults to
	// http://<listen_addr>.
	URL string `toml:"url"`
	// KbaseURLForAgents is the kbased base URL as reachable from
	// inside agent containers; labd injects it as KBASE_URL. Defaults
	// to http://host.docker.internal:<listen port>.
	KbaseURLForAgents string `toml:"kbase_url_for_agents"`
	// AdminToken guards kbased's admin API (principal registration,
	// token mint/revoke). labd authenticates with it when provisioning
	// agent kbase tokens. Empty disables the admin API — and with it
	// labd's kbase wiring. Set it to any long random string; it lives
	// only in lab.toml (gitignored) or the LAB_KBASED_ADMIN_TOKEN env
	// var, never in the database.
	AdminToken string `toml:"admin_token"`
}

// Default returns the built-in configuration, chosen so labd runs with
// zero config against the docker-compose dev Postgres.
func Default() Config {
	dataDir := ".lab"
	if home, err := os.UserHomeDir(); err == nil {
		dataDir = filepath.Join(home, ".lab")
	}
	return Config{
		PostgresDSN: "postgres://lab:lab@localhost:5432/lab?sslmode=disable",
		DataDir:     dataDir,
		LogLevel:    "info",
		Labd: Labd{
			ClientAPIAddr: "127.0.0.1:7710",
			AgentAPIAddr:  "127.0.0.1:7711",
		},
		Kbased: Kbased{
			ListenAddr:        "127.0.0.1:7720",
			URL:               "http://127.0.0.1:7720",
			KbaseURLForAgents: "http://host.docker.internal:7720",
		},
	}
}

// Load returns the effective config: defaults, overlaid with the TOML
// file at path (missing file is an error only when explicit is true),
// overlaid with LAB_* environment variables.
func Load(path string, explicit bool) (Config, error) {
	cfg := Default()
	meta, err := toml.DecodeFile(path, &cfg)
	switch {
	case err == nil:
		if undecoded := meta.Undecoded(); len(undecoded) > 0 {
			return cfg, fmt.Errorf("config %s: unknown keys: %v", path, undecoded)
		}
	case os.IsNotExist(err) && !explicit:
		// No config file; defaults + env only.
	default:
		return cfg, fmt.Errorf("config %s: %w", path, err)
	}
	cfg.applyEnv()
	return cfg, nil
}

func (c *Config) applyEnv() {
	setenv := func(dst *string, key string) {
		if v, ok := os.LookupEnv(key); ok {
			*dst = v
		}
	}
	setenv(&c.PostgresDSN, "LAB_POSTGRES_DSN")
	setenv(&c.DataDir, "LAB_DATA_DIR")
	setenv(&c.LogLevel, "LAB_LOG_LEVEL")
	setenv(&c.Labd.ClientAPIAddr, "LAB_LABD_CLIENT_API_ADDR")
	setenv(&c.Labd.AgentAPIAddr, "LAB_LABD_AGENT_API_ADDR")
	setenv(&c.Labd.ClaudeStubPath, "LAB_LABD_CLAUDE_STUB_PATH")
	setenv(&c.Kbased.ListenAddr, "LAB_KBASED_LISTEN_ADDR")
	setenv(&c.Kbased.URL, "LAB_KBASED_URL")
	setenv(&c.Kbased.KbaseURLForAgents, "LAB_KBASED_KBASE_URL_FOR_AGENTS")
	setenv(&c.Kbased.AdminToken, "LAB_KBASED_ADMIN_TOKEN")
}

// SlogLevel parses LogLevel into a slog.Level, defaulting to info for
// unrecognized values.
func (c Config) SlogLevel() slog.Level {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(c.LogLevel)); err != nil {
		return slog.LevelInfo
	}
	return lvl
}
