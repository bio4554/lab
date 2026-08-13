package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/bio4554/lab/internal/labd/store"
	"github.com/bio4554/lab/internal/wire"
)

func bearer(secret string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+secret)
	return h
}

func TestAgentAPI(t *testing.T) {
	st, pool := testStore(t)
	srv := httptest.NewServer((&AgentServer{Store: st, Log: testLogger()}).Handler())
	t.Cleanup(srv.Close)
	client := srv.Client()
	ctx := context.Background()

	// Caller + sibling in one project; a stranger in another.
	f := createFixture(t, st, pool)
	sibling, err := st.CreateAgent(ctx, store.NewAgent{
		ProjectID: f.Project.ID, Name: "sibling-" + uuid.NewString()[:8], Branch: "agent/sibling",
	})
	if err != nil {
		t.Fatal(err)
	}
	other := createFixture(t, st, pool) // separate project + agent

	_, secret, err := st.MintAgentToken(ctx, f.Agent.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Auth enforcement on every route.
	for _, route := range []struct{ method, path string }{
		{"GET", "/v1/whoami"},
		{"GET", "/v1/agents"},
		{"POST", "/v1/agents/" + sibling.Name + "/turns"},
		{"POST", "/v1/status"},
	} {
		doJSON(t, client, route.method, srv.URL+route.path, nil, nil, http.StatusUnauthorized, nil)
		doJSON(t, client, route.method, srv.URL+route.path, nil, nil, http.StatusUnauthorized, bearer("wrong-token"))
	}

	// whoami.
	var who wire.Whoami
	doJSON(t, client, "GET", srv.URL+"/v1/whoami", nil, &who, http.StatusOK, bearer(secret))
	if who.AgentID != f.Agent.ID || who.Name != f.Agent.Name ||
		who.Project != f.Project.Name || who.ProjectID != f.Project.ID {
		t.Errorf("whoami = %+v", who)
	}

	// Sibling listing: same project only.
	var agents []wire.Agent
	doJSON(t, client, "GET", srv.URL+"/v1/agents", nil, &agents, http.StatusOK, bearer(secret))
	if len(agents) != 2 {
		t.Fatalf("sibling list has %d agents, want 2: %+v", len(agents), agents)
	}
	for _, a := range agents {
		if a.ProjectID != f.Project.ID {
			t.Errorf("foreign agent in sibling list: %+v", a)
		}
	}

	// Cross-agent turn: lands with source_kind=agent, source_id=caller.
	var turn wire.Turn
	doJSON(t, client, "POST", srv.URL+"/v1/agents/"+sibling.Name+"/turns",
		wire.SubmitTurnRequest{Content: "please review my diff"}, &turn, http.StatusCreated, bearer(secret))
	if turn.AgentID != sibling.ID || turn.SourceKind != store.SourceKindAgent ||
		turn.SourceID == nil || *turn.SourceID != f.Agent.ID {
		t.Errorf("cross-agent turn = %+v", turn)
	}
	row, err := st.GetTurn(ctx, turn.ID)
	if err != nil || row.SourceKind != store.SourceKindAgent || *row.SourceID != f.Agent.ID {
		t.Errorf("stored turn = %+v, %v", row, err)
	}

	// An agent in another project is indistinguishable from a missing
	// one.
	doJSON(t, client, "POST", srv.URL+"/v1/agents/"+other.Agent.Name+"/turns",
		wire.SubmitTurnRequest{Content: "x"}, nil, http.StatusNotFound, bearer(secret))

	// Status self-report round-trip, visible in the listing; empty
	// clears.
	doJSON(t, client, "POST", srv.URL+"/v1/status",
		wire.ReportStatusRequest{Status: "refactoring the parser"}, nil, http.StatusNoContent, bearer(secret))
	doJSON(t, client, "GET", srv.URL+"/v1/agents", nil, &agents, http.StatusOK, bearer(secret))
	found := false
	for _, a := range agents {
		if a.ID == f.Agent.ID {
			found = a.StatusText != nil && *a.StatusText == "refactoring the parser"
		}
	}
	if !found {
		t.Errorf("status text not surfaced in listing: %+v", agents)
	}
	doJSON(t, client, "POST", srv.URL+"/v1/status",
		wire.ReportStatusRequest{}, nil, http.StatusNoContent, bearer(secret))
	if a, _ := st.GetAgent(ctx, f.Agent.ID); a.StatusText != nil {
		t.Errorf("status text not cleared: %v", *a.StatusText)
	}

	// Revocation kills the token.
	if _, err := st.RevokeAgentTokens(ctx, f.Agent.ID); err != nil {
		t.Fatal(err)
	}
	doJSON(t, client, "GET", srv.URL+"/v1/whoami", nil, nil, http.StatusUnauthorized, bearer(secret))
}
