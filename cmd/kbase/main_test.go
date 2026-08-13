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

// cliFixture stands up a live kbased (httptest server + real
// Postgres; skips when the database is unreachable), provisions an
// agent principal + project token the way labd does, points
// KBASE_URL/KBASE_TOKEN at it, and returns a runner for one CLI
// invocation. Everything the test writes is cleaned up.
func cliFixture(t *testing.T) func(stdin string, args ...string) (string, int) {
	t.Helper()
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
			"ALTER TABLE kbase.edges DISABLE TRIGGER edges_tombstone_only",
			"DELETE FROM kbase.edges WHERE created_by = $1",
			"ALTER TABLE kbase.edges ENABLE TRIGGER edges_tombstone_only",
			`DELETE FROM kbase.tickets
			 WHERE entry_id IN (SELECT id FROM kbase.entries WHERE created_by = $1)`,
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
	return func(stdin string, args ...string) (string, int) {
		t.Helper()
		var out, errOut bytes.Buffer
		code := run(args, strings.NewReader(stdin), &out, &errOut)
		if errOut.Len() > 0 {
			t.Logf("stderr: %s", errOut.String())
		}
		return out.String(), code
	}
}

// TestRoundTrip drives the CLI end to end: add → show → update → show
// --history → recall → list.
func TestRoundTrip(t *testing.T) {
	kb := cliFixture(t)

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

// TestGraphAndTicketRoundTrip drives the Phase 10 surface end to end:
// component add ×2 → link → graph in all three formats → unlink →
// graph reflects it; then a full ticket lifecycle with a failed second
// claim (one CAS attempt, exit 1, current state printed).
func TestGraphAndTicketRoundTrip(t *testing.T) {
	kb := cliFixture(t)

	// component add is a one-liner: without -m the title doubles as
	// the body, and no stdin read happens.
	out, code := kb("", "component", "add", "--title", "API Server")
	if code != 0 || !strings.Contains(out, "created api-server v1 (component") {
		t.Fatalf("component add: code %d, out %q", code, out)
	}
	if out, code = kb("", "component", "add", "--title", "Postgres DB"); code != 0 {
		t.Fatalf("component add 2: code %d, out %q", code, out)
	}

	out, code = kb("", "component", "link", "api-server", "postgres-db", "--label", "stores state in")
	if code != 0 || !strings.Contains(out, "linked api-server -> postgres-db [stores state in]") {
		t.Fatalf("link: code %d, out %q", code, out)
	}

	out, code = kb("", "graph")
	if code != 0 || !strings.Contains(out, "api-server (API Server)") ||
		!strings.Contains(out, "  -> postgres-db [stores state in]") {
		t.Fatalf("graph text: code %d, out %q", code, out)
	}

	out, code = kb("", "graph", "--format", "dot")
	if code != 0 || !strings.HasPrefix(out, "digraph kbase {\n") ||
		!strings.Contains(out, `"api-server" -> "postgres-db" [label="stores state in"];`) ||
		!strings.HasSuffix(strings.TrimSpace(out), "}") {
		t.Fatalf("graph dot: code %d, out %q", code, out)
	}

	out, code = kb("", "graph", "--format", "mermaid")
	if code != 0 || !strings.HasPrefix(out, "graph LR\n") ||
		!strings.Contains(out, "n_api_server[\"api-server\"]") ||
		!strings.Contains(out, "n_api_server -->|stores state in| n_postgres_db") {
		t.Fatalf("graph mermaid: code %d, out %q", code, out)
	}

	out, code = kb("", "component", "unlink", "api-server", "postgres-db")
	if code != 0 || !strings.Contains(out, "unlinked api-server -> postgres-db") {
		t.Fatalf("unlink: code %d, out %q", code, out)
	}
	if out, code = kb("", "graph"); code != 0 || strings.Contains(out, "->") {
		t.Fatalf("graph after unlink: code %d, out %q", code, out)
	}

	// Tickets: add ticket creates entry + claimable row in one call.
	out, code = kb("Investigate the flaky suite.", "add", "ticket", "--title", "Fix the build")
	if code != 0 || !strings.Contains(out, "created ticket fix-the-build (open, cas 0)") {
		t.Fatalf("add ticket: code %d, out %q", code, out)
	}
	if out, code = kb("", "ticket", "list", "--status", "open"); code != 0 ||
		!strings.Contains(out, "fix-the-build") {
		t.Fatalf("ticket list: code %d, out %q", code, out)
	}

	out, code = kb("", "ticket", "claim", "fix-the-build")
	if code != 0 || !strings.Contains(out, "claim: fix-the-build is now claimed (cas 1)") {
		t.Fatalf("claim: code %d, out %q", code, out)
	}
	// Second claim: exactly one CAS attempt, exit 1, current state
	// printed for the losing script.
	out, code = kb("", "ticket", "claim", "fix-the-build")
	if code != 1 || !strings.Contains(out, "claim failed — current state:") ||
		!strings.Contains(out, "status: claimed") {
		t.Fatalf("second claim: code %d, out %q", code, out)
	}

	if out, code = kb("", "ticket", "start", "fix-the-build"); code != 0 ||
		!strings.Contains(out, "is now in_progress") {
		t.Fatalf("start: code %d, out %q", code, out)
	}
	if out, code = kb("", "ticket", "comment", "fix-the-build", "-m", "Halfway there."); code != 0 {
		t.Fatalf("comment: code %d, out %q", code, out)
	}
	if out, code = kb("", "ticket", "done", "fix-the-build"); code != 0 ||
		!strings.Contains(out, "is now done") {
		t.Fatalf("done: code %d, out %q", code, out)
	}

	// The narrative lives in the entry chain: kbase show --history
	// shows every step with the acting principal.
	out, code = kb("", "show", "fix-the-build", "--history")
	if code != 0 ||
		!strings.Contains(out, "Investigate the flaky suite.") ||
		!strings.Contains(out, "_claimed by agent:cli-test-agent — ") ||
		!strings.Contains(out, "_started by agent:cli-test-agent — ") ||
		!strings.Contains(out, "_comment by agent:cli-test-agent — ") ||
		!strings.Contains(out, "Halfway there.") ||
		!strings.Contains(out, "_completed by agent:cli-test-agent — ") ||
		!strings.Contains(out, "v5") {
		t.Fatalf("show --history: code %d, out %q", code, out)
	}

	out, code = kb("", "ticket", "show", "fix-the-build")
	if code != 0 || !strings.Contains(out, "status: done") {
		t.Fatalf("ticket show: code %d, out %q", code, out)
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
