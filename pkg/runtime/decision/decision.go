package decision

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	runtimepolicy "github.com/clawvisor/clawvisor/internal/runtime/policy"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// NoPerCallReasonSentinel is the Reason value we send to the intent
// verifier when the agent's harness does not collect a per-call
// rationale at the tool layer (e.g., Codex's shell tool sends argv
// only, with no `description` field like Claude Code's Bash). The
// verifier prompt recognizes this exact string and skips the
// reason_coherence check in that case, evaluating the request on
// params and task scope alone. Keep this string in sync with the
// matching block in internal/intent/prompts.go.
const NoPerCallReasonSentinel = "<no per-call rationale: harness tool schema does not collect one>"

type VerdictKind string

const (
	VerdictAllow         VerdictKind = "allow"
	VerdictDeny          VerdictKind = "deny"
	VerdictNeedsApproval VerdictKind = "needs_approval"
)

type EvaluationPosture string

const (
	PostureEnforce EvaluationPosture = "enforce"
	PostureObserve EvaluationPosture = "observe"
)

type DenyReason string

const (
	DenyReasonNone   DenyReason = ""
	DenyReasonRule   DenyReason = "rule"
	DenyReasonIntent DenyReason = "intent"
)

type ObservationEffect string

const (
	ObservationNone        ObservationEffect = ""
	ObservationWouldBlock  ObservationEffect = "would_block"
	ObservationWouldReview ObservationEffect = "would_review"
)

type DecisionSource string

const (
	SourceRuleAllow          DecisionSource = "rule_allow"
	SourceRuleDeny           DecisionSource = "rule_deny"
	SourceRuleReview         DecisionSource = "rule_review"
	SourceTaskScope          DecisionSource = "task_scope"
	SourceTaskScopeMissing   DecisionSource = "task_scope_missing"
	SourceTaskScopeAmbiguous DecisionSource = "task_scope_ambiguous"
	SourceIntentRefusal      DecisionSource = "intent_refusal"
)

type TargetRequest struct {
	Host    string
	Method  string
	Path    string
	Query   map[string]any
	Body    map[string]any
	Headers map[string]string
}

type IntentVerifier interface {
	Verify(ctx context.Context, req IntentVerifyRequest) (*IntentVerdict, error)
}

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

type IntentVerdict struct {
	Allow       bool
	Explanation string
}

type AuthorizationInput struct {
	ToolUse conversation.ToolUse

	UserID  string
	AgentID string

	Posture EvaluationPosture

	Target TargetRequest

	Service string
	Action  string

	CandidateTasks []*store.Task
	ToolRules      []*store.RuntimePolicyRule
	EgressRules    []*store.RuntimePolicyRule

	IntentVerifier IntentVerifier

	// AllowMissingScope returns allow when no rule or task scope matches.
	// Use this only for rule-only evaluation paths where task scope is not
	// available for the tool surface being checked.
	AllowMissingScope bool
}

type AuthorizationDecision struct {
	Kind              VerdictKind
	Reason            string
	DenyReason        DenyReason
	ObservationEffect ObservationEffect

	Rule   *store.RuntimePolicyRule
	Task   *store.Task
	Action *store.TaskAction

	Source DecisionSource
}

type DecisionFingerprint struct {
	Source DecisionSource

	RuleID string
	TaskID string

	Service string
	Action  string

	TargetHost   string
	TargetMethod string
	TargetPath   string

	PolicyRevision string
	TaskRevision   string
}

func EvaluateAuthorization(ctx context.Context, in AuthorizationInput) (AuthorizationDecision, error) {
	posture := in.Posture
	if posture == "" {
		posture = PostureEnforce
	}
	toolInput := decodeToolInput(in.ToolUse.Input)

	denyRule, fallbackRule, err := selectPolicyRules(in, toolInput)
	if err != nil {
		return AuthorizationDecision{}, err
	}
	if denyRule != nil {
		return decisionForRule(denyRule, posture), nil
	}

	if in.Service != "" && in.Action != "" {
		decision, err := evaluateServiceActionScope(ctx, in, posture, toolInput)
		if err != nil {
			return AuthorizationDecision{}, err
		}
		if decision.Source != SourceTaskScopeMissing {
			return decision, nil
		}
		// Catalog resolved a (service, action) but no task declared
		// `authorized_actions` for it. Before falling through to
		// approval-required, give expected_tools_json a chance to
		// match — the lite-proxy's taskCreationPrompt tells the model
		// to declare scope by tool_name, so a task created via that
		// path will only have expected_tools populated.
		if match, err := runtimepolicy.MatchToolCall(in.CandidateTasks, in.ToolUse.Name, toolInput); err != nil {
			return AuthorizationDecision{}, err
		} else if match != nil {
			task := taskByID(in.CandidateTasks, match.TaskID)
			if reason, ok, err := runToolIntentVerify(ctx, in, task, match, toolInput); err != nil || !ok {
				if err != nil {
					return AuthorizationDecision{}, err
				}
				return AuthorizationDecision{
					Kind:       VerdictNeedsApproval,
					Reason:     firstNonEmpty(reason, "intent verifier refused this tool call"),
					DenyReason: DenyReasonIntent,
					Task:       task,
					Source:     SourceIntentRefusal,
				}, nil
			}
			return AuthorizationDecision{
				Kind:   VerdictAllow,
				Reason: firstNonEmpty(match.Item.Why, "matched expected tool scope"),
				Task:   task,
				Source: SourceTaskScope,
			}, nil
		}
		if fallbackRule != nil {
			return decisionForRule(fallbackRule, posture), nil
		}
		return decision, nil
	}

	if in.Target.Host != "" {
		match, err := runtimepolicy.MatchEgressRequest(in.CandidateTasks, runtimepolicy.EgressRequest{
			Host:    in.Target.Host,
			Method:  in.Target.Method,
			Path:    in.Target.Path,
			Query:   in.Target.Query,
			Body:    in.Target.Body,
			Headers: in.Target.Headers,
		})
		if err != nil {
			return AuthorizationDecision{}, err
		}
		if match != nil {
			return AuthorizationDecision{
				Kind:   VerdictAllow,
				Reason: firstNonEmpty(match.Item.Why, "matched expected egress scope"),
				Task:   taskByID(in.CandidateTasks, match.TaskID),
				Source: SourceTaskScope,
			}, nil
		}
	}

	match, err := runtimepolicy.MatchToolCall(in.CandidateTasks, in.ToolUse.Name, toolInput)
	if err != nil {
		return AuthorizationDecision{}, err
	}
	if match != nil {
		task := taskByID(in.CandidateTasks, match.TaskID)
		if reason, ok, err := runToolIntentVerify(ctx, in, task, match, toolInput); err != nil || !ok {
			if err != nil {
				return AuthorizationDecision{}, err
			}
			return AuthorizationDecision{
				Kind:       VerdictNeedsApproval,
				Reason:     firstNonEmpty(reason, "intent verifier refused this tool call"),
				DenyReason: DenyReasonIntent,
				Task:       task,
				Source:     SourceIntentRefusal,
			}, nil
		}
		return AuthorizationDecision{
			Kind:   VerdictAllow,
			Reason: firstNonEmpty(match.Item.Why, "matched expected tool scope"),
			Task:   task,
			Source: SourceTaskScope,
		}, nil
	}

	if fallbackRule != nil {
		return decisionForRule(fallbackRule, posture), nil
	}

	if in.AllowMissingScope {
		return AuthorizationDecision{
			Kind:   VerdictAllow,
			Reason: "no matching rule",
			Source: SourceTaskScopeMissing,
		}, nil
	}
	return reviewDecision(posture, SourceTaskScopeMissing, "no matching task scope"), nil
}

func selectPolicyRules(in AuthorizationInput, toolInput map[string]any) (*store.RuntimePolicyRule, *store.RuntimePolicyRule, error) {
	toolRule, err := runtimepolicy.MatchRuntimePolicyTool(in.ToolRules, in.AgentID, in.ToolUse.Name, toolInput)
	if err != nil {
		return nil, nil, err
	}
	var egressRule *store.RuntimePolicyRule
	if in.Target.Host != "" {
		egressRule, err = runtimepolicy.MatchRuntimePolicyEgress(in.EgressRules, in.AgentID, runtimepolicy.EgressRequest{
			Host:    in.Target.Host,
			Method:  in.Target.Method,
			Path:    in.Target.Path,
			Query:   in.Target.Query,
			Body:    in.Target.Body,
			Headers: in.Target.Headers,
		})
		if err != nil {
			return nil, nil, err
		}
	}
	matched := []*store.RuntimePolicyRule{toolRule, egressRule}
	return strictestRuleMatching(matched, isDenyRule), strictestRuleMatching(matched, isNonDenyRule), nil
}

func strictestRuleMatching(rules []*store.RuntimePolicyRule, keep func(*store.RuntimePolicyRule) bool) *store.RuntimePolicyRule {
	var picked *store.RuntimePolicyRule
	for _, rule := range rules {
		if !keep(rule) {
			continue
		}
		picked = stricterRule(picked, rule)
	}
	return picked
}

func isDenyRule(rule *store.RuntimePolicyRule) bool {
	return rule != nil && strings.EqualFold(strings.TrimSpace(rule.Action), "deny")
}

func isNonDenyRule(rule *store.RuntimePolicyRule) bool {
	return rule != nil && !strings.EqualFold(strings.TrimSpace(rule.Action), "deny")
}

func stricterRule(a, b *store.RuntimePolicyRule) *store.RuntimePolicyRule {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if crossPlaneRuleRank(b.Action) > crossPlaneRuleRank(a.Action) {
		return b
	}
	return a
}

func crossPlaneRuleRank(action string) int {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "deny":
		return 3
	case "review":
		return 2
	case "allow":
		return 1
	default:
		return 0
	}
}

func decisionForRule(rule *store.RuntimePolicyRule, posture EvaluationPosture) AuthorizationDecision {
	switch strings.ToLower(strings.TrimSpace(rule.Action)) {
	case "allow":
		return AuthorizationDecision{
			Kind:   VerdictAllow,
			Reason: firstNonEmpty(rule.Reason, "runtime allow rule matched this tool call"),
			Rule:   rule,
			Source: SourceRuleAllow,
		}
	case "deny":
		if posture == PostureObserve {
			return AuthorizationDecision{
				Kind:              VerdictAllow,
				Reason:            firstNonEmpty(rule.Reason, "observation mode: runtime deny rule would block this tool call"),
				DenyReason:        DenyReasonRule,
				ObservationEffect: ObservationWouldBlock,
				Rule:              rule,
				Source:            SourceRuleDeny,
			}
		}
		return AuthorizationDecision{
			Kind:       VerdictDeny,
			Reason:     firstNonEmpty(rule.Reason, "runtime deny rule blocked this tool call"),
			DenyReason: DenyReasonRule,
			Rule:       rule,
			Source:     SourceRuleDeny,
		}
	case "review":
		return reviewDecisionWithRule(posture, rule)
	default:
		return reviewDecisionWithRule(posture, rule)
	}
}

func reviewDecisionWithRule(posture EvaluationPosture, rule *store.RuntimePolicyRule) AuthorizationDecision {
	reason := "runtime review rule matched this tool call"
	if rule != nil {
		reason = firstNonEmpty(rule.Reason, reason)
	}
	decision := reviewDecision(posture, SourceRuleReview, reason)
	decision.Rule = rule
	return decision
}

func evaluateServiceActionScope(ctx context.Context, in AuthorizationInput, posture EvaluationPosture, toolInput map[string]any) (AuthorizationDecision, error) {
	classification := runtimepolicy.ClassifyGatewayRequest(in.CandidateTasks, in.AgentID, in.Service, "", in.Action)
	switch classification.Kind {
	case runtimepolicy.ClassificationBelongsToExistingTask:
		task := classification.MatchedTask
		action := findMatchingAction(task, in.Service, in.Action)
		if reason, ok, err := runIntentVerify(ctx, in, task, action, toolInput); err != nil || !ok {
			if err != nil {
				return AuthorizationDecision{}, err
			}
			return AuthorizationDecision{
				Kind:       VerdictDeny,
				Reason:     firstNonEmpty(reason, "intent verifier refused this tool call"),
				DenyReason: DenyReasonIntent,
				Task:       task,
				Action:     action,
				Source:     SourceIntentRefusal,
			}, nil
		}
		return AuthorizationDecision{
			Kind:   VerdictAllow,
			Reason: firstNonEmpty(reasonForTask(task), "matched task scope"),
			Task:   task,
			Action: action,
			Source: SourceTaskScope,
		}, nil
	case runtimepolicy.ClassificationAmbiguous:
		return reviewDecision(posture, SourceTaskScopeAmbiguous, "ambiguous: multiple active tasks cover this action"), nil
	default:
		return reviewDecision(posture, SourceTaskScopeMissing, "no matching task scope"), nil
	}
}

func runIntentVerify(ctx context.Context, in AuthorizationInput, task *store.Task, action *store.TaskAction, params map[string]any) (string, bool, error) {
	if in.IntentVerifier == nil || action == nil {
		return "", true, nil
	}
	// Normalize before comparing — verification mode is sourced from YAML
	// fixtures and store fields that have been observed with mixed casing
	// or leading/trailing whitespace ("Off ", "LENIENT", etc.).
	mode := strings.TrimSpace(strings.ToLower(action.Verification))
	if mode == "off" {
		return "", true, nil
	}
	purpose := ""
	taskID := ""
	if task != nil {
		purpose = task.Purpose
		taskID = task.ID
	}
	verdict, err := in.IntentVerifier.Verify(ctx, IntentVerifyRequest{
		TaskPurpose: purpose,
		ExpectedUse: action.ExpectedUse,
		Service:     in.Service,
		Action:      in.Action,
		Params:      params,
		Reason:      resolveToolReason(in.ToolUse.Name, params),
		TaskID:      taskID,
		Lenient:     mode == "lenient",
	})
	if err != nil {
		return "", false, err
	}
	if verdict == nil || verdict.Allow {
		if verdict == nil {
			return "", true, nil
		}
		return verdict.Explanation, true, nil
	}
	return verdict.Explanation, false, nil
}

func runToolIntentVerify(ctx context.Context, in AuthorizationInput, task *store.Task, match *runtimepolicy.ToolMatch, params map[string]any) (string, bool, error) {
	if in.IntentVerifier == nil || match == nil {
		return "", true, nil
	}
	mode := ""
	purpose := ""
	expectedUse := ""
	taskID := match.TaskID
	if task != nil {
		mode = strings.TrimSpace(strings.ToLower(task.IntentVerificationMode))
		purpose = task.Purpose
		expectedUse = task.ExpectedUse
		taskID = task.ID
	}
	if mode == "off" {
		return "", true, nil
	}
	if expectedUse == "" {
		expectedUse = match.Item.Why
	}
	// The verifier expects Reason to be the agent's per-call rationale —
	// "why am I making THIS specific call." Do NOT fall back to
	// match.Item.Why: that's the task's pre-declared scope description
	// (same text we already pass as ExpectedUse), and the verifier
	// correctly flags a verbatim copy as "instructions/procedural steps
	// rather than a 'why' clause." Pull a fresh rationale from the tool
	// input instead; harnesses like Claude Code prompt the model for a
	// short `description` on each Bash call for exactly this purpose.
	verdict, err := in.IntentVerifier.Verify(ctx, IntentVerifyRequest{
		TaskPurpose: purpose,
		ExpectedUse: expectedUse,
		Service:     "runtime.tool",
		Action:      in.ToolUse.Name,
		Params:      params,
		Reason:      resolveToolReason(in.ToolUse.Name, params),
		TaskID:      taskID,
		Lenient:     mode == "lenient",
	})
	if err != nil {
		return "", false, err
	}
	if verdict == nil || verdict.Allow {
		if verdict == nil {
			return "", true, nil
		}
		return verdict.Explanation, true, nil
	}
	return verdict.Explanation, false, nil
}

func reviewDecision(posture EvaluationPosture, source DecisionSource, reason string) AuthorizationDecision {
	if posture == PostureObserve {
		return AuthorizationDecision{
			Kind:              VerdictAllow,
			Reason:            firstNonEmpty(reason, "observation mode: tool use would require runtime approval"),
			ObservationEffect: ObservationWouldReview,
			Source:            source,
		}
	}
	return AuthorizationDecision{
		Kind:   VerdictNeedsApproval,
		Reason: firstNonEmpty(reason, "tool call requires runtime approval"),
		Source: source,
	}
}

func Fingerprint(decision AuthorizationDecision, in AuthorizationInput) DecisionFingerprint {
	fp := DecisionFingerprint{
		Source:       decision.Source,
		Service:      in.Service,
		Action:       in.Action,
		TargetHost:   in.Target.Host,
		TargetMethod: in.Target.Method,
		TargetPath:   in.Target.Path,
	}
	if decision.Rule != nil {
		fp.RuleID = decision.Rule.ID
		fp.PolicyRevision = fmt.Sprint(decision.Rule.UpdatedAt.UnixNano())
	}
	if decision.Task != nil {
		fp.TaskID = decision.Task.ID
		fp.TaskRevision = fmt.Sprint(decision.Task.SchemaVersion)
	}
	return fp
}

func EquivalentFingerprint(a, b DecisionFingerprint) bool {
	return a == b
}

func findMatchingAction(task *store.Task, service, action string) *store.TaskAction {
	if task == nil {
		return nil
	}
	for i := range task.AuthorizedActions {
		a := &task.AuthorizedActions[i]
		if a.Service == service && (a.Action == action || a.Action == "*") {
			return a
		}
	}
	return nil
}

func taskByID(tasks []*store.Task, id string) *store.Task {
	for _, task := range tasks {
		if task != nil && task.ID == id {
			return task
		}
	}
	return nil
}

func reasonForTask(task *store.Task) string {
	if task == nil {
		return ""
	}
	return "matched task " + task.ID
}

func decodeToolInput(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// toolsExpectingPerCallRationale enumerates tool names whose harness
// schema requires a per-call rationale field. For these tools, an
// absent or sentinel-stuffed rationale field is a non-compliance
// signal (or a bypass attempt), NOT a harness limitation — the
// verifier's coherence check must still run rather than be skipped
// via NoPerCallReasonSentinel.
//
// Without this list, a model on Claude Code (whose Bash schema
// requires `description`) could trivially defeat the coherence check
// by omitting the description field: the proxy would substitute the
// system sentinel, the verifier would treat it as "harness has no
// rationale" and skip the check entirely.
//
// Lowercase + trimmed. Keep in sync with the tool schemas actual
// harnesses enforce. Tools that legitimately ship without a
// rationale convention (Codex's argv-only `shell`/`exec_command`)
// stay out of this set — for them the sentinel correctly signals a
// genuine harness limitation.
var toolsExpectingPerCallRationale = map[string]bool{
	"bash":     true, // Claude Code: `description` is required by tool schema
	"webfetch": true, // Claude Code: `prompt` is the per-call rationale
}

// resolveToolReason picks the per-call Reason to send to the
// verifier. Tools known to require a rationale and lacking one get
// an empty string (verifier evaluates coherence and likely flags
// insufficient — desired). Tools without a known rationale
// convention get the NoPerCallReasonSentinel so the verifier prompt
// knows to skip the coherence check.
func resolveToolReason(toolName string, params map[string]any) string {
	if reason := perCallToolReason(params); reason != "" {
		return reason
	}
	if toolsExpectingPerCallRationale[strings.ToLower(strings.TrimSpace(toolName))] {
		return ""
	}
	return NoPerCallReasonSentinel
}

// perCallToolReason pulls a per-call rationale out of tool input. Claude
// Code's Bash tool prompts the model for a short `description` on every
// call; Codex and other harnesses use similar conventions. When the
// agent supplies one we forward it to the verifier as the Reason — it's
// the only field meant to capture "why THIS call, right now," distinct
// from the task's pre-declared scope.
//
// Keys are limited to names that semantically mean "rationale": a
// generic `purpose` field on a task-creation or evaluator tool would
// be USER data (a task's purpose, an evaluator's subject), not the
// agent's why-clause for this specific call — feeding that to the
// verifier risks false coherence findings.
func perCallToolReason(params map[string]any) string {
	if params == nil {
		return ""
	}
	// First pass: detect a sentinel forgery in ANY inspected field. A
	// model that drops the sentinel into `description` and a plausible
	// rationale into `reason` would otherwise still get a rationale
	// forwarded — the bypass attempt itself is the signal we care
	// about, and "advance past sentinel to the next field" lets the
	// model surface the wrong-but-not-flagged field as the rationale
	// by knowing this lookup order. Fall through to the system
	// sentinel instead.
	for _, key := range []string{"description", "reason", "rationale"} {
		if v, ok := params[key].(string); ok && strings.TrimSpace(v) == NoPerCallReasonSentinel {
			return ""
		}
	}
	for _, key := range []string{"description", "reason", "rationale"} {
		if v, ok := params[key].(string); ok {
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
