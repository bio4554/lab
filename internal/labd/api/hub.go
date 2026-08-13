package api

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bio4554/lab/internal/labd/store"
)

// Hub is labd's single LISTEN connection. It listens on lab_events
// (event appended → wake SSE subscribers) and lab_turns (turn
// enqueued → wake that agent's hosted driver) and fans out.
//
// Subscribers receive coalesced wake-ups, not payloads: on each wake
// the SSE handler reads new rows from the store by id, which makes the
// stream gapless and duplicate-free by construction even across
// notification loss or hub reconnects.
type Hub struct {
	pool *pgxpool.Pool
	log  *slog.Logger
	// OnTurn, when non-nil, is invoked with the agent id of every
	// lab_turns notification (labd wires it to the driver WakeHub).
	onTurn func(uuid.UUID)

	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

// NewHub returns a Hub; Run must be started for wake-ups to flow.
func NewHub(pool *pgxpool.Pool, onTurn func(uuid.UUID), log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{pool: pool, log: log, onTurn: onTurn, subs: make(map[chan struct{}]struct{})}
}

// Subscribe registers an event-wake channel. cancel unregisters it.
func (h *Hub) Subscribe() (wake <-chan struct{}, cancel func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

// broadcast wakes every subscriber (coalescing pending wakes).
func (h *Hub) broadcast() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Run holds the LISTEN connection until ctx is cancelled, reconnecting
// with backoff on connection loss. Notification loss during a
// reconnect is safe: subscribers re-read from the store on every wake,
// and their next wake (or the SSE keepalive-independent poll on
// subscribe) catches them up.
func (h *Hub) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := h.listen(ctx); err != nil && ctx.Err() == nil {
			h.log.Warn("event listener disconnected; retrying", "error", err)
		}
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
		}
	}
}

func (h *Hub) listen(ctx context.Context) error {
	conn, err := h.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	for _, ch := range []string{store.NotifyChannel, store.TurnsNotifyChannel} {
		if _, err := conn.Exec(ctx, "LISTEN "+ch); err != nil {
			return err
		}
	}
	// Late subscribers missed nothing (they read the store first), but
	// anyone waiting across a reconnect gap should re-check now.
	h.broadcast()
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		switch n.Channel {
		case store.NotifyChannel:
			h.broadcast()
		case store.TurnsNotifyChannel:
			if h.onTurn == nil {
				continue
			}
			id, err := uuid.Parse(n.Payload)
			if err != nil {
				h.log.Warn("bad lab_turns payload", "payload", n.Payload)
				continue
			}
			h.onTurn(id)
		}
	}
}
