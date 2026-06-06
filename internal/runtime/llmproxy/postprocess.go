package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// IntentVerifier matches the intent.Verifier contract. The lite-proxy
// declares its own narrow interface to avoid pulling the LLM provider
// dependency into this package.

// CredentialedRewriteRecoveryReason is the user-facing recovery
// message for credential-rewrite errors. Used by the
// policies.CredentialRewriteEvaluator on rewriter_error.
func CredentialedRewriteRecoveryReason(v inspector.Verdict, err error) string {
	if err == nil {
		return "Clawvisor: rewriter refused"
	}
	// Sentinel match — the inspector package owns the canonical error
	// value, so substring matching on err.Error() would silently break
	// if the message text ever changes. errors.Is is the durable
	// boundary.
	if errors.Is(err, inspector.ErrNoRewriter) {
		var b strings.Builder
		b.WriteString("Clawvisor: detected credentialed API access, but this tool shape cannot be rewritten. ")
		b.WriteString("Detected ")
		b.WriteString(firstNonEmpty(v.Method, "HTTP"))
		if v.Host != "" {
			b.WriteString(" ")
			b.WriteString(v.Host)
		}
		if v.Path != "" {
			b.WriteString(v.Path)
		}
		if len(v.CredentialLocations) > 0 || len(v.Placeholders) > 0 {
			b.WriteString(" using an autovault placeholder")
		}
		b.WriteString(". Recover by minting a script session: POST ")
		b.WriteString("https://" + ControlSyntheticHost + ControlSyntheticPath + "/autovault/script-session")
		// Build the example with placeholder text when the verdict's
		// host/method are unknown — otherwise the example would
		// render as `target_host, methods:[]` which isn't a valid
		// shape and would mislead the agent on the field format.
		host := v.Host
		if host == "" {
			host = "<target host>"
		}
		method := v.Method
		if method == "" {
			method = "GET"
		}
		b.WriteString(" with `{placeholder, target_host:\"")
		b.WriteString(host)
		b.WriteString("\", methods:[\"")
		b.WriteString(method)
		b.WriteString("\"], path_prefixes:[<service-specific prefix covering ")
		if v.Path != "" {
			b.WriteString(v.Path)
		} else {
			b.WriteString("the requests you are making")
		}
		b.WriteString(">], max_uses, ttl_seconds, why}` (hard limits: TTL ≤ 120s, max_uses ≤ 200, GET-only initially). ")
		b.WriteString("Then from your script call `base_url + <upstream path>` with `X-Clawvisor-Caller: Bearer <caller_token>` and `Authorization: Bearer <placeholder>` on each request. ")
		b.WriteString("See GET ")
		b.WriteString("https://" + ControlSyntheticHost + ControlSyntheticPath + "/autovault/script")
		b.WriteString(" for the full request shape and error recovery codes.")
		return b.String()
	}
	return "Clawvisor: rewriter refused — " + err.Error()
}

// coalesceFromCaptures builds the single PendingLiteApproval covering
// every tool_use in a turn. The first approval-needing capture becomes
// the primary (its decision context is mapped to the singular
// ToolUse/Inspector/Fingerprint/Reason fields the rest of the codebase
// already understands). PrimaryIndex records where the primary sat in
// the original turn, so AllHolds() — and the release path that emits
// from it — keep the model's tool_use order intact. Reordering would
// break dependent sequences like Bash producing stdout that a
// following WebFetch consumes.

// ApprovalPrompt renders the agent-facing message that substitutes for a
// paused tool call. When approvalID is non-empty, the InlineApprovalIDMarker
// footer is appended so subsequent turns can disambiguate which hold a bare
// "y"/"n" reply targets — important when one agent's transcript contains
// multiple pending prompts, or when several agents share a Clawvisor token
// and only the per-transcript marker reliably identifies the right hold.
func ApprovalPrompt(tu conversation.ToolUse, reason, approvalID string) string {
	preview := conversation.MakeToolInputPreview(tu.Input)
	var b strings.Builder
	b.WriteString("Clawvisor paused this tool call for approval.")
	if tu.Name != "" {
		b.WriteString("\n\nTool: `")
		b.WriteString(tu.Name)
		b.WriteString("`")
	}
	if reason != "" {
		b.WriteString("\nReason: ")
		b.WriteString(reason)
	}
	if preview != "" {
		b.WriteString("\nInput: ")
		b.WriteString(preview)
	}
	b.WriteString("\n\nReply `yes` or `y` to run this tool call, `no` or `n` to block it, or `task` to instruct the agent to include this in a task definition for approval.")
	b.WriteString(approvalIDFooter(approvalID))
	return b.String()
}

// DecisionIntentVerifierFor wraps a (possibly nil) IntentVerifier so
// runtimedecision.AuthorizationInput can consume it directly. The
// wrapper translates between the package-local IntentVerifyRequest /
// IntentVerdict types and runtimedecision's.
func DecisionIntentVerifierFor(v IntentVerifier) runtimedecision.IntentVerifier {
	return decisionIntentVerifier{inner: v}
}

type decisionIntentVerifier struct {
	inner IntentVerifier
}

func (v decisionIntentVerifier) Verify(ctx context.Context, req runtimedecision.IntentVerifyRequest) (*runtimedecision.IntentVerdict, error) {
	if v.inner == nil {
		return nil, nil
	}
	verdict, err := v.inner.Verify(ctx, IntentVerifyRequest{
		TaskPurpose: req.TaskPurpose,
		ExpectedUse: req.ExpectedUse,
		Service:     req.Service,
		Action:      req.Action,
		Params:      req.Params,
		Reason:      req.Reason,
		TaskID:      req.TaskID,
		Lenient:     req.Lenient,
	})
	if err != nil || verdict == nil {
		return nil, err
	}
	return &runtimedecision.IntentVerdict{
		Allow:       verdict.Allow,
		Explanation: verdict.Explanation,
	}, nil
}

// AuditAgentForCfg builds a minimal *store.Agent for the audit emitter
// from the postprocess config. The emitter only reads UserID and ID; we
// avoid an extra DB lookup by synthesizing the struct.
func AuditAgentForCfg(cfg PostprocessConfig) *store.Agent {
	if cfg.Audit == nil || cfg.AgentID == "" || cfg.AgentUserID == "" {
		return nil
	}
	return &store.Agent{ID: cfg.AgentID, UserID: cfg.AgentUserID}
}

// taskIDFromDecision extracts the matched task's ID from a decision,
// returning "" when there is no associated task. Trace-only helper.
func taskIDFromDecision(dec runtimedecision.AuthorizationDecision) string {
	if dec.Task == nil {
		return ""
	}
	return dec.Task.ID
}

// redactPlaceholderForReason returns the placeholder's prefix +
// length suffix — enough for operators to identify which placeholder
// was missing vs. which actually exists in the DB, without exposing
// the full random suffix in audit reasons that may surface in UIs or
// logs shared more broadly than the placeholder itself.
func redactPlaceholderForReason(ph string) string {
	const head = 18 // long enough to keep `autovault_<svc>_…`
	if len(ph) <= head {
		return ph
	}
	return ph[:head] + "…(" + strconv.Itoa(len(ph)) + " chars)"
}

// runIntentVerify runs LLM intent verification when the matched TaskAction
// opts in. Returns (reason, ok). ok=false on a refusal verdict; ok=true when
// the verifier was not consulted (off mode / missing dep) or returned Allow.
//
// Verification mode mapping (matches gateway behavior):
//   - "off"             → skip verification, allow.
//   - "lenient"         → call verifier with Lenient=true.
//   - "strict" / empty  → call verifier with Lenient=false.
//
// On verifier error we fail-open (audit will record), matching the gateway's
// behavior so a transient LLM outage doesn't block tool use; #37 will tighten
// this to fail-closed once the circuit breaker is in place.
// RunIntentVerify is the exported version of the per-task-scope intent
// check the credentialed path runs after TaskScope.Check confirms the
// scope match.
func RunIntentVerify(ctx context.Context, cfg PostprocessConfig, dec TaskScopeDecision, resolved ResolvedAction, tu conversation.ToolUse) (string, bool) {
	return runIntentVerify(ctx, cfg, dec, resolved, tu)
}

func runIntentVerify(ctx context.Context, cfg PostprocessConfig, dec TaskScopeDecision, resolved ResolvedAction, tu conversation.ToolUse) (string, bool) {
	if cfg.IntentVerifier == nil || dec.MatchedAction == nil {
		return "", true
	}
	mode := dec.MatchedAction.Verification
	if mode == "off" {
		return "", true
	}
	purpose := ""
	if dec.MatchedTask != nil {
		purpose = dec.MatchedTask.Purpose
	}
	var params map[string]any
	if len(tu.Input) > 0 {
		_ = json.Unmarshal(tu.Input, &params)
	}
	verdict, err := cfg.IntentVerifier.Verify(ctx, IntentVerifyRequest{
		TaskPurpose: purpose,
		ExpectedUse: dec.MatchedAction.ExpectedUse,
		Service:     resolved.ServiceID,
		Action:      resolved.ActionID,
		Params:      params,
		Reason:      "lite-proxy tool_use " + tu.Name,
		TaskID:      dec.TaskID,
		Lenient:     mode == "lenient",
	})
	if err != nil {
		// Circuit-breaker outage signals fail-closed: until the verifier
		// recovers, we refuse rather than allow tool_use without scope
		// validation. Other errors (timeouts, transient network failures)
		// fail-open to match the gateway's behavior so a single hiccup
		// doesn't strand the agent.
		if errors.Is(err, ErrCircuitOpen) {
			return "verifier_circuit_open", false
		}
		return fmt.Sprintf("verifier_error: %s", err.Error()), true
	}
	if verdict == nil {
		// Verifier disabled at config level — treat as off.
		return "", true
	}
	if verdict.Allow {
		return verdict.Explanation, true
	}
	return verdict.Explanation, false
}

// matchByRoute resolves the response rewriter that pairs with the inbound
// route. The conversation.ResponseRegistry's MatchesResponse depends on
// the request's host (for runtime-proxy CONNECT use); for lite-proxy we
// dispatch by route path instead.

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
