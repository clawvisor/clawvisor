package conversation

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestNewConversationID_ShapeAndUniqueness(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 100; i++ {
		id, err := NewConversationID()
		if err != nil {
			t.Fatalf("mint err: %v", err)
		}
		if !strings.HasPrefix(id, ConversationIDPrefix) {
			t.Fatalf("missing %q prefix: %q", ConversationIDPrefix, id)
		}
		tail := strings.TrimPrefix(id, ConversationIDPrefix)
		if len(tail) != 26 {
			t.Fatalf("expected 26-char base32 tail, got %d (%q)", len(tail), id)
		}
		// Lowercase base32: a-z2-7. The scanner regex is a tolerant
		// superset (a-z0-9), but the mint itself MUST stay inside the
		// alphabet so a future regex tightening doesn't break echoes
		// of historical IDs.
		for _, r := range tail {
			if !(r >= 'a' && r <= 'z') && !(r >= '2' && r <= '7') {
				t.Fatalf("char %q outside base32 alphabet in %q", r, id)
			}
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("collision after %d mints: %q", i+1, id)
		}
		seen[id] = struct{}{}
	}
}

func TestNewConversationID_PropagatesEntropyError(t *testing.T) {
	saved := conversationIDRandRead
	t.Cleanup(func() { conversationIDRandRead = saved })
	conversationIDRandRead = func(b []byte) (int, error) {
		return 0, errors.New("boom")
	}
	if _, err := NewConversationID(); err == nil || !strings.Contains(err.Error(), "generate conversation id") {
		t.Fatalf("expected wrapped entropy error, got %v", err)
	}
}

func TestRenderConversationIDMarker_RoundTripsThroughRegex(t *testing.T) {
	id, err := NewConversationID()
	if err != nil {
		t.Fatal(err)
	}
	rendered := RenderConversationIDMarker(id)
	if !strings.HasPrefix(rendered, ConversationIDMarker) || !strings.HasSuffix(rendered, "]") {
		t.Fatalf("rendered marker has unexpected shape: %q", rendered)
	}
	matches := conversationIDMarkerRE.FindStringSubmatch(rendered)
	if matches == nil || matches[1] != id {
		t.Fatalf("regex did not extract minted id: rendered=%q matches=%v", rendered, matches)
	}
}

func chatCompletionsReq(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestFindInjectedConversationID_SingleAssistantMarker(t *testing.T) {
	req := chatCompletionsReq(t)
	body := []byte(`{"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"[Clawvisor] Routing this conversation through Clawvisor. [clawvisor:conversation=cv-conv-abcdefghijklmnopqrstuvwxyz]"}
	]}`)
	got := FindInjectedConversationID(req, ProviderOpenAI, body)
	if got != "cv-conv-abcdefghijklmnopqrstuvwxyz" {
		t.Fatalf("got %q", got)
	}
}

func TestFindInjectedConversationID_MostRecentWins(t *testing.T) {
	// Two assistant turns carry markers. Most recent (last in order)
	// wins — compaction may keep an earlier marker, but the freshest
	// is the one produced by the live mint.
	req := chatCompletionsReq(t)
	body := []byte(`{"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"[clawvisor:conversation=cv-conv-aaaaaaaaaaaaaaaaaaaaaaaaaa]"},
		{"role":"user","content":"again"},
		{"role":"assistant","content":"[clawvisor:conversation=cv-conv-bbbbbbbbbbbbbbbbbbbbbbbbbb]"}
	]}`)
	got := FindInjectedConversationID(req, ProviderOpenAI, body)
	if got != "cv-conv-bbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("got %q, want the second marker", got)
	}
}

func TestFindInjectedConversationID_IgnoresUserAuthoredMarker(t *testing.T) {
	// Security-critical: a user typing the marker text into their own
	// message MUST NOT hijack the conversation ID. Only assistant-role
	// content is scanned.
	req := chatCompletionsReq(t)
	body := []byte(`{"messages":[
		{"role":"system","content":"system [clawvisor:conversation=cv-conv-systemmmmmmmmmmmmmmmmmmmm]"},
		{"role":"user","content":"hey [clawvisor:conversation=cv-conv-attackerrrrrrrrrrrrrrrrrrr]"},
		{"role":"tool","content":"tool out [clawvisor:conversation=cv-conv-toolllllllllllllllllllllllll]"}
	]}`)
	got := FindInjectedConversationID(req, ProviderOpenAI, body)
	if got != "" {
		t.Fatalf("user/system/tool-authored marker leaked: got %q", got)
	}
}

func TestFindInjectedConversationID_AssistantBlockContent(t *testing.T) {
	// Assistant content carried as typed-block array (e.g. multimodal).
	// flattenOpenAIContent already handles both shapes — verify the
	// marker scanner picks it up the same way.
	req := chatCompletionsReq(t)
	body := []byte(`{"messages":[
		{"role":"assistant","content":[
			{"type":"text","text":"some prose"},
			{"type":"text","text":"[clawvisor:conversation=cv-conv-blockcontentmarkerrrrrrrr]"}
		]}
	]}`)
	got := FindInjectedConversationID(req, ProviderOpenAI, body)
	if got != "cv-conv-blockcontentmarkerrrrrrrr" {
		t.Fatalf("got %q", got)
	}
}

func TestFindInjectedConversationID_MarkerInCodeFence(t *testing.T) {
	// Models / harnesses sometimes wrap parts of an assistant message
	// in code fences. The regex anchors on the literal bracket form,
	// so surrounding fences / backticks are inert.
	req := chatCompletionsReq(t)
	body := []byte(`{"messages":[
		{"role":"assistant","content":"see footer below:\n\n` + "```" + `\n[clawvisor:conversation=cv-conv-fencedmarkerrrrrrrrrrrrrr]\n` + "```" + `"}
	]}`)
	got := FindInjectedConversationID(req, ProviderOpenAI, body)
	if got != "cv-conv-fencedmarkerrrrrrrrrrrrrr" {
		t.Fatalf("got %q", got)
	}
}

func TestFindInjectedConversationID_NoMarker(t *testing.T) {
	req := chatCompletionsReq(t)
	body := []byte(`{"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"hello, no marker here"}
	]}`)
	if got := FindInjectedConversationID(req, ProviderOpenAI, body); got != "" {
		t.Fatalf("got %q, expected empty", got)
	}
}

func TestFindInjectedConversationID_MalformedBody(t *testing.T) {
	req := chatCompletionsReq(t)
	if got := FindInjectedConversationID(req, ProviderOpenAI, []byte("{not json")); got != "" {
		t.Fatalf("got %q on malformed body, expected empty", got)
	}
}

func TestFindInjectedConversationID_RejectsNonChatCompletionsEndpoint(t *testing.T) {
	// Responses endpoint must NOT consult the marker — it has a native
	// session ID and we never inject markers there.
	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/responses", nil)
	body := []byte(`{"messages":[
		{"role":"assistant","content":"[clawvisor:conversation=cv-conv-shouldnotresolvvvvvvvvvvvv]"}
	]}`)
	if got := FindInjectedConversationID(req, ProviderOpenAI, body); got != "" {
		t.Fatalf("Responses endpoint unexpectedly matched marker: %q", got)
	}
}

func TestFindInjectedConversationID_RejectsNonOpenAIProvider(t *testing.T) {
	req := chatCompletionsReq(t)
	body := []byte(`{"messages":[
		{"role":"assistant","content":"[clawvisor:conversation=cv-conv-anthropicccccccccccccccc]"}
	]}`)
	if got := FindInjectedConversationID(req, ProviderAnthropic, body); got != "" {
		t.Fatalf("Anthropic provider unexpectedly matched marker: %q", got)
	}
}

func TestFindInjectedConversationID_NilRequest(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":"[clawvisor:conversation=cv-conv-niiiiiiiiiiiiiiiiiiiiiiiiil]"}]}`)
	if got := FindInjectedConversationID(nil, ProviderOpenAI, body); got != "" {
		t.Fatalf("nil request must not match: %q", got)
	}
}

func TestFindInjectedConversationID_MalformedMarkerIgnored(t *testing.T) {
	// Marker key matches but value doesn't have the cv-conv- prefix
	// (e.g., an old approval ID accidentally embedded as a conversation
	// value). Must NOT match the conversation scanner.
	req := chatCompletionsReq(t)
	body := []byte(`{"messages":[
		{"role":"assistant","content":"[clawvisor:conversation=cv-not-the-conv-prefix-xx]"}
	]}`)
	if got := FindInjectedConversationID(req, ProviderOpenAI, body); got != "" {
		t.Fatalf("non-cv-conv value unexpectedly matched: %q", got)
	}
}

func TestConversationID_OpenAIChatPrefersMarkerOverFingerprint(t *testing.T) {
	// Integration check on the public ConversationID() entry point:
	// when an assistant turn carries a marker, it wins over the
	// fingerprint fallback even if the first user message would have
	// produced a stable fingerprint.
	req := chatCompletionsReq(t)
	body := []byte(`{"messages":[
		{"role":"user","content":"identical opening line"},
		{"role":"assistant","content":"[clawvisor:conversation=cv-conv-preferredmarkerrrrrrrrrrr]"},
		{"role":"user","content":"continue"}
	]}`)
	got := ConversationID(req, ProviderOpenAI, body)
	if got != "cv-conv-preferredmarkerrrrrrrrrrr" {
		t.Fatalf("ConversationID = %q, want minted marker", got)
	}
}

func TestConversationID_OpenAIChatFallsBackToFingerprintWhenNoMarker(t *testing.T) {
	req := chatCompletionsReq(t)
	body := []byte(`{"messages":[
		{"role":"user","content":"opening line"},
		{"role":"assistant","content":"no marker yet"}
	]}`)
	got := ConversationID(req, ProviderOpenAI, body)
	if !strings.HasPrefix(got, "fp-") {
		t.Fatalf("ConversationID = %q, want fp- fallback", got)
	}
}
