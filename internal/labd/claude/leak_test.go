package claude

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bio4554/lab/internal/labd/creds"
	"github.com/bio4554/lab/internal/labd/store"
)

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestNoSecretLeaks runs a full mint → resolve → deliver cycle with a
// fabricated secret and asserts the plaintext appears nowhere but the
// vault-decrypted env map: not in any log line the driver emitted, not
// in the events table, not in the stored credential row.
func TestNoSecretLeaks(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const secret = "fabricated-leak-canary-tCq27-never-log-me"

	// Mint: encrypt into the credential row the harness agent uses.
	vault, err := creds.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	enc, err := vault.Encrypt(secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx,
		"UPDATE lab.credentials SET secret_enc = $2 WHERE id = $1", h.cred.ID, enc); err != nil {
		t.Fatal(err)
	}

	// Driver with every log line captured and the store-backed source.
	logs := &syncBuffer{}
	d := New(Options{
		Store:        h.st,
		Creds:        creds.NewSource(h.st, vault, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		Logger:       slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		PollInterval: 20 * time.Millisecond,
	})

	// Resolve: the env map is the one sanctioned home of the plaintext.
	agent, err := h.st.GetAgent(ctx, h.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	env, err := d.resolveEnv(ctx, agent)
	if err != nil {
		t.Fatal(err)
	}
	if env["ANTHROPIC_API_KEY"] != secret {
		t.Fatalf("resolveEnv env = %v, want the decrypted secret under ANTHROPIC_API_KEY", env)
	}

	// Deliver: pump a full fixture turn (init, assistant, result).
	sess, err := h.st.CreateSession(ctx, h.agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := h.st.EnqueueTurn(ctx, store.NewTurn{
		AgentID: h.agent.ID, SourceKind: store.SourceKindUser, Content: "do the thing",
	})
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeProc()
	res := startPump(ctx, d, h, sess, f.proc, nil)
	f.readStdin(t)
	for _, l := range fixtureLines(t, "simple.jsonl") {
		f.writeLine(t, l)
	}
	waitFor(t, "turn done", func() bool {
		got, err := h.st.GetTurn(ctx, turn.ID)
		return err == nil && got.Status == store.TurnStatusDone
	})
	f.endProcess()
	waitPump(t, res)

	// Scan: logs.
	if got := logs.String(); strings.Contains(got, secret) {
		t.Errorf("secret appeared in driver logs:\n%s", got)
	}
	// Scan: every event payload of the session.
	events, err := h.st.EventsSince(ctx, sess.ID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("no events persisted; scan proves nothing")
	}
	for _, ev := range events {
		if strings.Contains(string(ev.Payload), secret) {
			t.Errorf("secret appeared in event %d (%s)", ev.Seq, ev.Kind)
		}
	}
	// Scan: the credential row holds ciphertext, not the plaintext.
	cred, err := h.st.GetCredential(ctx, h.cred.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(cred.SecretEnc, []byte(secret)) {
		t.Error("secret_enc contains the plaintext")
	}
	// Scan: turn rows (content + error columns).
	gotTurn, err := h.st.GetTurn(ctx, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotTurn.Content, secret) || (gotTurn.Error != nil && strings.Contains(*gotTurn.Error, secret)) {
		t.Error("secret appeared in the turn row")
	}
}
