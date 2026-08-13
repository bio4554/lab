package claude

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/google/uuid"
)

// Manager errors.
var (
	ErrAlreadyRunning = errors.New("claude: agent driver already running")
	ErrNotRunning     = errors.New("claude: agent driver not running")
)

// Runner is the Manager's seam onto Driver.Run; tests substitute a
// fake.
type Runner interface {
	Run(ctx context.Context, agentID uuid.UUID) error
}

// Manager hosts one Run goroutine per started agent inside labd. It
// owns only goroutine bookkeeping; all agent state lives in the store,
// written by the driver itself.
type Manager struct {
	runner Runner
	log    *slog.Logger

	mu     sync.Mutex
	agents map[uuid.UUID]*managed
}

type managed struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// NewManager returns a Manager driving agents through runner.
func NewManager(runner Runner, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{runner: runner, log: log, agents: make(map[uuid.UUID]*managed)}
}

// Start spawns the agent's driver goroutine. The driver's context is
// detached from the caller's (it lives until Stop/StopAll or the
// driver exits on its own). Returns ErrAlreadyRunning if the agent is
// already hosted.
func (m *Manager) Start(agentID uuid.UUID) error {
	m.mu.Lock()
	if _, ok := m.agents[agentID]; ok {
		m.mu.Unlock()
		return ErrAlreadyRunning
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &managed{cancel: cancel, done: make(chan struct{})}
	m.agents[agentID] = h
	m.mu.Unlock()

	go func() {
		defer close(h.done)
		defer cancel()
		err := m.runner.Run(ctx, agentID)
		if err != nil && !errors.Is(err, context.Canceled) {
			m.log.Error("agent driver exited", "agent", agentID, "error", err)
		}
		m.mu.Lock()
		delete(m.agents, agentID)
		m.mu.Unlock()
	}()
	return nil
}

// Stop cancels the agent's driver and waits for it to finish (the
// driver's own shutdown grace-drain applies) or for ctx to expire.
// Returns ErrNotRunning if the agent is not hosted.
func (m *Manager) Stop(ctx context.Context, agentID uuid.UUID) error {
	m.mu.Lock()
	h, ok := m.agents[agentID]
	m.mu.Unlock()
	if !ok {
		return ErrNotRunning
	}
	h.cancel()
	select {
	case <-h.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// StopAll cancels every hosted driver and waits for each (bounded by
// ctx). Used on daemon shutdown, before the HTTP servers drain.
func (m *Manager) StopAll(ctx context.Context) {
	m.mu.Lock()
	handles := make([]*managed, 0, len(m.agents))
	for _, h := range m.agents {
		h.cancel()
		handles = append(handles, h)
	}
	m.mu.Unlock()
	for _, h := range handles {
		select {
		case <-h.done:
		case <-ctx.Done():
			return
		}
	}
}

// IsRunning reports whether the agent has a hosted driver.
func (m *Manager) IsRunning(agentID uuid.UUID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.agents[agentID]
	return ok
}

// Running returns the hosted agent IDs.
func (m *Manager) Running() []uuid.UUID {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]uuid.UUID, 0, len(m.agents))
	for id := range m.agents {
		ids = append(ids, id)
	}
	return ids
}

// WakeHub fans turn-enqueue notifications out to hosted drivers. labd
// LISTENs on lab_turns and calls Wake; each driver's pump selects on
// Chan (wired through Options.TurnWake).
type WakeHub struct {
	mu    sync.Mutex
	chans map[uuid.UUID]chan struct{}
}

// NewWakeHub returns an empty hub.
func NewWakeHub() *WakeHub {
	return &WakeHub{chans: make(map[uuid.UUID]chan struct{})}
}

// Chan returns the agent's wake channel (created on first use).
func (w *WakeHub) Chan(agentID uuid.UUID) <-chan struct{} {
	return w.get(agentID)
}

// Wake signals the agent's channel without blocking; a wake while one
// is already pending coalesces.
func (w *WakeHub) Wake(agentID uuid.UUID) {
	select {
	case w.get(agentID) <- struct{}{}:
	default:
	}
}

func (w *WakeHub) get(agentID uuid.UUID) chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	ch, ok := w.chans[agentID]
	if !ok {
		ch = make(chan struct{}, 1)
		w.chans[agentID] = ch
	}
	return ch
}
