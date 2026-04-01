package sqladapter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/adapters"
)

// Integration tests hit real Postgres and MySQL containers.
// Run with: go test -tags integration ./internal/adapters/sql/ -v
//
// Start databases first:
//   docker compose -f internal/adapters/sql/testdata/docker-compose.yml up -d --wait

func pgCred(t *testing.T) []byte {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = "postgres://testuser:testpass@localhost:15432/testdb?sslmode=disable"
	}
	b, _ := json.Marshal(credential{Driver: "postgres", DSN: dsn})
	return b
}

func mysqlCred(t *testing.T) []byte {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		dsn = "testuser:testpass@tcp(localhost:13306)/testdb"
	}
	b, _ := json.Marshal(credential{Driver: "mysql", DSN: dsn})
	return b
}

type driverTest struct {
	name string
	cred func(t *testing.T) []byte
}

func drivers() []driverTest {
	return []driverTest{
		{"postgres", pgCred},
		{"mysql", mysqlCred},
	}
}

func TestIntegrationRoundTrip(t *testing.T) {
	if os.Getenv("SQL_INTEGRATION") == "" {
		t.Skip("set SQL_INTEGRATION=1 to run; needs docker containers")
	}

	a := New()
	ctx := context.Background()

	for _, drv := range drivers() {
		t.Run(drv.name, func(t *testing.T) {
			cred := drv.cred(t)

			// Clean up from previous runs.
			a.Execute(ctx, adapters.Request{
				Action:     "execute",
				Params:     map[string]any{"sql": "DROP TABLE IF EXISTS sqladapter_test"},
				Credential: cred,
			})

			// CREATE
			_, err := a.Execute(ctx, adapters.Request{
				Action:     "execute",
				Params:     map[string]any{"sql": "CREATE TABLE sqladapter_test (id INT PRIMARY KEY, name VARCHAR(100), email VARCHAR(200))"},
				Credential: cred,
			})
			if err != nil {
				t.Fatalf("create table: %v", err)
			}

			// INSERT
			res, err := a.Execute(ctx, adapters.Request{
				Action: "execute",
				Params: map[string]any{
					"sql": "INSERT INTO sqladapter_test (id, name, email) VALUES (1, 'Alice', 'alice@test.com'), (2, 'Bob', 'bob@test.com')",
				},
				Credential: cred,
			})
			if err != nil {
				t.Fatalf("insert: %v", err)
			}
			data := res.Data.(map[string]any)
			if data["rows_affected"] != int64(2) {
				t.Errorf("rows_affected = %v, want 2", data["rows_affected"])
			}

			// SELECT
			res, err = a.Execute(ctx, adapters.Request{
				Action:     "query",
				Params:     map[string]any{"sql": "SELECT name, email FROM sqladapter_test ORDER BY id"},
				Credential: cred,
			})
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			rows := res.Data.([]map[string]any)
			if len(rows) != 2 {
				t.Fatalf("expected 2 rows, got %d", len(rows))
			}
			if fmt.Sprint(rows[0]["name"]) != "Alice" {
				t.Errorf("row 0 name = %v", rows[0]["name"])
			}

			// LIST TABLES
			res, err = a.Execute(ctx, adapters.Request{
				Action:     "list_tables",
				Credential: cred,
			})
			if err != nil {
				t.Fatalf("list_tables: %v", err)
			}
			tables := res.Data.([]map[string]any)
			found := false
			for _, tbl := range tables {
				if fmt.Sprint(tbl["table_name"]) == "sqladapter_test" {
					found = true
				}
			}
			if !found {
				t.Errorf("sqladapter_test not in table list: %v", tables)
			}

			// DESCRIBE TABLE
			res, err = a.Execute(ctx, adapters.Request{
				Action:     "describe_table",
				Params:     map[string]any{"table": "sqladapter_test"},
				Credential: cred,
			})
			if err != nil {
				t.Fatalf("describe_table: %v", err)
			}
			cols := res.Data.([]map[string]any)
			if len(cols) != 3 {
				t.Errorf("expected 3 columns, got %d: %v", len(cols), cols)
			}

			// CLEANUP
			_, _ = a.Execute(ctx, adapters.Request{
				Action:     "execute",
				Params:     map[string]any{"sql": "DROP TABLE sqladapter_test"},
				Credential: cred,
			})
		})
	}
}
