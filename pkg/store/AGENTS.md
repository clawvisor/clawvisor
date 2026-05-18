# pkg/store — Data Layer

## OVERVIEW

The Store interface and its two implementations form Clawvisor's persistence layer. Every handler, daemon, and background job reads and writes state through this package. No other package issues SQL directly.

## STRUCTURE

```
pkg/store/
├── store.go          # Store interface (905 lines, 75+ methods) + all domain types
├── context.go        # AgentFromContext / WithAgent helpers
├── sqlite/
│   ├── sqlite.go      # DB init, WAL mode, migration runner
│   ├── store.go       # SQLite implementation (3711 lines)
│   └── migrations/    # 45 numbered .sql files (001_init … 041_agent_token_expiry)
└── postgres/
    ├── postgres.go     # Pool init, migration runner
    ├── store.go        # Postgres implementation (3246 lines)
    └── migrations/     # 44 numbered .sql files (001_init … 041_agent_token_expiry)
```

## WHERE TO LOOK

| Task | Location | Notes |
|------|----------|-------|
| Add a new Store method | `store.go` interface, then both `sqlite/store.go` and `postgres/store.go` | Must implement in both |
| Add a new domain type | `store.go` (bottom half, after the interface) | Structs with JSON tags |
| Add a DB column or table | `sqlite/migrations/` and `postgres/migrations/` | Next number in sequence, both dirs |
| Fix a query bug | `sqlite/store.go` or `postgres/store.go` | Check the other impl too |
| Change migration ordering | Never. Renaming an applied file re-applies it | |
| Add a filter/pagination type | `store.go` alongside the interface (e.g. `TaskFilter`, `AuditFilter`) | |
| Context-passing for auth | `context.go` | `AgentFromContext` / `WithAgent` |

## CONVENTIONS

- **Interface in `pkg/`, implementations in subdirectories.** The `Store` interface lives in `store.go`; concrete types are `sqlite.Store` and `postgres.Store` (both wrap `*sql.DB` / `*pgxpool.Pool`).
- **Two implementations must stay in sync.** Every interface method needs both a SQLite and a Postgres implementation. SQLite uses `?` placeholders; Postgres uses `$1, $2, ...`.
- **Migrations are embedded and auto-applied.** Each subdirectory embeds its `migrations/` via `//go:embed`. The runner creates `schema_migrations` and applies any `.sql` file not yet recorded, in lexicographic order, each inside its own transaction.
- **Migration file naming:** `NNN_descriptive_name.sql`, strictly increasing. The filename is the primary key in `schema_migrations`, so never rename an already-applied file.
- **SQLite tolerates duplicate-column errors** in migrations (for renumbering safety). Postgres does not; use `IF NOT EXISTS` or `ADD COLUMN IF NOT EXISTS` where needed.
- **SQLite runs with `MaxOpenConns(1)`, WAL mode, `busy_timeout=5000`, and `foreign_keys=ON`.** These are set in `sqlite.New`.
- **Sentinel errors:** `ErrNotFound` and `ErrConflict` are package-level vars in `store.go`. Handlers check these with `errors.Is`.
- **Atomic state transitions** use `UpdateTaskStatusFrom`, `UpdatePendingApprovalStatusFrom`, `ClaimPendingApprovalForExecution`, and `ClaimStalledExecutingApprovalForRecovery`. These return `(bool, error)` where `bool` indicates whether the caller won the race.
- **`ConsumeSession` and `ConsumeAuthorizationCode`** atomically delete-and-return to prevent replay attacks on refresh tokens and OAuth codes.

## ANTI-PATTERNS

- NEVER write raw SQL outside `sqlite/store.go` or `postgres/store.go`. All database access goes through the Store interface.
- NEVER add a method to only one implementation. If the interface changes, both must change.
- NEVER include `BEGIN`/`COMMIT` in migration `.sql` files. The runner wraps each file in a transaction already.
- NEVER use `CREATE INDEX CONCURRENTLY` in Postgres migrations. It cannot run inside a transaction.
- NEVER rename an already-applied migration file. The filename is the idempotency key.
- NEVER log or expose credential values, token hashes, or full request bodies from Store types. The `json:"-"` tags on sensitive fields exist for a reason.