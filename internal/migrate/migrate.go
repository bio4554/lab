// Package migrate wires goose for the two independent migration
// streams (lab and kbase), each versioned in its own goose table
// inside its own schema.
package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"

	"github.com/bio4554/lab/migrations"
)

// Stream is one independent migration stream: a set of embedded SQL
// migrations applied to one Postgres schema, versioned in that
// schema's own goose table.
type Stream struct {
	// Name identifies the stream ("lab" or "kbase").
	Name string
	// Schema is the Postgres schema the stream manages.
	Schema string
	// Table is the schema-qualified goose version table.
	Table string
	// FS contains the stream's *.sql files at its root.
	FS fs.FS
}

// Lab and Kbase are the two streams defined by this repo.
var (
	Lab   = Stream{Name: "lab", Schema: "lab", Table: "lab.goose_version", FS: mustSub(migrations.Lab, "lab")}
	Kbase = Stream{Name: "kbase", Schema: "kbase", Table: "kbase.goose_version", FS: mustSub(migrations.Kbase, "kbase")}
)

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// ByName returns the stream with the given name.
func ByName(name string) (Stream, error) {
	switch name {
	case Lab.Name:
		return Lab, nil
	case Kbase.Name:
		return Kbase, nil
	default:
		return Stream{}, fmt.Errorf("unknown migration stream %q (want %q or %q)", name, Lab.Name, Kbase.Name)
	}
}

func (s Stream) provider(db *sql.DB) (*goose.Provider, error) {
	store, err := database.NewStore(database.DialectPostgres, s.Table)
	if err != nil {
		return nil, fmt.Errorf("stream %s: %w", s.Name, err)
	}
	p, err := goose.NewProvider("", db, s.FS, goose.WithStore(store))
	if err != nil {
		return nil, fmt.Errorf("stream %s: %w", s.Name, err)
	}
	return p, nil
}

// Up applies all pending migrations and reports how many it applied
// (0 when the stream was already up to date). It creates the target
// schema first so the goose version table has somewhere to live; the
// initial migration's CREATE SCHEMA IF NOT EXISTS is then a no-op.
func (s Stream) Up(ctx context.Context, db *sql.DB) (int, error) {
	if _, err := db.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+s.Schema); err != nil {
		return 0, fmt.Errorf("stream %s: create schema: %w", s.Name, err)
	}
	p, err := s.provider(db)
	if err != nil {
		return 0, err
	}
	results, err := p.Up(ctx)
	if err != nil {
		return 0, fmt.Errorf("stream %s: up: %w", s.Name, err)
	}
	return len(results), nil
}

// Down rolls back the most recently applied migration.
func (s Stream) Down(ctx context.Context, db *sql.DB) error {
	p, err := s.provider(db)
	if err != nil {
		return err
	}
	if _, err := p.Down(ctx); err != nil {
		return fmt.Errorf("stream %s: down: %w", s.Name, err)
	}
	return nil
}

// Current reports whether the database has every migration in the
// stream applied. A missing schema or version table counts as not
// current rather than an error, so daemons can health-report against
// an empty database.
func (s Stream) Current(ctx context.Context, db *sql.DB) (bool, error) {
	var schemaExists bool
	err := db.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)",
		s.Schema).Scan(&schemaExists)
	if err != nil {
		return false, fmt.Errorf("stream %s: %w", s.Name, err)
	}
	if !schemaExists {
		return false, nil
	}
	p, err := s.provider(db)
	if err != nil {
		return false, err
	}
	pending, err := p.HasPending(ctx)
	if err != nil {
		return false, fmt.Errorf("stream %s: %w", s.Name, err)
	}
	return !pending, nil
}
