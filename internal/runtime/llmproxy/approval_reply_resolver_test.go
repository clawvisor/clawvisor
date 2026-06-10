package llmproxy

import (
	"context"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

func TestResolveApprovalReplyActionCharacterizesRouting(t *testing.T) {
	tests := []struct {
		name       string
		holds      []PendingLiteApproval
		verb       string
		approvalID string
		wantKind   approvalReplyActionKind
		wantID     string
	}{
		{
			name: "bare_approve_targets_newest_tool_hold",
			holds: []PendingLiteApproval{
				resolverTestHold("cv-aaaaaaaaaaaaaaaaaaaaaaaaaa", StageAwaitingTaskApproval),
				resolverTestHold("cv-bbbbbbbbbbbbbbbbbbbbbbbbbb", StageTool),
			},
			verb:     "approve",
			wantKind: approvalReplyActionReleaseTool,
			wantID:   "cv-bbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
		{
			name: "bare_approve_targets_newest_inline_hold",
			holds: []PendingLiteApproval{
				resolverTestHold("cv-cccccccccccccccccccccccccc", StageTool),
				resolverTestHold("cv-dddddddddddddddddddddddddd", StageAwaitingTaskApproval),
			},
			verb:     "approve",
			wantKind: approvalReplyActionApproveInlineTask,
			wantID:   "cv-dddddddddddddddddddddddddd",
		},
		{
			name: "bare_deny_targets_newest_inline_hold",
			holds: []PendingLiteApproval{
				resolverTestHold("cv-eeeeeeeeeeeeeeeeeeeeeeeeee", StageTool),
				resolverTestHold("cv-ffffffffffffffffffffffffff", StageAwaitingTaskApproval),
			},
			verb:     "deny",
			wantKind: approvalReplyActionDenyInlineTask,
			wantID:   "cv-ffffffffffffffffffffffffff",
		},
		{
			name: "explicit_id_targets_older_tool_hold",
			holds: []PendingLiteApproval{
				resolverTestHold("cv-gggggggggggggggggggggggggg", StageTool),
				resolverTestHold("cv-hhhhhhhhhhhhhhhhhhhhhhhhhh", StageAwaitingTaskApproval),
			},
			verb:       "approve",
			approvalID: "cv-gggggggggggggggggggggggggg",
			wantKind:   approvalReplyActionReleaseTool,
			wantID:     "cv-gggggggggggggggggggggggggg",
		},
		{
			name: "explicit_id_targets_older_inline_hold",
			holds: []PendingLiteApproval{
				resolverTestHold("cv-iiiiiiiiiiiiiiiiiiiiiiiiii", StageAwaitingTaskApproval),
				resolverTestHold("cv-jjjjjjjjjjjjjjjjjjjjjjjjjj", StageTool),
			},
			verb:       "approve",
			approvalID: "cv-iiiiiiiiiiiiiiiiiiiiiiiiii",
			wantKind:   approvalReplyActionApproveInlineTask,
			wantID:     "cv-iiiiiiiiiiiiiiiiiiiiiiiiii",
		},
		{
			name: "task_targets_newest_hold",
			holds: []PendingLiteApproval{
				resolverTestHold("cv-kkkkkkkkkkkkkkkkkkkkkkkkkk", StageTool),
				resolverTestHold("cv-llllllllllllllllllllllllll", StageAwaitingTaskApproval),
			},
			verb:     "task",
			wantKind: approvalReplyActionStartInlineTaskDefinition,
			wantID:   "cv-llllllllllllllllllllllllll",
		},
		{
			name: "missing_explicit_id_is_noop",
			holds: []PendingLiteApproval{
				resolverTestHold("cv-mmmmmmmmmmmmmmmmmmmmmmmmmm", StageTool),
			},
			verb:       "approve",
			approvalID: "cv-nnnnnnnnnnnnnnnnnnnnnnnnnn",
			wantKind:   approvalReplyActionNoop,
		},
		{
			name: "unknown_verb_is_noop",
			holds: []PendingLiteApproval{
				resolverTestHold("cv-oooooooooooooooooooooooooo", StageTool),
			},
			verb:     "maybe",
			wantKind: approvalReplyActionNoop,
		},
		{
			name: "bare_approve_routes_expansion_stage_to_inline_expansion",
			holds: []PendingLiteApproval{
				resolverTestHold("cv-pppppppppppppppppppppppppp", StageTool),
				resolverTestHold("cv-qqqqqqqqqqqqqqqqqqqqqqqqqq", StageAwaitingExpansionApproval),
			},
			verb:     "approve",
			wantKind: approvalReplyActionApproveInlineExpansion,
			wantID:   "cv-qqqqqqqqqqqqqqqqqqqqqqqqqq",
		},
		{
			name: "bare_deny_routes_expansion_stage_to_inline_expansion",
			holds: []PendingLiteApproval{
				resolverTestHold("cv-rrrrrrrrrrrrrrrrrrrrrrrrrr", StageAwaitingExpansionApproval),
			},
			verb:     "deny",
			wantKind: approvalReplyActionDenyInlineExpansion,
			wantID:   "cv-rrrrrrrrrrrrrrrrrrrrrrrrrr",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cache := NewMemoryPendingApprovalCache(time.Minute)
			ctx := context.Background()
			for _, hold := range tc.holds {
				if _, err := cache.Hold(ctx, hold); err != nil {
					t.Fatal(err)
				}
			}
			got, err := resolveApprovalReplyAction(ctx, approvalReplyRoutingRequest{
				UserID:          "user-1",
				AgentID:         "agent-1",
				Provider:        conversation.ProviderAnthropic,
				PendingApproval: cache,
				Verb:            tc.verb,
				ApprovalID:      tc.approvalID,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got.Kind != tc.wantKind {
				t.Fatalf("Kind=%q, want %q; action=%+v", got.Kind, tc.wantKind, got)
			}
			if tc.wantID == "" {
				if got.Hold != nil {
					t.Fatalf("Hold=%+v, want nil", got.Hold)
				}
				return
			}
			if got.Hold == nil || got.Hold.ID != tc.wantID {
				t.Fatalf("Hold ID=%v, want %q", approvalActionHoldID(got), tc.wantID)
			}
		})
	}
}

func resolverTestHold(id string, stage PendingApprovalStage) PendingLiteApproval {
	return PendingLiteApproval{
		ID:       id,
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    stage,
	}
}

func approvalActionHoldID(action approvalReplyAction) string {
	if action.Hold == nil {
		return "<nil>"
	}
	return action.Hold.ID
}
