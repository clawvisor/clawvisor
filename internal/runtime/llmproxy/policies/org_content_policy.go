package policies

import (
	"context"
	"encoding/json"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/orggov"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// OrgContentPolicy invokes the cloud callback to scan the request body
// for matches against admin-authored content patterns. On block, the
// admin-authored block_message (or fallback) is used as the rejection
// reason — the host handler surfaces it in the 403 response body so
// the end user sees actionable guidance.
type OrgContentPolicy struct {
	callbacks     orggov.Callbacks
	orgIDForAgent func(ctx context.Context, agentID string) string
}

func NewOrgContentPolicy(callbacks orggov.Callbacks, orgIDForAgent func(ctx context.Context, agentID string) string) *OrgContentPolicy {
	return &OrgContentPolicy{callbacks: callbacks, orgIDForAgent: orgIDForAgent}
}

func (OrgContentPolicy) Name() string { return "org_content_policy" }

// Preprocess extracts user-message text from the request body and runs
// the cloud callback. block-action match → Deny with blockMessage as
// reason. flag-action match → Allow with audit-trail entry.
func (p *OrgContentPolicy) Preprocess(ctx context.Context, req pipeline.ReadOnlyRequest, _ pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	if p == nil || p.callbacks.ScanContentPolicy == nil {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeAllow}, nil
	}
	orgID := ""
	if p.orgIDForAgent != nil {
		orgID = p.orgIDForAgent(ctx, req.AgentID())
	}
	if orgID == "" {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeAllow}, nil
	}
	content := extractScanContent(req)
	if content == "" {
		return pipeline.RequestVerdict{Outcome: pipeline.OutcomeAllow}, nil
	}
	allow, blockMessage, reason, flagged := p.callbacks.ScanContentPolicy(ctx, orgID, content)
	if allow {
		if len(flagged) > 0 && p.callbacks.RecordViolation != nil {
			p.callbacks.RecordViolation(ctx, orggov.ViolationEvent{
				OrgID:       orgID,
				UserID:      req.UserID(),
				AgentID:     req.AgentID(),
				PolicyKind:  "content_policy",
				ActionTaken: "flagged",
				Detail:      reason,
			})
		}
		return pipeline.RequestVerdict{
			Outcome: pipeline.OutcomeAllow,
			AuditParams: map[string]any{
				"content_policy_flagged": flagged,
			},
		}, nil
	}
	if p.callbacks.RecordViolation != nil {
		p.callbacks.RecordViolation(ctx, orggov.ViolationEvent{
			OrgID:       orgID,
			UserID:      req.UserID(),
			AgentID:     req.AgentID(),
			PolicyKind:  "content_policy",
			ActionTaken: "blocked",
			Detail:      reason,
		})
	}
	// Use the admin-authored block_message if set, else a generic string.
	denyReason := blockMessage
	if denyReason == "" {
		denyReason = "blocked by content policy: " + reason
	}
	return pipeline.RequestVerdict{
		Outcome: pipeline.OutcomeDeny,
		Reason:  denyReason,
		AuditParams: map[string]any{
			"org_content_policy_block": true,
			"reason":                   reason,
		},
	}, nil
}

// extractScanContent returns a concatenation of user-message text from
// the request body. Supports:
//
//   - Anthropic Messages and OpenAI Chat Completions (`messages: [{role,
//     content}]`).
//   - OpenAI Responses (top-level `input` as string OR array of typed
//     parts, plus top-level `instructions` system-equivalent).
//   - Legacy completions API (top-level `prompt`).
//
// Without the Responses-shape branch, every request to /v1/responses
// would bypass the content policy because the Chat Completions
// extractor doesn't know to look at top-level input/instructions.
// Opaque blobs (base64 images, tool definitions) are skipped.
// Best-effort — returns "" when the body doesn't parse.
func extractScanContent(req pipeline.ReadOnlyRequest) string {
	body := req.RawBody()
	if len(body) == 0 {
		return ""
	}
	var probe struct {
		Messages     []json.RawMessage `json:"messages"`
		Prompt       string            `json:"prompt"` // legacy completions API
		Input        json.RawMessage   `json:"input"`        // OpenAI Responses
		Instructions string            `json:"instructions"` // OpenAI Responses (system-equivalent)
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	out := probe.Prompt
	if probe.Instructions != "" {
		out += "\n" + probe.Instructions
	}
	// OpenAI Responses: top-level `input` is either a bare string OR an
	// array of typed message parts. Scan both shapes — same role
	// filtering as messages[]: user + system contribute, assistant is
	// the model's own output and isn't governed.
	if len(probe.Input) > 0 {
		var s string
		if err := json.Unmarshal(probe.Input, &s); err == nil {
			out += "\n" + s
		} else {
			var items []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
				Type    string          `json:"type"`
				Text    string          `json:"text"`
			}
			if err := json.Unmarshal(probe.Input, &items); err == nil {
				for _, it := range items {
					// Untyped "input_text" parts at the array root (rare,
					// but supported by the Responses schema).
					if it.Role == "" && (it.Type == "input_text" || it.Type == "text") && it.Text != "" {
						out += "\n" + it.Text
						continue
					}
					if it.Role != "user" && it.Role != "system" && it.Role != "developer" {
						continue
					}
					out += "\n" + extractMessageContentText(it.Content)
				}
			}
		}
	}
	for _, m := range probe.Messages {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(m, &msg); err != nil {
			continue
		}
		if msg.Role != "user" && msg.Role != "system" {
			// We scan user + system content. assistant content is the
			// model's own output and isn't governed by content policy.
			continue
		}
		out += "\n" + extractMessageContentText(msg.Content)
	}
	return out
}

// extractMessageContentText flattens a message-content field that may
// be a bare string OR an array of typed parts ({type: "text"|"input_text",
// text: "..."}). Returns "" when the shape doesn't parse — best-effort.
func extractMessageContentText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	var arr []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &arr); err != nil {
		return ""
	}
	var out string
	for _, p := range arr {
		// "text" is the Chat Completions/Anthropic shape; "input_text" is
		// the Responses shape. "" defaults to text for forgiveness.
		if p.Type == "text" || p.Type == "input_text" || p.Type == "" {
			if out != "" {
				out += "\n"
			}
			out += p.Text
		}
	}
	return out
}
