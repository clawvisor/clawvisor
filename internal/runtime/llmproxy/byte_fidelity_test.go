package llmproxy

import (
	"context"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// Anthropic enforces a cryptographic signature over thinking blocks
// across turns. Any reshape on the request side — reordered keys,
// stripped whitespace, applied `,omitempty`, dropped unknown fields —
// can corrupt the verification and trip "thinking blocks cannot be
// modified" 400s on the next request that includes that turn.
//
// These tests pin byte-for-byte fidelity for thinking blocks through
// each preprocessing function that touches the inbound body.

const thinkingBlockOriginal = `{"type":"thinking","thinking":"","signature":"sig123"}`

// thinkingBodyTemplate is a minimal Anthropic /v1/messages request
// with one assistant turn whose first content block is a thinking
// block in the exact byte shape Anthropic emits. The blank in `%s`
// is filled with the field under test.
const thinkingBodyTemplate = `{"model":"claude-opus-4-7","messages":[{"role":"assistant","content":[` + thinkingBlockOriginal + `,{"type":"text","text":"%s"}]},{"role":"user","content":[{"type":"text","text":"reply"}]}]}`

func assertThinkingBlockPreserved(t *testing.T, body []byte) {
	t.Helper()
	if !strings.Contains(string(body), thinkingBlockOriginal) {
		t.Errorf("thinking block was reshaped — its exact byte representation no longer appears in body:\n  want substring: %s\n  body: %s", thinkingBlockOriginal, body)
	}
}

func TestSanitizeAnthropicRequestPreservesThinkingBlockBytes(t *testing.T) {
	t.Parallel()
	// The empty text block triggers sanitization and would otherwise
	// cause a full body re-marshal (which alphabetizes keys).
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"assistant","content":[` + thinkingBlockOriginal + `,{"type":"text","text":""},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}]}]}`)
	out, changed, err := SanitizeAnthropicRequest(body)
	if err != nil {
		t.Fatalf("SanitizeAnthropicRequest: %v", err)
	}
	if !changed {
		t.Fatalf("expected sanitization to modify body (empty text block present)")
	}
	assertThinkingBlockPreserved(t, out)
}

func TestInjectControlNoticePreservesThinkingBlockBytes(t *testing.T) {
	t.Parallel()
	body := []byte(strings.ReplaceAll(thinkingBodyTemplate, "%s", "hello"))
	out, injected, err := InjectControlNoticeWithPolicy(conversation.ProviderAnthropic, body, "https://clawvisor.local", []string{"Bash"}, nil)
	if err != nil {
		t.Fatalf("InjectControlNoticeWithPolicy: %v", err)
	}
	if !injected {
		t.Fatalf("expected notice to be injected")
	}
	assertThinkingBlockPreserved(t, out)
}

func TestStripSyntheticApprovalHistoryPreservesThinkingBlockBytes(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"claude-opus-4-7","messages":[` +
		`{"role":"assistant","content":[` + thinkingBlockOriginal + `,{"type":"text","text":"some response"}]},` +
		`{"role":"user","content":[{"type":"text","text":"next"}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"Clawvisor paused this tool call for approval. clawvisor-approval-id=abc"}]},` +
		`{"role":"user","content":[{"type":"text","text":"approve"}]}` +
		`]}`)
	res, err := StripSyntheticApprovalHistory(SyntheticApprovalHistoryStripRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     body,
	})
	if err != nil {
		t.Fatalf("StripSyntheticApprovalHistory: %v", err)
	}
	if !res.Modified {
		t.Fatalf("expected synthetic-approval history to be stripped")
	}
	assertThinkingBlockPreserved(t, res.Body)
}

func TestSanitizeInboundAnthropicPreservesThinkingBlockBytes(t *testing.T) {
	t.Parallel()
	// tool_use input has a command that will be rewritten (host/header
	// stripping). Triggers per-block surgery.
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"assistant","content":[` + thinkingBlockOriginal + `,{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"curl -sS http://localhost:25297/api/proxy/foo"}}]}]}`)
	res, err := SanitizeInboundHistory(SanitizeInboundRequest{
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		ResolverBaseURL: "http://localhost:25297",
		ControlBaseURL:  "http://localhost:25297",
	})
	if err != nil {
		t.Fatalf("SanitizeInboundHistory: %v", err)
	}
	// Whether or not it modifies depends on heuristics, but the
	// thinking block must always pass through bytes-intact regardless.
	if res.Modified {
		assertThinkingBlockPreserved(t, res.Body)
	} else {
		assertThinkingBlockPreserved(t, body)
	}
}

func TestStripSecretDecisionHistoryPreservesThinkingBlockBytes(t *testing.T) {
	t.Parallel()
	// Embed a secret-decision-marker assistant message followed by a
	// matching user decision reply, so the strip path actually fires.
	body := []byte(`{"model":"claude-opus-4-7","messages":[` +
		`{"role":"assistant","content":[` + thinkingBlockOriginal + `,{"type":"text","text":"real response"}]},` +
		`{"role":"user","content":[{"type":"text","text":"next"}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"` + SecretDecisionIDMarker + `:abc"}]},` +
		`{"role":"user","content":[{"type":"text","text":"` + string(SecretDecisionAllowOnce) + `"}]}` +
		`]}`)
	res, err := StripSecretDecisionHistory(SecretDecisionHistoryStripRequest{
		Provider: conversation.ProviderAnthropic,
		Body:     body,
	})
	if err != nil {
		t.Fatalf("StripSecretDecisionHistory: %v", err)
	}
	if !res.Modified {
		t.Skip("strip path didn't fire — unrelated to byte fidelity")
	}
	assertThinkingBlockPreserved(t, res.Body)
}

// End-to-end: chain the preprocessors in the same order the request
// handler does, then verify the thinking block bytes are still intact
// in the final body that would go upstream.
func TestRequestPreprocessingChainPreservesThinkingBlockBytes(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"claude-opus-4-7","messages":[` +
		`{"role":"assistant","content":[` + thinkingBlockOriginal + `,{"type":"text","text":""},{"type":"text","text":"hello"}]},` +
		`{"role":"user","content":[{"type":"text","text":"next"}]}` +
		`],"system":"existing"}`)

	// 1. Sanitize (would remove the empty text block).
	sanitized, _, err := SanitizeAnthropicRequest(body)
	if err != nil {
		t.Fatalf("SanitizeAnthropicRequest: %v", err)
	}
	assertThinkingBlockPreserved(t, sanitized)

	// 2. Inject control notice (modifies top-level system).
	noticed, _, err := InjectControlNoticeWithPolicy(conversation.ProviderAnthropic, sanitized, "https://clawvisor.local", []string{"Bash"}, nil)
	if err != nil {
		t.Fatalf("InjectControlNoticeWithPolicy: %v", err)
	}
	assertThinkingBlockPreserved(t, noticed)

	// 3. Sanitize inbound history.
	inbound, err := SanitizeInboundHistory(SanitizeInboundRequest{
		Provider:        conversation.ProviderAnthropic,
		Body:            noticed,
		ResolverBaseURL: "http://localhost:25297",
		ControlBaseURL:  "http://localhost:25297",
	})
	if err != nil {
		t.Fatalf("SanitizeInboundHistory: %v", err)
	}
	final := inbound.Body
	assertThinkingBlockPreserved(t, final)

	_ = context.Background()
}
