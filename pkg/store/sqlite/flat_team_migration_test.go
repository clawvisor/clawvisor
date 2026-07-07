package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
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

// TestActorRetainMigrationPreservesChainFacts guards the 063 wrinkle: the
// audit_log recreate DROPs the old table, and BOTH llm_request_cost and
// chain_facts reference audit_log(id) ON DELETE CASCADE. With foreign_keys
// ON that DROP would cascade and wipe chain_facts unless 063 snapshots and
// restores it (mirroring migration 044). This applies every migration up to
// 062, seeds a chain_facts + cost row, then applies 063 and asserts both
// survive.
func TestActorRetainMigrationPreservesChainFacts(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "retain.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}

	// Read every migration from disk, applying all of them BEFORE 063 so we
	// reach the exact pre-063 schema, then keep 063 aside to apply after
	// seeding.
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	pre063 := fstest.MapFS{}
	const target = "063_audit_actor_retain.sql"
	var mig063 []byte
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		data, err := os.ReadFile(filepath.Join("migrations", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if e.Name() >= target {
			if e.Name() == target {
				mig063 = data
			}
			continue
		}
		pre063["migrations/"+e.Name()] = &fstest.MapFile{Data: data}
	}
	if mig063 == nil {
		t.Fatalf("missing %s", target)
	}
	if err := runMigrationsFS(ctx, db, pre063); err != nil {
		t.Fatalf("apply pre-063 migrations: %v", err)
	}

	// Seed a full user→agent→task→audit_log→chain_facts chain plus a cost
	// row, all satisfying their FKs.
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	exec(`INSERT INTO users (id,email,password_hash) VALUES ('u1','u1@x','h')`)
	exec(`INSERT INTO agents (id,user_id,name,token_hash) VALUES ('a1','u1','agent','tok')`)
	exec(`INSERT INTO tasks (id,user_id,agent_id,purpose) VALUES ('t1','u1','a1','p')`)
	exec(`INSERT INTO audit_log (id,user_id,request_id,service,action,decision,outcome)
	      VALUES ('au1','u1','req1','gmail','send','allow','ok')`)
	exec(`INSERT INTO chain_facts (id,task_id,session_id,audit_id,service,action,fact_type,fact_value)
	      VALUES ('cf1','t1','s1','au1','gmail','send','recipient','a@b.c')`)
	exec(`INSERT INTO llm_request_cost (audit_id,user_id,request_id,timestamp,provider,model)
	      VALUES ('au1','u1','req1','2026-01-01T00:00:00Z','anthropic','claude')`)

	// Apply 063 through the same runner (records it in schema_migrations).
	only063 := fstest.MapFS{"migrations/" + target: &fstest.MapFile{Data: mig063}}
	if err := runMigrationsFS(ctx, db, only063); err != nil {
		t.Fatalf("apply 063: %v", err)
	}

	count := func(table string) int {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	if got := count("chain_facts"); got != 1 {
		t.Fatalf("chain_facts survived=%d, want 1 (063 DROP cascaded it away)", got)
	}
	if got := count("llm_request_cost"); got != 1 {
		t.Fatalf("llm_request_cost survived=%d, want 1", got)
	}
	// The restored chain_facts row still points at its audit row.
	var auditID string
	if err := db.QueryRowContext(ctx, `SELECT audit_id FROM chain_facts WHERE id='cf1'`).Scan(&auditID); err != nil {
		t.Fatalf("chain_facts row lost: %v", err)
	}
	if auditID != "au1" {
		t.Fatalf("chain_facts.audit_id=%q want au1", auditID)
	}
}
