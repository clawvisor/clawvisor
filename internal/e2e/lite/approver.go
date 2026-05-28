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
	case "tool_use_block":
		// Tool-use blocks escalate to a task definition by default.
		// A future scenario could opt in to "deny" via a new
		// approval kind (e.g. "tool_use" rule), but for now there's
		// no use case for it in the library.
		return "task", "escalate"
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
