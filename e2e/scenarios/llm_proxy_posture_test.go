package scenarios_test

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// postureAgentReq issues a POST /api/v1/messages with the agent token in
// X-Clawvisor-Agent-Token and arbitrary extra headers (the client's own
// provider credential lives in Authorization / x-api-key). Returns the
// response for status/body assertions.
func postureAgentReq(t *testing.T, cv *testapp.Server, agentToken string, headers map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("X-Clawvisor-Agent-Token", agentToken)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("/api/v1/messages: %v", err)
	}
	return resp
}

func newPostureAgent(t *testing.T, cv *testapp.Server, userToken, name string) (id, token string) {
	t.Helper()
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, userToken, "/api/agents", map[string]any{"name": name}, &agent)
	return agent.ID, agent.Token
}

func auditContains(t *testing.T, cv *testapp.Server, userToken, needle string) bool {
	t.Helper()
	req, _ := http.NewRequest("GET", cv.URL+"/api/audit", nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("audit get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	compact := strings.ReplaceAll(string(body), " ", "")
	return strings.Contains(compact, needle)
}

// TestGovernStripsClientApiKey (§4b): govern posture (vault), the request
// presents a DIFFERENT client provider API key; the upstream capture must
// receive the vault key, not the client's, and audit records auth_mode:vault.
func TestGovernStripsClientApiKey(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC":   upstream.URL(),
		"CLAWVISOR_PROXY_LITE_UPSTREAM_AUTH": "vault",
	})
	user := cv.LoginAsLocalUser(t)
	const vaultKey = "sk-ant-api03-VAULT-injected-key"
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", vaultKey)
	_, token := newPostureAgent(t, cv, user.AccessToken, "govern-strip")

	resp := postureAgentReq(t, cv, token, map[string]string{
		"Authorization": "Bearer sk-ant-api03-CLIENT-different-key",
	})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBodyStr(resp))
	}
	if upstream.Count() != 1 {
		t.Fatalf("upstream hits=%d, want 1", upstream.Count())
	}
	got := upstream.Last()
	if k := got.Headers.Get("x-api-key"); k != vaultKey {
		t.Fatalf("upstream x-api-key=%q, want vault key %q", k, vaultKey)
	}
	if strings.Contains(got.Headers.Get("Authorization"), "CLIENT-different-key") {
		t.Fatalf("client API key leaked upstream: Authorization=%q", got.Headers.Get("Authorization"))
	}
	if !auditContains(t, cv, user.AccessToken, `"auth_mode":"vault"`) {
		t.Fatal("audit did not record auth_mode: vault")
	}
}

// TestGovernHeaderPlacementCannotSelectPassthrough (§4b F1): govern, cvis_ in
// X-Clawvisor-Agent-Token and a client key in Authorization must STILL inject
// the vault key — the middleware's header-derived passthrough flag is
// overridden in vault posture.
func TestGovernHeaderPlacementCannotSelectPassthrough(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC":   upstream.URL(),
		"CLAWVISOR_PROXY_LITE_UPSTREAM_AUTH": "vault",
	})
	user := cv.LoginAsLocalUser(t)
	const vaultKey = "sk-ant-api03-VAULT-wins"
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", vaultKey)
	_, token := newPostureAgent(t, cv, user.AccessToken, "govern-f1")

	// A client API key in Authorization would, under the old header-source
	// flag, select passthrough. In vault posture it must not.
	resp := postureAgentReq(t, cv, token, map[string]string{
		"Authorization": "Bearer sk-ant-api03-STALE-laptop-key",
	})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBodyStr(resp))
	}
	got := upstream.Last()
	if k := got.Headers.Get("x-api-key"); k != vaultKey {
		t.Fatalf("upstream x-api-key=%q, want vault key %q (passthrough not overridden)", k, vaultKey)
	}
	if strings.Contains(got.Headers.Get("Authorization"), "STALE-laptop-key") {
		t.Fatalf("client key leaked — header placement selected passthrough: %q", got.Headers.Get("Authorization"))
	}
}

// TestObserveCarriesSubscriptionSession (§4c): observe/passthrough carries a
// Claude subscription OAuth bearer + anthropic-beta header to the upstream
// unchanged, and audit records auth_mode:passthrough.
func TestObserveCarriesSubscriptionSession(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC":   upstream.URL(),
		"CLAWVISOR_PROXY_LITE_UPSTREAM_AUTH": "passthrough",
	})
	user := cv.LoginAsLocalUser(t)
	_, token := newPostureAgent(t, cv, user.AccessToken, "observe-sub")

	const subBearer = "Bearer sk-ant-oat01-SUBSCRIPTION-session"
	resp := postureAgentReq(t, cv, token, map[string]string{
		"Authorization":  subBearer,
		"anthropic-beta": "oauth-test",
	})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBodyStr(resp))
	}
	if upstream.Count() != 1 {
		t.Fatalf("upstream hits=%d, want 1", upstream.Count())
	}
	got := upstream.Last()
	if got.Headers.Get("Authorization") != subBearer {
		t.Fatalf("subscription bearer altered: Authorization=%q, want %q", got.Headers.Get("Authorization"), subBearer)
	}
	if got.Headers.Get("anthropic-beta") != "oauth-test" {
		t.Fatalf("anthropic-beta altered: %q, want oauth-test", got.Headers.Get("anthropic-beta"))
	}
	if !auditContains(t, cv, user.AccessToken, `"auth_mode":"passthrough"`) {
		t.Fatal("audit did not record auth_mode: passthrough")
	}
}

// TestGovernAutoGovernsSubscriptionSeat: govern (vault) with the default
// govern_subscription_seats=true FORWARDS the seat's own subscription bearer
// upstream (billing stays on the subscription — no rebilling) rather than
// refusing it. A vault key IS seeded to prove billing-neutrality: it must NOT
// be injected. Audit records auth_mode:subscription_passthrough +
// subscription_governed:true so an admin sees a governed (credential-local,
// enforced) subscription seat. Policy enforcement under this mode is proven
// separately in llm_proxy_local_gov_test.go.
func TestGovernAutoGovernsSubscriptionSeat(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC":   upstream.URL(),
		"CLAWVISOR_PROXY_LITE_UPSTREAM_AUTH": "vault",
	})
	user := cv.LoginAsLocalUser(t)
	// Seed a vault key that MUST NOT surface upstream (billing-neutral proof).
	const vaultKey = "sk-ant-api03-VAULT-must-not-inject"
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", vaultKey)
	_, token := newPostureAgent(t, cv, user.AccessToken, "govern-autogovern")

	const subBearer = "Bearer sk-ant-oat01-SUBSCRIPTION"
	resp := postureAgentReq(t, cv, token, map[string]string{
		"Authorization":  subBearer,
		"anthropic-beta": "oauth-test",
	})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d, want 200 (subscription seat auto-governed, not refused); body=%s", resp.StatusCode, readBodyStr(resp))
	}
	if upstream.Count() != 1 {
		t.Fatalf("upstream hits=%d, want 1", upstream.Count())
	}
	got := upstream.Last()
	if got.Headers.Get("Authorization") != subBearer {
		t.Fatalf("subscription bearer not forwarded: Authorization=%q, want %q", got.Headers.Get("Authorization"), subBearer)
	}
	if k := got.Headers.Get("x-api-key"); k != "" {
		t.Fatalf("vault key injected (rebilling!): upstream x-api-key=%q, want empty", k)
	}
	if strings.Contains(got.Headers.Get("Authorization"), "VAULT-must-not-inject") {
		t.Fatalf("vault key leaked upstream: Authorization=%q", got.Headers.Get("Authorization"))
	}
	if got.Headers.Get("anthropic-beta") != "oauth-test" {
		t.Fatalf("anthropic-beta altered: %q, want oauth-test", got.Headers.Get("anthropic-beta"))
	}
	if !auditContains(t, cv, user.AccessToken, `"auth_mode":"subscription_passthrough"`) {
		t.Fatal("audit did not record auth_mode: subscription_passthrough")
	}
	if !auditContains(t, cv, user.AccessToken, `"subscription_governed":true`) {
		t.Fatal("audit did not record subscription_governed: true")
	}
}

// TestGovernAutoGovernsUnrecognizedBearer: under the default auto-govern, an
// opaque (unrecognized) bearer is treated like any forwardable client bearer —
// forwarded upstream and governed — matching Observe, which forwards any
// bearer. (Under strict mode it fails closed; see
// TestGovernStrictModeRefusesUnrecognizedBearer.)
func TestGovernAutoGovernsUnrecognizedBearer(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC":   upstream.URL(),
		"CLAWVISOR_PROXY_LITE_UPSTREAM_AUTH": "vault",
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-api03-vault")
	_, token := newPostureAgent(t, cv, user.AccessToken, "govern-opaque-auto")

	const opaque = "Bearer opaque-unknown-future-token"
	resp := postureAgentReq(t, cv, token, map[string]string{
		"Authorization": opaque,
	})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d, want 200 (unrecognized bearer auto-governed); body=%s", resp.StatusCode, readBodyStr(resp))
	}
	if got := upstream.Last(); got.Headers.Get("Authorization") != opaque {
		t.Fatalf("unrecognized bearer not forwarded: Authorization=%q", got.Headers.Get("Authorization"))
	}
	if !auditContains(t, cv, user.AccessToken, `"auth_mode":"subscription_passthrough"`) {
		t.Fatal("audit did not record auth_mode: subscription_passthrough for the auto-governed bearer")
	}
}

// TestGovernStrictModeRefusesSubscriptionSeat: with the opt-out
// govern_subscription_seats=false, govern reverts to the strict
// "vaulted keys only" behavior — a subscription bearer is refused with
// SUBSCRIPTION_SEAT_NOT_GOVERNABLE and never reaches upstream (no rebill).
func TestGovernStrictModeRefusesSubscriptionSeat(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC":               upstream.URL(),
		"CLAWVISOR_PROXY_LITE_UPSTREAM_AUTH":             "vault",
		"CLAWVISOR_PROXY_LITE_GOVERN_SUBSCRIPTION_SEATS": "false",
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-api03-vault")
	_, token := newPostureAgent(t, cv, user.AccessToken, "govern-strict-refuse")

	resp := postureAgentReq(t, cv, token, map[string]string{
		"Authorization": "Bearer sk-ant-oat01-SUBSCRIPTION",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want 403; body=%s", resp.StatusCode, readBodyStr(resp))
	}
	if !strings.Contains(readBodyStr(resp), "SUBSCRIPTION_SEAT_NOT_GOVERNABLE") {
		t.Fatal("strict mode should refuse with SUBSCRIPTION_SEAT_NOT_GOVERNABLE")
	}
	if upstream.Count() != 0 {
		t.Fatalf("upstream hits=%d, want 0 (no silent rebill)", upstream.Count())
	}
}

// TestGovernStrictModeRefusesUnrecognizedBearer (fail-closed under strict): with
// govern_subscription_seats=false, an opaque non-API-key bearer is refused, not
// forwarded — preserving the keys-off-laptops guarantee.
func TestGovernStrictModeRefusesUnrecognizedBearer(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC":               upstream.URL(),
		"CLAWVISOR_PROXY_LITE_UPSTREAM_AUTH":             "vault",
		"CLAWVISOR_PROXY_LITE_GOVERN_SUBSCRIPTION_SEATS": "false",
	})
	user := cv.LoginAsLocalUser(t)
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", "sk-ant-api03-vault")
	_, token := newPostureAgent(t, cv, user.AccessToken, "govern-strict-opaque")

	resp := postureAgentReq(t, cv, token, map[string]string{
		"Authorization": "Bearer opaque-unknown-future-token",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want 403; body=%s", resp.StatusCode, readBodyStr(resp))
	}
	if !strings.Contains(readBodyStr(resp), "SUBSCRIPTION_SEAT_NOT_GOVERNABLE") {
		t.Fatal("opaque bearer should fail closed with SUBSCRIPTION_SEAT_NOT_GOVERNABLE under strict mode")
	}
	if upstream.Count() != 0 {
		t.Fatalf("upstream hits=%d, want 0", upstream.Count())
	}
}

// TestGovernApiKeyWithOauthBetaHeaderNotDosed (§4c F2 reverse): a real org
// API key plus a stray anthropic-beta: oauth-* header is still treated as an
// API key — the header alone does not force refusal.
func TestGovernApiKeyWithOauthBetaHeaderNotDosed(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC":   upstream.URL(),
		"CLAWVISOR_PROXY_LITE_UPSTREAM_AUTH": "vault",
	})
	user := cv.LoginAsLocalUser(t)
	const vaultKey = "sk-ant-api03-VAULT-key"
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", vaultKey)
	_, token := newPostureAgent(t, cv, user.AccessToken, "govern-beta")

	resp := postureAgentReq(t, cv, token, map[string]string{
		"Authorization":  "Bearer sk-ant-api03-REAL-client-key",
		"anthropic-beta": "oauth-2025-04-01",
	})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d, want 200 (API key must not be DoSed by the header); body=%s", resp.StatusCode, readBodyStr(resp))
	}
	if upstream.Count() != 1 {
		t.Fatalf("upstream hits=%d, want 1", upstream.Count())
	}
	if got := upstream.Last(); got.Headers.Get("x-api-key") != vaultKey {
		t.Fatalf("upstream x-api-key=%q, want vault key", got.Headers.Get("x-api-key"))
	}
}

// TestGovernMigratesSubscriptionWithConsent (§4c): govern +
// allow_subscription_billing_migration strips the subscription bearer and
// injects the vault key; audit records auth_mode:vault.
func TestGovernMigratesSubscriptionWithConsent(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC":         upstream.URL(),
		"CLAWVISOR_PROXY_LITE_UPSTREAM_AUTH":       "vault",
		"CLAWVISOR_PROXY_LITE_ALLOW_SUB_MIGRATION": "true",
	})
	user := cv.LoginAsLocalUser(t)
	const vaultKey = "sk-ant-api03-VAULT-migrated"
	llmCredSet(t, cv, user.AccessToken, "anthropic", "", vaultKey)
	_, token := newPostureAgent(t, cv, user.AccessToken, "govern-migrate")

	resp := postureAgentReq(t, cv, token, map[string]string{
		"Authorization": "Bearer sk-ant-oat01-SUBSCRIPTION",
	})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBodyStr(resp))
	}
	got := upstream.Last()
	if got.Headers.Get("x-api-key") != vaultKey {
		t.Fatalf("upstream x-api-key=%q, want vault key %q", got.Headers.Get("x-api-key"), vaultKey)
	}
	if strings.Contains(got.Headers.Get("Authorization"), "oat01") {
		t.Fatalf("subscription bearer not stripped: Authorization=%q", got.Headers.Get("Authorization"))
	}
	if !auditContains(t, cv, user.AccessToken, `"auth_mode":"vault"`) {
		t.Fatal("audit did not record auth_mode: vault after migration")
	}
}

// TestPassthroughForwardsClientXApiKey (§4): passthrough posture where the
// client presents ONLY an x-api-key (Anthropic SDK convention, no Authorization
// bearer) must forward that key upstream — never fall through to the vault key.
// No vault key is seeded here, so an implicit vault fallback (which §4 forbids)
// would surface UPSTREAM_KEY_MISSING and never reach the upstream capture.
func TestPassthroughForwardsClientXApiKey(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC":   upstream.URL(),
		"CLAWVISOR_PROXY_LITE_UPSTREAM_AUTH": "passthrough",
	})
	user := cv.LoginAsLocalUser(t)
	// Deliberately do NOT seed a vault key — a vault fallback would fail.
	_, token := newPostureAgent(t, cv, user.AccessToken, "passthrough-xapikey")

	const clientKey = "sk-ant-api03-CLIENT-own-key"
	resp := postureAgentReq(t, cv, token, map[string]string{
		"x-api-key": clientKey,
	})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d, want 200 (client x-api-key should be forwarded, not a vault fallback); body=%s", resp.StatusCode, readBodyStr(resp))
	}
	if upstream.Count() != 1 {
		t.Fatalf("upstream hits=%d, want 1", upstream.Count())
	}
	got := upstream.Last()
	if k := got.Headers.Get("x-api-key"); k != clientKey {
		t.Fatalf("upstream x-api-key=%q, want the client key %q", k, clientKey)
	}
	if !auditContains(t, cv, user.AccessToken, `"auth_mode":"passthrough"`) {
		t.Fatal("audit did not record auth_mode: passthrough")
	}
}

// TestPassthroughNoCredential (§4): passthrough posture with only the cvis
// token (no client provider credential) returns 401 PASSTHROUGH_NO_CREDENTIAL.
func TestPassthroughNoCredential(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"CLAWVISOR_LLM_UPSTREAM_ANTHROPIC":   upstream.URL(),
		"CLAWVISOR_PROXY_LITE_UPSTREAM_AUTH": "passthrough",
	})
	user := cv.LoginAsLocalUser(t)
	_, token := newPostureAgent(t, cv, user.AccessToken, "passthrough-nocred")

	// Only the cvis token — no provider bearer / x-api-key.
	resp := postureAgentReq(t, cv, token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401; body=%s", resp.StatusCode, readBodyStr(resp))
	}
	if !strings.Contains(readBodyStr(resp), "PASSTHROUGH_NO_CREDENTIAL") {
		t.Fatal("body missing PASSTHROUGH_NO_CREDENTIAL")
	}
	if upstream.Count() != 0 {
		t.Fatalf("upstream hits=%d, want 0", upstream.Count())
	}
}
