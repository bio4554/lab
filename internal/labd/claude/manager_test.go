package claude

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeRunner blocks in Run until its context is cancelled or a
// per-agent error is injected.
type fakeRunner struct {
	mu      sync.Mutex
	started map[uuid.UUID]chan error // Run exits when it receives on this
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{started: make(map[uuid.UUID]chan error)}
}

func (f *fakeRunner) Run(ctx context.Context, agentID uuid.UUID) error {
	f.mu.Lock()
	ch := make(chan error, 1)
	f.started[agentID] = ch
	f.mu.Unlock()
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeRunner) exit(agentID uuid.UUID, err error) {
	f.mu.Lock()
	ch := f.started[agentID]
	f.mu.Unlock()
	ch <- err
}

func TestManagerLifecycle(t *testing.T) {
	f := newFakeRunner()
	m := NewManager(f, nil)
	ctx := context.Background()
	a, b := uuid.New(), uuid.New()

	if err := m.Start(a); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Start(a); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("second Start = %v, want ErrAlreadyRunning", err)
	}
	if err := m.Start(b); err != nil {
		t.Fatalf("Start(b): %v", err)
	}
	if !m.IsRunning(a) || !m.IsRunning(b) || len(m.Running()) != 2 {
		t.Errorf("running = %v", m.Running())
	}

	// Stop waits for the driver to exit on cancellation.
	if err := m.Stop(ctx, a); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if m.IsRunning(a) {
		t.Error("agent a still running after Stop")
	}
	if err := m.Stop(ctx, a); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Stop(stopped) = %v, want ErrNotRunning", err)
	}

	// A driver that exits on its own (error) deregisters itself.
	f.exit(b, errors.New("boom"))
	waitFor(t, "b to deregister", func() bool { return !m.IsRunning(b) })

	if err := m.Start(b); err != nil {
		t.Fatalf("restart after self-exit: %v", err)
	}
	m.StopAll(ctx)
	if n := len(m.Running()); n != 0 {
		t.Errorf("running after StopAll = %d", n)
	}
}

func TestWakeHub(t *testing.T) {
	w := NewWakeHub()
	a := uuid.New()
	ch := w.Chan(a)

	select {
	case <-ch:
		t.Fatal("wake before Wake()")
	default:
	}
	w.Wake(a)
	w.Wake(a) // coalesces; must not block
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("no wake delivered")
	}
	select {
	case <-ch:
		t.Fatal("coalesced wake delivered twice")
	default:
	}
}
