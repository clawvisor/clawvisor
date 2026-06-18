package postproc

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	runtimetasks "github.com/clawvisor/clawvisor/internal/runtime/tasks"
	"github.com/clawvisor/clawvisor/internal/taskrisk"
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

// The legacy "state signal" path (a prior llmproxy.StageAwaitingTaskDefinition
// hold seeded by llmproxy.RewriteTaskApprovalReply) is no longer reachable in
// production — task replies now fully Resolve the original hold rather
// than transitioning its stage, so there is no awaiting-definition
// hold for the intercept to observe. Inline approvals flow only
// through the ?surface=inline query signal, exercised by
// TestPostprocess_InlineTaskInterceptedWithSurfaceInlineQueryParam.

func TestPostprocess_AsyncControlTasksPostFallsThroughWhenNoHold(t *testing.T) {
	// No awaiting_task_definition hold → the model is doing async task
	// creation (or just calling /api/control/tasks directly), which should
	// hit the dashboard-backed rewrite path unchanged.
	cache := llmproxy.NewMemoryPendingApprovalCache(time.Minute)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

	body := anthropicBashControlTasksPost(inlineTaskBody)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})

	got := Postprocess(req, body, "application/json", llmproxy.PostprocessConfig{
		ToolUseEvaluatorFactory: pipelineFactory,
		AgentContext: llmproxy.AgentContext{
			AgentUserID: userID,
			AgentID:     agentID,
		},
		ApprovalContext: llmproxy.ApprovalContext{
			PendingApprovals: cache,
		},
		RewriteContext: llmproxy.RewriteContext{
			Inspector:    insp,
			RewriteOpts:  inspector.DefaultRewriteOpts("http://localhost:25297"),
			CallerNonces: llmproxy.NewMemoryCallerNonceCache(time.Minute),
			Store:        st,
		},
		RoutingContext: llmproxy.RoutingContext{
			ControlBaseURL: "http://localhost:25297",
		},
	})

	if !got.Rewritten {
		t.Fatalf("expected control-tool rewrite to fire when no inline hold exists")
	}
	out := string(got.Body)
	if !strings.Contains(out, "http://localhost:25297/api/control/tasks") {
		t.Fatalf("expected control URL rewrite; got %s", out)
	}
}

// TestInlineTask_PostprocessIntoRelease drives both halves of the
// state machine through real exported entry points: Postprocess
// intercepts the model-emitted POST /api/control/tasks (via the
// ?surface=inline query signal — the only production-reachable signal
// today) and registers the inner hold; llmproxy.TryReleasePendingApproval
// consumes the user's "approve" reply, drives the llmproxy.InlineTaskCreator,
// and emits the synthetic response. Mirrors the production wiring
// with stubs for the creator.
func TestInlineTask_PostprocessIntoRelease(t *testing.T) {
	ctx := context.Background()
	cache := llmproxy.NewMemoryPendingApprovalCache(time.Minute)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

	// Drive Postprocess on a model response that emits the bash-form
	// POST /api/control/tasks WITH the surface=inline query — that's how a
	// compliant model signals an inline approval gesture in production.
	body := anthropicBashControlTasksPostWithQuery(inlineTaskBody, "surface=inline")
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	postResult := Postprocess(req, body, "application/json", llmproxy.PostprocessConfig{
		ToolUseEvaluatorFactory: pipelineFactory,
		AgentContext: llmproxy.AgentContext{
			AgentUserID: userID,
			AgentID:     agentID,
		},
		ApprovalContext: llmproxy.ApprovalContext{
			PendingApprovals: cache,
		},
		RewriteContext: llmproxy.RewriteContext{
			Inspector:    insp,
			RewriteOpts:  inspector.DefaultRewriteOpts("http://localhost:25297"),
			CallerNonces: llmproxy.NewMemoryCallerNonceCache(time.Minute),
			Store:        st,
		},
		RoutingContext: llmproxy.RoutingContext{
			ControlBaseURL: "http://localhost:25297",
		},
	})
	if !strings.Contains(string(postResult.Body), "Clawvisor wants to create a task") {
		t.Fatalf("postprocess intercept did not substitute prompt: %s", postResult.Body)
	}

	// Find the inner hold the intercept just registered. We need its id
	// to send the user's "approve" reply at it.
	holds := cache.SnapshotHoldsForTest(userID, agentID, conversation.ProviderAnthropic)
	var innerID string
	for _, h := range holds {
		if h.Stage == llmproxy.StageAwaitingTaskApproval {
			innerID = h.ID
			break
		}
	}
	if innerID == "" {
		t.Fatal("postprocess did not register an inner hold")
	}

	// User types yes.
	creator := &capturingInlineCreator{
		resp: &llmproxy.InlineApprovedTask{
			ID:               "task-uuid-final",
			Status:           "active",
			Purpose:          "Build a landing page",
			Lifetime:         "session",
			ApprovalSource:   "inline_chat",
			ApprovalRecordID: "appr-final",
		},
	}
	approveBody := []byte(`{"messages":[{"role":"user","content":"yes ` + innerID + `"}]}`)
	rewrite, err := llmproxy.RewriteInlineTaskApprovalReply(ctx, llmproxy.InlineApprovalRewriteRequest{
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
	if !strings.Contains(strings.ToLower(string(rewrite.Body)), "task was created and approved by the user") {
		t.Fatalf("rewritten body missing canonical augmentation context: %s", rewrite.Body)
	}
	if rewrite.TaskID != "task-uuid-final" {
		t.Errorf("TaskID=%q, want task-uuid-final", rewrite.TaskID)
	}

	// Both holds should be gone now.
	remaining := len(cache.SnapshotHoldsForTest(userID, agentID, conversation.ProviderAnthropic))
	if remaining != 0 {
		t.Errorf("expected all holds dropped; %d remain", remaining)
	}
}

// capturingInlineCreator is a test llmproxy.InlineTaskCreator that records the
// inputs and returns a canned response (or an error when fail is set,
// so auto-approve gate fall-back paths can be exercised). Implements
// the llmproxy.InlineTaskPendingCreator extension so the human-prompt path's
// pre-Hold CreatePendingInlineTask call lands in `pendingTaskID`.
type capturingInlineCreator struct {
	called bool
	fail   bool
	resp   *llmproxy.InlineApprovedTask

	pendingCalled bool
	pendingFail   bool
	pendingTaskID string
	approveCalled bool
	denyCalled    bool
	expireCalled  bool
	expireFail    bool
	expiredIDs    []string
}

func (c *capturingInlineCreator) CreateInlineApprovedTask(_ context.Context, _ *store.Agent, _ *runtimetasks.TaskCreateRequest, _ string) (*llmproxy.InlineApprovedTask, error) {
	c.called = true
	if c.fail {
		return nil, fmtErrorf("simulated inline creator failure")
	}
	return c.resp, nil
}

func (c *capturingInlineCreator) CreatePendingInlineTask(_ context.Context, _ *store.Agent, _ *runtimetasks.TaskCreateRequest, _ string, _ *taskrisk.RiskAssessment) (string, error) {
	c.pendingCalled = true
	if c.pendingFail {
		return "", fmtErrorf("simulated pending creator failure")
	}
	if c.pendingTaskID == "" {
		c.pendingTaskID = "task-stub-pending"
	}
	return c.pendingTaskID, nil
}

func (c *capturingInlineCreator) ApproveInlineTask(_ context.Context, _, _ string) (*llmproxy.InlineApprovedTask, error) {
	c.approveCalled = true
	c.called = true
	if c.fail {
		return nil, fmtErrorf("simulated inline approve failure")
	}
	return c.resp, nil
}

func (c *capturingInlineCreator) ExpireInlineTask(_ context.Context, taskID, _ string) error {
	c.expireCalled = true
	c.expiredIDs = append(c.expiredIDs, taskID)
	if c.expireFail {
		return fmtErrorf("simulated inline expire failure")
	}
	return nil
}

func (c *capturingInlineCreator) DenyInlineTask(_ context.Context, _, _ string) error {
	c.denyCalled = true
	return nil
}

// fmtErrorf is a tiny local helper to avoid adding an extra import for
// this single test path.
func fmtErrorf(s string) error { return &simpleErr{s: s} }

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }

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
	cache := llmproxy.NewMemoryPendingApprovalCache(time.Minute)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

	body := anthropicBashControlTasksPostWithQuery(inlineTaskBody, "surface=inline")
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})

	got := Postprocess(req, body, "application/json", llmproxy.PostprocessConfig{
		ToolUseEvaluatorFactory: pipelineFactory,
		AgentContext: llmproxy.AgentContext{
			AgentUserID: userID,
			AgentID:     agentID,
		},
		AuditContext: llmproxy.AuditContext{
			Audit:     llmproxy.NewAuditEmitter(st, nil, nil),
			RequestID: "req-inline-task-pending",
		},
		ApprovalContext: llmproxy.ApprovalContext{
			PendingApprovals: cache,
		},
		RewriteContext: llmproxy.RewriteContext{
			Inspector:    insp,
			RewriteOpts:  inspector.DefaultRewriteOpts("http://localhost:25297"),
			CallerNonces: llmproxy.NewMemoryCallerNonceCache(time.Minute),
			Store:        st,
		},
		RoutingContext: llmproxy.RoutingContext{
			ControlBaseURL: "http://localhost:25297",
		},
	})

	out := string(got.Body)
	if !strings.Contains(out, "Clawvisor wants to create a task") {
		t.Fatalf("expected inline approval prompt when surface=inline; got %s", out)
	}
	if strings.Contains(out, "X-Clawvisor-Caller") {
		t.Fatalf("surface=inline should NOT rewrite the curl through to the daemon; got %s", out)
	}
	rows, _, err := st.ListAuditEntries(req.Context(), userID, store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(rows))
	}
	row := rows[0]
	if row.Action != "lite_proxy.tool_use.approve" || row.Decision != "approve" || row.Outcome != "pending" {
		t.Fatalf("expected pending approval audit row, got action=%q decision=%q outcome=%q", row.Action, row.Decision, row.Outcome)
	}
	if row.ToolUseID == nil || *row.ToolUseID != "toolu_1" {
		t.Fatalf("tool_use_id=%v, want toolu_1", row.ToolUseID)
	}

	// One inner hold should now exist, with AwaitingTaskFor="" (no
	// outer hold to cascade to).
	holds := cache.SnapshotHoldsForTest(userID, agentID, conversation.ProviderAnthropic)
	if len(holds) != 1 {
		t.Fatalf("expected 1 hold; got %d", len(holds))
	}
	if holds[0].Stage != llmproxy.StageAwaitingTaskApproval {
		t.Errorf("hold stage = %q, want awaiting_task_approval", holds[0].Stage)
	}
	if holds[0].AwaitingTaskFor != "" {
		t.Errorf("query-only intercept should leave AwaitingTaskFor empty; got %q", holds[0].AwaitingTaskFor)
	}
	_ = ctx
}

func TestMaybeInterceptInlineTaskDefinition_ExpiresEvictedInlineHold(t *testing.T) {
	ctx := context.Background()
	cache := llmproxy.NewMemoryPendingApprovalCache(time.Minute)
	cache.SetMaxForTest(1)
	userID := "user-inline-evict"
	agentID := "agent-inline-evict"

	if _, err := cache.Hold(ctx, llmproxy.PendingLiteApproval{
		ID:            "cv-oldevictedxxxxxxxxxxxxx",
		UserID:        userID,
		AgentID:       agentID,
		Provider:      conversation.ProviderAnthropic,
		Stage:         llmproxy.StageAwaitingTaskApproval,
		PendingTaskID: "task-old-evicted",
		ToolUse:       conversation.ToolUse{ID: "toolu_old", Name: "Bash"},
	}); err != nil {
		t.Fatalf("seed old inline hold: %v", err)
	}

	cmd := `curl -sS -X POST 'https://clawvisor.local/control/tasks?wait=true&timeout=120&surface=inline' -H 'Content-Type: application/json' --data '` + inlineTaskBody + `'`
	input, err := json.Marshal(map[string]string{"command": cmd})
	if err != nil {
		t.Fatalf("marshal tool input: %v", err)
	}
	tu := conversation.ToolUse{ID: "toolu_new", Name: "Bash", Input: input}
	call, ok := llmproxy.ParseControlToolUse(tu)
	if !ok {
		t.Fatalf("control call did not parse")
	}
	creator := &capturingInlineCreator{pendingTaskID: "task-new-pending"}

	_, handled := llmproxy.MaybeInterceptInlineTaskDefinition(
		httptest.NewRequest("POST", "/v1/messages", nil),
		llmproxy.PostprocessConfig{
			AgentContext: llmproxy.AgentContext{
				AgentUserID: userID,
				AgentID:     agentID,
			},
			ApprovalContext: llmproxy.ApprovalContext{
				PendingApprovals:  cache,
				InlineTaskCreator: creator,
			},
		},
		func(_, _, _ string) {},
		func(string, ...any) {},
		conversation.ProviderAnthropic,
		tu,
		call,
	)
	if !handled {
		t.Fatalf("inline task definition was not intercepted")
	}
	if !creator.expireCalled {
		t.Fatalf("ExpireInlineTask was not called for the evicted inline hold")
	}
	if len(creator.expiredIDs) != 1 || creator.expiredIDs[0] != "task-old-evicted" {
		t.Fatalf("expired ids = %v, want [task-old-evicted]", creator.expiredIDs)
	}
}

func TestMaybeInterceptInlineTaskDefinition_ExpiresPendingTaskOnHoldFailure(t *testing.T) {
	inner := llmproxy.NewMemoryPendingApprovalCache(time.Minute)
	cache := &flakyHoldFromCallN{inner: inner, failOnCall: 1}
	userID := "user-inline-hold-fail"
	agentID := "agent-inline-hold-fail"

	cmd := `curl -sS -X POST 'https://clawvisor.local/control/tasks?wait=true&timeout=120&surface=inline' -H 'Content-Type: application/json' --data '` + inlineTaskBody + `'`
	input, err := json.Marshal(map[string]string{"command": cmd})
	if err != nil {
		t.Fatalf("marshal tool input: %v", err)
	}
	tu := conversation.ToolUse{ID: "toolu_new", Name: "Bash", Input: input}
	call, ok := llmproxy.ParseControlToolUse(tu)
	if !ok {
		t.Fatalf("control call did not parse")
	}
	creator := &capturingInlineCreator{pendingTaskID: "task-hold-failed"}

	_, handled := llmproxy.MaybeInterceptInlineTaskDefinition(
		httptest.NewRequest("POST", "/v1/messages", nil),
		llmproxy.PostprocessConfig{
			AgentContext: llmproxy.AgentContext{
				AgentUserID: userID,
				AgentID:     agentID,
			},
			ApprovalContext: llmproxy.ApprovalContext{
				PendingApprovals:  cache,
				InlineTaskCreator: creator,
			},
		},
		func(_, _, _ string) {},
		func(string, ...any) {},
		conversation.ProviderAnthropic,
		tu,
		call,
	)
	if handled {
		t.Fatalf("hold failure should fall through to dashboard rewrite")
	}
	if !creator.expireCalled {
		t.Fatalf("ExpireInlineTask was not called for operational hold failure")
	}
	if creator.denyCalled {
		t.Fatalf("DenyInlineTask should not be used for operational hold failure")
	}
	if len(creator.expiredIDs) != 1 || creator.expiredIDs[0] != "task-hold-failed" {
		t.Fatalf("expired ids = %v, want [task-hold-failed]", creator.expiredIDs)
	}
}

func TestPostprocess_InlineTaskPromptRendersCredentialsAndRisk(t *testing.T) {
	cache := llmproxy.NewMemoryPendingApprovalCache(time.Minute)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

	taskBody := `{"purpose":"Create GitHub release issues","intent_verification_mode":"strict","expires_in_seconds":600,"expected_tools":[{"tool_name":"Bash","why":"Call the GitHub API."}],"required_credentials":[{"vault_item_id":"github","why":"Create issues in owner/repo."}]}`
	body := anthropicBashControlTasksPostWithQuery(taskBody, "surface=inline")
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})

	got := Postprocess(req, body, "application/json", llmproxy.PostprocessConfig{
		ToolUseEvaluatorFactory: pipelineFactory,
		AgentContext: llmproxy.AgentContext{
			AgentUserID: userID,
			AgentID:     agentID,
		},
		ApprovalContext: llmproxy.ApprovalContext{
			PendingApprovals: cache,
		},
		RewriteContext: llmproxy.RewriteContext{
			Inspector:    insp,
			RewriteOpts:  inspector.DefaultRewriteOpts("http://localhost:25297"),
			CallerNonces: llmproxy.NewMemoryCallerNonceCache(time.Minute),
			Store:        st,
		},
		RoutingContext: llmproxy.RoutingContext{
			ControlBaseURL: "http://localhost:25297",
		},
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
	cache := llmproxy.NewMemoryPendingApprovalCache(time.Minute)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

	taskBody := `{"purpose":"Call agentphone","intent_verification_mode":"strict","expires_in_seconds":600,"expected_tools":[{"tool_name":"Bash","why":"Call the agentphone API."}],"required_credentials":[{"vault_item_id":"agentphone"}]}`
	body := anthropicBashControlTasksPostWithQuery(taskBody, "surface=inline")
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})

	got := Postprocess(req, body, "application/json", llmproxy.PostprocessConfig{
		ToolUseEvaluatorFactory: pipelineFactory,
		AgentContext: llmproxy.AgentContext{
			AgentUserID: userID,
			AgentID:     agentID,
		},
		ApprovalContext: llmproxy.ApprovalContext{
			PendingApprovals: cache,
		},
		RewriteContext: llmproxy.RewriteContext{
			Inspector:    insp,
			RewriteOpts:  inspector.DefaultRewriteOpts("http://localhost:25297"),
			CallerNonces: llmproxy.NewMemoryCallerNonceCache(time.Minute),
			Store:        st,
		},
		RoutingContext: llmproxy.RoutingContext{
			ControlBaseURL: "http://localhost:25297",
		},
	})

	out := string(got.Body)
	if strings.Contains(out, "Clawvisor wants to create a task") {
		t.Fatalf("invalid inline task should not render an approval prompt: %s", out)
	}
	if !strings.Contains(out, "http://localhost:25297/api/control/tasks") {
		t.Fatalf("invalid inline task should fall through to control rewrite so the tool receives the validation error: %s", out)
	}

	holds := len(cache.SnapshotHoldsForTest(userID, agentID, conversation.ProviderAnthropic))
	if holds != 0 {
		t.Fatalf("invalid inline task should not create an approval hold, got %d", holds)
	}
}

func TestPostprocess_InlineTaskStandingWithExpiresFallsThrough(t *testing.T) {
	// lifetime=standing + expires_in_seconds>0 is rejected at task
	// creation time (tasks_inline.go); the intercept catches the same
	// conflict before rendering so the user never sees an approval
	// prompt that would resolve to a failed create.
	cache := llmproxy.NewMemoryPendingApprovalCache(time.Minute)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

	taskBody := `{"purpose":"Standing task with expiry","lifetime":"standing","expires_in_seconds":600,"expected_tools":[{"tool_name":"Bash","why":"x"}]}`
	body := anthropicBashControlTasksPostWithQuery(taskBody, "surface=inline")
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})

	got := Postprocess(req, body, "application/json", llmproxy.PostprocessConfig{
		ToolUseEvaluatorFactory: pipelineFactory,
		AgentContext: llmproxy.AgentContext{
			AgentUserID: userID,
			AgentID:     agentID,
		},
		ApprovalContext: llmproxy.ApprovalContext{
			PendingApprovals: cache,
		},
		RewriteContext: llmproxy.RewriteContext{
			Inspector:    insp,
			RewriteOpts:  inspector.DefaultRewriteOpts("http://localhost:25297"),
			CallerNonces: llmproxy.NewMemoryCallerNonceCache(time.Minute),
			Store:        st,
		},
		RoutingContext: llmproxy.RoutingContext{
			ControlBaseURL: "http://localhost:25297",
		},
	})

	out := string(got.Body)
	if strings.Contains(out, "Clawvisor wants to create a task") {
		t.Fatalf("standing+expires_in_seconds should not render an approval prompt: %s", out)
	}
	if !strings.Contains(out, "http://localhost:25297/api/control/tasks") {
		t.Fatalf("standing+expires_in_seconds should fall through to control rewrite: %s", out)
	}

	holds := len(cache.SnapshotHoldsForTest(userID, agentID, conversation.ProviderAnthropic))
	if holds != 0 {
		t.Fatalf("standing+expires_in_seconds should not create an approval hold, got %d", holds)
	}
}

func TestPostprocess_InlineTaskBareNoSignalRoutesToDashboard(t *testing.T) {
	// No prior `task` reply AND no surface=inline → fall through to
	// the regular control-rewrite path (dashboard task creation).
	cache := llmproxy.NewMemoryPendingApprovalCache(time.Minute)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

	body := anthropicBashControlTasksPost(inlineTaskBody)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})

	got := Postprocess(req, body, "application/json", llmproxy.PostprocessConfig{
		ToolUseEvaluatorFactory: pipelineFactory,
		AgentContext: llmproxy.AgentContext{
			AgentUserID: userID,
			AgentID:     agentID,
		},
		ApprovalContext: llmproxy.ApprovalContext{
			PendingApprovals: cache,
		},
		RewriteContext: llmproxy.RewriteContext{
			Inspector:    insp,
			RewriteOpts:  inspector.DefaultRewriteOpts("http://localhost:25297"),
			CallerNonces: llmproxy.NewMemoryCallerNonceCache(time.Minute),
			Store:        st,
		},
		RoutingContext: llmproxy.RoutingContext{
			ControlBaseURL: "http://localhost:25297",
		},
	})
	if !got.Rewritten {
		t.Fatalf("expected dashboard control-rewrite when no inline signal; got %s", got.Body)
	}
	if !strings.Contains(string(got.Body), "http://localhost:25297/api/control/tasks") {
		t.Fatalf("expected control URL rewritten; got %s", got.Body)
	}
}

func TestReleaseInlineTaskApproval_QueryOnlyHoldNoOuterCascade(t *testing.T) {
	// A query-signal inner hold has AwaitingTaskFor="" — the release
	// path must NOT try to drop a non-existent outer hold.
	cache := llmproxy.NewMemoryPendingApprovalCache(time.Minute)
	ctx := context.Background()

	inner, err := cache.Hold(ctx, llmproxy.PendingLiteApproval{
		ID:       "cv-innerholdxxxxxxxxxxxxxxxxx",
		UserID:   "user-1",
		AgentID:  "agent-1",
		Provider: conversation.ProviderAnthropic,
		ToolUse:  conversation.ToolUse{ID: "toolu_post", Name: "Bash"},
		Stage:    llmproxy.StageAwaitingTaskApproval,
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
		resp: &llmproxy.InlineApprovedTask{ID: "task-q", Status: "active", ApprovalSource: "inline_chat", Lifetime: "session", ApprovalRecordID: "appr-q"},
	}
	approveBody := []byte(`{"messages":[{"role":"user","content":"yes ` + inner.Pending.ID + `"}]}`)
	rewrite, err := llmproxy.RewriteInlineTaskApprovalReply(ctx, llmproxy.InlineApprovalRewriteRequest{
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

// stubInlineRiskAssessor is a recorder for the LLM-backed task risk path.
type stubInlineRiskAssessor struct {
	got    llmproxy.TaskRiskAssessRequest
	called bool
	out    *llmproxy.TaskRiskAssessment
}

func (s *stubInlineRiskAssessor) AssessEnvelope(_ context.Context, req llmproxy.TaskRiskAssessRequest) *llmproxy.TaskRiskAssessment {
	s.called = true
	s.got = req
	return s.out
}

// autoApproveFixture bundles the recurring test setup for the
// conversation-based auto-approval gate so each test can focus on the
// specific condition being exercised.
type autoApproveFixture struct {
	t        *testing.T
	cache    *llmproxy.MemoryPendingApprovalCache
	store    store.Store
	userID   string
	agentID  string
	assessor *stubInlineRiskAssessor
	creator  *capturingInlineCreator
	insp     *inspector.Inspector
	drifts   llmproxy.ScopeDriftRegistry
}

func newAutoApproveFixture(t *testing.T, assessment *llmproxy.TaskRiskAssessment) autoApproveFixture {
	t.Helper()
	cache := llmproxy.NewMemoryPendingApprovalCache(time.Minute)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	return autoApproveFixture{
		t:        t,
		cache:    cache,
		store:    st,
		userID:   userID,
		agentID:  agentID,
		assessor: &stubInlineRiskAssessor{out: assessment},
		creator: &capturingInlineCreator{
			resp: &llmproxy.InlineApprovedTask{
				ID:               "task-auto-approved",
				Status:           "active",
				Purpose:          "Build a landing page at /tmp/landing",
				Lifetime:         "session",
				ApprovalSource:   "inline_chat",
				ApprovalRecordID: "appr-auto",
			},
		},
		insp:   inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{}),
		drifts: llmproxy.NewMemoryScopeDriftRegistry(0),
	}
}

func (f autoApproveFixture) run(threshold string, turns []string) llmproxy.PostprocessResult {
	body := anthropicBashControlTasksPostWithQuery(inlineTaskBody, "surface=inline")
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	return Postprocess(req, body, "application/json", llmproxy.PostprocessConfig{
		ToolUseEvaluatorFactory: pipelineFactory,
		AgentContext: llmproxy.AgentContext{
			AgentUserID: f.userID,
			AgentID:     f.agentID,
			AgentName:   "test-agent",
		},
		AuthorizationContext: llmproxy.AuthorizationContext{
			ScopeDrifts: f.drifts,
		},
		ApprovalContext: llmproxy.ApprovalContext{
			PendingApprovals:                 f.cache,
			TaskRiskAssessor:                 f.assessor,
			RecentUserTurns:                  turns,
			ConversationAutoApproveThreshold: threshold,
			InlineTaskCreator:                f.creator,
		},
		RewriteContext: llmproxy.RewriteContext{
			Inspector:    f.insp,
			RewriteOpts:  inspector.DefaultRewriteOpts("http://localhost:25297"),
			CallerNonces: llmproxy.NewMemoryCallerNonceCache(time.Minute),
			Store:        f.store,
		},
		RoutingContext: llmproxy.RoutingContext{
			ControlBaseURL: "http://localhost:25297",
		},
	})
}

func (f autoApproveFixture) holdCount() int {
	return len(f.cache.SnapshotHoldsForTest(f.userID, f.agentID, conversation.ProviderAnthropic))
}

// TestAutoApproveUserNotice_TruncatesByRune ensures the user-facing
// notice's purpose suffix is truncated on rune boundaries, not byte
// boundaries. The purpose is model-controlled and can be non-ASCII;
// a byte-slice would split a multibyte UTF-8 sequence and render as
// U+FFFD once JSON-marshalled.
func TestAutoApproveUserNotice_TruncatesByRune(t *testing.T) {
	// Each Chinese character is 3 bytes in UTF-8; 300 chars = 900
	// bytes, well over the 200-rune cap.
	long := strings.Repeat("漢", 300)
	got := llmproxy.AutoApproveUserNotice(long)
	// Result must be valid UTF-8 — no replacement chars from
	// mid-rune slicing.
	if strings.Contains(got, "�") {
		t.Errorf("notice contains U+FFFD; truncation split a rune mid-sequence:\n%s", got)
	}
	// The truncated purpose body sits inside the backticked envelope,
	// so the ellipsis appears immediately before the closing backtick
	// rather than at the very end of the rendered string.
	if !strings.Contains(got, "…`") {
		t.Errorf("notice body should end with ellipsis before the closing backtick; got: %s", got)
	}
}

// TestAutoApproveUserNotice_StripsBackticksFromPurpose pins the
// defense that keeps a model-authored purpose from terminating the
// inline-code span the notice is wrapped in. Without the strip, a
// purpose like "fix the `foo` helper" would render the trailing
// guidance as prose instead of code, and any embedded backticks
// would leak the wrapping in chat UIs.
func TestAutoApproveUserNotice_StripsBackticksFromPurpose(t *testing.T) {
	got := llmproxy.AutoApproveUserNotice("fix the `foo` helper")
	if strings.Count(got, "`") != 2 {
		t.Errorf("expected exactly the two wrapping backticks; got %q", got)
	}
	if !strings.Contains(got, "fix the foo helper") {
		t.Errorf("expected purpose with backticks stripped; got %q", got)
	}
}

// TestAutoApprove_FiresOnLowRiskYesMatch is the happy path: user
// authorized the work in the conversation, risk is low, threshold is
// low — gate fires, task is created, no prompt rendered.
func TestAutoApprove_FiresOnLowRiskYesMatch(t *testing.T) {
	f := newAutoApproveFixture(t, &llmproxy.TaskRiskAssessment{
		RiskLevel:              "low",
		Explanation:            "Read-only landing-page work.",
		IntentMatch:            "yes",
		IntentMatchExplanation: "User asked to build a landing page at /tmp/landing.",
	})

	got := f.run("low", []string{"build me a landing page at /tmp/landing"})

	if !f.creator.called {
		t.Fatal("auto-approve gate must invoke the inline task creator")
	}
	if f.holdCount() != 0 {
		t.Errorf("auto-approve gate must not register a pending hold; got %d holds", f.holdCount())
	}
	out := string(got.Body)
	if strings.Contains(out, "Clawvisor wants to create a task") {
		t.Errorf("auto-approve must skip the human prompt; got %s", out)
	}
	// New placeholder shape: the augmentation context lands as the
	// tool_result content on the next inbound /v1/messages via the
	// scope-drift inbound rewriter. The response leg carries a Bash
	// placeholder tool_use with the CLAWVISOR_AUTO_APPROVED marker so
	// the harness's local execution is a harmless `:` and the operator
	// sees what happened in the transcript comment.
	if !strings.Contains(out, llmproxy.AutoApprovePlaceholderMarker) {
		t.Errorf("auto-approve must substitute a Bash placeholder carrying the AUTO_APPROVED marker; got %s", out)
	}
	if !strings.Contains(out, "task-auto-approved") {
		t.Errorf("placeholder should name the created task id; got %s", out)
	}
	subst, ok := f.drifts.LookupPendingSubstitution(context.Background(), llmproxy.PendingSubstitutionKey{
		AgentID:        f.agentID,
		ConversationID: "",
		ToolUseID:      "toolu_1",
	})
	if !ok {
		t.Fatal("auto-approve must register a pending substitution under the model's original tool_use_id")
	}
	if !strings.Contains(subst.MenuText, "was created and approved by the user") {
		t.Errorf("pending substitution must carry the success augmentation; got %q", subst.MenuText)
	}
	if !strings.Contains(subst.MenuText, "task-auto-approved") {
		t.Errorf("pending substitution must carry the created task id; got %q", subst.MenuText)
	}
	// The assessor still ran and saw the recent turns — that's how it
	// arrived at intent_match=yes in the first place.
	if !f.assessor.called {
		t.Fatal("expected risk assessor to be called even on auto-approve path")
	}
	if len(f.assessor.got.RecentUserTurns) == 0 {
		t.Errorf("assessor should receive recent user turns; got %+v", f.assessor.got.RecentUserTurns)
	}
}

// TestAutoApprove_VerdictUsesPlaceholder verifies that the auto-approve
// gate sets a SubstituteWithToolCall on the verdict — the canonical
// Bash placeholder mechanism shared with scope-drift and recoverable-
// deny. The legacy Continue+SubstituteWith pair is gone (continuation
// was removed alongside this migration).
func TestAutoApprove_VerdictUsesPlaceholder(t *testing.T) {
	f := newAutoApproveFixture(t, &llmproxy.TaskRiskAssessment{
		RiskLevel:   "low",
		IntentMatch: "yes",
	})
	got := f.run("low", []string{"build me a landing page at /tmp/landing"})
	if len(got.Decisions) == 0 {
		t.Fatal("expected at least one tool_use decision")
	}
	v := got.Decisions[0].Verdict
	if v.SubstituteWithToolCall == nil {
		t.Fatal("auto-approve verdict must set SubstituteWithToolCall (Bash placeholder)")
	}
	if v.SubstituteWithToolCall.Name != "Bash" {
		t.Errorf("placeholder should be a Bash no-op; got %q", v.SubstituteWithToolCall.Name)
	}
	cmd, _ := v.SubstituteWithToolCall.Input["command"].(string)
	if !strings.Contains(cmd, llmproxy.AutoApprovePlaceholderMarker) {
		t.Errorf("placeholder command must carry the AUTO_APPROVED marker; got %q", cmd)
	}
	if !v.SuppressSubstituteText {
		t.Error("auto-approve verdict must set SuppressSubstituteText so the rewriter doesn't double-emit")
	}
}

// TestAutoApprove_DoesNotFireOnThresholdOff exercises the default case.
// Even with a perfect intent match and low risk, threshold="off" must
// keep the human in the loop.
func TestAutoApprove_DoesNotFireOnThresholdOff(t *testing.T) {
	f := newAutoApproveFixture(t, &llmproxy.TaskRiskAssessment{
		RiskLevel:   "low",
		IntentMatch: "yes",
	})
	got := f.run("off", []string{"build me a landing page"})

	if f.creator.called {
		t.Fatal("threshold=off must never auto-approve")
	}
	if f.holdCount() != 1 {
		t.Errorf("threshold=off must register a pending hold; got %d holds", f.holdCount())
	}
	if !strings.Contains(string(got.Body), "Clawvisor wants to create a task") {
		t.Errorf("threshold=off must render the human prompt")
	}
}

// TestAutoApprove_DoesNotFireOnRiskAboveThreshold confirms the cap
// works: low threshold with medium risk must still prompt the human.
func TestAutoApprove_DoesNotFireOnRiskAboveThreshold(t *testing.T) {
	f := newAutoApproveFixture(t, &llmproxy.TaskRiskAssessment{
		RiskLevel:   "medium",
		IntentMatch: "yes",
	})
	got := f.run("low", []string{"build me a landing page"})

	if f.creator.called {
		t.Fatal("risk above threshold must not auto-approve")
	}
	if f.holdCount() != 1 {
		t.Errorf("expected pending hold; got %d", f.holdCount())
	}
	if !strings.Contains(string(got.Body), "Clawvisor wants to create a task") {
		t.Error("expected human prompt")
	}
}

// TestAutoApprove_DoesNotFireOnPartialIntent ensures the gate is
// strict: only a "yes" intent_match counts. "partial" — user asked for
// some but not all of the requested scope — must prompt.
func TestAutoApprove_DoesNotFireOnPartialIntent(t *testing.T) {
	f := newAutoApproveFixture(t, &llmproxy.TaskRiskAssessment{
		RiskLevel:   "low",
		IntentMatch: "partial",
	})
	got := f.run("medium", []string{"check my calendar"})

	if f.creator.called {
		t.Fatal("intent_match=partial must not auto-approve")
	}
	if !strings.Contains(string(got.Body), "Clawvisor wants to create a task") {
		t.Error("expected human prompt")
	}
}

// TestAutoApprove_DoesNotFireOnUnknownIntent covers the empty-context
// path: no recent human turns → assessor emits intent_match=unknown →
// gate must not fire even at threshold=medium.
func TestAutoApprove_DoesNotFireOnUnknownIntent(t *testing.T) {
	f := newAutoApproveFixture(t, &llmproxy.TaskRiskAssessment{
		RiskLevel:   "low",
		IntentMatch: "unknown",
	})
	got := f.run("medium", nil)

	if f.creator.called {
		t.Fatal("intent_match=unknown must not auto-approve")
	}
	if !strings.Contains(string(got.Body), "Clawvisor wants to create a task") {
		t.Error("expected human prompt")
	}
}

// TestAutoApprove_DoesNotFireWithoutRecentTurnsEvenWhenAssessorSaysYes
// is the deterministic-floor test. A misbehaving or compromised LLM
// assessor could emit intent_match="yes" even when the runtime knows
// the inbound transcript carried zero human-authored turns. The gate
// must reject this on its own — it cannot defer that question to the
// LLM, because the assessor itself is the layer being constrained.
func TestAutoApprove_DoesNotFireWithoutRecentTurnsEvenWhenAssessorSaysYes(t *testing.T) {
	f := newAutoApproveFixture(t, &llmproxy.TaskRiskAssessment{
		RiskLevel:              "low",
		IntentMatch:            "yes",
		IntentMatchExplanation: "Assessor incorrectly claims user authorized this.",
	})
	// Empty turns slice — the extraction layer found no genuine
	// human-authored content in the inbound body.
	got := f.run("medium", nil)

	if f.creator.called {
		t.Fatal("empty recent turns must prevent auto-approval regardless of intent_match")
	}
	if f.holdCount() != 1 {
		t.Errorf("empty recent turns must register a human approval hold; got %d holds", f.holdCount())
	}
	if !strings.Contains(string(got.Body), "Clawvisor wants to create a task") {
		t.Error("expected human prompt when no recent turns were extracted")
	}
}

// TestAutoApprove_DoesNotFireWithConflicts ensures internal task
// inconsistencies always reach the human. Even an intent_match=yes
// verdict with low risk must prompt when the assessor flagged a
// conflict.
func TestAutoApprove_DoesNotFireWithConflicts(t *testing.T) {
	f := newAutoApproveFixture(t, &llmproxy.TaskRiskAssessment{
		RiskLevel:   "low",
		IntentMatch: "yes",
		Conflicts: []llmproxy.TaskRiskConflict{{
			Field:       "expected_use",
			Description: "expected_use mentions sending email but purpose is read-only",
			Severity:    "warning",
		}},
	})
	got := f.run("medium", []string{"build me a landing page"})

	if f.creator.called {
		t.Fatal("conflicts present must not auto-approve")
	}
	if !strings.Contains(string(got.Body), "Clawvisor wants to create a task") {
		t.Error("expected human prompt")
	}
}

// TestAutoApprove_WritesTaskLinkedAuditRow confirms the audit trail
// for an auto-approved task carries the task_id / approval_record_id
// linkage that downstream consumers depend on. Without this row,
// dashboards filtering by task_id can't reconstruct which task was
// auto-approved — only that the intercept fired.
func TestAutoApprove_WritesTaskLinkedAuditRow(t *testing.T) {
	f := newAutoApproveFixture(t, &llmproxy.TaskRiskAssessment{
		RiskLevel:              "low",
		Explanation:            "Read-only landing-page work.",
		IntentMatch:            "yes",
		IntentMatchExplanation: "User asked for it.",
	})

	body := anthropicBashControlTasksPostWithQuery(inlineTaskBody, "surface=inline")
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	got := Postprocess(req, body, "application/json", llmproxy.PostprocessConfig{
		ToolUseEvaluatorFactory: pipelineFactory,
		AgentContext: llmproxy.AgentContext{
			AgentUserID: f.userID,
			AgentID:     f.agentID,
			AgentName:   "test-agent",
		},
		AuditContext: llmproxy.AuditContext{
			Audit:     llmproxy.NewAuditEmitter(f.store, nil, nil),
			RequestID: "req-auto-approve-audit",
		},
		AuthorizationContext: llmproxy.AuthorizationContext{
			ScopeDrifts: f.drifts,
		},
		ApprovalContext: llmproxy.ApprovalContext{
			PendingApprovals:                 f.cache,
			TaskRiskAssessor:                 f.assessor,
			RecentUserTurns:                  []string{"build me a landing page at /tmp/landing"},
			ConversationAutoApproveThreshold: "low",
			InlineTaskCreator:                f.creator,
		},
		RewriteContext: llmproxy.RewriteContext{
			Inspector:    f.insp,
			RewriteOpts:  inspector.DefaultRewriteOpts("http://localhost:25297"),
			CallerNonces: llmproxy.NewMemoryCallerNonceCache(time.Minute),
			Store:        f.store,
		},
		RoutingContext: llmproxy.RoutingContext{
			ControlBaseURL: "http://localhost:25297",
		},
	})

	if !f.creator.called {
		t.Fatal("auto-approve gate should have fired")
	}
	// Wire shape: response leg carries the Bash placeholder; the
	// augmentation context lands in the pending substitution registry
	// for delivery on the next inbound /v1/messages.
	if !strings.Contains(string(got.Body), llmproxy.AutoApprovePlaceholderMarker) {
		t.Errorf("expected AUTO_APPROVED placeholder marker in body; got %s", got.Body)
	}
	subst, ok := f.drifts.LookupPendingSubstitution(req.Context(), llmproxy.PendingSubstitutionKey{
		AgentID:        f.agentID,
		ConversationID: "",
		ToolUseID:      "toolu_1",
	})
	if !ok {
		t.Fatal("auto-approve must register a pending substitution carrying the success augmentation")
	}
	if !strings.Contains(subst.MenuText, "was created and approved by the user") {
		t.Errorf("pending substitution must carry the success augmentation; got %q", subst.MenuText)
	}

	rows, _, err := f.store.ListAuditEntries(req.Context(), f.userID, store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAuditEntries: %v", err)
	}
	// Expect at least two rows: the generic tool_use approve row and
	// the task-linked inline_auto_approved row.
	var taskLinked *store.AuditEntry
	for i := range rows {
		if rows[i].Action == "lite_proxy.task_create.inline_auto_approved" {
			taskLinked = rows[i]
			break
		}
	}
	if taskLinked == nil {
		t.Fatalf("missing lite_proxy.task_create.inline_auto_approved audit row; got actions %v", auditActions(rows))
	}
	if taskLinked.TaskID == nil || *taskLinked.TaskID != "task-auto-approved" {
		t.Errorf("task-linked row task_id = %v, want task-auto-approved", taskLinked.TaskID)
	}
	if taskLinked.Decision != "allow" || taskLinked.Outcome != "inline_task_auto_approved" {
		t.Errorf("decision=%q outcome=%q, want allow / inline_task_auto_approved",
			taskLinked.Decision, taskLinked.Outcome)
	}
	if taskLinked.ToolUseID == nil || *taskLinked.ToolUseID == "" {
		t.Error("expected tool_use_id set on auto-approve audit row")
	}
}

func auditActions(rows []*store.AuditEntry) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Action
	}
	return out
}

// TestAutoApprove_FallsBackOnCreatorError exercises the create-failure
// safety net: when the gate would fire but the inline task creator
// errors, fall through to the human prompt rather than dropping the
// task on the floor.
func TestAutoApprove_FallsBackOnCreatorError(t *testing.T) {
	f := newAutoApproveFixture(t, &llmproxy.TaskRiskAssessment{
		RiskLevel:   "low",
		IntentMatch: "yes",
	})
	f.creator.fail = true
	got := f.run("low", []string{"build me a landing page"})

	if f.holdCount() != 1 {
		t.Errorf("creator error must leave a fall-back hold; got %d holds", f.holdCount())
	}
	if !strings.Contains(string(got.Body), "Clawvisor wants to create a task") {
		t.Error("creator error must render the human prompt")
	}
}

// TestAutoApprove_FallsBackWhenCreatorMissing covers the
// misconfigured-runtime path. If the threshold says fire but no
// creator is wired, the runtime must NOT silently drop the task or
// pretend to approve; it should fall through to the human prompt and
// audit the gap.
func TestAutoApprove_FallsBackWhenCreatorMissing(t *testing.T) {
	cache := llmproxy.NewMemoryPendingApprovalCache(time.Minute)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	assessor := &stubInlineRiskAssessor{out: &llmproxy.TaskRiskAssessment{
		RiskLevel: "low", IntentMatch: "yes",
	}}
	body := anthropicBashControlTasksPostWithQuery(inlineTaskBody, "surface=inline")
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})

	got := Postprocess(req, body, "application/json", llmproxy.PostprocessConfig{
		ToolUseEvaluatorFactory: pipelineFactory,
		AgentContext: llmproxy.AgentContext{
			AgentUserID: userID,
			AgentID:     agentID,
			AgentName:   "test-agent",
		},
		ApprovalContext: llmproxy.ApprovalContext{
			PendingApprovals:                 cache,
			TaskRiskAssessor:                 assessor,
			RecentUserTurns:                  []string{"build me a landing page"},
			ConversationAutoApproveThreshold: "low",
		},
		RewriteContext: llmproxy.RewriteContext{
			Inspector:    insp,
			RewriteOpts:  inspector.DefaultRewriteOpts("http://localhost:25297"),
			CallerNonces: llmproxy.NewMemoryCallerNonceCache(time.Minute),
			Store:        st,
		},
		RoutingContext: llmproxy.RoutingContext{
			ControlBaseURL: "http://localhost:25297",
		},
	})

	if !strings.Contains(string(got.Body), "Clawvisor wants to create a task") {
		t.Error("missing creator must fall back to human prompt")
	}
}

func TestPostprocess_InlineTaskSubstitutesLLMRiskExplanation(t *testing.T) {
	cache := llmproxy.NewMemoryPendingApprovalCache(time.Minute)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

	assessor := &stubInlineRiskAssessor{out: &llmproxy.TaskRiskAssessment{
		RiskLevel:   "medium",
		Explanation: "Writing to /tmp may collide with other processes' files.",
	}}

	body := anthropicBashControlTasksPostWithQuery(inlineTaskBody, "surface=inline")
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})

	got := Postprocess(req, body, "application/json", llmproxy.PostprocessConfig{
		ToolUseEvaluatorFactory: pipelineFactory,
		AgentContext: llmproxy.AgentContext{
			AgentUserID: userID,
			AgentID:     agentID,
			AgentName:   "test-agent",
		},
		ApprovalContext: llmproxy.ApprovalContext{
			PendingApprovals: cache,
			TaskRiskAssessor: assessor,
		},
		RewriteContext: llmproxy.RewriteContext{
			Inspector:    insp,
			RewriteOpts:  inspector.DefaultRewriteOpts("http://localhost:25297"),
			CallerNonces: llmproxy.NewMemoryCallerNonceCache(time.Minute),
			Store:        st,
		},
		RoutingContext: llmproxy.RoutingContext{
			ControlBaseURL: "http://localhost:25297",
		},
	})

	if !assessor.called {
		t.Fatalf("expected LLM risk assessor to be called during inline task intercept")
	}
	if assessor.got.AgentName != "test-agent" {
		t.Fatalf("assessor request missing agent name: %+v", assessor.got)
	}
	if assessor.got.Purpose == "" {
		t.Fatalf("assessor request missing purpose: %+v", assessor.got)
	}
	if len(assessor.got.ExpectedTools) == 0 {
		t.Fatalf("assessor request missing expected tools: %+v", assessor.got)
	}
	out := string(got.Body)
	if !strings.Contains(out, "Writing to /tmp may collide") {
		t.Fatalf("approval prompt should carry LLM explanation; got %s", out)
	}
	if !strings.Contains(out, "medium") {
		t.Fatalf("approval prompt should reflect LLM risk level; got %s", out)
	}
}

// TestPostprocess_InlineTaskFallsBackWhenLLMUnknown confirms the merge
// path: an "unknown" LLM verdict (spend-cap exhausted, parse failure, etc.)
// drops back to the deterministic envelope policy so the prompt still
// renders a risk read.
func TestPostprocess_InlineTaskFallsBackWhenLLMUnknown(t *testing.T) {
	cache := llmproxy.NewMemoryPendingApprovalCache(time.Minute)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

	assessor := &stubInlineRiskAssessor{out: &llmproxy.TaskRiskAssessment{
		RiskLevel:   "unknown",
		Explanation: "Risk assessment failed: spend cap exhausted",
	}}

	body := anthropicBashControlTasksPostWithQuery(inlineTaskBody, "surface=inline")
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})

	got := Postprocess(req, body, "application/json", llmproxy.PostprocessConfig{
		ToolUseEvaluatorFactory: pipelineFactory,
		AgentContext: llmproxy.AgentContext{
			AgentUserID: userID,
			AgentID:     agentID,
		},
		ApprovalContext: llmproxy.ApprovalContext{
			PendingApprovals: cache,
			TaskRiskAssessor: assessor,
		},
		RewriteContext: llmproxy.RewriteContext{
			Inspector:    insp,
			RewriteOpts:  inspector.DefaultRewriteOpts("http://localhost:25297"),
			CallerNonces: llmproxy.NewMemoryCallerNonceCache(time.Minute),
			Store:        st,
		},
		RoutingContext: llmproxy.RoutingContext{
			ControlBaseURL: "http://localhost:25297",
		},
	})

	if !assessor.called {
		t.Fatalf("expected assessor to be called even when it returns unknown")
	}
	out := string(got.Body)
	if !strings.Contains(out, "constrained runtime envelope") {
		t.Fatalf("expected deterministic-policy fallback explanation; got %s", out)
	}
}

func TestPostprocess_InlineTaskMalformedBodyFallsThrough(t *testing.T) {
	ctx := context.Background()
	cache := llmproxy.NewMemoryPendingApprovalCache(time.Minute)
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

	if _, err := cache.Hold(ctx, llmproxy.PendingLiteApproval{
		ID:       "cv-origtooluuid00000000000001",
		UserID:   userID,
		AgentID:  agentID,
		Provider: conversation.ProviderAnthropic,
		ToolUse:  conversation.ToolUse{ID: "toolu_bad", Name: "Bash"},
		Stage:    llmproxy.StageAwaitingTaskDefinition,
	}); err != nil {
		t.Fatal(err)
	}

	// Body that's not valid JSON for TaskCreateRequest — actually empty.
	body := anthropicBashControlTasksPost(`{"purpose":""}`)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})

	got := Postprocess(req, body, "application/json", llmproxy.PostprocessConfig{
		ToolUseEvaluatorFactory: pipelineFactory,
		AgentContext: llmproxy.AgentContext{
			AgentUserID: userID,
			AgentID:     agentID,
		},
		ApprovalContext: llmproxy.ApprovalContext{
			PendingApprovals: cache,
		},
		RewriteContext: llmproxy.RewriteContext{
			Inspector:    insp,
			RewriteOpts:  inspector.DefaultRewriteOpts("http://localhost:25297"),
			CallerNonces: llmproxy.NewMemoryCallerNonceCache(time.Minute),
			Store:        st,
		},
		RoutingContext: llmproxy.RoutingContext{
			ControlBaseURL: "http://localhost:25297",
		},
	})

	if !got.Rewritten {
		t.Fatalf("expected fallback to regular control rewrite on missing purpose; got %s", got.Body)
	}
}
