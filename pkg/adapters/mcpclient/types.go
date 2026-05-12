// Package mcpclient is a minimal JSON-RPC 2.0 client for the Model Context Protocol.
// Two transports are provided: line-delimited JSON over an io.ReadWriter (used for
// stdio subprocesses and in-process pipes) and HTTP POST against an MCP "streamable
// HTTP" endpoint (used for remote, vendor-hosted MCP servers).
package mcpclient

import (
	"context"
	"encoding/json"
)

const protocolVersion = "2025-06-18"

// Caller is the common surface every MCP client transport exposes. The
// MCPAdapter and the activation-time tool-discovery code program against this
// interface so stdio and HTTP transports are interchangeable.
type Caller interface {
	Initialize(ctx context.Context) error
	ListTools(ctx context.Context) ([]Tool, error)
	CallTool(ctx context.Context, name string, args map[string]any) (*ToolResult, error)
	Close() error
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Tool is the subset of the MCP tool definition this client cares about.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	// Annotations carries the standard MCP tool hints (readOnlyHint,
	// destructiveHint, idempotentHint, openWorldHint). The gateway uses these
	// to derive risk classification — replacing the per-adapter Risk YAML.
	Annotations map[string]any `json:"annotations,omitempty"`
}

// ToolResult is the response from tools/call.
type ToolResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}
