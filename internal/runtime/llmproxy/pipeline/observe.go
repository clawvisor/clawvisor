package pipeline

import "context"

// Observe-posture enforcement downgrade (spec 02 §3).
//
// In the Observe posture the pipeline runs in full — inspection,
// attribution, audit, cost — but every ENFORCING verdict a policy
// returns (deny, approval hold / short-circuit, or a policy body
// rewrite) is recorded as an observation and then neutralized so the
// request/response proceeds exactly as if the policy had allowed it.
// Mechanical policies (parsing/sanitization, history strip, control-tool
// and notice injection, placeholder / credential resolution) are NOT
// enforcement and keep acting normally — otherwise byte-fidelity and
// prompt-cache warmth break. Classification is by policy Name(), not by
// outcome, because the same OutcomeRewrite can be a mechanical transform
// or a policy redaction.
//
// The mode is threaded through context so every runSinglePolicy call
// site inherits it from the request context without a signature change.

type observeModeCtxKey struct{}

// WithObserveMode marks ctx so RunPre / EvaluateToolUses downgrade
// enforcing verdicts to observations instead of enforcing them.
func WithObserveMode(ctx context.Context) context.Context {
	return context.WithValue(ctx, observeModeCtxKey{}, true)
}

// ObserveModeFromContext reports whether ctx carries the Observe-posture
// downgrade flag.
func ObserveModeFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(observeModeCtxKey{}).(bool)
	return v
}

// observeExemptPolicies lists the pre-phase policy Name() strings that
// STILL act in Observe mode because they are mechanical, not
// enforcement. Everything not in this set has its deny / short-circuit /
// body-rewrite downgraded to a recorded observation.
//
// Classification (spec 02 §3 deliverable — enumerated in the PR):
//
//	MECHANICAL (exempt — run normally):
//	  anthropic_sanitize     request parse/normalize; its "deny" is a
//	                         malformed-body client bug, not a verdict
//	  inbound_sanitize       strips rewriter transport details from history
//	  synthetic_history_strip prompt-cache-preserving history reconstruction
//	  secret_history_strip   history hygiene
//	  secret_rewrites        vault placeholder / credential resolution
//	  secret_decision        applies the user's chosen secret action
//	                         (allow_once/discard/vault/not_secret); a
//	                         control-flow reply-processor, not a verdict —
//	                         parallels approval_release / task_approval_reply.
//	                         Its enforcing counterpart is secret_hold below.
//	  control_notice         notice injection
//	  inline_task_augment    history augmentation
//	  approval_release       resolves an in-flight hold (no hold exists in observe)
//	  task_approval_reply    processes an approval reply (control-flow)
//	  control_tool_use       control-tool handling
//	  credential_rewrite     resolver/credential input rewrite
//	  pass_through           no-op
//	  script_session         cv-script session bookkeeping
//	  inspector              observation channel (never enforces on its own)
//
//	ENFORCING (downgraded in observe):
//	  org_model_policy, org_spend_cap_policy, org_content_policy
//	  inline_task_intercept  (approval hold short-circuit)
//	  task_scope, intent_verify, boundary_check, authorization,
//	  pending_approval_hold, secret_hold
var observeExemptPolicies = map[string]bool{
	"anthropic_sanitize":      true,
	"inbound_sanitize":        true,
	"synthetic_history_strip": true,
	"secret_history_strip":    true,
	"secret_rewrites":         true,
	"secret_decision":         true,
	"control_notice":          true,
	"inline_task_augment":     true,
	"approval_release":        true,
	"task_approval_reply":     true,
	"control_tool_use":        true,
	"credential_rewrite":      true,
	"pass_through":            true,
	"script_session":          true,
	"inspector":               true,
}

// observeExempt reports whether policyName still acts in Observe mode.
func observeExempt(policyName string) bool {
	return observeExemptPolicies[policyName]
}

// ObservedVerdict records one enforcing verdict that Observe mode
// downgraded to an observation, for the handler's audit row.
type ObservedVerdict struct {
	Policy  string
	Outcome string
	Reason  string
}
