package llmproxy

import (
	"context"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
	"github.com/clawvisor/clawvisor/pkg/runtime/toolnames"
)

// EvaluateTriggerMissAuthorization handles the trigger-miss
// authorization branch of newToolUseEvaluator (postprocess.go
// ~968–1085). Exposed so the handler-side pipeline factory's
// TriggerMissAuthorizer can call it directly instead of delegating
// back through BuildLegacyToolUseEvaluator.
//
// The function:
//   - Detects read-only shell + sensitive-path special cases.
//   - When CandidateTasks / ToolRules / EgressRules are wired (or a
//     sensitive shell path forces evaluation), runs
//     runtimedecision.EvaluateAuthorization.
//   - Maps VerdictAllow / Deny / NeedsApproval into a conversation
//     verdict, including PendingApprovals.Hold for the approval path
//     and ApprovalPrompt rendering for the substitute text.
//   - When the decision engine isn't wired, returns the legacy
//     "pass_through" allow.
//
// emit receives an audit row at each decision point — the row shape
// matches the legacy newToolUseEvaluator's audit() closure exactly.
//
// Returns the conversation.ToolUseVerdict the rewriter consumes.
func EvaluateTriggerMissAuthorization(
	ctx context.Context,
	cfg PostprocessConfig,
	provider conversation.Provider,
	tu conversation.ToolUse,
	v inspector.Verdict,
	emit func(BufferedAudit),
) conversation.ToolUseVerdict {
	audit := func(decision, outcome, reason, taskID string) {
		if emit == nil {
			return
		}
		emit(BufferedAudit{
			ToolUse:  tu,
			Verdict:  v,
			Decision: decision,
			Outcome:  outcome,
			Reason:   reason,
			TaskID:   taskID,
		})
	}

	// Read-only shell + sensitive-path detection. Mirrors the legacy
	// gates exactly: readOnlyShellCommand is true only when (a) the
	// tool is a shell tool name, (b) read-only-shell-commands-allowed
	// rule fires for this agent, (c) the parsed command is itself
	// read-only, and (d) it does NOT touch a sensitive path under the
	// sensitive-file guard.
	readOnlyShellCommand := false
	sensitiveShellPath := false
	if toolnames.IsShellToolName(tu.Name) && readOnlyShellCommandsAllowed(tu.Name, cfg.AgentID, cfg.ToolRules) {
		if cmd := shellCommandFromInput(tu.Input); cmd != "" {
			readOnlyShellCommand, _ = inspector.IsReadOnlyBashCommand(cmd)
			if toolnames.SensitiveFileGuardEnabled(tu.Name, cfg.AgentID, cfg.ToolRules) {
				if tok, reason, hit := inspector.CommandReferencesSensitivePath(cmd); hit {
					sensitiveShellPath = true
					readOnlyShellCommand = false
					audit("block", "sensitive_path_in_read_only_shell", "command references sensitive path "+tok+" ("+reason+")", "")
				}
			}
		}
	}

	if cfg.CandidateTasks == nil && cfg.ToolRules == nil && cfg.EgressRules == nil && !sensitiveShellPath {
		// No decision-engine inputs wired and no sensitive-path
		// override — record the call as ordinary pass-through.
		audit("allow", "pass_through", "no credential trigger", "")
		return conversation.ToolUseVerdict{Allowed: true}
	}

	decisionInput := runtimedecision.AuthorizationInput{
		ToolUse:                tu,
		UserID:                 cfg.AgentUserID,
		AgentID:                cfg.AgentID,
		Posture:                cfg.Posture,
		CandidateTasks:         cfg.CandidateTasks,
		ToolRules:              cfg.ToolRules,
		EgressRules:            cfg.EgressRules,
		PreferredTaskID:        cfg.PreferredTaskID,
		IntentVerifier:         decisionIntentVerifier{inner: cfg.IntentVerifier},
		SkipIntentVerification: readOnlyShellCommand,
	}
	dec, err := runtimedecision.EvaluateAuthorization(ctx, decisionInput)
	if err != nil {
		audit("block", "decision_error", err.Error(), "")
		return conversation.ToolUseVerdict{Allowed: false, Reason: "Clawvisor: authorization failed — " + err.Error()}
	}
	matchedTaskID := taskIDFromDecision(dec)

	switch dec.Kind {
	case runtimedecision.VerdictAllow:
		// Sliding-lifetime task expiry bump on each authorized call.
		if dec.Task != nil {
			_, _, _ = slideTaskExpiry(ctx, cfg.Store, dec.Task, time.Now().UTC())
		}
		audit("allow", string(dec.Source), dec.Reason, matchedTaskID)
		return conversation.ToolUseVerdict{Allowed: true}
	case runtimedecision.VerdictDeny:
		audit("block", string(dec.Source), dec.Reason, matchedTaskID)
		return conversation.ToolUseVerdict{Allowed: false, Reason: "Clawvisor: " + dec.Reason}
	case runtimedecision.VerdictNeedsApproval:
		// Background-shell poll (Codex's write_stdin with empty
		// chars) is a passthrough — no state change, no side effect.
		if dec.Source == runtimedecision.SourceTaskScopeMissing && isShellPollTool(tu.Name, tu.Input) {
			audit("allow", "shell_poll_pass_through", "background-shell poll ("+tu.Name+")", "")
			return conversation.ToolUseVerdict{Allowed: true}
		}
		if dec.Source == runtimedecision.SourceTaskScopeMissing && readOnlyShellCommand {
			audit("allow", "readonly_shell_pass_through", "read-only shell command", "")
			return conversation.ToolUseVerdict{Allowed: true}
		}
		// Hold first so the approval ID can be embedded in the
		// substitute message footer; the agent's next turn carries
		// that marker so a bare "y"/"n" resolves to this hold.
		var approvalID string
		if cfg.PendingApprovals != nil {
			held, err := cfg.PendingApprovals.Hold(ctx, PendingLiteApproval{
				UserID:         cfg.AgentUserID,
				AgentID:        cfg.AgentID,
				Provider:       provider,
				ConversationID: cfg.ConversationID,
				ToolUse:        tu,
				Inspector:      v,
				Fingerprint:    runtimedecision.Fingerprint(dec, decisionInput),
				Reason:         dec.Reason,
			})
			if err != nil {
				audit("block", "approval_hold_error", err.Error(), "")
				return conversation.ToolUseVerdict{
					Allowed: false,
					Reason:  "Clawvisor: approval unavailable — " + err.Error(),
				}
			}
			if held.Evicted != nil {
				audit("block", "approval_evicted", "superseded pending approval "+held.Evicted.ID, "")
				CleanupEvictedInlineTask(ctx, cfg, held.Evicted)
			}
			approvalID = held.Pending.ID
		}
		audit("block", string(dec.Source), dec.Reason, matchedTaskID)
		return conversation.ToolUseVerdict{
			Allowed:        false,
			Reason:         "Clawvisor: approval required — " + dec.Reason,
			SubstituteWith: ApprovalPrompt(tu, dec.Reason, approvalID),
			HeldKindHint:   "approval",
		}
	}
	// Unknown decision kind — fail closed.
	audit("block", "decision_unknown", "unknown decision kind", "")
	return conversation.ToolUseVerdict{Allowed: false, Reason: "Clawvisor: unknown decision kind"}
}
