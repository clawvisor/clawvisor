// Package sqladapter implements the Clawvisor adapter for SQL databases.
// It allows agents to query and modify SQL databases (PostgreSQL, MySQL, SQLite)
// through Clawvisor's authorization layer.
//
// Credentials are stored as JSON: {"driver":"postgres","dsn":"postgres://..."}
// Supported drivers: "postgres", "mysql", "sqlite".
package sqladapter

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"golang.org/x/oauth2"

	_ "github.com/go-sql-driver/mysql" // registers "mysql" driver
	"github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite" // registers "sqlite" driver

	"github.com/clawvisor/clawvisor/pkg/adapters"
)

const (
	serviceID     = "sql"
	maxRows       = 500
	maxColWidth   = 1000
	queryTimeout  = 30 * time.Second
)

// credential is the JSON structure stored in the vault.
type credential struct {
	Driver string `json:"driver"` // "postgres", "mysql", or "sqlite"
	DSN    string `json:"dsn"`    // connection string / file path
}

// Adapter implements adapters.Adapter for SQL databases.
type Adapter struct{}

// New returns a new SQL adapter.
func New() *Adapter { return &Adapter{} }

func (a *Adapter) ServiceID() string { return serviceID }

func (a *Adapter) SupportedActions() []string {
	return []string{"query", "execute", "list_tables", "describe_table"}
}

func (a *Adapter) OAuthConfig() *oauth2.Config                        { return nil }
func (a *Adapter) RequiredScopes() []string                           { return nil }
func (a *Adapter) CredentialFromToken(_ *oauth2.Token) ([]byte, error) {
	return nil, fmt.Errorf("sql: no token exchange — uses connection string")
}

func (a *Adapter) ValidateCredential(credBytes []byte) error {
	var c credential
	if err := json.Unmarshal(credBytes, &c); err != nil {
		return fmt.Errorf("sql: invalid credential JSON: %w", err)
	}
	switch c.Driver {
	case "postgres", "mysql", "sqlite":
	default:
		return fmt.Errorf("sql: unsupported driver %q (use \"postgres\", \"mysql\", or \"sqlite\")", c.Driver)
	}
	if c.DSN == "" {
		return fmt.Errorf("sql: dsn must not be empty")
	}
	return nil
}

// ServiceMetadata implements adapters.MetadataProvider.
func (a *Adapter) ServiceMetadata() adapters.ServiceMetadata {
	maxRowsVal := maxRows
	return adapters.ServiceMetadata{
		DisplayName: "SQL Database",
		Description: "Query and modify SQL databases (PostgreSQL, MySQL, SQLite)",
		ActionMeta: map[string]adapters.ActionMeta{
			"query": {
				DisplayName: "Run query",
				Category:    "read",
				Sensitivity: "medium",
				Description: "Execute a read-only SQL query (SELECT)",
				Params: []adapters.ParamMeta{
					{Name: "sql", Type: "string", Required: true},
					{Name: "args", Type: "array"},
					{Name: "max_rows", Type: "int", Default: 100, Max: &maxRowsVal},
				},
			},
			"execute": {
				DisplayName: "Execute statement",
				Category:    "write",
				Sensitivity: "high",
				Description: "Execute a SQL write statement (INSERT, UPDATE, DELETE, etc.)",
				Params: []adapters.ParamMeta{
					{Name: "sql", Type: "string", Required: true},
					{Name: "args", Type: "array"},
				},
			},
			"list_tables": {
				DisplayName: "List tables",
				Category:    "read",
				Sensitivity: "low",
				Description: "List all tables in the database",
			},
			"describe_table": {
				DisplayName: "Describe table",
				Category:    "read",
				Sensitivity: "low",
				Description: "Show column names and types for a table",
				Params: []adapters.ParamMeta{
					{Name: "table", Type: "string", Required: true},
				},
			},
		},
		VerificationHints: "The sql adapter executes raw SQL. Verify that write operations (INSERT/UPDATE/DELETE) match the agent's stated intent. The 'query' action is read-only; 'execute' is for writes.",
	}
}

// VerificationHints implements adapters.VerificationHinter.
func (a *Adapter) VerificationHints() string {
	return a.ServiceMetadata().VerificationHints
}

func (a *Adapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	cred, err := parseCred(req.Credential)
	if err != nil {
		return nil, err
	}

	switch req.Action {
	case "query":
		return a.query(ctx, cred, req.Params)
	case "execute":
		return a.execute(ctx, cred, req.Params)
	case "list_tables":
		return a.listTables(ctx, cred)
	case "describe_table":
		return a.describeTable(ctx, cred, req.Params)
	default:
		return nil, fmt.Errorf("sql: unsupported action %q", req.Action)
	}
}

// query executes a read-only SELECT and returns rows as maps.
func (a *Adapter) query(ctx context.Context, cred *credential, params map[string]any) (*adapters.Result, error) {
	sqlStr, ok := params["sql"].(string)
	if !ok || sqlStr == "" {
		return nil, fmt.Errorf("sql: 'sql' parameter is required")
	}

	limit := 100
	if v, ok := params["max_rows"]; ok {
		if n, ok := toInt(v); ok && n > 0 {
			limit = n
		}
	}
	if limit > maxRows {
		limit = maxRows
	}

	args := toArgs(params["args"])

	db, err := openDB(cred)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	// Use a read-only transaction for safety.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("sql: begin read-only tx: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("sql: query: %w", err)
	}
	defer rows.Close()

	data, count, err := scanRows(rows, limit)
	if err != nil {
		return nil, err
	}

	summary := fmt.Sprintf("%d row(s) returned", count)
	if count >= limit {
		summary += fmt.Sprintf(" (limit %d)", limit)
	}
	return &adapters.Result{Summary: summary, Data: data}, nil
}

// execute runs a write statement and returns rows affected.
func (a *Adapter) execute(ctx context.Context, cred *credential, params map[string]any) (*adapters.Result, error) {
	sqlStr, ok := params["sql"].(string)
	if !ok || sqlStr == "" {
		return nil, fmt.Errorf("sql: 'sql' parameter is required")
	}

	args := toArgs(params["args"])

	db, err := openDB(cred)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	result, err := db.ExecContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("sql: execute: %w", err)
	}

	affected, _ := result.RowsAffected()
	return &adapters.Result{
		Summary: fmt.Sprintf("%d row(s) affected", affected),
		Data:    map[string]any{"rows_affected": affected},
	}, nil
}

// listTables returns all user tables in the database.
func (a *Adapter) listTables(ctx context.Context, cred *credential) (*adapters.Result, error) {
	db, err := openDB(cred)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var query string
	switch cred.Driver {
	case "postgres":
		query = `SELECT table_schema, table_name FROM information_schema.tables
			WHERE table_schema NOT IN ('pg_catalog', 'information_schema')
			ORDER BY table_schema, table_name`
	case "mysql":
		query = `SELECT table_schema, table_name FROM information_schema.tables
			WHERE table_schema NOT IN ('information_schema', 'mysql', 'performance_schema', 'sys')
			ORDER BY table_schema, table_name`
	case "sqlite":
		query = `SELECT '' AS table_schema, name AS table_name FROM sqlite_master
			WHERE type='table' AND name NOT LIKE 'sqlite_%'
			ORDER BY name`
	}

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("sql: list_tables: %w", err)
	}
	defer rows.Close()

	var tables []map[string]any
	for rows.Next() {
		var schema, name string
		if err := rows.Scan(&schema, &name); err != nil {
			return nil, fmt.Errorf("sql: list_tables scan: %w", err)
		}
		entry := map[string]any{"table_name": name}
		if schema != "" {
			entry["schema"] = schema
		}
		tables = append(tables, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sql: list_tables: %w", err)
	}

	return &adapters.Result{
		Summary: fmt.Sprintf("%d table(s)", len(tables)),
		Data:    tables,
	}, nil
}

// describeTable returns column info for a table.
func (a *Adapter) describeTable(ctx context.Context, cred *credential, params map[string]any) (*adapters.Result, error) {
	table, ok := params["table"].(string)
	if !ok || table == "" {
		return nil, fmt.Errorf("sql: 'table' parameter is required")
	}

	db, err := openDB(cred)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var query string
	var args []any
	switch cred.Driver {
	case "postgres":
		query = `SELECT column_name, data_type, is_nullable, column_default
			FROM information_schema.columns
			WHERE table_name = $1
			ORDER BY ordinal_position`
		args = []any{table}
	case "mysql":
		query = `SELECT column_name, column_type, is_nullable, column_default
			FROM information_schema.columns
			WHERE table_name = ?
			ORDER BY ordinal_position`
		args = []any{table}
	case "sqlite":
		// PRAGMA doesn't support parameterized queries, so we validate the table name.
		if !isSimpleIdentifier(table) {
			return nil, fmt.Errorf("sql: invalid table name %q", table)
		}
		query = fmt.Sprintf("PRAGMA table_info(%s)", table)
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sql: describe_table: %w", err)
	}
	defer rows.Close()

	var columns []map[string]any
	switch cred.Driver {
	case "postgres", "mysql":
		for rows.Next() {
			var name, dtype, nullable string
			var dflt sql.NullString
			if err := rows.Scan(&name, &dtype, &nullable, &dflt); err != nil {
				return nil, fmt.Errorf("sql: describe_table scan: %w", err)
			}
			col := map[string]any{
				"column_name": name,
				"data_type":   dtype,
				"nullable":    nullable == "YES",
			}
			if dflt.Valid {
				col["default"] = dflt.String
			}
			columns = append(columns, col)
		}
	case "sqlite":
		for rows.Next() {
			var cid int
			var name, dtype string
			var notNull int
			var dflt sql.NullString
			var pk int
			if err := rows.Scan(&cid, &name, &dtype, &notNull, &dflt, &pk); err != nil {
				return nil, fmt.Errorf("sql: describe_table scan: %w", err)
			}
			col := map[string]any{
				"column_name": name,
				"data_type":   dtype,
				"nullable":    notNull == 0,
			}
			if dflt.Valid {
				col["default"] = dflt.String
			}
			if pk > 0 {
				col["primary_key"] = true
			}
			columns = append(columns, col)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sql: describe_table: %w", err)
	}

	return &adapters.Result{
		Summary: fmt.Sprintf("%d column(s) in %s", len(columns), table),
		Data:    columns,
	}, nil
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func parseCred(raw []byte) (*credential, error) {
	var c credential
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("sql: invalid credential: %w", err)
	}
	switch c.Driver {
	case "postgres", "mysql", "sqlite":
	default:
		return nil, fmt.Errorf("sql: unsupported driver %q", c.Driver)
	}
	if c.DSN == "" {
		return nil, fmt.Errorf("sql: dsn must not be empty")
	}
	return &c, nil
}

func openDB(cred *credential) (*sql.DB, error) {
	driverName := cred.Driver
	switch driverName {
	case "postgres":
		driverName = "pgx"
	case "mysql":
		// go-sql-driver/mysql registers as "mysql" — no rename needed.
	}
	db, err := sql.Open(driverName, cred.DSN)
	if err != nil {
		return nil, fmt.Errorf("sql: open %s: %w", cred.Driver, err)
	}
	// Sensible defaults for a gateway adapter — we open/close per request.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(queryTimeout)
	return db, nil
}

// scanRows reads up to limit rows and returns them as []map[string]any.
func scanRows(rows *sql.Rows, limit int) ([]map[string]any, int, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, 0, fmt.Errorf("sql: columns: %w", err)
	}

	var result []map[string]any
	count := 0
	for rows.Next() && count < limit {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, 0, fmt.Errorf("sql: scan row: %w", err)
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			row[col] = formatValue(values[i])
		}
		result = append(result, row)
		count++
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("sql: rows: %w", err)
	}
	return result, count, nil
}

// formatValue converts SQL scan values into JSON-safe types.
func formatValue(v any) any {
	switch val := v.(type) {
	case nil:
		return nil
	case []byte:
		s := string(val)
		if len(s) > maxColWidth {
			return s[:maxColWidth] + " [truncated]"
		}
		return s
	case time.Time:
		return val.Format(time.RFC3339)
	default:
		return val
	}
}

// toArgs converts a params["args"] value to []any for parameterized queries.
func toArgs(v any) []any {
	if v == nil {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	return arr
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case float64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

// isSimpleIdentifier validates that a string is safe to use in a SQL identifier
// position (letters, digits, underscores only).
func isSimpleIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_') {
			return false
		}
	}
	return true
}

// init registers the pgx driver name alias so sql.Open("pgx", dsn) works.
func init() {
	// pgx/v5/stdlib auto-registers "pgx" when imported, but we ensure the
	// import is used here so the compiler doesn't drop it.
	_ = stdlib.RegisterConnConfig
}

// Ensure interface compliance at compile time.
var (
	_ adapters.Adapter            = (*Adapter)(nil)
	_ adapters.MetadataProvider   = (*Adapter)(nil)
	_ adapters.VerificationHinter = (*Adapter)(nil)
)

