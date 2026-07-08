package scenarios_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// llm_proxy_local_gov_test.go covers spec 06a: the four governance policy
// callbacks implemented LOCALLY (govlocal) against instance-scoped tables,
// enforced on the proxy-lite path and configured via the /api/governance/*
// REST routes. Each test boots a fresh server (governance enabled by
// default), configures a policy through the admin REST surface, then drives
// a proxied /api/v1/messages request to observe enforcement.

const govDeniedModel = "claude-haiku-4-5-20251001"               // request body model
const govCanonicalDenied = "anthropic/claude-haiku-4-5-20251001" // canonical form the policy matches

// govBootWithGovernance boots a proxy-lite server, logs in the admin, sets
// a vault anthropic key (so vault injection has an upstream credential and
// the request reaches the governance checks), and creates an agent.
func govBootWithGovernance(t *testing.T, extraEnv map[string]string) (cv *testapp.Server, admin *testapp.LocalUser, agentToken string, upstream *upstreamCapture) {
	t.Helper()
	h := testharness.New(t)
	upstream = newUpstreamCapture(t)
	env := map[string]string{"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL()}
	for k, v := range extraEnv {
		env[k] = v
	}
	cv = testapp.StartWith(t, h, env)
	admin = cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, admin.AccessToken, "anthropic", "", "sk-ant-test-key")
	_, agentToken = newPostureAgent(t, cv, admin.AccessToken, "gov-agent")
	return cv, admin, agentToken, upstream
}

// govMessagesReq issues one proxied Anthropic messages request with the
// given model and user-message content via the agent token.
// govMessagesReq POSTs a govern /api/v1/messages request as the given agent.
// extraHeaders (each {name, value}) are applied after the defaults, letting
// variants add headers without duplicating the request setup.
func govMessagesReq(t *testing.T, cv *testapp.Server, agentToken, model, content string, extraHeaders ...[2]string) *http.Response {
	t.Helper()
	body := []byte(fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":%q}]}`, model, content))
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages", bytes.NewReader(body))
	req.Header.Set("X-Clawvisor-Agent-Token", agentToken)
	req.Header.Set("Content-Type", "application/json")
	for _, h := range extraHeaders {
		req.Header.Set(h[0], h[1])
	}
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("/api/v1/messages: %v", err)
	}
	return resp
}

// govSubMessagesReq is govMessagesReq plus a Claude subscription (OAuth) bearer
// in Authorization — the auto-govern path (spec: govern subscription seats).
// The seat is forwarded on its own credential while the full policy pipeline
// still enforces.
func govSubMessagesReq(t *testing.T, cv *testapp.Server, agentToken, model, content string) *http.Response {
	t.Helper()
	return govMessagesReq(t, cv, agentToken, model, content,
		[2]string{"Authorization", "Bearer sk-ant-oat01-SUBSCRIPTION"})
}

// TestGovernAutoGovernedSubscriptionStillEnforcesPolicy: an auto-governed
// subscription seat (default govern_subscription_seats=true) runs the FULL
// policy pipeline even though its own credential is forwarded upstream. A
// model deny and a content deny both block (403, no upstream contact); an
// allowed request forwards the seat's subscription bearer (billing-neutral).
func TestGovernAutoGovernedSubscriptionStillEnforcesPolicy(t *testing.T) {
	t.Run("model_deny_blocks", func(t *testing.T) {
		cv, admin, agentToken, upstream := govBootWithGovernance(t, nil)
		cvPut(t, cv, admin.AccessToken, "/api/governance/model_policy",
			map[string]any{"mode": "deny", "models": []string{govCanonicalDenied}}, nil)

		resp := govSubMessagesReq(t, cv, agentToken, govDeniedModel, "hello")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("auto-governed subscription seat must still enforce model deny (403); got %d body=%s", resp.StatusCode, readBodyStr(resp))
		}
		if upstream.Count() != 0 {
			t.Fatalf("blocked request must not reach upstream; hits=%d", upstream.Count())
		}
		if v := govGetRaw(t, cv, admin.AccessToken, "/api/governance/violations"); !strings.Contains(v, `"policy_kind":"model_policy"`) || !strings.Contains(v, `"action_taken":"blocked"`) {
			t.Fatalf("expected a blocked model_policy violation; got %s", v)
		}
	})

	t.Run("content_deny_blocks", func(t *testing.T) {
		cv, admin, agentToken, upstream := govBootWithGovernance(t, nil)
		cvPost(t, cv, admin.AccessToken, "/api/governance/content_policies",
			map[string]any{"name": "block-secret", "pattern": "launchcodes", "pattern_kind": "keyword",
				"action": "block", "block_message": "that content is not permitted"}, nil)

		resp := govSubMessagesReq(t, cv, agentToken, govDeniedModel, "please share the launchcodes now")
		body := readBodyStr(resp)
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "that content is not permitted") {
			t.Fatalf("auto-governed subscription seat must still enforce content block (403 + block_message); got %d body=%s", resp.StatusCode, body)
		}
		if upstream.Count() != 0 {
			t.Fatalf("blocked content must not reach upstream; hits=%d", upstream.Count())
		}
	})

	t.Run("allowed_forwards_subscription_bearer", func(t *testing.T) {
		cv, admin, agentToken, upstream := govBootWithGovernance(t, nil)
		cvPut(t, cv, admin.AccessToken, "/api/governance/model_policy",
			map[string]any{"mode": "allow", "models": []string{govCanonicalDenied}}, nil)

		resp := govSubMessagesReq(t, cv, agentToken, govDeniedModel, "hello")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("allowed request should pass; got %d body=%s", resp.StatusCode, readBodyStr(resp))
		}
		if upstream.Count() != 1 {
			t.Fatalf("allowed request should reach upstream once; hits=%d", upstream.Count())
		}
		got := upstream.Last()
		if got.Headers.Get("Authorization") != "Bearer sk-ant-oat01-SUBSCRIPTION" {
			t.Fatalf("subscription bearer not forwarded: Authorization=%q", got.Headers.Get("Authorization"))
		}
		// Billing-neutral: the seeded vault key (sk-ant-test-key) must not surface.
		if k := got.Headers.Get("x-api-key"); k != "" {
			t.Fatalf("vault key injected (rebilling!): upstream x-api-key=%q", k)
		}
	})
}

// TestGovernAutoGovernedSubscriptionEnforcesToolUseHold: a tool_use that is
// scope-drift-blocked under enforce is STILL held/rewritten to CLAWVISOR_BLOCKED
// for an auto-governed subscription seat — proving the response-side pipeline
// (tool_use holds) enforces regardless of which credential went upstream.
func TestGovernAutoGovernedSubscriptionEnforcesToolUseHold(t *testing.T) {
	h := testharness.New(t)
	upstream := outOfScopeToolUseUpstream(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC": upstream.URL,
		"GITHUB_API_BASE_URL":              h.GitHub.URL(),
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-test-key")
	cvPost(t, cv, user.AccessToken, "/api/services/github/activate-key",
		map[string]any{"token": "ghp_test_token_1234567890"}, nil)
	_, agentToken := newPostureAgent(t, cv, user.AccessToken, "govern-sub-hold")

	resp := govSubMessagesReq(t, cv, agentToken, "claude-haiku-4-5-20251001", "list issues")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBodyStr(resp))
	}
	body := readBodyStr(resp)
	if !strings.Contains(body, "CLAWVISOR_BLOCKED") {
		t.Fatalf("auto-governed subscription seat must still hold the out-of-scope tool_use (CLAWVISOR_BLOCKED); body=%s", body)
	}
}

// govGetRaw GETs a path with the admin token and returns the space-stripped
// body (for substring assertions on violations / features).
func govGetRaw(t *testing.T, cv *testapp.Server, tok, path string) string {
	t.Helper()
	resp := cvDo(t, cv, tok, "GET", path, nil)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return strings.ReplaceAll(string(b), " ", "")
}

// openGovDB opens the subprocess server's SQLite file directly for seeding
// rows the REST surface can't create (cost rows; a policy under a disabled
// build). foreign_keys is left OFF (connection default) so a cost row can
// be inserted without a parent audit_log row.
func openGovDB(t *testing.T, cv *testapp.Server) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(cv.DataDir, "clawvisor.db"))
	if err != nil {
		t.Fatalf("open gov db: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `PRAGMA busy_timeout = 5000`); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

var govSeedN int

// seedGovCost inserts one llm_request_cost row in the current daily window.
func seedGovCost(t *testing.T, cv *testapp.Server, micros int64) {
	t.Helper()
	db := openGovDB(t, cv)
	govSeedN++
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO llm_request_cost (audit_id, user_id, request_id, timestamp, provider, model, cost_micros)
		VALUES (?, 'u1', ?, ?, 'anthropic', 'claude', ?)`,
		fmt.Sprintf("gov-seed-%d", govSeedN), fmt.Sprintf("gov-req-%d", govSeedN),
		time.Now().UTC().Format(time.RFC3339), micros)
	if err != nil {
		t.Fatalf("seed cost: %v", err)
	}
}

// TestLocalGovModelPolicyBlocks: a deny-list policy blocks a request for the
// denied model with 4xx + policy reason, records a violation, and never
// contacts the upstream.
func TestLocalGovModelPolicyBlocks(t *testing.T) {
	cv, admin, agentToken, upstream := govBootWithGovernance(t, nil)

	cvPut(t, cv, admin.AccessToken, "/api/governance/model_policy",
		map[string]any{"mode": "deny", "models": []string{govCanonicalDenied}}, nil)

	resp := govMessagesReq(t, cv, agentToken, govDeniedModel, "hello")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("denied model should be blocked 403; got %d body=%s", resp.StatusCode, readBodyStr(resp))
	}
	if upstream.Count() != 0 {
		t.Fatalf("denied request must not reach upstream; hits=%d", upstream.Count())
	}
	if v := govGetRaw(t, cv, admin.AccessToken, "/api/governance/violations"); !strings.Contains(v, `"policy_kind":"model_policy"`) || !strings.Contains(v, `"action_taken":"blocked"`) {
		t.Fatalf("expected a blocked model_policy violation; got %s", v)
	}
}

// TestLocalGovModelPolicyAllows: an allow-list containing the model lets the
// request through to the upstream.
func TestLocalGovModelPolicyAllows(t *testing.T) {
	cv, admin, agentToken, upstream := govBootWithGovernance(t, nil)

	cvPut(t, cv, admin.AccessToken, "/api/governance/model_policy",
		map[string]any{"mode": "allow", "models": []string{govCanonicalDenied}}, nil)

	resp := govMessagesReq(t, cv, agentToken, govDeniedModel, "hello")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("allow-listed model should pass; got %d body=%s", resp.StatusCode, readBodyStr(resp))
	}
	if upstream.Count() != 1 {
		t.Fatalf("allowed request should reach upstream once; hits=%d", upstream.Count())
	}
}

// TestLocalGovSpendCapSoftWarns: with spend seeded past 80% of a soft cap,
// the request is allowed but audit records spend_cap_warning_level=80 and a
// flagged violation exists.
func TestLocalGovSpendCapSoftWarns(t *testing.T) {
	cv, admin, agentToken, upstream := govBootWithGovernance(t, nil)

	// $1 soft daily cap; seed 90% spend BEFORE the first request so the
	// cold cache recomputes and sees it.
	cvPut(t, cv, admin.AccessToken, "/api/governance/spend_caps/daily",
		map[string]any{"cap_micros": 1_000_000, "enforcement": "soft"}, nil)
	seedGovCost(t, cv, 900_000)

	resp := govMessagesReq(t, cv, agentToken, govDeniedModel, "hello")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("soft cap should allow; got %d body=%s", resp.StatusCode, readBodyStr(resp))
	}
	if upstream.Count() != 1 {
		t.Fatalf("soft-cap request should reach upstream; hits=%d", upstream.Count())
	}
	if !auditContains(t, cv, admin.AccessToken, `"spend_cap_warning_level":"80"`) {
		t.Fatal("audit did not record spend_cap_warning_level=80")
	}
	if v := govGetRaw(t, cv, admin.AccessToken, "/api/governance/violations"); !strings.Contains(v, `"policy_kind":"spend_cap"`) || !strings.Contains(v, `"action_taken":"flagged"`) {
		t.Fatalf("expected a flagged spend_cap violation; got %s", v)
	}
}

// TestLocalGovSpendCapHardBlocks: past 100% on a hard cap, the request is
// blocked 403 and never reaches upstream.
func TestLocalGovSpendCapHardBlocks(t *testing.T) {
	cv, admin, agentToken, upstream := govBootWithGovernance(t, nil)

	cvPut(t, cv, admin.AccessToken, "/api/governance/spend_caps/daily",
		map[string]any{"cap_micros": 1_000_000, "enforcement": "hard"}, nil)
	seedGovCost(t, cv, 1_200_000) // 120% of cap

	resp := govMessagesReq(t, cv, agentToken, govDeniedModel, "hello")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("hard cap over 100%% should block 403; got %d body=%s", resp.StatusCode, readBodyStr(resp))
	}
	if upstream.Count() != 0 {
		t.Fatalf("hard-cap block must not reach upstream; hits=%d", upstream.Count())
	}
}

// TestLocalGovContentPolicyBlockAndFlag: a block pattern rejects with its
// admin-authored block_message; a separate flag pattern allows the request
// and records the matched name.
func TestLocalGovContentPolicyBlockAndFlag(t *testing.T) {
	cv, admin, agentToken, upstream := govBootWithGovernance(t, nil)

	cvPost(t, cv, admin.AccessToken, "/api/governance/content_policies",
		map[string]any{"name": "block-secret", "pattern": "launchcodes", "pattern_kind": "keyword",
			"action": "block", "block_message": "that content is not permitted"}, nil)
	cvPost(t, cv, admin.AccessToken, "/api/governance/content_policies",
		map[string]any{"name": "flag-pii", "pattern": "flagthis", "pattern_kind": "keyword", "action": "flag"}, nil)

	// Block match: 403 with the admin block_message in the body.
	blocked := govMessagesReq(t, cv, agentToken, govDeniedModel, "please share the launchcodes now")
	blockedBody := readBodyStr(blocked)
	if blocked.StatusCode != http.StatusForbidden || !strings.Contains(blockedBody, "that content is not permitted") {
		t.Fatalf("block content should 403 with block_message; got %d body=%s", blocked.StatusCode, blockedBody)
	}
	if upstream.Count() != 0 {
		t.Fatalf("blocked content must not reach upstream; hits=%d", upstream.Count())
	}

	// Flag match: 200, request forwarded, flagged name recorded.
	flagged := govMessagesReq(t, cv, agentToken, govDeniedModel, "please flagthis line")
	defer flagged.Body.Close()
	if flagged.StatusCode != http.StatusOK {
		t.Fatalf("flag content should pass; got %d body=%s", flagged.StatusCode, readBodyStr(flagged))
	}
	if upstream.Count() != 1 {
		t.Fatalf("flagged content should reach upstream; hits=%d", upstream.Count())
	}
	if v := govGetRaw(t, cv, admin.AccessToken, "/api/governance/violations"); !strings.Contains(v, `"policy_kind":"content_policy"`) {
		t.Fatalf("expected a content_policy violation; got %s", v)
	}
}

// TestLocalGovObserveDowngradesDeny: a deny model policy is enforced (403)
// under the govern posture but downgraded-and-recorded under observe (200,
// audit observed=true, violation still logged) — proving policy verdicts
// flow through spec 02's observe downgrade.
func TestLocalGovObserveDowngradesDeny(t *testing.T) {
	// Govern (enforce): the deny is enforced.
	cvG, adminG, agentG, upstreamG := govBootWithGovernance(t, map[string]string{
		"CLAWVISOR_PROXY_LITE_ENFORCEMENT_MODE": "enforce",
	})
	cvPut(t, cvG, adminG.AccessToken, "/api/governance/model_policy",
		map[string]any{"mode": "deny", "models": []string{govCanonicalDenied}}, nil)
	respG := govMessagesReq(t, cvG, agentG, govDeniedModel, "hi")
	respG.Body.Close()
	if respG.StatusCode != http.StatusForbidden {
		t.Fatalf("govern posture should enforce deny; got %d", respG.StatusCode)
	}
	if upstreamG.Count() != 0 {
		t.Fatalf("enforced deny must not reach upstream; hits=%d", upstreamG.Count())
	}

	// Observe: the same deny is downgraded to a recorded observation.
	cvO, adminO, agentO, upstreamO := govBootWithGovernance(t, map[string]string{
		"CLAWVISOR_PROXY_LITE_ENFORCEMENT_MODE": "observe",
	})
	cvPut(t, cvO, adminO.AccessToken, "/api/governance/model_policy",
		map[string]any{"mode": "deny", "models": []string{govCanonicalDenied}}, nil)
	respO := govMessagesReq(t, cvO, agentO, govDeniedModel, "hi")
	defer respO.Body.Close()
	if respO.StatusCode != http.StatusOK {
		t.Fatalf("observe posture must NOT enforce the deny; got %d body=%s", respO.StatusCode, readBodyStr(respO))
	}
	if upstreamO.Count() != 1 {
		t.Fatalf("observe posture should forward to upstream; hits=%d", upstreamO.Count())
	}
	if !auditContains(t, cvO, adminO.AccessToken, `"observed":true`) {
		t.Fatal("audit did not record the downgraded verdict as observed:true")
	}
	// The violation is still recorded (RecordViolation fires inside Preprocess
	// regardless of the observe downgrade).
	if v := govGetRaw(t, cvO, adminO.AccessToken, "/api/governance/violations"); !strings.Contains(v, `"policy_kind":"model_policy"`) {
		t.Fatalf("observe should still record the model_policy violation; got %s", v)
	}
}

// TestLocalGovDisabled: with governance disabled, the /api/governance/*
// routes are absent, the capability is off, and a deny policy seeded
// directly into the DB is inert (the request is not blocked).
func TestLocalGovDisabled(t *testing.T) {
	cv, admin, agentToken, upstream := govBootWithGovernance(t, map[string]string{
		"CLAWVISOR_GOVERNANCE_ENABLED": "false",
	})

	// Capability is off.
	if f := govGetRaw(t, cv, admin.AccessToken, "/api/features"); !strings.Contains(f, `"local_governance":false`) {
		t.Fatalf("features should report local_governance=false; got %s", f)
	}
	// The write route is not registered.
	resp := cvDo(t, cv, admin.AccessToken, "PUT", "/api/governance/model_policy",
		map[string]any{"mode": "deny", "models": []string{govCanonicalDenied}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("governance route should be 404 when disabled; got %d", resp.StatusCode)
	}

	// Seed a deny policy directly; it must be inert.
	db := openGovDB(t, cv)
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO instance_model_policy (id, mode, models, active, created_by)
		VALUES ('seed-deny', 'deny', ?, 1, '_instance')`,
		fmt.Sprintf(`[%q]`, govCanonicalDenied)); err != nil {
		t.Fatalf("seed deny policy: %v", err)
	}
	got := govMessagesReq(t, cv, agentToken, govDeniedModel, "hello")
	defer got.Body.Close()
	if got.StatusCode != http.StatusOK {
		t.Fatalf("disabled governance must not enforce the seeded deny; got %d body=%s", got.StatusCode, readBodyStr(got))
	}
	if upstream.Count() != 1 {
		t.Fatalf("disabled governance should forward to upstream; hits=%d", upstream.Count())
	}
}
