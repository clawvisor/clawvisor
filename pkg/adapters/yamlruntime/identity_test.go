package yamlruntime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/adapters/yamldef"
)

func TestFetchIdentity_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "token ghp_test" {
			t.Errorf("expected 'token ghp_test', got %q", got)
		}
		json.NewEncoder(w).Encode(map[string]any{"login": "octocat", "id": 1})
	}))
	defer srv.Close()

	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{
			ID: "github",
			Identity: &yamldef.IdentityDef{
				Endpoint: "/user",
				Field:    "login",
			},
		},
		Auth: yamldef.AuthDef{
			Type:         "api_key",
			Header:       "Authorization",
			HeaderPrefix: "token ",
		},
		API:     yamldef.APIDef{BaseURL: srv.URL, Type: "rest"},
		Actions: map[string]yamldef.Action{},
	}

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatal(err)
	}

	identity, err := adapter.FetchIdentity(context.Background(), testCred("ghp_test"))
	if err != nil {
		t.Fatalf("FetchIdentity failed: %v", err)
	}
	if identity != "octocat" {
		t.Errorf("expected identity 'octocat', got %q", identity)
	}
}

func TestFetchIdentity_NestedField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"user": map[string]any{"email": "test@example.com"},
		})
	}))
	defer srv.Close()

	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{
			ID: "custom",
			Identity: &yamldef.IdentityDef{
				Endpoint: "/me",
				Field:    "user.email",
			},
		},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: "Bearer "},
		API:     yamldef.APIDef{BaseURL: srv.URL, Type: "rest"},
		Actions: map[string]yamldef.Action{},
	}

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatal(err)
	}

	identity, err := adapter.FetchIdentity(context.Background(), testCred("tok"))
	if err != nil {
		t.Fatalf("FetchIdentity failed: %v", err)
	}
	if identity != "test@example.com" {
		t.Errorf("expected 'test@example.com', got %q", identity)
	}
}

func TestFetchIdentity_NoIdentityDef(t *testing.T) {
	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{ID: "noid"},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: "Bearer "},
		API:     yamldef.APIDef{BaseURL: "http://unused", Type: "rest"},
		Actions: map[string]yamldef.Action{},
	}

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatal(err)
	}

	identity, err := adapter.FetchIdentity(context.Background(), testCred("tok"))
	if err != nil {
		t.Fatalf("FetchIdentity failed: %v", err)
	}
	if identity != "" {
		t.Errorf("expected empty identity, got %q", identity)
	}
}

func TestFetchIdentity_AbsoluteURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"name": "alice"})
	}))
	defer srv.Close()

	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{
			ID: "custom",
			Identity: &yamldef.IdentityDef{
				Endpoint: srv.URL + "/identity",
				Field:    "name",
			},
		},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: "Bearer "},
		API:     yamldef.APIDef{BaseURL: "http://should-not-be-used", Type: "rest"},
		Actions: map[string]yamldef.Action{},
	}

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatal(err)
	}

	identity, err := adapter.FetchIdentity(context.Background(), testCred("tok"))
	if err != nil {
		t.Fatalf("FetchIdentity failed: %v", err)
	}
	if identity != "alice" {
		t.Errorf("expected 'alice', got %q", identity)
	}
}

func TestFetchIdentity_GraphQLBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		// Verify the body was sent.
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("failed to decode body: %v", err)
		}
		if _, ok := body["query"]; !ok {
			t.Errorf("expected body to contain 'query' field")
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"viewer": map[string]any{"email": "dev@linear.app"},
			},
		})
	}))
	defer srv.Close()

	def := yamldef.ServiceDef{
		Service: yamldef.ServiceInfo{
			ID: "linear",
			Identity: &yamldef.IdentityDef{
				Endpoint: "/",
				Method:   "POST",
				Body:     `{"query": "{ viewer { email } }"}`,
				Field:    "data.viewer.email",
			},
		},
		Auth:    yamldef.AuthDef{Type: "api_key", Header: "Authorization", HeaderPrefix: ""},
		API:     yamldef.APIDef{BaseURL: srv.URL, Type: "graphql"},
		Actions: map[string]yamldef.Action{},
	}

	adapter, err := New(def, nil)
	if err != nil {
		t.Fatal(err)
	}

	identity, err := adapter.FetchIdentity(context.Background(), testCred("lin_api_test"))
	if err != nil {
		t.Fatalf("FetchIdentity failed: %v", err)
	}
	if identity != "dev@linear.app" {
		t.Errorf("expected 'dev@linear.app', got %q", identity)
	}
}
