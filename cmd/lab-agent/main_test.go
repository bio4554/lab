package main

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bio4554/lab/internal/labd/api"
	"github.com/bio4554/lab/internal/labd/store"
	"github.com/bio4554/lab/internal/migrate"
)

// cliFixture stands up a live agent API (httptest + real Postgres;
// skips when the database is unreachable), creates an orchestrator
// (can_spawn) with a sibling worker, mints the orchestrator's token,
// and returns the store handles plus a runner for one CLI invocation
// (-url/-token are appended, so the environment is untouched).
type cliFixture struct {
	st     *store.Store
	caller store.Agent
	worker store.Agent
	run    func(stdin string, args ...string) (stdout, stderr string, code int)
}

func newCLIFixture(t *testing.T) *cliFixture {
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
	if _, err := migrate.Lab.Up(ctx, db); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	st := store.New(pool)
	proj, err := st.CreateProject(ctx, store.NewProject{
		Name:       "clitest-" + uuid.NewString(),
		OriginKind: store.OriginKindLocalPath,
		Origin:     "/tmp/" + t.Name(),
		Stack:      "base",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		for _, q := range []string{
			"DELETE FROM lab.events WHERE agent_id IN (SELECT id FROM lab.agents WHERE project_id = $1)",
			"DELETE FROM lab.turns WHERE agent_id IN (SELECT id FROM lab.agents WHERE project_id = $1)",
			"DELETE FROM lab.sessions WHERE agent_id IN (SELECT id FROM lab.agents WHERE project_id = $1)",
			"DELETE FROM lab.agent_tokens WHERE agent_id IN (SELECT id FROM lab.agents WHERE project_id = $1)",
			"DELETE FROM lab.agents WHERE project_id = $1",
			"DELETE FROM lab.projects WHERE id = $1",
		} {
			if _, err := pool.Exec(ctx, q, proj.ID); err != nil {
				t.Errorf("cleanup %q: %v", q, err)
			}
		}
	})
	caller, err := st.CreateAgent(ctx, store.NewAgent{
		ProjectID: proj.ID, Name: "orch-" + uuid.NewString()[:8],
		Branch: "agent/orch", CanSpawn: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := st.CreateAgent(ctx, store.NewAgent{
		ProjectID: proj.ID, Name: "worker-" + uuid.NewString()[:8],
		Branch: "agent/worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := st.MintAgentToken(ctx, caller.ID)
	if err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer((&api.AgentServer{Store: st, Log: log}).Handler())
	t.Cleanup(srv.Close)

	return &cliFixture{
		st:     st,
		caller: caller,
		worker: worker,
		run: func(stdin string, args ...string) (string, string, int) {
			var out, errb bytes.Buffer
			code := run(append(args, "-url", srv.URL, "-token", secret),
				strings.NewReader(stdin), &out, &errb)
			return out.String(), errb.String(), code
		},
	}
}

// TestCLIRoundTrip drives the documented loop against a live agent
// API: whoami → status → agents → send → spawn.
func TestCLIRoundTrip(t *testing.T) {
	f := newCLIFixture(t)
	ctx := context.Background()

	stdout, stderr, code := f.run("", "whoami")
	if code != 0 || !strings.Contains(stdout, f.caller.Name) {
		t.Fatalf("whoami = %d, %q, %q", code, stdout, stderr)
	}

	if _, stderr, code := f.run("", "status", "splitting work into tickets"); code != 0 {
		t.Fatalf("status = %d, %q", code, stderr)
	}

	stdout, stderr, code = f.run("", "agents")
	if code != 0 {
		t.Fatalf("agents = %d, %q", code, stderr)
	}
	for _, want := range []string{f.caller.Name, f.worker.Name, "splitting work into tickets", "CONTEXT"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("agents output missing %q:\n%s", want, stdout)
		}
	}

	// send: the queued turn is attributed to the caller.
	stdout, stderr, code = f.run("", "send", f.worker.Name, "claim and complete ticket demo-1")
	if code != 0 || !strings.Contains(stdout, "queued turn") {
		t.Fatalf("send = %d, %q, %q", code, stdout, stderr)
	}
	turn, err := f.st.PeekQueuedTurn(ctx, f.worker.ID)
	if err != nil || turn == nil {
		t.Fatalf("queued turn = %v, %v", turn, err)
	}
	if turn.SourceKind != store.SourceKindAgent || turn.SourceID == nil || *turn.SourceID != f.caller.ID {
		t.Errorf("turn attribution = %s/%v, want agent/%s", turn.SourceKind, turn.SourceID, f.caller.ID)
	}
	if turn.Content != "claim and complete ticket demo-1" {
		t.Errorf("turn content = %q", turn.Content)
	}

	// send via stdin.
	stdout, _, code = f.run("do the second thing", "send", f.worker.Name)
	if code != 0 || !strings.Contains(stdout, "queued turn") {
		t.Fatalf("send via stdin = %d, %q", code, stdout)
	}

	// spawn: created in the caller's project, no transitive spawn.
	stdout, stderr, code = f.run("", "spawn", "-name", "spawned-"+uuid.NewString()[:8], "-role", "you are a worker")
	if code != 0 || !strings.Contains(stdout, "spawned agent") {
		t.Fatalf("spawn = %d, %q, %q", code, stdout, stderr)
	}

	// status "" clears.
	if _, stderr, code := f.run("", "status", ""); code != 0 {
		t.Fatalf("status clear = %d, %q", code, stderr)
	}
	if a, err := f.st.GetAgent(ctx, f.caller.ID); err != nil || a.StatusText != nil {
		t.Errorf("status not cleared: %v, %v", a.StatusText, err)
	}
}

// TestCLIUsageErrors: bad invocations exit 2 without touching the API.
func TestCLIUsageErrors(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(nil, strings.NewReader(""), &out, &errb); code != 2 {
		t.Errorf("no args = %d, want 2", code)
	}
	errb.Reset()
	if code := run([]string{"bogus"}, strings.NewReader(""), &out, &errb); code != 2 {
		t.Errorf("unknown command = %d, want 2", code)
	}
	errb.Reset()
	if code := run([]string{"spawn"}, strings.NewReader(""), &out, &errb); code != 2 {
		t.Errorf("spawn without --name = %d, want 2; stderr %q", code, errb.String())
	}
	errb.Reset()
	// No URL/token in env or flags: a usage error, not an API error.
	t.Setenv("LAB_API_URL", "")
	t.Setenv("LAB_AGENT_TOKEN", "")
	if code := run([]string{"whoami"}, strings.NewReader(""), &out, &errb); code != 2 {
		t.Errorf("whoami without connection = %d, want 2; stderr %q", code, errb.String())
	}
}
