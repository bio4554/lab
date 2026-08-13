package main

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bio4554/lab/internal/kbased"
	"github.com/bio4554/lab/internal/kbclient"
	"github.com/bio4554/lab/internal/migrate"
)

// TestRoundTrip drives the CLI end to end against a live kbased
// (httptest server + real Postgres; skips when the database is
// unreachable): add → show → update → show --history → recall → list.
func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("LAB_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://lab:lab@localhost:5432/lab?sslmode=disable"
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		t.Skipf("postgres unreachable (run `make db-up`, or set LAB_TEST_DSN): %v", err)
	}
	t.Cleanup(pool.Close)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate.Kbase.Up(ctx, db); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	store := kbased.NewStore(pool)
	const adminToken = "cli-test-admin-token"
	srv := httptest.NewServer((&kbased.Server{Store: store, AdminToken: adminToken}).Handler())
	t.Cleanup(srv.Close)

	// Provision an agent principal + project token the way labd does.
	admin := kbclient.New(srv.URL, adminToken)
	principal, err := admin.RegisterPrincipal(ctx, kbclient.RegisterPrincipalRequest{
		Kind: "agent", ExternalID: t.Name() + "-" + uuid.NewString(), DisplayName: "cli-test-agent",
	})
	if err != nil {
		t.Fatalf("register principal: %v", err)
	}
	project := uuid.New()
	minted, err := admin.MintToken(ctx, kbclient.MintTokenRequest{
		PrincipalID: principal.ID, ProjectID: &project,
	})
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	t.Cleanup(func() {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Errorf("cleanup: %v", err)
			return
		}
		defer tx.Rollback(ctx)
		for _, q := range []string{
			"ALTER TABLE kbase.entry_versions DISABLE TRIGGER entry_versions_append_only",
			"DELETE FROM kbase.entry_versions WHERE author = $1",
			"ALTER TABLE kbase.entry_versions ENABLE TRIGGER entry_versions_append_only",
			"DELETE FROM kbase.entries WHERE created_by = $1",
			"DELETE FROM kbase.tokens WHERE principal_id = $1",
			"DELETE FROM kbase.principals WHERE id = $1",
		} {
			var err error
			if strings.HasPrefix(q, "ALTER") {
				_, err = tx.Exec(ctx, q)
			} else {
				_, err = tx.Exec(ctx, q, principal.ID)
			}
			if err != nil {
				t.Errorf("cleanup %q: %v", q, err)
				return
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Errorf("cleanup commit: %v", err)
		}
	})

	t.Setenv("KBASE_URL", srv.URL)
	t.Setenv("KBASE_TOKEN", minted.Token)

	// kbase reads content from stdin (the agent workflow: pipe or
	// heredoc) and everything else from flags.
	kb := func(stdin string, args ...string) (string, int) {
		t.Helper()
		var out, errOut bytes.Buffer
		code := run(args, strings.NewReader(stdin), &out, &errOut)
		if errOut.Len() > 0 {
			t.Logf("stderr: %s", errOut.String())
		}
		return out.String(), code
	}

	out, code := kb("Chosen for FTS and the shared instance.",
		"add", "decision", "--title", "Use Postgres")
	if code != 0 || !strings.Contains(out, "created use-postgres v1") {
		t.Fatalf("add: code %d, out %q", code, out)
	}

	out, code = kb("", "show", "use-postgres")
	if code != 0 || !strings.Contains(out, "# Use Postgres") ||
		!strings.Contains(out, "Chosen for FTS") ||
		!strings.Contains(out, "agent:cli-test-agent") {
		t.Fatalf("show: code %d, out %q", code, out)
	}

	out, code = kb("Amended: also chosen for LISTEN/NOTIFY.", "update", "use-postgres", "-")
	if code != 0 || !strings.Contains(out, "updated use-postgres to v2") {
		t.Fatalf("update: code %d, out %q", code, out)
	}

	out, code = kb("", "show", "use-postgres", "--history")
	if code != 0 || !strings.Contains(out, "History:") ||
		!strings.Contains(out, "v1") || !strings.Contains(out, "v2") {
		t.Fatalf("show --history: code %d, out %q", code, out)
	}

	out, code = kb("", "recall", "postgres", "-n", "3")
	if code != 0 || !strings.Contains(out, "use-postgres") {
		t.Fatalf("recall: code %d, out %q", code, out)
	}

	out, code = kb("", "list", "--type", "decision")
	if code != 0 || !strings.Contains(out, "use-postgres") {
		t.Fatalf("list: code %d, out %q", code, out)
	}

	// Script-friendly failure modes: bad usage exits 2, API errors 1.
	if _, code := kb("", "add", "decision"); code != 2 {
		t.Fatalf("add without --title: exit %d, want 2", code)
	}
	if _, code := kb("", "show", "no-such-slug"); code != 1 {
		t.Fatalf("show missing entry: exit %d, want 1", code)
	}
	if _, code := kb("body", "add", "decision", "--title", "Sneaky global", "--global"); code != 1 {
		t.Fatalf("project token adding --global: exit %d, want 1", code)
	}
}

// TestUsageErrors needs no server or database.
func TestUsageErrors(t *testing.T) {
	t.Setenv("KBASE_URL", "")
	t.Setenv("KBASE_TOKEN", "")
	var out bytes.Buffer
	if code := run(nil, strings.NewReader(""), io.Discard, &out); code != 2 {
		t.Errorf("no args: exit %d, want 2", code)
	}
	if code := run([]string{"bogus"}, strings.NewReader(""), io.Discard, &out); code != 2 {
		t.Errorf("unknown command: exit %d, want 2", code)
	}
	// No connection info at all is a usage error, reported before any
	// network traffic.
	code := run([]string{"list"}, strings.NewReader(""), io.Discard, &out)
	if code != 2 {
		t.Errorf("list without env: exit %d, want 2", code)
	}
	if code := run([]string{"help"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Errorf("help: exit %d, want 0", code)
	}
}
