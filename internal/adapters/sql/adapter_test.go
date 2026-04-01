package sqladapter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/adapters"
)

func testCred(t *testing.T) []byte {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	b, _ := json.Marshal(credential{Driver: "sqlite", DSN: dbPath})
	return b
}

func setupTestDB(t *testing.T) []byte {
	t.Helper()
	cred := testCred(t)
	a := New()

	ctx := context.Background()
	// Create a table with some data.
	_, err := a.Execute(ctx, adapters.Request{
		Action:     "execute",
		Params:     map[string]any{"sql": "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, email TEXT)"},
		Credential: cred,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Execute(ctx, adapters.Request{
		Action:     "execute",
		Params:     map[string]any{"sql": "INSERT INTO users (name, email) VALUES ('Alice', 'alice@example.com'), ('Bob', 'bob@example.com')"},
		Credential: cred,
	})
	if err != nil {
		t.Fatal(err)
	}
	return cred
}

func TestServiceID(t *testing.T) {
	a := New()
	if a.ServiceID() != "sql" {
		t.Errorf("got %q, want %q", a.ServiceID(), "sql")
	}
}

func TestSupportedActions(t *testing.T) {
	a := New()
	actions := a.SupportedActions()
	want := map[string]bool{"query": true, "execute": true, "list_tables": true, "describe_table": true}
	for _, act := range actions {
		if !want[act] {
			t.Errorf("unexpected action %q", act)
		}
		delete(want, act)
	}
	for act := range want {
		t.Errorf("missing action %q", act)
	}
}

func TestValidateCredential(t *testing.T) {
	a := New()

	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid postgres", `{"driver":"postgres","dsn":"postgres://localhost/mydb"}`, false},
		{"valid mysql", `{"driver":"mysql","dsn":"user:pass@tcp(localhost:3306)/mydb"}`, false},
		{"valid sqlite", `{"driver":"sqlite","dsn":"/tmp/test.db"}`, false},
		{"bad driver", `{"driver":"oracle","dsn":"foo"}`, true},
		{"empty dsn", `{"driver":"postgres","dsn":""}`, true},
		{"bad json", `not json`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := a.ValidateCredential([]byte(tt.input))
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateCredential(%s) err=%v, wantErr=%v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestQuery(t *testing.T) {
	cred := setupTestDB(t)
	a := New()
	ctx := context.Background()

	res, err := a.Execute(ctx, adapters.Request{
		Action:     "query",
		Params:     map[string]any{"sql": "SELECT name, email FROM users ORDER BY name"},
		Credential: cred,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary != "2 row(s) returned" {
		t.Errorf("summary = %q", res.Summary)
	}
	rows, ok := res.Data.([]map[string]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %v", res.Data)
	}
	if rows[0]["name"] != "Alice" {
		t.Errorf("first row name = %v", rows[0]["name"])
	}
}

func TestQueryWithArgs(t *testing.T) {
	cred := setupTestDB(t)
	a := New()
	ctx := context.Background()

	res, err := a.Execute(ctx, adapters.Request{
		Action:     "query",
		Params:     map[string]any{"sql": "SELECT name FROM users WHERE email = ?", "args": []any{"bob@example.com"}},
		Credential: cred,
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := res.Data.([]map[string]any)
	if len(rows) != 1 || rows[0]["name"] != "Bob" {
		t.Errorf("unexpected result: %v", rows)
	}
}

func TestQueryMaxRows(t *testing.T) {
	cred := setupTestDB(t)
	a := New()
	ctx := context.Background()

	res, err := a.Execute(ctx, adapters.Request{
		Action:     "query",
		Params:     map[string]any{"sql": "SELECT * FROM users", "max_rows": float64(1)},
		Credential: cred,
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := res.Data.([]map[string]any)
	if len(rows) != 1 {
		t.Errorf("expected 1 row, got %d", len(rows))
	}
}

func TestExecute(t *testing.T) {
	cred := setupTestDB(t)
	a := New()
	ctx := context.Background()

	res, err := a.Execute(ctx, adapters.Request{
		Action:     "execute",
		Params:     map[string]any{"sql": "UPDATE users SET name = 'Charlie' WHERE email = 'alice@example.com'"},
		Credential: cred,
	})
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(map[string]any)
	if data["rows_affected"] != int64(1) {
		t.Errorf("rows_affected = %v", data["rows_affected"])
	}
}

func TestListTables(t *testing.T) {
	cred := setupTestDB(t)
	a := New()
	ctx := context.Background()

	res, err := a.Execute(ctx, adapters.Request{
		Action:     "list_tables",
		Credential: cred,
	})
	if err != nil {
		t.Fatal(err)
	}
	tables := res.Data.([]map[string]any)
	if len(tables) != 1 || tables[0]["table_name"] != "users" {
		t.Errorf("unexpected tables: %v", tables)
	}
}

func TestDescribeTable(t *testing.T) {
	cred := setupTestDB(t)
	a := New()
	ctx := context.Background()

	res, err := a.Execute(ctx, adapters.Request{
		Action:     "describe_table",
		Params:     map[string]any{"table": "users"},
		Credential: cred,
	})
	if err != nil {
		t.Fatal(err)
	}
	cols := res.Data.([]map[string]any)
	if len(cols) != 3 {
		t.Errorf("expected 3 columns, got %d: %v", len(cols), cols)
	}
}

func TestDescribeTableBadName(t *testing.T) {
	cred := setupTestDB(t)
	a := New()
	ctx := context.Background()

	_, err := a.Execute(ctx, adapters.Request{
		Action:     "describe_table",
		Params:     map[string]any{"table": "users; DROP TABLE users"},
		Credential: cred,
	})
	if err == nil {
		t.Fatal("expected error for SQL injection attempt")
	}
}

func TestInterfaceCompliance(t *testing.T) {
	var _ adapters.Adapter = New()
	var _ adapters.MetadataProvider = New()
	var _ adapters.VerificationHinter = New()
}

func TestMetadata(t *testing.T) {
	a := New()
	meta := a.ServiceMetadata()
	if meta.DisplayName != "SQL Database" {
		t.Errorf("DisplayName = %q", meta.DisplayName)
	}
	if len(meta.ActionMeta) != 4 {
		t.Errorf("expected 4 action metas, got %d", len(meta.ActionMeta))
	}
}

// Ensure temp files are cleaned up.
func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
