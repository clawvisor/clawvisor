package postgres

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigrationsAgainstLivePostgres exercises the real startup runner against a
// live server. The unit guards in migration_safety_test.go only inspect SQL
// text; this covers the behaviour that text cannot prove -- that concurrent
// servers do not race the same DDL, and that migration 062 lands as a
// metadata-only change rather than a table rewrite.
//
// Opt in by pointing CLAWVISOR_TEST_POSTGRES_DSN at a scratch database:
//
//	docker run -d --name pg -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=cv -p 5432:5432 postgres:15
//	CLAWVISOR_TEST_POSTGRES_DSN='postgres://postgres:pw@localhost:5432/cv?sslmode=disable' \
//	    go test ./pkg/store/postgres/ -run TestMigrationsAgainstLivePostgres
//
// The target database must be empty; the test applies every migration to it.
func TestMigrationsAgainstLivePostgres(t *testing.T) {
	dsn := os.Getenv("CLAWVISOR_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set CLAWVISOR_TEST_POSTGRES_DSN to run the live-Postgres migration exercise")
	}
	ctx := context.Background()

	// Cloud starts multiple services against one database. Without the
	// advisory lock these racers take the same schema_migrations snapshot and
	// collide on identical DDL, which surfaces in production as a boot
	// crashloop because a failed migration exits the process.
	const racers = 4
	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := range racers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			pool, err := pgxpool.New(ctx, dsn)
			if err != nil {
				errs[n] = err
				return
			}
			defer pool.Close()
			errs[n] = runMigrations(ctx, pool)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent migration run %d failed: %v", i, err)
		}
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	t.Run("062 adds actor_email as a metadata-only column", func(t *testing.T) {
		var nullable, def string
		err := pool.QueryRow(ctx, `
			SELECT is_nullable, COALESCE(column_default, '')
			FROM information_schema.columns
			WHERE table_name = 'audit_log' AND column_name = 'actor_email'
		`).Scan(&nullable, &def)
		if err != nil {
			t.Fatalf("actor_email column missing: %v", err)
		}
		if nullable != "NO" {
			t.Errorf("actor_email is_nullable = %q, want NO", nullable)
		}
		if def == "" {
			t.Error("actor_email has no default; ADD COLUMN would rewrite the table on PG<11 semantics")
		}
	})

	t.Run("audit_log.user_id survives user deletion", func(t *testing.T) {
		var nullable string
		if err := pool.QueryRow(ctx, `
			SELECT is_nullable FROM information_schema.columns
			WHERE table_name = 'audit_log' AND column_name = 'user_id'
		`).Scan(&nullable); err != nil {
			t.Fatalf("query user_id: %v", err)
		}
		if nullable != "YES" {
			t.Errorf("user_id is_nullable = %q, want YES so ON DELETE SET NULL can retain history", nullable)
		}
	})

	t.Run("replacement FK is installed NOT VALID", func(t *testing.T) {
		var validated bool
		if err := pool.QueryRow(ctx, `
			SELECT convalidated FROM pg_constraint WHERE conname = 'audit_log_user_id_fkey'
		`).Scan(&validated); err != nil {
			t.Fatalf("query constraint: %v", err)
		}
		if validated {
			t.Error("audit_log_user_id_fkey was validated at startup; that scans the whole table " +
				"under a DDL lock. It must ship NOT VALID and be validated by the post-062 script.")
		}
	})

	t.Run("large audit index is deferred out of startup", func(t *testing.T) {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_audit_time'`).Scan(&n); err != nil {
			t.Fatalf("query indexes: %v", err)
		}
		if n != 0 {
			t.Error("idx_audit_time was built by a startup migration; it must be created " +
				"CONCURRENTLY by scripts/postgres/post-062-backfill.sql")
		}
	})
}
