package postgres

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Throwaway: exercises the real runner against a live PG15.
func TestTmpConcurrentMigrations(t *testing.T) {
	dsn := os.Getenv("TMP_PG_DSN")
	if dsn == "" {
		t.Skip("no TMP_PG_DSN")
	}
	ctx := context.Background()

	const racers = 4
	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := 0; i < racers; i++ {
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
			t.Errorf("racer %d failed: %v", i, err)
		}
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	var applied int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	t.Logf("migrations applied: %d", applied)

	// actor_email must exist and be metadata-only (no backfill at startup).
	var col int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name='audit_log' AND column_name='actor_email'`).Scan(&col); err != nil {
		t.Fatal(err)
	}
	if col != 1 {
		t.Errorf("actor_email column missing")
	}

	// idx_audit_time must NOT be built by startup migrations.
	var idx int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname='idx_audit_time'`).Scan(&idx); err != nil {
		t.Fatal(err)
	}
	if idx != 0 {
		t.Errorf("idx_audit_time was built during startup migrations; should be deferred")
	}
}
