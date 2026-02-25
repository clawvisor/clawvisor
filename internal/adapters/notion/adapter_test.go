package notion

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/clawvisor/clawvisor/internal/adapters"
)

func validCred(token string) []byte {
	b, _ := json.Marshal(storedCredential{Type: "api_key", Token: token})
	return b
}

// ── ValidateCredential ────────────────────────────────────────────────────────

func TestValidateCredential_Valid(t *testing.T) {
	a := New()
	if err := a.ValidateCredential(validCred("secret_abc123")); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestValidateCredential_EmptyToken(t *testing.T) {
	a := New()
	if err := a.ValidateCredential(validCred("")); err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestValidateCredential_InvalidJSON(t *testing.T) {
	a := New()
	if err := a.ValidateCredential([]byte("{bad")); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// ── ServiceID / SupportedActions ──────────────────────────────────────────────

func TestServiceID(t *testing.T) {
	if got := New().ServiceID(); got != "notion" {
		t.Fatalf("expected 'notion', got %q", got)
	}
}

func TestSupportedActions(t *testing.T) {
	actions := New().SupportedActions()
	expected := map[string]bool{
		"search":         true,
		"get_page":       true,
		"create_page":    true,
		"update_page":    true,
		"query_database": true,
		"list_databases": true,
	}
	if len(actions) != len(expected) {
		t.Fatalf("expected %d actions, got %d", len(expected), len(actions))
	}
	for _, a := range actions {
		if !expected[a] {
			t.Errorf("unexpected action %q", a)
		}
	}
}

// ── Execute unknown action ────────────────────────────────────────────────────

func TestExecute_UnknownAction(t *testing.T) {
	_, err := New().Execute(context.Background(), adapters.Request{
		Action:     "nonexistent",
		Params:     map[string]any{},
		Credential: validCred("secret_test"),
	})
	if err == nil {
		t.Fatal("expected error for unknown action")
	}
}

// ── OAuth stubs ───────────────────────────────────────────────────────────────

func TestOAuthStubs(t *testing.T) {
	a := New()
	if a.OAuthConfig() != nil {
		t.Error("OAuthConfig should return nil")
	}
	if a.RequiredScopes() != nil {
		t.Error("RequiredScopes should return nil")
	}
	if _, err := a.CredentialFromToken(nil); err == nil {
		t.Error("CredentialFromToken should return error")
	}
}

// ── Notion-Version header ─────────────────────────────────────────────────────

func TestTokenTransport_SetsNotionVersion(t *testing.T) {
	var gotVersion, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.Header.Get("Notion-Version")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
	}))
	defer srv.Close()

	client := notionClient("secret_test")
	req, _ := http.NewRequest("GET", srv.URL, nil)
	client.Do(req)

	if gotVersion != "2022-06-28" {
		t.Errorf("expected Notion-Version '2022-06-28', got %q", gotVersion)
	}
	if gotAuth != "Bearer secret_test" {
		t.Errorf("expected 'Bearer secret_test', got %q", gotAuth)
	}
}

// ── flattenTitle ──────────────────────────────────────────────────────────────

func TestFlattenTitle_WithTitle(t *testing.T) {
	props := map[string]any{
		"Name": map[string]any{
			"type": "title",
			"title": []any{
				map[string]any{"plain_text": "My Page Title"},
			},
		},
	}
	got := flattenTitle(props)
	if got != "My Page Title" {
		t.Errorf("expected 'My Page Title', got %q", got)
	}
}

func TestFlattenTitle_NoTitle(t *testing.T) {
	props := map[string]any{
		"Status": map[string]any{
			"type": "select",
			"select": map[string]any{
				"name": "Done",
			},
		},
	}
	got := flattenTitle(props)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestFlattenTitle_EmptyProps(t *testing.T) {
	if got := flattenTitle(nil); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

// ── flattenProperties ─────────────────────────────────────────────────────────

func TestFlattenProperties(t *testing.T) {
	props := map[string]any{
		"Name": map[string]any{
			"type": "title",
			"title": []any{
				map[string]any{"plain_text": "Test"},
			},
		},
		"Notes": map[string]any{
			"type": "rich_text",
			"rich_text": []any{
				map[string]any{"plain_text": "Some notes here"},
			},
		},
		"Count": map[string]any{
			"type":   "number",
			"number": float64(42),
		},
		"Status": map[string]any{
			"type":   "select",
			"select": map[string]any{"name": "Done"},
		},
		"Tags": map[string]any{
			"type": "multi_select",
			"multi_select": []any{
				map[string]any{"name": "urgent"},
				map[string]any{"name": "bug"},
			},
		},
		"Active": map[string]any{
			"type":     "checkbox",
			"checkbox": true,
		},
		"Due": map[string]any{
			"type": "date",
			"date": map[string]any{"start": "2026-03-01"},
		},
		"Link": map[string]any{
			"type": "url",
			"url":  "https://example.com",
		},
	}

	flat := flattenProperties(props)

	tests := map[string]any{
		"Name":   "Test",
		"Notes":  "Some notes here",
		"Count":  float64(42),
		"Status": "Done",
		"Active": true,
		"Due":    "2026-03-01",
		"Link":   "https://example.com",
	}
	for k, want := range tests {
		got := flat[k]
		if fmt := json.Marshal; fmt != nil { // just a trick to use encoding/json
			a, _ := json.Marshal(got)
			b, _ := json.Marshal(want)
			if string(a) != string(b) {
				t.Errorf("flattenProperties[%q] = %v, want %v", k, got, want)
			}
		}
	}

	// Check multi_select
	tags, ok := flat["Tags"].([]string)
	if !ok {
		t.Fatalf("Tags should be []string, got %T", flat["Tags"])
	}
	if len(tags) != 2 || tags[0] != "urgent" || tags[1] != "bug" {
		t.Errorf("Tags = %v, want [urgent, bug]", tags)
	}
}

// ── get_page with test server ─────────────────────────────────────────────────

func TestGetPage_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":           "page-123",
			"object":       "page",
			"url":          "https://notion.so/page-123",
			"created_time": "2026-01-15T10:00:00.000Z",
			"archived":     false,
			"properties": map[string]any{
				"Name": map[string]any{
					"type": "title",
					"title": []any{
						map[string]any{"plain_text": "Test Page"},
					},
				},
			},
		})
	}))
	defer srv.Close()

	a := New()
	client := notionClient("secret_test")
	// Override the client to point at test server
	client.Transport = &rewriteTransport{base: client.Transport, target: srv.URL}

	result, err := a.getPage(context.Background(), client, map[string]any{"page_id": "page-123"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Summary != "Page page-123" {
		t.Errorf("expected 'Page page-123', got %q", result.Summary)
	}
}

func TestGetPage_MissingPageID(t *testing.T) {
	a := New()
	_, err := a.getPage(context.Background(), &http.Client{}, map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing page_id")
	}
}

func TestCreatePage_MissingParent(t *testing.T) {
	a := New()
	_, err := a.createPage(context.Background(), &http.Client{}, map[string]any{
		"properties": map[string]any{},
	})
	if err == nil {
		t.Fatal("expected error for missing parent")
	}
}

func TestUpdatePage_MissingPageID(t *testing.T) {
	a := New()
	_, err := a.updatePage(context.Background(), &http.Client{}, map[string]any{
		"properties": map[string]any{},
	})
	if err == nil {
		t.Fatal("expected error for missing page_id")
	}
}

func TestQueryDatabase_MissingDatabaseID(t *testing.T) {
	a := New()
	_, err := a.queryDatabase(context.Background(), &http.Client{}, map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing database_id")
	}
}

// ── HTTP error handling ───────────────────────────────────────────────────────

func TestGetPage_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"object":"error","status":404,"message":"page not found"}`))
	}))
	defer srv.Close()

	a := New()
	client := notionClient("secret_test")
	client.Transport = &rewriteTransport{base: client.Transport, target: srv.URL}

	_, err := a.getPage(context.Background(), client, map[string]any{"page_id": "bad-id"})
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

// rewriteTransport redirects all requests to the test server.
type rewriteTransport struct {
	base   http.RoundTripper
	target string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	parsed, _ := req.URL.Parse(t.target + req.URL.Path)
	if req.URL.RawQuery != "" {
		parsed.RawQuery = req.URL.RawQuery
	}
	clone.URL = parsed
	return t.base.RoundTrip(clone)
}
