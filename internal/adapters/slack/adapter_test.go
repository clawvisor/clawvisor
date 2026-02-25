package slack

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
	if err := a.ValidateCredential(validCred("xoxb-test-token")); err != nil {
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
	if err := a.ValidateCredential([]byte("not json")); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// ── ServiceID / SupportedActions ──────────────────────────────────────────────

func TestServiceID(t *testing.T) {
	a := New()
	if got := a.ServiceID(); got != "slack" {
		t.Fatalf("expected 'slack', got %q", got)
	}
}

func TestSupportedActions(t *testing.T) {
	a := New()
	actions := a.SupportedActions()
	expected := map[string]bool{
		"list_channels":  true,
		"get_channel":    true,
		"list_messages":  true,
		"send_message":   true,
		"search_messages": true,
		"list_users":     true,
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
	a := New()
	_, err := a.Execute(context.Background(), adapters.Request{
		Action:     "nonexistent",
		Params:     map[string]any{},
		Credential: validCred("xoxb-test"),
	})
	if err == nil {
		t.Fatal("expected error for unknown action")
	}
}

// ── OAuth stubs return nil/error ──────────────────────────────────────────────

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

// ── Slack ok:false error handling ─────────────────────────────────────────────

func TestListChannels_SlackErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Slack returns HTTP 200 with ok:false on errors
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"error": "invalid_auth",
		})
	}))
	defer srv.Close()

	// Temporarily override the API base for testing
	a := New()
	client := &http.Client{Transport: &testTransport{base: http.DefaultTransport, baseURL: srv.URL + "/"}}
	_, err := a.listChannels(context.Background(), client, map[string]any{})
	if err == nil {
		t.Fatal("expected error for ok:false response")
	}
	if got := err.Error(); got != "slack list_channels: invalid_auth" {
		t.Fatalf("expected 'slack list_channels: invalid_auth', got %q", got)
	}
}

func TestListChannels_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"channels": []map[string]any{
				{
					"id":          "C123",
					"name":        "general",
					"topic":       map[string]any{"value": "General chat"},
					"purpose":     map[string]any{"value": "Company-wide"},
					"num_members": 42,
					"is_private":  false,
					"is_archived": false,
				},
				{
					"id":          "C456",
					"name":        "random",
					"topic":       map[string]any{"value": ""},
					"purpose":     map[string]any{"value": "Random stuff"},
					"num_members": 30,
					"is_private":  false,
					"is_archived": false,
				},
			},
		})
	}))
	defer srv.Close()

	a := New()
	client := &http.Client{Transport: &testTransport{base: http.DefaultTransport, baseURL: srv.URL + "/"}}
	result, err := a.listChannels(context.Background(), client, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Summary != "2 channel(s)" {
		t.Errorf("expected '2 channel(s)', got %q", result.Summary)
	}
}

func TestSendMessage_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"channel": "C123",
			"ts":      "1234567890.123456",
		})
	}))
	defer srv.Close()

	a := New()
	client := &http.Client{Transport: &testTransport{base: http.DefaultTransport, baseURL: srv.URL + "/"}}
	result, err := a.sendMessage(context.Background(), client, map[string]any{
		"channel": "C123",
		"text":    "hello",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Summary != "Message sent to C123" {
		t.Errorf("expected 'Message sent to C123', got %q", result.Summary)
	}
}

func TestSendMessage_MissingParams(t *testing.T) {
	a := New()
	client := &http.Client{}
	_, err := a.sendMessage(context.Background(), client, map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing params")
	}
}

func TestSearchMessages_MissingQuery(t *testing.T) {
	a := New()
	client := &http.Client{}
	_, err := a.searchMessages(context.Background(), client, map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing query")
	}
}

// ── Auth header ───────────────────────────────────────────────────────────────

func TestTokenTransport_SetsAuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "channels": []any{}})
	}))
	defer srv.Close()

	client := slackClient("xoxb-my-token")
	req, _ := http.NewRequest("GET", srv.URL, nil)
	client.Do(req)

	if gotAuth != "Bearer xoxb-my-token" {
		t.Errorf("expected 'Bearer xoxb-my-token', got %q", gotAuth)
	}
}

// ── testTransport rewrites URLs to point at the test server ───────────────────

type testTransport struct {
	base    http.RoundTripper
	baseURL string // test server URL ending with "/"
}

func (t *testTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Rewrite the Slack API URL to the test server
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = req.URL.Host
	// Replace slack.com/api/ with test server
	path := req.URL.Path
	if len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}
	newURL := t.baseURL + path
	if req.URL.RawQuery != "" {
		newURL += "?" + req.URL.RawQuery
	}
	parsed, _ := req.URL.Parse(newURL)
	clone.URL = parsed
	return t.base.RoundTrip(clone)
}
