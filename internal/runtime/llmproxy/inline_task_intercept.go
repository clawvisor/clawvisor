package llmproxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	runtimepolicy "github.com/clawvisor/clawvisor/internal/runtime/policy"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/internal/taskrisk"
)

// inlineTaskApprovalTTL is how long the user has to type yes/no
// after the model has emitted the task definition. Bounded so the second
// gesture sits within the same approval cache window as the first; if
// the user walks away mid-flight both holds expire together and the
// model's next turn naturally re-prompts.
const inlineTaskApprovalTTL = 10 * time.Minute

// InlineSurfaceQueryParam is the query-string flag the model adds to
// POST /api/control/tasks to opt in to the inline-approval flow when there
// is no prior `task` reply (e.g. the agent knows the user is sitting
// in the chat and prefers to approve there). Absent + no awaiting-
// definition hold = the existing async dashboard path.
const InlineSurfaceQueryParam = "surface"

// InlineSurfaceQueryValue is the value of the surface query parameter
// the model passes to opt in to the inline-approval flow.
const InlineSurfaceQueryValue = "inline"

// maybeInterceptInlineTaskDefinition is the postprocess hook that
// routes a model-emitted POST /api/control/tasks tool_use through the
// inline approval flow.
//
// The single opt-in signal is a `?surface=inline` query parameter on
// the URL: the agent is declaring "the user is here, approve inline."
// (An earlier state-signal path keyed on a prior
// StageAwaitingTaskDefinition hold was removed once
// RewriteTaskApprovalReply switched to fully Resolving the original
// tool hold on "task" reply — no awaiting-definition hold ever exists
// in production traffic for the intercept to observe.)
//
// When the query signal fires, the model never actually POSTs the
// task — the tool_use_result is replaced with a rendered yes/no
// prompt, and the user's next "yes" creates the task pre-approved.
//
// Returns (_, false) when the signal is absent, the body fails to
// parse, or the path isn't POST /api/control/tasks — callers should
// fall through to the regular control-rewrite path so headless task
// creation still routes through the dashboard handler unchanged.
func maybeInterceptInlineTaskDefinition(
	req *http.Request,
	cfg PostprocessConfig,
	audit func(decision, outcome, reason string),
	trace func(event string, kv ...any),
	provider conversation.Provider,
	tu conversation.ToolUse,
	call ControlCall,
) (conversation.ToolUseVerdict, bool) {
	if cfg.PendingApprovals == nil {
		return conversation.ToolUseVerdict{}, false
	}
	// Only intercept POSTs to /api/control/tasks; the dashboard handler
	// covers GETs (skill catalog) and other control paths. Exact
	// path equality — HasSuffix would also match attacker-shaped paths
	// like /foo/bar/api/control/tasks if the host check ever loosened.
	if !strings.EqualFold(call.Method, "POST") || call.URL.Path != "/api/control/tasks" {
		return conversation.ToolUseVerdict{}, false
	}

	// Query signal: agent explicitly opted in via ?surface=inline. This
	// is the only signal we honor in production — the older "state
	// signal" branch (a prior StageAwaitingTaskDefinition hold from a
	// "task" reply) is unreachable now that RewriteTaskApprovalReply
	// fully Resolves the original tool hold rather than transitioning
	// its stage. taskCreationPrompt teaches the model to include
	// ?surface=inline, so compliant traffic flows through here; the
	// query-less path correctly falls through to the dashboard rewrite.
	// Both key and value match case-SENSITIVELY: `url.Values.Get` is
	// case-sensitive on the key, and harnesses emit the exact
	// surface=inline string we teach them in taskCreationPrompt.
	// Mixed-case (Surface=INLINE) is not a shape we promise to honor;
	// keeping symmetric strictness avoids future-reader surprise.
	querySignal := call.URL.Query().Get(InlineSurfaceQueryParam) == InlineSurfaceQueryValue
	if !querySignal {
		return conversation.ToolUseVerdict{}, false
	}

	// On the failure paths below, we audit with decision="fallthrough"
	// rather than "block" because the function returns (verdict{}, false)
	// and the caller proceeds to the regular control-rewrite path.
	// Emitting "block" here would record a misleading audit row paired
	// with whatever decision the dashboard rewriter ultimately reaches
	// for the same tool_use — operators chasing inline-task failures
	// would see a "block" followed by an unrelated outcome for the
	// same request.
	bodyBytes, ok := controlTaskBodyFromInput(tu.Input)
	if !ok || len(bodyBytes) == 0 {
		audit("fallthrough", "inline_task_body_missing", "POST /api/control/tasks had no body; deferring to dashboard rewrite")
		return conversation.ToolUseVerdict{}, false
	}
	parsed := &runtimetasks.TaskCreateRequest{}
	if err := json.Unmarshal(bodyBytes, parsed); err != nil {
		audit("fallthrough", "inline_task_body_malformed", err.Error())
		return conversation.ToolUseVerdict{}, false
	}
	if strings.TrimSpace(parsed.Purpose) == "" {
		audit("fallthrough", "inline_task_missing_purpose", "task body missing purpose; deferring to dashboard rewrite")
		return conversation.ToolUseVerdict{}, false
	}
	env := runtimetasks.Envelope{
		ExpectedTools:          parsed.ExpectedTools,
		ExpectedEgress:         parsed.ExpectedEgress,
		RequiredCredentials:    parsed.RequiredCredentials,
		IntentVerificationMode: parsed.IntentVerificationMode,
		ExpectedUse:            parsed.ExpectedUse,
		SchemaVersion:          parsed.SchemaVersion,
	}
	if env.SchemaVersion == 0 {
		env.SchemaVersion = 2
	}
	if env.IntentVerificationMode == "" {
		env.IntentVerificationMode = "strict"
	}
	if issues := runtimepolicy.ValidateTaskEnvelope(env); len(issues) > 0 {
		audit("fallthrough", "inline_task_invalid_envelope", inlineTaskValidationReason(issues)+"; deferring to dashboard rewrite")
		return conversation.ToolUseVerdict{}, false
	}

	now := time.Now().UTC()
	innerHold, holdErr := cfg.PendingApprovals.Hold(req.Context(), PendingLiteApproval{
		UserID:         cfg.AgentUserID,
		AgentID:        cfg.AgentID,
		Provider:       provider,
		ConversationID: cfg.ConversationID,
		ToolUse:        tu,
		Reason:         "inline task creation awaiting user approval",
		Stage:          StageAwaitingTaskApproval,
		TaskDefinition: parsed,
		CreatedAt:      now,
		ExpiresAt:      now.Add(inlineTaskApprovalTTL),
	})
	if holdErr != nil {
		audit("block", "inline_task_hold_failed", holdErr.Error())
		return conversation.ToolUseVerdict{}, false
	}

	audit("approve", "pending", "inline_task_pending_approval: awaiting user yes/no on inline task definition (query)")
	trace("inline_task.held",
		"approval_id", innerHold.Pending.ID,
		"purpose", parsed.Purpose,
		"signal", "query",
	)
	assessment := assessInlineTaskRisk(req, cfg, parsed, env, trace)
	return conversation.ToolUseVerdict{
		Allowed:        false,
		Reason:         "Clawvisor: awaiting inline task approval",
		SubstituteWith: renderTaskApprovalPromptWithRisk(parsed, innerHold.Pending.ID, assessment),
	}, true
}

// assessInlineTaskRisk runs the LLM-backed risk assessor (when configured) and
// merges its verdict with the deterministic envelope-shape policy. The
// envelope policy is the floor — it catches structural risk (wildcard hosts,
// regex matchers, intent-verification off) that the LLM may underweight or
// miss. The LLM verdict supplies the user-facing explanation and any extra
// factors when its level is at least as high as the floor.
//
// Returns the deterministic envelope assessment alone when the assessor is
// nil, returns nil-from-LLM (e.g. spend cap exhausted), or returns an
// "unknown"/error result. This keeps the inline approval prompt rendering
// even if the LLM call fails — the user still sees the deterministic risk
// read, just without the LLM's explanation.
func assessInlineTaskRisk(
	req *http.Request,
	cfg PostprocessConfig,
	parsed *runtimetasks.TaskCreateRequest,
	env runtimetasks.Envelope,
	trace func(event string, kv ...any),
) *taskrisk.RiskAssessment {
	envelopeAssessment := runtimepolicy.AssessTaskEnvelope(parsed.Purpose, env)
	if cfg.TaskRiskAssessor == nil {
		return envelopeAssessment
	}

	llmVerdict := cfg.TaskRiskAssessor.AssessEnvelope(req.Context(), TaskRiskAssessRequest{
		Purpose:                parsed.Purpose,
		AgentName:              cfg.AgentName,
		UserID:                 cfg.AgentUserID,
		ExpectedTools:          env.ExpectedTools,
		ExpectedEgress:         env.ExpectedEgress,
		RequiredCredentials:    env.RequiredCredentials,
		IntentVerificationMode: env.IntentVerificationMode,
		ExpectedUse:            env.ExpectedUse,
	})
	if llmVerdict == nil || strings.EqualFold(strings.TrimSpace(llmVerdict.RiskLevel), "unknown") {
		trace("inline_task.risk_llm_unavailable")
		return envelopeAssessment
	}

	llmAssessment := &taskrisk.RiskAssessment{
		RiskLevel:   llmVerdict.RiskLevel,
		Explanation: llmVerdict.Explanation,
		Factors:     llmVerdict.Factors,
	}
	return mergeInlineRisk(llmAssessment, envelopeAssessment)
}

// mergeInlineRisk picks the higher of the two risk levels and prefers the
// LLM explanation when it set the ceiling; the envelope policy supplies the
// explanation only when it raised the level above the LLM's read. Factors
// from both are concatenated.
func mergeInlineRisk(llm, envelope *taskrisk.RiskAssessment) *taskrisk.RiskAssessment {
	if llm == nil {
		return envelope
	}
	if envelope == nil {
		return llm
	}
	out := *llm
	if riskRank(envelope.RiskLevel) > riskRank(llm.RiskLevel) {
		out.RiskLevel = envelope.RiskLevel
		if envelope.Explanation != "" {
			out.Explanation = envelope.Explanation
		}
	}
	out.Factors = append(append([]string{}, llm.Factors...), envelope.Factors...)
	out.Conflicts = append(append([]taskrisk.ConflictDetail{}, llm.Conflicts...), envelope.Conflicts...)
	return &out
}

func riskRank(level string) int {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "low":
		return 0
	case "medium":
		return 1
	case "high":
		return 2
	case "critical":
		return 3
	}
	return -1
}

func inlineTaskValidationReason(issues []runtimepolicy.ValidationIssue) string {
	var parts []string
	for _, issue := range issues {
		parts = append(parts, issue.Field+": "+issue.Message)
	}
	return strings.Join(parts, "; ")
}

// controlTaskBodyFromInput extracts the POST body from the tool_use's
// structured or command form. Mirrors ParseControlToolUseWithBase's
// reachable shapes but returns just the body bytes — the URL has
// already been classified by the caller. Routes through the shared
// parseControlCmd helper so both single-statement (curl with stdin
// heredoc) and multi-statement (cat-heredoc + curl --data @file)
// shapes resolve to the actual body bytes.
func controlTaskBodyFromInput(in json.RawMessage) ([]byte, bool) {
	if len(in) == 0 {
		return nil, false
	}
	// Structured form: { "url": "...", "method": "POST", "body": ... }
	var structured struct {
		Body json.RawMessage `json:"body,omitempty"`
	}
	if err := json.Unmarshal(in, &structured); err == nil && len(structured.Body) > 0 {
		var bodyString string
		if json.Unmarshal(structured.Body, &bodyString) == nil {
			return []byte(bodyString), true
		}
		return structured.Body, true
	}
	// Bash form: { "cmd"/"command": "..." }. Re-use the same parser the
	// rewrite path uses so single-stmt and cat-then-curl resolve
	// identically; controlPartsFromCommandInput already handles
	// @path → heredoc body substitution.
	if _, _, body, ok := controlPartsFromCommandInput(in, ""); ok && len(body) > 0 {
		return body, true
	}
	return nil, false
}
