package llmproxy

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// flattenOpenAITaskReplyContent must scan all text-bearing blocks, not
// just the last one. A multi-block user message with the approve verb
// in any block — or split across blocks — was producing false negatives.
func TestFlattenOpenAITaskReplyContent_MultiBlock(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		mustHas string
	}{
		{
			name:    "approve_verb_in_first_block",
			raw:     `[{"type":"input_text","text":"approve cv-abc123"},{"type":"input_text","text":"trailing prose"}]`,
			mustHas: "approve cv-abc123",
		},
		{
			name:    "approve_split_across_blocks",
			raw:     `[{"type":"input_text","text":"please "},{"type":"input_text","text":"approve cv-xyz"}]`,
			mustHas: "approve cv-xyz",
		},
		{
			name:    "approve_in_middle",
			raw:     `[{"type":"input_text","text":"hi"},{"type":"input_text","text":"approve cv-mid"},{"type":"input_text","text":"thanks"}]`,
			mustHas: "approve cv-mid",
		},
		{
			name:    "simple_string",
			raw:     `"approve cv-simple"`,
			mustHas: "approve cv-simple",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := flattenOpenAITaskReplyContent(json.RawMessage(tc.raw))
			if !strings.Contains(got, tc.mustHas) {
				t.Fatalf("flattened content missing %q; got %q", tc.mustHas, got)
			}
		})
	}
}

// "task" reply drops the original tool hold so it can't later be
// resolved as a regular tool approval. There's no way for the user
// to go back to the original prompt — the harness has already
// shown the task-creation prompt — so leaving the hold in the
// cache was a latent privilege issue: a bare "approve" later could
// resolve the orphan and run the originally-blocked tool.
func TestRewriteTaskApprovalReplyDropsHold(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	ctx := context.Background()

	held, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-tooluuid000000000000000001",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		ToolUse:  conversation.ToolUse{ID: "toolu_1", Name: "Bash", Input: json.RawMessage(`{"command":"mkdir -p /tmp/x"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"task cv-tooluuid000000000000000001"}]}]}`)
	req := TaskReplyRewriteRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
	}
	out, err := RewriteTaskApprovalReply(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Rewritten {
		t.Fatalf("expected Rewritten=true; got %+v", out)
	}
	if !strings.Contains(string(out.Body), "control/tasks") {
		t.Fatalf("expected rewritten body to instruct task creation; got %s", out.Body)
	}
	// surface=inline must be in the example URL — the intercept
	// relies on the query signal now that the state signal hold is
	// gone.
	if !strings.Contains(string(out.Body), "surface=inline") {
		t.Fatalf("rewritten body must instruct surface=inline so intercept fires on the model's POST; got %s", out.Body)
	}

	peeked, err := cache.Peek(ctx, ResolveRequest{
		UserID:     "user-1",
		AgentID:    "agent-1",
		Provider:   conversation.ProviderAnthropic,
		ApprovalID: held.Pending.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if peeked != nil {
		t.Fatalf("expected hold to be DROPPED after task reply; got %+v", peeked)
	}
}

func TestRewriteTaskApprovalReplyNoopWhenNoHold(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"task cv-missing"}]}]}`)
	req := TaskReplyRewriteRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
	}
	out, err := RewriteTaskApprovalReply(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if out.Rewritten {
		t.Fatalf("expected no rewrite when no matching hold")
	}
}

// After typing "task" the user shouldn't be able to inadvertently
// release the original tool by typing a bare "approve" later. The
// drop-on-task contract prevents the original tool hold from
// sitting around as an orphan that bare-approve could resolve.
func TestRewriteTaskApprovalReplyOrphanCannotBeApprovedLater(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	ctx := context.Background()

	// Original blocked tool — say, a dangerous rm.
	_, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-dangerousrmxxxxxxxxxxxxxxxx",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageTool,
		ToolUse:  conversation.ToolUse{ID: "toolu_rm", Name: "Bash", Input: json.RawMessage(`{"command":"rm -rf /important"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}

	// User types "task" — wants to define a scoped task instead of
	// approving the rm directly.
	body := []byte(`{"messages":[{"role":"user","content":"task"}]}`)
	out, err := RewriteTaskApprovalReply(ctx, TaskReplyRewriteRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
	})
	if err != nil || !out.Rewritten {
		t.Fatalf("task reply must rewrite; out=%+v err=%v", out, err)
	}

	// Cache should now be empty for this scope.
	peeked, _ := cache.Peek(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1", Provider: conversation.ProviderAnthropic,
	})
	if peeked != nil {
		t.Fatalf("task reply must leave no hold the user can later approve; got %+v", peeked)
	}
	// A bare-approve from the user later must NOT find the dropped
	// rm hold and run the dangerous command.
	resolved, err := cache.Resolve(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1", Provider: conversation.ProviderAnthropic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved != nil {
		t.Fatalf("a later bare approve must NOT pick up the dropped tool hold; got %+v", resolved)
	}
}

func TestRewriteTaskApprovalReplyIgnoresNonTaskVerbs(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	if _, err := cache.Hold(context.Background(), PendingLiteApproval{
		ID:       "cv-tooluuid000000000000000002",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		ToolUse:  conversation.ToolUse{ID: "toolu_2", Name: "Bash"},
	}); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"approve cv-tooluuid000000000000000002"}]}]}`)
	req := TaskReplyRewriteRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
	}
	out, err := RewriteTaskApprovalReply(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if out.Rewritten {
		t.Fatalf("approve verb should not be rewritten as task")
	}
	// hold remains in original stage
	peeked, _ := cache.Peek(context.Background(), ResolveRequest{
		UserID: "user-1", AgentID: "agent-1", Provider: conversation.ProviderAnthropic, ApprovalID: "cv-tooluuid000000000000000002",
	})
	if peeked == nil || peeked.Stage != StageTool {
		t.Fatalf("approve verb must not transition stage; peeked=%+v", peeked)
	}
}
