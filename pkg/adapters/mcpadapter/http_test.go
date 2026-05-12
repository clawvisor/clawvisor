package mcpadapter_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/adapters/mcpadapter"
	"github.com/clawvisor/clawvisor/pkg/adapters/mcpclient"
)

// mcpHTTPMock is a tiny in-process MCP server that speaks streamable HTTP.
// Routes initialize / tools/list / tools/call. Asserts the auth header is
// present for non-initialize methods so the test catches credential
// propagation regressions.
type mcpHTTPMock struct {
	sessionID  string
	expectAuth string
	mu         sync.Mutex
	initialized bool
}

func (m *mcpHTTPMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id,omitempty"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	// Auth check for anything that's not initialize/initialized.
	if req.Method != "initialize" && req.Method != "notifications/initialized" {
		if got := r.Header.Get("Authorization"); got != m.expectAuth {
			http.Error(w, "auth: got "+got+" want "+m.expectAuth, http.StatusUnauthorized)
			return
		}
		if got := r.Header.Get("Mcp-Session-Id"); got != m.sessionID {
			http.Error(w, "session: got "+got+" want "+m.sessionID, http.StatusBadRequest)
			return
		}
	}

	writeResp := func(result any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  result,
		})
	}

	switch req.Method {
	case "initialize":
		m.mu.Lock()
		m.initialized = true
		m.mu.Unlock()
		w.Header().Set("Mcp-Session-Id", m.sessionID)
		writeResp(map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "mock-http-mcp"},
		})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		writeResp(map[string]any{
			"tools": []mcpclient.Tool{
				{Name: "search", Description: "Search.", Annotations: map[string]any{"readOnlyHint": true}},
				{Name: "whoami", Description: "Identity.", Annotations: map[string]any{"readOnlyHint": true}},
			},
		})
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		var data any
		switch p.Name {
		case "search":
			q, _ := p.Arguments["query"].(string)
			data = map[string]any{"results": []map[string]any{{"id": "remote-1", "title": "match for " + q}}}
		case "whoami":
			data = map[string]any{"workspace_name": "RemoteWorkspace"}
		default:
			http.Error(w, "unknown tool", http.StatusBadRequest)
			return
		}
		payload, _ := json.Marshal(data)
		writeResp(mcpclient.ToolResult{
			Content: []mcpclient.ToolContent{{Type: "text", Text: string(payload)}},
		})
	default:
		http.Error(w, "method not found", http.StatusNotFound)
	}
}

// TestHTTPTransport_EndToEnd exercises an MCP server reached over HTTP —
// no subprocess, no node, no npm. This is the production-shape transport
// for vendor-hosted MCP servers.
func TestHTTPTransport_EndToEnd(t *testing.T) {
	mock := &mcpHTTPMock{sessionID: "sess-abc", expectAuth: "Bearer secret_test"}
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)

	spec := mcpadapter.Spec{}
	spec.Service.ID = "remote-mcp"
	spec.Service.DisplayName = "Remote (HTTP)"
	spec.MCP.Transport = "http"
	spec.MCP.Endpoint = srv.URL
	spec.MCP.Whoami = &mcpadapter.WhoamiSpec{Tool: "whoami", Field: "workspace_name"}

	transport := &mcpadapter.HTTPTransport{
		Endpoint: srv.URL,
	}
	adapter := mcpadapter.FromSpec(spec, transport)

	credJSON := []byte(`{"token":"secret_test"}`)

	// Discovery against the remote server.
	tools, err := adapter.DiscoverTools(context.Background(), credJSON)
	if err != nil {
		t.Fatalf("DiscoverTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools from remote server, got %d", len(tools))
	}

	// Identity via whoami over HTTP.
	identity, err := adapter.FetchIdentity(context.Background(), credJSON, nil)
	if err != nil {
		t.Fatalf("FetchIdentity: %v", err)
	}
	// Whoami results flow through normalizeAlias, which lowercases everything
	// and strips chars outside the alias set. RemoteWorkspace → remoteworkspace.
	if identity != "remoteworkspace" {
		t.Fatalf("expected normalized identity from whoami, got %q", identity)
	}

	// Tool execution.
	perUser := adapter.ForUser(tools)
	result, err := perUser.Execute(context.Background(), adapters.Request{
		Action:     "search",
		Params:     map[string]any{"query": "remote"},
		Credential: credJSON,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	data, _ := result.Data.(map[string]any)
	results, _ := data["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("expected results from remote search, got %#v", data)
	}
	first, _ := results[0].(map[string]any)
	if title, _ := first["title"].(string); !strings.Contains(title, "remote") {
		t.Fatalf("expected query to flow through HTTP, got title=%q", title)
	}
}

// TestHTTPTransport_AuthHeaderMissing proves credential propagation reaches
// the HTTP header: a request without a valid Bearer token gets rejected by
// the mock server.
func TestHTTPTransport_AuthHeaderMissing(t *testing.T) {
	mock := &mcpHTTPMock{sessionID: "sess-xyz", expectAuth: "Bearer secret_correct"}
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)

	spec := mcpadapter.Spec{}
	spec.Service.ID = "remote-mcp"
	spec.MCP.Transport = "http"
	spec.MCP.Endpoint = srv.URL
	adapter := mcpadapter.FromSpec(spec, &mcpadapter.HTTPTransport{Endpoint: srv.URL})

	_, err := adapter.DiscoverTools(context.Background(), []byte(`{"token":"wrong"}`))
	if err == nil {
		t.Fatal("expected auth error from remote server when token is wrong")
	}
	if !strings.Contains(err.Error(), "401") && !strings.Contains(err.Error(), "auth") {
		t.Fatalf("expected auth/401 error, got: %v", err)
	}
}
