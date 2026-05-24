package conversation

import (
	"net/http"
	"strings"
	"testing"
)

func TestConversationIDAnthropic(t *testing.T) {
	body := []byte(`{
		"model": "claude-haiku-4-5-20251001",
		"messages": [{"role":"user","content":[{"type":"text","text":"hello"}]}],
		"metadata": {"user_id": "{\"device_id\":\"4228e00a\",\"account_uuid\":\"ed1a14b7-62c4-4f09-822a-0b2d37dbeaae\",\"session_id\":\"28598686-9456-4994-84ab-0b6e7d9b2b7d\"}"}
	}`)
	got := ConversationID(nil, ProviderAnthropic, body)
	want := "28598686-9456-4994-84ab-0b6e7d9b2b7d"
	if got != want {
		t.Fatalf("ConversationID anthropic = %q, want %q", got, want)
	}
}

func TestConversationIDAnthropicNoSession(t *testing.T) {
	// metadata.user_id is present but doesn't contain session_id — should
	// fall back to empty, not the device_id, because device_id is shared
	// across conversations on the same install.
	body := []byte(`{"metadata": {"user_id": "{\"device_id\":\"4228e00a\"}"}}`)
	if got := ConversationID(nil, ProviderAnthropic, body); got != "" {
		t.Fatalf("ConversationID without session_id = %q, want empty", got)
	}
}

func TestConversationIDAnthropicMalformed(t *testing.T) {
	cases := [][]byte{
		nil,
		[]byte(``),
		[]byte(`{"metadata": {}}`),
		[]byte(`{`),
		// metadata.user_id is a string but not JSON
		[]byte(`{"metadata": {"user_id": "not-json"}}`),
	}
	for _, body := range cases {
		if got := ConversationID(nil, ProviderAnthropic, body); got != "" {
			t.Fatalf("ConversationID(%q) = %q, want empty", string(body), got)
		}
	}
}

func TestConversationIDOpenAIResponses(t *testing.T) {
	req, err := http.NewRequest("POST", "https://api.openai.com/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{
		"model": "gpt-5.5",
		"prompt_cache_key": "019e55ba-3a42-7932-9262-b71c9c7e6281",
		"input": []
	}`)
	got := ConversationID(req, ProviderOpenAI, body)
	want := "019e55ba-3a42-7932-9262-b71c9c7e6281"
	if got != want {
		t.Fatalf("ConversationID openai responses = %q, want %q", got, want)
	}
}

func TestConversationIDOpenAIResponsesMissingKey(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/responses", nil)
	body := []byte(`{"model":"gpt-5.5"}`)
	if got := ConversationID(req, ProviderOpenAI, body); got != "" {
		t.Fatalf("ConversationID without prompt_cache_key = %q, want empty", got)
	}
}

func TestConversationIDOpenAIChatFingerprint(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", nil)
	body1 := []byte(`{"messages":[{"role":"system","content":"You are OpenClaw"},{"role":"user","content":"Can you create a landing page in /tmp/claude-test-1 directory?"}]}`)
	body2 := []byte(`{"messages":[{"role":"system","content":"You are OpenClaw"},{"role":"user","content":"Can you create a landing page in /tmp/claude-test-2 directory?"}]}`)

	id1 := ConversationID(req, ProviderOpenAI, body1)
	id2 := ConversationID(req, ProviderOpenAI, body2)
	if id1 == "" || id2 == "" {
		t.Fatalf("fingerprint should be non-empty; id1=%q id2=%q", id1, id2)
	}
	if id1 == id2 {
		t.Fatalf("distinct first user messages should produce distinct fingerprints; got %q for both", id1)
	}
	if !strings.HasPrefix(id1, "fp-") {
		t.Fatalf("fingerprint should be prefixed with fp-; got %q", id1)
	}
}

func TestConversationIDOpenAIChatFingerprintStable(t *testing.T) {
	// Same first user message → same fingerprint, even if later turns differ.
	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", nil)
	body1 := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	body2 := []byte(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"more"}]}`)

	id1 := ConversationID(req, ProviderOpenAI, body1)
	id2 := ConversationID(req, ProviderOpenAI, body2)
	if id1 != id2 {
		t.Fatalf("fingerprint should be stable across turns; turn1=%q turn2=%q", id1, id2)
	}
}

func TestConversationIDOpenAIChatBlockContent(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", nil)
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	if got := ConversationID(req, ProviderOpenAI, body); got == "" {
		t.Fatalf("fingerprint should handle typed-block content; got empty")
	}
}

func TestConversationIDUnknownProvider(t *testing.T) {
	if got := ConversationID(nil, Provider("unknown"), []byte(`{}`)); got != "" {
		t.Fatalf("unknown provider should return empty; got %q", got)
	}
}
