package store

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
)

// ErrTokenInvalid is returned by VerifyAgentToken for unknown or
// revoked tokens. It deliberately does not distinguish the two.
var ErrTokenInvalid = errors.New("store: invalid agent token")

const tokenCols = "id, agent_id, secret_hash, created_at, revoked_at"

// hashToken is the stored form of a token secret. Only this hash ever
// touches the database or logs; the plaintext exists in memory at mint
// and verify time and in the agent container's env.
func hashToken(secret string) []byte {
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

// MintAgentToken creates a token for the agent and returns the token
// row plus the plaintext secret — the only time the plaintext is ever
// available. The caller injects it into the container env.
func (s *Store) MintAgentToken(ctx context.Context, agentID uuid.UUID) (AgentToken, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return AgentToken{}, "", fmt.Errorf("mint agent token: %w", err)
	}
	secret := hex.EncodeToString(raw)
	rows, _ := s.pool.Query(ctx, `
		INSERT INTO lab.agent_tokens (agent_id, secret_hash)
		VALUES ($1, $2)
		RETURNING `+tokenCols,
		agentID, hashToken(secret))
	tok, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[AgentToken])
	if err != nil {
		return AgentToken{}, "", fmt.Errorf("mint agent token: %w", err)
	}
	return tok, secret, nil
}

// VerifyAgentToken resolves a presented plaintext secret to its agent
// ID. Lookup is by SHA-256 hash; the fetched hash is re-checked with a
// constant-time compare. Unknown and revoked tokens both return
// ErrTokenInvalid.
func (s *Store) VerifyAgentToken(ctx context.Context, secret string) (uuid.UUID, error) {
	want := hashToken(secret)
	rows, _ := s.pool.Query(ctx, `
		SELECT `+tokenCols+` FROM lab.agent_tokens
		WHERE secret_hash = $1 AND revoked_at IS NULL`,
		want)
	tok, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[AgentToken])
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrTokenInvalid
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("verify agent token: %w", err)
	}
	if subtle.ConstantTimeCompare(tok.SecretHash, want) != 1 {
		return uuid.Nil, ErrTokenInvalid
	}
	return tok.AgentID, nil
}

// RevokeAgentTokens revokes all of the agent's active tokens and
// returns how many it revoked. Minting a fresh container token revokes
// the previous ones first.
func (s *Store) RevokeAgentTokens(ctx context.Context, agentID uuid.UUID) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE lab.agent_tokens SET revoked_at = now()
		WHERE agent_id = $1 AND revoked_at IS NULL`,
		agentID)
	if err != nil {
		return 0, fmt.Errorf("revoke agent tokens: %w", err)
	}
	return tag.RowsAffected(), nil
}
