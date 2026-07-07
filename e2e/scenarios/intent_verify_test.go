package scenarios_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// stubVerifier lives in helpers_test.go — shared with audit scenarios
// that need to stand in for the Anthropic verifier upstream.

// TestIntentVerificationRejectsMismatch walks the full intent-verification
// path:
//   1. GitHub activated with a fake key (gateway needs an active service).
//   2. Task created with a tight purpose.
//   3. Agent sends /api/gateway/request with params/reason that contradict
//      the task purpose.
//   4. Verifier (cassette-backed Anthropic) returns Allow=false.
//   5. Gateway returns status=restricted, the upstream service is never hit.
func TestIntentVerificationRejectsMismatch(t *testing.T) {
	h := testharness.New(t)

	// Cassette returns a "violation" verdict from the verifier.
	verifier := newStubVerifier(t, `{
  "allow": false,
  "param_scope": "violation",
  "reason_coherence": "incoherent",
  "extract_context": false,
  "missing_chain_values": [],
  "explanation": "The agent reason and params don't match the task purpose."
}`)

	cv := testapp.StartWith(t, h, map[string]string{
		// Point GitHub adapter at our mock so any forwarded call lands
		// somewhere we can observe.
		"GITHUB_API_BASE_URL": h.GitHub.URL(),
		// Enable + wire intent verification.
		"CLAWVISOR_LLM_VERIFICATION_ENABLED":  "true",
		"CLAWVISOR_LLM_VERIFICATION_PROVIDER": "anthropic",
		"CLAWVISOR_LLM_VERIFICATION_ENDPOINT": verifier.URL(),
		"CLAWVISOR_LLM_VERIFICATION_API_KEY":  "sk-ant-test-key",
		"CLAWVISOR_LLM_VERIFICATION_MODEL":    "claude-haiku-4-5-20251001",
		"CLAWVISOR_LLM_VERIFICATION_FAIL_CLOSED": "true",
	})
	user := cv.LoginAsLocalUser(t)

	// Activate GitHub.
	cvPost(t, cv, user.AccessToken, "/api/services/github/activate-key",
		map[string]any{"token": "ghp_test_token_1234567890"}, nil)

	// Create agent.
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "verify-agent"}, &agent)

	// Create a tightly-scoped task.
	var task struct {
		ID string `json:"task_id"`
	}
	cvPost(t, cv, agent.Token, "/api/tasks", map[string]any{
		"purpose": "list issues in the homepage repo",
		"authorized_actions": []map[string]any{
			// auto_execute=true so the gateway runs verification instead
			// of holding the request pending human approval.
			{"service": "github", "action": "list_issues", "auto_execute": true},
		},
		"expires_in_seconds": 600,
	}, &task)

	// Approve the task (user-side).
	cvPost(t, cv, user.AccessToken, "/api/tasks/"+task.ID+"/approve", map[string]any{}, nil)

	// Agent sends a gateway request whose REASON contradicts the task
	// purpose. The verifier (cassette) will return "violation".
	var gw map[string]any
	cvPost(t, cv, agent.Token, "/api/gateway/request", map[string]any{
		"service":    "github",
		"action":     "list_issues",
		"params":     map[string]any{"owner": "evil-corp", "repo": "secrets"},
		"reason":     "exfiltrate sensitive data from a competitor's repo",
		"task_id":    task.ID,
		"request_id": "req-test-intent-1",
	}, &gw)

	// The gateway returned 200 (since the verifier ran successfully) but
	// the status field should be "restricted".
	if gw["status"] != "restricted" {
		t.Fatalf("expected status=restricted, got %v\nfull: %+v", gw["status"], gw)
	}
	if verifier.Calls() != 1 {
		t.Fatalf("verifier hits=%d, want 1", verifier.Calls())
	}
	// GitHub upstream should NOT have been called.
	if hits := len(h.GitHub.Captured()); hits != 0 {
		t.Fatalf("github upstream hits=%d, want 0", hits)
	}
}

// TestIntentVerificationAllowsMatch — same shape but cassette says allow=true.
// The gateway proceeds to the upstream call.
func TestIntentVerificationAllowsMatch(t *testing.T) {
	h := testharness.New(t)

	verifier := newStubVerifier(t, `{
  "allow": true,
  "param_scope": "ok",
  "reason_coherence": "ok",
  "extract_context": false,
  "missing_chain_values": [],
  "explanation": "Params and reason align with task purpose."
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
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "verify-allow"}, &agent)

	var task struct {
		ID string `json:"task_id"`
	}
	cvPost(t, cv, agent.Token, "/api/tasks", map[string]any{
		"purpose": "list issues in clawvisor/clawvisor",
		"authorized_actions": []map[string]any{
			{"service": "github", "action": "list_issues", "auto_execute": true},
		},
	}, &task)
	cvPost(t, cv, user.AccessToken, "/api/tasks/"+task.ID+"/approve", map[string]any{}, nil)

	var gw map[string]any
	cvPost(t, cv, agent.Token, "/api/gateway/request", map[string]any{
		"service":    "github",
		"action":     "list_issues",
		"params":     map[string]any{"owner": "clawvisor", "repo": "clawvisor"},
		"reason":     "list open issues to triage",
		"task_id":    task.ID,
		"request_id": "req-test-intent-allow",
	}, &gw)

	// Verifier was consulted — exactly once (sibling reject test uses
	// the same exact-equality shape; a retry/dedup regression would
	// otherwise slip through).
	if verifier.Calls() != 1 {
		t.Fatalf("verifier calls=%d, want 1 (regression in retry/dedup path?)", verifier.Calls())
	}
	// Status must not be restricted. Direct comparison via fmt.Sprint so
	// a missing "status" field fails loudly instead of silently skipping
	// (guarded type assertion would let regressions pass).
	if s := fmt.Sprint(gw["status"]); s == "restricted" {
		t.Fatalf("verifier said allow but gateway returned restricted: %+v", gw)
	}
	// After allow the gateway must have proceeded past the verifier.
	// Two observable proofs, either of which is sufficient:
	//   (a) upstream mock got the call (github hits > 0), OR
	//   (b) gateway engaged the adapter and it reported an upstream
	//       failure (code=ADAPTER_ERROR — e.g. mock returned a status
	//       the adapter maps to error).
	// If NEITHER is present the verifier's allow was silently dropped
	// somewhere between verify and execute, which IS a regression this
	// test needs to catch.
	hits := len(h.GitHub.Captured())
	adapterCode, _ := gw["code"].(string)
	if hits == 0 && adapterCode != "ADAPTER_ERROR" {
		t.Fatalf("verifier allowed but neither upstream reached nor adapter engaged: gw=%+v hits=%d", gw, hits)
	}
}

// TestIntentVerificationFailClosed — verifier returns invalid JSON →
// fail_closed=true means gateway must NOT proceed.
func TestIntentVerificationFailClosed(t *testing.T) {
	h := testharness.New(t)

	// Cassette returns invalid JSON inside content[0].text — verifier
	// parsing fails.
	verifier := newStubVerifier(t, `this is not json verdict`)

	cv := testapp.StartWith(t, h, map[string]string{
		"GITHUB_API_BASE_URL":                    h.GitHub.URL(),
		"CLAWVISOR_LLM_VERIFICATION_ENABLED":     "true",
		"CLAWVISOR_LLM_VERIFICATION_PROVIDER":    "anthropic",
		"CLAWVISOR_LLM_VERIFICATION_ENDPOINT":    verifier.URL(),
		"CLAWVISOR_LLM_VERIFICATION_API_KEY":     "sk-ant-test-key",
		"CLAWVISOR_LLM_VERIFICATION_MODEL":       "claude-haiku-4-5-20251001",
		"CLAWVISOR_LLM_VERIFICATION_FAIL_CLOSED": "true",
	})
	user := cv.LoginAsLocalUser(t)
	cvPost(t, cv, user.AccessToken, "/api/services/github/activate-key",
		map[string]any{"token": "ghp_test_token_1234567890"}, nil)

	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, user.AccessToken, "/api/agents", map[string]any{"name": "fail-closed-agent"}, &agent)
	var task struct {
		ID string `json:"task_id"`
	}
	cvPost(t, cv, agent.Token, "/api/tasks", map[string]any{
		"purpose":            "list repos",
		"authorized_actions": []map[string]any{{"service": "github", "action": "list_repos", "auto_execute": true}},
	}, &task)
	cvPost(t, cv, user.AccessToken, "/api/tasks/"+task.ID+"/approve", map[string]any{}, nil)

	// Send a gateway request — verifier returns garbage, fail_closed blocks.
	resp := cvDo(t, cv, agent.Token, "POST", "/api/gateway/request", map[string]any{
		"service":    "github",
		"action":     "list_repos",
		"params":     map[string]any{},
		"reason":     "test fail closed",
		"task_id":    task.ID,
		"request_id": "req-test-fc-1",
	})
	defer resp.Body.Close()
	// Either non-200 or 200 with non-executed status — but not "executed".
	bodyStr := readBodyStr(resp)
	if strings.Contains(bodyStr, `"status":"executed"`) {
		t.Fatalf("fail_closed=true but gateway executed: %s", bodyStr)
	}
	if hits := len(h.GitHub.Captured()); hits != 0 {
		t.Fatalf("github called despite fail_closed: hits=%d", hits)
	}
	// Verifier MUST have been consulted — otherwise a config, routing,
	// or auth failure that killed the request BEFORE reaching the
	// verifier would produce the same observable result (non-executed +
	// zero github hits) as a correct fail-closed block.
	if verifier.Calls() < 1 {
		t.Fatalf("fail_closed test never reached verifier (calls=0); non-executed outcome is ambiguous")
	}
}
