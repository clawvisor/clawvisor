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

func TestReleaseInlineTaskApprovalSynthesizesRunnableCatCmd(t *testing.T) {
	// Regression: the synthetic tool_use input must be a real Bash
	// invocation (command field carrying a heredoc cat) — not a raw
	// task-payload JSON map. The harness validates tool_use inputs
	// against the tool's schema; Bash with an unexpected `task_id`
	// field rejects the entire call with InputValidationError.
	cache := NewMemoryPendingApprovalCache(time.Minute)
	_, innerID := seedInlineTaskHolds(t, cache)

	creator := &fakeInlineTaskCreator{
		resp: &InlineApprovedTask{
			ID:             "task-uuid-rt",
			Status:         "active",
			ApprovalSource: "inline_chat",
			Lifetime:       "session",
			Purpose:        "Run command",
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
	if result.Decision != "allow" {
		t.Fatalf("expected allow; got %+v", result)
	}
	// Body is an Anthropic synthesized assistant message; the
	// tool_use input must include a `command` field with a cat
	// heredoc, NOT a top-level `task_id` field.
	out := string(result.Body)
	if !strings.Contains(out, `"command"`) {
		t.Errorf("synthetic tool_use must carry a `command` field for Bash; body=%s", out)
	}
	// The body is JSON-encoded so `<<` is escaped as `<<`.
	if !strings.Contains(out, "cat \\u003c\\u003c") {
		t.Errorf("synthetic command must be a cat heredoc; body=%s", out)
	}
	if !strings.Contains(out, `task-uuid-rt`) {
		t.Errorf("synthetic command should print the task id; body=%s", out)
	}
	// Sanity: the JSON-decoded input shouldn't have task_id at the top
	// level. That's what was failing the harness.
	if strings.Contains(out, `"task_id":"task-uuid-rt"`) && !strings.Contains(out, `task-uuid-rt\nCV_TASK_RESULT`) {
		// the only place "task_id":"task-uuid-rt" should appear is
		// inside the cat heredoc body — not as a top-level input field.
		// We assert that by requiring the closing CV_TASK_RESULT
		// delimiter to appear nearby. (Heuristic; full structural
		// check would require unmarshaling.)
		idx := strings.Index(out, `"task_id":"task-uuid-rt"`)
		region := out[max(idx-20, 0):min(idx+80, len(out))]
		if !strings.Contains(region, "CV_TASK_RESULT") && !strings.Contains(region, "\\nCV_TASK_RESULT") {
			t.Errorf("task_id appears OUTSIDE the cat heredoc — regression; nearby=%q", region)
		}
	}
}

func TestInlineTaskSyntheticInput_PreservesCodexExecCommandFields(t *testing.T) {
	// Codex exec_command shape carries workdir/yield_time_ms/etc.
	// alongside `cmd`. The synth must preserve those and replace
	// only `cmd`.
	inner := &PendingLiteApproval{
		ToolUse: conversation.ToolUse{
			Name: "exec_command",
			Input: []byte(`{
				"cmd": "curl -X POST ... --data @- <<JSON ...",
				"workdir": "/tmp/x",
				"yield_time_ms": 180000,
				"max_output_tokens": 2000
			}`),
		},
	}
	task := &InlineApprovedTask{ID: "task-zz", Status: "active"}
	got := inlineTaskSyntheticInput(inner, task, "exec_command")
	if cmd, _ := got["cmd"].(string); !strings.HasPrefix(cmd, "cat <<") {
		t.Errorf("expected `cmd` rewritten to cat heredoc; got %v", got["cmd"])
	}
	if _, present := got["command"]; present {
		t.Errorf("exec_command shape should not gain `command` field; got %v", got)
	}
	if got["workdir"] != "/tmp/x" || got["max_output_tokens"] == nil {
		t.Errorf("preserved fields were dropped; got %v", got)
	}
}

func TestInlineTaskSyntheticInput_BashShape(t *testing.T) {
	inner := &PendingLiteApproval{
		ToolUse: conversation.ToolUse{
			Name:  "Bash",
			Input: []byte(`{"command":"curl ..."}`),
		},
	}
	task := &InlineApprovedTask{ID: "task-bb", Status: "active"}
	got := inlineTaskSyntheticInput(inner, task, "Bash")
	if cmd, _ := got["command"].(string); !strings.HasPrefix(cmd, "cat <<") {
		t.Errorf("expected `command` rewritten to cat heredoc; got %v", got["command"])
	}
	if _, present := got["cmd"]; present {
		t.Errorf("Bash shape should not gain `cmd` field; got %v", got)
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
