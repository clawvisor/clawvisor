package linear

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
	if err := a.ValidateCredential(validCred("lin_api_abc123")); err != nil {
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
	if err := a.ValidateCredential([]byte("nope")); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// ── ServiceID / SupportedActions ──────────────────────────────────────────────

func TestServiceID(t *testing.T) {
	if got := New().ServiceID(); got != "linear" {
		t.Fatalf("expected 'linear', got %q", got)
	}
}

func TestSupportedActions(t *testing.T) {
	actions := New().SupportedActions()
	expected := map[string]bool{
		"list_issues":   true,
		"get_issue":     true,
		"create_issue":  true,
		"update_issue":  true,
		"add_comment":   true,
		"list_teams":    true,
		"list_projects": true,
		"search_issues": true,
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
		Credential: validCred("lin_api_test"),
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

// ── Authorization header (no Bearer prefix) ──────────────────────────────────

func TestTokenTransport_NoBearerPrefix(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"teams": map[string]any{"nodes": []any{}},
			},
		})
	}))
	defer srv.Close()

	client := linearClient("lin_api_mykey")
	req, _ := http.NewRequest("POST", srv.URL, nil)
	client.Do(req)

	// Linear uses bare token, not "Bearer lin_api_mykey"
	if gotAuth != "lin_api_mykey" {
		t.Errorf("expected 'lin_api_mykey', got %q", gotAuth)
	}
}

// ── GraphQL error handling ────────────────────────────────────────────────────

func TestGraphqlDo_GraphQLError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"errors": []map[string]any{
				{"message": "Authentication required"},
			},
		})
	}))
	defer srv.Close()

	client := linearClient("bad-token")
	client.Transport = &rewriteTransport{base: client.Transport, target: srv.URL}
	var out struct{}
	err := graphqlDo(context.Background(), client, "query { viewer { id } }", nil, &out)
	if err == nil {
		t.Fatal("expected error for GraphQL error response")
	}
	if got := err.Error(); got != "graphql error: Authentication required" {
		t.Errorf("expected 'graphql error: Authentication required', got %q", got)
	}
}

func TestGraphqlDo_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	client := linearClient("bad-token")
	client.Transport = &rewriteTransport{base: client.Transport, target: srv.URL}
	var out struct{}
	err := graphqlDo(context.Background(), client, "query { viewer { id } }", nil, &out)
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
}

// ── listTeams with test server ────────────────────────────────────────────────

func TestListTeams_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"teams": map[string]any{
					"nodes": []map[string]any{
						{"id": "team-1", "name": "Engineering", "key": "ENG"},
						{"id": "team-2", "name": "Design", "key": "DES"},
					},
				},
			},
		})
	}))
	defer srv.Close()

	a := New()
	client := linearClient("lin_api_test")
	client.Transport = &rewriteTransport{base: client.Transport, target: srv.URL}

	result, err := a.listTeams(context.Background(), client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Summary != "2 team(s)" {
		t.Errorf("expected '2 team(s)', got %q", result.Summary)
	}
}

// ── Required params validation ────────────────────────────────────────────────

func TestGetIssue_MissingIssueID(t *testing.T) {
	a := New()
	_, err := a.getIssue(context.Background(), &http.Client{}, map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing issue_id")
	}
}

func TestCreateIssue_MissingTitle(t *testing.T) {
	a := New()
	_, err := a.createIssue(context.Background(), &http.Client{}, map[string]any{
		"team_id": "team-1",
	})
	if err == nil {
		t.Fatal("expected error for missing title")
	}
}

func TestCreateIssue_MissingTeamID(t *testing.T) {
	a := New()
	_, err := a.createIssue(context.Background(), &http.Client{}, map[string]any{
		"title": "Bug fix",
	})
	if err == nil {
		t.Fatal("expected error for missing team_id")
	}
}

func TestUpdateIssue_MissingIssueID(t *testing.T) {
	a := New()
	_, err := a.updateIssue(context.Background(), &http.Client{}, map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing issue_id")
	}
}

func TestAddComment_MissingParams(t *testing.T) {
	a := New()
	_, err := a.addComment(context.Background(), &http.Client{}, map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing issue_id and body")
	}
}

func TestSearchIssues_MissingQuery(t *testing.T) {
	a := New()
	_, err := a.searchIssues(context.Background(), &http.Client{}, map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing query")
	}
}

// ── commaPrefix helper ────────────────────────────────────────────────────────

func TestCommaPrefix(t *testing.T) {
	if got := commaPrefix(""); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
	if got := commaPrefix("foo: bar"); got != ", foo: bar" {
		t.Errorf("expected ', foo: bar', got %q", got)
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
