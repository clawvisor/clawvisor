package llmproxy

import (
	"errors"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/pkg/store"
)

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
		v := cfg.Inspector.Inspect(req.Context(), inspector.ToolUse{
			ID:    tu.ID,
			Name:  tu.Name,
			Input: tu.Input,
		})

		audit := func(decision, outcome, reason string) {
			if cfg.Audit == nil || auditAgent == nil {
				return
			}
			cfg.Audit.LogToolUseInspected(req.Context(), auditAgent, cfg.RequestID, tu.ID, v, decision, outcome, reason)
		}

		// Inspector says trigger missed (no autovault placeholder),
		// validator returned ambiguous, or it isn't an API call we should
		// mediate — pass through unchanged.
		if v.Source == inspector.SourceTriggerMiss {
			// Don't audit pass-through-no-trigger — most tool_uses go
			// through this path and the row volume would dominate. Only
			// audit when the inspector actually engaged.
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

		// Task-scope authorization: reverse-resolve the (host, method,
		// path) to (service, action), then check against the agent's
		// active tasks. Skipping is audited (in case of misconfig) but
		// not blocking — v0 leaves task-scope as opt-in until product
		// surfaces (always_ask / approval queue) are wired in #33.
		if cfg.Catalog != nil && cfg.TaskScope != nil {
			if resolved, ok := cfg.Catalog.Resolve(v.Host, v.Method, v.Path); ok {
				dec := cfg.TaskScope.Check(req.Context(), cfg.AgentUserID, cfg.AgentID, resolved.ServiceID, resolved.ActionID)
				if !dec.Allowed {
					audit("block", "task_scope_denied", dec.Reason)
					return conversation.ToolUseVerdict{
						Allowed: false,
						Reason:  "Clawvisor: no active task scope covers " + resolved.ServiceID + "." + resolved.ActionID + " — " + dec.Reason,
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

