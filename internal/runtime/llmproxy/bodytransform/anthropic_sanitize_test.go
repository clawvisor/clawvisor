package bodytransform

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSanitizeAnthropicRequestRemovesEmptyTextBlocks(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"system":[{"type":"text","text":""},{"type":"text","text":"system"}],
		"messages":[
			{"role":"user","content":"hello"},
			{"role":"assistant","content":[
				{"type":"text","text":""},
				{"type":"tool_use","id":"toolu_1","name":"WebFetch","input":{"url":"https://clawvisor.local/control/skill","prompt":"What is here?"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":""},{"type":"text","text":"ok"}]},
				{"type":"text","text":"  "}
			]},
			{"role":"assistant","content":[{"type":"text","text":""}]}
		]
	}`)

	got, changed, err := SanitizeAnthropicRequest(body)
	if err != nil {
		t.Fatalf("SanitizeAnthropicRequest: %v", err)
	}
	if !changed {
		t.Fatal("expected sanitizer to report a change")
	}
	if strings.Contains(string(got), `"text":""`) {
		t.Fatalf("sanitized request still has empty text block: %s", got)
	}
	if strings.Contains(string(got), `"text":"  "`) {
		t.Fatalf("sanitized request still has whitespace-only text block: %s", got)
	}
	if !strings.Contains(string(got), `"type":"tool_use"`) ||
		!strings.Contains(string(got), `"type":"tool_result"`) ||
		!strings.Contains(string(got), `"text":"ok"`) {
		t.Fatalf("sanitized request lost non-empty content: %s", got)
	}

	var parsed map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("sanitized JSON invalid: %v", err)
	}
}

func TestSanitizeAnthropicRequestNoop(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	got, changed, err := SanitizeAnthropicRequest(body)
	if err != nil {
		t.Fatalf("SanitizeAnthropicRequest: %v", err)
	}
	if changed {
		t.Fatalf("expected no change, got %s", got)
	}
	if string(got) != string(body) {
		t.Fatalf("noop body changed: %s", got)
	}
}

func TestSanitizeAnthropicRequestMalformedErrorDoesNotEchoBodySecrets(t *testing.T) {
	secrets := []string{
		"sk-ant-api03-secret-value",
		"Bearer ghp_secretsecretsecret",
		"autovault_github_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
	}
	body := []byte(`{"messages":[{"role":"user","content":"` + strings.Join(secrets, " ") + `"}],}`)
	_, _, err := SanitizeAnthropicRequest(body)
	if err == nil {
		t.Fatal("expected malformed JSON error")
	}
	got := err.Error()
	for _, secret := range secrets {
		if strings.Contains(got, secret) {
			t.Fatalf("malformed error echoed secret %q: %s", secret, got)
		}
	}
}
