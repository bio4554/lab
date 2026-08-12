package store

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bio4554/lab/internal/migrate"
)

func testDSN() string {
	if dsn := os.Getenv("LAB_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://lab:lab@localhost:5432/lab?sslmode=disable"
}

var migrateOnce sync.Once

// testStore connects to the dev Postgres and ensures the lab stream is
// migrated, skipping the test when the database is unreachable.
// Override the DSN with LAB_TEST_DSN.
func testStore(t *testing.T) *Store {
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

	return New(pool)
}

// fixture is one credential + project + agent, deleted (with all
// dependent rows the test created) at cleanup so repeated runs against
// the dev database stay clean.
type fixture struct {
	Credential Credential
	Project    Project
	Agent      Agent
}

func createFixture(t *testing.T, s *Store) fixture {
	t.Helper()
	ctx := context.Background()

	cred, err := s.CreateCredential(ctx, NewCredential{
		Kind:      CredentialKindAPIKey,
		SecretEnc: []byte("opaque-test-bytes"),
		Label:     "test " + t.Name(),
	})
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	proj, err := s.CreateProject(ctx, NewProject{
		Name:                "test-" + uuid.NewString(),
		OriginKind:          OriginKindLocalPath,
		Origin:              "/tmp/" + t.Name(),
		Stack:               "go",
		DefaultCredentialID: &cred.ID,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	agent, err := s.CreateAgent(ctx, NewAgent{
		ProjectID:    proj.ID,
		Name:         "agent-" + uuid.NewString(),
		RolePrompt:   "you are a test",
		CredentialID: &cred.ID,
		Branch:       "agent/test",
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	t.Cleanup(func() {
		for _, q := range []string{
			"DELETE FROM lab.events WHERE agent_id = $1",
			"DELETE FROM lab.turns WHERE agent_id = $1",
			"DELETE FROM lab.sessions WHERE agent_id = $1",
			"DELETE FROM lab.usage_rollups WHERE agent_id = $1",
			"DELETE FROM lab.agents WHERE id = $1",
		} {
			if _, err := s.pool.Exec(ctx, q, agent.ID); err != nil {
				t.Errorf("cleanup %q: %v", q, err)
			}
		}
		if _, err := s.pool.Exec(ctx, "DELETE FROM lab.projects WHERE id = $1", proj.ID); err != nil {
			t.Errorf("cleanup project: %v", err)
		}
		if _, err := s.pool.Exec(ctx, "DELETE FROM lab.credentials WHERE id = $1", cred.ID); err != nil {
			t.Errorf("cleanup credential: %v", err)
		}
	})

	return fixture{Credential: cred, Project: proj, Agent: agent}
}

func TestProjectCredentialAgentCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	f := createFixture(t, s)

	proj, err := s.GetProject(ctx, f.Project.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if proj.Name != f.Project.Name || proj.OriginKind != OriginKindLocalPath ||
		proj.DefaultCredentialID == nil || *proj.DefaultCredentialID != f.Credential.ID {
		t.Errorf("GetProject = %+v", proj)
	}
	if byName, err := s.GetProjectByName(ctx, f.Project.Name); err != nil || byName.ID != proj.ID {
		t.Errorf("GetProjectByName = %+v, %v", byName, err)
	}
	projects, err := s.ListProjects(ctx)
	if err != nil || len(projects) == 0 {
		t.Errorf("ListProjects = %d, %v", len(projects), err)
	}

	cred, err := s.GetCredential(ctx, f.Credential.ID)
	if err != nil {
		t.Fatalf("GetCredential: %v", err)
	}
	if cred.Status != CredentialStatusActive || string(cred.SecretEnc) != "opaque-test-bytes" {
		t.Errorf("GetCredential = %+v", cred)
	}
	if err := s.UpdateCredentialStatus(ctx, cred.ID, "revoked"); err != nil {
		t.Fatalf("UpdateCredentialStatus: %v", err)
	}
	if cred, _ = s.GetCredential(ctx, cred.ID); cred.Status != "revoked" {
		t.Errorf("status after update = %q, want revoked", cred.Status)
	}

	agent, err := s.GetAgent(ctx, f.Agent.ID)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if agent.State != AgentStateStopped || string(agent.Budget) != "{}" {
		t.Errorf("GetAgent = %+v", agent)
	}
	if err := s.UpdateAgentState(ctx, agent.ID, AgentStateWorking); err != nil {
		t.Fatalf("UpdateAgentState: %v", err)
	}
	cid := "deadbeef"
	if err := s.SetAgentContainer(ctx, agent.ID, &cid); err != nil {
		t.Fatalf("SetAgentContainer: %v", err)
	}
	agent, _ = s.GetAgent(ctx, agent.ID)
	if agent.State != AgentStateWorking || agent.ContainerID == nil || *agent.ContainerID != cid {
		t.Errorf("after updates = %+v", agent)
	}
	if err := s.SetAgentContainer(ctx, agent.ID, nil); err != nil {
		t.Fatalf("SetAgentContainer(nil): %v", err)
	}
	if agent, _ = s.GetAgent(ctx, agent.ID); agent.ContainerID != nil {
		t.Errorf("container id not cleared: %v", *agent.ContainerID)
	}
	agents, err := s.ListAgents(ctx, f.Project.ID)
	if err != nil || len(agents) != 1 {
		t.Errorf("ListAgents = %d, %v; want 1", len(agents), err)
	}

	if _, err := s.GetProject(ctx, uuid.New()); err != ErrNotFound {
		t.Errorf("GetProject(random) = %v, want ErrNotFound", err)
	}
	if err := s.DeleteAgent(ctx, uuid.New()); err != ErrNotFound {
		t.Errorf("DeleteAgent(random) = %v, want ErrNotFound", err)
	}
}

func TestDeleteRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	cred, err := s.CreateCredential(ctx, NewCredential{
		Kind: CredentialKindOAuthToken, SecretEnc: []byte("x"), Label: "del " + t.Name(),
	})
	if err != nil {
		t.Fatal(err)
	}
	proj, err := s.CreateProject(ctx, NewProject{
		Name: "test-" + uuid.NewString(), OriginKind: OriginKindGitURL,
		Origin: "https://example.com/x.git", Stack: "base",
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.CreateAgent(ctx, NewAgent{
		ProjectID: proj.ID, Name: "a", Branch: "agent/a",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteAgent(ctx, agent.ID); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}
	if err := s.DeleteProject(ctx, proj.ID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if err := s.DeleteCredential(ctx, cred.ID); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	if _, err := s.GetAgent(ctx, agent.ID); err != ErrNotFound {
		t.Errorf("GetAgent after delete = %v, want ErrNotFound", err)
	}
}

func TestSessions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	f := createFixture(t, s)

	if cur, err := s.CurrentSession(ctx, f.Agent.ID); err != nil || cur != nil {
		t.Fatalf("CurrentSession(no sessions) = %v, %v; want nil, nil", cur, err)
	}

	s1, err := s.CreateSession(ctx, f.Agent.ID, nil)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.SetClaudeSessionID(ctx, s1.ID, "claude-abc"); err != nil {
		t.Fatalf("SetClaudeSessionID: %v", err)
	}
	cur, err := s.CurrentSession(ctx, f.Agent.ID)
	if err != nil || cur == nil || cur.ID != s1.ID {
		t.Fatalf("CurrentSession = %+v, %v; want s1", cur, err)
	}
	if cur.ClaudeSessionID == nil || *cur.ClaudeSessionID != "claude-abc" {
		t.Errorf("claude_session_id = %v", cur.ClaudeSessionID)
	}

	if err := s.EndSession(ctx, s1.ID, "context exhausted"); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	if err := s.EndSession(ctx, s1.ID, "again"); err != ErrNotFound {
		t.Errorf("EndSession(ended) = %v, want ErrNotFound", err)
	}
	if cur, err := s.CurrentSession(ctx, f.Agent.ID); err != nil || cur != nil {
		t.Fatalf("CurrentSession after end = %v, %v; want nil, nil", cur, err)
	}

	s2, err := s.CreateSession(ctx, f.Agent.ID, &s1.ID)
	if err != nil {
		t.Fatalf("CreateSession(chained): %v", err)
	}
	if s2.PrevSessionID == nil || *s2.PrevSessionID != s1.ID {
		t.Errorf("prev_session_id = %v, want %v", s2.PrevSessionID, s1.ID)
	}
	got, err := s.GetSession(ctx, s1.ID)
	if err != nil || got.EndReason == nil || *got.EndReason != "context exhausted" || got.EndedAt == nil {
		t.Errorf("GetSession(s1) = %+v, %v", got, err)
	}
}

func TestRollups(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	f := createFixture(t, s)

	w1 := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	w2 := w1.Add(5 * time.Hour)

	for _, d := range []struct {
		w time.Time
		d UsageDelta
	}{
		{w1, UsageDelta{TokensIn: 100, TokensOut: 50, CostUSD: 0.25, Turns: 1}},
		{w1, UsageDelta{TokensIn: 30, TokensOut: 20, CostUSD: 0.05, Turns: 2}},
		{w2, UsageDelta{TokensIn: 1, TokensOut: 1, CostUSD: 0.01, Turns: 1}},
	} {
		if err := s.AddUsage(ctx, f.Credential.ID, f.Agent.ID, d.w, d.d); err != nil {
			t.Fatalf("AddUsage: %v", err)
		}
	}

	all, err := s.UsageInWindow(ctx, f.Credential.ID, w1)
	if err != nil {
		t.Fatalf("UsageInWindow: %v", err)
	}
	want := Usage{TokensIn: 131, TokensOut: 71, CostUSD: 0.31, Turns: 4}
	if all != want {
		t.Errorf("UsageInWindow(w1) = %+v, want %+v", all, want)
	}

	recent, err := s.UsageInWindow(ctx, f.Credential.ID, w2)
	if err != nil {
		t.Fatalf("UsageInWindow: %v", err)
	}
	if recent.TokensIn != 1 || recent.Turns != 1 {
		t.Errorf("UsageInWindow(w2) = %+v", recent)
	}

	empty, err := s.UsageInWindow(ctx, f.Credential.ID, w2.Add(time.Hour))
	if err != nil || empty != (Usage{}) {
		t.Errorf("UsageInWindow(future) = %+v, %v; want zero", empty, err)
	}
}
