package llmproxy

import (
	"context"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
)

// CredentialedAuthorizationResult is what
// EvaluateCredentialedAuthorization returns. When Allowed is true the
// caller proceeds with the credential rewrite (the inspector verdict
// has been validated against the agent's task scope + intent). When
// false, the embedded Verdict is the refusal conversation verdict
// (Hold, Deny, etc.).
type CredentialedAuthorizationResult struct {
	Allowed bool
	Verdict conversation.ToolUseVerdict
	// MatchedTaskID is the resolved task ID when allowed. Empty when
	// the call falls through any non-matching branch.
	MatchedTaskID string
}

// EvaluateCredentialedAuthorization runs the credentialed-path task
// scope + intent verification (postprocess.go ~1126–1250). Exposed so
// the handler-side pipeline factory's TaskScope resolver can call it
// directly.
//
// Decision tree:
//   - Decision-engine inputs wired → run EvaluateAuthorization on the
//     resolved (service, action). VerdictAllow proceeds; Deny / Hold
//     return a refusal conversation verdict.
//   - Decision-engine inputs NOT wired but Catalog + TaskScope set →
//     run TaskScope.Check + runIntentVerify. Mismatch returns refusal.
//   - Neither set → pass through to rewrite.
//
// emit receives one audit row at each terminal decision point — row
// shape matches the legacy newToolUseEvaluator's audit() closure.
func EvaluateCredentialedAuthorization(
	ctx context.Context,
	cfg PostprocessConfig,
	provider conversation.Provider,
	tu conversation.ToolUse,
	v inspector.Verdict,
	emit func(conversation.AuditEvent),
) CredentialedAuthorizationResult {
	audit := func(decision, outcome, reason, taskID string) {
		if emit == nil {
			return
		}
		emit(conversation.AuditEvent{
			ToolUse:          tu,
			InspectorVerdict: v,
			Decision:         conversation.DecisionKind(decision),
			OutcomeName:      outcome,
			Reason:           reason,
			TaskID:           taskID,
		})
	}

	if cfg.CandidateTasks != nil || cfg.ToolRules != nil || cfg.EgressRules != nil {
		resolved := ResolvedAction{}
		if cfg.Catalog != nil {
			resolved, _ = cfg.Catalog.Resolve(v.Host, v.Method, v.Path)
		}
		decisionInput := runtimedecision.AuthorizationInput{
			ToolUse:         tu,
			UserID:          cfg.AgentUserID,
			AgentID:         cfg.AgentID,
			Posture:         cfg.Posture,
			Target:          runtimedecision.TargetRequest{Host: v.Host, Method: v.Method, Path: v.Path},
			Service:         resolved.ServiceID,
			Action:          resolved.ActionID,
			CandidateTasks:  cfg.CandidateTasks,
			ToolRules:       cfg.ToolRules,
			EgressRules:     cfg.EgressRules,
			PreferredTaskID: cfg.PreferredTaskID,
			IntentVerifier:  decisionIntentVerifier{inner: cfg.IntentVerifier},
		}
		dec, err := runtimedecision.EvaluateAuthorization(ctx, decisionInput)
		if err != nil {
			audit("block", "decision_error", err.Error(), "")
			return CredentialedAuthorizationResult{
				Allowed: false,
				Verdict: conversation.ToolUseVerdict{
					Allowed: false,
					Reason:  "Clawvisor: authorization failed — " + err.Error(),
				},
			}
		}
		matchedTaskID := taskIDFromDecision(dec)
		switch dec.Kind {
		case runtimedecision.VerdictAllow:
			if dec.Task != nil {
				_, _, _ = SlideTaskExpiry(ctx, cfg.Store, dec.Task, time.Now().UTC())
			}
			return CredentialedAuthorizationResult{
				Allowed:       true,
				MatchedTaskID: matchedTaskID,
			}
		case runtimedecision.VerdictDeny:
			audit("block", string(dec.Source), dec.Reason, matchedTaskID)
			return CredentialedAuthorizationResult{
				Allowed: false,
				Verdict: conversation.ToolUseVerdict{
					Allowed: false,
					Reason:  "Clawvisor: " + dec.Reason,
				},
				MatchedTaskID: matchedTaskID,
			}
		case runtimedecision.VerdictNeedsApproval:
			var approvalID string
			if cfg.PendingApprovals != nil {
				held, herr := cfg.PendingApprovals.Hold(ctx, PendingLiteApproval{
					UserID:         cfg.AgentUserID,
					AgentID:        cfg.AgentID,
					Provider:       provider,
					ConversationID: cfg.ConversationID,
					ToolUse:        tu,
					Inspector:      v,
					Fingerprint:    runtimedecision.Fingerprint(dec, decisionInput),
					Reason:         dec.Reason,
				})
				if herr != nil {
					audit("block", "approval_hold_error", herr.Error(), "")
					return CredentialedAuthorizationResult{
						Allowed: false,
						Verdict: conversation.ToolUseVerdict{
							Allowed: false,
							Reason:  "Clawvisor: approval unavailable — " + herr.Error(),
						},
					}
				}
				if held.Evicted != nil {
					audit("block", "approval_evicted", "superseded pending approval "+held.Evicted.ID, "")
					CleanupEvictedInlineTask(ctx, cfg, held.Evicted)
				}
				approvalID = held.Pending.ID
			}
			audit("block", string(dec.Source), dec.Reason, matchedTaskID)
			return CredentialedAuthorizationResult{
				Allowed: false,
				Verdict: conversation.ToolUseVerdict{
					Allowed:        false,
					Reason:         "Clawvisor: approval required — " + dec.Reason,
					SubstituteWith: ApprovalPrompt(tu, dec.Reason, approvalID),
					HeldKindHint:   "approval",
				},
				MatchedTaskID: matchedTaskID,
			}
		}
	}

	// Legacy TaskScope.Check + intent verify fallback (when
	// decision-engine inputs aren't wired).
	if cfg.Catalog != nil && cfg.TaskScope != nil {
		if resolved, ok := cfg.Catalog.Resolve(v.Host, v.Method, v.Path); ok {
			dec := cfg.TaskScope.Check(ctx, cfg.AgentUserID, cfg.AgentID, resolved.ServiceID, resolved.ActionID)
			if !dec.Allowed {
				audit("block", "task_scope_denied", dec.Reason, "")
				return CredentialedAuthorizationResult{
					Allowed: false,
					Verdict: conversation.ToolUseVerdict{
						Allowed: false,
						Reason:  "Clawvisor: no active task scope covers " + resolved.ServiceID + "." + resolved.ActionID + " — " + dec.Reason,
					},
				}
			}
			if reason, ok := runIntentVerify(ctx, cfg, dec, resolved, tu); !ok {
				audit("block", "intent_verification_failed", reason, dec.TaskID)
				return CredentialedAuthorizationResult{
					Allowed: false,
					Verdict: conversation.ToolUseVerdict{
						Allowed: false,
						Reason:  "Clawvisor: intent verification refused " + resolved.ServiceID + "." + resolved.ActionID + " — " + reason,
					},
					MatchedTaskID: dec.TaskID,
				}
			}
			if dec.MatchedTask != nil {
				_, _, _ = SlideTaskExpiry(ctx, cfg.Store, dec.MatchedTask, time.Now().UTC())
			}
			return CredentialedAuthorizationResult{
				Allowed:       true,
				MatchedTaskID: dec.TaskID,
			}
		}
	}

	// Neither path wired — pass through to rewrite (preserves v0
	// fail-open behavior the legacy code documents).
	return CredentialedAuthorizationResult{Allowed: true}
}
