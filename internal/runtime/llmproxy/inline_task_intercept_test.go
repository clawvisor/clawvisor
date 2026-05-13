package llmproxy

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// inlineTaskBody is a typical task body the model would POST after the
// user types "task" on an inline approval prompt.
const inlineTaskBody = `{"purpose":"Build a landing page at /tmp/landing","intent_verification_mode":"strict","expires_in_seconds":600,"expected_tools_json":[{"tool_name":"Bash","why":"Create directory"},{"tool_name":"Write","why":"Create HTML files"}]}`

func anthropicBashControlTasksPost(body string) []byte {
	cmd := `curl -sS -X POST 'https://clawvisor.local/control/tasks?wait=true&timeout=120' -H 'Content-Type: application/json' --data '` + body + `'`
	enc, err := json.Marshal(map[string]string{"command": cmd})
	if err != nil {
		panic(err)
	}
	return anthropicJSONWithNamedToolUse("Bash", string(enc))
}

func TestPostprocess_InlineTaskInterceptedWhenAwaiting(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	// Pre-seed the original tool hold in StageAwaitingTaskDefinition,
	// the state RewriteTaskApprovalReply leaves it in.
	originalHold, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-origtooluuid00000000000000",
		UserID:   userID,
		AgentID:  agentID,
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

	body := anthropicBashControlTasksPost(inlineTaskBody)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:        insp,
		RewriteOpts:      inspector.DefaultRewriteOpts("http://localhost:25297"),
		CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
		Store:            st,
		AgentUserID:      userID,
		AgentID:          agentID,
		ControlBaseURL:   "http://localhost:25297",
		PendingApprovals: cache,
	})

	if got.SkippedReason != "" {
		t.Fatalf("postprocess skipped: %s", got.SkippedReason)
	}

	out := string(got.Body)
	// The substitute prompt — NOT the rewritten curl — should appear.
	if !strings.Contains(out, "Clawvisor wants to create a task") {
		t.Fatalf("expected inline approval prompt in body; got %s", out)
	}
	if !strings.Contains(out, "Build a landing page at /tmp/landing") {
		t.Fatalf("expected purpose in substituted prompt; got %s", out)
	}
	// Sanity: the rewritten /control/tasks URL must NOT leak through
	// (otherwise the bash would actually run and create the task).
	if strings.Contains(out, "X-Clawvisor-Caller") {
		t.Fatalf("expected the POST tool_use to be replaced, not rewritten through; got %s", out)
	}

	// Both the original (awaiting_task_definition) and the new
	// (awaiting_task_approval) holds should be in the cache. Find them
	// by scanning the cache's internal store.
	cache.mu.Lock()
	allHolds := append([]PendingLiteApproval(nil), cache.pending[pendingApprovalKey{
		userID: userID, agentID: agentID, provider: conversation.ProviderAnthropic,
	}]...)
	cache.mu.Unlock()
	if len(allHolds) != 2 {
		t.Fatalf("expected 2 holds in cache; got %d", len(allHolds))
	}
	var inner *PendingLiteApproval
	for i := range allHolds {
		if allHolds[i].Stage == StageAwaitingTaskApproval {
			inner = &allHolds[i]
		}
	}
	if inner == nil {
		t.Fatal("no awaiting_task_approval hold found")
	}
	if inner.AwaitingTaskFor != originalHold.Pending.ID {
		t.Fatalf("inner hold should link back via AwaitingTaskFor=%q; got %q", originalHold.Pending.ID, inner.AwaitingTaskFor)
	}
	if inner.TaskDefinition == nil || inner.TaskDefinition.Purpose == "" {
		t.Fatalf("inner hold should carry parsed TaskDefinition; got %+v", inner.TaskDefinition)
	}
	if inner.ToolUse.ID != "toolu_1" {
		t.Fatalf("inner hold ToolUse should be the POST tool_use; got %+v", inner.ToolUse)
	}
}

func TestPostprocess_AsyncControlTasksPostFallsThroughWhenNoHold(t *testing.T) {
	// No awaiting_task_definition hold → the model is doing async task
	// creation (or just calling /control/tasks directly), which should
	// hit the dashboard-backed rewrite path unchanged.
	cache := NewMemoryPendingApprovalCache(time.Minute)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	body := anthropicBashControlTasksPost(inlineTaskBody)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:        insp,
		RewriteOpts:      inspector.DefaultRewriteOpts("http://localhost:25297"),
		CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
		Store:            st,
		AgentUserID:      userID,
		AgentID:          agentID,
		ControlBaseURL:   "http://localhost:25297",
		PendingApprovals: cache,
	})

	if !got.Rewritten {
		t.Fatalf("expected control-tool rewrite to fire when no inline hold exists")
	}
	out := string(got.Body)
	if !strings.Contains(out, "http://localhost:25297/control/tasks") {
		t.Fatalf("expected control URL rewrite; got %s", out)
	}
}

func TestPostprocess_InlineTaskRefreshesOriginalHoldTTL(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	// Original hold near expiry (1 second left).
	nearExpiry := time.Now().Add(time.Second)
	if _, err := cache.Hold(ctx, PendingLiteApproval{
		ID:        "cv-stalentooluuid0000000000000",
		UserID:    userID,
		AgentID:   agentID,
		Provider:  conversation.ProviderAnthropic,
		ToolUse:   conversation.ToolUse{ID: "toolu_x", Name: "Bash"},
		Stage:     StageAwaitingTaskDefinition,
		ExpiresAt: nearExpiry,
	}); err != nil {
		t.Fatal(err)
	}

	body := anthropicBashControlTasksPost(inlineTaskBody)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})

	_ = Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:        insp,
		RewriteOpts:      inspector.DefaultRewriteOpts("http://localhost:25297"),
		CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
		Store:            st,
		AgentUserID:      userID,
		AgentID:          agentID,
		ControlBaseURL:   "http://localhost:25297",
		PendingApprovals: cache,
	})

	refreshed, err := cache.Peek(ctx, ResolveRequest{
		UserID:     userID,
		AgentID:    agentID,
		Provider:   conversation.ProviderAnthropic,
		ApprovalID: "cv-stalentooluuid0000000000000",
	})
	if err != nil {
		t.Fatal(err)
	}
	if refreshed == nil {
		t.Fatal("expected original hold to survive intercept")
	}
	if !refreshed.ExpiresAt.After(nearExpiry.Add(time.Minute)) {
		t.Fatalf("expected original hold TTL to be refreshed past near-expiry; got %v", refreshed.ExpiresAt)
	}
}

// TestInlineTask_PostprocessIntoRelease drives both halves of the
// state machine through real exported entry points: Postprocess
// intercepts the model-emitted POST /control/tasks and registers the
// inner hold; TryReleasePendingApproval consumes the user's "approve"
// reply, drives the InlineTaskCreator, and emits the synthetic
// response. Mirrors the production wiring with stubs for the creator.
func TestInlineTask_PostprocessIntoRelease(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	// Pre-seed the outer hold the way RewriteTaskApprovalReply would
	// have done after the user typed "task" on the original Bash prompt.
	if _, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-origtoolxxxxxxxxxxxxxxxxxx",
		UserID:   userID,
		AgentID:  agentID,
		Provider: conversation.ProviderAnthropic,
		ToolUse: conversation.ToolUse{
			ID:    "toolu_orig",
			Name:  "Bash",
			Input: json.RawMessage(`{"command":"mkdir -p /tmp/landing"}`),
		},
		Stage: StageAwaitingTaskDefinition,
	}); err != nil {
		t.Fatal(err)
	}

	// Drive Postprocess on a model response that emits the bash-form
	// POST /control/tasks.
	body := anthropicBashControlTasksPost(inlineTaskBody)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	postResult := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:        insp,
		RewriteOpts:      inspector.DefaultRewriteOpts("http://localhost:25297"),
		CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
		Store:            st,
		AgentUserID:      userID,
		AgentID:          agentID,
		ControlBaseURL:   "http://localhost:25297",
		PendingApprovals: cache,
	})
	if !strings.Contains(string(postResult.Body), "Clawvisor wants to create a task") {
		t.Fatalf("postprocess intercept did not substitute prompt: %s", postResult.Body)
	}

	// Find the inner hold the intercept just registered. We need its id
	// to send the user's "approve" reply at it.
	cache.mu.Lock()
	holds := append([]PendingLiteApproval(nil), cache.pending[pendingApprovalKey{
		userID: userID, agentID: agentID, provider: conversation.ProviderAnthropic,
	}]...)
	cache.mu.Unlock()
	var innerID string
	for _, h := range holds {
		if h.Stage == StageAwaitingTaskApproval {
			innerID = h.ID
			break
		}
	}
	if innerID == "" {
		t.Fatal("postprocess did not register an inner hold")
	}

	// User types approve.
	creator := &capturingInlineCreator{
		resp: &InlineApprovedTask{
			ID:               "task-uuid-final",
			Status:           "active",
			Purpose:          "Build a landing page",
			Lifetime:         "session",
			ApprovalSource:   "inline_chat",
			ApprovalRecordID: "appr-final",
		},
	}
	approveBody := []byte(`{"messages":[{"role":"user","content":"approve ` + innerID + `"}]}`)
	rel := TryReleasePendingApproval(ctx, ReleaseRequest{
		HTTPRequest:       httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:          conversation.ProviderAnthropic,
		Body:              approveBody,
		Agent:             &store.Agent{ID: agentID, UserID: userID},
		PendingApproval:   cache,
		InlineTaskCreator: creator,
	})
	if rel.Decision != "allow" || rel.Outcome != "inline_task_approved" {
		t.Fatalf("release decision=%q outcome=%q", rel.Decision, rel.Outcome)
	}
	if !creator.called {
		t.Fatal("InlineTaskCreator should have been invoked")
	}
	if !strings.Contains(string(rel.Body), "task-uuid-final") {
		t.Fatalf("synthetic response missing task id: %s", rel.Body)
	}

	// Both holds should be gone now.
	cache.mu.Lock()
	remaining := len(cache.pending[pendingApprovalKey{
		userID: userID, agentID: agentID, provider: conversation.ProviderAnthropic,
	}])
	cache.mu.Unlock()
	if remaining != 0 {
		t.Errorf("expected all holds dropped; %d remain", remaining)
	}
}

// capturingInlineCreator is a test InlineTaskCreator that records the
// inputs and returns a canned response.
type capturingInlineCreator struct {
	called bool
	resp   *InlineApprovedTask
}

func (c *capturingInlineCreator) CreateInlineApprovedTask(_ context.Context, _ *store.Agent, _ *runtimetasks.TaskCreateRequest, _ string) (*InlineApprovedTask, error) {
	c.called = true
	return c.resp, nil
}

func TestPostprocess_InlineTaskMalformedBodyFallsThrough(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	if _, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-origtooluuid00000000000001",
		UserID:   userID,
		AgentID:  agentID,
		Provider: conversation.ProviderAnthropic,
		ToolUse:  conversation.ToolUse{ID: "toolu_bad", Name: "Bash"},
		Stage:    StageAwaitingTaskDefinition,
	}); err != nil {
		t.Fatal(err)
	}

	// Body that's not valid JSON for TaskCreateRequest — actually empty.
	body := anthropicBashControlTasksPost(`{"purpose":""}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})

	got := Postprocess(req, body, "application/json", PostprocessConfig{
		Inspector:        insp,
		RewriteOpts:      inspector.DefaultRewriteOpts("http://localhost:25297"),
		CallerNonces:     NewMemoryCallerNonceCache(time.Minute),
		Store:            st,
		AgentUserID:      userID,
		AgentID:          agentID,
		ControlBaseURL:   "http://localhost:25297",
		PendingApprovals: cache,
	})

	if !got.Rewritten {
		t.Fatalf("expected fallback to regular control rewrite on missing purpose; got %s", got.Body)
	}
}
