package mcp

import (
	"log"
	"sync"

	"github.com/clawvisor/clawvisor/skills"
)

// Prompt is an MCP prompt definition.
type Prompt struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// PromptMessage is a message in a prompt response.
type PromptMessage struct {
	Role    string      `json:"role"`
	Content ToolContent `json:"content"`
}

// promptDefs returns the static list of MCP prompts.
func promptDefs() []Prompt {
	return []Prompt{
		{
			Name:        "clawvisor-workflow",
			Description: "How to use Clawvisor's task-scoped authorization, gateway requests, and service catalog. Read this before making your first request.",
		},
	}
}

// promptContent returns the messages for the named prompt.
func promptContent(name string) (string, []PromptMessage, bool) {
	switch name {
	case "clawvisor-workflow":
		return "Clawvisor workflow instructions", []PromptMessage{
			{
				Role:    "user",
				Content: ToolContent{Type: "text", Text: getWorkflowPrompt()},
			},
		}, true
	default:
		return "", nil, false
	}
}

var (
	workflowPromptOnce sync.Once
	workflowPromptText string
)

// getWorkflowPrompt lazily renders the MCP workflow prompt from the shared
// SKILL.md template.
func getWorkflowPrompt() string {
	workflowPromptOnce.Do(func() {
		text, err := skills.Render(skills.TargetMCP)
		if err != nil {
			log.Printf("WARNING: failed to render MCP workflow prompt: %v", err)
			workflowPromptText = "Error rendering workflow prompt. Use fetch_catalog, create_task, get_task, gateway_request, expand_task, and complete_task tools."
			return
		}
		workflowPromptText = text
	})
	return workflowPromptText
}
