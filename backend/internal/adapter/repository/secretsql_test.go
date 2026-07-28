package repository

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goodylili/mountabo/internal/usecase"
)

func testMasterKey(b byte) []byte {
	return bytes.Repeat([]byte{b}, 32)
}

func openSecretStore(t *testing.T, path string, masterKey []byte) *EncryptedSecretStore {
	t.Helper()
	db, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewEncryptedSecretStore(db, masterKey)
	if err != nil {
		t.Fatalf("NewEncryptedSecretStore: %v", err)
	}
	return store
}

func TestEncryptedSecretStore_RoundTrip(t *testing.T) {
	store := openSecretStore(t, filepath.Join(t.TempDir(), "s.db"), testMasterKey(0x01))
	seen := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	in := map[string]usecase.SecretValue{
		"DATABASE_URL": {Value: "postgres://secret", GitHubUpdatedAtSeen: seen},
		"API_KEY":      {Value: "sk-123", GitHubUpdatedAtSeen: seen},
	}
	if err := store.SaveSecretValues("acme", "shop", "production", in); err != nil {
		t.Fatalf("SaveSecretValues: %v", err)
	}

	got, err := store.LoadSecretValues("acme", "shop", "production")
	if err != nil {
		t.Fatalf("LoadSecretValues: %v", err)
	}
	if len(got) != 2 || got["DATABASE_URL"].Value != "postgres://secret" || got["API_KEY"].Value != "sk-123" {
		t.Fatalf("round-trip values wrong: %+v", got)
	}
	if !got["DATABASE_URL"].GitHubUpdatedAtSeen.Equal(seen) {
		t.Errorf("updatedAtSeen = %v, want %v", got["DATABASE_URL"].GitHubUpdatedAtSeen, seen)
	}

	// A save merges rather than replaces.
	if err := store.SaveSecretValues("acme", "shop", "production", map[string]usecase.SecretValue{
		"API_KEY": {Value: "sk-rotated", GitHubUpdatedAtSeen: seen},
	}); err != nil {
		t.Fatalf("merge SaveSecretValues: %v", err)
	}
	got, _ = store.LoadSecretValues("acme", "shop", "production")
	if got["DATABASE_URL"].Value != "postgres://secret" || got["API_KEY"].Value != "sk-rotated" {
		t.Fatalf("merge lost or mis-updated a value: %+v", got)
	}
}

func TestEncryptedSecretStore_DeleteAndClear(t *testing.T) {
	store := openSecretStore(t, filepath.Join(t.TempDir(), "s.db"), testMasterKey(0x01))
	_ = store.SaveSecretValues("acme", "shop", "production", map[string]usecase.SecretValue{
		"A": {Value: "1"}, "B": {Value: "2"},
	})
	if err := store.DeleteSecretValue("acme", "shop", "production", "A"); err != nil {
		t.Fatalf("DeleteSecretValue: %v", err)
	}
	got, _ := store.LoadSecretValues("acme", "shop", "production")
	if _, ok := got["A"]; ok || got["B"].Value != "2" {
		t.Fatalf("delete wrong: %+v", got)
	}
	// Deleting the last value drops the row; a clear on nothing is a no-op.
	if err := store.DeleteSecretValue("acme", "shop", "production", "B"); err != nil {
		t.Fatalf("DeleteSecretValue last: %v", err)
	}
	if err := store.ClearSecretValues("acme", "shop", "production"); err != nil {
		t.Fatalf("ClearSecretValues: %v", err)
	}
	got, _ = store.LoadSecretValues("acme", "shop", "production")
	if len(got) != 0 {
		t.Fatalf("want empty after clear, got %+v", got)
	}
}

// A DEK wrapped by one master key can't be unwrapped by another, so reopening the
// same DB with a different master key fails fast rather than corrupting data.
func TestEncryptedSecretStore_WrongMasterKeyFailsToOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	store := openSecretStore(t, path, testMasterKey(0x01))
	_ = store.SaveSecretValues("acme", "shop", "production", map[string]usecase.SecretValue{"A": {Value: "1"}})

	db, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := NewEncryptedSecretStore(db, testMasterKey(0x02)); err == nil {
		t.Fatal("opening with the wrong master key should fail")
	}
}

// The DEK survives a restart: the same master key unwraps the stored DEK and the
// values decrypt on a fresh store instance over the same DB.
func TestEncryptedSecretStore_SurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	first := openSecretStore(t, path, testMasterKey(0x01))
	_ = first.SaveSecretValues("acme", "shop", "production", map[string]usecase.SecretValue{"A": {Value: "keep-me"}})

	second := openSecretStore(t, path, testMasterKey(0x01))
	got, err := second.LoadSecretValues("acme", "shop", "production")
	if err != nil {
		t.Fatalf("LoadSecretValues after reopen: %v", err)
	}
	if got["A"].Value != "keep-me" {
		t.Fatalf("value lost across reopen: %+v", got)
	}
}

// A stored blob is bound to its (owner, repo, environment) by the AAD, so it can't
// be decrypted as if it belonged to a different environment.
func TestEncryptedSecretStore_AADBindsToEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	store := openSecretStore(t, path, testMasterKey(0x01))
	_ = store.SaveSecretValues("acme", "shop", "production", map[string]usecase.SecretValue{"A": {Value: "1"}})

	var nonce, ciphertext []byte
	if err := store.db.QueryRow(
		`SELECT nonce, ciphertext FROM env_secrets WHERE owner=? AND repo=? AND environment=?`,
		"acme", "shop", "production").Scan(&nonce, &ciphertext); err != nil {
		t.Fatalf("read row: %v", err)
	}
	// Same DEK, correct AAD decrypts; a different environment's AAD does not.
	if _, err := open(store.dek, nonce, ciphertext, envAAD("acme", "shop", "production")); err != nil {
		t.Fatalf("correct AAD should decrypt: %v", err)
	}
	if _, err := open(store.dek, nonce, ciphertext, envAAD("acme", "shop", "staging")); err == nil {
		t.Fatal("a different environment's AAD must not decrypt")
	}
}

// After a single insert into a fresh DB, the plaintext value must appear nowhere:
// not in the ciphertext column, not in secret_meta, and not in the raw db file.
func TestEncryptedSecretStore_NoPlaintextAtRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	db, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	store, err := NewEncryptedSecretStore(db, testMasterKey(0x01))
	if err != nil {
		t.Fatalf("NewEncryptedSecretStore: %v", err)
	}
	secret := "UNIQUE-PLAINTEXT-DO-NOT-LEAK-9f3a"
	if err := store.SaveSecretValues("acme", "shop", "production", map[string]usecase.SecretValue{
		"LEAKY": {Value: secret},
	}); err != nil {
		t.Fatalf("SaveSecretValues: %v", err)
	}

	var ciphertext, metaCipher []byte
	if err := db.QueryRow(`SELECT ciphertext FROM env_secrets LIMIT 1`).Scan(&ciphertext); err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	if err := db.QueryRow(`SELECT ciphertext FROM secret_meta LIMIT 1`).Scan(&metaCipher); err != nil {
		t.Fatalf("read secret_meta: %v", err)
	}
	if bytes.Contains(ciphertext, []byte(secret)) {
		t.Error("plaintext leaked into env_secrets.ciphertext")
	}
	if bytes.Contains(metaCipher, []byte(secret)) {
		t.Error("plaintext leaked into secret_meta")
	}
	// Close so the rollback journal is finalized into the main file, then scan the
	// raw bytes. Default journal mode keeps data in the main .db (no WAL sidecar).
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // path is a test-owned temp file
	if err != nil {
		t.Fatalf("read db file: %v", err)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Error("plaintext leaked into the raw database file")
	}
}

func TestLoadMasterKey_PrefersEnvOverFile(t *testing.T) {
	dir := t.TempDir()
	envKey := testMasterKey(0x07)
	t.Setenv("MOUNTABO_SECRET_KEY", base64.StdEncoding.EncodeToString(envKey))
	got, err := LoadMasterKey(dir)
	if err != nil {
		t.Fatalf("LoadMasterKey: %v", err)
	}
	if !bytes.Equal(got, envKey) {
		t.Fatal("LoadMasterKey should return the env key")
	}
	// The env key is authoritative: no key file is written as a side effect.
	if _, err := os.Stat(filepath.Join(dir, "secret.key")); !os.IsNotExist(err) {
		t.Error("no key file should be created when MOUNTABO_SECRET_KEY is set")
	}
}

func TestLoadMasterKey_RejectsInvalidEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MOUNTABO_SECRET_KEY", "not-base64!!!")
	if _, err := LoadMasterKey(dir); err == nil {
		t.Fatal("invalid base64 MOUNTABO_SECRET_KEY should fail")
	}
	t.Setenv("MOUNTABO_SECRET_KEY", base64.StdEncoding.EncodeToString([]byte("too-short")))
	if _, err := LoadMasterKey(dir); err == nil {
		t.Fatal("a non-32-byte MOUNTABO_SECRET_KEY should fail")
	}
}

func TestLoadMasterKey_GeneratesStableFileKey(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadMasterKey(dir)
	if err != nil {
		t.Fatalf("first LoadMasterKey: %v", err)
	}
	path := filepath.Join(dir, "secret.key")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("key file not created: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("key file perms = %v, want 0600", info.Mode().Perm())
	}
	second, err := LoadMasterKey(dir)
	if err != nil {
		t.Fatalf("second LoadMasterKey: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("the generated key file should be stable across loads")
	}
}
