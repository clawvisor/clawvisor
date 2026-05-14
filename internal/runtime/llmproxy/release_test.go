package llmproxy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestTryReleasePendingApprovalWrongExplicitIDDoesNotConsume(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)
	held, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-abcdefghijklmnopqrstuvwxyz",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
	})
	if err != nil {
		t.Fatal(err)
	}

	result := TryReleasePendingApproval(ctx, ReleaseRequest{
		Provider:        conversation.ProviderAnthropic,
		Body:            []byte(`{"messages":[{"role":"user","content":"approve cv-wrongwrong12"}]}`),
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
	})
	if !result.Handled || result.HTTPStatus != 404 {
		t.Fatalf("wrong explicit ID should be handled as not found: %+v", result)
	}

	resolved, err := cache.Resolve(ctx, ResolveRequest{
		UserID:     "user-1",
		AgentID:    "agent-1",
		Provider:   conversation.ProviderAnthropic,
		ApprovalID: held.Pending.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved == nil || resolved.ID != held.Pending.ID {
		t.Fatalf("approval was consumed by wrong ID; resolved=%+v", resolved)
	}
}

func TestTryReleasePendingApprovalParsesLongExplicitID(t *testing.T) {
	verb, id := conversation.ParseApprovalReplyText("please approve\napprove cv-abcdefghijklmnopqrstuvwxyz")
	if verb != "approve" || id != "cv-abcdefghijklmnopqrstuvwxyz" {
		t.Fatalf("long approval ID did not parse: verb=%q id=%q", verb, id)
	}
	verb, id = conversation.ParseApprovalReplyText(strings.ToUpper("deny cv-abcdef123456"))
	if verb != "deny" || id != "cv-abcdef123456" {
		t.Fatalf("short approval ID compatibility broke: verb=%q id=%q", verb, id)
	}
	verb, id = conversation.ParseApprovalReplyText("task")
	if verb != "task" || id != "" {
		t.Fatalf("bare task did not parse: verb=%q id=%q", verb, id)
	}
}

func TestRewriteTaskApprovalReplyRewritesAndDropsHold(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)
	held, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-abcdefghijklmnopqrstuvwxyz",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		ToolUse: conversation.ToolUse{
			ID:    "toolu_1",
			Name:  "Bash",
			Input: json.RawMessage(`{"command":"ls /tmp/ | grep -i greet"}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := RewriteTaskApprovalReply(ctx, TaskReplyRewriteRequest{
		Provider:        conversation.ProviderAnthropic,
		Body:            []byte(`{"messages":[{"role":"user","content":"task"}]}`),
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Rewritten {
		t.Fatalf("task reply result = %+v", result)
	}
	if !strings.Contains(string(result.Body), "https://clawvisor.local/control/tasks") ||
		!strings.Contains(string(result.Body), "ls /tmp/ | grep -i greet") {
		t.Fatalf("task guidance missing expected content: %s", result.Body)
	}

	// Hold must be dropped — there's no way back to approving the
	// original tool, so leaving it in the cache risks an orphan
	// being resolved later by a bare "approve" on something else.
	resolved, err := cache.Resolve(ctx, ResolveRequest{
		UserID:     "user-1",
		AgentID:    "agent-1",
		Provider:   conversation.ProviderAnthropic,
		ApprovalID: held.Pending.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved != nil {
		t.Fatalf("task reply must drop the hold; got resolved=%+v", resolved)
	}
}
