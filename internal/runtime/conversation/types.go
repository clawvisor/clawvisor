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
	// ProviderGoogle is the Phase 6 third-provider stub. Defined here
	// so the value is recognized across the codebase, but the parser
	// and stream codec only exist at stub level — production traffic
	// for Google routes through provider-neutral paths until full
	// codec work lands.
	ProviderGoogle Provider = "google"
)

type ToolUse struct {
	ID    string
	Index int
	Name  string
	Input json.RawMessage
}
