package claude

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/bio4554/lab/internal/labd/store"
)

// fakeKBase implements KBaseTokenSource.
type fakeKBase struct {
	url, token string
	err        error
}

func (f fakeKBase) ProvisionAgentToken(ctx context.Context, agentID uuid.UUID, displayName string, projectID uuid.UUID) (string, string, error) {
	return f.url, f.token, f.err
}

func TestAddKBaseEnv(t *testing.T) {
	ctx := context.Background()
	project := store.Project{ID: uuid.New()}
	agent := store.Agent{ID: uuid.New(), Name: "impl-1"}

	// No provisioner configured: env untouched.
	d := New(Options{})
	if env := d.addKBaseEnv(ctx, project, agent, nil); env != nil {
		t.Fatalf("no provisioner: env = %v, want nil", env)
	}

	// Provisioned: KBASE_URL + KBASE_TOKEN land next to existing vars.
	d = New(Options{KBase: fakeKBase{url: "http://host.docker.internal:7720", token: "s3cret"}})
	env := d.addKBaseEnv(ctx, project, agent, map[string]string{"LAB_PROJECT": "p"})
	if env["KBASE_URL"] != "http://host.docker.internal:7720" || env["KBASE_TOKEN"] != "s3cret" {
		t.Fatalf("env = %v", env)
	}
	if env["LAB_PROJECT"] != "p" {
		t.Fatal("existing env vars were dropped")
	}

	// kbased down: degrade gracefully — no kbase vars, no error, the
	// container still starts.
	d = New(Options{KBase: fakeKBase{err: errors.New("connection refused")}})
	env = d.addKBaseEnv(ctx, project, agent, map[string]string{"LAB_PROJECT": "p"})
	if _, ok := env["KBASE_URL"]; ok {
		t.Fatal("failed provisioning still injected KBASE_URL")
	}
	if _, ok := env["KBASE_TOKEN"]; ok {
		t.Fatal("failed provisioning still injected KBASE_TOKEN")
	}
	if env["LAB_PROJECT"] != "p" {
		t.Fatal("existing env vars were dropped")
	}
}
