package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bio4554/lab/internal/labd/store"
	"github.com/bio4554/lab/internal/migrate"
)

func testDSN() string {
	if dsn := os.Getenv("LAB_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://lab:lab@localhost:5432/lab?sslmode=disable"
}

var migrateOnce sync.Once

// testStore connects to the dev Postgres (skipping when unreachable)
// and returns the store plus the raw pool for cleanup queries.
func testStore(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, testDSN())
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

	migrateOnce.Do(func() {
		db, err := sql.Open("pgx", testDSN())
		if err != nil {
			t.Fatalf("open for migrate: %v", err)
		}
		defer db.Close()
		if err := migrate.Lab.Up(ctx, db); err != nil {
			t.Fatalf("migrate lab up: %v", err)
		}
	})

	return store.New(pool), pool
}

// cleanupProject registers deletion of a project row and everything
// its agents own.
func cleanupProject(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, q := range []string{
			"DELETE FROM lab.events WHERE agent_id IN (SELECT id FROM lab.agents WHERE project_id = $1)",
			"DELETE FROM lab.turns WHERE agent_id IN (SELECT id FROM lab.agents WHERE project_id = $1)",
			"DELETE FROM lab.sessions WHERE agent_id IN (SELECT id FROM lab.agents WHERE project_id = $1)",
			"DELETE FROM lab.usage_rollups WHERE agent_id IN (SELECT id FROM lab.agents WHERE project_id = $1)",
			"DELETE FROM lab.agent_tokens WHERE agent_id IN (SELECT id FROM lab.agents WHERE project_id = $1)",
			"DELETE FROM lab.agents WHERE project_id = $1",
			"DELETE FROM lab.projects WHERE id = $1",
		} {
			if _, err := pool.Exec(ctx, q, projectID); err != nil {
				t.Errorf("cleanup %q: %v", q, err)
			}
		}
	})
}

// testFixture is a project + agent created directly through the store
// (no git repo, no container) for endpoint tests that don't provision.
type testFixture struct {
	Project store.Project
	Agent   store.Agent
}

func createFixture(t *testing.T, st *store.Store, pool *pgxpool.Pool) testFixture {
	t.Helper()
	ctx := context.Background()
	proj, err := st.CreateProject(ctx, store.NewProject{
		Name:       "apitest-" + uuid.NewString(),
		OriginKind: store.OriginKindLocalPath,
		Origin:     "/tmp/" + t.Name(),
		Stack:      "base",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	cleanupProject(t, pool, proj.ID)
	agent, err := st.CreateAgent(ctx, store.NewAgent{
		ProjectID: proj.ID,
		Name:      "agent-" + uuid.NewString()[:8],
		Branch:    "agent/test",
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	return testFixture{Project: proj, Agent: agent}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// doJSON performs a request with an optional JSON body and decodes the
// JSON response into out (skipped when out is nil or the body is
// empty).
func doJSON(t *testing.T, client *http.Client, method, url string, body, out any, wantStatus int, header http.Header) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s %s = %d, want %d; body: %s", method, url, resp.StatusCode, wantStatus, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: decoding %q: %v", method, url, raw, err)
		}
	}
}
