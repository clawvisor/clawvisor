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
	called    bool
	gotAgent  *store.Agent
	gotReq    *runtimetasks.TaskCreateRequest
	gotOrigID string
	resp      *InlineApprovedTask
	err       error
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

func TestRewriteInlineTaskApproval_ApproveCreatesTaskAndRewritesBody(t *testing.T) {
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
			Credentials: []InlineTaskCredentialPlaceholder{{
				VaultItemID:      "agentphone",
				ServiceID:        "agentphone",
				Placeholder:      "autovault_agentphone_real123",
				ExpiresAtRFC3339: "2026-05-15T20:38:00Z",
			}},
		},
	}

	body := []byte(`{"messages":[{"role":"user","content":"approve ` + innerID + `"}]}`)
	out, err := RewriteInlineTaskApprovalReply(context.Background(), InlineApprovalRewriteRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
		Creator:         creator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Rewritten {
		t.Fatal("expected body to be rewritten")
	}
	if out.Decision != "allow" || out.Outcome != "inline_task_approved" {
		t.Fatalf("decision=%q outcome=%q", out.Decision, out.Outcome)
	}
	if out.TaskID != "task-uuid-123" {
		t.Errorf("TaskID=%q, want task-uuid-123", out.TaskID)
	}
	if len(out.Credentials) != 1 || out.Credentials[0].Placeholder != "autovault_agentphone_real123" {
		t.Fatalf("expected minted credential placeholder in rewrite result, got %+v", out.Credentials)
	}
	if !creator.called {
		t.Fatal("expected creator to be called")
	}
	if creator.gotOrigID != outerID {
		t.Errorf("creator gotOrigID=%q, want %q", creator.gotOrigID, outerID)
	}
	// Body carries the canonical augmentation context. The per-task
	// task_id is intentionally NOT in the rewrite — see the no-drift
	// invariant in TestAugment_OneShotAndPersistentProduceIdenticalText.
	rewrittenBody := string(out.Body)
	if !strings.Contains(rewrittenBody, "task was created and approved by the user inline") {
		t.Errorf("rewritten body missing canonical augmentation context: %s", rewrittenBody)
	}
	if !strings.Contains(strings.ToLower(rewrittenBody), "do not post /control/tasks") {
		t.Errorf("rewritten body missing do-not-repost guidance: %s", rewrittenBody)
	}
	if !strings.Contains(rewrittenBody, "agentphone=autovault_agentphone_real123") {
		t.Errorf("rewritten body missing exact credential placeholder: %s", rewrittenBody)
	}
	// Both holds gone.
	for _, id := range []string{outerID, innerID} {
		peeked, _ := cache.Peek(context.Background(), ResolveRequest{
			UserID: "user-1", AgentID: "agent-1", Provider: conversation.ProviderAnthropic, ApprovalID: id,
		})
		if peeked != nil {
			t.Errorf("hold %s should be dropped on approve; got %+v", id, peeked)
		}
	}
}

func TestRewriteInlineTaskApproval_DenyDropsBothHoldsNoCreator(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	outerID, innerID := seedInlineTaskHolds(t, cache)

	creator := &fakeInlineTaskCreator{}
	body := []byte(`{"messages":[{"role":"user","content":"deny ` + innerID + `"}]}`)
	out, err := RewriteInlineTaskApprovalReply(context.Background(), InlineApprovalRewriteRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
		Creator:         creator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Rewritten || out.Decision != "deny" {
		t.Fatalf("decision=%q rewritten=%v", out.Decision, out.Rewritten)
	}
	if creator.called {
		t.Fatal("creator must not be called on deny")
	}
	rewrittenBody := string(out.Body)
	if !strings.Contains(rewrittenBody, "user denied") &&
		!strings.Contains(rewrittenBody, "denied the task-creation") {
		t.Errorf("rewritten body missing denial language: %s", rewrittenBody)
	}
	for _, id := range []string{outerID, innerID} {
		peeked, _ := cache.Peek(context.Background(), ResolveRequest{
			UserID: "user-1", AgentID: "agent-1", Provider: conversation.ProviderAnthropic, ApprovalID: id,
		})
		if peeked != nil {
			t.Errorf("hold %s should be dropped on deny; got %+v", id, peeked)
		}
	}
}

func TestRewriteInlineTaskApproval_CreatorFailureRewritesAsDeny(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	_, innerID := seedInlineTaskHolds(t, cache)

	creator := &fakeInlineTaskCreator{err: errors.New("invalid task envelope")}
	body := []byte(`{"messages":[{"role":"user","content":"approve ` + innerID + `"}]}`)
	out, err := RewriteInlineTaskApprovalReply(context.Background(), InlineApprovalRewriteRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
		Creator:         creator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Rewritten || out.Decision != "deny" || out.Outcome != "inline_task_create_failed" {
		t.Fatalf("decision=%q outcome=%q rewritten=%v", out.Decision, out.Outcome, out.Rewritten)
	}
	if !strings.Contains(string(out.Body), "invalid task envelope") {
		t.Errorf("rewritten body missing creator error message: %s", out.Body)
	}
}

func TestRewriteInlineTaskApproval_MissingCreatorRewritesAsDeny(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	_, innerID := seedInlineTaskHolds(t, cache)

	body := []byte(`{"messages":[{"role":"user","content":"approve ` + innerID + `"}]}`)
	out, err := RewriteInlineTaskApprovalReply(context.Background(), InlineApprovalRewriteRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
		// Creator nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Decision != "deny" || out.Outcome != "inline_task_creator_missing" {
		t.Fatalf("decision=%q outcome=%q", out.Decision, out.Outcome)
	}
}

// Regression for 2026-05-14T04:03:23 production failure: an
// unresolved tool-stage hold sat ahead of the inline-task hold in
// the cache. User typed bare "approve". The cache's no-ID peek
// returned the OLDER tool-stage hold (FIFO), our rewrite saw it
// wasn't StageAwaitingTaskApproval and bailed. TryReleasePendingApproval
// then resolved the older tool hold as a regular approve, and the
// inline-task hold sat unresolved — no task created, no audit row,
// next agent turn hit task_scope_missing.
//
// With the Stage filter, the inline-task rewriter targets its
// hold specifically regardless of cache ordering.
func TestRewriteInlineTaskApproval_FindsInlineHoldBehindStaleToolHold(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	ctx := context.Background()

	// Older tool-stage hold (e.g. from a prior intent_refusal the
	// user never replied to). This goes into items[0].
	if _, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-staletoolholdxxxxxxxxxxxxx",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageTool,
		ToolUse:  conversation.ToolUse{ID: "toolu_stale", Name: "Read"},
	}); err != nil {
		t.Fatal(err)
	}
	// Inline-task hold added AFTER → goes into items[1].
	inner, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-innerholdxxxxxxxxxxxxxxxxx",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageAwaitingTaskApproval,
		ToolUse:  conversation.ToolUse{ID: "toolu_post", Name: "Bash"},
		TaskDefinition: &runtimetasks.TaskCreateRequest{
			Purpose: "Test inline task",
			ExpectedTools: []runtimetasks.ExpectedTool{
				{ToolName: "Bash", Why: "x"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	creator := &fakeInlineTaskCreator{
		resp: &InlineApprovedTask{
			ID:             "task-stale-test",
			Status:         "active",
			ApprovalSource: "inline_chat",
			Lifetime:       "session",
		},
	}
	// Bare "approve" — no cv-id. This is the production failure mode.
	body := []byte(`{"messages":[{"role":"user","content":"approve"}]}`)
	out, err := RewriteInlineTaskApprovalReply(ctx, InlineApprovalRewriteRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
		Creator:         creator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Rewritten {
		t.Fatal("inline-task rewrite must fire even when a stale tool hold sits ahead in the cache")
	}
	if out.Decision != "allow" || out.Outcome != "inline_task_approved" {
		t.Fatalf("decision=%q outcome=%q", out.Decision, out.Outcome)
	}
	if !creator.called {
		t.Fatal("creator was not called — inline-task hold was not resolved")
	}
	if out.TaskID != "task-stale-test" {
		t.Errorf("TaskID=%q, want task-stale-test", out.TaskID)
	}
	// Stale tool hold must still be there for the regular release
	// path to handle separately — we should not have collateral-
	// damaged it.
	stale, _ := cache.Peek(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1", Provider: conversation.ProviderAnthropic,
		ApprovalID: "cv-staletoolholdxxxxxxxxxxxxx",
	})
	if stale == nil {
		t.Error("stale tool hold should remain; the rewriter must not touch other holds")
	}
	// Inline-task hold must be consumed.
	gone, _ := cache.Peek(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1", Provider: conversation.ProviderAnthropic,
		ApprovalID: inner.Pending.ID,
	})
	if gone != nil {
		t.Errorf("inline-task hold should be consumed; got %+v", gone)
	}
}

// A bare approve belongs to the newest visible prompt, not merely the
// newest inline-task prompt. If an older inline-task hold is still
// pending but a newer regular tool approval prompt has been rendered,
// the inline rewriter must leave the bare reply alone so the regular
// release path can resolve the newest tool hold.
func TestRewriteInlineTaskApproval_BareApproveDoesNotStealNewerToolHold(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	ctx := context.Background()

	// Older inline-task hold that should NOT consume a later bare approve.
	inlineHeld, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-inlineolderxxxxxxxxxxxxxxx",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageAwaitingTaskApproval,
		TaskDefinition: &runtimetasks.TaskCreateRequest{
			Purpose: "Older inline task",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Newer regular tool hold: this is the prompt the user just saw.
	toolHeld, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-toolnewerxxxxxxxxxxxxxxxxx",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageTool,
		ToolUse:  conversation.ToolUse{ID: "toolu_newer", Name: "Bash"},
	})
	if err != nil {
		t.Fatal(err)
	}

	creator := &fakeInlineTaskCreator{
		resp: &InlineApprovedTask{ID: "task-should-not-be-created"},
	}
	body := []byte(`{"messages":[{"role":"user","content":"approve"}]}`)
	out, err := RewriteInlineTaskApprovalReply(ctx, InlineApprovalRewriteRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
		Creator:         creator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Rewritten || out.Decision != "" {
		t.Fatalf("bare approve should not be consumed by older inline hold; got %+v", out)
	}
	if creator.called {
		t.Fatal("older inline hold incorrectly created a task from a bare reply to a newer tool prompt")
	}
	for _, id := range []string{inlineHeld.Pending.ID, toolHeld.Pending.ID} {
		peeked, err := cache.Peek(ctx, ResolveRequest{
			UserID: "user-1", AgentID: "agent-1",
			Provider: conversation.ProviderAnthropic, ApprovalID: id,
		})
		if err != nil {
			t.Fatal(err)
		}
		if peeked == nil {
			t.Fatalf("hold %s should remain for the correct release path", id)
		}
	}
}

func TestRewriteInlineTaskApproval_NoToolHoldIsNoop(t *testing.T) {
	// User typed "approve" but there's no inner hold (e.g. it's a
	// regular tool-stage approval, not an inline-task one). The
	// rewrite should be a no-op so TryReleasePendingApproval can
	// handle it.
	cache := NewMemoryPendingApprovalCache(time.Minute)
	if _, err := cache.Hold(context.Background(), PendingLiteApproval{
		ID:       "cv-toolstageholdxxxxxxxxxxxxx",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageTool, // not the awaiting_task_approval stage
		ToolUse:  conversation.ToolUse{ID: "toolu_x", Name: "Bash"},
	}); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"messages":[{"role":"user","content":"approve cv-toolstageholdxxxxxxxxxxxxx"}]}`)
	out, err := RewriteInlineTaskApprovalReply(context.Background(), InlineApprovalRewriteRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Rewritten {
		t.Fatal("tool-stage approve should NOT trigger inline rewrite")
	}
	if !equalBytes(out.Body, body) {
		t.Errorf("body should be unchanged; got %s", out.Body)
	}
}

// TryReleasePendingApproval should fail-closed if it sees an
// awaiting_task_approval hold (defensive check; production wires the
// preprocess so the hold is consumed before release ever sees it).
func TestTryReleasePendingApproval_FailsClosedOnAwaitingTaskApproval(t *testing.T) {
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
	if result.Decision != "deny" || result.Outcome != "inline_task_preprocess_missing" {
		t.Fatalf("expected fail-closed: deny + inline_task_preprocess_missing; got %+v", result)
	}
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
