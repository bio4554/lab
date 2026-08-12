package migrate

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"testing"
	"testing/fstest"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestByName(t *testing.T) {
	for _, want := range []Stream{Lab, Kbase} {
		got, err := ByName(want.Name)
		if err != nil {
			t.Fatalf("ByName(%q): %v", want.Name, err)
		}
		if got.Schema != want.Schema || got.Table != want.Table {
			t.Errorf("ByName(%q) = %+v", want.Name, got)
		}
	}
	if _, err := ByName("nope"); err == nil {
		t.Error("ByName(nope): want error")
	}
}

func TestEmbeddedStreamsHaveMigrations(t *testing.T) {
	for _, s := range []Stream{Lab, Kbase} {
		matches, err := fs.Glob(s.FS, "*.sql")
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) == 0 {
			t.Errorf("stream %s: no embedded *.sql migrations", s.Name)
		}
	}
}

// testDB connects to the dev Postgres, skipping the test when it is
// unreachable (e.g. make db-up has not run). Override with LAB_TEST_DSN.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("LAB_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://lab:lab@localhost:5432/lab?sslmode=disable"
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Skipf("postgres unreachable (run `make db-up`, or set LAB_TEST_DSN): %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestSchemaCurrency exercises the currency check against a throwaway
// stream in its own schema: not current on an empty database, current
// after Up, not current again after rolling one migration back.
func TestSchemaCurrency(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	const schema = "lab_migrate_test"
	stream := Stream{
		Name:   "labtest",
		Schema: schema,
		Table:  schema + ".goose_version",
		FS: fstest.MapFS{
			"00001_create_schema.sql": &fstest.MapFile{Data: []byte(
				"-- +goose Up\nCREATE SCHEMA IF NOT EXISTS " + schema + ";\n-- +goose Down\nSELECT 1;\n")},
			"00002_widgets.sql": &fstest.MapFile{Data: []byte(
				"-- +goose Up\nCREATE TABLE " + schema + ".widgets (id int);\n-- +goose Down\nDROP TABLE " + schema + ".widgets;\n")},
		},
	}
	cleanup := func() {
		if _, err := db.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			t.Fatalf("drop test schema: %v", err)
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	if current, err := stream.Current(ctx, db); err != nil || current {
		t.Fatalf("empty db: Current = %v, %v; want false, nil", current, err)
	}
	if err := stream.Up(ctx, db); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if current, err := stream.Current(ctx, db); err != nil || !current {
		t.Fatalf("after Up: Current = %v, %v; want true, nil", current, err)
	}
	if err := stream.Down(ctx, db); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if current, err := stream.Current(ctx, db); err != nil || current {
		t.Fatalf("after Down: Current = %v, %v; want false, nil", current, err)
	}
}

// TestStreamsAreIndependent applies both real streams and checks each
// keeps its own version table in its own schema.
func TestStreamsAreIndependent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	for _, s := range []Stream{Lab, Kbase} {
		if err := s.Up(ctx, db); err != nil {
			t.Fatalf("stream %s: Up: %v", s.Name, err)
		}
		var exists bool
		err := db.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = $1 AND table_name = 'goose_version')",
			s.Schema).Scan(&exists)
		if err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Errorf("stream %s: version table %s missing", s.Name, s.Table)
		}
		if current, err := s.Current(ctx, db); err != nil || !current {
			t.Errorf("stream %s: Current = %v, %v; want true, nil", s.Name, current, err)
		}
	}
}
