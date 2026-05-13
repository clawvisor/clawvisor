package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

	// CallerNonces mints the short-lived single-use nonce that takes
	// the place of the agent's bearer token in the rewritten tool_use's
	// X-Clawvisor-Caller header. The nonce is bound to (agent, host,
	// method, path); the resolver-side middleware consumes it on the
	// matching call. When non-nil, the rewriter receives a freshly
	// minted nonce per tool_use; the agent's raw token never enters
	// the model's conversation context. When nil, credentialed rewrites
	// fail closed with a configuration error.
	CallerNonces CallerNonceCache

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

	// Trace, when non-nil, receives one JSON-line event per inspector
	// decision point for this request. Disabled by default; enabled
	// via cfg.ProxyLite.TraceLogPath. Calls on a nil *TraceLogger are
	// no-ops, so production code doesn't branch.
	Trace *TraceLogger
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
		// trace emits one JSONL line per decision point when
		// cfg.Trace is configured. The kv slice is event-specific.
		trace := func(event string, kv ...any) {
			if cfg.Trace == nil {
				return
			}
			m := map[string]any{
				"event":       event,
				"request_id":  cfg.RequestID,
				"user_id":     cfg.AgentUserID,
				"agent_id":    cfg.AgentID,
				"tool_use_id": tu.ID,
				"tool_name":   tu.Name,
			}
			for i := 0; i+1 < len(kv); i += 2 {
				key, ok := kv[i].(string)
				if !ok {
					continue
				}
				m[key] = kv[i+1]
			}
			cfg.Trace.Emit(m)
		}
		trace(TraceEventToolUseEntry,
			"input_preview", truncateForTrace(string(tu.Input), traceInputPreviewLimit),
			"input_bytes", len(tu.Input),
			"trigger_hit", inspector.TriggerHits(inspector.ToolUse{ID: tu.ID, Name: tu.Name, Input: tu.Input}),
		)

		if call, ok := ParseControlToolUseWithBase(tu, cfg.ControlBaseURL); ok {
			v = call.Verdict
			// Inline task approval interception. When the user is
			// mid-flight on a "task" gesture (the original tool hold has
			// been transitioned to StageAwaitingTaskDefinition) and the
			// model now emits POST /control/tasks, we route the task body
			// through the inline approval path instead of letting it
			// proxy through to the dashboard. The model never sees the
			// real /control/tasks handler — its tool_use_result is
			// replaced with a rendered approve/deny prompt; the user's
			// next "approve" creates the task pre-approved and the
			// follow-up turn auto-releases the original tool call.
			if inlineVerdict, inlineHandled := maybeInterceptInlineTaskDefinition(
				req, cfg, audit, trace, rewriter.Name(), tu, call,
			); inlineHandled {
				return inlineVerdict
			}
			// Mint a nonce bound to the rewritten control URL's
			// (host, method, path) — the rewritten curl carries it in
			// X-Clawvisor-Caller; the daemon's nonce middleware on
			// /control/* one-shot consumes it. Without this, the
			// rewriter would have to embed the agent's raw cvis_ token
			// (which the nonce middleware rejects) in the model's
			// conversation context.
			if cfg.CallerNonces == nil {
				audit("block", "caller_nonce_unavailable", "caller nonce cache not configured")
				return conversation.ToolUseVerdict{
					Allowed: false,
					Reason:  "Clawvisor: caller nonce cache not configured; refusing to embed agent token in control tool_use",
				}
			}
			nonce, mintErr := cfg.CallerNonces.Mint(req.Context(), cfg.AgentID, NonceTarget{
				Host:   v.Host,
				Method: v.Method,
				Path:   v.Path,
			})
			if mintErr != nil {
				audit("block", "caller_nonce_mint_failed", mintErr.Error())
				return conversation.ToolUseVerdict{
					Allowed: false,
					Reason:  "Clawvisor: caller nonce mint failed — " + mintErr.Error(),
				}
			}
			rewritten, _, rewriteOK, err := RewriteControlToolUse(tu, cfg.ControlBaseURL, nonce)
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
			trace(TraceEventControlRewrite,
				"host", v.Host,
				"method", v.Method,
				"path", v.Path,
				"nonce_prefix", nonce[:min(len(nonce), 14)],
				"rewrite_bytes", len(rewritten),
			)
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
		trace(TraceEventInspectVerdict,
			"source", string(v.Source),
			"is_api_call", v.IsAPICall,
			"ambiguous", v.Ambiguous,
			"method", v.Method,
			"host", v.Host,
			"path", v.Path,
			"placeholders", v.Placeholders,
			"reason", v.Reason,
		)

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
				trace(TraceEventDecision,
					"path", "trigger_miss",
					"kind", string(dec.Kind),
					"source", string(dec.Source),
					"reason", dec.Reason,
					"task_id", taskIDFromDecision(dec),
				)
				// Local-only tools (Read, Edit, Glob, Skill, …) never make
				// outbound HTTP calls. Requiring an active task to cover
				// every one is useless friction — but a task that DID
				// declare expected_tools or a deny rule that targets the
				// tool must still apply. So we run the evaluator
				// normally and only override the "no matching task scope"
				// review verdict to allow for local-only tools.
				localOnly := inspector.IsLocalOnlyTool(tu.Name)
				switch dec.Kind {
				case runtimedecision.VerdictAllow:
					audit("allow", string(dec.Source), dec.Reason)
					return conversation.ToolUseVerdict{Allowed: true}
				case runtimedecision.VerdictDeny:
					audit("block", string(dec.Source), dec.Reason)
					return conversation.ToolUseVerdict{Allowed: false, Reason: "Clawvisor: " + dec.Reason}
				case runtimedecision.VerdictNeedsApproval:
					if localOnly && dec.Source == runtimedecision.SourceTaskScopeMissing {
						audit("allow", "local_only_pass_through", "local-only tool ("+tu.Name+")")
						trace(TraceEventDecision, "path", "trigger_miss", "kind", "allow", "source", "local_only_pass_through", "reason", "local-only tool ("+tu.Name+")")
						return conversation.ToolUseVerdict{Allowed: true}
					}
					// Codex's write_stdin with empty chars is the
					// harness polling a background shell for output —
					// equivalent to Claude Code's BashOutput. No
					// state change, no side effect. Pass through.
					if dec.Source == runtimedecision.SourceTaskScopeMissing && isShellPollTool(tu.Name, tu.Input) {
						audit("allow", "shell_poll_pass_through", "background-shell poll ("+tu.Name+")")
						trace(TraceEventDecision, "path", "trigger_miss", "kind", "allow", "source", "shell_poll_pass_through", "reason", "background-shell poll")
						return conversation.ToolUseVerdict{Allowed: true}
					}
					// Read-only bash commands (pwd, ls, cat, grep |
					// wc, …) classified as side-effect-free by the
					// AST classifier run without task scope. Writes,
					// network, mutating flags (sed -i, find -exec)
					// still fall through to the approval prompt.
					if dec.Source == runtimedecision.SourceTaskScopeMissing && isShellTool(tu.Name) {
						if cmd := shellCommandFromInput(tu.Input); cmd != "" {
							if readOK, _ := inspector.IsReadOnlyBashCommand(cmd); readOK {
								audit("allow", "readonly_shell_pass_through", "read-only bash command")
								trace(TraceEventDecision, "path", "trigger_miss", "kind", "allow", "source", "readonly_shell_pass_through", "reason", "read-only bash command", "cmd_preview", truncateForTrace(cmd, 200))
								return conversation.ToolUseVerdict{Allowed: true}
							}
						}
					}
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
		boundaryReason, boundaryOK := boundaryCheckVerdict(req, cfg, v)
		trace(TraceEventBoundaryCheck,
			"ok", boundaryOK,
			"reason", boundaryReason,
			"placeholders", v.Placeholders,
			"verdict_host", v.Host,
		)
		if !boundaryOK {
			audit("block", "boundary_check_failed", boundaryReason)
			return conversation.ToolUseVerdict{
				Allowed: false,
				Reason:  "Clawvisor: target host outside placeholder bound-service — " + boundaryReason,
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
			trace(TraceEventDecision,
				"path", "credentialed",
				"kind", string(dec.Kind),
				"source", string(dec.Source),
				"reason", dec.Reason,
				"service", resolved.ServiceID,
				"action", resolved.ActionID,
				"task_id", taskIDFromDecision(dec),
			)
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

		// Mint a per-tool nonce that stands in for the agent's bearer
		// token in the rewritten tool_use's X-Clawvisor-Caller header.
		// The nonce is bound to (agent, host, method, path); the
		// resolver consumes it one-shot on the matching call. Failure
		// to mint (cache misconfigured or backend down) fails closed —
		// we won't embed the raw agent token in the conversation as a
		// fallback.
		if cfg.CallerNonces == nil {
			audit("block", "caller_nonce_unavailable", "caller nonce cache not configured")
			return conversation.ToolUseVerdict{
				Allowed: false,
				Reason:  "Clawvisor: caller nonce cache not configured; refusing to embed agent token in tool_use",
			}
		}
		nonce, mintErr := cfg.CallerNonces.Mint(req.Context(), cfg.AgentID, NonceTarget{
			Host:   v.Host,
			Method: v.Method,
			Path:   v.Path,
		})
		if mintErr != nil {
			audit("block", "caller_nonce_mint_failed", mintErr.Error())
			return conversation.ToolUseVerdict{
				Allowed: false,
				Reason:  "Clawvisor: caller nonce mint failed — " + mintErr.Error(),
			}
		}
		opts := cfg.RewriteOpts
		opts.CallerToken = nonce
		rewritten, err := inspector.Rewrite(inspector.ToolUse{
			ID:    tu.ID,
			Name:  tu.Name,
			Input: tu.Input,
		}, v, opts)
		if err != nil {
			audit("block", "rewriter_error", err.Error())
			return conversation.ToolUseVerdict{
				Allowed: false,
				Reason:  "Clawvisor: rewriter refused — " + err.Error(),
			}
		}
		audit("rewrite", "success", v.Reason)
		trace(TraceEventRewriteApplied,
			"host", v.Host,
			"method", v.Method,
			"path", v.Path,
			"placeholders", v.Placeholders,
			"nonce_prefix", nonce[:min(len(nonce), 14)],
			"rewrite_bytes", len(rewritten),
		)
		return conversation.ToolUseVerdict{
			Allowed:      true,
			RewriteInput: rewritten,
		}
	}

	result, err := rewriter.Rewrite(body, contentType, eval)
	if err != nil {
		// Fail closed: the rewriter failed mid-body so we don't know
		// whether a credentialed placeholder survived into the response.
		// Returning the original body would pass it (or worse, the
		// literal placeholder) to the harness. Drop the body and surface
		// a non-empty SkippedReason; the handler checks SkippedReason to
		// emit a 502 instead of writing the upstream body unchanged.
		return PostprocessResult{
			Body:          nil,
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

// ambiguousRefusalGuidance produces the substitute message the model
// sees when the inspector refused a credentialed call as ambiguous.
// The model needs actionable instructions on how to rewrite the call
// in a shape Clawvisor can mediate — otherwise it retries the same
// shape and ends up in a loop, or worse, copies a fragment back into
// the conversation and gets stuck.
func ambiguousRefusalGuidance(tu conversation.ToolUse, v inspector.Verdict) string {
	preview := conversation.MakeToolInputPreview(tu.Input)
	var b strings.Builder
	b.WriteString("Clawvisor refused this credentialed call: ")
	b.WriteString(v.Reason)
	b.WriteString(".")
	if tu.Name != "" {
		b.WriteString("\n\nTool: `")
		b.WriteString(tu.Name)
		b.WriteString("`")
	}
	if preview != "" {
		b.WriteString("\nInput: ")
		b.WriteString(preview)
	}
	// Tailored guidance based on the parser's specific objection.
	reason := strings.ToLower(v.Reason)
	switch {
	case strings.Contains(reason, "shell metacharacter"):
		b.WriteString("\n\nRewrite the command as a single curl invocation with no pipes, redirects, command chaining (`|`, `;`, `&&`, `>`, `2>&1`), command substitution (`$(...)`, backticks), or subshells. Clawvisor needs to parse the curl shape to inject credentials safely. If you need to filter or post-process the response, run a separate tool call after the curl returns.")
	case strings.Contains(reason, "unknown curl flag"):
		b.WriteString("\n\nThe curl flag isn't on Clawvisor's allowlist (only common safe flags like `-s`, `-S`, `-f`, `-i`, `-A`, `-o`, `--max-time` are accepted; `-L`, `-k`, `-x`, `-d`, `--data*`, `-T`, `-F` are refused). Rewrite without that flag.")
	case strings.Contains(reason, "expected exactly one positional URL"):
		b.WriteString("\n\nUse exactly one URL positional argument. If you need to call multiple endpoints, run separate tool calls.")
	case strings.Contains(reason, "placeholder not in"):
		b.WriteString("\n\nThe credential placeholder must appear in an HTTP header (e.g. `-H 'Authorization: Bearer autovault_…'`). Body, query, or non-header locations are not yet supported for rewrite.")
	default:
		b.WriteString("\n\nRewrite the call in the simplest shape Clawvisor can mediate: a single curl invocation with `-H 'Authorization: Bearer <autovault_placeholder>'` and one URL positional argument. No pipes, redirects, or command chaining.")
	}
	return b.String()
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
	// The user just typed "task" at the inline prompt — they are
	// definitionally at the chat surface. Pass ?surface=inline so the
	// proxy holds the approve/deny gesture inline rather than routing
	// to the dashboard's notification queue. wait=true is harmless for
	// the inline path (the curl never actually runs) but keeps the
	// shape compatible if the model decides to omit surface=inline.
	//
	// Use the single-curl `--data @- <<JSON` shape. The proxy DOES
	// accept a cat-heredoc-to-file then curl --data @file pattern, but
	// it's strictly more error-prone — keep the prompt to one shape.
	return "Please request a Clawvisor task for this work using the proxy-lite control endpoint. Before creating the task, tell me that I will need to approve it. Use a single curl invocation to POST the task definition to `https://clawvisor.local/control/tasks?wait=true&timeout=120&surface=inline` so I can approve it without leaving the chat. Include the blocked action and any related tools or commands you expect to need.\n\nExample (use this exact shape — one curl, JSON via `--data @-` heredoc, no intermediate file):\n\n```sh\ncurl -sS -X POST 'https://clawvisor.local/control/tasks?wait=true&timeout=120&surface=inline' \\\n  -H 'Content-Type: application/json' \\\n  --data @- <<'JSON'\n" + string(raw) + "\nJSON\n```"
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

// isShellTool reports whether name matches one of the harness shell
// tools whose input carries a literal bash command line. Used by the
// scope-free-reads pass-through path to decide whether to invoke the
// AST classifier on the tool's cmd field.
func isShellTool(name string) bool {
	switch name {
	case "Bash", "shell", "exec_command":
		return true
	}
	return false
}

// isShellPollTool reports whether a tool_use is a harness poll on a
// background shell — read-equivalent and worth passing through. The
// canonical case is Codex's `write_stdin` with empty `chars`, which
// the harness emits continuously while a backgrounded `exec_command`
// is running. Non-empty `chars` is actual input typed into a shell
// (potentially mutating); stay strict.
func isShellPollTool(name string, raw json.RawMessage) bool {
	if name != "write_stdin" {
		return false
	}
	if len(raw) == 0 {
		return false
	}
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		return false
	}
	chars, ok := input["chars"].(string)
	if !ok {
		return false
	}
	return strings.TrimSpace(chars) == ""
}

// shellCommandFromInput extracts the command string from a shell-tool
// input JSON. Claude Code's Bash uses `command`; Codex's exec_command
// uses `cmd`. Returns "" when neither is present or non-string.
func shellCommandFromInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		return ""
	}
	if v, ok := input["cmd"].(string); ok && v != "" {
		return v
	}
	if v, ok := input["command"].(string); ok {
		return v
	}
	return ""
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
				return "placeholder not registered: " + redactPlaceholderForReason(ph), false
			}
			return "store error: " + err.Error(), false
		}
		if rec.UserID != cfg.AgentUserID || rec.AgentID != cfg.AgentID {
			return "placeholder owned by another agent (placeholder=" + redactPlaceholderForReason(ph) + ")", false
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
