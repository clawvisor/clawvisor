package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// IntentVerifier matches the intent.Verifier contract. The lite-proxy
// declares its own narrow interface to avoid pulling the LLM provider
// dependency into this package.
type IntentVerifier interface {
	Verify(ctx context.Context, req IntentVerifyRequest) (*IntentVerdict, error)
}

// IntentVerifyRequest is the per-tool-use input to the verifier. Mirrors
// the gateway's intent.VerifyRequest but stripped down to fields the
// lite-proxy can populate from the inspector verdict + matched task.
type IntentVerifyRequest struct {
	TaskPurpose string
	ExpectedUse string
	Service     string
	Action      string
	Params      map[string]any
	Reason      string
	TaskID      string
	Lenient     bool
}

// IntentVerdict mirrors intent.VerificationVerdict (Allow + Explanation
// are the fields lite-proxy actually consumes).
type IntentVerdict struct {
	Allow       bool
	Explanation string
}

// PostprocessConfig wires the inspector + rewriter into the LLM endpoint
// handler's response path. The handler reads the upstream response body
// and calls Postprocess; the result is what the harness sees.
type PostprocessConfig struct {
	// Inspector decides whether each tool_use should be rewritten or
	// passed through. Required.
	Inspector *inspector.Inspector

	// RewriteOpts controls how the rewriter produces the redirected
	// tool_use input. Required when rewrite paths fire.
	RewriteOpts inspector.RewriteOpts

	// Store provides placeholder lookup for the boundary check. The
	// validator's claimed Host is rebound to the placeholder's bound
	// service host allowlist; mismatch fails closed. Required when
	// rewrites are enabled.
	Store store.Store

	// AgentUserID + AgentID scope placeholder ownership to the calling
	// agent. Required for the boundary check.
	AgentUserID string
	AgentID     string

	// Audit is the emitter for runtime.llm_proxy.* events. nil disables
	// audit logging from the postprocess path. The handler keeps audit
	// for the endpoint-call shape; postprocess adds per-tool-use rows.
	Audit *AuditEmitter

	// RequestID is the audit RequestID for tool_use rows so they group
	// with the parent endpoint call.
	RequestID string

	// ResponseRegistry is the conversation rewriter registry. Defaults
	// to conversation.DefaultResponseRegistry() when nil.
	ResponseRegistry *conversation.ResponseRegistry

	// Catalog reverse-resolves (host, method, path) → (service, action)
	// so the task-scope checker can decide whether an active task covers
	// this call. Optional: when nil, task-scope is skipped (v0 fail-open
	// for backwards compatibility on deployments without it wired).
	Catalog interface {
		Resolve(host, method, path string) (ResolvedAction, bool)
	}

	// TaskScope authorizes the resolved (service, action) against the
	// agent's active tasks. Optional: when nil, task-scope is skipped.
	// Skipping is audited so dashboards can show the gap.
	TaskScope TaskScopeChecker

	// IntentVerifier runs the LLM intent check against the matched
	// TaskAction's expected_use whenever the matched action's
	// Verification mode is "strict" (default) or "lenient". Optional:
	// when nil, intent verification is skipped.
	IntentVerifier IntentVerifier

	// Shared decision evaluator inputs. When any of these are set,
	// Postprocess authorizes through pkg/runtime/decision after inspector
	// boundary validation. When all are nil, it falls back to the legacy
	// Catalog/TaskScope flow for compatibility with older tests/configs.
	Posture        runtimedecision.EvaluationPosture
	CandidateTasks []*store.Task
	ToolRules      []*store.RuntimePolicyRule
	EgressRules    []*store.RuntimePolicyRule

	PendingApprovals PendingApprovalCache

	// ControlBaseURL is the daemon URL used for synthetic Clawvisor control
	// endpoint rewrites. Empty disables the control-plane rewrite path.
	ControlBaseURL string
}

// PostprocessResult reports what happened during postprocess. The handler
// uses it to log audit events and surface decisions.
type PostprocessResult struct {
	// Body is the post-processed response body to return to the harness.
	// Identical to the input body when no rewrites applied.
	Body []byte

	// ContentType is the response Content-Type to return.
	ContentType string

	// Rewritten reports whether any tool_use was mutated.
	Rewritten bool

	// Decisions is the per-tool-use audit trail produced by the inspector.
	Decisions []conversation.ToolUseDecisionRecord

	// Skipped reports paths where rewrite logic was bypassed (e.g.
	// streaming SSE in v0). Empty when the response was processed.
	SkippedReason string
}

// Postprocess inspects an upstream response body and applies tool_use
// rewrites where the inspector + boundary check allow. It honors the
// existing block-or-pass evaluator semantics and adds the rewrite path.
//
// Both JSON and SSE Anthropic responses are handled; the SSE path
// whole-buffers the upstream stream, parses it, and re-emits a
// synthesized SSE turn with rewritten tool_use input bytes substituted
// in. Streaming-while-rewriting (true block-by-block emit) is a future
// optimization — the response shape the harness sees is identical.
//
// Returns the response body the handler should write to the harness.
func Postprocess(req *http.Request, body []byte, contentType string, cfg PostprocessConfig) PostprocessResult {
	if cfg.Inspector == nil {
		return PostprocessResult{Body: body, ContentType: contentType, SkippedReason: "no inspector configured"}
	}

	registry := cfg.ResponseRegistry
	if registry == nil {
		registry = conversation.DefaultResponseRegistry()
	}

	// MatchesResponse on the existing rewriters checks the request's host;
	// for the lite-proxy endpoint the host is `clawvisor.example`, not
	// `api.anthropic.com`. Use the parser registry instead — it's
	// route-keyed via ParserForRoute (added for lite-proxy).
	rewriter := matchByRoute(req, registry)
	if rewriter == nil {
		return PostprocessResult{Body: body, ContentType: contentType, SkippedReason: "no rewriter for route"}
	}

	auditAgent := auditAgentForCfg(cfg)

	eval := func(tu conversation.ToolUse) conversation.ToolUseVerdict {
		var v inspector.Verdict
		audit := func(decision, outcome, reason string) {
			if cfg.Audit == nil || auditAgent == nil {
				return
			}
			cfg.Audit.LogToolUseInspected(req.Context(), auditAgent, cfg.RequestID, tu, v, decision, outcome, reason)
		}

		if call, ok := ParseControlToolUseWithBase(tu, cfg.ControlBaseURL); ok {
			v = call.Verdict
			rewritten, _, rewriteOK, err := RewriteControlToolUse(tu, cfg.ControlBaseURL, cfg.RewriteOpts.CallerToken)
			if !rewriteOK {
				audit("block", "control_unavailable", "no control rewrite base URL configured")
				return conversation.ToolUseVerdict{
					Allowed: false,
					Reason:  "Clawvisor: control endpoint unavailable",
				}
			}
			if err != nil {
				audit("block", "control_rewriter_error", err.Error())
				return conversation.ToolUseVerdict{
					Allowed: false,
					Reason:  "Clawvisor: control endpoint rewrite refused — " + err.Error(),
				}
			}
			audit("rewrite", "clawvisor_control", v.Reason)
			return conversation.ToolUseVerdict{
				Allowed:      true,
				RewriteInput: rewritten,
			}
		}

		v = cfg.Inspector.Inspect(req.Context(), inspector.ToolUse{
			ID:    tu.ID,
			Name:  tu.Name,
			Input: tu.Input,
		})

		// Inspector says trigger missed (no autovault placeholder). There
		// is no credential rewrite to perform, but shared authorization
		// still sees ordinary tool_use calls such as Bash/Read.
		if v.Source == inspector.SourceTriggerMiss {
			if cfg.CandidateTasks != nil || cfg.ToolRules != nil || cfg.EgressRules != nil {
				decisionInput := runtimedecision.AuthorizationInput{
					ToolUse:        tu,
					UserID:         cfg.AgentUserID,
					AgentID:        cfg.AgentID,
					Posture:        cfg.Posture,
					CandidateTasks: cfg.CandidateTasks,
					ToolRules:      cfg.ToolRules,
					EgressRules:    cfg.EgressRules,
					IntentVerifier: decisionIntentVerifier{inner: cfg.IntentVerifier},
				}
				dec, err := runtimedecision.EvaluateAuthorization(req.Context(), decisionInput)
				if err != nil {
					audit("block", "decision_error", err.Error())
					return conversation.ToolUseVerdict{Allowed: false, Reason: "Clawvisor: authorization failed — " + err.Error()}
				}
				switch dec.Kind {
				case runtimedecision.VerdictAllow:
					audit("allow", string(dec.Source), dec.Reason)
				case runtimedecision.VerdictDeny:
					audit("block", string(dec.Source), dec.Reason)
					return conversation.ToolUseVerdict{Allowed: false, Reason: "Clawvisor: " + dec.Reason}
				case runtimedecision.VerdictNeedsApproval:
					substitute := approvalPrompt(tu, dec.Reason)
					if cfg.PendingApprovals != nil {
						held, err := cfg.PendingApprovals.Hold(req.Context(), PendingLiteApproval{
							UserID:      cfg.AgentUserID,
							AgentID:     cfg.AgentID,
							Provider:    rewriter.Name(),
							ToolUse:     tu,
							Inspector:   v,
							Fingerprint: runtimedecision.Fingerprint(dec, decisionInput),
							Reason:      dec.Reason,
						})
						if err != nil {
							audit("block", "approval_hold_error", err.Error())
							return conversation.ToolUseVerdict{
								Allowed: false,
								Reason:  "Clawvisor: approval unavailable — " + err.Error(),
							}
						}
						if held.Evicted != nil {
							audit("block", "approval_evicted", "superseded pending approval "+held.Evicted.ID)
						}
					}
					audit("block", string(dec.Source), dec.Reason)
					return conversation.ToolUseVerdict{
						Allowed:        false,
						Reason:         "Clawvisor: approval required — " + dec.Reason,
						SubstituteWith: substitute,
					}
				}
			}
			// Record ordinary tool uses even when no credential trigger was
			// present so lite-proxy activity shows the agent's tool calls.
			audit("allow", "pass_through", "no credential trigger")
			return conversation.ToolUseVerdict{Allowed: true}
		}
		if v.Ambiguous || !v.IsAPICall {
			audit("block", "ambiguous", v.Reason)
			return conversation.ToolUseVerdict{
				Allowed: false,
				Reason:  "Clawvisor: ambiguous credentialed call refused — " + v.Reason,
			}
		}

		// Authorization boundary: the validator's `Host` is a candidate.
		// The authoritative source is the placeholder's bound service
		// host allowlist. Look it up and run BoundaryCheck. Mismatch =
		// fail closed.
		if reason, ok := boundaryCheckVerdict(req, cfg, v); !ok {
			audit("block", "boundary_check_failed", reason)
			return conversation.ToolUseVerdict{
				Allowed: false,
				Reason:  "Clawvisor: target host outside placeholder bound-service — " + reason,
			}
		}

		decisionHandled := false
		if cfg.CandidateTasks != nil || cfg.ToolRules != nil || cfg.EgressRules != nil {
			resolved := ResolvedAction{}
			if cfg.Catalog != nil {
				resolved, _ = cfg.Catalog.Resolve(v.Host, v.Method, v.Path)
			}
			decisionInput := runtimedecision.AuthorizationInput{
				ToolUse:        tu,
				UserID:         cfg.AgentUserID,
				AgentID:        cfg.AgentID,
				Posture:        cfg.Posture,
				Target:         runtimedecision.TargetRequest{Host: v.Host, Method: v.Method, Path: v.Path},
				Service:        resolved.ServiceID,
				Action:         resolved.ActionID,
				CandidateTasks: cfg.CandidateTasks,
				ToolRules:      cfg.ToolRules,
				EgressRules:    cfg.EgressRules,
				IntentVerifier: decisionIntentVerifier{inner: cfg.IntentVerifier},
			}
			dec, err := runtimedecision.EvaluateAuthorization(req.Context(), decisionInput)
			if err != nil {
				audit("block", "decision_error", err.Error())
				return conversation.ToolUseVerdict{
					Allowed: false,
					Reason:  "Clawvisor: authorization failed — " + err.Error(),
				}
			}
			switch dec.Kind {
			case runtimedecision.VerdictAllow:
				// Continue to credential rewrite below.
				decisionHandled = true
			case runtimedecision.VerdictDeny:
				audit("block", string(dec.Source), dec.Reason)
				return conversation.ToolUseVerdict{
					Allowed: false,
					Reason:  "Clawvisor: " + dec.Reason,
				}
			case runtimedecision.VerdictNeedsApproval:
				if cfg.PendingApprovals != nil {
					held, err := cfg.PendingApprovals.Hold(req.Context(), PendingLiteApproval{
						UserID:      cfg.AgentUserID,
						AgentID:     cfg.AgentID,
						Provider:    rewriter.Name(),
						ToolUse:     tu,
						Inspector:   v,
						Fingerprint: runtimedecision.Fingerprint(dec, decisionInput),
						Reason:      dec.Reason,
					})
					if err != nil {
						audit("block", "approval_hold_error", err.Error())
						return conversation.ToolUseVerdict{
							Allowed: false,
							Reason:  "Clawvisor: approval unavailable — " + err.Error(),
						}
					}
					if held.Evicted != nil {
						audit("block", "approval_evicted", "superseded pending approval "+held.Evicted.ID)
					}
				}
				audit("block", string(dec.Source), dec.Reason)
				return conversation.ToolUseVerdict{
					Allowed:        false,
					Reason:         "Clawvisor: approval required — " + dec.Reason,
					SubstituteWith: approvalPrompt(tu, dec.Reason),
				}
			}
		}

		// Task-scope authorization: reverse-resolve the (host, method,
		// path) to (service, action), then check against the agent's
		// active tasks. Skipping is audited (in case of misconfig) but
		// not blocking — v0 leaves task-scope as opt-in until product
		// surfaces (always_ask / approval queue) are wired in #33.
		if !decisionHandled && cfg.Catalog != nil && cfg.TaskScope != nil {
			if resolved, ok := cfg.Catalog.Resolve(v.Host, v.Method, v.Path); ok {
				dec := cfg.TaskScope.Check(req.Context(), cfg.AgentUserID, cfg.AgentID, resolved.ServiceID, resolved.ActionID)
				if !dec.Allowed {
					audit("block", "task_scope_denied", dec.Reason)
					return conversation.ToolUseVerdict{
						Allowed: false,
						Reason:  "Clawvisor: no active task scope covers " + resolved.ServiceID + "." + resolved.ActionID + " — " + dec.Reason,
					}
				}
				// Intent verification: when the matched TaskAction's
				// Verification mode opts in (strict | lenient | empty)
				// and an IntentVerifier is configured, the LLM compares
				// the request's params + tool_use shape to the matched
				// expected_use. Off mode and missing verifier skip silently.
				if reason, ok := runIntentVerify(req.Context(), cfg, dec, resolved, tu); !ok {
					audit("block", "intent_verification_failed", reason)
					return conversation.ToolUseVerdict{
						Allowed: false,
						Reason:  "Clawvisor: intent verification refused " + resolved.ServiceID + "." + resolved.ActionID + " — " + reason,
					}
				}
			}
			// Catalog miss: log via audit reason field but don't block.
			// The fact that the (host, method, path) didn't resolve to a
			// known (service, action) is an inspector or catalog gap, not
			// an attack signal — the BoundaryCheck above already constrained
			// the host to the placeholder's bound-service allowlist.
		}

		rewritten, err := inspector.Rewrite(inspector.ToolUse{
			ID:    tu.ID,
			Name:  tu.Name,
			Input: tu.Input,
		}, v, cfg.RewriteOpts)
		if err != nil {
			audit("block", "rewriter_error", err.Error())
			return conversation.ToolUseVerdict{
				Allowed: false,
				Reason:  "Clawvisor: rewriter refused — " + err.Error(),
			}
		}
		audit("rewrite", "success", v.Reason)
		return conversation.ToolUseVerdict{
			Allowed:      true,
			RewriteInput: rewritten,
		}
	}

	result, err := rewriter.Rewrite(body, contentType, eval)
	if err != nil {
		// Fail closed: if the rewriter errored AFTER an autovault trigger
		// fired (we won't know without inspecting input bodies, so we
		// conservatively assume yes), the safe behavior is to refuse the
		// response rather than pass it through with literal placeholders.
		// Today's eval callback returns ambiguous-on-trigger-miss as
		// Allowed:true; rewriter errors are mostly malformed-body cases.
		// Caller decides how to surface; we mark as skipped so handlers
		// can choose to 502 rather than write through unchanged.
		return PostprocessResult{
			Body:          body,
			ContentType:   contentType,
			SkippedReason: "rewriter error: " + err.Error(),
		}
	}
	return PostprocessResult{
		Body:        result.Body,
		ContentType: contentType,
		Rewritten:   result.Rewritten,
		Decisions:   result.Decisions,
	}
}

func approvalPrompt(tu conversation.ToolUse, reason string) string {
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
	b.WriteString("\n\nReply `approve` to run this tool call, `deny` to block it, or `task` to instruct the agent to include this in a task definition for approval.")
	return b.String()
}

func taskCreationPrompt(tu conversation.ToolUse) string {
	toolName := strings.TrimSpace(tu.Name)
	if toolName == "" {
		return ""
	}
	payload := map[string]any{
		"purpose": "Describe the user-visible task you are trying to complete, including why this tool access is needed.",
		"expected_tools_json": []map[string]any{{
			"tool_name": toolName,
			"why":       taskToolWhy(tu),
		}},
		"intent_verification_mode": "strict",
		"expires_in_seconds":       600,
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return ""
	}
	return "Your action was blocked. Please create a task that covers the blocked action, along with any other tools or commands you expect to use to complete the user's request. Add more `expected_tools_json` entries as needed.\n\nUse the official Clawvisor proxy-lite control plane described in your system instructions: call a shell/curl tool to POST to the synthetic URL `https://clawvisor.local/control/tasks`. Do not use Clawvisor MCP tools such as `mcp__claude_ai_clawvisor__create_task` for this bootstrap step; those MCP tools are ordinary model tools and may themselves require task scope.\n\nExample:\n\n```sh\ncurl -sS -X POST https://clawvisor.local/control/tasks \\\n  -H 'Content-Type: application/json' \\\n  --data @- <<'JSON'\n" + string(raw) + "\nJSON\n```"
}

func taskToolWhy(tu conversation.ToolUse) string {
	switch strings.TrimSpace(tu.Name) {
	case "Bash", "bash", "exec_command":
		if command := toolInputString(tu.Input, "command", "cmd"); command != "" {
			return "Run shell command(s) needed for the task, including: " + command
		}
	case "Read":
		if path := toolInputString(tu.Input, "file_path", "path"); path != "" {
			return "Read files needed for the task, including: " + path
		}
	case "Write", "Edit", "NotebookEdit":
		if path := toolInputString(tu.Input, "file_path", "path"); path != "" {
			return "Modify files needed for the task, including: " + path
		}
	case "WebFetch", "WebSearch":
		if target := toolInputString(tu.Input, "url", "query"); target != "" {
			return "Use web access needed for the task, including: " + target
		}
	}
	return "Use this tool for the requested task. Include a concise description of the command pattern, file path, URL, or operation."
}

func toolInputString(raw json.RawMessage, keys ...string) string {
	if len(raw) == 0 {
		return ""
	}
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		return ""
	}
	for _, key := range keys {
		if v, ok := input[key].(string); ok {
			v = strings.TrimSpace(v)
			if v != "" {
				return v
			}
		}
	}
	return ""
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

// auditAgentForCfg builds a minimal *store.Agent for the audit emitter
// from the postprocess config. The emitter only reads UserID and ID; we
// avoid an extra DB lookup by synthesizing the struct.
func auditAgentForCfg(cfg PostprocessConfig) *store.Agent {
	if cfg.Audit == nil || cfg.AgentID == "" || cfg.AgentUserID == "" {
		return nil
	}
	return &store.Agent{ID: cfg.AgentID, UserID: cfg.AgentUserID}
}

// boundaryCheckVerdict validates the inspector's claimed host against
// the bound-service allowlist of every placeholder it found. Returns
// (reason, ok). ok=false on any mismatch, ownership failure, or unknown
// service — fail-closed by construction.
func boundaryCheckVerdict(req *http.Request, cfg PostprocessConfig, v inspector.Verdict) (string, bool) {
	if cfg.Store == nil {
		return "no store configured for boundary check", false
	}
	if cfg.AgentUserID == "" || cfg.AgentID == "" {
		return "no agent context for boundary check", false
	}
	if len(v.Placeholders) == 0 {
		return "verdict missing placeholder for boundary lookup", false
	}
	for _, ph := range v.Placeholders {
		rec, err := cfg.Store.GetRuntimePlaceholder(req.Context(), ph)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return "placeholder not registered", false
			}
			return "store error: " + err.Error(), false
		}
		if rec.UserID != cfg.AgentUserID || rec.AgentID != cfg.AgentID {
			return "placeholder owned by another agent", false
		}
		hosts := inspector.BoundServiceHosts(rec.ServiceID)
		if len(hosts) == 0 {
			return "no bound-service hosts for service " + rec.ServiceID, false
		}
		if ok, reason := inspector.BoundaryCheck(v, hosts); !ok {
			return reason, false
		}
	}
	return "", true
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
func matchByRoute(req *http.Request, registry *conversation.ResponseRegistry) conversation.ResponseRewriter {
	if registry == nil || req == nil || req.URL == nil {
		return nil
	}
	parsers := conversation.DefaultRegistry()
	parser := parsers.ParserForRoute(req.URL.Path)
	if parser == nil {
		return nil
	}
	provider := parser.Name()
	return registry.ForProvider(provider)
}
