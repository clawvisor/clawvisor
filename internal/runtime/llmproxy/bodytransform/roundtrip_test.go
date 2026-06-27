package bodytransform_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/bodytransform"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/controltool"
)

// TestRewriteSanitizeRoundTrip pins the inverse-property contract
// between the per-tool rewriter (controltool.RewriteControlToolUse —
// rewrites the model's synthetic clawvisor.local URL into the
// resolver URL + injects auth headers) and the inbound sanitizer
// (bodytransform.SanitizeInboundHistory — runs on the next turn's
// inbound body and reverts the same artifacts before the model sees
// its own history).
//
// Why this matters: the rewriter mutates tool_use bytes in the
// outbound SSE stream so the harness's exec dials the resolver
// directly, but the harness records those mutated bytes verbatim as
// the assistant's emission. Without a tight inverse on the next
// inbound, the model accumulates the post-rewrite shape (resolver
// URL, stale nonces) as a self-taught exemplar and emits the wrong
// shape on subsequent turns — which the rewriter's host-gate then
// skips, the harness's exec runs the unrewritten curl, and the
// model misreads the resulting connection-refused as "Clawvisor's
// control plane is down."
//
// Each case is a model-realistic synthetic-shape curl. We:
//
//  1. Wrap the input in an Anthropic-shaped /v1/messages body as a
//     single assistant tool_use.
//  2. Send it through the rewriter (with a fixed nonce) to produce
//     the bytes the harness would record after one rewrite turn.
//  3. Send THOSE bytes (now living in conversation history) through
//     the sanitizer.
//  4. Assert byte-for-byte equality with the original synthetic
//     command. Any drift here is a leak the model would learn from
//     on the next turn.
//
// The cases cover every model-realistic synthetic shape today:
// GET reads, POST with --data heredoc, POST with --data inline,
// extra harmless flags, and pre-existing model headers that must
// survive the round-trip.
func TestRewriteSanitizeRoundTrip(t *testing.T) {
	const (
		controlBase  = "http://localhost:25297"
		resolverBase = "http://localhost:25297/api/proxy"
		callerToken  = "cv-nonce-roundtrip0test001"
	)

	cases := []struct {
		name    string
		command string
	}{
		{
			name:    "vault items GET",
			command: `curl -sS 'https://clawvisor.local/control/vault/items'`,
		},
		{
			name:    "skill index GET",
			command: `curl -sS 'https://clawvisor.local/control/skill'`,
		},
		{
			name:    "tasks POST surface inline (heredoc)",
			command: "curl -sS -X POST 'https://clawvisor.local/control/tasks?surface=inline' -H 'Content-Type: application/json' --data @- <<'JSON'\n{\"purpose\":\"do thing\",\"expected_tools\":[]}\nJSON",
		},
		{
			name:    "tasks POST surface inline (inline data)",
			command: `curl -sS -X POST 'https://clawvisor.local/control/tasks?surface=inline' -H 'Content-Type: application/json' --data '{"purpose":"do thing"}'`,
		},
		{
			name:    "task checkout POST",
			command: `curl -sS -X POST 'https://clawvisor.local/control/task/checkout' -H 'Content-Type: application/json' --data '{"task_id":"t-abc"}'`,
		},
		{
			name:    "task expand POST (path with id)",
			command: `curl -sS -X POST 'https://clawvisor.local/control/tasks/t-abc/expand?surface=inline' -H 'Content-Type: application/json' --data '{"additional_tools":[]}'`,
		},
		{
			name:    "failure POST",
			command: `curl -sS -X POST 'https://clawvisor.local/control/failure?reason=malformed' -H 'Content-Type: application/json' --data '{"original_command":"junk"}'`,
		},
		{
			name:    "autovault script GET",
			command: `curl -sS 'https://clawvisor.local/control/autovault/script'`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			toolUse := conversation.ToolUse{
				ID:    "toolu_rt_" + strings.ReplaceAll(tc.name, " ", "_"),
				Name:  "Bash",
				Input: mustMarshal(t, map[string]any{"command": tc.command}),
			}
			rewrittenInput, _, ok, err := controltool.RewriteControlToolUse(toolUse, controlBase, callerToken, "")
			if err != nil {
				t.Fatalf("rewrite returned error: %v", err)
			}
			if !ok {
				t.Fatalf("rewrite refused to rewrite this command — verdict didn't recognize it; rewriter contract requires every model-taught synthetic shape to be rewritable")
			}
			// Sanity-check the rewriter actually mutated the command:
			// if the inverse contract is vacuously true (rewriter was a
			// no-op) the test isn't pinning anything.
			rewrittenCommand := extractCommand(t, rewrittenInput)
			if rewrittenCommand == tc.command {
				t.Fatalf("rewrite did not mutate the command — round-trip is vacuous; command: %q", tc.command)
			}
			if !strings.Contains(rewrittenCommand, "X-Clawvisor-Target-Host") {
				t.Errorf("rewrite missing X-Clawvisor-Target-Host injection — sanitize won't have anything to strip; rewritten: %q", rewrittenCommand)
			}
			if !strings.Contains(rewrittenCommand, "X-Clawvisor-Caller") {
				t.Errorf("rewrite missing X-Clawvisor-Caller injection — sanitize won't have the auth header to strip; rewritten: %q", rewrittenCommand)
			}
			if !strings.Contains(rewrittenCommand, callerToken) {
				t.Errorf("rewrite missing caller nonce in command; rewritten: %q", rewrittenCommand)
			}
			if strings.Contains(rewrittenCommand, "clawvisor.local/control") {
				t.Errorf("rewrite left synthetic URL intact — host-swap didn't happen; rewritten: %q", rewrittenCommand)
			}

			// Wrap the rewritten input as a one-message inbound body and
			// run sanitize.
			body := buildAnthropicBody(t, toolUse.ID, rewrittenInput)
			res, err := bodytransform.SanitizeInboundHistory(bodytransform.SanitizeInboundRequest{
				Provider:        conversation.ProviderAnthropic,
				Body:            body,
				ResolverBaseURL: resolverBase,
				ControlBaseURL:  controlBase,
			})
			if err != nil {
				t.Fatalf("sanitize returned error: %v", err)
			}
			if !res.Modified {
				t.Fatalf("sanitize was a no-op against rewritten body — that means the round-trip would leave the rewriter's artifacts in conversation history forever")
			}
			sanitizedCommand := extractCommandFromBody(t, res.Body)

			// The core property: sanitize fully reverses the rewriter,
			// down to byte equality with the model's original synthetic
			// shape. Any drift is a leak — the rewriter's artifacts
			// become the model's exemplar.
			if sanitizedCommand != tc.command {
				t.Errorf("round-trip drift:\n   original: %q\n  sanitized: %q\n rewritten: %q", tc.command, sanitizedCommand, rewrittenCommand)
			}

			// Defense-in-depth: even if a future rewriter shape sneaks
			// past the equality check above, no rewriter artifact may
			// survive in the sanitized body.
			if strings.Contains(sanitizedCommand, "X-Clawvisor-") {
				t.Errorf("sanitized command still carries X-Clawvisor-* header artifact: %q", sanitizedCommand)
			}
			if strings.Contains(sanitizedCommand, "cv-nonce-") {
				t.Errorf("sanitized command still carries cv-nonce-* token leak: %q", sanitizedCommand)
			}
			if strings.Contains(sanitizedCommand, "localhost:25297") {
				t.Errorf("sanitized command still carries resolver host: %q", sanitizedCommand)
			}
			if strings.Contains(sanitizedCommand, "/api/control/") {
				t.Errorf("sanitized command still carries resolver-side /api/control path: %q", sanitizedCommand)
			}
		})
	}
}

// TestRewriteSanitizeRoundTrip_PreservesUnrelatedHeaders pins one
// adjacent property the inverse-pair test doesn't cover: when the
// model emits a control curl that already includes non-injected
// headers (Content-Type, Accept, etc.), those headers must survive
// the round-trip. Sanitize only strips the rewriter's known names;
// every other header is the model's own and may not be touched.
func TestRewriteSanitizeRoundTrip_PreservesUnrelatedHeaders(t *testing.T) {
	const (
		controlBase  = "http://localhost:25297"
		resolverBase = "http://localhost:25297/api/proxy"
		callerToken  = "cv-nonce-preserve000001"
	)
	command := `curl -sS -X POST 'https://clawvisor.local/control/tasks?surface=inline' -H 'Content-Type: application/json' -H 'Accept: application/json' --data '{"purpose":"x"}'`
	toolUse := conversation.ToolUse{
		ID:    "toolu_preserve",
		Name:  "Bash",
		Input: mustMarshal(t, map[string]any{"command": command}),
	}
	rewrittenInput, _, ok, err := controltool.RewriteControlToolUse(toolUse, controlBase, callerToken, "")
	if err != nil || !ok {
		t.Fatalf("rewrite ok=%v err=%v", ok, err)
	}
	body := buildAnthropicBody(t, toolUse.ID, rewrittenInput)
	res, err := bodytransform.SanitizeInboundHistory(bodytransform.SanitizeInboundRequest{
		Provider:        conversation.ProviderAnthropic,
		Body:            body,
		ResolverBaseURL: resolverBase,
		ControlBaseURL:  controlBase,
	})
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	sanitized := extractCommandFromBody(t, res.Body)
	for _, want := range []string{
		"Content-Type: application/json",
		"Accept: application/json",
		`'{"purpose":"x"}'`,
	} {
		if !strings.Contains(sanitized, want) {
			t.Errorf("sanitize dropped model-emitted artifact %q from %q", want, sanitized)
		}
	}
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}

// extractCommand parses a rewriter output (tool_use input bytes) and
// returns the command field. Tools may use `cmd` or `command`; the
// rewriter preserves whichever name was on the input.
func extractCommand(t *testing.T, input []byte) string {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(input, &raw); err != nil {
		t.Fatalf("unmarshal tool_use input: %v", err)
	}
	if s, ok := raw["command"].(string); ok {
		return s
	}
	if s, ok := raw["cmd"].(string); ok {
		return s
	}
	t.Fatalf("tool_use input missing command/cmd: %s", input)
	return ""
}

func extractCommandFromBody(t *testing.T, body []byte) string {
	t.Helper()
	// content may be a plain string (user turns) or an array of blocks
	// (assistant turns). Decode with json.RawMessage and walk only
	// array-shaped content.
	var b struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatalf("unmarshal sanitized body: %v", err)
	}
	for _, m := range b.Messages {
		if m.Role != "assistant" {
			continue
		}
		var blocks []struct {
			Type  string          `json:"type"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			continue
		}
		for _, blk := range blocks {
			if blk.Type == "tool_use" {
				return extractCommand(t, blk.Input)
			}
		}
	}
	t.Fatalf("no assistant tool_use in body: %s", body)
	return ""
}

func buildAnthropicBody(t *testing.T, toolUseID string, input json.RawMessage) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []map[string]any{
			{"role": "user", "content": "do the thing"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "tool_use", "id": toolUseID, "name": "Bash", "input": json.RawMessage(input)},
			}},
		},
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return body
}
