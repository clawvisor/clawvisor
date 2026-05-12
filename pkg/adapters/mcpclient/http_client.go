package mcpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// readMCPBody parses a single JSON-RPC response from either an
// application/json body (single object) or a text/event-stream body
// (one or more SSE events, where the `data:` lines carry the payload).
// We're a synchronous client — we read until we have one complete
// JSON-RPC response payload and return.
func readMCPBody(body io.Reader, contentType string) ([]byte, error) {
	if strings.HasPrefix(strings.ToLower(contentType), "text/event-stream") {
		return readSSEResponse(body)
	}
	return io.ReadAll(io.LimitReader(body, 4*1024*1024))
}

// readSSEResponse consumes an SSE stream and returns the JSON payload from
// the first `data:` line(s) it sees. Subsequent events are ignored — we
// expect one JSON-RPC response per call.
func readSSEResponse(body io.Reader) ([]byte, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var dataLines []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			// Blank line ends an event. If we collected data, we're done.
			if len(dataLines) > 0 {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		// Other SSE field lines (event:, id:, retry:) are ignored.
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return nil, err
	}
	if len(dataLines) == 0 {
		return nil, fmt.Errorf("sse: no data lines in response")
	}
	return []byte(strings.Join(dataLines, "\n")), nil
}

// HTTPClient speaks MCP "streamable HTTP" — every JSON-RPC request is a
// POST to the configured endpoint; the response body carries one JSON-RPC
// response back. Sessions are tracked via the standard Mcp-Session-Id header
// (set by the server on Initialize, sent back on every subsequent call).
//
// Streaming responses (SSE) are not supported in this prototype — the client
// expects a single JSON response per request. That's sufficient for
// initialize, tools/list, and synchronous tools/call without progress
// notifications, which is what the gateway exercises today.
type HTTPClient struct {
	endpoint  string
	headers   map[string]string
	http      *http.Client
	id        atomic.Int64
	mu        sync.Mutex
	sessionID string
}

// NewHTTP builds an HTTP-transport MCP client. The headers map is sent on
// every request (typically containing the Authorization header).
func NewHTTP(endpoint string, headers map[string]string, httpClient *http.Client) *HTTPClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	hcopy := make(map[string]string, len(headers))
	for k, v := range headers {
		hcopy[k] = v
	}
	return &HTTPClient{endpoint: endpoint, headers: hcopy, http: httpClient}
}

func (c *HTTPClient) Initialize(ctx context.Context) error {
	params, _ := json.Marshal(map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "clawvisor-mcpclient",
			"version": "0.1.0",
		},
	})
	var raw json.RawMessage
	if err := c.call(ctx, "initialize", params, &raw); err != nil {
		return err
	}
	// Send the initialized notification (no id → no response expected).
	// Some servers require it before tools/* will work; if the transport
	// or server rejects it, surface the error rather than silently
	// continuing with a half-initialized session.
	if _, err := c.do(ctx, rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"}); err != nil {
		return fmt.Errorf("notifications/initialized: %w", err)
	}
	return nil
}

func (c *HTTPClient) ListTools(ctx context.Context) ([]Tool, error) {
	var resp struct {
		Tools []Tool `json:"tools"`
	}
	if err := c.call(ctx, "tools/list", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Tools, nil
}

func (c *HTTPClient) CallTool(ctx context.Context, name string, args map[string]any) (*ToolResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	params, err := json.Marshal(map[string]any{"name": name, "arguments": args})
	if err != nil {
		return nil, fmt.Errorf("marshal tools/call params: %w", err)
	}
	var result ToolResult
	if err := c.call(ctx, "tools/call", params, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// Close is a no-op — HTTP transport has no persistent connection to release.
// Sessions on the server side time out on their own schedule.
func (c *HTTPClient) Close() error { return nil }

func (c *HTTPClient) call(ctx context.Context, method string, params json.RawMessage, out any) error {
	id := c.id.Add(1)
	req := rpcRequest{JSONRPC: "2.0", ID: int(id), Method: method, Params: params}
	resp, err := c.do(ctx, req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if resp == nil {
		return nil // notification path
	}
	// JSON-RPC 2.0 §5: response id MUST match the request id. Most
	// transports preserve this naturally, but a misbehaving proxy or a
	// server that returns a stray response from a different in-flight
	// request would otherwise be silently mis-attributed.
	if int64(resp.ID) != id {
		return fmt.Errorf("%s: response id mismatch (got %d, want %d)", method, resp.ID, id)
	}
	if resp.Error != nil {
		return fmt.Errorf("%s: rpc error %d: %s", method, resp.Error.Code, resp.Error.Message)
	}
	if out != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("decode %s result: %w", method, err)
		}
	}
	return nil
}

// do issues one POST. Notifications (req.ID == 0) return (nil, nil) on
// success. Other requests decode and return the response.
func (c *HTTPClient) do(ctx context.Context, rpcReq rpcRequest) (*rpcResponse, error) {
	body, _ := json.Marshal(rpcReq)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range c.headers {
		httpReq.Header.Set(k, v)
	}
	c.mu.Lock()
	sess := c.sessionID
	c.mu.Unlock()
	if sess != "" {
		httpReq.Header.Set("Mcp-Session-Id", sess)
	}

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	// Capture session ID set by the server (typically on initialize).
	if sid := httpResp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.mu.Lock()
		c.sessionID = sid
		c.mu.Unlock()
	}

	if httpResp.StatusCode == http.StatusAccepted || httpResp.StatusCode == http.StatusNoContent {
		// Notification accepted, no body expected.
		return nil, nil
	}
	if httpResp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		return nil, fmt.Errorf("http %d: %s", httpResp.StatusCode, string(body))
	}
	// Notifications can still get a 200 with empty body; honor that too.
	if rpcReq.ID == 0 {
		return nil, nil
	}

	// MCP streamable HTTP responses can be either JSON or an SSE stream —
	// servers pick whichever fits the call (Notion uses SSE post-auth).
	// Dispatch on Content-Type.
	ctype := httpResp.Header.Get("Content-Type")
	payload, err := readMCPBody(httpResp.Body, ctype)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var resp rpcResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &resp, nil
}

var _ Caller = (*HTTPClient)(nil)
