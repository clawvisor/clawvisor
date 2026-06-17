// Package orggov provides the callback contract that the cloud
// governance control panel uses to enforce per-org guardrails on the
// LLM proxy path.
//
// The clawvisor binary itself never knows about org policies — it
// invokes the callbacks if configured and falls through to allow
// otherwise. The cloud package implements these callbacks against its
// org_model_policy / org_spend_cap / org_content_policy / org_task_policy
// tables.
//
// Each callback returns (allow, reason). Reason is recorded in the
// audit trail and (for the cloud impl) in the org_policy_violation
// log. Implementations MUST be safe for concurrent calls.
package orggov

import "context"

// Callbacks is the suite of per-org governance hooks the lite-proxy
// pipeline consults at request time. All fields are optional; nil
// callbacks degrade to "allow." The cloud package builds this struct
// from its govcache/snapshot loader at startup.
type Callbacks struct {
	// CheckModelPolicy returns (false, reason) when the model is
	// blocked by the org's allow/deny policy. The model identifier is
	// the canonical form (`<provider>/<rest>`, e.g. "openai/gpt-4o").
	CheckModelPolicy func(ctx context.Context, orgID, model string) (allow bool, reason string)

	// CheckSpendCap returns (false, reason) when the org has crossed
	// its hard-mode spend cap. Soft-mode crossings return (true, reason)
	// — the reason is logged but the request proceeds.
	CheckSpendCap func(ctx context.Context, orgID string) (allow bool, reason string)

	// ScanContentPolicy returns (false, reason) when the extracted
	// user-visible request text matches a block-mode content policy.
	// flag-mode matches return (true, reason). Content is the
	// canonical-extracted text from bodytransform; opaque blobs (base64
	// images, tool definition JSON) are NOT scanned.
	ScanContentPolicy func(ctx context.Context, orgID, content string) (allow bool, reason string, flagged []string)

	// RecordViolation persists a policy violation event. Called by
	// the lite-proxy policies whenever a check returns (allow=false)
	// or returns a non-empty reason in soft-mode. Failures are
	// best-effort — never block the request on logging.
	RecordViolation func(ctx context.Context, evt ViolationEvent)
}

// ViolationEvent is the shape passed to Callbacks.RecordViolation.
// Mirrors the cloud-side OrgPolicyViolation row but the lite-proxy
// doesn't depend on cloud types.
type ViolationEvent struct {
	OrgID       string
	UserID      string
	AgentID     string
	TaskID      string
	PolicyKind  string // "model_policy" | "spend_cap" | "content_policy"
	ActionTaken string // "blocked" | "flagged" | "warned"
	Detail      string
}
