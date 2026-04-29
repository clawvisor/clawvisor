package tasks

import (
	"encoding/json"

	"github.com/clawvisor/clawvisor/pkg/store"
)

type ExpectedTool struct {
	ToolName   string         `json:"tool_name"`
	Why        string         `json:"why"`
	InputShape map[string]any `json:"input_shape,omitempty"`
	InputRegex string         `json:"input_regex,omitempty"`
}

type ExpectedEgress struct {
	Host            string         `json:"host"`
	Why             string         `json:"why"`
	Method          string         `json:"method,omitempty"`
	Path            string         `json:"path,omitempty"`
	PathRegex       string         `json:"path_regex,omitempty"`
	QueryShape      map[string]any `json:"query_shape,omitempty"`
	BodyShape       map[string]any `json:"body_shape,omitempty"`
	Headers         map[string]any `json:"headers,omitempty"`
	CredentialAlias string         `json:"credential_alias,omitempty"`
}

type Envelope struct {
	ExpectedTools          []ExpectedTool
	ExpectedEgress         []ExpectedEgress
	IntentVerificationMode string
	ExpectedUse            string
	SchemaVersion          int
}

func EnvelopeFromTask(task *store.Task) (Envelope, error) {
	env := Envelope{
		IntentVerificationMode: task.IntentVerificationMode,
		ExpectedUse:            task.ExpectedUse,
		SchemaVersion:          task.SchemaVersion,
	}
	if task.SchemaVersion == 0 {
		env.SchemaVersion = 1
	}
	if len(task.ExpectedTools) > 0 {
		if err := json.Unmarshal(task.ExpectedTools, &env.ExpectedTools); err != nil {
			return Envelope{}, err
		}
	}
	if len(task.ExpectedEgress) > 0 {
		if err := json.Unmarshal(task.ExpectedEgress, &env.ExpectedEgress); err != nil {
			return Envelope{}, err
		}
	}
	return env, nil
}
