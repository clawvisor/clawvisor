package scenarios_test

import (
	"net/http"
	"testing"

	"github.com/clawvisor/clawvisor/e2e/testapp"
	"github.com/clawvisor/clawvisor/internal/auth"
	"github.com/clawvisor/clawvisor/testharness"
)

// genAPIToken mints a canonically-formatted cvat_ value for use as a
// module-seeded bootstrap token (spec 03 generates the identical shape).
func genAPIToken(t *testing.T) string {
	t.Helper()
	raw, _, err := auth.GenerateAPIToken()
	if err != nil {
		t.Fatalf("GenerateAPIToken: %v", err)
	}
	return raw
}

// mintToken POSTs /api/tokens with the given auth token and returns the
// created plaintext value.
func mintToken(t *testing.T, cv *testapp.Server, authTok, name, scope string) (id, plaintext string) {
	t.Helper()
	var out struct {
		ID    string `json:"id"`
		Token string `json:"token"`
		Scope string `json:"scope"`
	}
	body := map[string]any{"name": name}
	if scope != "" {
		body["scope"] = scope
	}
	cvPost(t, cv, authTok, "/api/tokens", body, &out)
	if out.Token == "" || out.ID == "" {
		t.Fatalf("mint returned empty token/id: %+v", out)
	}
	if !auth.ValidAPITokenFormat(out.Token) {
		t.Fatalf("minted token %q fails canonical format", out.Token)
	}
	return out.ID, out.Token
}

// TestAPIToken_MintListRevoke drives the full lifecycle over real HTTP:
// mint (plaintext only on create) → list (prefix, never plaintext/hash) →
// use → revoke → revoked token is rejected 401 TOKEN_REVOKED.
func TestAPIToken_MintListRevoke(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	user := cv.LoginAsLocalUser(t)

	id, tok := mintToken(t, cv, user.AccessToken, "ci", "instance-admin")

	// List via JWT — must show the prefix and never the plaintext or hash.
	var list struct {
		Tokens []map[string]any `json:"tokens"`
	}
	cvGet(t, cv, user.AccessToken, "/api/tokens", &list)
	if len(list.Tokens) != 1 {
		t.Fatalf("list len = %d, want 1", len(list.Tokens))
	}
	row := list.Tokens[0]
	if row["token_prefix"] == nil || row["token_prefix"].(string) == "" {
		t.Fatalf("list row missing token_prefix: %+v", row)
	}
	if _, ok := row["token"]; ok {
		t.Fatal("list must never include plaintext token")
	}
	if _, ok := row["token_hash"]; ok {
		t.Fatal("list must never include token_hash")
	}

	// The token authenticates a wrapped route.
	resp := cvDo(t, cv, tok, "GET", "/api/agents", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token GET /api/agents = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Revoke, then the token is rejected on the next request.
	resp = cvDo(t, cv, user.AccessToken, "DELETE", "/api/tokens/"+id, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /api/tokens = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	resp = cvDo(t, cv, tok, "GET", "/api/agents", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token GET = %d, want 401", resp.StatusCode)
	}
	if !bodyHasCodeStr(readBodyStr(resp), "TOKEN_REVOKED") {
		t.Fatal("revoked token should return TOKEN_REVOKED")
	}
}

// TestAPIToken_AdminGateByScope: an instance-admin token reaches the
// admin-gated token-management route (POST /api/tokens) even though it
// authenticates as `_instance` (role member in spec 04) — scope satisfies
// the gate for token auth.
func TestAPIToken_AdminGateByScope(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	user := cv.LoginAsLocalUser(t)

	_, adminTok := mintToken(t, cv, user.AccessToken, "admin", "instance-admin")

	// Use the instance-admin token to mint ANOTHER token — an admin op.
	var out struct {
		Token string `json:"token"`
	}
	cvPost(t, cv, adminTok, "/api/tokens", map[string]any{"name": "second", "scope": "instance-admin"}, &out)
	if out.Token == "" {
		t.Fatal("instance-admin token could not reach the admin-gated mint route")
	}
}

// TestAPIToken_MintScopeSplit: spec 04 lands the trust split, so POST
// /api/tokens must mint config-read and config-write tokens (not just
// instance-admin) — otherwise the userOrTokenRead/userOrToken route gates
// that already accept those scopes are inert. A bogus scope is still 400.
func TestAPIToken_MintScopeSplit(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	user := cv.LoginAsLocalUser(t)

	for _, scope := range []string{"config-read", "config-write", "instance-admin"} {
		var out struct {
			Token string `json:"token"`
			Scope string `json:"scope"`
		}
		cvPost(t, cv, user.AccessToken, "/api/tokens", map[string]any{"name": scope + "-tok", "scope": scope}, &out)
		if out.Token == "" || out.Scope != scope {
			t.Fatalf("mint scope=%q: token=%q returned scope=%q", scope, out.Token, out.Scope)
		}
	}

	// An unknown scope is rejected with 400 INVALID_SCOPE.
	resp := cvDo(t, cv, user.AccessToken, "POST", "/api/tokens", map[string]any{"name": "bad", "scope": "root"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !bodyHasCodeStr(readBodyStr(resp), "INVALID_SCOPE") {
		t.Fatalf("bogus scope: status=%d want 400 INVALID_SCOPE", resp.StatusCode)
	}
}

// TestAPIToken_Attribution: an agent created via an API token is owned by
// `_instance`, not by the JWT user who minted the token. It is therefore
// visible when listing under the token (which resolves to `_instance`) but
// NOT under the human's own JWT account.
func TestAPIToken_Attribution(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	user := cv.LoginAsLocalUser(t)
	_, tok := mintToken(t, cv, user.AccessToken, "tf", "instance-admin")

	// Create an agent using the token.
	var created struct {
		ID string `json:"id"`
	}
	cvPost(t, cv, tok, "/api/agents", map[string]any{"name": "headless-agent"}, &created)
	if created.ID == "" {
		t.Fatal("agent create via token returned no id")
	}

	// Visible under the token (owned by _instance).
	var viaToken []map[string]any
	cvGet(t, cv, tok, "/api/agents", &viaToken)
	if !agentListed(viaToken, created.ID) {
		t.Fatalf("agent %s not visible under token auth", created.ID)
	}

	// NOT visible under the human JWT account (different owner).
	var viaJWT []map[string]any
	cvGet(t, cv, user.AccessToken, "/api/agents", &viaJWT)
	if agentListed(viaJWT, created.ID) {
		t.Fatal("token-created agent must not appear in the human's personal account")
	}
}

// TestAPIToken_JWTFallthrough: a normal JWT request on a token-wrapped
// route behaves exactly as before (no cvat_ token presented).
func TestAPIToken_JWTFallthrough(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	user := cv.LoginAsLocalUser(t)

	resp := cvDo(t, cv, user.AccessToken, "GET", "/api/agents", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("JWT GET /api/agents = %d, want 200", resp.StatusCode)
	}

	// A missing/invalid credential is still rejected.
	resp2 := cvDo(t, cv, "", "GET", "/api/agents", nil)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-auth GET /api/agents = %d, want 401", resp2.StatusCode)
	}
}

// TestAPIToken_ExpiredRejected: a token whose expires_at has passed is
// rejected 401 TOKEN_EXPIRED.
func TestAPIToken_ExpiredRejected(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	user := cv.LoginAsLocalUser(t)

	var out struct {
		Token string `json:"token"`
	}
	// Mint with an already-past expiry.
	cvPost(t, cv, user.AccessToken, "/api/tokens", map[string]any{
		"name":       "expired",
		"scope":      "instance-admin",
		"expires_at": "2000-01-01T00:00:00Z",
	}, &out)

	resp := cvDo(t, cv, out.Token, "GET", "/api/agents", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !bodyHasCodeStr(readBodyStr(resp), "TOKEN_EXPIRED") {
		t.Fatalf("expired token GET = %d, want 401 TOKEN_EXPIRED", resp.StatusCode)
	}
}

// TestBootstrap_BurnsOnFirstMint: boot with a module-shaped
// CLAWVISOR_BOOTSTRAP_TOKEN, with no human login ever having occurred.
//  1. A read call does NOT burn the bootstrap token (retryable apply).
//  2. The first scoped-token mint burns it (second use → 401 TOKEN_REVOKED).
//  3. The minted token drives the headless flow: create an agent + a vault
//     credential and read them back (acceptance criterion #1).
func TestBootstrap_BurnsOnFirstMint(t *testing.T) {
	h := testharness.New(t)
	bootstrap := genAPIToken(t)
	cv := testapp.StartWith(t, h, map[string]string{"CLAWVISOR_BOOTSTRAP_TOKEN": bootstrap})

	// (1) A read does not burn the bootstrap token.
	resp := cvDo(t, cv, bootstrap, "GET", "/api/agents", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bootstrap read GET /api/agents = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// (2) First scoped-token mint burns the bootstrap credential.
	_, longLived := mintToken(t, cv, bootstrap, "terraform", "instance-admin")

	resp = cvDo(t, cv, bootstrap, "GET", "/api/agents", nil)
	burnedBody := readBodyStr(resp)
	if resp.StatusCode != http.StatusUnauthorized || !bodyHasCodeStr(burnedBody, "TOKEN_REVOKED") {
		t.Fatalf("bootstrap token after mint = %d body=%q, want 401 TOKEN_REVOKED", resp.StatusCode, burnedBody)
	}

	// (3) Headless flow with the long-lived token: create an agent...
	var agent struct {
		ID string `json:"id"`
	}
	cvPost(t, cv, longLived, "/api/agents", map[string]any{"name": "provisioned"}, &agent)
	if agent.ID == "" {
		t.Fatal("headless agent create failed")
	}
	// ...set a vault credential...
	cvPost(t, cv, longLived, "/api/vault/items", map[string]any{"id": "deploy-key", "value": "s3cr3t"}, nil)
	// ...and read them back.
	var agents []map[string]any
	cvGet(t, cv, longLived, "/api/agents", &agents)
	if !agentListed(agents, agent.ID) {
		t.Fatal("provisioned agent not readable back headlessly")
	}
	var items struct {
		Entries []map[string]any `json:"entries"`
	}
	cvGet(t, cv, longLived, "/api/vault/items", &items)
	if !vaultItemListed(items.Entries, "deploy-key") {
		t.Fatalf("vault credential not readable back headlessly: %+v", items.Entries)
	}
}

// TestAPIToken_FeatureAdvertised: GET /api/features advertises
// api_tokens:true so the Terraform provider can capability-negotiate
// (PRD §9.1, acceptance criterion #6).
func TestAPIToken_FeatureAdvertised(t *testing.T) {
	h := testharness.New(t)
	cv := testapp.Start(t, h)
	var fs map[string]any
	cvGet(t, cv, "", "/api/features", &fs)
	if v, ok := fs["api_tokens"].(bool); !ok || !v {
		t.Fatalf("expected api_tokens:true in /api/features, got %v", fs["api_tokens"])
	}
}

func agentListed(list []map[string]any, id string) bool {
	for _, a := range list {
		if a["id"] == id {
			return true
		}
	}
	return false
}

func vaultItemListed(list []map[string]any, id string) bool {
	for _, it := range list {
		if it["id"] == id {
			return true
		}
	}
	return false
}

func bodyHasCodeStr(body, code string) bool {
	return len(body) > 0 && (containsSub(body, `"code":"`+code+`"`) || containsSub(body, `"code": "`+code+`"`))
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
