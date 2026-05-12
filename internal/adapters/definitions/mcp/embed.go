// Package mcp embeds *.mcp.yaml definitions for MCP-backed adapters.
// Adding a new MCP-backed service means dropping a single YAML file in
// this directory — no Go code, no per-service adapter registration.
package mcp

import "embed"

//go:embed *.mcp.yaml
var FS embed.FS
