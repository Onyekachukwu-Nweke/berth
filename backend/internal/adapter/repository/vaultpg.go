package repository

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/goodylili/mountabo/internal/usecase"
)

// githubTokenKey is the vault entry holding the GitHub connection token.
const githubTokenKey = "github-token"

// ErrSecretNotFound is returned by LoadSecret when no value is stored under key.
var ErrSecretNotFound = errors.New("secret not found")

// VaultSQL stores the platform's own credentials in Postgres, sealed with
// AES-256-GCM under a master key that never touches the database. Each value is
// bound to its key as additional authenticated data, so a sealed blob copied
// onto another row fails to open instead of being served as that row's secret.
// It satisfies usecase.SecretVault and usecase.TokenStore.
type VaultSQL struct {
	db   *sql.DB
	aead cipher.AEAD
}

var (
	_ usecase.SecretVault = (*VaultSQL)(nil)
	_ usecase.TokenStore  = (*VaultSQL)(nil)
)

// NewVaultSQL returns a vault over db sealing with masterKey, which must be
// exactly 32 bytes (AES-256).
func NewVaultSQL(db *sql.DB, masterKey []byte) (*VaultSQL, error) {
	if len(masterKey) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes, got %d", len(masterKey))
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("init cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("init gcm: %w", err)
	}
	return &VaultSQL{db: db, aead: aead}, nil
}

func (v *VaultSQL) seal(key, value string) ([]byte, error) {
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	return v.aead.Seal(nonce, nonce, []byte(value), []byte(key)), nil
}

func (v *VaultSQL) open(key string, sealed []byte) (string, error) {
	n := v.aead.NonceSize()
	if len(sealed) < n {
		return "", fmt.Errorf("secret %q: sealed value is truncated", key)
	}
	plain, err := v.aead.Open(nil, sealed[:n], sealed[n:], []byte(key))
	if err != nil {
		return "", fmt.Errorf("secret %q: cannot decrypt (wrong MOUNTABO_MASTER_KEY?)", key)
	}
	return string(plain), nil
}

// SaveSecret seals value and stores it under key, replacing any existing value.
func (v *VaultSQL) SaveSecret(key, value string) error {
	sealed, err := v.seal(key, value)
	if err != nil {
		return err
	}
	if _, err := v.db.Exec(`INSERT INTO vault_secrets (key, sealed) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET sealed = excluded.sealed, updated_at = now()`, key, sealed); err != nil {
		return fmt.Errorf("save secret: %w", err)
	}
	return nil
}

// LoadSecret returns the value stored under key, or an error wrapping
// ErrSecretNotFound when there is none.
func (v *VaultSQL) LoadSecret(key string) (string, error) {
	var sealed []byte
	err := v.db.QueryRow(`SELECT sealed FROM vault_secrets WHERE key = $1`, key).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ErrSecretNotFound, key)
	}
	if err != nil {
		return "", fmt.Errorf("load secret: %w", err)
	}
	return v.open(key, sealed)
}

// DeleteSecret removes the value under key. It is idempotent.
func (v *VaultSQL) DeleteSecret(key string) error {
	if _, err := v.db.Exec(`DELETE FROM vault_secrets WHERE key = $1`, key); err != nil {
		return fmt.Errorf("delete secret: %w", err)
	}
	return nil
}

// Save stores the GitHub connection token.
func (v *VaultSQL) Save(t usecase.Token) error {
	blob, err := json.Marshal(t) //nolint:gosec // marshaled only to be sealed into the vault; never logged
	if err != nil {
		return fmt.Errorf("marshal token: %w", err)
	}
	return v.SaveSecret(githubTokenKey, string(blob))
}

// Load returns the GitHub connection token, or usecase.ErrNotConnected when none
// has been stored.
func (v *VaultSQL) Load() (usecase.Token, error) {
	blob, err := v.LoadSecret(githubTokenKey)
	if errors.Is(err, ErrSecretNotFound) {
		return usecase.Token{}, usecase.ErrNotConnected
	}
	if err != nil {
		return usecase.Token{}, err
	}
	var t usecase.Token
	if err := json.Unmarshal([]byte(blob), &t); err != nil {
		return usecase.Token{}, fmt.Errorf("unmarshal token: %w", err)
	}
	return t, nil
}

// Delete removes the GitHub connection token. It is idempotent.
func (v *VaultSQL) Delete() error {
	return v.DeleteSecret(githubTokenKey)
}
