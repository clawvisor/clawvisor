package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	_ "modernc.org/sqlite" // registers "sqlite" driver
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// New opens a SQLite database at the given path and runs pending migrations.
func New(ctx context.Context, path string) (*sql.DB, error) {
	// modernc.org/sqlite registers as "sqlite"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite: %w", err)
	}
	// SQLite works best with a single connection for writes
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging sqlite: %w", err)
	}

	// Enable WAL mode, foreign keys, and a busy timeout so concurrent
	// readers (agent auth) don't fail immediately when the single
	// connection is held by a dashboard write.
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode = WAL`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 5000`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		db.Close()
		return nil, err
	}

	if err := runMigrations(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}
	return db, nil
}

func runMigrations(ctx context.Context, db *sql.DB) error {
	return runMigrationsFS(ctx, db, migrationsFS)
}

func runMigrationsFS(ctx context.Context, db *sql.DB, migrations fs.FS) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}

	rows, err := db.QueryContext(ctx, `SELECT name FROM schema_migrations ORDER BY name`)
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

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", entry.Name(), err)
		}
		if _, err := tx.ExecContext(ctx, string(data)); err != nil {
			// SQLite doesn't support ADD COLUMN IF NOT EXISTS. Tolerate
			// "duplicate column name" errors so migrations survive renumbering
			// (e.g., 020_agent_org_id.sql → 021_agent_org_id.sql).
			if !strings.Contains(err.Error(), "duplicate column name") {
				_ = tx.Rollback()
				return fmt.Errorf("applying migration %s: %w", entry.Name(), err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (name) VALUES (?)`,
			entry.Name(),
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("recording migration %s: %w", entry.Name(), err)
		}
		if err := tx.Commit(); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("committing migration %s: %w", entry.Name(), err)
		}
	}
	return nil
}
