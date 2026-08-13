package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/bio4554/lab/internal/labd/store"
	"github.com/bio4554/lab/internal/wire"
)

// TestAgentAPISpawn covers the worker-spawn policy: 401 without a
// token, 403 without can_spawn, and for an allowed caller a spawned
// agent that lands in the caller's project, inherits the caller's
// credential, appears in the sibling listing, and cannot itself spawn.
func TestAgentAPISpawn(t *testing.T) {
	st, pool := testStore(t)
	srv := httptest.NewServer((&AgentServer{Store: st, Log: testLogger()}).Handler())
	t.Cleanup(srv.Close)
	client := srv.Client()
	ctx := context.Background()

	// The credential outlives the project fixture's cleanup (LIFO), so
	// agents referencing it are deleted first.
	cred, err := st.CreateCredential(ctx, store.NewCredential{
		Kind: store.CredentialKindAPIKey, SecretEnc: []byte("x"), Label: "spawn " + t.Name(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM lab.credentials WHERE id = $1", cred.ID); err != nil {
			t.Errorf("cleanup credential: %v", err)
		}
	})

	f := createFixture(t, st, pool)
	if err := st.SetAgentCredential(ctx, f.Agent.ID, &cred.ID); err != nil {
		t.Fatal(err)
	}
	_, secret, err := st.MintAgentToken(ctx, f.Agent.ID)
	if err != nil {
		t.Fatal(err)
	}

	spawnReq := wire.SpawnAgentRequest{Name: "worker-" + uuid.NewString()[:8], RolePrompt: "you are a worker", Model: "claude-sonnet-5"}

	// Unauthenticated and non-spawner callers are refused.
	doJSON(t, client, "POST", srv.URL+"/v1/agents", spawnReq, nil, http.StatusUnauthorized, nil)
	doJSON(t, client, "POST", srv.URL+"/v1/agents", spawnReq, nil, http.StatusForbidden, bearer(secret))

	// Mark the caller an orchestrator; the spawn goes through.
	if _, err := pool.Exec(ctx, "UPDATE lab.agents SET can_spawn = true WHERE id = $1", f.Agent.ID); err != nil {
		t.Fatal(err)
	}
	var spawned wire.Agent
	doJSON(t, client, "POST", srv.URL+"/v1/agents", spawnReq, &spawned, http.StatusCreated, bearer(secret))
	if spawned.ProjectID != f.Project.ID {
		t.Errorf("spawned project = %s, want caller's %s", spawned.ProjectID, f.Project.ID)
	}
	if spawned.CredentialID == nil || *spawned.CredentialID != cred.ID {
		t.Errorf("spawned credential = %v, want inherited %s", spawned.CredentialID, cred.ID)
	}
	if spawned.CanSpawn {
		t.Error("spawned agent has can_spawn = true; transitive spawning must be off")
	}
	if spawned.Model == nil || *spawned.Model != "claude-sonnet-5" {
		t.Errorf("spawned model = %v", spawned.Model)
	}

	// Immediately visible in the sibling listing.
	var agents []wire.Agent
	doJSON(t, client, "GET", srv.URL+"/v1/agents", nil, &agents, http.StatusOK, bearer(secret))
	found := false
	for _, a := range agents {
		if a.ID == spawned.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("spawned agent missing from sibling listing: %+v", agents)
	}

	// No transitive spawn: the worker's own token gets a 403.
	_, workerSecret, err := st.MintAgentToken(ctx, spawned.ID)
	if err != nil {
		t.Fatal(err)
	}
	doJSON(t, client, "POST", srv.URL+"/v1/agents",
		wire.SpawnAgentRequest{Name: "grandchild"}, nil, http.StatusForbidden, bearer(workerSecret))

	// Missing name is a 400.
	doJSON(t, client, "POST", srv.URL+"/v1/agents",
		wire.SpawnAgentRequest{}, nil, http.StatusBadRequest, bearer(secret))

	// Spawning the same name again is a 409 (unique (project_id, name)),
	// not a 500.
	doJSON(t, client, "POST", srv.URL+"/v1/agents", spawnReq, nil, http.StatusConflict, bearer(secret))
}

// TestAgentCreateDuplicateName: the client API's agent create maps the
// (project_id, name) unique violation to a 409 with a clear message.
func TestAgentCreateDuplicateName(t *testing.T) {
	st, pool := testStore(t)
	srv := httptest.NewServer((&ClientServer{Store: st, Log: testLogger()}).Handler())
	t.Cleanup(srv.Close)

	f := createFixture(t, st, pool)
	req := wire.CreateAgentRequest{Name: f.Agent.Name}
	doJSON(t, srv.Client(), "POST", srv.URL+"/v1/projects/"+f.Project.Name+"/agents",
		req, nil, http.StatusConflict, nil)
}

// TestAgentAPIContextTokens: both listings surface the current
// session's occupancy — the latest result event's input + cache sum.
func TestAgentAPIContextTokens(t *testing.T) {
	st, pool := testStore(t)
	srv := httptest.NewServer((&AgentServer{Store: st, Log: testLogger()}).Handler())
	t.Cleanup(srv.Close)
	ctx := context.Background()

	f := createFixture(t, st, pool)
	_, secret, err := st.MintAgentToken(ctx, f.Agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateSession(ctx, f.Agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(ctx, sess.ID, f.Agent.ID, nil, "result",
		json.RawMessage(`{"type":"result","usage":{"input_tokens":140000,"cache_creation_input_tokens":5000,"cache_read_input_tokens":3000}}`)); err != nil {
		t.Fatal(err)
	}

	var agents []wire.Agent
	doJSON(t, srv.Client(), "GET", srv.URL+"/v1/agents", nil, &agents, http.StatusOK, bearer(secret))
	for _, a := range agents {
		if a.ID == f.Agent.ID && a.ContextTokens != 148000 {
			t.Errorf("agent API context_tokens = %d, want 148000", a.ContextTokens)
		}
	}

	// The client API listing reports the same gauge.
	csrv := httptest.NewServer((&ClientServer{Store: st, Log: testLogger()}).Handler())
	t.Cleanup(csrv.Close)
	var clientAgents []wire.Agent
	doJSON(t, csrv.Client(), "GET", csrv.URL+"/v1/projects/"+f.Project.Name+"/agents",
		nil, &clientAgents, http.StatusOK, nil)
	for _, a := range clientAgents {
		if a.ID == f.Agent.ID && a.ContextTokens != 148000 {
			t.Errorf("client API context_tokens = %d, want 148000", a.ContextTokens)
		}
	}
}
