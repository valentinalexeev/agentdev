package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"
)

// The role api computes the schema version from images/api/schema.sql and grants.sql with the same formula
// (roles/api/defaults api_schema_version, tests/db_init.yml): sha256 of both files, first 16 hex characters (#382).
func TestSchemaVersionMatchesFiles(t *testing.T) {
	s, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	g, err := os.ReadFile("grants.sql")
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(append(s, g...))
	if want := hex.EncodeToString(h[:])[:16]; schemaVersion() != want {
		t.Fatalf("schemaVersion %s, files give %s", schemaVersion(), want)
	}
}

// TestPgWaitSchema needs TEST_DATABASE_URL as a superuser: the API waits for the version the db-init Job writes.
func TestPgWaitSchema(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pg, err := newPgStore(ctx, url, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.db.Exec(ctx, `DROP TABLE IF EXISTS schema_version`); err != nil {
		t.Fatal(err)
	}
	if err := waitSchema(ctx, pg.db, "v1", 100*time.Millisecond); err == nil {
		t.Fatal("no table: want an error")
	}
	if _, err := pg.db.Exec(ctx, `CREATE TABLE schema_version (id int PRIMARY KEY CHECK (id = 1), version text NOT NULL, applied timestamptz NOT NULL DEFAULT now());
		INSERT INTO schema_version (id, version) VALUES (1, 'old')`); err != nil {
		t.Fatal(err)
	}
	if err := waitSchema(ctx, pg.db, "v1", 100*time.Millisecond); err == nil {
		t.Fatal("old version: want an error")
	}
	go func() {
		time.Sleep(time.Second)
		pg.db.Exec(ctx, `UPDATE schema_version SET version = 'v1'`)
	}()
	if err := waitSchema(ctx, pg.db, "v1", 20*time.Second); err != nil {
		t.Fatal(err)
	}
	pg.db.Exec(ctx, `DROP TABLE schema_version`)
}
