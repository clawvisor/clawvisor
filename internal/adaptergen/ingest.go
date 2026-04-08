package adaptergen

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SourceType identifies the kind of API source material.
type SourceType string

const (
	SourceMCP     SourceType = "mcp"
	SourceOpenAPI SourceType = "openapi"
	SourceDocs    SourceType = "docs"
)

// Source represents the input material for adapter generation.
type Source struct {
	Type    SourceType `json:"source_type"`
	Content string     `json:"source"` // raw content (JSON, YAML, or text)

	// ServiceID override — if empty, the LLM derives it from the source.
	ServiceID string `json:"service_id,omitempty"`

	// AuthType override — if provided, forces a specific auth type.
	AuthType string `json:"auth_type,omitempty"` // "api_key", "oauth2", "basic", "none"
}

// MCPToolDef is a minimal MCP tool definition for ingestion.
type MCPToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// buildGenerationPrompt constructs the user message for the generation LLM call
// from the source material and any overrides.
func buildGenerationPrompt(src Source) (string, error) {
	if strings.TrimSpace(src.Content) == "" {
		return "", fmt.Errorf("source content is empty")
	}

	var prefix string
	switch src.Type {
	case SourceMCP:
		prefix = mcpIngestPrompt
	case SourceOpenAPI:
		prefix = openAPIIngestPrompt
	case SourceDocs:
		prefix = docsIngestPrompt
	default:
		return "", fmt.Errorf("unsupported source type: %q", src.Type)
	}

	var sb strings.Builder
	sb.WriteString(prefix)
	sb.WriteString("\n")

	if src.ServiceID != "" {
		fmt.Fprintf(&sb, "\nUse service ID: %q\n", src.ServiceID)
	}
	if src.AuthType != "" {
		fmt.Fprintf(&sb, "\nUse authentication type: %q\n", src.AuthType)
	}

	sb.WriteString("\n--- SOURCE MATERIAL ---\n")
	sb.WriteString(src.Content)

	return sb.String(), nil
}

// buildRiskPrompt constructs the user message for the risk classification LLM call.
// It takes the generated YAML (with UNCLASSIFIED risk placeholders) and extracts
// the action metadata needed for independent risk assessment.
func buildRiskPrompt(yamlContent string) string {
	var sb strings.Builder
	sb.WriteString("Classify the risk for each action in this adapter definition:\n\n")
	sb.WriteString(yamlContent)
	return sb.String()
}
