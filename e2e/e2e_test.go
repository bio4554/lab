//go:build e2e

// Package e2e drives the real stack — labd + kbased + Postgres + real
// Docker containers — with a scripted stand-in for the claude binary
// (see ./claudestub), so the whole loop runs deterministically with no
// Anthropic credential. Run with `make e2e`; it skips (not fails) when
// Docker or Postgres is unreachable.
//
// The suite uses a dedicated database (lab_e2e, dropped and recreated
// each run) and a temp data dir, so it never touches dev state. The
// stub reaches agent images via labd's LAB_LABD_CLAUDE_STUB_PATH knob:
// the builder bakes it into the base image as /usr/local/bin/claude
// with the real-CLI install layer skipped, under a content hash that
// can never alias a real image.
package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bio4554/lab/internal/labclient"
	"github.com/bio4554/lab/internal/migrate"
	"github.com/bio4554/lab/internal/wire"
)

const (
	e2eDB      = "lab_e2e"
	adminToken = "e2e-admin-token-not-a-secret"
	project    = "e2e"
	agentName  = "e2e-worker"
)

func baseDSN() string {
	if dsn := os.Getenv("LAB_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://lab:lab@localhost:5432/lab?sslmode=disable"
}

// harness is the running stack: both daemons, their config, and the
// handles the subtests share.
type harness struct {
	t        *testing.T
	repoRoot string
	binDir   string
	dataDir  string
	dsn      string // lab_e2e database

	clientAddr string
	agentAddr  string
	kbasedAddr string

	docker *client.Client
	db     *sql.DB // lab_e2e, for direct assertions
	api    *labclient.Client

	labd   *exec.Cmd
	kbased *exec.Cmd

	canaries []canary // foreign/unlabeled containers the sweep must never touch
}

// freePort reserves an ephemeral localhost port and releases it for
// the daemon to bind.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	// Skip, not fail, when the environment is missing.
	docker, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("docker client: %v", err)
	}
	if _, err := docker.Ping(ctx); err != nil {
		t.Skipf("docker unreachable: %v", err)
	}
	admin, err := sql.Open("pgx", baseDSN())
	if err != nil {
		t.Fatal(err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := admin.PingContext(pingCtx); err != nil {
		t.Skipf("postgres unreachable (run `make db-up`, or set LAB_TEST_DSN): %v", err)
	}
	t.Cleanup(func() { admin.Close() })

	// Fresh database, both streams migrated.
	if _, err := admin.Exec("DROP DATABASE IF EXISTS " + e2eDB + " WITH (FORCE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("CREATE DATABASE " + e2eDB); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(baseDSN())
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + e2eDB
	dsn := u.String()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := migrate.Lab.Up(ctx, db); err != nil {
		t.Fatalf("migrate lab: %v", err)
	}
	if _, err := migrate.Kbase.Up(ctx, db); err != nil {
		t.Fatalf("migrate kbase: %v", err)
	}

	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()

	h := &harness{
		t:          t,
		repoRoot:   repoRoot,
		binDir:     binDir,
		dataDir:    t.TempDir(),
		dsn:        dsn,
		clientAddr: freePort(t),
		agentAddr:  freePort(t),
		kbasedAddr: freePort(t),
		docker:     docker,
		db:         db,
	}
	h.api = labclient.New(h.clientAddr)

	// Host binaries, plus the linux claude stub for the agent image.
	h.goBuild(filepath.Join(binDir, "labd"), "./cmd/labd", nil)
	h.goBuild(filepath.Join(binDir, "kbased"), "./cmd/kbased", nil)
	arch := h.daemonArch(ctx)
	h.goBuild(filepath.Join(binDir, "claudestub"), "./e2e/claudestub",
		[]string{"GOOS=linux", "GOARCH=" + arch, "CGO_ENABLED=0"})

	// Plant the canaries BEFORE labd ever runs: containers + volumes
	// that look like other deployments' agents. Every boot sweep in the
	// suite (including the kill-9 restart's) must leave them alone.
	h.plantCanaries(ctx)

	h.kbased = h.startDaemon("kbased")
	h.waitHealthz(h.kbasedAddr)
	h.labd = h.startDaemon("labd")
	h.waitHealthz(h.clientAddr)

	t.Cleanup(h.teardown)
	return h
}

// Canaries: one container labeled as a *foreign deployment's* agent
// and one with no deployment label at all (the pre-fix labeling), each
// with a .claude volume. The boot sweep sees both — their agent ids do
// not exist in the (fresh) e2e database, which is exactly the state
// that once made the sweep destroy real dev agents — and must classify
// them as foreign and never remove them.
type canary struct {
	name        string
	agentID     string
	containerID string
}

func (h *harness) plantCanaries(ctx context.Context) {
	h.t.Helper()
	// The base agent image builds FROM debian:bookworm-slim, so this
	// pull is warm after any prior suite/image build.
	if _, _, err := h.docker.ImageInspectWithRaw(ctx, "debian:bookworm-slim"); err != nil {
		rc, err := h.docker.ImagePull(ctx, "debian:bookworm-slim", image.PullOptions{})
		if err != nil {
			h.t.Fatalf("pulling canary image: %v", err)
		}
		io.Copy(io.Discard, rc)
		rc.Close()
	}
	plant := func(name string, labels map[string]string, agentID string) canary {
		resp, err := h.docker.ContainerCreate(ctx, &container.Config{
			Image:  "debian:bookworm-slim",
			Cmd:    []string{"sleep", "infinity"},
			Labels: labels,
		}, nil, nil, nil, name)
		if err != nil {
			h.t.Fatalf("planting canary %s: %v", name, err)
		}
		if _, err := h.docker.VolumeCreate(ctx, volume.CreateOptions{Name: "lab-claude-" + agentID}); err != nil {
			h.t.Fatalf("planting canary volume for %s: %v", name, err)
		}
		return canary{name: name, agentID: agentID, containerID: resp.ID}
	}
	foreignID, unlabeledID := uuid.NewString(), uuid.NewString()
	h.canaries = []canary{
		plant("lab-agent-"+foreignID, map[string]string{
			"lab.agent-id":   foreignID,
			"lab.project-id": uuid.NewString(),
			"lab.deployment": "canary-foreign-deployment",
		}, foreignID),
		plant("lab-agent-"+unlabeledID, map[string]string{
			"lab.agent-id":   unlabeledID,
			"lab.project-id": uuid.NewString(),
		}, unlabeledID),
	}
	h.t.Cleanup(h.removeCanaries)
}

// assertCanariesAlive fails the test if any canary container or volume
// has been removed.
func (h *harness) assertCanariesAlive(ctx context.Context) {
	h.t.Helper()
	for _, c := range h.canaries {
		if _, err := h.docker.ContainerInspect(ctx, c.containerID); err != nil {
			h.t.Errorf("canary container %s (agent %s) is gone: %v — the sweep touched a foreign deployment's container", c.name, c.agentID, err)
		}
		if _, err := h.docker.VolumeInspect(ctx, "lab-claude-"+c.agentID); err != nil {
			h.t.Errorf("canary volume lab-claude-%s is gone: %v — the sweep touched a foreign deployment's volume", c.agentID, err)
		}
	}
}

func (h *harness) removeCanaries() {
	ctx := context.Background()
	for _, c := range h.canaries {
		h.docker.ContainerRemove(ctx, c.containerID, container.RemoveOptions{Force: true})
		h.docker.VolumeRemove(ctx, "lab-claude-"+c.agentID, true)
	}
}

func (h *harness) goBuild(out, pkg string, extraEnv []string) {
	h.t.Helper()
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = h.repoRoot
	cmd.Env = append(os.Environ(), extraEnv...)
	if outp, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("go build %s: %v\n%s", pkg, err, outp)
	}
}

func (h *harness) daemonArch(ctx context.Context) string {
	h.t.Helper()
	info, err := h.docker.Info(ctx)
	if err != nil {
		h.t.Fatalf("docker info: %v", err)
	}
	switch info.Architecture {
	case "x86_64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		h.t.Fatalf("unsupported docker architecture %q", info.Architecture)
		return ""
	}
}

// daemonEnv pins every config key so neither a stray lab.toml nor the
// caller's environment can leak into the stack under test.
func (h *harness) daemonEnv() []string {
	return append(os.Environ(),
		"LAB_POSTGRES_DSN="+h.dsn,
		"LAB_DATA_DIR="+h.dataDir,
		"LAB_LOG_LEVEL=debug",
		"LAB_LABD_CLIENT_API_ADDR="+h.clientAddr,
		"LAB_LABD_AGENT_API_ADDR="+h.agentAddr,
		"LAB_LABD_CLAUDE_STUB_PATH="+filepath.Join(h.binDir, "claudestub"),
		"LAB_KBASED_LISTEN_ADDR="+h.kbasedAddr,
		"LAB_KBASED_URL=http://"+h.kbasedAddr,
		"LAB_KBASED_KBASE_URL_FOR_AGENTS=http://host.docker.internal:"+portOf(h.kbasedAddr),
		"LAB_KBASED_ADMIN_TOKEN="+adminToken,
	)
}

func portOf(addr string) string {
	_, port, _ := net.SplitHostPort(addr)
	return port
}

// startDaemon launches labd or kbased from the built binaries with the
// pinned env. cwd is the repo root so labd's image builder can resolve
// the module for cross-compiling the in-container CLIs.
func (h *harness) startDaemon(name string) *exec.Cmd {
	h.t.Helper()
	cmd := exec.Command(filepath.Join(h.binDir, name))
	cmd.Dir = h.repoRoot
	cmd.Env = h.daemonEnv()
	log, err := os.OpenFile(filepath.Join(h.binDir, name+".log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		h.t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		h.t.Fatalf("start %s: %v", name, err)
	}
	return cmd
}

func (h *harness) waitHealthz(addr string) {
	h.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	h.dumpLogs()
	h.t.Fatalf("daemon on %s not healthy within 30s", addr)
}

// dumpLogs surfaces both daemons' logs on failure.
func (h *harness) dumpLogs() {
	for _, name := range []string{"labd", "kbased"} {
		if b, err := os.ReadFile(filepath.Join(h.binDir, name+".log")); err == nil {
			h.t.Logf("── %s log ──\n%s", name, b)
		}
	}
}

func (h *harness) teardown() {
	// Graceful daemon shutdown first (SIGTERM), then remove the test
	// project's containers and volumes so a rerun starts clean.
	for _, cmd := range []*exec.Cmd{h.labd, h.kbased} {
		if cmd == nil || cmd.Process == nil {
			continue
		}
		cmd.Process.Signal(syscall.SIGTERM)
	}
	for _, cmd := range []*exec.Cmd{h.labd, h.kbased} {
		if cmd == nil || cmd.Process == nil {
			continue
		}
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(45 * time.Second):
			cmd.Process.Kill()
			<-done
		}
	}
	h.removeAgentContainers()
	if h.t.Failed() {
		h.dumpLogs()
	}
}

// removeAgentContainers force-removes THIS run's agent containers and
// their .claude volumes, scoped by the agent ids in the e2e database
// (still present — it is dropped by the NEXT run's setup). Filtering
// on the label key alone would match every lab agent on the machine
// and destroy real agents' containers and session-state volumes.
func (h *harness) removeAgentContainers() {
	ctx := context.Background()
	db, err := sql.Open("pgx", h.dsn)
	if err != nil {
		h.t.Logf("cleanup: opening e2e db: %v", err)
		return
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "SELECT id FROM lab.agents")
	if err != nil {
		h.t.Logf("cleanup: listing e2e agents: %v", err)
		return
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			h.t.Logf("cleanup: scanning agent id: %v", err)
			return
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		f := filters.NewArgs(filters.Arg("label", "lab.agent-id="+id))
		list, err := h.docker.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
		if err != nil {
			h.t.Logf("cleanup: listing containers for agent %s: %v", id, err)
			continue
		}
		for _, c := range list {
			h.docker.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
		}
		h.docker.VolumeRemove(ctx, "lab-claude-"+id, true)
	}
}

// restartLabd relaunches labd after a kill and waits for health (the
// boot sweep runs before the listeners come up, so a healthy daemon
// implies the sweep finished).
func (h *harness) restartLabd() {
	h.t.Helper()
	h.labd = h.startDaemon("labd")
	h.waitHealthz(h.clientAddr)
}

// doJSON covers the two client-API calls labclient lacks (credential
// create, budget set).
func (h *harness) doJSON(method, path string, body any, wantStatus int) {
	h.t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		h.t.Fatal(err)
	}
	req, err := http.NewRequest(method, "http://"+h.clientAddr+path, bytes.NewReader(b))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		h.t.Fatalf("%s %s = %d, want %d; body: %s", method, path, resp.StatusCode, wantStatus, raw)
	}
}

// waitTurn polls a turn until it reaches a terminal status or the
// timeout expires, returning the final turn.
func (h *harness) waitTurn(id uuid.UUID, timeout time.Duration) wire.Turn {
	h.t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	var last wire.Turn
	for time.Now().Before(deadline) {
		turn, err := h.api.GetTurn(ctx, id)
		if err == nil {
			last = turn
			if turn.Status == "done" || turn.Status == "error" {
				return turn
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	h.dumpLogs()
	h.t.Fatalf("turn %s not finished within %s (last: %+v)", id, timeout, last)
	return last
}

// waitTurnStatus polls until the turn reports the given status.
func (h *harness) waitTurnStatus(id uuid.UUID, status string, timeout time.Duration) {
	h.t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		turn, err := h.api.GetTurn(ctx, id)
		if err == nil && turn.Status == status {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	h.dumpLogs()
	h.t.Fatalf("turn %s did not reach %q within %s", id, status, timeout)
}

// currentSession returns the agent's open session id.
func (h *harness) currentSession() uuid.UUID {
	h.t.Helper()
	agents, err := h.api.ListAgents(context.Background(), project)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, a := range agents {
		if a.Name == agentName {
			if a.SessionID == nil {
				h.t.Fatal("agent has no open session")
			}
			return *a.SessionID
		}
	}
	h.t.Fatalf("agent %s not found", agentName)
	return uuid.Nil
}

// sessionText concatenates the assistant text of a session's events.
func (h *harness) sessionText(session uuid.UUID) string {
	h.t.Helper()
	events, err := h.api.AllSessionEvents(context.Background(), session, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	var sb strings.Builder
	for _, e := range events {
		var payload struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(e.Payload, &payload) == nil && payload.Type == "assistant" {
			for _, c := range payload.Message.Content {
				sb.WriteString(c.Text)
				sb.WriteString("\n")
			}
		}
	}
	return sb.String()
}

// TestE2E runs the whole scripted loop in order; each stage depends on
// the previous one's state, mirroring real operation.
func TestE2E(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Origin: a local git repo with one commit (worktrees need a HEAD).
	origin := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "e2e@example.invalid"},
		{"config", "user.name", "e2e"},
		{"commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = origin
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	var firstTurnSession uuid.UUID

	t.Run("01_core_loop", func(t *testing.T) {
		// Credential: a fake API key — the stub never talks to Anthropic,
		// but rollups and budget enforcement key off the credential row.
		h.doJSON("POST", "/v1/credentials", wire.CreateCredentialRequest{
			Kind: "api_key", Label: "e2e-fake", Secret: "sk-e2e-fake-not-a-secret",
		}, http.StatusCreated)

		if _, err := h.api.CreateProject(ctx, wire.CreateProjectRequest{
			Name: project, Origin: origin, Stack: "base",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.api.CreateAgent(ctx, project, wire.CreateAgentRequest{
			Name: agentName, RolePrompt: "you are a deterministic stub",
			CredentialKind: "api_key",
		}); err != nil {
			t.Fatal(err)
		}
		if err := h.api.StartAgent(ctx, project, agentName); err != nil {
			t.Fatal(err)
		}

		turn, err := h.api.SubmitTurn(ctx, project, agentName, "hello stub")
		if err != nil {
			t.Fatal(err)
		}
		// Generous: the first turn includes the base image build.
		done := h.waitTurn(turn.ID, 10*time.Minute)
		if done.Status != "done" {
			t.Fatalf("turn = %+v, want done", done)
		}

		firstTurnSession = h.currentSession()
		text := h.sessionText(firstTurnSession)
		if !strings.Contains(text, "stub-echo: hello stub") {
			t.Errorf("session transcript missing echo; got: %q", text)
		}

		// Usage rollups landed for the agent.
		var rollups int
		if err := h.db.QueryRow(
			"SELECT count(*) FROM lab.usage_rollups r JOIN lab.agents a ON a.id = r.agent_id WHERE a.name = $1",
			agentName).Scan(&rollups); err != nil {
			t.Fatal(err)
		}
		if rollups == 0 {
			t.Error("no usage_rollups rows after a completed turn")
		}
	})

	t.Run("02_second_turn_same_process", func(t *testing.T) {
		turn, err := h.api.SubmitTurn(ctx, project, agentName, "second turn")
		if err != nil {
			t.Fatal(err)
		}
		if done := h.waitTurn(turn.ID, 2*time.Minute); done.Status != "done" {
			t.Fatalf("turn = %+v, want done", done)
		}
		if sess := h.currentSession(); sess != firstTurnSession {
			t.Errorf("session changed between turns: %s → %s", firstTurnSession, sess)
		}
		if !strings.Contains(h.sessionText(firstTurnSession), "stub-echo: second turn") {
			t.Error("second turn's echo missing from the transcript")
		}
	})

	t.Run("03_kill9_recovery_sweep", func(t *testing.T) {
		stuck, err := h.api.SubmitTurn(ctx, project, agentName, "sleep:600")
		if err != nil {
			t.Fatal(err)
		}
		h.waitTurnStatus(stuck.ID, "running", 2*time.Minute)

		// kill -9: no drain, no cleanup — the crash the sweep exists for.
		if err := h.labd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		h.labd.Wait()
		h.restartLabd()

		// The sweep ran before the API came up: the stuck turn is
		// errored, not wedged.
		turn, err := h.api.GetTurn(ctx, stuck.ID)
		if err != nil {
			t.Fatal(err)
		}
		if turn.Status != "error" || turn.Error == nil || *turn.Error != "labd restarted mid-turn" {
			t.Fatalf("stuck turn = %q / %v, want error / labd restarted mid-turn", turn.Status, turn.Error)
		}

		// And the agent answers the next turn normally.
		fresh, err := h.api.SubmitTurn(ctx, project, agentName, "after the crash")
		if err != nil {
			t.Fatal(err)
		}
		if done := h.waitTurn(fresh.ID, 3*time.Minute); done.Status != "done" {
			t.Fatalf("post-recovery turn = %+v, want done", done)
		}

		// The restart's boot sweep saw the canaries (agent ids unknown
		// to lab_e2e) and must have classified them foreign, not
		// removed them.
		h.assertCanariesAlive(ctx)
	})

	t.Run("04_retire_chains_successor", func(t *testing.T) {
		old := h.currentSession()
		resp, err := h.api.RetireAgent(ctx, project, agentName, wire.RetireAgentRequest{
			Reason: "e2e retirement", Seed: "recover your state",
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp.PrevSessionID == nil || *resp.PrevSessionID != old {
			t.Fatalf("successor prev = %v, want %s", resp.PrevSessionID, old)
		}
		if resp.SessionID == old {
			t.Fatal("retire did not open a new session")
		}

		// The seed turn delivers into the successor session.
		deadline := time.Now().Add(3 * time.Minute)
		for {
			if strings.Contains(h.sessionText(resp.SessionID), "stub-echo: recover your state") {
				break
			}
			if time.Now().After(deadline) {
				h.dumpLogs()
				t.Fatal("seed turn not answered in the successor session")
			}
			time.Sleep(500 * time.Millisecond)
		}
	})

	t.Run("05_budget_deny_raise_release", func(t *testing.T) {
		budgetPath := "/v1/projects/" + project + "/agents/" + agentName + "/budget"
		// Several turns have completed this hour, so one is already too
		// many: the gate denies and the turn stays queued.
		h.doJSON("PUT", budgetPath, wire.BudgetPayload{
			Budget: json.RawMessage(`{"max_turns_hour": 1}`),
		}, http.StatusOK)

		held, err := h.api.SubmitTurn(ctx, project, agentName, "held by budget")
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Second) // several pump poll cycles
		turn, err := h.api.GetTurn(ctx, held.ID)
		if err != nil {
			t.Fatal(err)
		}
		if turn.Status != "queued" {
			t.Fatalf("held turn = %q, want still queued (denied, never errored)", turn.Status)
		}

		// Raise the budget: the queued turn delivers without any other
		// intervention.
		h.doJSON("PUT", budgetPath, wire.BudgetPayload{
			Budget: json.RawMessage(`{"max_turns_hour": 10000}`),
		}, http.StatusOK)
		if done := h.waitTurn(held.ID, 2*time.Minute); done.Status != "done" {
			t.Fatalf("released turn = %+v, want done", done)
		}
	})

	t.Run("06_canaries_survive_the_suite", func(t *testing.T) {
		// End-of-suite check: after every sweep, provision cycle, and
		// the kill-9 restart, the foreign-labeled and unlabeled canary
		// containers and their volumes are untouched.
		h.assertCanariesAlive(ctx)
	})
}
