package drive

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type staticOAuthProvider struct{}

func (staticOAuthProvider) OAuthClientCredentials() (string, string) { return "id", "secret" }

func newTestClient(srv *httptest.Server) *http.Client {
	return &http.Client{Transport: rewriteTransport{base: srv.URL}}
}

type rewriteTransport struct{ base string }

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(r.base, "http://")
	return http.DefaultTransport.RoundTrip(clone)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestExportFile_SheetTabByName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/drive/v3/files/sheet-1":
			writeJSON(w, map[string]any{"id": "sheet-1", "name": "Routing", "mimeType": sheetsMimeType})
		case "/v4/spreadsheets/sheet-1":
			writeJSON(w, map[string]any{
				"sheets": []any{
					map[string]any{"properties": map[string]any{"title": "README"}},
					map[string]any{"properties": map[string]any{"title": "Routing Map"}},
				},
			})
		case "/v4/spreadsheets/sheet-1/values/'Routing Map'":
			writeJSON(w, map[string]any{
				"range":  "Routing Map!A1:B2",
				"values": [][]any{{"GP", "Sector"}, {"Alice", "Fintech"}},
			})
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := New(staticOAuthProvider{})
	res, err := a.exportFile(t.Context(), newTestClient(srv), map[string]any{
		"file_id":    "sheet-1",
		"mime_type":  "text/csv",
		"sheet_name": "Routing Map",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected map data, got %T", res.Data)
	}
	if data["sheet_name"] != "Routing Map" {
		t.Errorf("sheet_name = %v, want Routing Map", data["sheet_name"])
	}
	// Exported content is base64 so it round-trips byte for byte.
	if data["encoding"] != "base64" {
		t.Fatalf("encoding = %v, want base64", data["encoding"])
	}
	raw, err := base64.StdEncoding.DecodeString(data["content"].(string))
	if err != nil {
		t.Fatalf("content is not decodable base64: %v", err)
	}
	content := string(raw)
	if !strings.Contains(content, "GP,Sector") || !strings.Contains(content, "Alice,Fintech") {
		t.Errorf("content missing expected rows:\n%s", content)
	}
}

// TestExportFile_SheetTabQuotesEmbeddedApostrophe verifies that a tab title
// containing a single quote (e.g. "Q1'25") is escaped per A1 notation rules.
func TestExportFile_SheetTabQuotesEmbeddedApostrophe(t *testing.T) {
	var valuesHit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/drive/v3/files/sheet-1":
			writeJSON(w, map[string]any{"id": "sheet-1", "name": "Routing", "mimeType": sheetsMimeType})
		case r.URL.Path == "/v4/spreadsheets/sheet-1":
			writeJSON(w, map[string]any{
				"sheets": []any{
					map[string]any{"properties": map[string]any{"title": "Q1'25"}},
				},
			})
		case strings.HasPrefix(r.URL.Path, "/v4/spreadsheets/sheet-1/values/"):
			valuesHit = r.URL.Path
			writeJSON(w, map[string]any{"values": [][]any{{"x"}}})
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := New(staticOAuthProvider{})
	_, err := a.exportFile(t.Context(), newTestClient(srv), map[string]any{
		"file_id":    "sheet-1",
		"mime_type":  "text/csv",
		"sheet_name": "Q1'25",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Expect range 'Q1''25' (single quote in title doubled), URL-encoded.
	if !strings.HasSuffix(valuesHit, "/'Q1''25'") {
		t.Errorf("values path = %q, want suffix /'Q1''25'", valuesHit)
	}
}

func TestExportFile_RejectsSheetNameOnNonSheet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/drive/v3/files/doc-1" {
			writeJSON(w, map[string]any{"id": "doc-1", "name": "Memo", "mimeType": "application/vnd.google-apps.document"})
			return
		}
		t.Errorf("unexpected request path: %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	a := New(staticOAuthProvider{})
	_, err := a.exportFile(t.Context(), newTestClient(srv), map[string]any{
		"file_id":    "doc-1",
		"mime_type":  "text/csv",
		"sheet_name": "Sheet1",
	})
	if err == nil || !strings.Contains(err.Error(), "only supported when the source is a Google Sheet") {
		t.Errorf("expected source-type error, got %v", err)
	}
}

func TestGetFile_SheetIncludesAvailableTabs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/drive/v3/files/sheet-1":
			writeJSON(w, map[string]any{"id": "sheet-1", "name": "Routing", "mimeType": sheetsMimeType})
		case "/v4/spreadsheets/sheet-1":
			writeJSON(w, map[string]any{
				"sheets": []any{
					map[string]any{"properties": map[string]any{"title": "README"}},
					map[string]any{"properties": map[string]any{"title": "Routing Map"}},
				},
			})
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := New(staticOAuthProvider{})
	res, err := a.getFile(t.Context(), newTestClient(srv), map[string]any{
		"file_id": "sheet-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data := res.Data.(map[string]any)
	tabs, ok := data["available_tabs"].([]string)
	if !ok {
		t.Fatalf("available_tabs = %T, want []string", data["available_tabs"])
	}
	if len(tabs) != 2 || tabs[0] != "README" || tabs[1] != "Routing Map" {
		t.Errorf("available_tabs = %v, want [README Routing Map]", tabs)
	}
}

func TestExportFile_SheetWithoutSheetNameEmitsHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/drive/v3/files/sheet-1":
			writeJSON(w, map[string]any{"id": "sheet-1", "name": "Routing", "mimeType": sheetsMimeType})
		case "/drive/v3/files/sheet-1/export":
			_, _ = w.Write([]byte("col1,col2\n"))
		case "/v4/spreadsheets/sheet-1":
			writeJSON(w, map[string]any{
				"sheets": []any{
					map[string]any{"properties": map[string]any{"title": "README"}},
					map[string]any{"properties": map[string]any{"title": "Routing Map"}},
				},
			})
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := New(staticOAuthProvider{})
	res, err := a.exportFile(t.Context(), newTestClient(srv), map[string]any{
		"file_id":   "sheet-1",
		"mime_type": "text/csv",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Meta == nil || res.Meta["hint"] == nil {
		t.Fatal("expected meta.hint pointing at sheet_name")
	}
	hint, _ := res.Meta["hint"].(string)
	if !strings.Contains(hint, "sheet_name") || !strings.Contains(hint, "first tab") {
		t.Errorf("hint missing expected guidance: %q", hint)
	}
	tabs, ok := res.Meta["available_tabs"].([]string)
	if !ok {
		t.Fatalf("available_tabs = %T, want []string", res.Meta["available_tabs"])
	}
	if len(tabs) != 2 || tabs[0] != "README" || tabs[1] != "Routing Map" {
		t.Errorf("available_tabs = %v, want [README Routing Map]", tabs)
	}
	data := res.Data.(map[string]any)
	dataTabs, ok := data["available_tabs"].([]string)
	if !ok {
		t.Fatalf("data.available_tabs = %T, want []string", data["available_tabs"])
	}
	if len(dataTabs) != 2 || dataTabs[0] != "README" || dataTabs[1] != "Routing Map" {
		t.Errorf("data.available_tabs = %v, want [README Routing Map]", dataTabs)
	}
}

func TestExportFile_UnknownSheetName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/drive/v3/files/sheet-1":
			writeJSON(w, map[string]any{"id": "sheet-1", "name": "Routing", "mimeType": sheetsMimeType})
		case "/v4/spreadsheets/sheet-1":
			writeJSON(w, map[string]any{
				"sheets": []any{
					map[string]any{"properties": map[string]any{"title": "README"}},
					map[string]any{"properties": map[string]any{"title": "Routing Map"}},
				},
			})
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := New(staticOAuthProvider{})
	_, err := a.exportFile(t.Context(), newTestClient(srv), map[string]any{
		"file_id":    "sheet-1",
		"mime_type":  "text/csv",
		"sheet_name": "Nope",
	})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not-found error listing available tabs, got %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "Routing Map") {
		t.Errorf("expected error to list available tabs (got %v)", err)
	}
}

// Regression for the truncation bug: downloads were capped at MaxBodyLen and
// the prefix was returned as a successful, byte-exact result.
func TestDownloadFile_RefusesOversizedInsteadOfTruncating(t *testing.T) {
	big := bytes.Repeat([]byte("A"), 300_000) // over the old 200,000 cap
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("alt") == "media" {
			_, _ = w.Write(big)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "f1", "name": "big.bin", "mimeType": "application/octet-stream", "size": "300000",
		})
	}))
	defer srv.Close()

	adapter := New(staticOAuthProvider{})

	// Default limit is 1 MB, so 300 KB now succeeds intact — previously it
	// came back truncated to 200,000 bytes.
	res, err := adapter.downloadFile(context.Background(), newTestClient(srv), map[string]any{"file_id": "f1"})
	if err != nil {
		t.Fatalf("300 KB should fit the default limit: %v", err)
	}
	got, err := base64.StdEncoding.DecodeString(res.Data.(map[string]any)["content"].(string))
	if err != nil {
		t.Fatalf("undecodable base64: %v", err)
	}
	if !bytes.Equal(got, big) {
		t.Errorf("content truncated: got %d bytes, want %d", len(got), len(big))
	}

	// Over the requested limit must be an error, not a truncated success.
	_, err = adapter.downloadFile(context.Background(), newTestClient(srv),
		map[string]any{"file_id": "f1", "max_bytes": float64(1024)})
	if err == nil {
		t.Fatal("expected an error for a file over max_bytes")
	}
	if !strings.Contains(err.Error(), "max_bytes") {
		t.Errorf("error should point at max_bytes, got: %v", err)
	}
}
