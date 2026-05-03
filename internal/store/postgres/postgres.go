package postgres

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// New creates a pgxpool connection pool and runs pending migrations.
func New(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("opening postgres pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}
	if err := runMigrations(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}
	return pool, nil
}

func runMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	return runMigrationsFS(ctx, pool, migrationsFS)
}

// runMigrationsFS applies every unapplied .sql file under migrations/ in
// lexicographic order. Each file runs inside its own transaction together
// with the matching schema_migrations row insert, so a partial failure
// rolls back the entire file.
//
// Convention for new migration files:
//   - Do NOT include BEGIN/COMMIT — the runner already wraps each file in a
//     transaction and an explicit BEGIN inside one will fail.
//   - Do NOT use statements that cannot run inside a transaction
//     (e.g. CREATE INDEX CONCURRENTLY, ALTER SYSTEM). If you need one,
//     split it into its own file and wrap it in a way the runner can detect
//     — at present the runner has no escape hatch, so prefer the regular
//     non-CONCURRENTLY index for now.
//   - File names should be NNN_name.sql with NNN strictly increasing; the
//     name is used as the schema_migrations primary key, so renaming an
//     already-applied file will reapply it.
func runMigrationsFS(ctx context.Context, pool *pgxpool.Pool, migrations fs.FS) error {
	// Create migrations tracking table
	_, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`)
	if err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}

	// Read applied migrations
	rows, err := pool.Query(ctx, `SELECT name FROM schema_migrations ORDER BY name`)
	if err != nil {
		return err
	}
	applied := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		applied[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// Read migration files
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		if applied[entry.Name()] {
			continue
		}

		data, err := fs.ReadFile(migrations, "migrations/"+entry.Name())
		if err != nil {
			return fmt.Errorf("reading migration %s: %w", entry.Name(), err)
		}

		tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", entry.Name(), err)
		}
		if _, err := tx.Exec(ctx, string(data)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("applying migration %s: %w", entry.Name(), err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (name) VALUES ($1)`,
			entry.Name(),
		); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("recording migration %s: %w", entry.Name(), err)
		}
		if err := tx.Commit(ctx); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("committing migration %s: %w", entry.Name(), err)
		}
	}
	return nil
}
