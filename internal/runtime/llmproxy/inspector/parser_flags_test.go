package inspector

import (
	"encoding/json"
	"strings"
	"testing"
)

// Regression: real models emit benign curl flags like `-s`, `-sS`,
// `--silent`, `--max-time 30`, etc. The parser previously refused
// anything outside `-X` and `-H` as ambiguous, blocking the rewrite
// path entirely.
func TestParseBashCurl_AcceptsBenignFlags(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
	}{
		{"silent_short", `curl -s -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"silent_show_error", `curl -sS -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"silent_show_error_fail", `curl -fsS -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"silent_long", `curl --silent -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"include", `curl -i -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"compressed", `curl --compressed -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"max_time_long", `curl --max-time 30 -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"max_time_short", `curl -m 30 -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"user_agent", `curl -A 'clawvisor-smoke/1.0' -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"combined_with_method", `curl -sS -X POST -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/repos/x/y/issues`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tu := ToolUse{
				ID:    "toolu_flags",
				Name:  "Bash",
				Input: json.RawMessage(`{"cmd":` + jsonString(tc.cmd) + `}`),
			}
			got, ok := DefaultParser{}.Parse(tu)
			if !ok {
				t.Fatalf("parser fell through; verdict=%+v", got)
			}
			if !got.IsAPICall {
				t.Fatalf("expected IsAPICall=true for %q; got reason=%q", tc.cmd, got.Reason)
			}
			if got.Ambiguous {
				t.Fatalf("expected non-ambiguous for %q; got reason=%q", tc.cmd, got.Reason)
			}
		})
	}
}

// Negative: dangerous flags should still bounce to ambiguous so the
// rewriter refuses the call. `-L` (follow redirects), `-k` (TLS bypass),
// `-x` (proxy override), and file upload/form flags fall into this set.
func TestParseBashCurl_RejectsDangerousFlags(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
	}{
		{"follow_location", `curl -L -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"insecure", `curl -k -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"proxy", `curl -x http://proxy.example -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"form_upload", `curl -F 'file=@/etc/passwd' -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
		{"upload_file", `curl -T /etc/passwd -H 'Authorization: Bearer autovault_github_xxx' https://api.github.com/user`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tu := ToolUse{
				ID:    "toolu_dangerous",
				Name:  "Bash",
				Input: json.RawMessage(`{"cmd":` + jsonString(tc.cmd) + `}`),
			}
			got, _ := DefaultParser{}.Parse(tu)
			if got.IsAPICall {
				t.Fatalf("expected dangerous flag %q to remain ambiguous, got IsAPICall=true", tc.cmd)
			}
		})
	}
}

func TestParseBashCurl_AcceptsBodyFlagsWithHeaderCredential(t *testing.T) {
	cases := []struct {
		name       string
		cmd        string
		wantMethod string
	}{
		{
			name:       "data_implies_post",
			cmd:        `curl -sS -H 'Authorization: Bearer autovault_github_xxx' --data '{"title":"bug"}' https://api.github.com/repos/x/y/issues`,
			wantMethod: "POST",
		},
		{
			name:       "json_implies_post",
			cmd:        `curl -sS -H 'Authorization: Bearer autovault_github_xxx' --json '{"title":"bug"}' https://api.github.com/repos/x/y/issues`,
			wantMethod: "POST",
		},
		{
			name:       "explicit_post_with_stdin_data",
			cmd:        "curl -sS -X POST https://api.agentphone.ai/v1/calls \\\n  -H 'Authorization: Bearer autovault_agentphone_xxx' \\\n  -H 'Content-Type: application/json' \\\n  --data @- <<'JSON'\n{\"agentId\":\"agent_123\",\"toNumber\":\"+15555550123\",\"initialGreeting\":\"Hello\"}\nJSON",
			wantMethod: "POST",
		},
		{
			name:       "get_keeps_data_in_query",
			cmd:        `curl -G -sS -H 'Authorization: Bearer autovault_github_xxx' --data-urlencode 'q=repo:clawvisor/clawvisor is:pr' https://api.github.com/search/issues`,
			wantMethod: "GET",
		},
		{
			name:       "get_after_data_still_keeps_get",
			cmd:        `curl -sS -H 'Authorization: Bearer autovault_github_xxx' --data-urlencode 'q=repo:clawvisor/clawvisor is:pr' --get https://api.github.com/search/issues`,
			wantMethod: "GET",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tu := ToolUse{
				ID:    "toolu_body",
				Name:  "Bash",
				Input: json.RawMessage(`{"command":` + jsonString(tc.cmd) + `}`),
			}
			got, ok := DefaultParser{}.Parse(tu)
			if !ok {
				t.Fatalf("parser fell through; verdict=%+v", got)
			}
			if got.Ambiguous || !got.IsAPICall {
				t.Fatalf("expected body curl to parse as non-ambiguous API call, got %+v", got)
			}
			if got.Method != tc.wantMethod {
				t.Fatalf("method=%q, want %q", got.Method, tc.wantMethod)
			}
			if len(got.Placeholders) != 1 {
				t.Fatalf("expected one header placeholder, got %+v", got.Placeholders)
			}
		})
	}
}

func TestParseBashCurl_RejectsBodyPlaceholder(t *testing.T) {
	tu := ToolUse{
		ID:    "toolu_body_placeholder",
		Name:  "Bash",
		Input: json.RawMessage(`{"command":"curl -sS -H 'Authorization: Bearer autovault_github_header' --data '{\"token\":\"autovault_github_body\"}' https://api.github.com/repos/x/y/issues"}`),
	}
	got, ok := DefaultParser{}.Parse(tu)
	if !ok {
		t.Fatalf("expected parser to claim body-placeholder input")
	}
	if !got.Ambiguous {
		t.Fatalf("expected body placeholder to be ambiguous, got %+v", got)
	}
	if !strings.Contains(got.Reason, "placeholder not in -H header") {
		t.Fatalf("unexpected reason: %q", got.Reason)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// isSafeBoolCurlFlag's short-flag cluster handling must only accept
// clusters where every character is in the safe-short set. A single
// letter outside the set in a cluster must reject the whole token.
func TestIsSafeBoolCurlFlag_ShortFlagClusters(t *testing.T) {
	cases := map[string]bool{
		"-s":   true,
		"-sS":  true,
		"-fsS": true,
		"-sSf": true,
		"-Lf":  false, // -L (location) is refused, so the cluster is refused
		"-sk":  false, // -k (insecure) is refused
		"-d":   false, // -d alone isn't in the bool set
	}
	for tok, want := range cases {
		got := isSafeBoolCurlFlag(tok)
		if got != want {
			t.Errorf("isSafeBoolCurlFlag(%q) = %v, want %v", tok, got, want)
		}
	}
}

// Sanity: the example error the user hit (`bash: unknown curl flag -s`)
// no longer fires for `curl -s`.
func TestParseBashCurl_DashSNoLongerAmbiguous(t *testing.T) {
	tu := ToolUse{
		ID:   "toolu_dash_s",
		Name: "Bash",
		Input: json.RawMessage(
			`{"cmd":"curl -s -H 'Authorization: Bearer autovault_github_abc' https://api.github.com/user"}`,
		),
	}
	got, ok := DefaultParser{}.Parse(tu)
	if !ok || got.Ambiguous {
		t.Fatalf("expected -s to be accepted; got ambiguous=%v reason=%q", got.Ambiguous, got.Reason)
	}
	if strings.Contains(got.Reason, "unknown curl flag") {
		t.Errorf("reason still mentions unknown curl flag: %q", got.Reason)
	}
}

// Regression: Claude Code commonly formats curl across multiple lines
// with a shell line-continuation before later flags. The parser must
// treat backslash-newline as whitespace, not as a second positional
// argument, so task-scoped credential calls can be mediated.
func TestParseBashCurl_AcceptsLineContinuationBeforeHeader(t *testing.T) {
	tu := ToolUse{
		ID:    "toolu_line_continuation",
		Name:  "Bash",
		Input: json.RawMessage(`{"command":"curl -sS https://api.agentphone.ai/v1/agents \\\n  -H \"Authorization: Bearer autovault_agentphone_0bTLIJkoUqI5-BUxlEx1W-sVyO0ekM3j\""}`),
	}

	got, ok := DefaultParser{}.Parse(tu)
	if !ok {
		t.Fatalf("parser fell through; verdict=%+v", got)
	}
	if got.Ambiguous || !got.IsAPICall {
		t.Fatalf("expected multiline curl to parse as non-ambiguous API call, got %+v", got)
	}
	if got.Host != "api.agentphone.ai" {
		t.Fatalf("host=%q, want api.agentphone.ai", got.Host)
	}
	if got.Path != "/v1/agents" {
		t.Fatalf("path=%q, want /v1/agents", got.Path)
	}
	if len(got.Placeholders) != 1 || got.Placeholders[0] != "autovault_agentphone_0bTLIJkoUqI5-BUxlEx1W-sVyO0ekM3j" {
		t.Fatalf("unexpected placeholders: %+v", got.Placeholders)
	}
}

// Tokenizer-rejected verdicts must surface as AgentRecoverable so the
// chain emits RecoverableDenyVerdict (one-shot continuation retry) instead
// of falling through to OutcomeHold and stalling on human approval. The
// fix is always the same — re-emit the curl with a `--data @-` heredoc —
// so the model can self-correct without a user round-trip.
//
// Repro: mvdan/sh parses `\'` as an escaped apostrophe, so the credentialed
// segment extracts cleanly; the minimal in-process tokenizer doesn't model
// backslash escapes and sees an unbalanced quote.
func TestParseBashCurl_TokenizerRejectIsAgentRecoverable(t *testing.T) {
	cmd := `curl -H 'Authorization: Bearer autovault_github_TESTAAAAAAAAAAAA' -d O\'Brien https://api.github.com/foo`
	tu := ToolUse{
		ID:    "toolu_tokenizer_reject",
		Name:  "Bash",
		Input: json.RawMessage(`{"cmd":` + jsonString(cmd) + `}`),
	}
	got, ok := DefaultParser{}.Parse(tu)
	if !ok {
		t.Fatalf("parser fell through; want an Ambiguous verdict, got %+v", got)
	}
	if !got.Ambiguous || got.IsAPICall {
		t.Fatalf("expected Ambiguous && !IsAPICall, got %+v", got)
	}
	if !got.AgentRecoverable {
		t.Fatalf("expected AgentRecoverable=true so chain emits RecoverableDenyVerdict; got %+v", got)
	}
	if !strings.Contains(got.Reason, "tokenizer rejected input") {
		t.Fatalf("reason=%q; want it to mention tokenizer rejection", got.Reason)
	}
}

// Every Ambiguous parser refusal that names a deterministic shape
// problem must surface as AgentRecoverable so InspectorChain emits
// RecoverableDenyVerdict (one-shot continuation retry) instead of
// falling through to OutcomeHold and stalling on human approval. This
// pins the contract for the bash-curl branches and the structured-
// fetch placeholder-location branch. A failure here means the model
// gets a user-approval prompt for a problem it could have self-fixed.
//
// Note: the tokenizer-rejected and extractCredentialedCurlSegment
// branches are pinned by their own dedicated tests above and in
// inspector_test.go; they're not duplicated here.
func TestParseBashCurl_AmbiguousSitesAreAgentRecoverable(t *testing.T) {
	const cred = `autovault_github_AAAAAAAAAAAAAAAA`
	authHeader := `'Authorization: Bearer ` + cred + `'`
	bash := func(cmd string) ToolUse {
		return ToolUse{
			ID:    "toolu_ambig_recover",
			Name:  "Bash",
			Input: json.RawMessage(`{"cmd":` + jsonString(cmd) + `}`),
		}
	}
	cases := []struct {
		name        string
		tu          ToolUse
		wantReason  string
	}{
		{
			name:       "bash_trailing_dash_X",
			tu:         bash(`curl -H ` + authHeader + ` https://api.github.com/ -X`),
			wantReason: "-X without value",
		},
		{
			name:       "bash_trailing_dash_H",
			tu:         bash(`curl -H ` + authHeader + ` https://api.github.com/ -H`),
			wantReason: "-H without value",
		},
		{
			name:       "bash_safe_value_flag_trailing",
			tu:         bash(`curl -H ` + authHeader + ` https://api.github.com/ -A`),
			wantReason: "-A without value",
		},
		{
			name:       "bash_body_flag_trailing",
			tu:         bash(`curl -H ` + authHeader + ` https://api.github.com/ -d`),
			wantReason: "-d without value",
		},
		{
			name:       "bash_placeholder_in_body",
			tu:         bash(`curl -d 'token=` + cred + `' https://api.github.com/`),
			wantReason: "placeholder not in -H header",
		},
		{
			name:       "bash_unknown_flag",
			tu:         bash(`curl -L -H ` + authHeader + ` https://api.github.com/`),
			wantReason: "unknown curl flag -L",
		},
		{
			name:       "bash_extra_positional",
			tu:         bash(`curl -H ` + authHeader + ` https://api.github.com/ https://api.github.com/extra`),
			wantReason: "expected exactly one positional URL",
		},
		{
			name:       "bash_positional_not_a_url",
			tu:         bash(`curl -H ` + authHeader + ` not_a_url`),
			wantReason: "positional is not a URL",
		},
		{
			name:       "bash_non_http_url",
			tu:         bash(`curl -H ` + authHeader + ` ftp://example.com/file`),
			wantReason: "non-http URL",
		},
		{
			name: "structured_fetch_placeholder_in_body",
			tu: ToolUse{
				ID:    "toolu_ambig_fetch",
				Name:  "WebFetch",
				Input: json.RawMessage(`{"url":"https://api.github.com/","method":"POST","body":"token=` + cred + `"}`),
			},
			wantReason: "placeholder not in known header credential location",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := DefaultParser{}.Parse(tc.tu)
			if !ok {
				t.Fatalf("parser fell through; want Ambiguous verdict, got %+v", got)
			}
			if !got.Ambiguous {
				t.Fatalf("expected Ambiguous=true; got %+v", got)
			}
			if !got.AgentRecoverable {
				t.Fatalf("expected AgentRecoverable=true so the chain emits RecoverableDenyVerdict; got %+v", got)
			}
			if got.IsAPICall {
				t.Fatalf("expected IsAPICall=false for refusal verdict; got %+v", got)
			}
			if !strings.Contains(got.Reason, tc.wantReason) {
				t.Fatalf("reason = %q; want it to contain %q", got.Reason, tc.wantReason)
			}
		})
	}
}

func TestOpenClawReadOnlyToolsAreLocalOnlyDefaults(t *testing.T) {
	for _, name := range []string{
		"memory_get",
		"memory_search",
		"session_status",
		"sessions_history",
		"sessions_list",
		"sessions_yield",
	} {
		if !IsLocalOnlyTool(name) {
			t.Fatalf("%s should be treated as local-only", name)
		}
		if !IsDefaultAllowedTool(name) {
			t.Fatalf("%s should be seeded as a default allowed tool", name)
		}
	}
	for _, name := range []string{
		"edit",
		"exec",
		"image",
		"process",
		"sessions_send",
		"sessions_spawn",
		"subagents",
		"web_fetch",
		"write",
	} {
		if IsDefaultAllowedTool(name) {
			t.Fatalf("%s should not be seeded as a default allowed tool", name)
		}
	}
}

func TestHermesReadOnlyToolsAreLocalOnlyDefaults(t *testing.T) {
	for _, name := range []string{
		"browser_console",
		"browser_get_images",
		"browser_snapshot",
		"clarify",
		"read_file",
		"search_files",
		"session_search",
		"skill_view",
		"skills_list",
	} {
		if !IsLocalOnlyTool(name) {
			t.Fatalf("%s should be treated as local-only", name)
		}
		if !IsDefaultAllowedTool(name) {
			t.Fatalf("%s should be seeded as a default allowed tool", name)
		}
	}
	for _, name := range []string{
		"browser_back",
		"browser_click",
		"browser_navigate",
		"browser_press",
		"browser_scroll",
		"browser_type",
		"browser_vision",
		"cronjob",
		"delegate_task",
		"execute_code",
		"image_generate",
		"memory",
		"patch",
		"process",
		"send_message",
		"skill_manage",
		"terminal",
		"text_to_speech",
		"vision_analyze",
		"write_file",
	} {
		if IsDefaultAllowedTool(name) {
			t.Fatalf("%s should not be seeded as a default allowed tool", name)
		}
	}
}

func TestCodexInternalToolsAreLocalOnlyDefaults(t *testing.T) {
	for _, name := range []string{
		"read_file",
		"view_image",
		"update_plan",
		"request_user_input",
		"tool_suggest",
		"wait_agent",
		"close_agent",
		"resume_agent",
	} {
		if !IsLocalOnlyTool(name) {
			t.Fatalf("%s should be treated as local-only", name)
		}
		if !IsDefaultAllowedTool(name) {
			t.Fatalf("%s should be seeded as a default allowed tool", name)
		}
	}
	for _, name := range []string{
		"list_mcp_resources",
		"list_mcp_resource_templates",
		"read_mcp_resource",
	} {
		if !IsDefaultAllowedTool(name) {
			t.Fatalf("%s should be seeded as a default allowed tool", name)
		}
	}
	for _, name := range []string{
		"exec_command",
		"write_stdin",
		"apply_patch",
		"spawn_agent",
		"send_input",
	} {
		if IsDefaultAllowedTool(name) {
			t.Fatalf("%s should not be seeded as a default allowed tool", name)
		}
	}
}

// Regression: a placeholder substring inside a local-only tool's args
// (Skill, Read, Edit, etc.) must pass through without engaging the
// LLM validator. Otherwise smoke-test installs without an LLM-backed
// validator see "ambiguous credentialed call refused" for tools that
// never make outbound HTTP calls.
func TestParser_LocalOnlyToolsPassThrough(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args string
	}{
		{"skill_with_placeholder_arg", "Skill", `{"skill":"clawvisor","args":"use autovault_github_xxx for the call"}`},
		{"read_file_with_placeholder_path", "Read", `{"file_path":"/tmp/autovault_github_xxx.json"}`},
		{"todo_write_with_placeholder", "TodoWrite", `{"todos":[{"content":"call api with autovault_github_xxx","activeForm":"calling api"}]}`},
		{"glob_with_placeholder_pattern", "Glob", `{"pattern":"autovault_github_xxx*.json"}`},
		// Codex's read_file is treated the same as Claude Code's Read.
		{"codex_read_file", "read_file", `{"path":"/tmp/autovault_github_xxx.json"}`},
		// OpenClaw's read tool is lowercase but should be treated as a local file read.
		{"openclaw_read", "read", `{"path":"/tmp/autovault_github_xxx.json"}`},
		{"ask_user_question", "AskUserQuestion", `{"question":"Use autovault_github_xxx for this task?","options":["yes","no"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tu := ToolUse{ID: "toolu_local", Name: tc.tool, Input: json.RawMessage(tc.args)}
			got, ok := DefaultParser{}.Parse(tu)
			if !ok {
				t.Fatalf("parser should claim local-only tool %q, fell through", tc.tool)
			}
			if got.IsAPICall {
				t.Errorf("local-only tool %q must not be IsAPICall=true", tc.tool)
			}
			if got.Ambiguous {
				t.Errorf("local-only tool %q must not be ambiguous; got reason=%q", tc.tool, got.Reason)
			}
		})
	}
}

// Sanity: known HTTP-shaped tools (WebFetch, Bash with curl) are NOT
// considered local-only — they still flow through the normal parser
// branch.
func TestParser_HTTPToolsNotInLocalAllowlist(t *testing.T) {
	if isLocalOnlyTool("WebFetch") {
		t.Errorf("WebFetch must not be in local-only allowlist")
	}
	if isLocalOnlyTool("Bash") {
		t.Errorf("Bash must not be in local-only allowlist (it can run curl)")
	}
	if isLocalOnlyTool("fetch") {
		t.Errorf("fetch must not be in local-only allowlist")
	}
}
