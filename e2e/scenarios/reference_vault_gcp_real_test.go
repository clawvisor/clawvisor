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

// TestReferenceVaultGCPRealLLM exercises the FULL reference-vault chain against
// real GCP Secret Manager and real Anthropic:
//
//   1. server boots with the gcp-sm reference backend + a reference allowlist
//   2. an instance-admin token stores a REFERENCE-mode anthropic credential
//      pointing at projects/.../secrets/clawvisor-anthropic-key (no secret value
//      ever touches Clawvisor — only the pointer)
//   3. an agent makes a govern LLM request with NO client key
//   4. the forwarder resolves the reference from real GCP Secret Manager, injects
//      the fetched key, and the request reaches real Anthropic → 200
//
// Gated on CLAWVISOR_ANTHROPIC_E2E_KEY + CLAWVISOR_TEST_GCP_SECRET_NAME (+ ADC).
// Upstream is NOT overridden → real api.anthropic.com.
func TestReferenceVaultGCPRealLLM(t *testing.T) {
	key := os.Getenv("CLAWVISOR_ANTHROPIC_E2E_KEY")
	secret := os.Getenv("CLAWVISOR_TEST_GCP_SECRET_NAME") // projects/{p}/secrets/{s}
	if key == "" || secret == "" {
		t.Skip("set CLAWVISOR_ANTHROPIC_E2E_KEY + CLAWVISOR_TEST_GCP_SECRET_NAME (+ ADC) to run")
	}
	// Allowlist prefix = everything up to the secret short-name's parent.
	allowPrefix := secret
	if i := strings.Index(secret, "/secrets/"); i != -1 {
		allowPrefix = secret[:i+len("/secrets/")]
	}

	const bootstrap = "cvat_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ" // cvat_ + 43

	h := testharness.New(t)
	cv := testapp.StartWith(t, h, map[string]string{
		"VAULT_REFERENCE_ALLOWLIST": allowPrefix, // permit the GCP SM secret
		"CLAWVISOR_BOOTSTRAP_TOKEN": bootstrap,    // seed → mint an instance-admin token
		// govern (vault) posture is the testapp default; real Anthropic upstream.
	})

	post := func(tok, path string, body any, out any) int {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", cv.URL+path, bytes.NewReader(b))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := cv.Client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		defer resp.Body.Close()
		if out != nil && resp.StatusCode/100 == 2 {
			json.NewDecoder(resp.Body).Decode(out)
		}
		return resp.StatusCode
	}

	// 1) Mint an instance-admin token from the bootstrap (burns the bootstrap).
	var minted struct {
		Token string `json:"token"`
	}
	if st := post(bootstrap, "/api/tokens", map[string]any{"name": "ref-admin", "scope": "instance-admin"}, &minted); st != 200 && st != 201 {
		t.Fatalf("mint instance-admin token: status=%d", st)
	}
	if minted.Token == "" {
		t.Fatal("mint returned empty token")
	}

	// 2) Store a REFERENCE-mode anthropic credential (instance-admin, allowlisted).
	putRef := func() int {
		b, _ := json.Marshal(map[string]any{
			"reference": map[string]any{"backend": "gcp-sm", "id": secret},
		})
		req, _ := http.NewRequest("PUT", cv.URL+"/api/runtime/llm-credentials/anthropic", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+minted.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := cv.Client.Do(req)
		if err != nil {
			t.Fatalf("put reference: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("put reference: status=%d body=%s", resp.StatusCode, body)
		}
		return resp.StatusCode
	}
	putRef()

	// 3) Create an agent (as the local user).
	user := cv.LoginAsLocalUser(t)
	var agent struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if st := post(user.AccessToken, "/api/agents", map[string]any{"name": "ref-agent"}, &agent); st/100 != 2 {
		t.Fatalf("create agent: status=%d", st)
	}

	// 4) Govern LLM request, NO client key → reference resolved from real GCP SM
	//    and injected → real Anthropic.
	body := `{"model":"claude-haiku-4-5-20251001","max_tokens":16,"messages":[{"role":"user","content":"Reply with exactly: OK"}]}`
	req, _ := http.NewRequest("POST", cv.URL+"/api/v1/messages", strings.NewReader(body))
	req.Header.Set("X-Clawvisor-Agent-Token", agent.Token)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	resp, err := cv.Client.Do(req)
	if err != nil {
		t.Fatalf("llm request: %v", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("llm request status=%d body=%s (want 200 — reference should resolve from GCP SM)", resp.StatusCode, rb)
	}
	if !strings.Contains(string(rb), `"type":"message"`) || !strings.Contains(string(rb), `"content"`) {
		t.Fatalf("expected a real Anthropic completion via GCP-SM-resolved key, got: %s", rb)
	}
	t.Logf("EVIDENCE: govern request with a gcp-sm reference resolved from real Secret Manager → real completion: %.180s", rb)
}
