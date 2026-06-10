package conversation

import (
	"encoding/json"
	"time"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
	RoleSystem    Role = "system"
)

type Turn struct {
	Role      Role
	Content   string
	Timestamp time.Time
	ToolName  string
}

type Provider string

const (
	ProviderAnthropic Provider = "anthropic"
	ProviderOpenAI    Provider = "openai"
	// ProviderGoogle is defined here so the value is recognized across
	// the codebase. The parser and stream codec are still partial, so
	// production traffic for Google routes through provider-neutral
	// paths until full codec work lands.
	ProviderGoogle Provider = "google"
)

type ToolUse struct {
	ID    string
	Index int
	Name  string
	Input json.RawMessage
	// CvReason is the agent-supplied per-call rationale extracted from
	// the `cvreason` field of Input. The control notice instructs agents
	// to include cvreason on every tool_use; the parser strips it from
	// Input so it never reaches the client and stores it here for use
	// by intent verification. Empty when the agent omitted it.
	CvReason string
}
