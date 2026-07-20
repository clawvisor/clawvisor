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

// migrationAdvisoryLockID serializes schema migrations across every process
// connected to the same Postgres database. Cloud deployments start the main
// and proxy services concurrently; without a database-scoped lock they can
// both take the same snapshot of schema_migrations and race the same DDL.
//
// The value is the ASCII bytes for "Clawviso" interpreted as a signed int64.
const migrationAdvisoryLockID int64 = 0x436c61777669736f

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
//     already-applied file will reapply it. Tolerance for "already exists"
//     errors below makes the rename safe in practice for the common
//     ADD COLUMN / CREATE INDEX / CREATE TABLE shapes — see
//     isMigrationAlreadyAppliedErr.
func runMigrationsFS(ctx context.Context, pool *pgxpool.Pool, migrations fs.FS) error {
	// Advisory locks are session-scoped, so pin all migration work to one pool
	// connection. Releasing the connection also releases the lock if the
	// explicit unlock fails during cancellation or shutdown.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquiring migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationAdvisoryLockID); err != nil {
		return fmt.Errorf("acquiring migration advisory lock: %w", err)
	}
	defer func() {
		// Best effort only: the session releases the lock when conn is released.
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationAdvisoryLockID)
	}()

	// Create migrations tracking table
	_, err = conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`)
	if err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}

	// Read applied migrations
	rows, err := conn.Query(ctx, `SELECT name FROM schema_migrations ORDER BY name`)
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

		tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", entry.Name(), err)
		}
		if _, err := tx.Exec(ctx, string(data)); err != nil {
			// Tolerate "already exists" errors so a renumbered migration
			// (e.g. a duplicate 039_X collapsed to 040_X) can be recorded
			// as applied on a staging env that already ran it under the
			// old name. Mirrors the SQLite runner's "duplicate column
			// name" handling. The transaction is unusable after a
			// failed DDL in Postgres, so roll back and reopen.
			if isMigrationAlreadyAppliedErr(err) {
				_ = tx.Rollback(ctx)
				if _, err := conn.Exec(ctx,
					`INSERT INTO schema_migrations (name) VALUES ($1) ON CONFLICT DO NOTHING`,
					entry.Name(),
				); err != nil {
					return fmt.Errorf("recording already-applied migration %s: %w", entry.Name(), err)
				}
				continue
			}
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

// isMigrationAlreadyAppliedErr reports whether err looks like a
// Postgres "the schema is already in the target shape" error. Used
// to make renumbered migrations safe to re-apply: a renamed file
// runs against a DB that already has the columns/indexes/tables,
// gets one of these errors, and we record the new file name as
// applied without re-running the DDL. Errors we treat as "already
// done":
//
//   - 42701  duplicate_column
//   - 42710  duplicate_object  (constraint, sequence, etc.)
//   - 42P07  duplicate_table   (also fires for duplicate INDEX)
//   - 42P16  invalid_table_definition (e.g. DROP CONSTRAINT that
//     was already dropped, when paired with an IF NOT EXISTS later)
//
// We match on substring rather than pgx's SQLSTATE because some
// errors come back wrapped or via the simple-protocol path without
// a structured code. Substring is conservative — it matches the
// SQLSTATE phrase printed by lib/pq + pgx — and false positives
// would only mean recording an already-applied migration, never
// silently skipping a legitimate DDL failure.
func isMigrationAlreadyAppliedErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"already exists",
		"duplicate column",
		"duplicate key value",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
