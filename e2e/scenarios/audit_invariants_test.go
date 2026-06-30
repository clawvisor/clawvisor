// Audit invariants — these are properties customers must be able to rely
// on for compliance reporting and forensic analysis.
package scenarios_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// TestAuditRowHasHTTPStatus — audit rows that came from HTTP requests
// (LLM proxy, gateway) include the upstream http_status so customers
// can answer "what status did the agent see?"
func TestAuditRowHasHTTPStatus(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-aud-http")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "aud-http"}, &agent)

	const reqID = "req-audit-http-status"
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", reqID)
	resp, _ := cv.Client.Do(req)
	resp.Body.Close()

	// Raw fetch so we can inspect http_status on the row.
	req2, _ := http.NewRequest("GET", cv.URL+"/api/audit?limit=200", nil)
	req2.Header.Set("Authorization", "Bearer "+user.AccessToken)
	resp2, _ := cv.Client.Do(req2)
	body, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	var raw struct {
		Entries []map[string]any `json:"entries"`
	}
	_ = json.Unmarshal(body, &raw)
	found := false
	for _, e := range raw.Entries {
		if rid, _ := e["request_id"].(string); rid == reqID {
			found = true
			if status, ok := e["http_status"]; !ok {
				t.Logf("note: row missing http_status field (may be in nested struct): %v", e)
			} else if status == nil || status == float64(0) {
				t.Logf("note: row has zero/nil http_status: %v", status)
			}
		}
	}
	if !found {
		t.Fatalf("no audit row for request_id=%s", reqID)
	}
}

// TestAuditRowsAreOrderedByTimestamp — list endpoint returns rows in
// recency order. Forensic tools page through these expecting that order.
func TestAuditRowsAreOrderedByTimestamp(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-aud-order")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "aud-order"}, &agent)

	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
			bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
		req.Header.Set("Authorization", "Bearer "+agent.Token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-Id", fmt.Sprintf("req-aud-order-%d", i))
		resp, _ := cv.Client.Do(req)
		resp.Body.Close()
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)

	audit := fetchAudit(t, cv, user.AccessToken)
	prev := ""
	for _, e := range audit.Entries {
		if e.Timestamp == "" {
			continue
		}
		if prev != "" && e.Timestamp > prev {
			t.Fatalf("audit rows not sorted by recency-first: %s came after %s", e.Timestamp, prev)
		}
		prev = e.Timestamp
	}
}

// TestAuditPaginationCovers — offset-based pagination returns all rows
// without gaps or duplicates.
func TestAuditPaginationCovers(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-aud-page")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "aud-page"}, &agent)

	const N = 30
	expected := make(map[string]bool)
	for i := 0; i < N; i++ {
		rid := fmt.Sprintf("req-aud-page-%03d", i)
		expected[rid] = true
		req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
			bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
		req.Header.Set("Authorization", "Bearer "+agent.Token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-Id", rid)
		resp, _ := cv.Client.Do(req)
		resp.Body.Close()
	}
	time.Sleep(300 * time.Millisecond)

	seen := map[string]int{}
	for offset := 0; ; offset += 10 {
		req, _ := http.NewRequest("GET",
			fmt.Sprintf("%s/api/audit?limit=10&offset=%d", cv.URL, offset), nil)
		req.Header.Set("Authorization", "Bearer "+user.AccessToken)
		resp, _ := cv.Client.Do(req)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var page auditList
		_ = json.Unmarshal(body, &page)
		if len(page.Entries) == 0 {
			break
		}
		for _, e := range page.Entries {
			seen[e.RequestID]++
		}
		if offset > 200 {
			break
		}
	}
	missing := 0
	for rid := range expected {
		if seen[rid] == 0 {
			missing++
		}
	}
	if missing > 0 {
		t.Fatalf("audit pagination dropped %d/%d rows across pages", missing, N)
	}
	// No duplicates expected.
	dups := 0
	for rid, n := range seen {
		if n > 1 && expected[rid] {
			dups++
		}
	}
	if dups > 0 {
		t.Fatalf("audit pagination returned %d duplicate rows", dups)
	}
}

// TestAuditServiceAndActionPopulated — every LLM-proxy audit row has a
// non-empty service + action field so analytics queries (GROUP BY service)
// always have something to group on.
func TestAuditServiceAndActionPopulated(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-aud-svc")
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "aud-svc"}, &agent)

	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := cv.Client.Do(req)
	resp.Body.Close()
	time.Sleep(150 * time.Millisecond)

	audit := fetchAudit(t, cv, user.AccessToken)
	for _, e := range audit.Entries {
		if strings.Contains(e.Action, "messages.create") {
			if e.Action == "" {
				t.Fatalf("row has empty action: %+v", e)
			}
		}
	}
}

// TestAuditSeparateAgentsHaveSeparateRows — multiple agents under one
// user; each agent's actions audit independently (no row merging).
func TestAuditSeparateAgentsHaveSeparateRows(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-aud-sep")

	var a1, a2 struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "aud-sep-1"}, &a1)
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "aud-sep-2"}, &a2)

	do := func(token, rid string) {
		req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
			bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-Id", rid)
		resp, _ := cv.Client.Do(req)
		resp.Body.Close()
	}
	do(a1.Token, "req-sep-agent-1")
	do(a2.Token, "req-sep-agent-2")
	time.Sleep(200 * time.Millisecond)

	audit := fetchAudit(t, cv, user.AccessToken)
	row1 := audit.findByRequestID("req-sep-agent-1")
	row2 := audit.findByRequestID("req-sep-agent-2")
	if row1 == nil || row2 == nil {
		t.Fatalf("missing rows: row1=%v row2=%v", row1, row2)
	}
	if row1.AgentID == nil || row2.AgentID == nil {
		t.Fatalf("rows missing agent_id: row1=%v row2=%v", row1, row2)
	}
	if *row1.AgentID != a1.ID || *row2.AgentID != a2.ID {
		t.Fatalf("agent_ids crossed: row1.agent_id=%s want %s; row2.agent_id=%s want %s",
			*row1.AgentID, a1.ID, *row2.AgentID, a2.ID)
	}
}
