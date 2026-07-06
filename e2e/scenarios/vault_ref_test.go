package scenarios_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/testharness"
)

// mockSecretsManager stands in for AWS Secrets Manager over the AWS JSON 1.1
// protocol the SDK v2 speaks. It returns a fixed SecretString for a known
// SecretId and a ResourceNotFoundException for anything else, so the real
// refaws code path (SigV4 signing, endpoint override, error mapping) is
// exercised deterministically without a cloud account or credentials.
type mockSecretsManager struct {
	srv    *httptest.Server
	knownS map[string]string // SecretId -> SecretString
	hits   int
}

func newMockSecretsManager(t *testing.T, known map[string]string) *mockSecretsManager {
	t.Helper()
	m := &mockSecretsManager{knownS: known}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockSecretsManager) URL() string { return m.srv.URL }

func (m *mockSecretsManager) handle(w http.ResponseWriter, r *http.Request) {
	m.hits++
	raw, _ := io.ReadAll(r.Body)
	r.Body.Close()
	var req struct {
		SecretId string `json:"SecretId"`
	}
	_ = json.Unmarshal(raw, &req)
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	secret, ok := m.knownS[req.SecretId]
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"__type":"ResourceNotFoundException","message":"secret not found"}`))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ARN":          req.SecretId,
		"Name":         "prod/anthropic",
		"SecretString": secret,
	})
}

// awsRefEnv returns the env that points the aws-sm resolver at the mock and
// supplies dummy AMBIENT credentials (env chain — the ambient-identity model;
// no static keys live in Clawvisor config).
func awsRefEnv(endpoint, allowlist string) map[string]string {
	return map[string]string{
		"CLAWVISOR_VAULT_AWS_SM_ENDPOINT": endpoint,
		"VAULT_REFERENCE_ALLOWLIST":       allowlist,
		"AWS_ACCESS_KEY_ID":               "test",
		"AWS_SECRET_ACCESS_KEY":           "test",
		"AWS_REGION":                      "us-east-1",
	}
}

const (
	refARN      = "arn:aws:secretsmanager:us-east-1:123456789012:secret:prod/anthropic-x9Y"
	refAllowPfx = "arn:aws:secretsmanager:us-east-1:123456789012:secret:prod/"
)

// TestVaultRefInjection proves the full reference data flow: an admin stores a
// reference (not a value) for the upstream Anthropic key; when a proxy-lite
// request forwards, the key is RESOLVED from the fake secret store and injected
// upstream exactly as a pushed value would be — and the plaintext is never
// persisted in Clawvisor.
func TestVaultRefInjection(t *testing.T) {
	h := testharness.New(t)
	upstream := newUpstreamCapture(t)
	const resolvedKey = "sk-ant-resolved-from-aws-sm-ref-000"
	mockSM := newMockSecretsManager(t, map[string]string{refARN: resolvedKey})

	env := awsRefEnv(mockSM.URL(), refAllowPfx)
	env["CLAWVISOR_LLM_UPSTREAM_ANTHROPIC"] = upstream.URL()
	cv := testapp.StartWith(t, h, env)

	// Mint an instance-admin token; references are admin-gated (confused-deputy
	// control) and token-authenticated writes are owned by `_instance`.
	user := cv.LoginAsLocalUser(t)
	_, adminTok := mintToken(t, cv, user.AccessToken, "tf", "instance-admin")

	// The agent is created via the token, so it (and its LLM lookups) belong
	// to `_instance` — the same owner the reference is stored under.
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	cvPost(t, cv, adminTok, "/api/agents", map[string]any{"name": "ref-agent"}, &agent)

	// Store the reference for the "anthropic" LLM key, verifying it on create.
	cvPost(t, cv, adminTok, "/api/vault/items?verify=1", map[string]any{
		"id": "anthropic",
		"reference": map[string]any{
			"backend": "aws-sm",
			"id":      refARN,
		},
	}, nil)

	// Drive a proxy-lite request; the proxy must resolve + inject the key.
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"model":"claude-haiku-4-5-20251001","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+agent.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("/api/v1/messages: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	if upstream.Count() != 1 {
		t.Fatalf("upstream hits=%d, want 1", upstream.Count())
	}
	if k := upstream.Last().Headers.Get("x-api-key"); k != resolvedKey {
		t.Fatalf("upstream x-api-key=%q, want the resolved reference value", k)
	}

	// The resolved plaintext never appears in the vault listing (metadata
	// only) — the reference is stored, the secret is not. (The "anthropic"
	// storage key renders as an llm_provider_key in the listing; kind=reference
	// surfacing for a plain reference id is covered by the provider lane.)
	var items struct {
		Entries []map[string]any `json:"entries"`
	}
	cvGet(t, cv, adminTok, "/api/vault/items", &items)
	for _, it := range items.Entries {
		body, _ := json.Marshal(it)
		if bytes.Contains(body, []byte(resolvedKey)) {
			t.Fatalf("resolved plaintext leaked into vault listing: %s", body)
		}
	}
}

// TestVaultRefErrorSurface asserts the actionable 4xx errors a bad reference
// produces through the credential API (PRD §12): off-allowlist target, a
// verify against a missing upstream secret, a plain member's attempt, and a
// value trying to spoof a reference envelope.
func TestVaultRefErrorSurface(t *testing.T) {
	h := testharness.New(t)
	const resolvedKey = "sk-ant-present"
	mockSM := newMockSecretsManager(t, map[string]string{refARN: resolvedKey})
	cv := testapp.StartWith(t, h, awsRefEnv(mockSM.URL(), refAllowPfx))

	user := cv.LoginAsLocalUser(t)
	_, adminTok := mintToken(t, cv, user.AccessToken, "tf", "instance-admin")

	// (a) Off-allowlist target is rejected at create time, 400, generic —
	//     never contacts the backend.
	resp := cvDo(t, cv, adminTok, "POST", "/api/vault/items", map[string]any{
		"id":        "ref-a",
		"reference": map[string]any{"backend": "aws-sm", "id": "arn:aws:secretsmanager:us-east-1:123456789012:secret:other/db-password"},
	})
	body := readBodyStr(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !bodyHasCodeStr(body, "REF_TARGET_NOT_ALLOWED") {
		t.Fatalf("off-allowlist = %d body=%s, want 400 REF_TARGET_NOT_ALLOWED", resp.StatusCode, body)
	}

	// (b) verify=1 against a missing upstream secret surfaces REF_NOT_FOUND.
	resp = cvDo(t, cv, adminTok, "POST", "/api/vault/items?verify=1", map[string]any{
		"id":        "ref-b",
		"reference": map[string]any{"backend": "aws-sm", "id": refAllowPfx + "does-not-exist"},
	})
	body = readBodyStr(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || !bodyHasCodeStr(body, "REF_NOT_FOUND") {
		t.Fatalf("missing secret = %d body=%s, want 404 REF_NOT_FOUND", resp.StatusCode, body)
	}

	// (c) A plain member (JWT, no admin token) cannot mint a reference.
	resp = cvDo(t, cv, user.AccessToken, "POST", "/api/vault/items", map[string]any{
		"id":        "ref-c",
		"reference": map[string]any{"backend": "aws-sm", "id": refARN},
	})
	body = readBodyStr(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !bodyHasCodeStr(body, "REFERENCE_ADMIN_REQUIRED") {
		t.Fatalf("member reference = %d body=%s, want 403 REFERENCE_ADMIN_REQUIRED", resp.StatusCode, body)
	}

	// (d) A pushed value may not smuggle a reference envelope.
	resp = cvDo(t, cv, user.AccessToken, "POST", "/api/vault/items", map[string]any{
		"id":    "ref-d",
		"value": `{"$clawvisor_ref":1,"backend":"aws-sm","id":"` + refARN + `"}`,
	})
	body = readBodyStr(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("spoofed value = %d body=%s, want 400", resp.StatusCode, body)
	}
}
