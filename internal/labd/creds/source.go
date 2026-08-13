package creds

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bio4554/lab/internal/labd/store"
)

// Env var names for the two credential kinds — the same contract as
// the driver's Phase 5 env source (claude.EnvAPIKey/EnvOAuthToken;
// duplicated here so creds does not import the driver package).
// Exactly one is injected per container.
const (
	envAPIKey     = "ANTHROPIC_API_KEY"
	envOAuthToken = "CLAUDE_CODE_OAUTH_TOKEN"
)

// EnvVarForKind maps a credential kind to the env var the claude CLI
// reads it from.
func EnvVarForKind(kind string) (string, error) {
	switch kind {
	case store.CredentialKindAPIKey:
		return envAPIKey, nil
	case store.CredentialKindOAuthToken:
		return envOAuthToken, nil
	default:
		return "", fmt.Errorf("creds: unknown credential kind %q", kind)
	}
}

// Source is the store-backed claude.CredentialSource: it decrypts
// credentials.secret_enc with the vault and maps kind → env var
// exactly like the Phase 5 env source. Expiry is checked lazily here
// on every resolution (the daemon sweep also marks expired rows).
type Source struct {
	st    *store.Store
	vault *Vault
	log   *slog.Logger
}

// NewSource returns a Source resolving through st and vault.
func NewSource(st *store.Store, vault *Vault, log *slog.Logger) *Source {
	if log == nil {
		log = slog.Default()
	}
	return &Source{st: st, vault: vault, log: log}
}

// Resolve decrypts the credential and returns the env var + secret to
// inject. Expired or non-active credentials refuse to resolve; an
// expired-but-still-active row is flipped to expired on the way out.
func (s *Source) Resolve(ctx context.Context, cred store.Credential) (string, string, error) {
	if cred.ExpiresAt != nil && !time.Now().Before(*cred.ExpiresAt) {
		if cred.Status == store.CredentialStatusActive {
			if err := s.st.UpdateCredentialStatus(ctx, cred.ID, store.CredentialStatusExpired); err != nil {
				s.log.Warn("marking credential expired", "credential", cred.ID, "error", err)
			}
		}
		return "", "", fmt.Errorf("creds: credential %s (%s) expired %s", cred.Label, cred.ID, cred.ExpiresAt.Format(time.RFC3339))
	}
	if cred.Status != store.CredentialStatusActive {
		return "", "", fmt.Errorf("creds: credential %s (%s) has status %s", cred.Label, cred.ID, cred.Status)
	}
	envVar, err := EnvVarForKind(cred.Kind)
	if err != nil {
		return "", "", err
	}
	secret, err := s.vault.Decrypt(cred.SecretEnc)
	if err != nil {
		return "", "", fmt.Errorf("creds: credential %s (%s): %w", cred.Label, cred.ID, err)
	}
	return envVar, secret, nil
}
