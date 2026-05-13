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
