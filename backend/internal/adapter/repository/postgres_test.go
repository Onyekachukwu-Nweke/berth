package repository

import (
	"context"
	"testing"
)

// Reopening an already-migrated database must apply nothing and fail nothing:
// every replica runs migrate on boot.
func TestMigrate_IsIdempotent(t *testing.T) {
	db := testDB(t)
	if err := migrate(context.Background(), db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	all, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != len(all) {
		t.Errorf("schema_migrations rows = %d, want %d", n, len(all))
	}
}

// A database migrated by a newer build is refused rather than run against a
// schema this build does not understand.
func TestMigrate_RefusesANewerDatabase(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec(`INSERT INTO schema_migrations (version, name) VALUES (9999, 'future')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := migrate(context.Background(), db); err == nil {
		t.Fatal("a newer database was accepted")
	}
}
