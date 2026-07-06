package scenarios_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// TestLiveObservePassthrough (spec 02 keyed live lane): a real Anthropic call
// through an observe-mode (passthrough + enforcement-off) proxy-lite, with the
// operator's own credential presented as the CLIENT credential. Proves the
// Observe "zero behavior change" promise against the real upstream:
//
//   - response integrity: a real 200 assistant message flows back unaltered,
//   - cost row: usage tokens are metered into the audit record,
//   - no vault read: NO anthropic vault entry is configured, yet the request
//     succeeds via passthrough (auth_mode: passthrough), so the vault was
//     never consulted.
//
// Gated on ANTHROPIC_API_KEY (run e.g.
// ANTHROPIC_API_KEY="$CLAWVISOR_ANTHROPIC_E2E_KEY" go test ...). The upstream
// is left at the real https://api.anthropic.com default (no
// CLAWVISOR_LLM_UPSTREAM_ANTHROPIC override).
//
// IMPORTANT (verified against forward.go): observe passthrough forwards the
// client's Authorization: Bearer verbatim and strips inbound x-api-key. Real
// Anthropic rejects a plain API key sent as a Bearer token ("Invalid bearer
// token") — an sk-ant-api… key must ride x-api-key, which only the govern/
// vault path injects. So the passthrough lane's client credential is an
// OAuth/subscription bearer (sk-ant-oat01-…), the seat type Observe is built
// for. When ANTHROPIC_API_KEY holds a plain API key this test skips: the
// passthrough-carries-credential invariant is proven deterministically by
// TestObserveCarriesSubscriptionSession, and the govern/vault key-injection
// path is proven by TestGovernStripsClientApiKey.
func TestLiveObservePassthrough(t *testing.T) {
	cred := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	if cred == "" {
		t.Skip("ANTHROPIC_API_KEY not set; skipping live observe passthrough test")
	}
	if !strings.HasPrefix(cred, "sk-ant-oat01-") && !strings.HasPrefix(cred, "sk-ant-ort01-") {
		t.Skip("ANTHROPIC_API_KEY is a plain API key, not an OAuth/subscription bearer; " +
			"observe passthrough forwards Authorization and Anthropic rejects Bearer API keys. " +
			"Set an OAuth bearer to exercise this lane; passthrough carriage is covered " +
			"deterministically by TestObserveCarriesSubscriptionSession.")
	}

	h := testharness.New(t)
	// posture: observe -> proxy-lite enabled, upstream_auth=passthrough,
	// enforcement_mode=observe. No upstream override => real Anthropic.
	cv := testapp.StartWithConfig(t, h, nil, "posture: observe")

	user := cv.LoginAsLocalUser(t)
	// Deliberately DO NOT set an anthropic vault entry: passthrough must
	// forward the client's own credential and never touch the vault.
	_, token := newPostureAgent(t, cv, user.AccessToken, "live-observe")

	reqBody := []byte(`{"model":"claude-haiku-4-5-20251001","max_tokens":16,"messages":[{"role":"user","content":"Reply with the single word: pong"}]}`)
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages", bytes.NewReader(reqBody))
	req.Header.Set("X-Clawvisor-Agent-Token", token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	// The operator's own OAuth/subscription bearer as the CLIENT credential.
	// Passthrough forwards it to the upstream unchanged (no vault injection).
	req.Header.Set("Authorization", "Bearer "+cred)

	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("/api/v1/messages: %v", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, rb)
	}

	// Response integrity: a real Anthropic assistant message with content —
	// NOT a lite-proxy error envelope wrapped as an assistant turn.
	var msg struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rb, &msg); err != nil {
		t.Fatalf("decode upstream response: %v; body=%s", err, rb)
	}
	if msg.Type != "message" || msg.Role != "assistant" || len(msg.Content) == 0 {
		t.Fatalf("unexpected response shape (integrity / not a real upstream message): %s", rb)
	}

	// Cost row + no vault read: the audit record carries usage tokens
	// (cost metering ran) and auth_mode: passthrough (the vault key was
	// never injected — no vault read).
	if !auditContains(t, cv, user.AccessToken, `"auth_mode":"passthrough"`) {
		t.Fatal("audit did not record auth_mode: passthrough (vault should not have been read)")
	}
	if !auditContains(t, cv, user.AccessToken, `"usage_input_tokens"`) {
		t.Fatal("audit did not record a cost/usage row (usage_input_tokens missing)")
	}
}
