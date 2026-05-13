package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// fakeInlineTaskCreator captures the calls into the inline task creator
// so tests can verify both the inputs (parsed body, original tool_use)
// AND control the outputs (success/failure, returned task body).
type fakeInlineTaskCreator struct {
	called       bool
	gotAgent     *store.Agent
	gotReq       *runtimetasks.TaskCreateRequest
	gotOrigID    string
	resp         *InlineApprovedTask
	err          error
}

func (f *fakeInlineTaskCreator) CreateInlineApprovedTask(_ context.Context, agent *store.Agent, req *runtimetasks.TaskCreateRequest, originalToolUseID string) (*InlineApprovedTask, error) {
	f.called = true
	f.gotAgent = agent
	f.gotReq = req
	f.gotOrigID = originalToolUseID
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// seedInlineTaskHolds primes the cache the way the postprocess intercept
// would have: an outer StageAwaitingTaskDefinition hold + an inner
// StageAwaitingTaskApproval hold linking back.
func seedInlineTaskHolds(t *testing.T, cache *MemoryPendingApprovalCache) (outerID, innerID string) {
	t.Helper()
	ctx := context.Background()
	outer, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-origtoolxxxxxxxxxxxxxxxxxx",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		ToolUse: conversation.ToolUse{
			ID:    "toolu_orig",
			Name:  "Bash",
			Input: json.RawMessage(`{"command":"mkdir -p /tmp/landing"}`),
		},
		Stage: StageAwaitingTaskDefinition,
	})
	if err != nil {
		t.Fatal(err)
	}
	inner, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-innerholdxxxxxxxxxxxxxxxxx",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		ToolUse: conversation.ToolUse{
			ID:   "toolu_post",
			Name: "Bash",
		},
		Stage:           StageAwaitingTaskApproval,
		AwaitingTaskFor: outer.Pending.ID,
		TaskDefinition: &runtimetasks.TaskCreateRequest{
			Purpose:                "Build a landing page",
			IntentVerificationMode: "strict",
			ExpiresInSeconds:       600,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return outer.Pending.ID, inner.Pending.ID
}

func TestReleaseInlineTaskApprovalCreatesTaskAndDropsOriginalHold(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	outerID, innerID := seedInlineTaskHolds(t, cache)

	creator := &fakeInlineTaskCreator{
		resp: &InlineApprovedTask{
			ID:               "task-uuid-123",
			Status:           "active",
			Purpose:          "Build a landing page",
			Lifetime:         "session",
			ApprovalSource:   "inline_chat",
			ApprovalRecordID: "appr-uuid-456",
			ExpiresAtRFC3339: time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
		},
	}

	body := []byte(`{"messages":[{"role":"user","content":"approve ` + innerID + `"}]}`)
	result := TryReleasePendingApproval(context.Background(), ReleaseRequest{
		HTTPRequest:       httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:          conversation.ProviderAnthropic,
		Body:              body,
		Agent:             &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval:   cache,
		InlineTaskCreator: creator,
	})
	if !result.Handled {
		t.Fatal("expected release to be handled")
	}
	if result.Decision != "allow" {
		t.Fatalf("decision=%q, want allow; outcome=%s reason=%s", result.Decision, result.Outcome, result.Reason)
	}
	if result.Outcome != "inline_task_approved" {
		t.Fatalf("outcome=%q, want inline_task_approved", result.Outcome)
	}
	if !creator.called {
		t.Fatal("expected InlineTaskCreator.CreateInlineApprovedTask to be called")
	}
	if creator.gotAgent == nil || creator.gotAgent.ID != "agent-1" {
		t.Fatalf("creator received agent=%+v", creator.gotAgent)
	}
	if creator.gotReq == nil || creator.gotReq.Purpose != "Build a landing page" {
		t.Fatalf("creator received req=%+v", creator.gotReq)
	}
	if creator.gotOrigID != outerID {
		t.Fatalf("creator received originalToolUseID=%q, want %q (outer hold id)", creator.gotOrigID, outerID)
	}

	// The original hold must be dropped.
	peeked, _ := cache.Peek(context.Background(), ResolveRequest{
		UserID: "user-1", AgentID: "agent-1", Provider: conversation.ProviderAnthropic, ApprovalID: outerID,
	})
	if peeked != nil {
		t.Fatalf("expected original hold dropped; got %+v", peeked)
	}

	// Body should be the synthetic ALLOW response with the task payload.
	if !strings.Contains(string(result.Body), "task-uuid-123") {
		t.Fatalf("synthetic response missing task id; body=%s", result.Body)
	}
}

func TestReleaseInlineTaskApprovalDenyDropsBothHolds(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	outerID, innerID := seedInlineTaskHolds(t, cache)

	creator := &fakeInlineTaskCreator{}
	body := []byte(`{"messages":[{"role":"user","content":"deny ` + innerID + `"}]}`)
	result := TryReleasePendingApproval(context.Background(), ReleaseRequest{
		HTTPRequest:       httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:          conversation.ProviderAnthropic,
		Body:              body,
		Agent:             &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval:   cache,
		InlineTaskCreator: creator,
	})
	if result.Decision != "deny" {
		t.Fatalf("decision=%q, want deny", result.Decision)
	}
	if creator.called {
		t.Fatal("denied inline task must NOT call the creator")
	}
	for _, id := range []string{outerID, innerID} {
		peeked, _ := cache.Peek(context.Background(), ResolveRequest{
			UserID: "user-1", AgentID: "agent-1", Provider: conversation.ProviderAnthropic, ApprovalID: id,
		})
		if peeked != nil {
			t.Fatalf("hold %s should be dropped on deny; got %+v", id, peeked)
		}
	}
}

func TestReleaseInlineTaskApprovalCreatorFailureSurfacesAsDeny(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	_, innerID := seedInlineTaskHolds(t, cache)

	creator := &fakeInlineTaskCreator{err: errors.New("invalid task envelope")}
	body := []byte(`{"messages":[{"role":"user","content":"approve ` + innerID + `"}]}`)
	result := TryReleasePendingApproval(context.Background(), ReleaseRequest{
		HTTPRequest:       httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:          conversation.ProviderAnthropic,
		Body:              body,
		Agent:             &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval:   cache,
		InlineTaskCreator: creator,
	})
	if result.Decision != "deny" {
		t.Fatalf("decision=%q, want deny", result.Decision)
	}
	if result.Outcome != "inline_task_create_failed" {
		t.Fatalf("outcome=%q, want inline_task_create_failed", result.Outcome)
	}
	if !strings.Contains(result.Reason, "invalid task envelope") {
		t.Fatalf("reason missing creator error: %q", result.Reason)
	}
}

func TestReleaseInlineTaskApprovalMissingCreatorFailsClosed(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	_, innerID := seedInlineTaskHolds(t, cache)

	body := []byte(`{"messages":[{"role":"user","content":"approve ` + innerID + `"}]}`)
	result := TryReleasePendingApproval(context.Background(), ReleaseRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
	})
	if result.Decision != "deny" || result.Outcome != "inline_task_creator_missing" {
		t.Fatalf("expected deny+inline_task_creator_missing; got %+v", result)
	}
}
