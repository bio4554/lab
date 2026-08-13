package claude

import (
	"context"
	"fmt"
	"os"

	"github.com/bio4554/lab/internal/labd/store"
)

// Env var names for the two credential kinds. Exactly one is injected
// into an agent container, to avoid CLI precedence surprises.
const (
	EnvAPIKey     = "ANTHROPIC_API_KEY"
	EnvOAuthToken = "CLAUDE_CODE_OAUTH_TOKEN"
)

// CredentialSource resolves a credential row to the env var + secret
// value injected into the agent container. Phase 8 replaces the env
// passthrough with decryption of Credential.SecretEnc; the driver
// never logs or persists the returned secret.
type CredentialSource interface {
	Resolve(ctx context.Context, cred store.Credential) (envVar, secret string, err error)
}

// EnvCredentialSource reads the secret from the driver's own process
// environment, choosing the variable by the credential's kind. This is
// the Phase 5 stand-in for real credential storage.
type EnvCredentialSource struct{}

// Resolve maps the credential kind to its env var and returns the
// variable's current value. It errors when the variable is unset.
func (EnvCredentialSource) Resolve(_ context.Context, cred store.Credential) (string, string, error) {
	var envVar string
	switch cred.Kind {
	case store.CredentialKindAPIKey:
		envVar = EnvAPIKey
	case store.CredentialKindOAuthToken:
		envVar = EnvOAuthToken
	default:
		return "", "", fmt.Errorf("claude: unknown credential kind %q", cred.Kind)
	}
	secret := os.Getenv(envVar)
	if secret == "" {
		return "", "", fmt.Errorf("claude: credential kind %s requires %s in the driver environment", cred.Kind, envVar)
	}
	return envVar, secret, nil
}
