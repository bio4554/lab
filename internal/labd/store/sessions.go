package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const sessionCols = "id, agent_id, claude_session_id, started_at, ended_at, end_reason, prev_session_id"

// CreateSession starts a session for the agent. prevSessionID, when
// non-nil, chains it to the retired session it replaces.
func (s *Store) CreateSession(ctx context.Context, agentID uuid.UUID, prevSessionID *uuid.UUID) (Session, error) {
	rows, _ := s.pool.Query(ctx, `
		INSERT INTO lab.sessions (agent_id, prev_session_id)
		VALUES ($1, $2)
		RETURNING `+sessionCols,
		agentID, prevSessionID)
	sess, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Session])
	if err != nil {
		return Session{}, fmt.Errorf("create session: %w", err)
	}
	return sess, nil
}

func (s *Store) GetSession(ctx context.Context, id uuid.UUID) (Session, error) {
	rows, _ := s.pool.Query(ctx,
		"SELECT "+sessionCols+" FROM lab.sessions WHERE id = $1", id)
	sess, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Session])
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("get session: %w", err)
	}
	return sess, nil
}

// EndSession marks an open session ended with the given reason.
// Returns ErrNotFound if the session does not exist or already ended.
func (s *Store) EndSession(ctx context.Context, id uuid.UUID, reason string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE lab.sessions SET ended_at = now(), end_reason = $2
		WHERE id = $1 AND ended_at IS NULL`,
		id, reason)
	if err != nil {
		return fmt.Errorf("end session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetClaudeSessionID records the Claude Code session id once the CLI
// reports it, enabling --resume after restarts.
func (s *Store) SetClaudeSessionID(ctx context.Context, id uuid.UUID, claudeSessionID string) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE lab.sessions SET claude_session_id = $2 WHERE id = $1", id, claudeSessionID)
	if err != nil {
		return fmt.Errorf("set claude session id: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CurrentSession returns the agent's open (not ended) session, or nil
// if the agent has none.
func (s *Store) CurrentSession(ctx context.Context, agentID uuid.UUID) (*Session, error) {
	rows, _ := s.pool.Query(ctx, `
		SELECT `+sessionCols+` FROM lab.sessions
		WHERE agent_id = $1 AND ended_at IS NULL
		ORDER BY started_at DESC, id DESC
		LIMIT 1`,
		agentID)
	sess, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Session])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("current session: %w", err)
	}
	return &sess, nil
}
