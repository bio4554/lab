package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const credentialCols = "id, kind, secret_enc, label, status, expires_at, budget, limited_until, created_at"

func (s *Store) CreateCredential(ctx context.Context, c NewCredential) (Credential, error) {
	budget := c.Budget
	if len(budget) == 0 {
		budget = []byte("{}")
	}
	rows, _ := s.pool.Query(ctx, `
		INSERT INTO lab.credentials (kind, secret_enc, label, expires_at, budget)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+credentialCols,
		c.Kind, c.SecretEnc, c.Label, c.ExpiresAt, budget)
	cred, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Credential])
	if err != nil {
		return Credential{}, fmt.Errorf("create credential: %w", err)
	}
	return cred, nil
}

func (s *Store) GetCredential(ctx context.Context, id uuid.UUID) (Credential, error) {
	rows, _ := s.pool.Query(ctx,
		"SELECT "+credentialCols+" FROM lab.credentials WHERE id = $1", id)
	cred, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Credential])
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, ErrNotFound
	}
	if err != nil {
		return Credential{}, fmt.Errorf("get credential: %w", err)
	}
	return cred, nil
}

func (s *Store) ListCredentials(ctx context.Context) ([]Credential, error) {
	rows, _ := s.pool.Query(ctx,
		"SELECT "+credentialCols+" FROM lab.credentials ORDER BY created_at, id")
	creds, err := pgx.CollectRows(rows, pgx.RowToStructByName[Credential])
	if err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}
	return creds, nil
}

func (s *Store) UpdateCredentialStatus(ctx context.Context, id uuid.UUID, status string) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE lab.credentials SET status = $2 WHERE id = $1", id, status)
	if err != nil {
		return fmt.Errorf("update credential status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetCredentialBudget replaces the credential's budget limits (see
// budget.Limits for the jsonb contract).
func (s *Store) SetCredentialBudget(ctx context.Context, id uuid.UUID, budget json.RawMessage) error {
	if len(budget) == 0 {
		budget = []byte("{}")
	}
	tag, err := s.pool.Exec(ctx,
		"UPDATE lab.credentials SET budget = $2 WHERE id = $1", id, budget)
	if err != nil {
		return fmt.Errorf("set credential budget: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetCredentialExpiry sets (or clears, with nil) expires_at. A
// credential already marked expired goes back to active when its new
// expiry is in the future or removed.
func (s *Store) SetCredentialExpiry(ctx context.Context, id uuid.UUID, expiresAt *time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE lab.credentials SET expires_at = $2,
			status = CASE
				WHEN status = 'expired' AND ($2::timestamptz IS NULL OR $2::timestamptz > now()) THEN 'active'
				ELSE status
			END
		WHERE id = $1`, id, expiresAt)
	if err != nil {
		return fmt.Errorf("set credential expiry: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetCredentialLimited records a rate-limit hold until the given time;
// nil clears the hold (manual resume).
func (s *Store) SetCredentialLimited(ctx context.Context, id uuid.UUID, until *time.Time) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE lab.credentials SET limited_until = $2 WHERE id = $1", id, until)
	if err != nil {
		return fmt.Errorf("set credential limited: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// PauseAgentsForCredential flips every idle/working agent using the
// credential to paused (a rate-limit hold: containers stay up, turns
// stay queued). Returns the number of agents paused.
func (s *Store) PauseAgentsForCredential(ctx context.Context, credentialID uuid.UUID) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE lab.agents SET state = $2
		WHERE credential_id = $1 AND state IN ($3, $4)`,
		credentialID, AgentStatePaused, AgentStateIdle, AgentStateWorking)
	if err != nil {
		return 0, fmt.Errorf("pause agents for credential: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ResumeAgentsForCredential flips the credential's paused agents back
// to idle. Returns the number of agents resumed.
func (s *Store) ResumeAgentsForCredential(ctx context.Context, credentialID uuid.UUID) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE lab.agents SET state = $2
		WHERE credential_id = $1 AND state = $3`,
		credentialID, AgentStateIdle, AgentStatePaused)
	if err != nil {
		return 0, fmt.Errorf("resume agents for credential: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ExpireCredentials marks active credentials whose expires_at has
// passed as expired (the daemon's periodic sweep; resolution also
// checks lazily). Returns the number of credentials flipped.
func (s *Store) ExpireCredentials(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE lab.credentials SET status = $2
		WHERE status = $3 AND expires_at IS NOT NULL AND expires_at <= $1`,
		now, CredentialStatusExpired, CredentialStatusActive)
	if err != nil {
		return 0, fmt.Errorf("expire credentials: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ReleaseExpiredLimits clears rate-limit holds whose reset time has
// passed and resumes (paused → idle) the agents of those credentials,
// atomically. Returns the number of credentials released.
func (s *Store) ReleaseExpiredLimits(ctx context.Context, now time.Time) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("release expired limits: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		UPDATE lab.agents SET state = $2
		WHERE state = $3 AND credential_id IN (
			SELECT id FROM lab.credentials WHERE limited_until IS NOT NULL AND limited_until <= $1
		)`, now, AgentStateIdle, AgentStatePaused); err != nil {
		return 0, fmt.Errorf("release expired limits: resume agents: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE lab.credentials SET limited_until = NULL
		WHERE limited_until IS NOT NULL AND limited_until <= $1`, now)
	if err != nil {
		return 0, fmt.Errorf("release expired limits: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("release expired limits: commit: %w", err)
	}
	return tag.RowsAffected(), nil
}

// CountCredentialRefs counts the rows referencing the credential
// (agents plus project defaults); deletion is refused while non-zero.
func (s *Store) CountCredentialRefs(ctx context.Context, id uuid.UUID) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM lab.agents WHERE credential_id = $1)
		     + (SELECT count(*) FROM lab.projects WHERE default_credential_id = $1)`,
		id).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count credential refs: %w", err)
	}
	return n, nil
}

func (s *Store) DeleteCredential(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM lab.credentials WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("delete credential: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
