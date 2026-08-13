package creds

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bio4554/lab/internal/labd/store"
)

func testDSN() string {
	if dsn := os.Getenv("LAB_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://lab:lab@localhost:5432/lab?sslmode=disable"
}

func TestVaultRoundTrip(t *testing.T) {
	v, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"", "x", "sk-ant-fabricated-test-secret-000", strings.Repeat("long", 1000)} {
		box, err := v.Encrypt(secret)
		if err != nil {
			t.Fatal(err)
		}
		if secret != "" && bytes.Contains(box, []byte(secret)) {
			t.Fatal("ciphertext contains the plaintext")
		}
		got, err := v.Decrypt(box)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		if got != secret {
			t.Fatalf("round trip = %q, want %q", got, secret)
		}
	}

	// Same plaintext encrypts to different ciphertexts (random nonce).
	a, _ := v.Encrypt("same")
	b, _ := v.Encrypt("same")
	if bytes.Equal(a, b) {
		t.Fatal("two encryptions of the same secret are identical (nonce reuse?)")
	}

	// Tampering and truncation fail.
	box, _ := v.Encrypt("victim")
	box[len(box)-1] ^= 0xff
	if _, err := v.Decrypt(box); err == nil {
		t.Fatal("tampered ciphertext decrypted")
	}
	if _, err := v.Decrypt(box[:10]); err == nil {
		t.Fatal("truncated ciphertext decrypted")
	}

	// A different key cannot open it.
	v2, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	box2, _ := v.Encrypt("cross-key")
	if _, err := v2.Decrypt(box2); err == nil {
		t.Fatal("wrong key decrypted the ciphertext")
	}
}

func TestKeyFileCreationAndPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, KeyFileName)

	k1, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file mode = %04o, want 0600", perm)
	}

	// Second load returns the same key.
	k2, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if *k1 != *k2 {
		t.Fatal("reload returned a different key")
	}

	// Loosened permissions refuse to load, with a chmod hint.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(dir); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("0644 key file: err = %v, want refusal with chmod hint", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}

	// A wrong-size key file is rejected, not silently padded.
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(dir); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("short key file: err = %v, want corrupt-key error", err)
	}

	// Missing data dir is created (mode 0700).
	nested := filepath.Join(t.TempDir(), "a", "b")
	if _, err := LoadOrCreateKey(nested); err != nil {
		t.Fatal(err)
	}
}

func TestEnvVarForKind(t *testing.T) {
	if v, err := EnvVarForKind(store.CredentialKindAPIKey); err != nil || v != "ANTHROPIC_API_KEY" {
		t.Fatalf("api_key = %q, %v", v, err)
	}
	if v, err := EnvVarForKind(store.CredentialKindOAuthToken); err != nil || v != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Fatalf("oauth_token = %q, %v", v, err)
	}
	if _, err := EnvVarForKind("weird"); err == nil {
		t.Fatal("unknown kind should error")
	}
}

// TestSourceResolve exercises the store-backed source against live
// Postgres: decryption + env var mapping, expiry refusal with the lazy
// status flip, and non-active refusal. Fabricated secrets only.
func TestSourceResolve(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testDSN())
	if err != nil {
		t.Fatal(err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		t.Skipf("postgres unreachable (run `make db-up`, or set LAB_TEST_DSN): %v", err)
	}
	t.Cleanup(pool.Close)
	st := store.New(pool)

	vault, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := NewSource(st, vault, slog.New(slog.NewTextHandler(io.Discard, nil)))

	const secret = "fabricated-oauth-token-for-tests"
	enc, err := vault.Encrypt(secret)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := st.CreateCredential(ctx, store.NewCredential{
		Kind: store.CredentialKindOAuthToken, SecretEnc: enc, Label: "test " + t.Name(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, "DELETE FROM lab.credentials WHERE id = $1", cred.ID); err != nil {
			t.Errorf("cleanup credential: %v", err)
		}
	})

	envVar, got, err := src.Resolve(ctx, cred)
	if err != nil {
		t.Fatal(err)
	}
	if envVar != "CLAUDE_CODE_OAUTH_TOKEN" || got != secret {
		t.Fatalf("Resolve = %q, %q", envVar, got)
	}

	// Expired: refuses and lazily flips status.
	past := time.Now().Add(-time.Hour)
	if err := st.SetCredentialExpiry(ctx, cred.ID, &past); err != nil {
		t.Fatal(err)
	}
	expired, err := st.GetCredential(ctx, cred.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.Resolve(ctx, expired); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired Resolve err = %v", err)
	}
	after, err := st.GetCredential(ctx, cred.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != store.CredentialStatusExpired {
		t.Fatalf("status after lazy check = %q, want expired", after.Status)
	}

	// Un-expiring via SetCredentialExpiry reactivates and resolves again.
	future := time.Now().Add(time.Hour)
	if err := st.SetCredentialExpiry(ctx, cred.ID, &future); err != nil {
		t.Fatal(err)
	}
	revived, err := st.GetCredential(ctx, cred.ID)
	if err != nil {
		t.Fatal(err)
	}
	if revived.Status != store.CredentialStatusActive {
		t.Fatalf("status after future expiry = %q, want active", revived.Status)
	}
	if _, got, err := src.Resolve(ctx, revived); err != nil || got != secret {
		t.Fatalf("revived Resolve = %q, %v", got, err)
	}

	// A non-active status refuses even without expiry.
	revived.Status = "revoked"
	if _, _, err := src.Resolve(ctx, revived); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked Resolve err = %v", err)
	}

	// Wrong-key decryption surfaces as an error, not a wrong secret.
	otherVault, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	otherSrc := NewSource(st, otherVault, slog.New(slog.NewTextHandler(io.Discard, nil)))
	fresh, err := st.GetCredential(ctx, cred.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := otherSrc.Resolve(ctx, fresh); err == nil {
		t.Fatal("wrong-key Resolve should error")
	}
}
