package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// TestRoleMigrationBackfill seeds pre-migration users (no role column) and
// applies 061_user_roles.sql directly, asserting the earliest-created real
// user is backfilled admin and everyone else member, and that existing rows
// are backfilled verified.
func TestRoleMigrationBackfill(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "backfill.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// Minimal pre-migration users table (001_init shape).
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE users (
			id            TEXT PRIMARY KEY,
			email         TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			created_at    TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at    TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`); err != nil {
		t.Fatal(err)
	}
	// Insert out of order; founder is the earliest created_at.
	rows := []struct{ id, email, created string }{
		{"u-late", "late@x", "2023-06-01T00:00:00Z"},
		{"u-founder", "founder@x", "2023-01-01T00:00:00Z"},
		{"u-mid", "mid@x", "2023-03-01T00:00:00Z"},
		// A system row created AFTER the humans (as on a real upgrade) must
		// NOT steal admin.
		{"_instance", "instance@system.clawvisor.invalid", "2024-01-01T00:00:00Z"},
	}
	for _, r := range rows {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO users (id,email,password_hash,created_at,updated_at) VALUES (?,?,?,?,?)`,
			r.id, r.email, "h", r.created, r.created); err != nil {
			t.Fatal(err)
		}
	}

	mig, err := os.ReadFile("migrations/061_user_roles.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := db.ExecContext(ctx, string(mig)); err != nil {
		t.Fatalf("apply 061: %v", err)
	}

	roleOf := func(id string) string {
		var role string
		if err := db.QueryRowContext(ctx, `SELECT role FROM users WHERE id=?`, id).Scan(&role); err != nil {
			t.Fatal(err)
		}
		return role
	}
	if got := roleOf("u-founder"); got != "admin" {
		t.Fatalf("founder role=%q want admin", got)
	}
	for _, id := range []string{"u-late", "u-mid", "_instance"} {
		if got := roleOf(id); got != "member" {
			t.Fatalf("%s role=%q want member", id, got)
		}
	}
	// Every existing row is backfilled verified.
	var nullVerified int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE verified_at IS NULL`).Scan(&nullVerified); err != nil {
		t.Fatal(err)
	}
	if nullVerified != 0 {
		t.Fatalf("expected all existing rows verified, %d still NULL", nullVerified)
	}
}
