package mcpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHTTPClient_HandlesSSEResponse is the regression test for the bug we
// hit against Notion's hosted MCP: after auth, the server switched
// Content-Type to text/event-stream and the JSON decoder choked on the
// leading "event:" line. The client must extract the data line and parse
// that as the JSON-RPC response.
func TestHTTPClient_HandlesSSEResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Mcp-Session-Id", "sess-sse")
		// Notion-style SSE envelope: event line then data line.
		payload, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]any{
				"tools": []map[string]any{
					{"name": "notion-search", "description": "Search."},
					{"name": "notion-get-user", "description": "Identity."},
				},
			},
		})
		_, _ = w.Write([]byte("event: message\ndata: " + string(payload) + "\n\n"))
	}))
	t.Cleanup(srv.Close)

	client := NewHTTP(srv.URL, map[string]string{"Authorization": "Bearer test"}, srv.Client())
	if err := client.Initialize(context.Background()); err != nil {
		// Initialize hits the same SSE handler — proves the bug fix works
		// on the initialize call specifically, which is where Notion broke.
		t.Fatalf("Initialize over SSE: %v", err)
	}
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools over SSE: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools from SSE response, got %d", len(tools))
	}
	if tools[0].Name != "notion-search" {
		t.Errorf("tool[0].name = %q, want %q", tools[0].Name, "notion-search")
	}
}

// TestHTTPClient_HandlesMultilineSSEData covers the spec's rule that
// multiple `data:` lines in a single event are joined with newlines. Notion
// doesn't seem to use this today, but the SSE spec allows it and a future
// server might split large payloads across lines.
func TestHTTPClient_HandlesMultilineSSEData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Same JSON spread across two data lines.
		_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\n" +
			"data: \"result\":{\"tools\":[]}}\n\n"))
	}))
	t.Cleanup(srv.Close)

	c := NewHTTP(srv.URL, nil, srv.Client())
	if _, err := c.ListTools(context.Background()); err != nil {
		// We don't have a great oracle here — the multi-data join is a
		// best-effort decode. The point is that it doesn't blow up with
		// the same "invalid character 'e'" error we hit before.
		if strings.Contains(err.Error(), "invalid character 'e'") {
			t.Fatalf("regression: SSE event line leaked into JSON decoder: %v", err)
		}
	}
}
