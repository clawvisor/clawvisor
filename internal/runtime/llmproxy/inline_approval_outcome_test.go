package llmproxy

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestMemoryInlineApprovalOutcomeStore_RecordAndLookup(t *testing.T) {
	store := NewMemoryInlineApprovalOutcomeStore(time.Minute)
	store.Record("cv-1", InlineApprovalOutcome{Succeeded: true, TaskID: "task-1"})
	out, ok := store.Lookup("cv-1")
	if !ok || !out.Succeeded || out.TaskID != "task-1" {
		t.Fatalf("lookup = (%+v, %v)", out, ok)
	}
	if _, ok := store.Lookup(""); ok {
		t.Fatal("empty ID should miss")
	}
	if _, ok := store.Lookup("cv-missing"); ok {
		t.Fatal("missing ID should miss")
	}
}

func TestMemoryInlineApprovalOutcomeStore_TTL(t *testing.T) {
	store := NewMemoryInlineApprovalOutcomeStore(time.Millisecond)
	store.Record("cv-1", InlineApprovalOutcome{Succeeded: true})
	time.Sleep(5 * time.Millisecond)
	if _, ok := store.Lookup("cv-1"); ok {
		t.Fatal("expired entry must not be returned")
	}
}

// The prompt footer must round-trip through extractApprovalIDFromPrompt
// — that's the contract the augmenter relies on to correlate a prompt
// in conversation history with the outcome recorded by the rewrite.
func TestRenderTaskApprovalPrompt_EmbedsApprovalIDFooter(t *testing.T) {
	prompt := renderTaskApprovalPrompt(&runtimetasks.TaskCreateRequest{Purpose: "x"}, "cv-approve-123")
	if !strings.Contains(prompt, InlineApprovalIDMarker+"cv-approve-123]") {
		t.Fatalf("footer missing: %q", prompt)
	}
	if got := extractApprovalIDFromPrompt(prompt); got != "cv-approve-123" {
		t.Fatalf("extract = %q, want cv-approve-123", got)
	}
	// Empty approval ID renders no footer (back-compat with call sites
	// that haven't been updated).
	bare := renderTaskApprovalPrompt(&runtimetasks.TaskCreateRequest{Purpose: "x"}, "")
	if strings.Contains(bare, InlineApprovalIDMarker) {
		t.Fatalf("empty approval ID must not emit footer: %q", bare)
	}
}

// On a successful approve, the rewrite must record outcome=succeeded so
// the augmenter on the next turn can inject the success context. On
// failure (e.g., creator returns an error), it must record
// outcome=failed with the reason so the augmenter doesn't claim
// success.
func TestRewriteInlineTaskApprovalReply_RecordsSuccessOutcome(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)
	outcomes := NewMemoryInlineApprovalOutcomeStore(time.Minute)

	held, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-inline-success",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageAwaitingTaskApproval,
		TaskDefinition: &runtimetasks.TaskCreateRequest{
			Purpose: "Test task",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := RewriteInlineTaskApprovalReply(ctx, InlineApprovalRewriteRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            []byte(`{"messages":[{"role":"user","content":"approve"}]}`),
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
		Creator:         stubInlineTaskCreator{taskID: "task-created"},
		Outcomes:        outcomes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Decision != "allow" {
		t.Fatalf("decision = %q; want allow", out.Decision)
	}
	recorded, ok := outcomes.Lookup(held.Pending.ID)
	if !ok {
		t.Fatal("expected outcome to be recorded under the inner approval ID")
	}
	if !recorded.Succeeded || recorded.TaskID != "task-created" {
		t.Fatalf("recorded outcome = %+v", recorded)
	}
}

func TestRewriteInlineTaskApprovalReply_RecordsFailureOutcome(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)
	outcomes := NewMemoryInlineApprovalOutcomeStore(time.Minute)

	held, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-inline-fail",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageAwaitingTaskApproval,
		TaskDefinition: &runtimetasks.TaskCreateRequest{
			Purpose: "Test task",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := RewriteInlineTaskApprovalReply(ctx, InlineApprovalRewriteRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            []byte(`{"messages":[{"role":"user","content":"approve"}]}`),
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
		Creator:         stubInlineTaskCreator{err: "boom"},
		Outcomes:        outcomes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Decision != "deny" {
		t.Fatalf("decision = %q; want deny on failure", out.Decision)
	}
	recorded, ok := outcomes.Lookup(held.Pending.ID)
	if !ok {
		t.Fatal("expected outcome to be recorded even on failure")
	}
	if recorded.Succeeded {
		t.Fatalf("recorded outcome claims success on failure: %+v", recorded)
	}
	if !strings.Contains(recorded.FailureReason, "boom") {
		t.Fatalf("FailureReason should preserve the creator's error: %q", recorded.FailureReason)
	}
}

// stubInlineTaskCreator implements InlineTaskCreator for outcome tests.
type stubInlineTaskCreator struct {
	taskID string
	err    string
}

func (s stubInlineTaskCreator) CreateInlineApprovedTask(_ context.Context, _ *store.Agent, _ *runtimetasks.TaskCreateRequest, _ string) (*InlineApprovedTask, error) {
	if s.err != "" {
		return nil, &inlineCreatorTestError{msg: s.err}
	}
	return &InlineApprovedTask{ID: s.taskID, ApprovalRecordID: "ar-" + s.taskID}, nil
}

type inlineCreatorTestError struct{ msg string }

func (e *inlineCreatorTestError) Error() string { return e.msg }

// When the body shape can't be rewritten, the cache state must be
// untouched — earlier code Resolved the inner hold and Dropped the
// outer BEFORE the probe ran, stranding the user with no recoverable
// entries on a failed shape. The probe should now precede ALL cache
// mutations.
func TestRewriteInlineTaskApprovalReply_RewriteUnsupportedLeavesCacheIntact(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)

	outerHeld, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-outerxxxxxxxxxxxxxxxxxxxxx",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		Stage:    StageTool,
		ToolUse:  conversation.ToolUse{ID: "toolu_outer", Name: "Bash"},
	})
	if err != nil {
		t.Fatal(err)
	}
	innerHeld, err := cache.Hold(ctx, PendingLiteApproval{
		ID:              "cv-innerxxxxxxxxxxxxxxxxxxxxx",
		UserID:          "user-1",
		AgentID:         "agent-1",
		Provider:        conversation.ProviderAnthropic,
		Stage:           StageAwaitingTaskApproval,
		AwaitingTaskFor: outerHeld.Pending.ID,
		TaskDefinition:  &runtimetasks.TaskCreateRequest{Purpose: "x"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Body has the right verb but a shape the rewriter can't operate
	// on — provider switch falls through to (body, false, nil).
	out, err := RewriteInlineTaskApprovalReply(ctx, InlineApprovalRewriteRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.Provider("not-a-real-provider"),
		Body:            []byte(`{"messages":[{"role":"user","content":"approve"}]}`),
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
		Creator:         stubInlineTaskCreator{taskID: "task-should-not-be-created"},
	})
	// Body lacks a verb-bearing shape this provider can locate;
	// expect a clean no-op return (not Rewritten, no decision set)
	// rather than a body_rewrite_unsupported outcome.
	if err != nil {
		t.Fatal(err)
	}
	if out.Rewritten {
		t.Fatalf("rewrite should not fire on unsupported provider; out=%+v", out)
	}

	// Both holds must still be in the cache.
	if p, _ := cache.Peek(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1",
		Provider: conversation.ProviderAnthropic, ApprovalID: innerHeld.Pending.ID,
	}); p == nil {
		t.Fatal("inner inline hold was consumed before rewrite was confirmed")
	}
	if p, _ := cache.Peek(ctx, ResolveRequest{
		UserID: "user-1", AgentID: "agent-1",
		Provider: conversation.ProviderAnthropic, ApprovalID: outerHeld.Pending.ID,
	}); p == nil {
		t.Fatal("outer tool hold was dropped before rewrite was confirmed")
	}
}
