package llmproxy

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
{"purpose":"Build a landing page","intent_verification_mode":"strict","expires_in_seconds":600,"expected_tools_json":[{"tool_name":"Bash","why":"Create dir"}]}
EOF
curl -sS -X POST 'https://clawvisor.local/control/tasks?wait=true&timeout=120' -H 'Content-Type: application/json' --data @/tmp/clawvisor-task.json`

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
	if u == nil || !strings.HasSuffix(u.Path, "/control/tasks") {
		t.Errorf("URL = %v, want .../control/tasks", u)
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
	if !strings.Contains(out, `https://control.example/control/tasks`) {
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

// jsonQuote returns a JSON-encoded double-quoted string for s, including
// the surrounding quotes. Test-only convenience.
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
