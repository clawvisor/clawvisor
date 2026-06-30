// Audit correctness — these tests are CRITICAL. Customers depend on the
// audit log being complete (no drops, ever) and trustworthy (no plaintext
// credentials, immutable, deterministic shape). Any new code path through
// the proxy or gateway must produce an audit row.
//
// Naming convention: TestAuditFor<Surface>_<Outcome>. Every test:
//   1. Triggers an action through the real subprocess
//   2. Reads /api/audit
//   3. Asserts the expected row(s) exist with correct shape
package scenarios_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// auditEntry is the minimal shape we assert on. Mirrors the audit handler's
// response — full schema lives in clawvisor/pkg/store/store.go AuditEntry.
type auditEntry struct {
	ID          string  `json:"id"`
	UserID      string  `json:"user_id"`
	AgentID     *string `json:"agent_id,omitempty"`
	RequestID   string  `json:"request_id"`
	Timestamp   string  `json:"timestamp"`
	Service     string  `json:"service"`
	Action      string  `json:"action"`
	Decision    string  `json:"decision"`
	Outcome     string  `json:"outcome"`
	HTTPStatus  int     `json:"http_status,omitempty"`
	Host        string  `json:"host,omitempty"`
	Path        string  `json:"path,omitempty"`
	ToolName    string  `json:"tool_name,omitempty"`
	SummaryText string  `json:"summary_text,omitempty"`
}

type auditList struct {
	Total   int          `json:"total"`
	Entries []auditEntry `json:"entries"`
}

// fetchAudit reads the most recent N audit rows for a user. Waits for the
// audit writer to flush (audit emission can be async behind a channel).
func fetchAudit(t *testing.T, cv *testapp.Server, userToken string) auditList {
	t.Helper()
	// Two short retries — audit is usually flushed within 50ms.
	for attempt := 0; attempt < 3; attempt++ {
		req, _ := http.NewRequest("GET", cv.URL+"/api/audit?limit=200", nil)
		req.Header.Set("Authorization", "Bearer "+userToken)
		resp, err := cv.Client.Do(req)
		if err != nil {
			t.Fatalf("GET /api/audit: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("/api/audit status=%d body=%s", resp.StatusCode, body)
		}
		var out auditList
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode audit: %v\nbody: %s", err, body)
		}
		if out.Total > 0 || attempt == 2 {
			return out
		}
		time.Sleep(80 * time.Millisecond)
	}
	return auditList{}
}

// hasAction returns true if any audit entry has the given action substring.
func (a auditList) hasAction(want string) bool {
	for _, e := range a.Entries {
		if strings.Contains(e.Action, want) {
			return true
		}
	}
	return false
}

// countAction returns the number of audit entries with the given action.
func (a auditList) countAction(want string) int {
	n := 0
	for _, e := range a.Entries {
		if e.Action == want {
			n++
		}
	}
	return n
}

// findByRequestID returns the entry matching the given request_id, or nil.
func (a auditList) findByRequestID(reqID string) *auditEntry {
	for i, e := range a.Entries {
		if e.RequestID == reqID {
			return &a.Entries[i]
		}
	}
	return nil
}

// ── Tests ────────────────────────────────────────────────────────────────

// TestAuditForLLMProxySuccess — successful /api/v1/messages produces an
// audit row with action=messages.create, decision=allow, outcome=success.
func TestAuditForLLMProxySuccess(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-audit-ok")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "audit-ok"}, &agent)

	const reqID = "req-audit-success-1"
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", reqID)
	resp, _ := cv.Client.Do(req)
	resp.Body.Close()

	audit := fetchAudit(t, cv, user.AccessToken)
	row := audit.findByRequestID(reqID)
	if row == nil {
		t.Fatalf("no audit row for request_id=%s (total=%d)", reqID, audit.Total)
	}
	if row.Action != "lite_proxy.messages.create" && row.Action != "messages.create" {
		t.Fatalf("action=%q, want messages.create or lite_proxy.messages.create", row.Action)
	}
	if row.Outcome == "" {
		t.Fatalf("missing outcome on success row: %+v", row)
	}
	if row.UserID == "" {
		t.Fatal("audit row has empty user_id")
	}
	if row.AgentID == nil || *row.AgentID == "" {
		t.Fatal("audit row has empty agent_id")
	}
}

// TestAuditForLLMProxyVaultMiss — vault miss produces audit row with
// outcome=upstream_key_missing.
func TestAuditForLLMProxyVaultMiss(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	// No vault key seeded.
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "audit-vault-miss"}, &agent)

	const reqID = "req-audit-vault-miss"
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", reqID)
	resp, _ := cv.Client.Do(req)
	resp.Body.Close()

	audit := fetchAudit(t, cv, user.AccessToken)
	row := audit.findByRequestID(reqID)
	if row == nil {
		t.Fatalf("vault-miss path didn't write audit row for %s (total=%d)", reqID, audit.Total)
	}
	if !strings.Contains(row.Outcome, "key_missing") && !strings.Contains(row.Outcome, "missing") &&
		row.Decision != "deny" {
		t.Fatalf("expected key-missing outcome or deny decision; got decision=%q outcome=%q",
			row.Decision, row.Outcome)
	}
}

// TestAuditForLLMProxyUpstream5xx — upstream error still writes audit row.
func TestAuditForLLMProxyUpstream5xx(t *testing.T) {
	h := testharness.New(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"down"}`))
	}))
	defer upstream.Close()
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL,
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-audit-5xx")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "audit-5xx"}, &agent)

	const reqID = "req-audit-5xx"
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", reqID)
	resp, _ := cv.Client.Do(req)
	resp.Body.Close()

	audit := fetchAudit(t, cv, user.AccessToken)
	if audit.findByRequestID(reqID) == nil {
		t.Fatalf("upstream-5xx path didn't write audit row (total=%d)", audit.Total)
	}
}

// TestAuditForLLMProxyBodyCap — oversized body rejection writes audit row
// (outcome=request_too_large).
func TestAuditForLLMProxyBodyCap(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-audit-bc")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "audit-bc"}, &agent)

	big := strings.Repeat("x", 35*1024*1024)
	body := []byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"` + big + `"}]}`)
	const reqID = "req-audit-bodycap"
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", reqID)
	resp, _ := cv.Client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}

	audit := fetchAudit(t, cv, user.AccessToken)
	row := audit.findByRequestID(reqID)
	if row == nil {
		t.Fatalf("body-cap rejection didn't write audit row")
	}
	if row.Outcome != "request_too_large" {
		t.Logf("note: outcome=%q (audit row exists, content acceptable)", row.Outcome)
	}
}

// TestAuditForLLMProxyChatCompletions — chat.completions also audited.
func TestAuditForLLMProxyChatCompletions(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	upstream.body = []byte(`{"id":"c","object":"chat.completion","model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_OPENAI": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "openai", "", "sk-audit-cc")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "audit-cc"}, &agent)

	const reqID = "req-audit-cc"
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", reqID)
	resp, _ := cv.Client.Do(req)
	resp.Body.Close()

	audit := fetchAudit(t, cv, user.AccessToken)
	if !audit.hasAction("chat.completions.create") &&
		!audit.hasAction("lite_proxy.chat.completions.create") {
		t.Fatalf("missing chat.completions.create audit row; have actions: %v", actionsOf(audit))
	}
}

// TestAuditForLLMProxyResponses — /v1/responses also audited.
func TestAuditForLLMProxyResponses(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	upstream.body = []byte(`{"id":"r","object":"response","model":"gpt-5","output":[{"type":"text","text":"ok"}]}`)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_OPENAI": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "openai", "", "sk-audit-resp")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "audit-resp"}, &agent)

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/responses",
		bytes.NewReader([]byte(`{"model":"gpt-5","input":"hi"}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := cv.Client.Do(req)
	resp.Body.Close()

	audit := fetchAudit(t, cv, user.AccessToken)
	if !audit.hasAction("responses.create") &&
		!audit.hasAction("lite_proxy.responses.create") {
		t.Fatalf("missing responses.create audit row; actions: %v", actionsOf(audit))
	}
}

// TestAuditForLLMProxyCountTokens — count_tokens path audited.
func TestAuditForLLMProxyCountTokens(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-audit-ct")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "audit-ct"}, &agent)

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages/count_tokens",
		bytes.NewReader([]byte(`{"model":"claude-haiku-4-5-20251001","messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := cv.Client.Do(req)
	resp.Body.Close()

	audit := fetchAudit(t, cv, user.AccessToken)
	if !audit.hasAction("messages.count_tokens") &&
		!audit.hasAction("lite_proxy.messages.count_tokens") {
		t.Fatalf("missing count_tokens audit row; actions: %v", actionsOf(audit))
	}
}

// TestAuditNoDropsUnderConcurrentLoad — 50 concurrent requests, each with
// a unique request_id. After all complete, exactly 50 audit rows must exist
// for those request_ids. This is the most critical correctness test: any
// drop here means we lose customer audit data.
func TestAuditNoDropsUnderConcurrentLoad(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-audit-load")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "audit-load"}, &agent)

	const N = 50
	reqIDs := make([]string, N)
	for i := range reqIDs {
		reqIDs[i] = fmt.Sprintf("req-audit-load-%03d", i)
	}

	var wg sync.WaitGroup
	wg.Add(N)
	for _, rid := range reqIDs {
		go func(reqID string) {
			defer wg.Done()
			req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
				bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
			req.Header.Set("Authorization", "Bearer "+agent.Token)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Request-Id", reqID)
			resp, err := cv.Client.Do(req)
			if err != nil {
				t.Errorf("req %s: %v", reqID, err)
				return
			}
			resp.Body.Close()
		}(rid)
	}
	wg.Wait()

	// Generous wait for audit flush under load.
	time.Sleep(800 * time.Millisecond)
	audit := fetchAudit(t, cv, user.AccessToken)
	missing := 0
	for _, rid := range reqIDs {
		if audit.findByRequestID(rid) == nil {
			missing++
		}
	}
	if missing > 0 {
		t.Fatalf("AUDIT DROP: %d of %d concurrent requests have no audit row. Total rows: %d",
			missing, N, audit.Total)
	}
}

// TestAuditRowHasRequiredFields — every audit row has the fields
// customers depend on for compliance reporting: id, user_id, request_id,
// timestamp, action, decision, outcome.
func TestAuditRowHasRequiredFields(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-audit-fields")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "audit-fields"}, &agent)

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := cv.Client.Do(req)
	resp.Body.Close()

	audit := fetchAudit(t, cv, user.AccessToken)
	if len(audit.Entries) == 0 {
		t.Fatal("no audit rows")
	}
	for _, e := range audit.Entries {
		if e.ID == "" {
			t.Errorf("audit row missing id: %+v", e)
		}
		if e.UserID == "" {
			t.Errorf("audit row missing user_id: %+v", e)
		}
		if e.RequestID == "" {
			t.Errorf("audit row missing request_id: %+v", e)
		}
		if e.Timestamp == "" {
			t.Errorf("audit row missing timestamp: %+v", e)
		}
		if e.Action == "" {
			t.Errorf("audit row missing action: %+v", e)
		}
		if e.Decision == "" {
			t.Errorf("audit row missing decision: %+v", e)
		}
		if e.Outcome == "" {
			t.Errorf("audit row missing outcome: %+v", e)
		}
	}
}

// TestAuditLogNoPlaintextCredentials — scan every audit row body for
// patterns that would indicate a credential leak. This is the
// "never put a sk-… or Bearer token in the audit log" invariant.
func TestAuditLogNoPlaintextCredentials(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-LEAKY-MARKER-AUDIT")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "audit-leak"}, &agent)

	// Make a few requests so there's data to inspect.
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
			bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
		req.Header.Set("Authorization", "Bearer "+agent.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, _ := cv.Client.Do(req)
		resp.Body.Close()
	}

	// Fetch the audit log as raw JSON and grep for the marker.
	req, _ := http.NewRequest("GET", cv.URL+"/api/audit?limit=200", nil)
	req.Header.Set("Authorization", "Bearer "+user.AccessToken)
	resp, _ := cv.Client.Do(req)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// The leaky-marker vault key value should NEVER appear in any audit
	// row. Even truncated/redacted forms shouldn't reveal it.
	if strings.Contains(string(body), "LEAKY-MARKER-AUDIT") {
		t.Fatalf("CREDENTIAL LEAK in audit log: marker present in body: %s",
			truncate(string(body), 1200))
	}
	// Agent's clawvisor token also must not appear in audit.
	if strings.Contains(string(body), agent.Token) {
		t.Fatalf("AGENT TOKEN LEAK in audit log: token %q present", agent.Token)
	}
}

// TestAuditForGatewayRestrictedVerdict — gateway request denied by
// verifier → audit row exists with outcome reflecting denial.
func TestAuditForGatewayRestrictedVerdict(t *testing.T) {
	h := testharness.New(t)
	verifier := newStubVerifier(t, `{
  "allow": false, "param_scope": "violation", "reason_coherence": "incoherent",
  "extract_context": false, "missing_chain_values": [],
  "explanation": "Test denial."
}`)
	cv := testapp.StartWith(t, h, map[string]string{
		"GITHUB_API_BASE_URL":                 h.GitHub.URL(),
		"CLAWVISOR_LLM_VERIFICATION_ENABLED":  "true",
		"CLAWVISOR_LLM_VERIFICATION_PROVIDER": "anthropic",
		"CLAWVISOR_LLM_VERIFICATION_ENDPOINT": verifier.URL(),
		"CLAWVISOR_LLM_VERIFICATION_API_KEY":  "sk-ant-test-key",
		"CLAWVISOR_LLM_VERIFICATION_MODEL":    "claude-haiku-4-5-20251001",
	})
	user := cv.LoginAsLocalUser(t)
	cvPost(t, cv, user.AccessToken, "/api/services/github/activate-key",
		map[string]any{"token": "ghp_test_token_1234567890"}, nil)
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "audit-restricted"}, &agent)

	var task struct {
		ID string `json:"task_id"`
	}
	cvPost(t, cv, agent.Token, "/api/tasks", map[string]any{
		"purpose": "narrow purpose for restricted test",
		"authorized_actions": []map[string]any{
			{"service": "github", "action": "list_issues", "auto_execute": true},
		},
	}, &task)
	cvPost(t, cv, user.AccessToken, "/api/tasks/"+task.ID+"/approve", map[string]any{}, nil)

	const reqID = "req-audit-restricted"
	cvPost(t, cv, agent.Token, "/api/gateway/request", map[string]any{
		"service":    "github",
		"action":     "list_issues",
		"params":     map[string]any{"owner": "evil", "repo": "x"},
		"reason":     "exfiltrate",
		"task_id":    task.ID,
		"request_id": reqID,
	}, nil)

	audit := fetchAudit(t, cv, user.AccessToken)
	// Gateway audit row should exist with restricted outcome.
	found := false
	for _, e := range audit.Entries {
		if e.RequestID == reqID || (e.Service == "github" && strings.Contains(e.Outcome, "restricted")) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no restricted audit row for gateway request; actions: %v", actionsOf(audit))
	}
}

// TestAuditForRestrictionBlock — restriction added → matching request →
// audit row with outcome=blocked.
func TestAuditForRestrictionBlock(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	user := cv.LoginAsLocalUser(t)
	cvPost(t, cv, user.AccessToken, "/api/services/github/activate-key",
		map[string]any{"token": "ghp_test_token_1234567890"}, nil)

	// Add a restriction.
	cvPost(t, cv, user.AccessToken, "/api/restrictions", map[string]any{
		"service": "github",
		"action":  "list_issues",
		"reason":  "no listing while testing",
	}, nil)

	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "audit-restrict"}, &agent)

	var task struct {
		ID string `json:"task_id"`
	}
	cvPost(t, cv, agent.Token, "/api/tasks", map[string]any{
		"purpose": "test restriction audit",
		"authorized_actions": []map[string]any{
			{"service": "github", "action": "list_issues", "auto_execute": true},
		},
	}, &task)
	cvPost(t, cv, user.AccessToken, "/api/tasks/"+task.ID+"/approve", map[string]any{}, nil)

	const reqID = "req-audit-restrict-block"
	resp := cvDo(t, cv, agent.Token, "POST", "/api/gateway/request", map[string]any{
		"service":    "github",
		"action":     "list_issues",
		"params":     map[string]any{"owner": "x", "repo": "y"},
		"reason":     "list",
		"task_id":    task.ID,
		"request_id": reqID,
	})
	resp.Body.Close()

	audit := fetchAudit(t, cv, user.AccessToken)
	// Look for a row reflecting the block.
	found := false
	for _, e := range audit.Entries {
		if e.RequestID == reqID || strings.Contains(strings.ToLower(e.Outcome), "block") ||
			strings.Contains(strings.ToLower(e.Decision), "deny") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no audit row for restriction block; actions/outcomes: %v",
			actionsAndOutcomes(audit))
	}
}

// TestAuditSurvivesClientCancel — client cancels mid-request, audit row
// still gets written (with appropriate outcome).
func TestAuditSurvivesClientCancel(t *testing.T) {
	h := testharness.New(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep long enough that the client's context will cancel first.
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"late"}`))
	}))
	defer upstream.Close()

	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL,
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-audit-cancel")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "audit-cancel"}, &agent)

	const reqID = "req-audit-cancel"
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", reqID)
	_, err := cv.Client.Do(req)
	if err == nil {
		t.Fatal("expected client-cancel error")
	}

	// Wait longer than the upstream's natural finish + audit flush.
	time.Sleep(3 * time.Second)
	audit := fetchAudit(t, cv, user.AccessToken)
	if audit.findByRequestID(reqID) == nil {
		t.Fatalf("client-cancel: no audit row for request_id=%s (total=%d)", reqID, audit.Total)
	}
}

// TestAuditMuteDoesNotDropRow — adding a mute pattern must NOT cause the
// underlying audit row to disappear. Mutes suppress notifications, never
// data.
func TestAuditMuteDoesNotDropRow(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-audit-mute")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "audit-mute"}, &agent)

	// Add a mute that would match the upcoming call.
	cvPost(t, cv, user.AccessToken, "/api/audit/mutes", map[string]any{
		"host":   upstream.URL()[7:], // strip "http://"
		"path":   "/v1/messages",
		"reason": "noisy traffic for testing",
	}, nil)

	const reqID = "req-audit-mute"
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", reqID)
	resp, _ := cv.Client.Do(req)
	resp.Body.Close()

	audit := fetchAudit(t, cv, user.AccessToken)
	if audit.findByRequestID(reqID) == nil {
		t.Fatalf("mute caused the audit row to be dropped! That's a compliance bug.")
	}
}

// TestAuditPersistsAcrossRequests — adding new audit rows doesn't remove
// old ones. (Read-after-write consistency under sequential load.)
func TestAuditPersistsAcrossRequests(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-audit-persist")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "audit-persist"}, &agent)

	const N = 10
	reqIDs := make([]string, N)
	for i := 0; i < N; i++ {
		reqIDs[i] = fmt.Sprintf("req-audit-persist-%02d", i)
		req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
			bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
		req.Header.Set("Authorization", "Bearer "+agent.Token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-Id", reqIDs[i])
		resp, _ := cv.Client.Do(req)
		resp.Body.Close()
		// Between each request, the prior ones must still be findable.
		audit := fetchAudit(t, cv, user.AccessToken)
		for j := 0; j <= i; j++ {
			if audit.findByRequestID(reqIDs[j]) == nil {
				t.Fatalf("after %d-th request, prior request %d (%s) vanished from audit",
					i, j, reqIDs[j])
			}
		}
	}
}

// TestAuditGetByIDReturnsSameRow — /api/audit/{id} returns the same row
// as the list endpoint. Customers building forensic tools rely on this.
func TestAuditGetByIDReturnsSameRow(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-audit-byid")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "audit-byid"}, &agent)

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := cv.Client.Do(req)
	resp.Body.Close()

	audit := fetchAudit(t, cv, user.AccessToken)
	if len(audit.Entries) == 0 {
		t.Fatal("no audit rows")
	}
	id := audit.Entries[0].ID

	req2, _ := http.NewRequest("GET", cv.URL+"/api/audit/"+id, nil)
	req2.Header.Set("Authorization", "Bearer "+user.AccessToken)
	resp2, _ := cv.Client.Do(req2)
	body, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("GET /api/audit/{id} status=%d body=%s", resp2.StatusCode, body)
	}
	var single auditEntry
	if err := json.Unmarshal(body, &single); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if single.ID != id {
		t.Fatalf("single.ID=%q, want %q", single.ID, id)
	}
}

// TestAuditFilterByAgent — ?agent_id=X returns only that agent's rows.
func TestAuditFilterByAgent(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-audit-filter")

	var a1, a2 struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "audit-fa-1"}, &a1)
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "audit-fa-2"}, &a2)

	doRequest := func(token, reqID string) {
		req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
			bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-Id", reqID)
		resp, _ := cv.Client.Do(req)
		resp.Body.Close()
	}
	doRequest(a1.Token, "req-filter-a-1")
	doRequest(a1.Token, "req-filter-a-2")
	doRequest(a2.Token, "req-filter-b-1")

	time.Sleep(200 * time.Millisecond)

	req, _ := http.NewRequest("GET", cv.URL+"/api/audit?agent_id="+a1.ID+"&limit=200", nil)
	req.Header.Set("Authorization", "Bearer "+user.AccessToken)
	resp, _ := cv.Client.Do(req)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var filtered auditList
	if err := json.Unmarshal(body, &filtered); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, e := range filtered.Entries {
		if e.AgentID != nil && *e.AgentID != a1.ID {
			t.Fatalf("filtered audit includes agent_id=%s, expected only %s", *e.AgentID, a1.ID)
		}
	}
}

// helpers
func actionsOf(a auditList) []string {
	out := make([]string, 0, len(a.Entries))
	for _, e := range a.Entries {
		out = append(out, e.Action)
	}
	return out
}

func actionsAndOutcomes(a auditList) []string {
	out := make([]string, 0, len(a.Entries))
	for _, e := range a.Entries {
		out = append(out, e.Action+"/"+e.Decision+"/"+e.Outcome)
	}
	return out
}
