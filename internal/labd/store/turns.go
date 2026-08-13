package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const turnCols = "id, agent_id, session_id, source_kind, source_id, content, status, error, created_at, finished_at"

// TurnsNotifyChannel is the Postgres NOTIFY channel EnqueueTurn
// signals on. The payload is the agent_id; hosted drivers wake their
// idle poll on it (the poll remains the fallback).
const TurnsNotifyChannel = "lab_turns"

// EnqueueTurn appends a turn to the agent's queue and NOTIFYs
// lab_turns with the agent id. The notify is best-effort (outside the
// insert's implicit transaction); listeners still poll.
func (s *Store) EnqueueTurn(ctx context.Context, t NewTurn) (Turn, error) {
	rows, _ := s.pool.Query(ctx, `
		INSERT INTO lab.turns (agent_id, session_id, source_kind, source_id, content)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+turnCols,
		t.AgentID, t.SessionID, t.SourceKind, t.SourceID, t.Content)
	turn, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Turn])
	if err != nil {
		return Turn{}, fmt.Errorf("enqueue turn: %w", err)
	}
	if _, err := s.pool.Exec(ctx,
		"SELECT pg_notify($1, $2::text)", TurnsNotifyChannel, t.AgentID); err != nil {
		return turn, fmt.Errorf("enqueue turn: notify: %w", err)
	}
	return turn, nil
}

func (s *Store) GetTurn(ctx context.Context, id uuid.UUID) (Turn, error) {
	rows, _ := s.pool.Query(ctx,
		"SELECT "+turnCols+" FROM lab.turns WHERE id = $1", id)
	turn, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Turn])
	if errors.Is(err, pgx.ErrNoRows) {
		return Turn{}, ErrNotFound
	}
	if err != nil {
		return Turn{}, fmt.Errorf("get turn: %w", err)
	}
	return turn, nil
}

// NextQueuedTurn atomically flips the agent's oldest queued turn to
// running and returns it. It returns nil (no error) when the queue is
// empty or the agent already has a running turn — turns are serial per
// agent. Concurrent callers are serialized by a per-agent transaction
// advisory lock, so at most one turn per agent is ever running.
func (s *Store) NextQueuedTurn(ctx context.Context, agentID uuid.UUID) (*Turn, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("next queued turn: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// Serialize queued→running transitions per agent. Everything that
	// creates a running turn goes through this path; FinishTurn only
	// removes one, so it needs no lock.
	if _, err := tx.Exec(ctx,
		"SELECT pg_advisory_xact_lock(hashtextextended('lab.turn_queue:' || $1::text, 0))",
		agentID); err != nil {
		return nil, fmt.Errorf("next queued turn: lock: %w", err)
	}

	rows, _ := tx.Query(ctx, `
		UPDATE lab.turns SET status = 'running'
		WHERE id = (
			SELECT id FROM lab.turns
			WHERE agent_id = $1 AND status = 'queued'
			  AND NOT EXISTS (
				SELECT 1 FROM lab.turns r
				WHERE r.agent_id = $1 AND r.status = 'running'
			  )
			ORDER BY created_at, id
			LIMIT 1
		)
		RETURNING `+turnCols,
		agentID)
	turn, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Turn])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("next queued turn: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("next queued turn: commit: %w", err)
	}
	return &turn, nil
}

// PeekQueuedTurn returns the agent's oldest queued turn without
// claiming it, or nil when the queue is empty. The pump peeks before
// consulting the budget gate so a denied turn is never flipped to
// running.
func (s *Store) PeekQueuedTurn(ctx context.Context, agentID uuid.UUID) (*Turn, error) {
	rows, _ := s.pool.Query(ctx, `
		SELECT `+turnCols+` FROM lab.turns
		WHERE agent_id = $1 AND status = 'queued'
		ORDER BY created_at, id
		LIMIT 1`,
		agentID)
	turn, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Turn])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("peek queued turn: %w", err)
	}
	return &turn, nil
}

// RunningTurn returns the agent's running turn, or nil if none. The
// driver uses it on startup to error turns orphaned by a crash.
func (s *Store) RunningTurn(ctx context.Context, agentID uuid.UUID) (*Turn, error) {
	rows, _ := s.pool.Query(ctx, `
		SELECT `+turnCols+` FROM lab.turns
		WHERE agent_id = $1 AND status = 'running'
		ORDER BY created_at, id
		LIMIT 1`,
		agentID)
	turn, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Turn])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("running turn: %w", err)
	}
	return &turn, nil
}

// SetTurnSession stamps the session a turn was delivered in. Returns
// ErrNotFound if the turn does not exist.
func (s *Store) SetTurnSession(ctx context.Context, id, sessionID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE lab.turns SET session_id = $2 WHERE id = $1", id, sessionID)
	if err != nil {
		return fmt.Errorf("set turn session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// FinishTurn closes a running turn with status done or error. errMsg
// is recorded for error; ignored (stored as NULL) for done. Returns
// ErrNotFound if the turn does not exist or is not running.
func (s *Store) FinishTurn(ctx context.Context, id uuid.UUID, status string, errMsg string) error {
	var errCol *string
	switch status {
	case TurnStatusDone:
	case TurnStatusError:
		errCol = &errMsg
	default:
		return fmt.Errorf("finish turn: invalid status %q (want done or error)", status)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE lab.turns SET status = $2, error = $3, finished_at = now()
		WHERE id = $1 AND status = 'running'`,
		id, status, errCol)
	if err != nil {
		return fmt.Errorf("finish turn: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
