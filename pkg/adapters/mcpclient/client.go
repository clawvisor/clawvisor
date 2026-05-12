package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Client is a JSON-RPC 2.0 MCP client. A single read pump owns the underlying
// stream; per-call requests register a response channel keyed by request ID,
// and the pump dispatches each response to the right caller. This avoids the
// goroutine-leak / concurrent-bufio.Reader hazards of a "spawn a reader per
// call" design and correctly matches responses by id (so server log lines or
// notifications cannot be mistaken for the next call's response).
type Client struct {
	enc     *json.Encoder
	mu      sync.Mutex
	id      int
	pending map[int]chan *rpcResponse
	closed  bool
	closeCh chan struct{}
}

// New wraps an io.ReadWriter (subprocess stdio, or an in-process pipe pair)
// and starts the background reader. Caller is responsible for closing the
// underlying connection — that closure ends the read pump and unblocks any
// pending callers with an error.
func New(rw io.ReadWriter) *Client {
	c := &Client{
		enc:     json.NewEncoder(rw),
		pending: make(map[int]chan *rpcResponse),
		closeCh: make(chan struct{}),
	}
	go c.readLoop(rw)
	return c
}

// readLoop owns the bufio.Reader and routes each response to its caller.
// Lines that don't parse as a JSON-RPC response with an int id are dropped
// (notifications, server diagnostics, etc.). When the stream ends, all
// pending callers are unblocked with io.EOF.
func (c *Client) readLoop(r io.Reader) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var resp rpcResponse
			if jerr := json.Unmarshal(line, &resp); jerr == nil && resp.ID > 0 {
				c.mu.Lock()
				ch, ok := c.pending[resp.ID]
				delete(c.pending, resp.ID)
				c.mu.Unlock()
				if ok {
					ch <- &resp
				}
				// Unmatched id — likely a stale response after a context
				// cancellation. Drop it.
			}
		}
		if err != nil {
			break
		}
	}
	c.mu.Lock()
	c.closed = true
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	close(c.closeCh)
}

// Initialize performs the MCP handshake. Must be called before ListTools or CallTool.
func (c *Client) Initialize(ctx context.Context) error {
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
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("mcpclient: closed")
	}
	return c.enc.Encode(rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"})
}

// ListTools returns the set of tools advertised by the server.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var resp struct {
		Tools []Tool `json:"tools"`
	}
	if err := c.call(ctx, "tools/list", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Tools, nil
}

// CallTool invokes a tool by name with arbitrary JSON arguments.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (*ToolResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	params, err := json.Marshal(map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal tools/call params: %w", err)
	}
	var result ToolResult
	if err := c.call(ctx, "tools/call", params, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Client) call(ctx context.Context, method string, params json.RawMessage, out any) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("mcpclient: closed")
	}
	c.id++
	id := c.id
	ch := make(chan *rpcResponse, 1)
	c.pending[id] = ch
	req := rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	err := c.enc.Encode(req)
	c.mu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("write %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		// Stop waiting; drop the channel from the pending map. If the
		// response arrives later, readLoop discards it (no matching id in
		// pending). The reader goroutine is unaffected — it owns the stream
		// across the lifetime of the client, not per-call.
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case <-c.closeCh:
		return io.EOF
	case resp, ok := <-ch:
		if !ok {
			return io.EOF
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
}
