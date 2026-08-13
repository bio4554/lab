package store

import (
	"context"
	"database/sql"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/bio4554/lab/internal/migrate"
)

// TestLabMigrationRoundTrip exercises the real lab migration files —
// rewritten into a throwaway schema so the dev database's lab schema
// (and any data in it) is untouched — checking Up creates the phase-1
// tables, Down drops them cleanly, and Up restores them.
func TestLabMigrationRoundTrip(t *testing.T) {
	db, err := sql.Open("pgx", testDSN())
	if err != nil {
		t.Fatal(err)
	}
	// Closed via t.Cleanup (not defer) so it outlives the schema-drop
	// cleanup registered below.
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		t.Skipf("postgres unreachable (run `make db-up`, or set LAB_TEST_DSN): %v", err)
	}

	const schema = "lab_store_roundtrip_test"
	mapfs := fstest.MapFS{}
	names, err := fs.Glob(migrate.Lab.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		src, err := fs.ReadFile(migrate.Lab.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		sql := strings.ReplaceAll(string(src), "lab.", schema+".")
		sql = strings.ReplaceAll(sql, "EXISTS lab;", "EXISTS "+schema+";")
		mapfs[name] = &fstest.MapFile{Data: []byte(sql)}
	}

	stream := migrate.Stream{
		Name:   "labroundtrip",
		Schema: schema,
		Table:  schema + ".goose_version",
		FS:     mapfs,
	}
	cleanup := func() {
		if _, err := db.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			t.Fatalf("drop test schema: %v", err)
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	tables := []string{"projects", "credentials", "agents", "sessions", "turns", "events", "usage_rollups", "agent_tokens"}
	tableExists := func(name string) bool {
		var exists bool
		err := db.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = $1 AND table_name = $2)",
			schema, name).Scan(&exists)
		if err != nil {
			t.Fatal(err)
		}
		return exists
	}

	if _, err := stream.Up(ctx, db); err != nil {
		t.Fatalf("Up: %v", err)
	}
	for _, tbl := range tables {
		if !tableExists(tbl) {
			t.Fatalf("after Up: table %s.%s missing", schema, tbl)
		}
	}

	// Down once rolls back 00005 (agent spawn/retire columns), then
	// 00004 (credential budget columns), then 00003 (agent_tokens),
	// then 00002 (the phase-1 tables).
	columnExists := func(table, column string) bool {
		var exists bool
		err := db.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = $1 AND table_name = $2 AND column_name = $3)",
			schema, table, column).Scan(&exists)
		if err != nil {
			t.Fatal(err)
		}
		return exists
	}
	if !columnExists("credentials", "limited_until") {
		t.Fatalf("after Up: credentials.limited_until missing")
	}
	if !columnExists("agents", "can_spawn") || !columnExists("agents", "retire_context_tokens") {
		t.Fatalf("after Up: agent spawn/retire columns missing")
	}
	if err := stream.Down(ctx, db); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if columnExists("agents", "can_spawn") || columnExists("agents", "retire_context_tokens") {
		t.Fatalf("after Down of 00005: agent spawn/retire columns still present")
	}
	if err := stream.Down(ctx, db); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if columnExists("credentials", "budget") || columnExists("credentials", "limited_until") {
		t.Fatalf("after Down of 00004: credential budget columns still present")
	}
	if err := stream.Down(ctx, db); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if tableExists("agent_tokens") {
		t.Fatalf("after Down: table %s.agent_tokens still present", schema)
	}
	if !tableExists("agents") {
		t.Fatalf("after Down of 00003: table %s.agents missing", schema)
	}
	if err := stream.Down(ctx, db); err != nil {
		t.Fatalf("Down (again): %v", err)
	}
	for _, tbl := range tables {
		if tableExists(tbl) {
			t.Fatalf("after Down: table %s.%s still present", schema, tbl)
		}
	}

	if _, err := stream.Up(ctx, db); err != nil {
		t.Fatalf("Up (again): %v", err)
	}
	for _, tbl := range tables {
		if !tableExists(tbl) {
			t.Fatalf("after re-Up: table %s.%s missing", schema, tbl)
		}
	}
}
