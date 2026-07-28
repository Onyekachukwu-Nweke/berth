package repository

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/goodylili/mountabo/internal/usecase"
)

// EncryptedSecretStore persists the revealable env-secret values mountabo set,
// encrypted at rest in SQLite (so it works headless, unlike the OS keychain).
// Envelope encryption: a random data-encryption key (DEK) encrypts each
// environment's value blob; the DEK itself is wrapped by a master key (from the
// env or a key file) and stored in secret_meta. GitHub stays authoritative for
// which secret names are configured; this store owns the values it created.
type EncryptedSecretStore struct {
	db  *sql.DB
	dek []byte // 32-byte data-encryption key, unwrapped in memory
}

var _ usecase.SecretStore = (*EncryptedSecretStore)(nil)

const (
	secretSchema = `
CREATE TABLE IF NOT EXISTS env_secrets (
	owner TEXT NOT NULL,
	repo TEXT NOT NULL,
	environment TEXT NOT NULL,
	nonce BLOB NOT NULL,
	ciphertext BLOB NOT NULL,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (owner, repo, environment)
);
CREATE TABLE IF NOT EXISTS secret_meta (
	key TEXT PRIMARY KEY,
	version INTEGER NOT NULL,
	nonce BLOB NOT NULL,
	ciphertext BLOB NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);`
	dekMetaKey = "dek:v1"
	dekAAD     = "secret-meta:dek:v1" // binds the wrapped DEK to its slot
	masterEnv  = "MOUNTABO_SECRET_KEY"
	keyFile    = "secret.key"
)

// storedValue is the per-secret JSON persisted inside an environment's encrypted
// blob: the revealable value and the GitHub updated-at mountabo observed when it
// last synced (for drift detection).
type storedValue struct {
	Value               string    `json:"value"`
	GitHubUpdatedAtSeen time.Time `json:"githubUpdatedAtSeen"`
}

// LoadMasterKey resolves the 32-byte master key that wraps the DEK, failing fast
// rather than running half-configured: MOUNTABO_SECRET_KEY (base64 of exactly 32
// bytes; invalid → error, no silent fallback), else a 0600 key file under
// dataDir, generated on first run. A key file only protects a stolen DB copy /
// other OS users — a full data-dir snapshot has both key and ciphertext, so use
// MOUNTABO_SECRET_KEY (KMS/Vault-injectable) for headless/hosted.
func LoadMasterKey(dataDir string) ([]byte, error) {
	if enc := os.Getenv(masterEnv); enc != "" {
		key, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			return nil, fmt.Errorf("%s must be valid base64: %w", masterEnv, err)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("%s must decode to exactly 32 bytes, got %d", masterEnv, len(key))
		}
		return key, nil
	}

	path := filepath.Join(dataDir, keyFile)
	key, err := os.ReadFile(path) //nolint:gosec // path is the configured data dir joined with a fixed key-file name, not user input
	if err == nil {
		if len(key) != 32 {
			return nil, fmt.Errorf("key file %s is corrupt (want 32 bytes, got %d)", path, len(key))
		}
		// Ensure restrictive permissions even if the file predates this check.
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("secure key file %s: %w", path, err)
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read key file %s: %w", path, err)
	}
	// First run: generate and persist a new master key with 0600 permissions.
	key = make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("write key file %s: %w", path, err)
	}
	return key, nil
}

// NewEncryptedSecretStore applies the secrets schema and loads (or, on first
// run, creates) the DEK, unwrapping it with masterKey. A stored DEK that
// masterKey cannot unwrap is a fatal, explicit error rather than a silent
// re-key: the existing env-secret values would be undecryptable.
func NewEncryptedSecretStore(db *sql.DB, masterKey []byte) (*EncryptedSecretStore, error) {
	if _, err := db.Exec(secretSchema); err != nil {
		return nil, fmt.Errorf("apply secrets schema: %w", err)
	}
	dek, err := loadOrCreateDEK(db, masterKey)
	if err != nil {
		return nil, err
	}
	return &EncryptedSecretStore{db: db, dek: dek}, nil
}

// loadOrCreateDEK returns the unwrapped DEK, generating and wrapping a new one on
// first run.
func loadOrCreateDEK(db *sql.DB, masterKey []byte) ([]byte, error) {
	var nonce, ciphertext []byte
	err := db.QueryRow(`SELECT nonce, ciphertext FROM secret_meta WHERE key = ?`, dekMetaKey).Scan(&nonce, &ciphertext)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		dek := make([]byte, 32)
		if _, rerr := io.ReadFull(rand.Reader, dek); rerr != nil {
			return nil, fmt.Errorf("generate DEK: %w", rerr)
		}
		n, ct, serr := seal(masterKey, dek, []byte(dekAAD))
		if serr != nil {
			return nil, fmt.Errorf("wrap DEK: %w", serr)
		}
		now := time.Now().UTC().Format(time.RFC3339)
		if _, ierr := db.Exec(`INSERT INTO secret_meta (key, version, nonce, ciphertext, created_at, updated_at)
			VALUES (?, 1, ?, ?, ?, ?)`, dekMetaKey, n, ct, now, now); ierr != nil {
			return nil, fmt.Errorf("store wrapped DEK: %w", ierr)
		}
		return dek, nil
	case err != nil:
		return nil, fmt.Errorf("read wrapped DEK: %w", err)
	}
	dek, oerr := open(masterKey, nonce, ciphertext, []byte(dekAAD))
	if oerr != nil {
		return nil, fmt.Errorf("secret key mismatch or lost — stored env secrets cannot be decrypted with the current master key: %w", oerr)
	}
	return dek, nil
}

// envAAD binds an environment's ciphertext to its identity, so a blob can't be
// swapped into a different environment and still decrypt.
func envAAD(owner, repo, environment string) []byte {
	return []byte(owner + "\x00" + repo + "\x00" + environment)
}

// SaveSecretValues merges values into the environment's encrypted record.
func (s *EncryptedSecretStore) SaveSecretValues(owner, repo, environment string, values map[string]usecase.SecretValue) error {
	if len(values) == 0 {
		return nil
	}
	record, err := s.load(owner, repo, environment)
	if err != nil {
		return err
	}
	for name, v := range values {
		record[name] = storedValue{Value: v.Value, GitHubUpdatedAtSeen: v.GitHubUpdatedAtSeen}
	}
	return s.store(owner, repo, environment, record)
}

// LoadSecretValues returns the environment's stored values (empty when none).
func (s *EncryptedSecretStore) LoadSecretValues(owner, repo, environment string) (map[string]usecase.SecretValue, error) {
	record, err := s.load(owner, repo, environment)
	if err != nil {
		return nil, err
	}
	out := make(map[string]usecase.SecretValue, len(record))
	for name, v := range record {
		out[name] = usecase.SecretValue{Value: v.Value, GitHubUpdatedAtSeen: v.GitHubUpdatedAtSeen}
	}
	return out, nil
}

// DeleteSecretValue removes one value from the environment's record, deleting the
// row when it becomes empty.
func (s *EncryptedSecretStore) DeleteSecretValue(owner, repo, environment, name string) error {
	record, err := s.load(owner, repo, environment)
	if err != nil {
		return err
	}
	if _, ok := record[name]; !ok {
		return nil
	}
	delete(record, name)
	if len(record) == 0 {
		return s.ClearSecretValues(owner, repo, environment)
	}
	return s.store(owner, repo, environment, record)
}

// ClearSecretValues removes the environment's whole encrypted record (teardown).
func (s *EncryptedSecretStore) ClearSecretValues(owner, repo, environment string) error {
	if _, err := s.db.Exec(`DELETE FROM env_secrets WHERE owner = ? AND repo = ? AND environment = ?`,
		owner, repo, environment); err != nil {
		return fmt.Errorf("clear env secrets: %w", err)
	}
	return nil
}

// load decrypts and unmarshals the environment's record (empty when no row).
func (s *EncryptedSecretStore) load(owner, repo, environment string) (map[string]storedValue, error) {
	var nonce, ciphertext []byte
	err := s.db.QueryRow(`SELECT nonce, ciphertext FROM env_secrets WHERE owner = ? AND repo = ? AND environment = ?`,
		owner, repo, environment).Scan(&nonce, &ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]storedValue{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read env secrets: %w", err)
	}
	plaintext, err := open(s.dek, nonce, ciphertext, envAAD(owner, repo, environment))
	if err != nil {
		return nil, fmt.Errorf("decrypt env secrets: %w", err)
	}
	var record map[string]storedValue
	if err := json.Unmarshal(plaintext, &record); err != nil {
		return nil, fmt.Errorf("parse env secrets: %w", err)
	}
	return record, nil
}

// store marshals, encrypts and upserts the environment's record.
func (s *EncryptedSecretStore) store(owner, repo, environment string, record map[string]storedValue) error {
	plaintext, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode env secrets: %w", err)
	}
	nonce, ciphertext, err := seal(s.dek, plaintext, envAAD(owner, repo, environment))
	if err != nil {
		return fmt.Errorf("encrypt env secrets: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(`INSERT INTO env_secrets (owner, repo, environment, nonce, ciphertext, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(owner, repo, environment) DO UPDATE SET
			nonce = excluded.nonce, ciphertext = excluded.ciphertext, updated_at = excluded.updated_at`,
		owner, repo, environment, nonce, ciphertext, now); err != nil {
		return fmt.Errorf("write env secrets: %w", err)
	}
	return nil
}

// seal encrypts plaintext with key (AES-256-GCM) under a fresh random nonce and
// the given AAD, returning the nonce and ciphertext separately.
func seal(key, plaintext, aad []byte) (nonce, ciphertext []byte, err error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("generate nonce: %w", err)
	}
	return nonce, gcm.Seal(nil, nonce, plaintext, aad), nil
}

// open decrypts ciphertext with key, verifying the nonce + AAD.
func open(key, nonce, ciphertext, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, aad)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	return gcm, nil
}
