package lite

import (
	"strings"

	"github.com/clawvisor/clawvisor/internal/e2e/lite/drivers"
)

// scriptedApprover implements drivers.Approver from a scenario's
// `approvals:` block. The match is per-kind today (task_create).
// Tool-use blocks default to escalation ("task") unless an explicit
// rule says otherwise.
type scriptedApprover struct {
	cfg Approvals
}

func NewScriptedApprover(cfg Approvals) drivers.Approver {
	return &scriptedApprover{cfg: cfg}
}

func (a *scriptedApprover) Reply(kind, _ string) (reply, outcomeLabel string) {
	switch kind {
	case "task_approval":
		return a.taskApprovalReply()
	case "scope_drift_one_off_approval":
		// Same yes/no gesture as task_approval, but matched against a
		// separate rule kind so a scenario can approve task_create
		// while denying one-offs (or vice versa). Falls back to the
		// scenario's Default policy when no rule names
		// "scope_drift_one_off".
		return a.scopeDriftOneOffApprovalReply()
	case "scope_drift_menu":
		// The four-option menu is the substituted tool_use result,
		// not a user-approval gesture. The agent's next turn is
		// supposed to either retry the original tool, pick option
		// (a)/(b) via a /control/tasks{,/expand} call, or emit a
		// <clawvisor:decision> markup for (c)/(d). We forward a
		// brief nudge so the driver loop keeps the conversation
		// going — without this the driver would treat the menu as a
		// final answer and end the step.
		return "Read the scope-drift menu above and proceed: pick option (a) expand, (b) new task, (c) one-off markup, or (d) justify markup.", "continue"
	case "tool_use_block":
		// Tool-use blocks escalate to a task definition by default.
		// A future scenario could opt in to "deny" via a new
		// approval kind (e.g. "tool_use" rule), but for now there's
		// no use case for it in the library.
		return "task", "escalate"
	case "tool_use_hard_block":
		// A hard-block reply tells the agent to read the proxy's
		// stated reason (already in its conversation history) and
		// retry with a different tool_use shape. Models the natural
		// "your last command got refused — try a different approach"
		// nudge a real user would give.
		return "The proxy rejected that tool_use — read the block reason above and retry with a different shape (one credentialed curl per tool_use if you were chaining).", "retry"
	}
	return "", ""
}

func (a *scriptedApprover) taskApprovalReply() (reply, outcomeLabel string) {
	resolution := a.matchKind("task_create")
	if resolution == "" {
		resolution = a.cfg.Default
	}
	if isAllow(resolution) {
		return "yes", "approve"
	}
	if isDeny(resolution) {
		return "no", "deny"
	}
	// Unmatched + empty default → fall back to deny so missing-rule
	// scenarios don't quietly succeed.
	return "no", "deny"
}

func (a *scriptedApprover) scopeDriftOneOffApprovalReply() (reply, outcomeLabel string) {
	resolution := a.matchKind("scope_drift_one_off")
	if resolution == "" {
		resolution = a.cfg.Default
	}
	if isAllow(resolution) {
		return "yes", "approve"
	}
	if isDeny(resolution) {
		return "no", "deny"
	}
	return "no", "deny"
}

func (a *scriptedApprover) matchKind(kind string) string {
	for _, rule := range a.cfg.Rules {
		if strings.EqualFold(rule.Match.Kind, kind) || rule.Match.Kind == "" {
			return rule.Resolution
		}
	}
	return ""
}

func isAllow(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "allow_session", "allow_once", "allow_always", "approve":
		return true
	}
	return false
}

func isDeny(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "deny":
		return true
	}
	return false
}
