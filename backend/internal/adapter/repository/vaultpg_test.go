package repository

import (
	"bytes"
	"errors"
	"testing"
)

func testVault(t *testing.T) *VaultSQL {
	t.Helper()
	v, err := NewVaultSQL(testDB(t), bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("NewVaultSQL: %v", err)
	}
	return v
}

func TestNewVaultSQL_RejectsAShortKey(t *testing.T) {
	if _, err := NewVaultSQL(nil, []byte("too-short")); err == nil {
		t.Fatal("a 9-byte master key was accepted")
	}
}

func TestVaultSQL_SealsAndRoundTrips(t *testing.T) {
	v := testVault(t)
	const plain = "-----BEGIN OPENSSH PRIVATE KEY-----\nsecret\n"
	if err := v.SaveSecret("server/s1/key", plain); err != nil {
		t.Fatalf("SaveSecret: %v", err)
	}
	got, err := v.LoadSecret("server/s1/key")
	if err != nil || got != plain {
		t.Fatalf("LoadSecret = %q, %v", got, err)
	}

	// The database must never hold the plaintext.
	var sealed []byte
	if err := v.db.QueryRow(`SELECT sealed FROM vault_secrets WHERE key = 'server/s1/key'`).Scan(&sealed); err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if bytes.Contains(sealed, []byte("secret")) {
		t.Error("plaintext found in the stored value")
	}

	if _, err := v.LoadSecret("missing"); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("missing key err = %v, want ErrSecretNotFound", err)
	}
	if err := v.DeleteSecret("server/s1/key"); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	if err := v.DeleteSecret("server/s1/key"); err != nil {
		t.Fatalf("DeleteSecret twice should be a no-op: %v", err)
	}
}

// A sealed value moved onto another key's row must not decrypt: the key is
// bound in as additional authenticated data.
func TestVaultSQL_RefusesASwappedValue(t *testing.T) {
	v := testVault(t)
	if err := v.SaveSecret("a", "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.db.Exec(`INSERT INTO vault_secrets (key, sealed) SELECT 'b', sealed FROM vault_secrets WHERE key = 'a'`); err != nil {
		t.Fatal(err)
	}
	if got, err := v.LoadSecret("b"); err == nil {
		t.Fatalf("swapped value decrypted as %q", got)
	}
}

// A different master key cannot read what the first one sealed.
func TestVaultSQL_WrongMasterKeyFails(t *testing.T) {
	v := testVault(t)
	if err := v.SaveSecret("k", "v"); err != nil {
		t.Fatal(err)
	}
	other, err := NewVaultSQL(v.db, bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.LoadSecret("k"); err == nil {
		t.Fatal("a different master key decrypted the secret")
	}
}
