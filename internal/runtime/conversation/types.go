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
)

type ToolUse struct {
	ID    string
	Index int
	Name  string
	Input json.RawMessage
}
