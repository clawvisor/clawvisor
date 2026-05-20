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

type RequiredCredential struct {
	VaultItemID     string `json:"vault_item_id,omitempty"`
	VaultItemHandle string `json:"vault_item_handle,omitempty"`
	Why             string `json:"why"`
}

type Envelope struct {
	ExpectedTools          []ExpectedTool
	ExpectedEgress         []ExpectedEgress
	RequiredCredentials    []RequiredCredential
	IntentVerificationMode string
	ExpectedUse            string
	SchemaVersion          int
}

// TaskCreateRequest is the parsed body of `POST /api/control/tasks` (or
// equivalently `POST /api/tasks`). The full validating handler lives in
// internal/api/handlers; this lighter shape is used by the lite-proxy's
// inline task-approval flow to inspect a model-emitted task definition
// and (on approval) hand the same payload back to the task-creation
// helper.
//
// Field tags match the wire format. The runtime/tasks package lives
// outside internal/api/handlers to avoid an import cycle between the
// llm-proxy and the handlers package.
type TaskCreateRequest struct {
	Purpose                string               `json:"purpose"`
	AuthorizedActions      []map[string]any     `json:"authorized_actions,omitempty"`
	PlannedCalls           []map[string]any     `json:"planned_calls,omitempty"`
	ExpectedTools          []ExpectedTool       `json:"expected_tools,omitempty"`
	ExpectedEgress         []ExpectedEgress     `json:"expected_egress,omitempty"`
	RequiredCredentials    []RequiredCredential `json:"required_credentials,omitempty"`
	IntentVerificationMode string               `json:"intent_verification_mode,omitempty"`
	ExpectedUse            string               `json:"expected_use,omitempty"`
	SchemaVersion          int                  `json:"schema_version,omitempty"`
	ExpiresInSeconds       int                  `json:"expires_in_seconds,omitempty"`
	CallbackURL            string               `json:"callback_url,omitempty"`
	Lifetime               string               `json:"lifetime,omitempty"`
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
	if len(task.RequiredCredentials) > 0 {
		if err := json.Unmarshal(task.RequiredCredentials, &env.RequiredCredentials); err != nil {
			return Envelope{}, err
		}
	}
	return env, nil
}
