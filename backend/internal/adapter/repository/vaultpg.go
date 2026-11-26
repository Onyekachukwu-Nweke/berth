package repository

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Onyekachukwu-Nweke/berth/internal/usecase"
)

// ErrSecretNotFound is returned by LoadSecret when no value is stored under key.
var ErrSecretNotFound = errors.New("secret not found")

// VaultSQL stores the platform's own credentials in Postgres, sealed with
// AES-256-GCM under a master key that never touches the database. Each value is
// bound to its key as additional authenticated data, so a sealed blob copied
// onto another row fails to open instead of being served as that row's secret.
// It satisfies usecase.SecretVault.
type VaultSQL struct {
	db   *sql.DB
	aead cipher.AEAD
}

var _ usecase.SecretVault = (*VaultSQL)(nil)

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
		return "", fmt.Errorf("secret %q: cannot decrypt (wrong BERTH_MASTER_KEY?)", key)
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
