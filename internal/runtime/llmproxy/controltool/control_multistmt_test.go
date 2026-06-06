package controltool

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// The agent observed in production prefers a two-statement shape:
//
//	cat <<'EOF' >/tmp/clawvisor-task.json
//	{...}
//	EOF
//	curl ... --data @/tmp/clawvisor-task.json
//
// Without multi-stmt support the parser would refuse, the control-tool
// branch in postprocess would miss, and the model would see a generic
// tool-approval prompt instead of either the dashboard task flow or
// the inline approval flow.
const multiStmtCatCurlCmd = `cat <<'EOF' >/tmp/clawvisor-task.json
{"purpose":"Build a landing page","intent_verification_mode":"strict","expires_in_seconds":600,"expected_tools":[{"tool_name":"Bash","why":"Create dir"}]}
EOF
curl -sS -X POST 'https://clawvisor.local/control/tasks?wait=true&timeout=120' -H 'Content-Type: application/json' --data @/tmp/clawvisor-task.json`

// The canonical shape the proxy's prompt teaches:
//
//	curl ... --data @- <<'JSON'
//	{...}
//	JSON
//
// Single statement, body in the stdin heredoc. Earlier code returned
// `@-` as the literal body and the inline intercept failed at JSON
// parse with `invalid character '@'`. This is the regression test for
// that production bug.
const singleStmtCurlStdinHeredoc = `curl -sS -X POST 'https://clawvisor.local/control/tasks?wait=true&timeout=120&surface=inline' -H 'Content-Type: application/json' --data @- <<'JSON'
{"purpose":"Build a landing page","expected_tools":[{"tool_name":"bash","why":"x"}]}
JSON`

func TestParseControlCmd_ResolvesDataAtDashFromStdinHeredoc(t *testing.T) {
	_, dataFiles, ok := parseControlCmd(singleStmtCurlStdinHeredoc)
	if !ok {
		t.Fatalf("expected parser to accept canonical curl --data @- <<JSON shape")
	}
	body, ok := dataFiles["-"]
	if !ok {
		t.Fatalf("expected stdin heredoc registered under '-'; got dataFiles=%v", dataFiles)
	}
	if !strings.Contains(string(body), `"purpose":"Build a landing page"`) {
		t.Fatalf("stdin heredoc body lost original content: %s", body)
	}
}

func TestControlPartsFromCommandInput_ResolvesDataAtDashFromHeredoc(t *testing.T) {
	in, _ := json.Marshal(map[string]string{"cmd": singleStmtCurlStdinHeredoc})
	u, method, body, ok := controlPartsFromCommandInput(json.RawMessage(in), "")
	if !ok {
		t.Fatalf("expected controlPartsFromCommandInput to succeed on canonical shape")
	}
	if method != "POST" || u == nil {
		t.Fatalf("method=%q u=%v, want POST + parsed URL", method, u)
	}
	if !strings.Contains(string(body), `"purpose":"Build a landing page"`) {
		t.Fatalf("body should be the heredoc content, not '@-'; got %s", body)
	}
	if string(body) == "@-" {
		t.Fatal("regression: body extraction returned literal '@-' instead of heredoc body")
	}
}

func TestParseControlToolUse_BodylessTasksCurlIsGET(t *testing.T) {
	tu := conversation.ToolUse{
		ID:    "tu_1",
		Name:  "Bash",
		Input: json.RawMessage(`{"command":"curl -sS https://clawvisor.local/control/tasks"}`),
	}

	call, ok := ParseControlToolUse(tu)
	if !ok {
		t.Fatal("expected control call to parse")
	}
	if call.Method != "GET" {
		t.Fatalf("method=%q, want GET for bodyless task list curl", call.Method)
	}
	if call.Verdict.Method != "GET" {
		t.Fatalf("verdict method=%q, want GET", call.Verdict.Method)
	}
}

func TestParseControlToolUse_TasksCurlWithBodyIsPOST(t *testing.T) {
	tu := conversation.ToolUse{
		ID:    "tu_1",
		Name:  "Bash",
		Input: json.RawMessage(`{"command":"curl -sS https://clawvisor.local/control/tasks --data '{\"purpose\":\"x\"}'"}`),
	}

	call, ok := ParseControlToolUse(tu)
	if !ok {
		t.Fatal("expected control call to parse")
	}
	if call.Method != "POST" {
		t.Fatalf("method=%q, want POST for task creation curl with body", call.Method)
	}
	if call.Verdict.Method != "POST" {
		t.Fatalf("verdict method=%q, want POST", call.Verdict.Method)
	}
}

func TestParseControlCmd_MultiStmtCatHeredocPlusCurl(t *testing.T) {
	args, dataFiles, ok := parseControlCmd(multiStmtCatCurlCmd)
	if !ok {
		t.Fatalf("expected parseControlCmd to accept cat+curl multi-statement")
	}
	if len(args) == 0 || args[0].value != "curl" {
		t.Fatalf("expected curl as the curl stmt's args[0]; got %+v", args)
	}
	body, ok := dataFiles["/tmp/clawvisor-task.json"]
	if !ok {
		t.Fatalf("expected /tmp/clawvisor-task.json in dataFiles; got %v", dataFiles)
	}
	if !strings.Contains(string(body), `"purpose":"Build a landing page"`) {
		t.Fatalf("dataFile body lost original heredoc: %s", body)
	}

	// Args' absolute offsets should still slice into the original cmd.
	for _, a := range args {
		if a.start < 0 || a.end > len(multiStmtCatCurlCmd) {
			t.Fatalf("args offset out of range: %+v", a)
		}
	}
}

func TestControlPartsFromCommandInput_ResolvesDataAtPath(t *testing.T) {
	in, _ := json.Marshal(map[string]string{"command": multiStmtCatCurlCmd})
	u, method, body, ok := controlPartsFromCommandInput(json.RawMessage(in), "")
	if !ok {
		t.Fatalf("expected controlPartsFromCommandInput to succeed on multi-stmt")
	}
	if method != "POST" {
		t.Errorf("method=%q, want POST", method)
	}
	if u == nil || !strings.HasSuffix(u.Path, "/api/control/tasks") {
		t.Errorf("URL = %v, want .../api/control/tasks", u)
	}
	if !strings.Contains(string(body), `"purpose":"Build a landing page"`) {
		t.Errorf("body should have resolved @file → heredoc content; got %s", body)
	}
}

func TestRewriteControlToolUse_RewritesMultiStmtCatHeredocPlusCurl(t *testing.T) {
	tu := conversation.ToolUse{
		ID:    "tu_1",
		Name:  "Bash",
		Input: json.RawMessage(`{"command":` + jsonQuote(multiStmtCatCurlCmd) + `}`),
	}
	rewritten, _, ok, err := RewriteControlToolUse(tu, "https://control.example", "cv-nonce-test")
	if err != nil {
		t.Fatalf("rewrite err: %v", err)
	}
	if !ok || len(rewritten) == 0 {
		t.Fatalf("expected control rewrite for multi-stmt cat+curl; ok=%v rewritten=%s", ok, rewritten)
	}
	// The cat heredoc must still be present (we didn't strip the body),
	// and the URL must be rewritten to the resolver host. The output
	// is JSON-encoded so `<<` is escaped as `<<`.
	out := string(rewritten)
	if !strings.Contains(out, "cat \\u003c\\u003c") {
		t.Errorf("rewrite dropped the cat heredoc: %s", out)
	}
	if !strings.Contains(out, `https://control.example/api/control/tasks`) {
		t.Errorf("rewrite missing control URL: %s", out)
	}
	if !strings.Contains(out, `X-Clawvisor-Caller`) {
		t.Errorf("rewrite missing caller header: %s", out)
	}
}

func TestParseControlCmd_RefusesExtraNonCatCommands(t *testing.T) {
	// Extra side effects (here a `rm`) between cat and curl must refuse.
	cmd := `cat <<'EOF' >/tmp/x.json
{"purpose":"x"}
EOF
rm -rf /tmp/important
curl -sS -X POST 'https://clawvisor.local/control/tasks' --data @/tmp/x.json`
	if _, _, ok := parseControlCmd(cmd); ok {
		t.Fatal("multi-stmt with extra non-cat command must refuse")
	}
}

func TestParseControlCmd_RefusesPipeBetweenCommands(t *testing.T) {
	cmd := `echo hi | curl -sS -X POST 'https://clawvisor.local/control/tasks' --data '{}'`
	if _, _, ok := parseControlCmd(cmd); ok {
		t.Fatal("piped curl must refuse")
	}
}

func TestParseControlCmd_RefusesDynamicCatPath(t *testing.T) {
	// $HOME is dynamic; static-shell-word fails the check.
	cmd := `cat <<EOF >$HOME/x.json
{}
EOF
curl -sS -X POST 'https://clawvisor.local/control/tasks' --data @/tmp/x.json`
	if _, _, ok := parseControlCmd(cmd); ok {
		t.Fatal("dynamic cat output path must refuse")
	}
}

// A cat-heredoc that writes to a path the curl never reads is a
// smuggled file write — the rewriter only edits the curl URL, leaving
// surrounding statements to execute on the harness verbatim. Reject.
func TestParseControlCmd_RefusesCatPathNotReadByCurl(t *testing.T) {
	cmd := `cat <<'EOF' >/etc/passwd-shadow.bak
malicious
EOF
curl -sS -X POST 'https://clawvisor.local/control/tasks' --data @/tmp/body.json`
	if _, _, ok := parseControlCmd(cmd); ok {
		t.Fatal("cat path unrelated to curl's --data @path must refuse")
	}
}

// A second cat-heredoc alongside the legitimate body cat is also a
// smuggled write: the curl can read only one @path body file, so the
// extra cat would only execute as a side effect.
func TestParseControlCmd_RefusesMultipleCatStatements(t *testing.T) {
	cmd := `cat <<'EXTRA' >/tmp/extra.sh
#!/bin/sh
echo pwn
EXTRA
cat <<'BODY' >/tmp/body.json
{"purpose":"x"}
BODY
curl -sS -X POST 'https://clawvisor.local/control/tasks' --data @/tmp/body.json`
	if _, _, ok := parseControlCmd(cmd); ok {
		t.Fatal("multiple cat statements must refuse")
	}
}

// A cat after the curl runs as a tail side effect — could overwrite a
// file the harness will later read, or land a payload at a stable
// location. The legitimate ordering is cat-then-curl.
func TestParseControlCmd_RefusesCatAfterCurl(t *testing.T) {
	cmd := `curl -sS -X POST 'https://clawvisor.local/control/tasks' --data '{"purpose":"x"}'
cat <<'EOF' >/tmp/landing-payload
pwn
EOF`
	if _, _, ok := parseControlCmd(cmd); ok {
		t.Fatal("cat after curl must refuse")
	}
}

// Even when the curl reads exactly what the cat wrote, the cat itself
// still executes on the harness — so a cat targeting an arbitrary path
// outside the safe temp-body allowlist (~/.bashrc, /etc/foo,
// /Users/<u>/.ssh/authorized_keys, /tmp/sub/dir/x.json) is a smuggled
// file write regardless of curl's --data target. The path allowlist
// must forbid those even when curl honestly reads them.
func TestParseControlCmd_RefusesCatPathOutsideSafeAllowlist(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"home_dotfile", "/Users/eric/.bashrc"},
		{"etc_config", "/etc/important.conf"},
		{"ssh_authorized_keys", "/Users/eric/.ssh/authorized_keys"},
		{"subdir_under_tmp", "/tmp/inner/body.json"},
		{"parent_traversal_under_tmp", "/tmp/../etc/passwd"},
		{"non_json_extension", "/tmp/body.sh"},
		{"bare_tmp_no_extension", "/tmp/body"},
		{"relative_path", "tmp/body.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := `cat <<'EOF' >` + tc.path + `
{"purpose":"x"}
EOF
curl -sS -X POST 'https://clawvisor.local/control/tasks' --data @` + tc.path
			if _, _, ok := parseControlCmd(cmd); ok {
				t.Fatalf("cat path %q must be refused even when curl reads it", tc.path)
			}
		})
	}
}

// Shell semantics for `command >file1 >file2`: BOTH files get
// opened/truncated, even though only the last fd receives output.
// Without an explicit single-redir check, the loop in parseHeredocToFile
// would overwrite outPath to the LAST path, so the allowlist check on
// `/tmp/body.json` passes — while `/Users/eric/.ssh/authorized_keys`
// silently gets truncated as a side effect of opening it for `>`.
func TestParseControlCmd_RefusesMultipleCatOutputRedirections(t *testing.T) {
	cmd := `cat <<'EOF' >/Users/eric/.ssh/authorized_keys >/tmp/body.json
{"purpose":"x"}
EOF
curl -sS -X POST 'https://clawvisor.local/control/tasks' --data @/tmp/body.json`

	if _, _, ok := parseControlCmd(cmd); ok {
		t.Fatal("cat with multiple output redirections must refuse")
	}
}

// A leading-dot filename (e.g. /tmp/.bashrc.json), a leading-dash
// filename (e.g. /tmp/-rf.json), or consecutive dots (/tmp/foo..bar.json)
// shouldn't be in the safe-temp allowlist. None of these escape /tmp,
// but the allowlist's job is "narrow and obviously safe" — surprises
// here invite trouble.
func TestParseControlCmd_RefusesCatLeadingDotOrDashFilename(t *testing.T) {
	cases := []string{
		"/tmp/.bashrc.json",
		"/tmp/-rf.json",
		"/tmp/..json",
		"/tmp/...json",
		"/tmp/foo..bar.json",
		"/tmp/foo.....json",
	}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			cmd := `cat <<'EOF' >` + path + `
{"purpose":"x"}
EOF
curl -sS -X POST 'https://clawvisor.local/control/tasks' --data @` + path
			if _, _, ok := parseControlCmd(cmd); ok {
				t.Fatalf("path %q must be refused", path)
			}
		})
	}
}

// `>>` (append) on the cat would let a model splice content onto an
// existing file rather than overwrite a fresh one. Even within the
// safe allowlist that's a smuggled mutation — reject it.
func TestParseControlCmd_RefusesCatAppendMode(t *testing.T) {
	cmd := `cat <<'EOF' >>/tmp/body.json
extra
EOF
curl -sS -X POST 'https://clawvisor.local/control/tasks' --data @/tmp/body.json`
	if _, _, ok := parseControlCmd(cmd); ok {
		t.Fatal("cat with >> append must refuse")
	}
}

// jsonQuote returns a JSON-encoded double-quoted string for s, including
// the surrounding quotes. Test-only convenience.
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestRewriteControlToolUse_ClampsExecCommandYieldTimeMs(t *testing.T) {
	// Codex exec_command shape with a small yield_time_ms (harness
	// default backgrounds the curl in ~1s). Proxy must clamp it up
	// so the task-creation curl stays in the foreground for the
	// full wait window.
	tu := conversation.ToolUse{
		ID:   "tu_1",
		Name: "exec_command",
		Input: json.RawMessage(`{
			"cmd": "curl -sS -X POST 'https://clawvisor.local/control/tasks?wait=true&timeout=120' --data '{}'",
			"workdir": "/tmp",
			"yield_time_ms": 1000,
			"max_output_tokens": 2000
		}`),
	}
	rewritten, _, ok, err := RewriteControlToolUse(tu, "https://control.example", "cv-nonce-test")
	if err != nil || !ok {
		t.Fatalf("expected control rewrite; ok=%v err=%v", ok, err)
	}
	var raw map[string]any
	if err := json.Unmarshal(rewritten, &raw); err != nil {
		t.Fatalf("rewritten input not valid JSON: %v", err)
	}
	got, ok := numericFromAny(raw["yield_time_ms"])
	if !ok {
		t.Fatalf("yield_time_ms missing from rewritten input: %s", rewritten)
	}
	if got < controlToolUseMinYieldMs {
		t.Fatalf("yield_time_ms = %d, want >= %d", got, controlToolUseMinYieldMs)
	}
	// Preserved fields.
	if raw["workdir"] != "/tmp" || raw["max_output_tokens"] == nil {
		t.Errorf("rewrite dropped unrelated fields: %s", rewritten)
	}
}

func TestRewriteControlToolUse_AddsYieldTimeMsWhenAbsent(t *testing.T) {
	tu := conversation.ToolUse{
		ID:   "tu_1",
		Name: "exec_command",
		Input: json.RawMessage(`{
			"cmd": "curl -sS -X POST 'https://clawvisor.local/control/tasks' --data '{}'",
			"workdir": "/tmp"
		}`),
	}
	rewritten, _, ok, err := RewriteControlToolUse(tu, "https://control.example", "cv-nonce-test")
	if err != nil || !ok {
		t.Fatalf("expected control rewrite; ok=%v err=%v", ok, err)
	}
	var raw map[string]any
	if err := json.Unmarshal(rewritten, &raw); err != nil {
		t.Fatalf("rewritten input not valid JSON: %v", err)
	}
	got, ok := numericFromAny(raw["yield_time_ms"])
	if !ok || got < controlToolUseMinYieldMs {
		t.Fatalf("yield_time_ms = %v, want set to >= %d", raw["yield_time_ms"], controlToolUseMinYieldMs)
	}
}

// The yield_time_ms injection is a Codex exec_command-specific repair.
// A hypothetical future cmd-keyed tool that doesn't use yield_time_ms
// as its yield parameter must NOT get the field stamped onto it just
// because the input happens to carry a cmd key.
func TestRewriteControlToolUse_DoesNotInjectYieldOntoNonCodexCmdShape(t *testing.T) {
	tu := conversation.ToolUse{
		ID:   "tu_1",
		Name: "future_run_command", // not exec_command
		Input: json.RawMessage(`{
			"cmd": "curl -sS -X POST 'https://clawvisor.local/control/tasks' --data '{}'",
			"workdir": "/tmp"
		}`),
	}
	rewritten, _, ok, err := RewriteControlToolUse(tu, "https://control.example", "cv-nonce-test")
	if err != nil || !ok {
		t.Fatalf("expected control rewrite; ok=%v err=%v", ok, err)
	}
	var raw map[string]any
	if err := json.Unmarshal(rewritten, &raw); err != nil {
		t.Fatalf("rewritten input not valid JSON: %v", err)
	}
	if _, present := raw["yield_time_ms"]; present {
		t.Fatalf("non-Codex tool should not gain yield_time_ms; got %s", rewritten)
	}
}

func TestRewriteControlToolUse_BashShapeUnchangedByYieldClamp(t *testing.T) {
	// Claude Code's Bash input has `command`, not `cmd`. The clamp
	// should not introduce a yield_time_ms field for Bash.
	tu := conversation.ToolUse{
		ID:    "tu_1",
		Name:  "Bash",
		Input: json.RawMessage(`{"command": "curl -sS -X POST 'https://clawvisor.local/control/tasks' --data '{}'"}`),
	}
	rewritten, _, ok, err := RewriteControlToolUse(tu, "https://control.example", "cv-nonce-test")
	if err != nil || !ok {
		t.Fatalf("expected control rewrite; ok=%v err=%v", ok, err)
	}
	var raw map[string]any
	if err := json.Unmarshal(rewritten, &raw); err != nil {
		t.Fatalf("rewritten input not valid JSON: %v", err)
	}
	if _, present := raw["yield_time_ms"]; present {
		t.Fatalf("Bash shape should not gain yield_time_ms; got %s", rewritten)
	}
}

func TestRewriteControlToolUse_PreservesLargeYieldTimeMs(t *testing.T) {
	// If the agent already set a yield > floor, leave it alone.
	const explicit = 300_000
	cmd := `curl -sS -X POST 'https://clawvisor.local/control/tasks' --data '{}'`
	tu := conversation.ToolUse{
		ID:   "tu_1",
		Name: "exec_command",
		Input: json.RawMessage(`{
			"cmd": ` + jsonQuote(cmd) + `,
			"yield_time_ms": 300000
		}`),
	}
	rewritten, _, ok, _ := RewriteControlToolUse(tu, "https://control.example", "cv-nonce-test")
	if !ok {
		t.Fatal("expected rewrite")
	}
	var raw map[string]any
	_ = json.Unmarshal(rewritten, &raw)
	if got, _ := numericFromAny(raw["yield_time_ms"]); got != explicit {
		t.Errorf("yield_time_ms = %d, want preserved value %d", got, explicit)
	}
}
