package repository

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
)

// testDB returns a freshly migrated database isolated in its own schema, dropped
// when the test ends. Point MOUNTABO_TEST_DATABASE_URL at any Postgres you can
// create schemas in; without it, database tests are skipped.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	base := os.Getenv("MOUNTABO_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("MOUNTABO_TEST_DATABASE_URL not set; skipping Postgres test")
	}
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	schema := "test_" + hex.EncodeToString(suffix)

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_ = admin.Close()
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse MOUNTABO_TEST_DATABASE_URL: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	db, err := OpenPostgres(context.Background(), u.String())
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
