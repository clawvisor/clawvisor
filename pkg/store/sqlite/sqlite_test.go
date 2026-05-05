package sqlite

import (
	"context"
	"database/sql"
	"testing"
	"testing/fstest"
)

func TestRunMigrationsFSIsTransactional(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", t.TempDir()+"/migrations.db")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	migrations := fstest.MapFS{
		"migrations/001_broken.sql": &fstest.MapFile{
			Data: []byte(`
				CREATE TABLE atomic_test (id INTEGER PRIMARY KEY);
				INSERT INTO missing_table(id) VALUES (1);
			`),
		},
	}
	if err := runMigrationsFS(ctx, db, migrations); err == nil {
		t.Fatal("expected migration failure")
	}

	var tableCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'atomic_test'`).Scan(&tableCount); err != nil {
		t.Fatalf("QueryRowContext(sqlite_master): %v", err)
	}
	if tableCount != 0 {
		t.Fatalf("expected failed migration table creation to roll back, count=%d", tableCount)
	}

	var migrationCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&migrationCount); err != nil {
		t.Fatalf("QueryRowContext(schema_migrations): %v", err)
	}
	if migrationCount != 0 {
		t.Fatalf("expected failed migration to remain unrecorded, count=%d", migrationCount)
	}
}
