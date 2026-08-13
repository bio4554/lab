package claude

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bio4554/lab/internal/labd/runtime"
	"github.com/bio4554/lab/internal/labd/store"
	"github.com/bio4554/lab/internal/migrate"
)

// fakeRT is a canned container listing plus a record of removals — the
// ContainerRuntime seam that lets the sweep's decision table run
// against a live store without Docker.
type fakeRT struct {
	containers     []runtime.Container
	removed        []string
	removedVolumes []string
}

func (f *fakeRT) List(ctx context.Context, projectID string) ([]runtime.Container, error) {
	return f.containers, nil
}

func (f *fakeRT) Remove(ctx context.Context, containerID string, force bool) error {
	f.removed = append(f.removed, containerID)
	return nil
}

func (f *fakeRT) RemoveVolume(ctx context.Context, agentID string, force bool) error {
	f.removedVolumes = append(f.removedVolumes, agentID)
	return nil
}

// sweepTestStore migrates and connects to a dedicated throwaway
// database: the sweep's queries are global (all agents, all running
// turns), so running it against the shared dev database would clobber
// other package-parallel suites' in-flight rows.
func sweepTestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	const dbName = "lab_sweep_test"

	admin, err := sql.Open("pgx", testDSN())
	if err != nil {
		t.Fatal(err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := admin.PingContext(pingCtx); err != nil {
		t.Skipf("postgres unreachable (run `make db-up`, or set LAB_TEST_DSN): %v", err)
	}
	if _, err := admin.Exec("DROP DATABASE IF EXISTS " + dbName); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("CREATE DATABASE " + dbName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)"); err != nil {
			t.Errorf("dropping %s: %v", dbName, err)
		}
		admin.Close()
	})

	u, err := url.Parse(testDSN())
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + dbName
	dsn := u.String()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := migrate.Lab.Up(ctx, db); err != nil {
		t.Fatalf("migrate lab up: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return store.New(pool)
}

// TestSweepDecisionTable drives every branch of the boot sweep against
// a live store and a fake container listing, then re-runs it to prove
// idempotence.
func TestSweepDecisionTable(t *testing.T) {
	st := sweepTestStore(t)
	ctx := context.Background()

	proj, err := st.CreateProject(ctx, store.NewProject{
		Name: "sweep-" + uuid.NewString()[:8], OriginKind: store.OriginKindLocalPath,
		Origin: "/tmp/sweep", Stack: "base",
	})
	if err != nil {
		t.Fatal(err)
	}
	mkAgent := func(name string) store.Agent {
		t.Helper()
		a, err := st.CreateAgent(ctx, store.NewAgent{
			ProjectID: proj.ID, Name: name, Branch: "agent/" + name,
		})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	setContainer := func(a store.Agent, id string) {
		t.Helper()
		if err := st.SetAgentContainer(ctx, a.ID, &id); err != nil {
			t.Fatal(err)
		}
	}

	// intact: recorded container matches the listing → untouched.
	intact := mkAgent("intact")
	setContainer(intact, "c-intact")
	// replaced: the listing has a different container for the agent →
	// recorded id corrected.
	replaced := mkAgent("replaced")
	setContainer(replaced, "c-old")
	// gone: recorded container absent from the listing → id cleared.
	gone := mkAgent("gone")
	setContainer(gone, "c-gone")
	// working: reset to idle. paused/stopped: respected as-is.
	working := mkAgent("working")
	if err := st.UpdateAgentState(ctx, working.ID, store.AgentStateWorking); err != nil {
		t.Fatal(err)
	}
	paused := mkAgent("paused")
	if err := st.UpdateAgentState(ctx, paused.ID, store.AgentStatePaused); err != nil {
		t.Fatal(err)
	}

	// One running turn (stuck: its pump died with the old daemon) and
	// one queued turn (must stay queued).
	if _, err := st.EnqueueTurn(ctx, store.NewTurn{
		AgentID: working.ID, SourceKind: store.SourceKindUser, Content: "stuck",
	}); err != nil {
		t.Fatal(err)
	}
	stuck, err := st.NextQueuedTurn(ctx, working.ID)
	if err != nil || stuck == nil {
		t.Fatalf("claiming stuck turn: %v, %v", stuck, err)
	}
	queued, err := st.EnqueueTurn(ctx, store.NewTurn{
		AgentID: working.ID, SourceKind: store.SourceKindUser, Content: "later",
	})
	if err != nil {
		t.Fatal(err)
	}

	// A container whose agent was deleted from the DB.
	orphanAgentID := uuid.NewString()

	const dep = "dep-under-test"
	rt := &fakeRT{containers: []runtime.Container{
		{ID: "c-intact", AgentID: intact.ID.String(), ProjectID: proj.ID.String(), Deployment: dep, State: "running"},
		{ID: "c-new", AgentID: replaced.ID.String(), ProjectID: proj.ID.String(), Deployment: dep, State: "exited"},
		{ID: "c-orphan", AgentID: orphanAgentID, ProjectID: proj.ID.String(), Deployment: dep, State: "exited"},
		// Foreign deployments — even with agent ids unknown to this
		// database — are never removed. Neither are unlabeled (pre-fix)
		// containers.
		{ID: "c-foreign", AgentID: uuid.NewString(), ProjectID: uuid.NewString(), Deployment: "someone-else", State: "running"},
		{ID: "c-unlabeled", AgentID: uuid.NewString(), ProjectID: uuid.NewString(), State: "exited"},
	}}
	sw := &Sweeper{St: st, Rt: rt, Deployment: dep, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := sw.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	wantContainer := func(a store.Agent, want *string) {
		t.Helper()
		got, err := st.GetAgent(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case want == nil && got.ContainerID != nil:
			t.Errorf("%s: container_id = %q, want cleared", a.Name, *got.ContainerID)
		case want != nil && (got.ContainerID == nil || *got.ContainerID != *want):
			t.Errorf("%s: container_id = %v, want %q", a.Name, got.ContainerID, *want)
		}
	}
	strPtr := func(s string) *string { return &s }

	wantContainer(intact, strPtr("c-intact"))
	wantContainer(replaced, strPtr("c-new"))
	wantContainer(gone, nil)

	if len(rt.removed) != 1 || rt.removed[0] != "c-orphan" {
		t.Errorf("removed containers = %v, want [c-orphan] only — foreign/unlabeled must survive", rt.removed)
	}
	if len(rt.removedVolumes) != 1 || rt.removedVolumes[0] != orphanAgentID {
		t.Errorf("removed volumes = %v, want [%s]", rt.removedVolumes, orphanAgentID)
	}

	wantState := func(a store.Agent, want string) {
		t.Helper()
		got, err := st.GetAgent(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != want {
			t.Errorf("%s: state = %q, want %q", a.Name, got.State, want)
		}
	}
	wantState(working, store.AgentStateIdle)
	wantState(paused, store.AgentStatePaused)
	wantState(intact, store.AgentStateStopped)

	gotStuck, err := st.GetTurn(ctx, stuck.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotStuck.Status != store.TurnStatusError || gotStuck.Error == nil || *gotStuck.Error != "labd restarted mid-turn" {
		t.Errorf("stuck turn = %q / %v, want error / labd restarted mid-turn", gotStuck.Status, gotStuck.Error)
	}
	gotQueued, err := st.GetTurn(ctx, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotQueued.Status != store.TurnStatusQueued {
		t.Errorf("queued turn = %q, want still queued", gotQueued.Status)
	}

	// Idempotence: a second sweep — against the corrected listing —
	// finds nothing to repair.
	rt.containers = []runtime.Container{
		{ID: "c-intact", AgentID: intact.ID.String(), ProjectID: proj.ID.String(), Deployment: dep, State: "running"},
		{ID: "c-new", AgentID: replaced.ID.String(), ProjectID: proj.ID.String(), Deployment: dep, State: "exited"},
		{ID: "c-foreign", AgentID: uuid.NewString(), ProjectID: uuid.NewString(), Deployment: "someone-else", State: "running"},
	}
	rt.removed, rt.removedVolumes = nil, nil
	if err := sw.Sweep(ctx); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if len(rt.removed) != 0 || len(rt.removedVolumes) != 0 {
		t.Errorf("second sweep removed %v / %v, want nothing", rt.removed, rt.removedVolumes)
	}
	wantContainer(intact, strPtr("c-intact"))
	wantContainer(replaced, strPtr("c-new"))
	wantState(working, store.AgentStateIdle)
	wantState(paused, store.AgentStatePaused)
}
