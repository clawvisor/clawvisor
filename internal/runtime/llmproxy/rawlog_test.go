package llmproxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestRawIOLogger_EmitsJSONLineWithBody(t *testing.T) {
	var buf bytes.Buffer
	l := NewRawIOLogger(&buf)
	l.Emit(RawIOEvent{
		Phase:     "inbound_request",
		RequestID: "req-1",
		UserID:    "u",
		AgentID:   "a",
		Provider:  "anthropic",
		Method:    "POST",
		Path:      "/v1/messages",
		Body:      `{"messages":[{"role":"user","content":"hi"}]}`,
		BodyBytes: 44,
	})
	line := strings.TrimSpace(buf.String())
	if !strings.HasSuffix(line, "}") {
		t.Fatalf("output not a single JSON line: %q", buf.String())
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(line), &parsed); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, line)
	}
	if parsed["phase"] != "inbound_request" || parsed["request_id"] != "req-1" {
		t.Errorf("missing fields: %v", parsed)
	}
	if !strings.Contains(parsed["body"].(string), `"role":"user"`) {
		t.Errorf("body lost: %v", parsed["body"])
	}
	if _, hasEnc := parsed["body_encoding"]; hasEnc {
		t.Errorf("utf8 body should not be base64-encoded; got encoding=%v", parsed["body_encoding"])
	}
}

func TestRawIOLogger_AnnotatesStablePromptPrefix(t *testing.T) {
	var buf bytes.Buffer
	l := NewRawIOLogger(&buf)
	common := RawIOEvent{
		Phase:    "inbound_request",
		UserID:   "u",
		AgentID:  "a",
		Provider: "anthropic",
		Method:   "POST",
		Path:     "/v1/messages",
	}
	firstBody := `{"model":"claude-opus-4-7","system":[{"type":"text","text":"static control notice"}],"tools":[{"name":"Bash"}],"messages":[{"role":"user","content":"first"}]}`
	secondBody := `{"model":"claude-opus-4-7","system":[{"type":"text","text":"static control notice"}],"tools":[{"name":"Bash"}],"messages":[{"role":"user","content":"second"}]}`
	common.RequestID = "req-1"
	common.Body = firstBody
	common.BodyBytes = len(firstBody)
	l.Emit(common)
	common.RequestID = "req-2"
	common.Body = secondBody
	common.BodyBytes = len(secondBody)
	l.Emit(common)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected two log lines, got %d: %s", len(lines), buf.String())
	}
	var first, second map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("first line invalid JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("second line invalid JSON: %v", err)
	}
	if first["prompt_prefix_sha256"] == "" || second["prompt_prefix_sha256"] == "" {
		t.Fatalf("expected prompt prefix hashes: first=%v second=%v", first, second)
	}
	if first["prompt_prefix_sha256"] != second["prompt_prefix_sha256"] {
		t.Fatalf("same system/tools prefix should hash identically: first=%v second=%v", first["prompt_prefix_sha256"], second["prompt_prefix_sha256"])
	}
	if second["prompt_prefix_stable_with_previous"] != true {
		t.Fatalf("second request should be marked stable with previous prefix: %v", second)
	}
}

func TestRawIOLogger_FlagsPromptPrefixChange(t *testing.T) {
	var buf bytes.Buffer
	l := NewRawIOLogger(&buf)
	common := RawIOEvent{
		Phase:    "inbound_request",
		UserID:   "u",
		AgentID:  "a",
		Provider: "anthropic",
		Method:   "POST",
		Path:     "/v1/messages",
	}
	firstBody := `{"model":"claude-opus-4-7","system":"static notice","tools":[{"name":"Bash"}],"messages":[{"role":"user","content":"first"}]}`
	secondBody := `{"model":"claude-opus-4-7","system":"changed notice","tools":[{"name":"Bash"}],"messages":[{"role":"user","content":"second"}]}`
	common.RequestID = "req-1"
	common.Body = firstBody
	common.BodyBytes = len(firstBody)
	l.Emit(common)
	common.RequestID = "req-2"
	common.Body = secondBody
	common.BodyBytes = len(secondBody)
	l.Emit(common)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var second map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("second line invalid JSON: %v", err)
	}
	if second["prompt_prefix_stable_with_previous"] != false {
		t.Fatalf("second request should be marked unstable after prefix change: %v", second)
	}
	if second["prompt_prefix_previous_sha256"] == "" {
		t.Fatalf("changed prefix should include previous hash: %v", second)
	}
}

func TestRawIOLogger_NilReceiverIsNoop(t *testing.T) {
	var l *RawIOLogger
	// Must not panic.
	l.Emit(RawIOEvent{Phase: "x"})
}

func TestEncodeBody_UTF8PassesThroughBase64ForBinary(t *testing.T) {
	utf8Body := []byte(`{"x":1}`)
	got, enc := EncodeBody(utf8Body)
	if got != `{"x":1}` || enc != "" {
		t.Errorf("utf8 body encoded as %q (enc=%q), want passthrough", got, enc)
	}
	binBody := []byte{0xff, 0xfe, 0x00, 0x01}
	got, enc = EncodeBody(binBody)
	if enc != "base64" {
		t.Errorf("binary body should be base64; got enc=%q", enc)
	}
	if got == "" {
		t.Errorf("base64 body empty")
	}
}

func TestEncodeBody_RedactsLiveAutovaultPlaceholders(t *testing.T) {
	body := []byte(`{"system":"credential agentphone=autovault_agentphone_live123; use it"}`)
	got, enc := EncodeBody(body)
	if enc != "" {
		t.Fatalf("utf8 body should not be base64 encoded, got %q", enc)
	}
	if strings.Contains(got, "autovault_agentphone_live123") {
		t.Fatalf("live placeholder should be redacted from raw log body: %s", got)
	}
	if !strings.Contains(got, "<REDACTED:autovault>") {
		t.Fatalf("expected autovault redaction marker, got %s", got)
	}
}

func TestSafeHeaderSnapshot_KeepsOnlyAllowlist(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer secret-do-not-leak")
	h.Set("X-Request-Id", "rid-1")
	got := SafeHeaderSnapshot(h)
	if got["Content-Type"] != "application/json" || got["X-Request-Id"] != "rid-1" {
		t.Errorf("expected allowed headers preserved: %v", got)
	}
	if _, present := got["Authorization"]; present {
		t.Errorf("Authorization must NOT be captured into raw log")
	}
}
