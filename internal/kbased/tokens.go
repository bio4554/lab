package kbased

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bio4554/lab/internal/kbclient"
)

// ErrTokenInvalid is returned by VerifyToken for unknown or revoked
// tokens. It deliberately does not distinguish the two.
var ErrTokenInvalid = errors.New("kbased: invalid token")

// Caller is the authenticated identity of one request: the principal
// behind the token plus the token's project scope (nil = all
// projects).
type Caller struct {
	Principal kbclient.Principal
	ProjectID *uuid.UUID
}

// Scope returns the caller's visibility scope.
func (c Caller) Scope() Scope {
	return Scope{ProjectID: c.ProjectID}
}

// hashToken is the stored form of a token secret. Only this hash ever
// touches the database or logs; the plaintext exists in memory at mint
// and verify time and in the client's env.
func hashToken(secret string) []byte {
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

// MintToken creates a token for the principal (projectID nil =
// scope-less) and returns the response carrying the plaintext secret —
// the only time the plaintext is ever available.
func (s *Store) MintToken(ctx context.Context, principalID uuid.UUID, projectID *uuid.UUID) (kbclient.MintTokenResponse, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return kbclient.MintTokenResponse{}, fmt.Errorf("mint token: %w", err)
	}
	secret := hex.EncodeToString(raw)
	var resp kbclient.MintTokenResponse
	err := s.pool.QueryRow(ctx, `
		INSERT INTO kbase.tokens (principal_id, secret_hash, project_id)
		VALUES ($1, $2, $3)
		RETURNING id, principal_id, project_id, created_at`,
		principalID, hashToken(secret), projectID).
		Scan(&resp.TokenID, &resp.PrincipalID, &resp.ProjectID, &resp.CreatedAt)
	if err != nil {
		return kbclient.MintTokenResponse{}, fmt.Errorf("mint token: %w", err)
	}
	resp.Token = secret
	return resp, nil
}

// VerifyToken resolves a presented plaintext secret to its caller.
// Lookup is by SHA-256 hash; the fetched hash is re-checked with a
// constant-time compare. Unknown and revoked tokens both return
// ErrTokenInvalid.
func (s *Store) VerifyToken(ctx context.Context, secret string) (Caller, error) {
	want := hashToken(secret)
	var (
		hash   []byte
		caller Caller
	)
	err := s.pool.QueryRow(ctx, `
		SELECT t.secret_hash, t.project_id,
		       p.id, p.kind, p.external_id, p.display_name, p.created_at
		FROM kbase.tokens t
		JOIN kbase.principals p ON p.id = t.principal_id
		WHERE t.secret_hash = $1 AND t.revoked_at IS NULL`,
		want).
		Scan(&hash, &caller.ProjectID,
			&caller.Principal.ID, &caller.Principal.Kind,
			&caller.Principal.ExternalID, &caller.Principal.DisplayName,
			&caller.Principal.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Caller{}, ErrTokenInvalid
	}
	if err != nil {
		return Caller{}, fmt.Errorf("verify token: %w", err)
	}
	if subtle.ConstantTimeCompare(hash, want) != 1 {
		return Caller{}, ErrTokenInvalid
	}
	return caller, nil
}

// RevokeTokens revokes the principal's active tokens whose project
// scope equals projectID (nil = the scope-less tokens). Minting a
// fresh token for a scope revokes the previous ones first — one live
// token per (principal, scope).
func (s *Store) RevokeTokens(ctx context.Context, principalID uuid.UUID, projectID *uuid.UUID) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE kbase.tokens SET revoked_at = now()
		WHERE principal_id = $1 AND revoked_at IS NULL
		  AND project_id IS NOT DISTINCT FROM $2`,
		principalID, projectID)
	if err != nil {
		return 0, fmt.Errorf("revoke tokens: %w", err)
	}
	return tag.RowsAffected(), nil
}
