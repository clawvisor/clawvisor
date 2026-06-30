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
	// metadata.user_id is present but doesn't contain session_id (device_id
	// is shared across conversations on the same install, so it's not a
	// valid id source). Falls back to the first-user-message fingerprint
	// so per-conversation isolation works even for clients that don't set
	// the Claude-Code-specific session_id field.
	body := []byte(`{"metadata": {"user_id": "{\"device_id\":\"4228e00a\"}"}, "messages":[{"role":"user","content":"hello"}]}`)
	got := ConversationID(nil, ProviderAnthropic, body)
	if !strings.HasPrefix(got, "fp-") {
		t.Fatalf("ConversationID without session_id = %q, want fp- prefix (fingerprint fallback)", got)
	}
}

func TestConversationIDAnthropicMalformed(t *testing.T) {
	// Each case has no derivable id AND no user message to fingerprint, so
	// the result is genuinely empty. These are the only cases left where
	// ConversationID returns "" after the fingerprint fallback.
	cases := [][]byte{
		nil,
		[]byte(``),
		[]byte(`{"metadata": {}}`),
		[]byte(`{`),
		// metadata.user_id is a string but not JSON; still no messages.
		[]byte(`{"metadata": {"user_id": "not-json"}}`),
		// Has messages but only assistant role — no user turn to hash.
		[]byte(`{"messages":[{"role":"assistant","content":"hi"}]}`),
		// User-role messages that contain ONLY tool_result blocks must
		// not be fingerprinted: Anthropic wraps tool returns in
		// role:"user" because there's no "tool" role, and a
		// fingerprint computed from tool output would drift the
		// conversation id every time the tool's response changed.
		// Skip them; with no other user-authored turn this body has
		// nothing valid to hash, so the result is empty.
		[]byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_x","content":"ok"}]}]}`),
		// Mixed: assistant tool_use turn followed by user tool_result
		// wrapper. Still no user-authored text. Empty.
		[]byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_x","name":"Read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_x","content":"file contents"}]}]}`),
	}
	for _, body := range cases {
		if got := ConversationID(nil, ProviderAnthropic, body); got != "" {
			t.Fatalf("ConversationID(%q) = %q, want empty (no id source + no user message)", string(body), got)
		}
	}
}

// TestConversationIDAnthropicFingerprintFallback pins the per-conversation
// isolation invariant for raw Anthropic API clients that don't set
// metadata.user_id at all. Without the fingerprint fallback, every
// conversation from these clients would collide on "" and share a single
// task-checkout bucket — exactly the cross-conversation leak this PR
// closes for Claude Code clients.
func TestConversationIDAnthropicFingerprintFallback(t *testing.T) {
	bodyA := []byte(`{"messages":[{"role":"user","content":"build a landing page in /tmp/projA"}]}`)
	bodyB := []byte(`{"messages":[{"role":"user","content":"build a landing page in /tmp/projB"}]}`)
	idA := ConversationID(nil, ProviderAnthropic, bodyA)
	idB := ConversationID(nil, ProviderAnthropic, bodyB)
	if idA == "" || idB == "" {
		t.Fatalf("fingerprints must be non-empty: A=%q B=%q", idA, idB)
	}
	if !strings.HasPrefix(idA, "fp-") || !strings.HasPrefix(idB, "fp-") {
		t.Fatalf("fingerprints must carry fp- prefix: A=%q B=%q", idA, idB)
	}
	if idA == idB {
		t.Fatalf("distinct first user messages must partition: both got %q", idA)
	}

	// Stability: the same first user message produces the same fingerprint
	// even when later turns differ (only the FIRST user turn is hashed, so
	// compaction-replaced middle turns can't drift the id).
	bodyA2 := []byte(`{"messages":[{"role":"user","content":"build a landing page in /tmp/projA"},{"role":"assistant","content":"sure"},{"role":"user","content":"actually scratch that"}]}`)
	if got := ConversationID(nil, ProviderAnthropic, bodyA2); got != idA {
		t.Fatalf("fingerprint drifted across turns: turn1=%q turn3=%q", idA, got)
	}

	// Block-shaped content (Anthropic's array form with text blocks).
	bodyBlocks := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"build a landing page in /tmp/projA"}]}]}`)
	if got := ConversationID(nil, ProviderAnthropic, bodyBlocks); got != idA {
		t.Fatalf("string content and equivalent block content must produce same fingerprint: string=%q blocks=%q", idA, got)
	}

	// Tool-result skip: Anthropic uses role:"user" for tool returns
	// (no separate "tool" role), so a fingerprint that hashed
	// flattenAnthropicContent's tool_result text would be scoped by
	// tool output instead of the user's actual prompt. After a
	// harness replay that truncated the original user turn, the
	// fingerprint must SKIP the tool_result wrapper and find the
	// next user-authored text — here, the second user turn.
	bodyToolReplay := []byte(`{"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"path":"/etc/hosts"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"127.0.0.1 localhost"}]},
		{"role":"user","content":"build a landing page in /tmp/projA"}
	]}`)
	if got := ConversationID(nil, ProviderAnthropic, bodyToolReplay); got != idA {
		t.Fatalf("tool_result-only user message must be skipped; got %q, want %q (hash of next user-authored text)", got, idA)
	}

	// Two tool_result-only user wrappers ahead of the real user turn
	// — must still skip both and arrive at the third user message.
	bodyDoubleToolReplay := []byte(`{"messages":[
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"first tool output"}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"b","content":"second tool output"}]},
		{"role":"user","content":"build a landing page in /tmp/projA"}
	]}`)
	if got := ConversationID(nil, ProviderAnthropic, bodyDoubleToolReplay); got != idA {
		t.Fatalf("consecutive tool_result wrappers must all be skipped; got %q, want %q", got, idA)
	}

	// Mixed-block user message (text + tool_result in the same
	// content array) keeps the text-only extraction: only the "text"
	// block contributes to the fingerprint, so swapping the
	// tool_result payload doesn't drift the id.
	bodyMixedA := []byte(`{"messages":[{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"x","content":"OUTPUT_A"},
		{"type":"text","text":"build a landing page in /tmp/projA"}
	]}]}`)
	bodyMixedB := []byte(`{"messages":[{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"x","content":"OUTPUT_DIFFERENT"},
		{"type":"text","text":"build a landing page in /tmp/projA"}
	]}]}`)
	idMixedA := ConversationID(nil, ProviderAnthropic, bodyMixedA)
	idMixedB := ConversationID(nil, ProviderAnthropic, bodyMixedB)
	if idMixedA != idA {
		t.Fatalf("mixed user content must fingerprint on text only; got %q, want %q", idMixedA, idA)
	}
	if idMixedA != idMixedB {
		t.Fatalf("changing tool_result payload alongside identical text must not drift fingerprint; got %q vs %q", idMixedA, idMixedB)
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
	// No prompt_cache_key AND no input → still empty (no user message
	// to fingerprint).
	body := []byte(`{"model":"gpt-5.5"}`)
	if got := ConversationID(req, ProviderOpenAI, body); got != "" {
		t.Fatalf("ConversationID without prompt_cache_key and no input = %q, want empty", got)
	}
}

// TestConversationIDOpenAIResponsesFingerprintFallback pins the
// fingerprint fallback for /v1/responses clients that don't set
// prompt_cache_key (everyone except Codex). Covers both wire shapes
// the Responses API accepts: a bare-string input, and an array of
// role-tagged input items.
func TestConversationIDOpenAIResponsesFingerprintFallback(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/responses", nil)
	// Shape 1: bare-string input.
	bodyString := []byte(`{"model":"gpt-5.5","input":"build a landing page in /tmp/projA"}`)
	idString := ConversationID(req, ProviderOpenAI, bodyString)
	if !strings.HasPrefix(idString, "fp-") {
		t.Fatalf("bare-string input fingerprint = %q, want fp- prefix", idString)
	}
	// Shape 2: array of items with a user-role entry. Same text →
	// same fingerprint as the bare-string shape (callers should be
	// indifferent to which shape the client picked).
	bodyArray := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"build a landing page in /tmp/projA"}]}]}`)
	idArray := ConversationID(req, ProviderOpenAI, bodyArray)
	if idArray != idString {
		t.Fatalf("string-shape and equivalent array-shape fingerprints must match: string=%q array=%q", idString, idArray)
	}
	// Distinct first messages partition.
	bodyB := []byte(`{"model":"gpt-5.5","input":"build a landing page in /tmp/projB"}`)
	idB := ConversationID(req, ProviderOpenAI, bodyB)
	if idB == idString {
		t.Fatalf("distinct inputs must partition; both got %q", idString)
	}
	// prompt_cache_key still wins when present.
	bodyWithKey := []byte(`{"model":"gpt-5.5","prompt_cache_key":"019e55ba-3a42-7932-9262-b71c9c7e6281","input":"hello"}`)
	if got := ConversationID(req, ProviderOpenAI, bodyWithKey); got != "019e55ba-3a42-7932-9262-b71c9c7e6281" {
		t.Fatalf("native key must take precedence over fingerprint; got %q", got)
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
