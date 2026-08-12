package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.toml"), false)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Default()
	if cfg != want {
		t.Errorf("got %+v, want defaults %+v", cfg, want)
	}
	if cfg.PostgresDSN != "postgres://lab:lab@localhost:5432/lab?sslmode=disable" {
		t.Errorf("default DSN = %q", cfg.PostgresDSN)
	}
}

func TestExplicitMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.toml"), true)
	if err == nil {
		t.Fatal("want error for explicitly named missing config file")
	}
}

func TestLoadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lab.toml")
	content := `
postgres_dsn = "postgres://elsewhere:5432/lab"
log_level = "debug"

[labd]
client_api_addr = "127.0.0.1:9000"

[kbased]
listen_addr = "127.0.0.1:9002"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, true)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PostgresDSN != "postgres://elsewhere:5432/lab" {
		t.Errorf("PostgresDSN = %q", cfg.PostgresDSN)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q", cfg.LogLevel)
	}
	if cfg.Labd.ClientAPIAddr != "127.0.0.1:9000" {
		t.Errorf("ClientAPIAddr = %q", cfg.Labd.ClientAPIAddr)
	}
	// Unset file keys keep their defaults.
	if want := Default().Labd.AgentAPIAddr; cfg.Labd.AgentAPIAddr != want {
		t.Errorf("AgentAPIAddr = %q, want default %q", cfg.Labd.AgentAPIAddr, want)
	}
	if cfg.Kbased.ListenAddr != "127.0.0.1:9002" {
		t.Errorf("Kbased.ListenAddr = %q", cfg.Kbased.ListenAddr)
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lab.toml")
	if err := os.WriteFile(path, []byte("postgres_dns = \"typo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, true); err == nil {
		t.Fatal("want error for unknown config key")
	}
}

func TestEnvOverridesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lab.toml")
	if err := os.WriteFile(path, []byte("postgres_dsn = \"postgres://file:5432/lab\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAB_POSTGRES_DSN", "postgres://env:5432/lab")
	t.Setenv("LAB_LOG_LEVEL", "warn")
	t.Setenv("LAB_LABD_CLIENT_API_ADDR", "127.0.0.1:9100")
	t.Setenv("LAB_LABD_AGENT_API_ADDR", "127.0.0.1:9101")
	t.Setenv("LAB_KBASED_LISTEN_ADDR", "127.0.0.1:9102")
	t.Setenv("LAB_DATA_DIR", "/tmp/lab-data")

	cfg, err := Load(path, true)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PostgresDSN != "postgres://env:5432/lab" {
		t.Errorf("PostgresDSN = %q, want env value", cfg.PostgresDSN)
	}
	if cfg.LogLevel != "warn" {
		t.Errorf("LogLevel = %q", cfg.LogLevel)
	}
	if cfg.Labd.ClientAPIAddr != "127.0.0.1:9100" {
		t.Errorf("ClientAPIAddr = %q", cfg.Labd.ClientAPIAddr)
	}
	if cfg.Labd.AgentAPIAddr != "127.0.0.1:9101" {
		t.Errorf("AgentAPIAddr = %q", cfg.Labd.AgentAPIAddr)
	}
	if cfg.Kbased.ListenAddr != "127.0.0.1:9102" {
		t.Errorf("Kbased.ListenAddr = %q", cfg.Kbased.ListenAddr)
	}
	if cfg.DataDir != "/tmp/lab-data" {
		t.Errorf("DataDir = %q", cfg.DataDir)
	}
}

func TestSlogLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
		"bogus": slog.LevelInfo,
		"":      slog.LevelInfo,
	} {
		cfg := Config{LogLevel: in}
		if got := cfg.SlogLevel(); got != want {
			t.Errorf("SlogLevel(%q) = %v, want %v", in, got, want)
		}
	}
}
