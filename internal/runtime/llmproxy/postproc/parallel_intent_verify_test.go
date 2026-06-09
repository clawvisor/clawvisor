package postproc

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	runtimedecision "github.com/clawvisor/clawvisor/pkg/runtime/decision"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// blockingIntentVerifier blocks each Verify call until release is closed.
// Tracks peak in-flight calls so the test can prove the proxy fans out
// verifier calls in parallel rather than serializing them.
type blockingIntentVerifier struct {
	release    chan struct{}
	inFlight   int32
	peak       int32
	totalCalls int32
}

func (b *blockingIntentVerifier) Verify(ctx context.Context, _ llmproxy.IntentVerifyRequest) (*llmproxy.IntentVerdict, error) {
	atomic.AddInt32(&b.totalCalls, 1)
	cur := atomic.AddInt32(&b.inFlight, 1)
	for {
		peak := atomic.LoadInt32(&b.peak)
		if cur <= peak || atomic.CompareAndSwapInt32(&b.peak, peak, cur) {
			break
		}
	}
	defer atomic.AddInt32(&b.inFlight, -1)
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &llmproxy.IntentVerdict{Allow: true}, nil
}

// anthropicJSONWithParallelToolUses builds a response with N parallel
// Write tool_uses, each writing to a distinct path. The trigger-miss
// path (CandidateTasks → expected_tools matching) routes every one of
// them through runtimedecision.EvaluateAuthorization with intent
// verification on.
func anthropicJSONWithParallelToolUses(n int) []byte {
	type block struct {
		Type  string          `json:"type"`
		ID    string          `json:"id,omitempty"`
		Name  string          `json:"name,omitempty"`
		Input json.RawMessage `json:"input,omitempty"`
		Text  string          `json:"text,omitempty"`
	}
	blocks := []block{{Type: "text", Text: "sure"}}
	for i := 0; i < n; i++ {
		path := "/tmp/hello_" + jsonIndex(i) + ".py"
		input, _ := json.Marshal(map[string]string{
			"file_path": path,
			"content":   "print('hi')\n",
		})
		blocks = append(blocks, block{
			Type:  "tool_use",
			ID:    "toolu_" + jsonIndex(i),
			Name:  "Write",
			Input: input,
		})
	}
	body, _ := json.Marshal(map[string]any{
		"id":          "msg_1",
		"type":        "message",
		"role":        "assistant",
		"model":       "claude-haiku-4-5",
		"content":     blocks,
		"stop_reason": "tool_use",
	})
	return body
}

func jsonIndex(i int) string {
	// Two-digit zero-padded so tool_use IDs sort lexically. Enough room
	// for the tool-use counts the test exercises.
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

// TestPostprocess_ParallelToolUsesBatchIntentVerify proves that when a
// turn contains multiple parallel tool_uses that all route through
// EvaluateAuthorization, the intent verifier fan-out happens in
// parallel rather than serially. Without the batch pre-pass, the
// verifier sees one in-flight call at a time; with the pre-pass, all
// of them are in flight together.
func TestPostprocess_ParallelToolUsesBatchIntentVerify(t *testing.T) {
	const parallel = 4
	body := anthropicJSONWithParallelToolUses(parallel)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

	task := &store.Task{
		ID:                     "task-parallel",
		AgentID:                agentID,
		Purpose:                "Create hello.py variants",
		Status:                 "active",
		IntentVerificationMode: "strict",
		ExpectedUse:            "create hello.py files for testing",
		ExpectedTools:          json.RawMessage(`[{"tool_name":"Write","why":"create hello.py files"}]`),
	}

	verifier := &blockingIntentVerifier{release: make(chan struct{})}

	done := make(chan struct{})
	go func() {
		_ = Postprocess(req, body, "application/json", llmproxy.PostprocessConfig{
			ToolUseEvaluatorFactory: pipelineFactory,
			AgentContext: llmproxy.AgentContext{
				AgentUserID: userID,
				AgentID:     agentID,
			},
			AuthorizationContext: llmproxy.AuthorizationContext{
				CandidateTasks: []*store.Task{task},
				ToolRules:      []*store.RuntimePolicyRule{},
				EgressRules:    []*store.RuntimePolicyRule{},
				IntentVerifier: verifier,
				Posture:        runtimedecision.PostureEnforce,
			},
			ApprovalContext: llmproxy.ApprovalContext{
				PendingApprovals: llmproxy.NewMemoryPendingApprovalCache(time.Minute),
			},
			RewriteContext: llmproxy.RewriteContext{
				Inspector:    insp,
				RewriteOpts:  inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
				CallerNonces: llmproxy.NewMemoryCallerNonceCache(time.Minute),
				Store:        st,
			},
		})
		close(done)
	}()

	// Wait until all parallel verifier calls are parked simultaneously.
	// If batching regressed and the proxy serialized verifier calls, peak
	// would stay at 1 and this loop would time out.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&verifier.peak) >= parallel {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	peak := atomic.LoadInt32(&verifier.peak)
	close(verifier.release)
	<-done

	if peak < parallel {
		t.Fatalf("peak in-flight verifier calls = %d, want >= %d (verifier fan-out is serial?)", peak, parallel)
	}
	if got := atomic.LoadInt32(&verifier.totalCalls); got != parallel {
		t.Fatalf("verifier total calls = %d, want exactly %d (one per tool_use)", got, parallel)
	}
}

// anthropicJSONWithParallelCredentialedToolUses builds a response with
// N parallel WebFetch-style tool_uses, each carrying the autovault_github
// placeholder so the inspector classifies them as credentialed API calls.
// Each call targets a slightly different /repos/owner/repo-{i}/issues
// path so they're distinct tool_uses but all match the same (service,
// action) resolution.
func anthropicJSONWithParallelCredentialedToolUses(n int, placeholder string) []byte {
	type block struct {
		Type  string          `json:"type"`
		ID    string          `json:"id,omitempty"`
		Name  string          `json:"name,omitempty"`
		Input json.RawMessage `json:"input,omitempty"`
		Text  string          `json:"text,omitempty"`
	}
	blocks := []block{{Type: "text", Text: "sure"}}
	for i := 0; i < n; i++ {
		input, _ := json.Marshal(map[string]any{
			"url":     "https://api.github.com/repos/x/y-" + jsonIndex(i) + "/issues",
			"method":  "POST",
			"headers": map[string]string{"Authorization": "Bearer " + placeholder},
		})
		blocks = append(blocks, block{
			Type:  "tool_use",
			ID:    "toolu_" + jsonIndex(i),
			Name:  "WebFetch",
			Input: input,
		})
	}
	body, _ := json.Marshal(map[string]any{
		"id":          "msg_1",
		"type":        "message",
		"role":        "assistant",
		"model":       "claude-haiku-4-5",
		"content":     blocks,
		"stop_reason": "tool_use",
	})
	return body
}

// TestPostprocess_ParallelCredentialedToolUsesBatchIntentVerify is the
// credentialed-path counterpart to ParallelToolUsesBatchIntentVerify.
// It proves the TaskScopeEvaluator (credentialed) path also batches
// intent-verifier calls when a turn carries multiple parallel API
// tool_uses that all route through runtimedecision.EvaluateAuthorization.
func TestPostprocess_ParallelCredentialedToolUsesBatchIntentVerify(t *testing.T) {
	const parallel = 4
	placeholder := "autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	body := anthropicJSONWithParallelCredentialedToolUses(parallel, placeholder)
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	insp := inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	st, userID, agentID := seedPostprocessStore(t, placeholder)

	// Modern-path task: AuthorizedActions covers (github, create_issue)
	// with strict verification so the verifier fires for every sibling.
	task := &store.Task{
		ID:      "task-credentialed-parallel",
		AgentID: agentID,
		Purpose: "create issues for bug-triage",
		Status:  "active",
		AuthorizedActions: []store.TaskAction{
			{Service: "github", Action: "create_issue", Verification: "strict", ExpectedUse: "create bug-triage issues"},
		},
	}

	verifier := &blockingIntentVerifier{release: make(chan struct{})}

	done := make(chan struct{})
	go func() {
		_ = Postprocess(req, body, "application/json", llmproxy.PostprocessConfig{
			ToolUseEvaluatorFactory: pipelineFactory,
			AgentContext: llmproxy.AgentContext{
				AgentUserID: userID,
				AgentID:     agentID,
			},
			AuthorizationContext: llmproxy.AuthorizationContext{
				Catalog: stubCatalog{resolve: func(host, method, path string) (llmproxy.ResolvedAction, bool) {
					return llmproxy.ResolvedAction{ServiceID: "github", ActionID: "create_issue"}, true
				}},
				CandidateTasks: []*store.Task{task},
				ToolRules:      []*store.RuntimePolicyRule{},
				EgressRules:    []*store.RuntimePolicyRule{},
				IntentVerifier: verifier,
				Posture:        runtimedecision.PostureEnforce,
			},
			ApprovalContext: llmproxy.ApprovalContext{
				PendingApprovals: llmproxy.NewMemoryPendingApprovalCache(time.Minute),
			},
			RewriteContext: llmproxy.RewriteContext{
				Inspector:    insp,
				RewriteOpts:  inspector.DefaultRewriteOpts("https://proxy.example/api/proxy"),
				CallerNonces: llmproxy.NewMemoryCallerNonceCache(time.Minute),
				Store:        st,
			},
		})
		close(done)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&verifier.peak) >= parallel {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	peak := atomic.LoadInt32(&verifier.peak)
	close(verifier.release)
	<-done

	if peak < parallel {
		t.Fatalf("credentialed peak in-flight verifier calls = %d, want >= %d (verifier fan-out is serial?)", peak, parallel)
	}
	if got := atomic.LoadInt32(&verifier.totalCalls); got != parallel {
		t.Fatalf("credentialed verifier total calls = %d, want exactly %d (one per tool_use)", got, parallel)
	}
}
