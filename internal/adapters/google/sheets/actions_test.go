package sheets

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient returns an *http.Client whose transport rewrites every request
// URL so that it hits srv instead of the real API endpoints.
func newTestClient(srv *httptest.Server) *http.Client {
	return &http.Client{
		Transport: rewriteTransport{base: srv.URL, inner: http.DefaultTransport},
	}
}

// rewriteTransport replaces the scheme+host of every outgoing request with the
// test server's base URL, leaving the path and query intact.
type rewriteTransport struct {
	base  string // e.g. "http://127.0.0.1:PORT"
	inner http.RoundTripper
}

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(r.base, "http://")
	return r.inner.RoundTrip(clone)
}

// jsonHandler returns an HTTP handler that always responds 200 with v as JSON.
func jsonHandler(t *testing.T, v any) http.HandlerFunc {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("jsonHandler: marshal: %v", err)
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}
}

// adapter returns a zero-config SheetsAdapter (OAuth not needed for these tests
// because we bypass Execute and call the action methods directly).
func adapter() *SheetsAdapter { return New(staticOAuthProvider{}) }

// ---------------------------------------------------------------------------
// listSpreadsheets
// ---------------------------------------------------------------------------

func TestListSpreadsheets_HappyPath(t *testing.T) {
	resp := map[string]any{
		"files": []any{
			map[string]any{
				"id":           "sheet-id-1",
				"name":         "My Budget",
				"modifiedTime": "2024-01-01T00:00:00Z",
				"webViewLink":  "https://docs.google.com/spreadsheets/d/sheet-id-1",
			},
		},
	}
	srv := httptest.NewServer(jsonHandler(t, resp))
	defer srv.Close()

	result, err := adapter().listSpreadsheets(t.Context(), newTestClient(srv), map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	items, ok := result.Data.([]spreadsheetListItem)
	if !ok {
		t.Fatalf("unexpected data type: %T", result.Data)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ID != "sheet-id-1" {
		t.Errorf("unexpected ID: %q", items[0].ID)
	}
	if items[0].Name != "My Budget" {
		t.Errorf("unexpected name: %q", items[0].Name)
	}
}

func TestListSpreadsheets_WithQuery(t *testing.T) {
	srv := httptest.NewServer(jsonHandler(t, map[string]any{"files": []any{}}))
	defer srv.Close()

	// Valid query — should pass the allowlist.
	_, err := adapter().listSpreadsheets(t.Context(), newTestClient(srv), map[string]any{
		"query": "Budget 2024",
	})
	if err != nil {
		t.Fatalf("unexpected error for valid query: %v", err)
	}
}

func TestListSpreadsheets_QueryAllowlistRejects(t *testing.T) {
	srv := httptest.NewServer(jsonHandler(t, map[string]any{"files": []any{}}))
	defer srv.Close()

	badQueries := []string{
		"name' or '1'='1",       // quote injection
		"foo and trashed=false", // Drive operator
		"bar\\baz",              // backslash
		"x<y",                   // comparison operator
		"'",                     // single quote alone
	}
	for _, q := range badQueries {
		_, err := adapter().listSpreadsheets(t.Context(), newTestClient(srv), map[string]any{
			"query": q,
		})
		if err == nil {
			t.Errorf("expected error for query %q, got nil", q)
		}
	}
}

func TestListSpreadsheets_MaxResults(t *testing.T) {
	var gotPageSize string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPageSize = r.URL.Query().Get("pageSize")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"files":[]}`))
	}))
	defer srv.Close()

	_, err := adapter().listSpreadsheets(t.Context(), newTestClient(srv), map[string]any{
		"max_results": 25,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPageSize != "25" {
		t.Errorf("expected pageSize=25, got %q", gotPageSize)
	}
}

// ---------------------------------------------------------------------------
// getSpreadsheet
// ---------------------------------------------------------------------------

func TestGetSpreadsheet_HappyPath(t *testing.T) {
	resp := map[string]any{
		"spreadsheetId": "sheet-id-1",
		"properties": map[string]any{
			"title":    "My Budget",
			"locale":   "en_US",
			"timeZone": "America/New_York",
		},
		"sheets": []any{
			map[string]any{"properties": map[string]any{"sheetId": 0, "title": "Sheet1"}},
		},
	}
	srv := httptest.NewServer(jsonHandler(t, resp))
	defer srv.Close()

	result, err := adapter().getSpreadsheet(t.Context(), newTestClient(srv), map[string]any{
		"spreadsheet_id": "sheet-id-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	meta, ok := result.Data.(spreadsheetMeta)
	if !ok {
		t.Fatalf("unexpected data type: %T", result.Data)
	}
	if meta.Title != "My Budget" {
		t.Errorf("unexpected title: %q", meta.Title)
	}
	if len(meta.Sheets) != 1 || meta.Sheets[0].Title != "Sheet1" {
		t.Errorf("unexpected sheets: %+v", meta.Sheets)
	}
}

func TestGetSpreadsheet_MissingID(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	_, err := adapter().getSpreadsheet(t.Context(), newTestClient(srv), map[string]any{})
	if err == nil {
		t.Fatalf("expected error for missing spreadsheet_id")
	}
}

// ---------------------------------------------------------------------------
// readRange
// ---------------------------------------------------------------------------

func TestReadRange_HappyPath(t *testing.T) {
	resp := map[string]any{
		"spreadsheetId":  "sheet-id-1",
		"range":          "Sheet1!A1:B2",
		"majorDimension": "ROWS",
		"values": []any{
			[]any{"Name", "Score"},
			[]any{"Alice", "99"},
		},
	}
	srv := httptest.NewServer(jsonHandler(t, resp))
	defer srv.Close()

	result, err := adapter().readRange(t.Context(), newTestClient(srv), map[string]any{
		"spreadsheet_id": "sheet-id-1",
		"range":          "Sheet1!A1:B2",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rv, ok := result.Data.(rangeValues)
	if !ok {
		t.Fatalf("unexpected data type: %T", result.Data)
	}
	if len(rv.Values) != 2 {
		t.Errorf("expected 2 rows, got %d", len(rv.Values))
	}
}

func TestReadRange_MaxRowsClamped(t *testing.T) {
	rows := make([]any, 10)
	for i := range rows {
		rows[i] = []any{"cell"}
	}
	resp := map[string]any{
		"spreadsheetId":  "sheet-id-1",
		"range":          "Sheet1!A1:A10",
		"majorDimension": "ROWS",
		"values":         rows,
	}
	srv := httptest.NewServer(jsonHandler(t, resp))
	defer srv.Close()

	result, err := adapter().readRange(t.Context(), newTestClient(srv), map[string]any{
		"spreadsheet_id": "sheet-id-1",
		"range":          "Sheet1!A1:A10",
		"max_rows":       5,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rv := result.Data.(rangeValues)
	if len(rv.Values) != 5 {
		t.Errorf("expected 5 rows after clamping, got %d", len(rv.Values))
	}
}

// ---------------------------------------------------------------------------
// appendRows
// ---------------------------------------------------------------------------

func TestAppendRows_HappyPath(t *testing.T) {
	resp := map[string]any{
		"spreadsheetId": "sheet-id-1",
		"tableRange":    "Sheet1!A1:B1",
		"updates": map[string]any{
			"updatedRange": "Sheet1!A2:B2",
			"updatedRows":  1,
		},
	}
	srv := httptest.NewServer(jsonHandler(t, resp))
	defer srv.Close()

	result, err := adapter().appendRows(t.Context(), newTestClient(srv), map[string]any{
		"spreadsheet_id": "sheet-id-1",
		"range":          "Sheet1!A1:B1",
		"rows":           []any{[]any{"Alice", "99"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ar, ok := result.Data.(appendResult)
	if !ok {
		t.Fatalf("unexpected data type: %T", result.Data)
	}
	if ar.UpdatedRows != 1 {
		t.Errorf("expected 1 updated row, got %d", ar.UpdatedRows)
	}
}

func TestAppendRows_EmptyRowsRejected(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	_, err := adapter().appendRows(t.Context(), newTestClient(srv), map[string]any{
		"spreadsheet_id": "sheet-id-1",
		"range":          "Sheet1!A1",
		"rows":           []any{},
	})
	if err == nil {
		t.Fatalf("expected error for empty rows")
	}
}

// ---------------------------------------------------------------------------
// updateCells
// ---------------------------------------------------------------------------

func TestUpdateCells_HappyPath(t *testing.T) {
	resp := map[string]any{
		"spreadsheetId":  "sheet-id-1",
		"updatedRange":   "Sheet1!A1:B1",
		"updatedRows":    1,
		"updatedColumns": 2,
		"updatedCells":   2,
	}
	srv := httptest.NewServer(jsonHandler(t, resp))
	defer srv.Close()

	result, err := adapter().updateCells(t.Context(), newTestClient(srv), map[string]any{
		"spreadsheet_id": "sheet-id-1",
		"range":          "Sheet1!A1:B1",
		"values":         []any{[]any{"Alice", "100"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ur, ok := result.Data.(updateResult)
	if !ok {
		t.Fatalf("unexpected data type: %T", result.Data)
	}
	if ur.UpdatedCells != 2 {
		t.Errorf("expected 2 updated cells, got %d", ur.UpdatedCells)
	}
}

// ---------------------------------------------------------------------------
// createSpreadsheet
// ---------------------------------------------------------------------------

func TestCreateSpreadsheet_HappyPath(t *testing.T) {
	resp := map[string]any{
		"spreadsheetId":  "new-sheet-id",
		"spreadsheetUrl": "https://docs.google.com/spreadsheets/d/new-sheet-id",
		"properties":     map[string]any{"title": "Q1 Report"},
	}
	srv := httptest.NewServer(jsonHandler(t, resp))
	defer srv.Close()

	result, err := adapter().createSpreadsheet(t.Context(), newTestClient(srv), map[string]any{
		"title": "Q1 Report",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cr, ok := result.Data.(createResult)
	if !ok {
		t.Fatalf("unexpected data type: %T", result.Data)
	}
	if cr.SpreadsheetID != "new-sheet-id" {
		t.Errorf("unexpected spreadsheet_id: %q", cr.SpreadsheetID)
	}
	if cr.Title != "Q1 Report" {
		t.Errorf("unexpected title: %q", cr.Title)
	}
}

func TestCreateSpreadsheet_MissingTitle(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	_, err := adapter().createSpreadsheet(t.Context(), newTestClient(srv), map[string]any{})
	if err == nil {
		t.Fatalf("expected error for missing title")
	}
}

func TestCreateSpreadsheet_TitleTooLong(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	_, err := adapter().createSpreadsheet(t.Context(), newTestClient(srv), map[string]any{
		"title": strings.Repeat("x", 201),
	})
	if err == nil {
		t.Fatalf("expected error for title > 200 chars")
	}
}

// ---------------------------------------------------------------------------
// escapeDriveLiteral
// ---------------------------------------------------------------------------

func TestEscapeDriveLiteral(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"it's", "it''s"},
		{"O'Reilly", "O''Reilly"},
		{"x=('y')", "x=(''y'')"},
		{"plain", "plain"},
		{"", ""},
		{"''", "''''"},
	}
	for _, tc := range tests {
		if got := escapeDriveLiteral(tc.in); got != tc.want {
			t.Errorf("escapeDriveLiteral(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
