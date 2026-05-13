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
