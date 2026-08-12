package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const credentialCols = "id, kind, secret_enc, label, status, expires_at, created_at"

func (s *Store) CreateCredential(ctx context.Context, c NewCredential) (Credential, error) {
	rows, _ := s.pool.Query(ctx, `
		INSERT INTO lab.credentials (kind, secret_enc, label, expires_at)
		VALUES ($1, $2, $3, $4)
		RETURNING `+credentialCols,
		c.Kind, c.SecretEnc, c.Label, c.ExpiresAt)
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
