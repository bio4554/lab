package claude

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bio4554/lab/internal/labd/store"
	"github.com/bio4554/lab/internal/migrate"
)

const fixtureDir = "../../streamjson/testdata"

func testDSN() string {
	if dsn := os.Getenv("LAB_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://lab:lab@localhost:5432/lab?sslmode=disable"
}

var migrateOnce sync.Once

// testHarness is a live-Postgres store plus one credential + project +
// agent fixture, cleaned up (with every dependent row) afterwards.
type testHarness struct {
	st    *store.Store
	pool  *pgxpool.Pool
	cred  store.Credential
	proj  store.Project
	agent store.Agent
}

func newHarness(t *testing.T) *testHarness {
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
		if _, err := migrate.Lab.Up(ctx, db); err != nil {
			t.Fatalf("migrate lab up: %v", err)
		}
	})

	st := store.New(pool)
	cred, err := st.CreateCredential(ctx, store.NewCredential{
		Kind: store.CredentialKindAPIKey, SecretEnc: []byte{}, Label: "test " + t.Name(),
	})
	if err != nil {
		t.Fatal(err)
	}
	proj, err := st.CreateProject(ctx, store.NewProject{
		Name: "test-" + uuid.NewString(), OriginKind: store.OriginKindLocalPath,
		Origin: "/tmp/" + t.Name(), Stack: "base",
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, store.NewAgent{
		ProjectID: proj.ID, Name: "agent-" + uuid.NewString(),
		CredentialID: &cred.ID, Branch: "agent/test",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, q := range []string{
			"DELETE FROM lab.events WHERE agent_id = $1",
			"DELETE FROM lab.turns WHERE agent_id = $1",
			"DELETE FROM lab.sessions WHERE agent_id = $1",
			"DELETE FROM lab.usage_rollups WHERE agent_id = $1",
			"DELETE FROM lab.agents WHERE id = $1",
		} {
			if _, err := pool.Exec(ctx, q, agent.ID); err != nil {
				t.Errorf("cleanup %q: %v", q, err)
			}
		}
		if _, err := pool.Exec(ctx, "DELETE FROM lab.projects WHERE id = $1", proj.ID); err != nil {
			t.Errorf("cleanup project: %v", err)
		}
		if _, err := pool.Exec(ctx, "DELETE FROM lab.credentials WHERE id = $1", cred.ID); err != nil {
			t.Errorf("cleanup credential: %v", err)
		}
	})
	return &testHarness{st: st, pool: pool, cred: cred, proj: proj, agent: agent}
}

func testDriver(st *store.Store) *Driver {
	return New(Options{
		Store:        st,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		PollInterval: 20 * time.Millisecond,
	})
}

// fakeProc is the test's end of an in-memory claude process: the pump
// gets the process side; the test writes stdout lines and reads stdin.
type fakeProc struct {
	proc    process
	stdinR  *io.PipeReader
	stdout  *io.PipeWriter
	stderr  *io.PipeWriter
	stdinSc *bufio.Scanner
}

func newFakeProc() *fakeProc {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	return &fakeProc{
		proc:    process{stdin: stdinW, stdout: stdoutR, stderr: stderrR},
		stdinR:  stdinR,
		stdout:  stdoutW,
		stderr:  stderrW,
		stdinSc: bufio.NewScanner(stdinR),
	}
}

// readStdin returns the next stdin line the pump wrote (a user-message
// JSON line), failing the test if none arrives.
func (f *fakeProc) readStdin(t *testing.T) string {
	t.Helper()
	lineCh := make(chan string, 1)
	go func() {
		if f.stdinSc.Scan() {
			lineCh <- f.stdinSc.Text()
		} else {
			close(lineCh)
		}
	}()
	select {
	case line, ok := <-lineCh:
		if !ok {
			t.Fatal("stdin closed before a line arrived")
		}
		return line
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for stdin line")
		return ""
	}
}

func (f *fakeProc) writeLine(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(f.stdout, line+"\n"); err != nil {
		t.Fatalf("writing stdout line: %v", err)
	}
}

// endProcess simulates the claude process exiting.
func (f *fakeProc) endProcess() {
	f.stdout.Close()
	f.stderr.Close()
}

// startPump runs the pump in a goroutine and returns a channel with
// its outcome.
type pumpResult struct {
	pending *store.Turn
	err     error
}

func startPump(ctx context.Context, d *Driver, h *testHarness, sess store.Session, proc process, pending *store.Turn) chan pumpResult {
	ch := make(chan pumpResult, 1)
	go func() {
		p, err := d.pump(ctx, h.agent, sess, proc, pending)
		ch <- pumpResult{pending: p, err: err}
	}()
	return ch
}

func waitPump(t *testing.T, ch chan pumpResult) pumpResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for pump to return")
		return pumpResult{}
	}
}

// waitFor polls cond until it returns true or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func fixtureLines(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestClaudeArgs(t *testing.T) {
	base := "claude -p --input-format stream-json --output-format stream-json --verbose --dangerously-skip-permissions"
	model, sid := "claude-opus-5", "abc-123"
	cases := []struct {
		name          string
		role          string
		model, resume *string
		want          string
	}{
		{"fresh minimal", "", nil, nil, base},
		{"role and model", "be helpful", &model, nil, base + " --append-system-prompt be helpful --model claude-opus-5"},
		{"resume", "", nil, &sid, base + " --resume abc-123"},
		{"empty session id means no resume", "", nil, new(string), base},
		{"everything", "r", &model, &sid, base + " --append-system-prompt r --model claude-opus-5 --resume abc-123"},
	}
	for _, tc := range cases {
		if got := strings.Join(claudeArgs(tc.role, tc.model, tc.resume), " "); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// TestPumpFixtureStream is the core pump test: a fixture stream flows
// through an in-memory process into a live store. Events land with
// correct kinds, gapless seqs and turn tagging, the claude session id
// is captured, the turn finishes done, and usage is recorded.
func TestPumpFixtureStream(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := testDriver(h.st)
	sess, err := h.st.CreateSession(ctx, h.agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}

	lines := fixtureLines(t, "simple.jsonl")
	f := newFakeProc()
	res := startPump(ctx, d, h, sess, f.proc, nil)

	// Init arrives before any turn: persisted untagged, session id
	// captured.
	f.writeLine(t, lines[0])
	waitFor(t, "init event", func() bool {
		evs, err := h.st.EventsSince(ctx, sess.ID, 0, 10)
		return err == nil && len(evs) == 1
	})
	evs, err := h.st.EventsSince(ctx, sess.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if evs[0].Kind != "system" || evs[0].TurnID != nil || evs[0].Seq != 1 {
		t.Fatalf("init event = kind %q turn %v seq %d; want system, nil, 1", evs[0].Kind, evs[0].TurnID, evs[0].Seq)
	}
	var initLine struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &initLine); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "claude session id capture", func() bool {
		got, err := h.st.GetSession(ctx, sess.ID)
		return err == nil && got.ClaudeSessionID != nil && *got.ClaudeSessionID == initLine.SessionID
	})

	// Enqueue a turn; the pump delivers it as a user-message line.
	turn, err := h.st.EnqueueTurn(ctx, store.NewTurn{
		AgentID: h.agent.ID, SourceKind: store.SourceKindUser, Content: "summarize the repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	stdinLine := f.readStdin(t)
	var sent struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(stdinLine), &sent); err != nil {
		t.Fatalf("stdin line not JSON: %v", err)
	}
	if sent.Type != "user" || len(sent.Message.Content) != 1 || sent.Message.Content[0].Text != "summarize the repo" {
		t.Fatalf("stdin line = %s", stdinLine)
	}

	// Stream the rest of the fixture (assistant events, a wild
	// rate_limit_event, result) and wait for the turn to finish.
	for _, l := range lines[1:] {
		f.writeLine(t, l)
	}
	waitFor(t, "turn done", func() bool {
		got, err := h.st.GetTurn(ctx, turn.ID)
		return err == nil && got.Status == store.TurnStatusDone
	})
	got, err := h.st.GetTurn(ctx, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID == nil || *got.SessionID != sess.ID {
		t.Errorf("turn session = %v, want %s (stamped on delivery)", got.SessionID, sess.ID)
	}

	// All fixture lines persisted, in order, gapless, semantically
	// equal payloads (jsonb normalizes formatting), correct tagging.
	evs, err = h.st.EventsSince(ctx, sess.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != len(lines) {
		t.Fatalf("persisted %d events, want %d", len(evs), len(lines))
	}
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			t.Errorf("event %d: seq %d, want %d", i, ev.Seq, i+1)
		}
		var wantKind struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &wantKind); err != nil {
			t.Fatal(err)
		}
		if ev.Kind != wantKind.Type {
			t.Errorf("event %d: kind %q, want %q", i, ev.Kind, wantKind.Type)
		}
		var wantJSON, gotJSON any
		if err := json.Unmarshal([]byte(lines[i]), &wantJSON); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(ev.Payload, &gotJSON); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(wantJSON, gotJSON) {
			t.Errorf("event %d: payload differs from fixture line", i)
		}
		if i == 0 {
			continue // pre-turn event, tag checked above
		}
		if ev.TurnID == nil || *ev.TurnID != turn.ID {
			t.Errorf("event %d (kind %s): turn tag %v, want %s", i, ev.Kind, ev.TurnID, turn.ID)
		}
	}

	// Usage recorded from the result event.
	var wantRes struct {
		TotalCostUSD float64 `json:"total_cost_usd"`
		Usage        struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &wantRes); err != nil {
		t.Fatal(err)
	}
	usage, err := h.st.UsageInWindow(ctx, h.cred.ID, time.Now().UTC().Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	wantIn := wantRes.Usage.InputTokens + wantRes.Usage.CacheCreationInputTokens + wantRes.Usage.CacheReadInputTokens
	if usage.Turns != 1 || usage.TokensIn != wantIn || usage.TokensOut != wantRes.Usage.OutputTokens {
		t.Errorf("usage = %+v; want turns 1, in %d, out %d", usage, wantIn, wantRes.Usage.OutputTokens)
	}
	if diff := usage.CostUSD - wantRes.TotalCostUSD; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("usage cost = %v, want %v", usage.CostUSD, wantRes.TotalCostUSD)
	}

	// Process exit after a finished turn: pump reports it for the
	// replace loop, no turns harmed.
	f.endProcess()
	r := waitPump(t, res)
	if !errors.Is(r.err, errProcessExited) || r.pending != nil {
		t.Fatalf("pump = (%v, %v), want errProcessExited, nil pending", r.pending, r.err)
	}
}

// TestPumpUnknownKindsPersist feeds the hand-crafted future-kinds
// fixture: every unknown kind must persist like any other.
func TestPumpUnknownKindsPersist(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := testDriver(h.st)
	sess, err := h.st.CreateSession(ctx, h.agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}

	lines := fixtureLines(t, "unknown_kind.jsonl")
	f := newFakeProc()
	res := startPump(ctx, d, h, sess, f.proc, nil)
	for _, l := range lines {
		f.writeLine(t, l)
	}
	waitFor(t, "all events persisted", func() bool {
		evs, err := h.st.EventsSince(ctx, sess.ID, 0, 100)
		return err == nil && len(evs) == len(lines)
	})
	evs, err := h.st.EventsSince(ctx, sess.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i, ev := range evs {
		var want struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &want); err != nil {
			t.Fatal(err)
		}
		if ev.Kind != want.Type {
			t.Errorf("event %d: kind %q, want %q", i, ev.Kind, want.Type)
		}
	}
	f.endProcess()
	waitPump(t, res)
}

// TestPumpErrorResult maps an is_error result to turn status error.
func TestPumpErrorResult(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := testDriver(h.st)
	sess, err := h.st.CreateSession(ctx, h.agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := h.st.EnqueueTurn(ctx, store.NewTurn{
		AgentID: h.agent.ID, SourceKind: store.SourceKindUser, Content: "boom",
	})
	if err != nil {
		t.Fatal(err)
	}

	f := newFakeProc()
	res := startPump(ctx, d, h, sess, f.proc, nil)
	f.readStdin(t)
	f.writeLine(t, `{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"sid-err","total_cost_usd":0.01,"usage":{"input_tokens":1,"output_tokens":2}}`)
	waitFor(t, "turn error", func() bool {
		got, err := h.st.GetTurn(ctx, turn.ID)
		return err == nil && got.Status == store.TurnStatusError
	})
	got, err := h.st.GetTurn(ctx, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Error == nil || !strings.Contains(*got.Error, "error_during_execution") {
		t.Errorf("turn error = %v, want subtype recorded", got.Error)
	}
	f.endProcess()
	waitPump(t, res)
}

// TestPumpProcessDiesMidTurn: stdout EOF with a turn in flight errors
// the turn and reports errProcessExited.
func TestPumpProcessDiesMidTurn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := testDriver(h.st)
	sess, err := h.st.CreateSession(ctx, h.agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := h.st.EnqueueTurn(ctx, store.NewTurn{
		AgentID: h.agent.ID, SourceKind: store.SourceKindUser, Content: "die on me",
	})
	if err != nil {
		t.Fatal(err)
	}

	f := newFakeProc()
	res := startPump(ctx, d, h, sess, f.proc, nil)
	f.readStdin(t)
	f.writeLine(t, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"working on"}]},"session_id":"sid-mid"}`)
	f.endProcess()

	r := waitPump(t, res)
	if !errors.Is(r.err, errProcessExited) {
		t.Fatalf("pump err = %v, want errProcessExited", r.err)
	}
	got, err := h.st.GetTurn(ctx, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.TurnStatusError || got.Error == nil || !strings.Contains(*got.Error, "exited mid-turn") {
		t.Fatalf("turn = %s (%v), want error/exited mid-turn", got.Status, got.Error)
	}
}

// TestPumpSessionRetiredCarry: a queued turn addressed to a newer
// session makes the pump return it (still running) with
// errSessionRetired instead of delivering it to the old process.
func TestPumpSessionRetiredCarry(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := testDriver(h.st)
	oldSess, err := h.st.CreateSession(ctx, h.agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	newSess, err := h.st.CreateSession(ctx, h.agent.ID, &oldSess.ID)
	if err != nil {
		t.Fatal(err)
	}

	f := newFakeProc()
	res := startPump(ctx, d, h, oldSess, f.proc, nil)
	seed, err := h.st.EnqueueTurn(ctx, store.NewTurn{
		AgentID: h.agent.ID, SessionID: &newSess.ID,
		SourceKind: store.SourceKindUser, Content: "introduce yourself",
	})
	if err != nil {
		t.Fatal(err)
	}

	r := waitPump(t, res)
	f.endProcess()
	if !errors.Is(r.err, errSessionRetired) {
		t.Fatalf("pump err = %v, want errSessionRetired", r.err)
	}
	if r.pending == nil || r.pending.ID != seed.ID {
		t.Fatalf("pending = %+v, want carried seed turn %s", r.pending, seed.ID)
	}
	got, err := h.st.GetTurn(ctx, seed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.TurnStatusRunning {
		t.Fatalf("carried turn status = %s, want running (for redelivery)", got.Status)
	}

	// The replacement pump (new session) delivers the carried turn
	// immediately.
	f2 := newFakeProc()
	res2 := startPump(ctx, d, h, newSess, f2.proc, r.pending)
	line := f2.readStdin(t)
	if !strings.Contains(line, "introduce yourself") {
		t.Fatalf("redelivered line = %s", line)
	}
	f2.writeLine(t, `{"type":"result","subtype":"success","is_error":false,"session_id":"sid-new","total_cost_usd":0,"usage":{}}`)
	waitFor(t, "seed turn done", func() bool {
		got, err := h.st.GetTurn(ctx, seed.ID)
		return err == nil && got.Status == store.TurnStatusDone
	})
	f2.endProcess()
	waitPump(t, res2)
}

// TestPumpShutdownDrain: on ctx cancel the pump closes stdin, keeps
// persisting events through the grace period (a late result finishes
// the turn done), and returns ctx.Err().
func TestPumpShutdownDrain(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := testDriver(h.st)
	d.shutdownGrace = 5 * time.Second
	sess, err := h.st.CreateSession(context.Background(), h.agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := h.st.EnqueueTurn(context.Background(), store.NewTurn{
		AgentID: h.agent.ID, SourceKind: store.SourceKindUser, Content: "long task",
	})
	if err != nil {
		t.Fatal(err)
	}

	f := newFakeProc()
	res := startPump(ctx, d, h, sess, f.proc, nil)
	f.readStdin(t)

	cancel()
	// The pump must close our stdin now — the process side sees EOF.
	stdinEOF := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := f.stdinR.Read(buf)
		stdinEOF <- err
	}()
	select {
	case err := <-stdinEOF:
		if err != io.EOF {
			t.Fatalf("stdin read after cancel = %v, want io.EOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pump did not close stdin on shutdown")
	}
	// Late result during the grace drain.
	f.writeLine(t, `{"type":"result","subtype":"success","is_error":false,"session_id":"sid-drain","total_cost_usd":0,"usage":{}}`)
	f.endProcess()

	r := waitPump(t, res)
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("pump err = %v, want context.Canceled", r.err)
	}
	got, err := h.st.GetTurn(context.Background(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.TurnStatusDone {
		t.Fatalf("turn = %s (%v), want done (result drained during grace)", got.Status, got.Error)
	}
}

// TestPumpHardShutdown: grace expires without a result — the in-flight
// turn is errored.
func TestPumpHardShutdown(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := testDriver(h.st)
	d.shutdownGrace = 100 * time.Millisecond
	sess, err := h.st.CreateSession(context.Background(), h.agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := h.st.EnqueueTurn(context.Background(), store.NewTurn{
		AgentID: h.agent.ID, SourceKind: store.SourceKindUser, Content: "never finishes",
	})
	if err != nil {
		t.Fatal(err)
	}

	f := newFakeProc()
	res := startPump(ctx, d, h, sess, f.proc, nil)
	f.readStdin(t)
	cancel()

	r := waitPump(t, res)
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("pump err = %v, want context.Canceled", r.err)
	}
	got, err := h.st.GetTurn(context.Background(), turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.TurnStatusError || got.Error == nil || !strings.Contains(*got.Error, "hard shutdown") {
		t.Fatalf("turn = %s (%v), want error/hard shutdown", got.Status, got.Error)
	}
	f.endProcess()
}

// TestRetireChaining: Retire ends the old session with the reason,
// chains the new session, and queues the seed addressed to it.
func TestRetireChaining(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := testDriver(h.st) // no Runtime: container stop is skipped
	oldSess, err := h.st.CreateSession(ctx, h.agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetClaudeSessionID(ctx, oldSess.ID, "claude-old"); err != nil {
		t.Fatal(err)
	}

	next, err := d.Retire(ctx, h.agent.ID, "context exhausted", "Introduce yourself")
	if err != nil {
		t.Fatal(err)
	}
	if next.PrevSessionID == nil || *next.PrevSessionID != oldSess.ID {
		t.Fatalf("new session prev = %v, want %s", next.PrevSessionID, oldSess.ID)
	}
	if next.ClaudeSessionID != nil {
		t.Fatalf("new session claude id = %v, want nil (fresh start, no --resume)", next.ClaudeSessionID)
	}
	ended, err := h.st.GetSession(ctx, oldSess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ended.EndedAt == nil || ended.EndReason == nil || *ended.EndReason != "context exhausted" {
		t.Fatalf("old session = ended %v reason %v", ended.EndedAt, ended.EndReason)
	}
	cur, err := h.st.CurrentSession(ctx, h.agent.ID)
	if err != nil || cur == nil || cur.ID != next.ID {
		t.Fatalf("current session = %+v, %v; want %s", cur, err, next.ID)
	}
	seed, err := h.st.NextQueuedTurn(ctx, h.agent.ID)
	if err != nil || seed == nil {
		t.Fatalf("seed turn = %+v, %v", seed, err)
	}
	if seed.Content != "Introduce yourself" || seed.SessionID == nil || *seed.SessionID != next.ID {
		t.Fatalf("seed = %q session %v, want addressed to %s", seed.Content, seed.SessionID, next.ID)
	}

	// Retiring an agent with no open session still works (fresh chain
	// start).
	if err := h.st.FinishTurn(ctx, seed.ID, store.TurnStatusDone, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.st.EndSession(ctx, next.ID, "test cleanup"); err != nil {
		t.Fatal(err)
	}
	fresh, err := d.Retire(ctx, h.agent.ID, "unused", "")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.PrevSessionID != nil {
		t.Fatalf("fresh session prev = %v, want nil", fresh.PrevSessionID)
	}
}

// TestRunErrorsOrphanedTurn: Run (even when provisioning cannot
// proceed without git/docker) first errors a turn left running by a
// crashed driver, unblocking the serial queue.
func TestRunErrorsOrphanedTurn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	d := testDriver(h.st)
	if _, err := h.st.EnqueueTurn(ctx, store.NewTurn{
		AgentID: h.agent.ID, SourceKind: store.SourceKindUser, Content: "orphan me",
	}); err != nil {
		t.Fatal(err)
	}
	orphan, err := h.st.NextQueuedTurn(ctx, h.agent.ID)
	if err != nil || orphan == nil {
		t.Fatal(err)
	}

	err = d.Run(ctx, h.agent.ID)
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("Run = %v, want not-configured provisioning error", err)
	}
	got, err := h.st.GetTurn(ctx, orphan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.TurnStatusError || got.Error == nil || !strings.Contains(*got.Error, "orphaned") {
		t.Fatalf("orphan turn = %s (%v), want error/orphaned", got.Status, got.Error)
	}
}

// TestEnvCredentialSource covers the kind → env var mapping.
func TestEnvCredentialSource(t *testing.T) {
	ctx := context.Background()
	src := EnvCredentialSource{}

	t.Setenv(EnvAPIKey, "sk-test-value")
	envVar, secret, err := src.Resolve(ctx, store.Credential{Kind: store.CredentialKindAPIKey})
	if err != nil || envVar != EnvAPIKey || secret != "sk-test-value" {
		t.Fatalf("Resolve(api_key) = %q, %q, %v", envVar, secret, err)
	}

	t.Setenv(EnvOAuthToken, "")
	if _, _, err := src.Resolve(ctx, store.Credential{Kind: store.CredentialKindOAuthToken}); err == nil {
		t.Fatal("Resolve(oauth_token, unset) should error")
	}
	if _, _, err := src.Resolve(ctx, store.Credential{Kind: "weird"}); err == nil {
		t.Fatal("Resolve(unknown kind) should error")
	}
}
