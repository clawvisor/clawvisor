package handlers

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/internal/intent"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
)

// ScopeDriftHandler serves the agent-facing /control/scope-drift/{id}/*
// endpoints — option (c) one-off and option (d) justify in the
// four-choice menu. Options (a) and (b) reuse the existing
// /control/tasks/{id}/expand and /control/tasks handlers; this struct
// only owns the routes that have no pre-existing endpoint.
//
// Construction is dependency-injected: the registry holds the drifts,
// the verifier re-runs intent verification with the agent's
// justification populated. Both may be nil in test/older configs; the
// handlers return 503 in that case rather than panicking.
type ScopeDriftHandler struct {
	Registry llmproxy.ScopeDriftRegistry
	Verifier intent.Verifier
	Logger   *slog.Logger
}

// NewScopeDriftHandler is a small convenience constructor so callers
// can dependency-inject in one place. nil values are tolerated; the
// handler returns 503 when a required dependency is missing.
func NewScopeDriftHandler(registry llmproxy.ScopeDriftRegistry, verifier intent.Verifier, logger *slog.Logger) *ScopeDriftHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &ScopeDriftHandler{
		Registry: registry,
		Verifier: verifier,
		Logger:   logger,
	}
}

// justifyRequest is the body the agent posts to claim option (d).
type justifyRequest struct {
	// Justification is the agent's argument that the first verdict
	// was wrong. Threaded into intent.VerifyRequest.AgentJustification
	// and re-evaluated by the same verifier.
	Justification string `json:"justification"`
}

// OneOff handles POST /control/scope-drift/{id}/one-off — option (c).
//
// NOT YET WIRED — returns 501. The end-to-end one-off path requires a
// user-approval channel (the daemon notifier surfaces a prompt and the
// user's yes/no flips the drift outcome to succeeded, which inserts
// the pre-clear that lets the retry through). Until that channel is in
// place, claiming the drift here would burn the one-shot cap on an
// option that cannot complete — locking the agent out of (a)/(b)/(d)
// for a path the daemon can't actually drive to resolution.
//
// The 501 is returned BEFORE ClaimOption so the drift stays available
// for the agent to pick a working option. The menu prompt already
// avoids advertising (c) as a working option on this build; this
// handler exists so an agent that hand-crafts the URL gets a clear
// 501 + redirection instead of a silent 404.
func (h *ScopeDriftHandler) OneOff(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agent := middleware.AgentFromContext(ctx)
	if agent == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error":   "unauthorized",
			"message": "missing agent context",
		})
		return
	}
	driftID := strings.TrimSpace(r.PathValue("id"))
	if driftID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "drift_id_required",
			"message": "drift_id path segment is required",
		})
		return
	}
	h.Logger.InfoContext(ctx, "scope drift one-off attempted on unfinished surface",
		"drift_id", driftID,
		"agent_id", agent.ID,
	)
	writeJSON(w, http.StatusNotImplemented, map[string]any{
		"error":    "one_off_not_implemented",
		"drift_id": driftID,
		"message":  "Option (c) one-off requires a user-approval channel that is not wired on this build. The drift remains unclaimed — pick option (a) expand, (b) new task, or (d) justify instead.",
		"next_step": "Re-read the original drift menu and choose another option. The drift_id is still valid until its TTL expires.",
	})
}

// Justify handles POST /control/scope-drift/{id}/justify — option (d).
// The agent provides a justification arguing the original verdict was
// wrong. The same verifier re-evaluates with AgentJustification
// populated. On accept, the drift's pre-clear is set and the agent can
// retry. On reject, a clean one-off prompt is queued for the user (no
// echo of the failed justification).
func (h *ScopeDriftHandler) Justify(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agent := middleware.AgentFromContext(ctx)
	if agent == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error":   "unauthorized",
			"message": "missing agent context",
		})
		return
	}
	if h.Registry == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":   "scope_drift_registry_unavailable",
			"message": "scope drift registry is not configured on this daemon",
		})
		return
	}

	driftID := strings.TrimSpace(r.PathValue("id"))
	if driftID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "drift_id_required",
			"message": "drift_id path segment is required",
		})
		return
	}

	var body justifyRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	justification := strings.TrimSpace(body.Justification)
	if justification == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "justification_required",
			"message": "justification is required — articulate the concrete connection between this call and the active task purpose. Confident assertion alone will be rejected by the verifier.",
		})
		return
	}

	drift, err := h.Registry.Get(ctx, driftID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error":   "drift_not_found",
			"message": "no scope drift with that id (it may have expired)",
		})
		return
	}
	if drift.AgentID != agent.ID {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":   "drift_not_owned",
			"message": "this drift was not minted for your agent",
		})
		return
	}
	if drift.Source != llmproxy.ScopeDriftSourceIntentVerification {
		// Only verifier-source drifts can be /justify'd — task scope
		// denials reflect the absence of an authorized action, not a
		// verifier judgment, so no second-pass verification is
		// meaningful. Surface this clearly so the agent picks (a)/(b)
		// instead.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":         "drift_not_verifier_sourced",
			"chosen_option": drift.ChosenOption,
			"outcome":       drift.Outcome,
			"message":       "option (d) only applies when the block source is intent_verification. This drift was caused by task_scope. Use (a) expand or (b) new task instead.",
		})
		return
	}

	// Verifier readiness check MUST come before ClaimOption — a nil
	// verifier here would otherwise burn the one-shot cap on a drift
	// that never gets evaluated, locking the agent out of options
	// (a)/(b)/(c) without ever running a second pass. By short-circuiting
	// before the claim, the drift stays available for the agent to pick
	// another option once the misconfiguration is resolved.
	if h.Verifier == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":   "verifier_unavailable",
			"message": "intent verifier is not configured on this daemon. The drift remains unclaimed — pick option (a) expand or (b) new task instead.",
		})
		return
	}

	claimed, err := h.Registry.ClaimOption(ctx, driftID, llmproxy.ScopeDriftOptionJustify, "", justification)
	if errors.Is(err, llmproxy.ErrDriftAlreadyResolved) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":         "drift_already_resolved",
			"chosen_option": claimed.ChosenOption,
			"outcome":       claimed.Outcome,
			"message":       "this drift has already been resolved with a different option. The one-shot cap forbids re-claiming.",
		})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":   "drift_claim_failed",
			"message": err.Error(),
		})
		return
	}

	var params map[string]any
	if len(claimed.ToolUse.Input) > 0 {
		_ = json.Unmarshal(claimed.ToolUse.Input, &params)
	}
	verdict, verifyErr := h.Verifier.Verify(ctx, intent.VerifyRequest{
		TaskPurpose:        claimed.TaskPurpose,
		ExpectedUse:        claimed.ExpectedUse,
		Service:            claimed.Service,
		Action:             claimed.Action,
		Params:             params,
		Reason:             "lite-proxy tool_use " + claimed.ToolUse.Name + " — second-pass verification via scope-drift justify",
		TaskID:             claimed.TaskID,
		AgentJustification: justification,
		ProxyLite:          true,
	})
	if verifyErr != nil {
		// Verifier failure on the second pass is treated the same as a
		// rejection: fall back to the one-off path so a transient LLM
		// outage doesn't pin a legitimate agent forever. Audit will
		// record the error.
		h.Logger.WarnContext(ctx, "scope drift justify verifier error; falling back to one-off",
			"drift_id", driftID, "err", verifyErr)
		_ = h.Registry.SetOutcome(ctx, driftID, llmproxy.ScopeDriftOutcomeFellBack)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"status":         "verifier_unavailable_fallback",
			"drift_id":       driftID,
			"option":         "justify",
			"verifier_error": verifyErr.Error(),
			"next_step":      "The verifier could not be reached. Clawvisor surfaced a clean one-off approval prompt to the user. Poll GET /control/scope-drift/" + driftID + " for status.",
		})
		return
	}
	if verdict != nil && verdict.Allow {
		// Verifier-authoritative success: pre-clear the original call.
		// SetOutcome on succeeded inserts the (agent, fingerprint)
		// pre-clear so the agent's retry passes scope+intent checks
		// exactly once.
		if err := h.Registry.SetOutcome(ctx, driftID, llmproxy.ScopeDriftOutcomeSucceeded); err != nil {
			h.Logger.ErrorContext(ctx, "scope drift justify accepted but pre-clear write failed",
				"drift_id", driftID, "err", err)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":      "verifier_accepted",
			"drift_id":    driftID,
			"option":      "justify",
			"explanation": verdict.Explanation,
			"next_step":   "Re-emit the original tool call exactly as before. Clawvisor pre-clears it once on this drift_id. Do not change the params — the pre-clear matches on (agent, service, action, host, method, path).",
		})
		return
	}

	// Verifier rejected the justification: fall back to the clean
	// one-off prompt to the user. We deliberately do NOT echo the
	// justification or original drift cause into the fallback prompt
	// — the user sees a fresh "approve once?" decision.
	explanation := ""
	if verdict != nil {
		explanation = verdict.Explanation
	}
	_ = h.Registry.SetOutcome(ctx, driftID, llmproxy.ScopeDriftOutcomeFellBack)
	h.Logger.InfoContext(ctx, "scope drift justify rejected; falling back to one-off",
		"drift_id", driftID,
		"agent_id", agent.ID,
		"service", claimed.Service,
		"action", claimed.Action,
		"verifier_explanation", explanation,
	)
	// Use relative paths in next_step so the guidance is correct in any
	// deployment (proxy-lite intercept, direct API access from internal
	// tooling, alternate control hosts). The agent already knows the
	// origin it called us on; baking https://clawvisor.local into the
	// response would mislead non-local deployments.
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":               "verifier_rejected_fallback",
		"drift_id":             driftID,
		"option":               "justify",
		"verifier_explanation": explanation,
		"next_step": "The verifier did not accept your justification. Clawvisor surfaced a clean one-off approval prompt to the user. Poll GET /control/scope-drift/" +
			driftID + " on the same control host until outcome=\"succeeded\" or \"denied\". On succeeded, re-emit the original tool call.",
	})
}

// Get handles GET /control/scope-drift/{id} — returns the current
// status of a drift so the agent can poll the outcome of an async
// option (e.g. one-off pending user approval).
func (h *ScopeDriftHandler) Get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agent := middleware.AgentFromContext(ctx)
	if agent == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error":   "unauthorized",
			"message": "missing agent context",
		})
		return
	}
	if h.Registry == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":   "scope_drift_registry_unavailable",
			"message": "scope drift registry is not configured on this daemon",
		})
		return
	}

	driftID := strings.TrimSpace(r.PathValue("id"))
	if driftID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "drift_id_required",
			"message": "drift_id path segment is required",
		})
		return
	}

	drift, err := h.Registry.Get(ctx, driftID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error":   "drift_not_found",
			"message": "no scope drift with that id (it may have expired)",
		})
		return
	}
	if drift.AgentID != agent.ID {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":   "drift_not_owned",
			"message": "this drift was not minted for your agent",
		})
		return
	}

	writeJSON(w, http.StatusOK, drift)
}
