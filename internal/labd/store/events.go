package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const eventCols = "id, agent_id, session_id, turn_id, seq, kind, payload, ts"

// NotifyChannel is the Postgres NOTIFY channel AppendEvent signals on.
// The notification payload is {"event_id": N, "agent_id": "...",
// "session_id": "..."}.
const NotifyChannel = "lab_events"

// uniqueViolation is the Postgres error code for a unique-constraint
// conflict, used to detect seq races in AppendEvent.
const uniqueViolation = "23505"

// AppendEvent appends an event to the session's log, assigning the
// next per-session seq, and NOTIFYs lab_events in the same
// transaction. Concurrent appenders race on the (session_id, seq)
// unique constraint; losers retry, so seqs are gapless and duplicate
// free. An empty payload is stored as '{}'.
func (s *Store) AppendEvent(ctx context.Context, sessionID, agentID uuid.UUID, turnID *uuid.UUID, kind string, payload json.RawMessage) (Event, error) {
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	for {
		ev, err := s.tryAppendEvent(ctx, sessionID, agentID, turnID, kind, payload)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			if ctx.Err() != nil {
				return Event{}, fmt.Errorf("append event: %w", ctx.Err())
			}
			continue // lost the seq race; someone else committed, retry
		}
		if err != nil {
			return Event{}, fmt.Errorf("append event: %w", err)
		}
		return ev, nil
	}
}

func (s *Store) tryAppendEvent(ctx context.Context, sessionID, agentID uuid.UUID, turnID *uuid.UUID, kind string, payload json.RawMessage) (Event, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Event{}, err
	}
	defer tx.Rollback(ctx)

	rows, _ := tx.Query(ctx, `
		INSERT INTO lab.events (agent_id, session_id, turn_id, seq, kind, payload)
		SELECT $1, $2, $3, COALESCE(MAX(seq), 0) + 1, $4, $5
		FROM lab.events WHERE session_id = $2
		RETURNING `+eventCols,
		agentID, sessionID, turnID, kind, payload)
	ev, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Event])
	if err != nil {
		return Event{}, err
	}
	if _, err := tx.Exec(ctx, `
		SELECT pg_notify($1, json_build_object(
			'event_id', $2::bigint,
			'agent_id', $3::uuid,
			'session_id', $4::uuid)::text)`,
		NotifyChannel, ev.ID, agentID, sessionID); err != nil {
		return Event{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Event{}, err
	}
	return ev, nil
}

// EventsSince returns up to limit events of one session with seq >
// afterSeq, in seq order. Use afterSeq = 0 to tail from the start.
func (s *Store) EventsSince(ctx context.Context, sessionID uuid.UUID, afterSeq int64, limit int) ([]Event, error) {
	rows, _ := s.pool.Query(ctx, `
		SELECT `+eventCols+` FROM lab.events
		WHERE session_id = $1 AND seq > $2
		ORDER BY seq
		LIMIT $3`,
		sessionID, afterSeq, limit)
	events, err := pgx.CollectRows(rows, pgx.RowToStructByName[Event])
	if err != nil {
		return nil, fmt.Errorf("events since: %w", err)
	}
	return events, nil
}

// MaxEventID returns the highest event id, or 0 when the log is
// empty. The SSE stream uses it as the live-only starting point.
func (s *Store) MaxEventID(ctx context.Context) (int64, error) {
	var id int64
	if err := s.pool.QueryRow(ctx,
		"SELECT COALESCE(MAX(id), 0) FROM lab.events").Scan(&id); err != nil {
		return 0, fmt.Errorf("max event id: %w", err)
	}
	return id, nil
}

// SessionContextTokens returns the session's context occupancy: the
// latest result event's input_tokens + cache_creation_input_tokens +
// cache_read_input_tokens — what the context window actually held on
// the last turn. 0 when the session has no result event yet (fresh or
// just-retired sessions start empty).
func (s *Store) SessionContextTokens(ctx context.Context, sessionID uuid.UUID) (int64, error) {
	var tokens int64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE((payload->'usage'->>'input_tokens')::bigint, 0)
		     + COALESCE((payload->'usage'->>'cache_creation_input_tokens')::bigint, 0)
		     + COALESCE((payload->'usage'->>'cache_read_input_tokens')::bigint, 0)
		FROM lab.events
		WHERE session_id = $1 AND kind = 'result'
		ORDER BY seq DESC
		LIMIT 1`,
		sessionID).Scan(&tokens)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("session context tokens: %w", err)
	}
	return tokens, nil
}

// EventsSinceID returns up to limit events with id > afterEventID
// across all sessions, in id order — the global tail.
func (s *Store) EventsSinceID(ctx context.Context, afterEventID int64, limit int) ([]Event, error) {
	rows, _ := s.pool.Query(ctx, `
		SELECT `+eventCols+` FROM lab.events
		WHERE id > $1
		ORDER BY id
		LIMIT $2`,
		afterEventID, limit)
	events, err := pgx.CollectRows(rows, pgx.RowToStructByName[Event])
	if err != nil {
		return nil, fmt.Errorf("events since id: %w", err)
	}
	return events, nil
}
