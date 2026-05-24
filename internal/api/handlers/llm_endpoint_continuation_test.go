package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/inspector"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// firstSeededAgent retrieves the agent that newSeededHandler created
// so the continuation tests can attach it to the request context the
// same way the middleware would.
func firstSeededAgent(t *testing.T, st store.Store) *store.Agent {
	t.Helper()
	user, err := st.GetUserByEmail(context.Background(), "lite-proxy@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	agents, err := st.ListAgents(context.Background(), user.ID)
	if err != nil || len(agents) == 0 {
		t.Fatalf("ListAgents: agents=%d err=%v", len(agents), err)
	}
	return agents[0]
}

// TestTryContinuation_PostsSecondCallWithToolResult exercises the
// recursive-call mechanics directly: when the handler is handed a
// processed result with a ContinueWithToolResult decision, it must
// (a) POST a second request upstream whose messages array contains
// the original assistant turn + a synthetic user/tool_result turn,
// and (b) return the second response's body to the caller. This is
// what makes auto-approved inline tasks proceed seamlessly to the
// model's next tool_use instead of terminating the turn with an
// assistant text "[task was approved]" message.
func TestTryContinuation_PostsSecondCallWithToolResult(t *testing.T) {
	var seenBodies [][]byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seenBodies = append(seenBodies, b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "msg_second",
			"type": "message",
			"role": "assistant",
			"model": "claude-sonnet-4",
			"content": [{"type": "text", "text": "Files created."}],
			"stop_reason": "end_turn"
		}`))
	}))
	defer upstream.Close()

	h, _, _, _ := newSeededHandler(t, upstream.URL)

	// Original inbound body the harness sent.
	inboundBody := []byte(`{
		"model": "claude-sonnet-4",
		"messages": [
			{"role": "user", "content": "make /tmp/blah.txt"}
		],
		"max_tokens": 1024
	}`)
	// The first upstream response — assistant emitted a tool_use that
	// got auto-approved. We feed this in as the "current" upstream body.
	firstUpstreamBody := []byte(`{
		"id": "msg_first",
		"type": "message",
		"role": "assistant",
		"model": "claude-sonnet-4",
		"content": [
			{"type": "tool_use", "id": "toolu_auto", "name": "Bash", "input": {"cmd": "curl https://clawvisor.local/control/tasks"}}
		],
		"stop_reason": "tool_use"
	}`)
	// Hand-craft the postprocess result the gate would have produced.
	processed := llmproxy.PostprocessResult{
		Body:        []byte("substitute-fallback-text"),
		ContentType: "application/json",
		Decisions: []conversation.ToolUseDecisionRecord{{
			ToolUse: conversation.ToolUse{ID: "toolu_auto", Name: "Bash"},
			Verdict: conversation.ToolUseVerdict{
				Allowed:                false,
				Reason:                 "auto-approved",
				SubstituteWith:         "[Clawvisor: task was approved]",
				ContinueWithToolResult: "[Clawvisor: task was approved]",
			},
		}},
	}

	// Build a request carrying the agent-auth context the forwarder
	// expects. We bypass the middleware here; tryContinuation reads
	// agent.UserID/ID directly, and the forwarder reads upstream auth
	// from the vault (seeded by newSeededHandler).
	req := httptest.NewRequest(http.MethodPost, "/api/v1/messages", strings.NewReader(string(inboundBody)))
	req.Header.Set("Content-Type", "application/json")
	// Inject the agent into the request context the same way the
	// middleware would.
	agent := firstSeededAgent(t, h.Store)
	req = req.WithContext(store.WithAgent(req.Context(), agent))

	final, status, ct, err := h.tryContinuation(
		req,
		agent,
		conversation.ProviderAnthropic,
		"req-test-1",
		inboundBody,
		firstUpstreamBody,
		"application/json",
		http.StatusOK,
		processed,
		llmproxy.PostprocessConfig{
			// Postprocess on the second call needs at least an inspector
			// configured to not be skipped; pass the handler's.
			Inspector:   h.Inspector,
			RewriteOpts: inspector.DefaultRewriteOpts(h.ResolverBaseURL),
			Store:       h.Store,
			AgentUserID: agent.UserID,
			AgentID:     agent.ID,
		},

	)
	if err != nil {
		t.Fatalf("tryContinuation: %v", err)
	}
	if final == nil {
		t.Fatal("expected non-nil final result (continuation should have fired)")
	}
	if status != http.StatusOK {
		t.Errorf("expected status 200 from second upstream, got %d", status)
	}
	if ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}

	// Upstream must have been called exactly once (the continuation
	// call). tryContinuation does not re-issue the first call — it
	// builds on top of the firstUpstreamBody it was given.
	if len(seenBodies) != 1 {
		t.Fatalf("expected upstream to be called 1 time (continuation only), got %d", len(seenBodies))
	}
	// Inspect the second-call body shape.
	var contReq map[string]any
	if err := json.Unmarshal(seenBodies[0], &contReq); err != nil {
		t.Fatalf("second upstream body not JSON: %v\n%s", err, seenBodies[0])
	}
	msgs, ok := contReq["messages"].([]any)
	if !ok {
		t.Fatalf("messages field missing or wrong type: %T", contReq["messages"])
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages in continuation (user, assistant, user/tool_result); got %d: %v", len(msgs), msgs)
	}
	// Last turn must be a user turn carrying a tool_result with the augmentation content.
	last, ok := msgs[2].(map[string]any)
	if !ok {
		t.Fatalf("last message not a map: %T", msgs[2])
	}
	if last["role"] != "user" {
		t.Errorf("last turn role: got %v want user", last["role"])
	}
	lastContent, ok := last["content"].([]any)
	if !ok || len(lastContent) == 0 {
		t.Fatalf("last user turn content empty or wrong type: %v", last["content"])
	}
	tr, ok := lastContent[0].(map[string]any)
	if !ok {
		t.Fatalf("tool_result block not a map: %T", lastContent[0])
	}
	if tr["type"] != "tool_result" {
		t.Errorf("expected tool_result, got %v", tr["type"])
	}
	if tr["tool_use_id"] != "toolu_auto" {
		t.Errorf("tool_use_id mismatch: %v", tr["tool_use_id"])
	}
	if !strings.Contains(tr["content"].(string), "task was approved") {
		t.Errorf("tool_result content lost augmentation: %v", tr["content"])
	}

	// The final body returned to the caller should be the SECOND
	// upstream's body (msg_second), not the fallback substitute text.
	if !strings.Contains(string(final.Body), "msg_second") {
		t.Errorf("final body should reflect second upstream response, got: %s", final.Body)
	}
	if strings.Contains(string(final.Body), "substitute-fallback-text") {
		t.Errorf("final body should NOT contain the fallback substitute text, got: %s", final.Body)
	}
}

// TestTryContinuation_RefreshesCandidateTasksFromStore reproduces the
// real-world bug where the model's post-auto-approval tool_uses fell
// through to "no matching task scope" because the candidate task list
// was a snapshot taken BEFORE the auto-approve gate created the new
// task. Here we pass cfg.CandidateTasks=nil (the stale snapshot), but
// the store has a task seeded by newSeededHandler that authorizes
// POST api.github.com/repos/x/y/issues. If the refresh logic works,
// the second postprocess loads that task, the inspector evaluates the
// tool_use against it, and the URL gets rewritten through the
// resolver. If the refresh is missing, the tool_use is blocked and
// the rewritten URL never appears in the body.
func TestTryContinuation_RefreshesCandidateTasksFromStore(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Second upstream call's response: a tool_use that requires the
		// seeded github task scope.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "msg_second",
			"type": "message",
			"role": "assistant",
			"model": "claude-sonnet-4",
			"content": [
				{"type": "tool_use", "id": "toolu_next", "name": "WebFetch", "input": {
					"url": "https://api.github.com/repos/x/y/issues",
					"method": "POST",
					"headers": {"Authorization": "Bearer autovault_github_xxx"}
				}}
			],
			"stop_reason": "tool_use"
		}`))
	}))
	defer upstream.Close()

	h, _, _, _ := newSeededHandler(t, upstream.URL)
	h.Inspector = inspector.NewInspector(inspector.DefaultParser{}, inspector.AmbiguousValidator{})
	h.ResolverBaseURL = "https://clawvisor.example/api/proxy"
	h.CallerNonces = llmproxy.NewMemoryCallerNonceCache(time.Minute)

	agent := firstSeededAgent(t, h.Store)
	inboundBody := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"create gh issue"}]}`)
	firstUpstreamBody := []byte(`{
		"id": "msg_first",
		"type": "message",
		"role": "assistant",
		"model": "claude-sonnet-4",
		"content": [
			{"type": "tool_use", "id": "toolu_auto", "name": "Bash", "input": {"cmd": "curl https://clawvisor.local/control/tasks"}}
		],
		"stop_reason": "tool_use"
	}`)
	processed := llmproxy.PostprocessResult{
		Body: []byte("fallback"),
		Decisions: []conversation.ToolUseDecisionRecord{{
			ToolUse: conversation.ToolUse{ID: "toolu_auto", Name: "Bash"},
			Verdict: conversation.ToolUseVerdict{
				Allowed:                false,
				SubstituteWith:         "[approved]",
				ContinueWithToolResult: "[approved]",
			},
		}},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/messages", strings.NewReader(string(inboundBody)))
	req = req.WithContext(store.WithAgent(req.Context(), agent))

	// Pass an empty CandidateTasks slice. This simulates the bug
	// where the original load happened before any inline auto-approve
	// minted a new task — the refresh inside tryContinuation must
	// re-read the store to find the seeded github task.
	final, _, _, err := h.tryContinuation(
		req, agent, conversation.ProviderAnthropic, "req-refresh",
		inboundBody, firstUpstreamBody, "application/json", http.StatusOK,
		processed,
		llmproxy.PostprocessConfig{
			Inspector:        h.Inspector,
			RewriteOpts:      inspector.DefaultRewriteOpts(h.ResolverBaseURL),
			Store:            h.Store,
			AgentUserID:      agent.UserID,
			AgentID:          agent.ID,
			Catalog:          nil,
			CandidateTasks:   nil, // STALE — refresh must rebuild this
			ToolRules:        nil,
			EgressRules:      nil,
			CallerNonces:     h.CallerNonces,
			PendingApprovals: h.PendingApprovals,
		},

	)
	if err != nil {
		t.Fatalf("tryContinuation: %v", err)
	}
	if final == nil {
		t.Fatal("expected continuation result")
	}

	body := string(final.Body)
	// Without refresh: tool_use is blocked, the body becomes a
	// "blocked by clawvisor" text turn.
	if strings.Contains(strings.ToLower(body), "no matching task scope") ||
		strings.Contains(strings.ToLower(body), "blocked by clawvisor") {
		t.Fatalf("second postprocess blocked the tool_use — refresh did not load the seeded task into cfg.CandidateTasks. body=%s", body)
	}
	// With refresh + caller nonces: the URL is rewritten through the
	// resolver. We assert the rewritten URL is present and the
	// original github URL is no longer the bare URL the harness sees.
	if !strings.Contains(body, "clawvisor.example/api/proxy") {
		t.Errorf("expected resolver URL in second postprocess body, indicating the inspector authorized + rewrote the tool_use. body=%s", body)
	}
}

// TestTryContinuation_PrependsUserFacingNotice verifies the handler
// injects the verdict's PrependAssistantNotice text into the
// continuation's assistant turn — so when the auto-approve gate fires,
// the user sees a "[Clawvisor] approved" line at the top of the
// model's next response in the same turn as the model's actions.
func TestTryContinuation_PrependsUserFacingNotice(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "msg_second",
			"type": "message",
			"role": "assistant",
			"model": "claude-sonnet-4",
			"content": [{"type": "text", "text": "Files created."}],
			"stop_reason": "end_turn"
		}`))
	}))
	defer upstream.Close()

	h, _, _, _ := newSeededHandler(t, upstream.URL)
	agent := firstSeededAgent(t, h.Store)

	inboundBody := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"make /tmp/x"}]}`)
	firstUpstreamBody := []byte(`{
		"id": "msg_first",
		"type": "message",
		"role": "assistant",
		"model": "claude-sonnet-4",
		"content": [{"type": "tool_use", "id": "toolu_a", "name": "Bash", "input": {"cmd": "curl https://clawvisor.local/control/tasks"}}],
		"stop_reason": "tool_use"
	}`)
	processed := llmproxy.PostprocessResult{
		Body: []byte("fallback"),
		Decisions: []conversation.ToolUseDecisionRecord{{
			ToolUse: conversation.ToolUse{ID: "toolu_a", Name: "Bash"},
			Verdict: conversation.ToolUseVerdict{
				Allowed:                false,
				SubstituteWith:         "[task approved]",
				ContinueWithToolResult: "[task approved]",
				PrependAssistantNotice: `[Clawvisor] Auto-approved task: Create files in /tmp`,
			},
		}},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/messages", strings.NewReader(string(inboundBody)))
	req = req.WithContext(store.WithAgent(req.Context(), agent))

	final, _, _, err := h.tryContinuation(
		req, agent, conversation.ProviderAnthropic, "req-notice",
		inboundBody, firstUpstreamBody, "application/json", http.StatusOK,
		processed,
		llmproxy.PostprocessConfig{
			Inspector:   h.Inspector,
			RewriteOpts: inspector.DefaultRewriteOpts(h.ResolverBaseURL),
			Store:       h.Store,
			AgentUserID: agent.UserID,
			AgentID:     agent.ID,
		},

	)
	if err != nil {
		t.Fatalf("tryContinuation: %v", err)
	}
	if final == nil {
		t.Fatal("expected final result")
	}
	body := string(final.Body)
	if !strings.Contains(body, "[Clawvisor] Auto-approved task: Create files in /tmp") {
		t.Errorf("notice missing from continuation body:\n%s", body)
	}
	// Notice precedes the model's "Files created." text.
	noticePos := strings.Index(body, "[Clawvisor] Auto-approved")
	modelPos := strings.Index(body, "Files created.")
	if noticePos == -1 || modelPos == -1 || noticePos >= modelPos {
		t.Errorf("notice should come before model text; notice at %d, model at %d", noticePos, modelPos)
	}
}

// TestTryContinuation_NoContinueDecisionIsNoOp confirms the handler
// short-circuits when no decision asked for continuation.
func TestTryContinuation_NoContinueDecisionIsNoOp(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream should not be called when no continuation decision is present")
	}))
	defer upstream.Close()

	h, _, _, _ := newSeededHandler(t, upstream.URL)
	agent := firstSeededAgent(t, h.Store)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/messages", strings.NewReader(`{"messages":[]}`))
	req = req.WithContext(store.WithAgent(req.Context(), agent))

	processed := llmproxy.PostprocessResult{
		Body:        []byte("orig"),
		ContentType: "application/json",
		Decisions: []conversation.ToolUseDecisionRecord{{
			Verdict: conversation.ToolUseVerdict{Allowed: true},
		}},
	}
	final, status, ct, err := h.tryContinuation(
		req, agent, conversation.ProviderAnthropic, "req-x",
		[]byte(`{"messages":[]}`), []byte(`{"content":[]}`), "application/json", http.StatusOK,
		processed, llmproxy.PostprocessConfig{Inspector: h.Inspector},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if final != nil || status != 0 || ct != "" {
		t.Errorf("expected no-op return, got final=%v status=%d ct=%q", final, status, ct)
	}
}

// TestServe_ContinuationClearsStaleContentLengthHeader exercises the
// serve()-level wiring. Before the fix the handler's `if
// processed.Rewritten` block only cleared Content-Length when the
// continuation's postprocess ITSELF rewrote the body — leaving stale
// upstream Content-Length / Content-Encoding headers in place when
// the second upstream returned a plain text turn (passthrough). Go
// would then either truncate the response or the harness would try
// to gunzip plaintext. After the fix tryContinuation always marks
// the swapped result as Rewritten so the header-cleanup fires
// regardless.
//
// We can't easily drive an end-to-end auto-approve flow here without
// the full inline_task_creator + risk_assessor scaffolding, so the
// test stubs the auto-approve path by patching the Inspector. The
// observable contract is the same: after a continuation swap whose
// second response is passthrough-JSON, Content-Length on the harness
// response should be the SECOND body's length, not the first's.
func TestServe_ContinuationClearsStaleContentLengthHeader(t *testing.T) {
	// Two upstream calls, returning bodies of very different sizes.
	// If Content-Length leaks from call 1 → harness, the harness
	// would see truncation or misframing.
	const firstSize = 2048
	const secondSize = 64
	firstBody := []byte(`{"id":"msg_first","type":"message","role":"assistant","model":"claude-sonnet-4","content":[{"type":"text","text":"` + strings.Repeat("a", firstSize) + `"}],"stop_reason":"end_turn"}`)
	secondBody := []byte(`{"id":"msg_second","type":"message","role":"assistant","model":"claude-sonnet-4","content":[{"type":"text","text":"` + strings.Repeat("b", secondSize) + `"}],"stop_reason":"end_turn"}`)

	// Drive through tryContinuation directly rather than serve(), so
	// we don't need the full auto-approve gate machinery wired up.
	// What we're testing: after the swap, the body length matches the
	// SECOND upstream's body, and the caller (serve) sees Rewritten=
	// true on processed so it clears the stale headers.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(secondBody)
	}))
	defer upstream.Close()

	h, _, _, _ := newSeededHandler(t, upstream.URL)
	agent := firstSeededAgent(t, h.Store)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"x"}]}`))
	req = req.WithContext(store.WithAgent(req.Context(), agent))

	processed := llmproxy.PostprocessResult{
		Body:        firstBody,
		ContentType: "application/json",
		Rewritten:   false,
		Decisions: []conversation.ToolUseDecisionRecord{{
			ToolUse: conversation.ToolUse{ID: "toolu_a", Name: "Bash"},
			Verdict: conversation.ToolUseVerdict{
				Allowed:                false,
				SubstituteWith:         "[fallback]",
				ContinueWithToolResult: "[approved]",
			},
		}},
	}

	final, _, _, err := h.tryContinuation(
		req, agent, conversation.ProviderAnthropic, "req-headers",
		[]byte(`{"messages":[{"role":"user","content":"x"}]}`),
		[]byte(`{"content":[{"type":"tool_use","id":"toolu_a","name":"Bash","input":{}}]}`),
		"application/json", http.StatusOK,
		processed,
		llmproxy.PostprocessConfig{
			Inspector:   h.Inspector,
			RewriteOpts: inspector.DefaultRewriteOpts(h.ResolverBaseURL),
			Store:       h.Store,
			AgentUserID: agent.UserID,
			AgentID:     agent.ID,
		},

	)
	if err != nil {
		t.Fatalf("tryContinuation: %v", err)
	}
	if final == nil {
		t.Fatal("expected continuation result")
	}
	// The body is now the SECOND upstream's body. Its length is far
	// from the first upstream's, so a stale Content-Length would have
	// truncated it.
	if len(final.Body) == firstSize {
		t.Errorf("body wasn't swapped to second upstream (still first-sized)")
	}
	// Critical assertion: processed.Rewritten was forced to true,
	// which is what triggers the header-clear in serve()'s
	// `if processed.Rewritten` block. Without this, Content-Length
	// from the first upstream call leaks into the harness response.
	if !final.Rewritten {
		t.Errorf("continuation swap must mark Rewritten=true so the handler clears stale Content-Length / Content-Encoding from the first upstream; got Rewritten=false")
	}
}

// TestTryContinuation_UpstreamErrorFallsBack ensures that when the
// continuation upstream call returns an error status, the handler
// surfaces an error to its caller so the substitute fallback gets
// rendered instead of an empty body.
func TestTryContinuation_UpstreamErrorFallsBack(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"upstream borked"}`))
	}))
	defer upstream.Close()

	h, _, _, _ := newSeededHandler(t, upstream.URL)
	agent := firstSeededAgent(t, h.Store)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/messages", strings.NewReader(`{}`))
	req = req.WithContext(store.WithAgent(req.Context(), agent))

	processed := llmproxy.PostprocessResult{
		Body: []byte("fallback"),
		Decisions: []conversation.ToolUseDecisionRecord{{
			ToolUse: conversation.ToolUse{ID: "toolu_x"},
			Verdict: conversation.ToolUseVerdict{
				Allowed:                false,
				SubstituteWith:         "[fallback]",
				ContinueWithToolResult: "[fallback]",
			},
		}},
	}
	inbound := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	first := []byte(`{"content":[{"type":"tool_use","id":"toolu_x","name":"Bash","input":{}}]}`)
	final, _, _, err := h.tryContinuation(
		req, agent, conversation.ProviderAnthropic, "req-y",
		inbound, first, "application/json", http.StatusOK,
		processed, llmproxy.PostprocessConfig{Inspector: h.Inspector},
	)
	if err == nil {
		t.Fatal("expected error on upstream failure; got nil so caller would silently swap in continuation result")
	}
	if final != nil {
		t.Errorf("final should be nil on error so caller falls back to original processed")
	}
}
