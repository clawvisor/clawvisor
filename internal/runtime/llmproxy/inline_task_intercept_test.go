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
const inlineTaskBody = `{"purpose":"Build a landing page at /tmp/landing","intent_verification_mode":"strict","expires_in_seconds":600,"expected_tools":[{"tool_name":"Bash","why":"Create directory"},{"tool_name":"Write","why":"Create HTML files"}]}`

func anthropicBashControlTasksPost(body string) []byte {
	cmd := `curl -sS -X POST 'https://clawvisor.local/control/tasks?wait=true&timeout=120' -H 'Content-Type: application/json' --data '` + body + `'`
	enc, err := json.Marshal(map[string]string{"command": cmd})
	if err != nil {
		panic(err)
	}
	return anthropicJSONWithNamedToolUse("Bash", string(enc))
}

// The legacy "state signal" path (a prior StageAwaitingTaskDefinition
// hold seeded by RewriteTaskApprovalReply) is no longer reachable in
// production — task replies now fully Resolve the original hold rather
// than transitioning its stage, so there is no awaiting-definition
// hold for the intercept to observe. Inline approvals flow only
// through the ?surface=inline query signal, exercised by
// TestPostprocess_InlineTaskInterceptedWithSurfaceInlineQueryParam.

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

// TestInlineTask_PostprocessIntoRelease drives both halves of the
// state machine through real exported entry points: Postprocess
// intercepts the model-emitted POST /control/tasks (via the
// ?surface=inline query signal — the only production-reachable signal
// today) and registers the inner hold; TryReleasePendingApproval
// consumes the user's "approve" reply, drives the InlineTaskCreator,
// and emits the synthetic response. Mirrors the production wiring
// with stubs for the creator.
func TestInlineTask_PostprocessIntoRelease(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	// Drive Postprocess on a model response that emits the bash-form
	// POST /control/tasks WITH the surface=inline query — that's how a
	// compliant model signals an inline approval gesture in production.
	body := anthropicBashControlTasksPostWithQuery(inlineTaskBody, "surface=inline")
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

	// User types yes.
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
	approveBody := []byte(`{"messages":[{"role":"user","content":"yes ` + innerID + `"}]}`)
	rewrite, err := RewriteInlineTaskApprovalReply(ctx, InlineApprovalRewriteRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            approveBody,
		Agent:           &store.Agent{ID: agentID, UserID: userID},
		PendingApproval: cache,
		Creator:         creator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rewrite.Rewritten || rewrite.Decision != "allow" || rewrite.Outcome != "inline_task_approved" {
		t.Fatalf("rewrite decision=%q outcome=%q rewritten=%v", rewrite.Decision, rewrite.Outcome, rewrite.Rewritten)
	}
	if !creator.called {
		t.Fatal("Creator should have been invoked")
	}
	if !strings.Contains(string(rewrite.Body), "task was created and approved by the user inline") {
		t.Fatalf("rewritten body missing canonical augmentation context: %s", rewrite.Body)
	}
	if rewrite.TaskID != "task-uuid-final" {
		t.Errorf("TaskID=%q, want task-uuid-final", rewrite.TaskID)
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

// anthropicBashControlTasksPostWithQuery is anthropicBashControlTasksPost
// with an extra URL query parameter (e.g. surface=inline).
func anthropicBashControlTasksPostWithQuery(body string, extraQuery string) []byte {
	cmd := `curl -sS -X POST 'https://clawvisor.local/control/tasks?wait=true&timeout=120&` + extraQuery + `' -H 'Content-Type: application/json' --data '` + body + `'`
	enc, err := json.Marshal(map[string]string{"command": cmd})
	if err != nil {
		panic(err)
	}
	return anthropicJSONWithNamedToolUse("Bash", string(enc))
}

func TestPostprocess_InlineTaskInterceptedWithSurfaceInlineQueryParam(t *testing.T) {
	// No prior `task` reply, no awaiting-definition hold. The agent
	// explicitly opts into inline approval via ?surface=inline.
	ctx := context.Background()
	cache := NewMemoryPendingApprovalCache(time.Minute)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	body := anthropicBashControlTasksPostWithQuery(inlineTaskBody, "surface=inline")
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

	out := string(got.Body)
	if !strings.Contains(out, "Clawvisor wants to create a task") {
		t.Fatalf("expected inline approval prompt when surface=inline; got %s", out)
	}
	if strings.Contains(out, "X-Clawvisor-Caller") {
		t.Fatalf("surface=inline should NOT rewrite the curl through to the daemon; got %s", out)
	}

	// One inner hold should now exist, with AwaitingTaskFor="" (no
	// outer hold to cascade to).
	cache.mu.Lock()
	holds := append([]PendingLiteApproval(nil), cache.pending[pendingApprovalKey{
		userID: userID, agentID: agentID, provider: conversation.ProviderAnthropic,
	}]...)
	cache.mu.Unlock()
	if len(holds) != 1 {
		t.Fatalf("expected 1 hold; got %d", len(holds))
	}
	if holds[0].Stage != StageAwaitingTaskApproval {
		t.Errorf("hold stage = %q, want awaiting_task_approval", holds[0].Stage)
	}
	if holds[0].AwaitingTaskFor != "" {
		t.Errorf("query-only intercept should leave AwaitingTaskFor empty; got %q", holds[0].AwaitingTaskFor)
	}
	_ = ctx
}

func TestPostprocess_InlineTaskPromptRendersCredentialsAndRisk(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	taskBody := `{"purpose":"Create GitHub release issues","intent_verification_mode":"strict","expires_in_seconds":600,"expected_tools":[{"tool_name":"Bash","why":"Call the GitHub API."}],"required_credentials":[{"vault_item_id":"github","why":"Create issues in owner/repo."}]}`
	body := anthropicBashControlTasksPostWithQuery(taskBody, "surface=inline")
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

	out := string(got.Body)
	if !strings.Contains(out, "Credentials requested") || !strings.Contains(out, "github") {
		t.Fatalf("expected inline prompt to render requested credentials; got %s", out)
	}
	if !strings.Contains(out, "Risk") || !strings.Contains(out, "medium") {
		t.Fatalf("expected inline prompt to render risk level; got %s", out)
	}
}

func TestPostprocess_InlineTaskInvalidCredentialFallsThroughToToolError(t *testing.T) {
	cache := NewMemoryPendingApprovalCache(time.Minute)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxx")

	taskBody := `{"purpose":"Call agentphone","intent_verification_mode":"strict","expires_in_seconds":600,"expected_tools":[{"tool_name":"Bash","why":"Call the agentphone API."}],"required_credentials":[{"vault_item_id":"agentphone"}]}`
	body := anthropicBashControlTasksPostWithQuery(taskBody, "surface=inline")
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

	out := string(got.Body)
	if strings.Contains(out, "Clawvisor wants to create a task") {
		t.Fatalf("invalid inline task should not render an approval prompt: %s", out)
	}
	if !strings.Contains(out, "http://localhost:25297/control/tasks") {
		t.Fatalf("invalid inline task should fall through to control rewrite so the tool receives the validation error: %s", out)
	}

	cache.mu.Lock()
	holds := len(cache.pending[pendingApprovalKey{
		userID: userID, agentID: agentID, provider: conversation.ProviderAnthropic,
	}])
	cache.mu.Unlock()
	if holds != 0 {
		t.Fatalf("invalid inline task should not create an approval hold, got %d", holds)
	}
}

func TestPostprocess_InlineTaskBareNoSignalRoutesToDashboard(t *testing.T) {
	// No prior `task` reply AND no surface=inline → fall through to
	// the regular control-rewrite path (dashboard task creation).
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
		t.Fatalf("expected dashboard control-rewrite when no inline signal; got %s", got.Body)
	}
	if !strings.Contains(string(got.Body), "http://localhost:25297/control/tasks") {
		t.Fatalf("expected control URL rewritten; got %s", got.Body)
	}
}

func TestReleaseInlineTaskApproval_QueryOnlyHoldNoOuterCascade(t *testing.T) {
	// A query-signal inner hold has AwaitingTaskFor="" — the release
	// path must NOT try to drop a non-existent outer hold.
	cache := NewMemoryPendingApprovalCache(time.Minute)
	ctx := context.Background()

	inner, err := cache.Hold(ctx, PendingLiteApproval{
		ID:       "cv-innerholdxxxxxxxxxxxxxxxxx",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		ToolUse:  conversation.ToolUse{ID: "toolu_post", Name: "Bash"},
		Stage:    StageAwaitingTaskApproval,
		// AwaitingTaskFor intentionally empty — query-only inline.
		TaskDefinition: &runtimetasks.TaskCreateRequest{
			Purpose: "Inline-only task",
			ExpectedTools: []runtimetasks.ExpectedTool{
				{ToolName: "Bash", Why: "x"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	creator := &capturingInlineCreator{
		resp: &InlineApprovedTask{ID: "task-q", Status: "active", ApprovalSource: "inline_chat", Lifetime: "session", ApprovalRecordID: "appr-q"},
	}
	approveBody := []byte(`{"messages":[{"role":"user","content":"yes ` + inner.Pending.ID + `"}]}`)
	rewrite, err := RewriteInlineTaskApprovalReply(ctx, InlineApprovalRewriteRequest{
		HTTPRequest:     httptest.NewRequest("POST", "/v1/messages", nil),
		Provider:        conversation.ProviderAnthropic,
		Body:            approveBody,
		Agent:           &store.Agent{ID: "agent-1", UserID: "user-1"},
		PendingApproval: cache,
		Creator:         creator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rewrite.Rewritten || rewrite.Decision != "allow" || rewrite.Outcome != "inline_task_approved" {
		t.Fatalf("rewrite decision=%q outcome=%q rewritten=%v reason=%q",
			rewrite.Decision, rewrite.Outcome, rewrite.Rewritten, rewrite.Reason)
	}
	if !creator.called {
		t.Fatal("creator should have been invoked")
	}
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
