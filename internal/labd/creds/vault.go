// Package creds is real credential storage: a NaCl secretbox vault
// keyed from <data_dir>/secret.key encrypts credentials.secret_enc at
// rest, and a store-backed CredentialSource decrypts on container
// provisioning. Plaintext secrets exist only in memory (and the agent
// container's env); they are never logged, persisted, or returned by
// any API.
package creds

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/crypto/nacl/secretbox"
)

// KeyFileName is the vault key file under the lab data dir.
const KeyFileName = "secret.key"

const (
	keySize   = 32
	nonceSize = 24
)

// LoadOrCreateKey returns the 32-byte vault key at
// <dataDir>/secret.key, generating the file (mode 0600) on first use.
// It refuses a key file readable by group or other so a copied or
// chmod-ed key is caught before any secret is trusted to it.
func LoadOrCreateKey(dataDir string) (*[keySize]byte, error) {
	path := filepath.Join(dataDir, KeyFileName)
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return createKey(dataDir, path)
	}
	if err != nil {
		return nil, fmt.Errorf("creds: stat key file: %w", err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("creds: key file %s has mode %04o, readable beyond its owner; fix with: chmod 600 %s", path, perm, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("creds: read key file: %w", err)
	}
	if len(data) != keySize {
		return nil, fmt.Errorf("creds: key file %s is %d bytes, want %d; it is corrupt — restore it or remove it to generate a new key (existing credentials become undecryptable)", path, len(data), keySize)
	}
	key := new([keySize]byte)
	copy(key[:], data)
	return key, nil
}

func createKey(dataDir, path string) (*[keySize]byte, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creds: create data dir: %w", err)
	}
	key := new([keySize]byte)
	if _, err := rand.Read(key[:]); err != nil {
		return nil, fmt.Errorf("creds: generate key: %w", err)
	}
	// O_EXCL: never clobber a key that appeared concurrently.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("creds: create key file: %w", err)
	}
	if _, err := f.Write(key[:]); err != nil {
		f.Close()
		return nil, fmt.Errorf("creds: write key file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("creds: close key file: %w", err)
	}
	return key, nil
}

// Vault encrypts and decrypts credential secrets with NaCl secretbox.
type Vault struct {
	key [keySize]byte
}

// NewVault returns a Vault sealed with key.
func NewVault(key *[keySize]byte) *Vault {
	return &Vault{key: *key}
}

// Open loads (or creates) the key file under dataDir and returns the
// vault. This is the one-call constructor labd and labctl use.
func Open(dataDir string) (*Vault, error) {
	key, err := LoadOrCreateKey(dataDir)
	if err != nil {
		return nil, err
	}
	return NewVault(key), nil
}

// Encrypt seals a secret: a random 24-byte nonce prefix followed by
// the secretbox ciphertext. The result goes into
// lab.credentials.secret_enc.
func (v *Vault) Encrypt(secret string) ([]byte, error) {
	var nonce [nonceSize]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("creds: generate nonce: %w", err)
	}
	return secretbox.Seal(nonce[:], []byte(secret), &nonce, &v.key), nil
}

// Decrypt opens a secret sealed by Encrypt. It fails on truncated
// input, a wrong key, or tampered ciphertext — indistinguishably, by
// secretbox design.
func (v *Vault) Decrypt(box []byte) (string, error) {
	if len(box) < nonceSize+secretbox.Overhead {
		return "", fmt.Errorf("creds: ciphertext too short (%d bytes)", len(box))
	}
	var nonce [nonceSize]byte
	copy(nonce[:], box[:nonceSize])
	plain, ok := secretbox.Open(nil, box[nonceSize:], &nonce, &v.key)
	if !ok {
		return "", errors.New("creds: decryption failed (wrong key or corrupt ciphertext)")
	}
	return string(plain), nil
}
