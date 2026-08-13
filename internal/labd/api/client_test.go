package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bio4554/lab/internal/labd/claude"
	"github.com/bio4554/lab/internal/labd/gitrepo"
	"github.com/bio4554/lab/internal/labd/store"
	"github.com/bio4554/lab/internal/wire"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// newClientServer wires a ClientServer against the live store, a
// temp-dir git manager, and a provisioning-free driver (no Docker).
func newClientServer(t *testing.T) (*httptest.Server, *store.Store, *pgxpool.Pool) {
	t.Helper()
	st, pool := testStore(t)
	driver := claude.New(claude.Options{Store: st, Logger: testLogger()})
	hub := NewHub(pool, nil, testLogger())
	hubCtx, stopHub := context.WithCancel(context.Background())
	t.Cleanup(stopHub)
	go hub.Run(hubCtx)
	s := &ClientServer{
		Store:   st,
		Pool:    pool,
		Git:     gitrepo.NewManager(t.TempDir()),
		Driver:  driver,
		Manager: claude.NewManager(driver, testLogger()),
		Hub:     hub,
		Version: "test",
		Log:     testLogger(),
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, st, pool
}

// scratchGitRepo creates a committed local repo for project-create
// tests, skipping when git is unavailable.
func scratchGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "--allow-empty", "-m", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func TestProjectAndAgentCRUD(t *testing.T) {
	srv, st, pool := newClientServer(t)
	client := srv.Client()
	name := "apitest-" + uuid.NewString()[:8]

	// Create: bad stack rejected, good request lands and clones.
	doJSON(t, client, "POST", srv.URL+"/v1/projects",
		wire.CreateProjectRequest{Name: name, Origin: scratchGitRepo(t), Stack: "no-such-stack"},
		nil, http.StatusBadRequest, nil)
	var proj wire.Project
	doJSON(t, client, "POST", srv.URL+"/v1/projects",
		wire.CreateProjectRequest{Name: name, Origin: scratchGitRepo(t), Stack: "base"},
		&proj, http.StatusCreated, nil)
	cleanupProject(t, pool, proj.ID)
	if proj.Name != name || proj.OriginKind != store.OriginKindLocalPath {
		t.Errorf("created project = %+v", proj)
	}

	var got wire.Project
	doJSON(t, client, "GET", srv.URL+"/v1/projects/"+name, nil, &got, http.StatusOK, nil)
	if got.ID != proj.ID {
		t.Errorf("get project = %+v", got)
	}
	var projects []wire.Project
	doJSON(t, client, "GET", srv.URL+"/v1/projects", nil, &projects, http.StatusOK, nil)
	found := false
	for _, p := range projects {
		found = found || p.ID == proj.ID
	}
	if !found {
		t.Error("created project missing from list")
	}
	doJSON(t, client, "GET", srv.URL+"/v1/projects/nonexistent-xyz", nil, nil, http.StatusNotFound, nil)

	// Agents.
	var agent wire.Agent
	doJSON(t, client, "POST", srv.URL+"/v1/projects/"+name+"/agents",
		wire.CreateAgentRequest{Name: "impl1", RolePrompt: "test role", CredentialKind: "oauth_token"},
		&agent, http.StatusCreated, nil)
	if agent.Name != "impl1" || agent.State != store.AgentStateStopped || agent.Branch != "agent/impl1" {
		t.Errorf("created agent = %+v", agent)
	}
	var agents []wire.Agent
	doJSON(t, client, "GET", srv.URL+"/v1/projects/"+name+"/agents", nil, &agents, http.StatusOK, nil)
	if len(agents) != 1 || agents[0].ID != agent.ID || agents[0].Running {
		t.Errorf("agent list = %+v", agents)
	}

	// Delete: refused while agents exist, allowed after.
	doJSON(t, client, "DELETE", srv.URL+"/v1/projects/"+name, nil, nil, http.StatusConflict, nil)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DELETE FROM lab.agents WHERE id = $1", agent.ID); err != nil {
		t.Fatal(err)
	}
	doJSON(t, client, "DELETE", srv.URL+"/v1/projects/"+name, nil, nil, http.StatusNoContent, nil)
	if _, err := st.GetProject(ctx, proj.ID); err != store.ErrNotFound {
		t.Errorf("project after delete = %v, want ErrNotFound", err)
	}

	// Daemon status.
	var status wire.DaemonStatus
	doJSON(t, client, "GET", srv.URL+"/v1/status", nil, &status, http.StatusOK, nil)
	if status.Version != "test" || !status.DBHealthy || len(status.Stacks) == 0 {
		t.Errorf("status = %+v", status)
	}
}

func TestTurnSubmitAndGet(t *testing.T) {
	srv, st, pool := newClientServer(t)
	client := srv.Client()
	f := createFixture(t, st, pool)
	base := srv.URL + "/v1/projects/" + f.Project.Name + "/agents/" + f.Agent.Name

	doJSON(t, client, "POST", base+"/turns", wire.SubmitTurnRequest{}, nil, http.StatusBadRequest, nil)

	var turn wire.Turn
	doJSON(t, client, "POST", base+"/turns",
		wire.SubmitTurnRequest{Content: "do the thing"}, &turn, http.StatusCreated, nil)
	if turn.AgentID != f.Agent.ID || turn.SourceKind != store.SourceKindUser || turn.Status != store.TurnStatusQueued {
		t.Errorf("submitted turn = %+v", turn)
	}

	row, err := st.GetTurn(context.Background(), turn.ID)
	if err != nil || row.Status != store.TurnStatusQueued || row.Content != "do the thing" {
		t.Errorf("stored turn = %+v, %v", row, err)
	}

	var got wire.Turn
	doJSON(t, client, "GET", srv.URL+"/v1/turns/"+turn.ID.String(), nil, &got, http.StatusOK, nil)
	if got.ID != turn.ID || got.Content != "do the thing" {
		t.Errorf("get turn = %+v", got)
	}
	doJSON(t, client, "GET", srv.URL+"/v1/turns/"+uuid.NewString(), nil, nil, http.StatusNotFound, nil)
}

func TestEventsBackfill(t *testing.T) {
	srv, st, pool := newClientServer(t)
	client := srv.Client()
	f := createFixture(t, st, pool)
	ctx := context.Background()

	sess, err := st.CreateSession(ctx, f.Agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for range 5 {
		ev, err := st.AppendEvent(ctx, sess.ID, f.Agent.ID, nil, "assistant", []byte(`{"type":"assistant"}`))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ev.ID)
	}

	// Per-session pagination by seq.
	var page []wire.Event
	doJSON(t, client, "GET", srv.URL+"/v1/sessions/"+sess.ID.String()+"/events?limit=2", nil, &page, http.StatusOK, nil)
	if len(page) != 2 || page[0].Seq != 1 || page[1].Seq != 2 {
		t.Fatalf("first page = %+v", page)
	}
	doJSON(t, client, "GET", srv.URL+"/v1/sessions/"+sess.ID.String()+"/events?after_seq=2&limit=10", nil, &page, http.StatusOK, nil)
	if len(page) != 3 || page[0].Seq != 3 || page[2].Seq != 5 {
		t.Fatalf("second page = %+v", page)
	}
	for _, e := range page {
		if e.SessionID != sess.ID || e.Kind != "assistant" {
			t.Errorf("event = %+v", e)
		}
	}

	// Global tail by id. Other tests may append events concurrently
	// (shared dev database), so assert on this session's events only.
	var global []wire.Event
	doJSON(t, client, "GET", srv.URL+"/v1/events?after_id="+itoa(ids[2]), nil, &global, http.StatusOK, nil)
	var ours []int64
	for _, e := range global {
		if e.SessionID == sess.ID {
			ours = append(ours, e.ID)
		}
	}
	if len(ours) != 2 || ours[0] != ids[3] || ours[1] != ids[4] {
		t.Fatalf("global tail (ours) = %v, want [%d %d]", ours, ids[3], ids[4])
	}
}

func TestSessionListAndProjectUsage(t *testing.T) {
	srv, st, pool := newClientServer(t)
	client := srv.Client()
	ctx := context.Background()
	f := createFixture(t, st, pool)

	// Two chained sessions: old (ended, 2 events) → current (1 event).
	old, err := st.CreateSession(ctx, f.Agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := st.AppendEvent(ctx, old.ID, f.Agent.ID, nil, "assistant", []byte(`{"type":"assistant"}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.EndSession(ctx, old.ID, "retired"); err != nil {
		t.Fatal(err)
	}
	cur, err := st.CreateSession(ctx, f.Agent.ID, &old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(ctx, cur.ID, f.Agent.ID, nil, "system", []byte(`{"type":"system"}`)); err != nil {
		t.Fatal(err)
	}

	base := srv.URL + "/v1/projects/" + f.Project.Name + "/agents/" + f.Agent.Name
	var sessions []wire.Session
	doJSON(t, client, "GET", base+"/sessions", nil, &sessions, http.StatusOK, nil)
	if len(sessions) != 2 {
		t.Fatalf("sessions = %+v, want 2", sessions)
	}
	if sessions[0].ID != cur.ID || sessions[0].EventCount != 1 || sessions[0].EndedAt != nil {
		t.Errorf("current session = %+v", sessions[0])
	}
	if sessions[0].PrevSessionID == nil || *sessions[0].PrevSessionID != old.ID {
		t.Errorf("current session prev = %v, want %s", sessions[0].PrevSessionID, old.ID)
	}
	if sessions[1].ID != old.ID || sessions[1].EventCount != 2 ||
		sessions[1].EndReason == nil || *sessions[1].EndReason != "retired" {
		t.Errorf("old session = %+v", sessions[1])
	}
	doJSON(t, client, "GET", srv.URL+"/v1/projects/"+f.Project.Name+"/agents/nope/sessions",
		nil, nil, http.StatusNotFound, nil)

	// Usage: one recent window (hour+today+total) and one >26h old
	// (total only, regardless of local midnight).
	cred, err := st.CreateCredential(ctx, store.NewCredential{Kind: store.CredentialKindAPIKey, SecretEnc: []byte{}, Label: "usage test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DELETE FROM lab.usage_rollups WHERE credential_id = $1", cred.ID)
		pool.Exec(context.Background(), "DELETE FROM lab.credentials WHERE id = $1", cred.ID)
	})
	now := time.Now()
	if err := st.AddUsage(ctx, cred.ID, f.Agent.ID, now.Add(-10*time.Minute),
		store.UsageDelta{TokensIn: 100, TokensOut: 10, CostUSD: 0.5, Turns: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddUsage(ctx, cred.ID, f.Agent.ID, now.Add(-26*time.Hour),
		store.UsageDelta{TokensIn: 1000, TokensOut: 200, CostUSD: 2, Turns: 3}); err != nil {
		t.Fatal(err)
	}

	var usage []wire.AgentUsage
	doJSON(t, client, "GET", srv.URL+"/v1/projects/"+f.Project.Name+"/usage", nil, &usage, http.StatusOK, nil)
	if len(usage) != 1 {
		t.Fatalf("usage = %+v, want 1 row", usage)
	}
	u := usage[0]
	if u.AgentID != f.Agent.ID || u.AgentName != f.Agent.Name {
		t.Errorf("usage row identity = %+v", u)
	}
	if u.LastHour.TokensIn != 100 || u.LastHour.Turns != 1 {
		t.Errorf("last hour = %+v", u.LastHour)
	}
	if u.Today.TokensIn != 100 || u.Today.TokensOut != 10 {
		t.Errorf("today = %+v", u.Today)
	}
	if u.Total.TokensIn != 1100 || u.Total.TokensOut != 210 || u.Total.Turns != 4 {
		t.Errorf("total = %+v", u.Total)
	}
}
