package llmproxy

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
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
		t.Fatalf("expected strip path to fire — the test fixture is explicitly constructed to trigger it; a regression that no longer fires would silently bypass byte-fidelity assertions")
	}
	assertThinkingBlockPreserved(t, res.Body)
}

// --- Observe-posture byte fidelity (spec 02 §3) ---
//
// Contract: a request that WOULD be denied / held / rewritten by an
// enforcing policy must produce the SAME upstream bytes as the same
// request with the triggering policy absent. This isolates verdict
// effects (which observe suppresses) from mechanical transforms (which
// still run). Exercised against the real pipeline.RunPre downgrade.

// fidelityRequest is a minimal pipeline.ReadOnlyRequest for these tests.
type fidelityRequest struct{ body []byte }

func (r *fidelityRequest) Provider() conversation.Provider { return conversation.ProviderAnthropic }
func (r *fidelityRequest) StreamShape() conversation.StreamShape {
	return conversation.StreamShapeUnknown
}
func (r *fidelityRequest) Turns() []conversation.Turn           { return nil }
func (r *fidelityRequest) HTTPRequest() *http.Request           { return nil }
func (r *fidelityRequest) RawBody() []byte                      { return append([]byte(nil), r.body...) }
func (r *fidelityRequest) IsFirstTurn() bool                    { return true }
func (r *fidelityRequest) ConversationID() string               { return "" }
func (r *fidelityRequest) UserID() string                       { return "" }
func (r *fidelityRequest) AgentID() string                      { return "" }
func (r *fidelityRequest) ValidateReplacementBody([]byte) error { return nil }

// redactingPolicy simulates a non-mechanical policy (e.g. content-policy
// redaction) that both rewrites the body and returns a deny. Its name is
// NOT on the observe-exempt allowlist, so observe mode must record it and
// pass the ORIGINAL bytes through.
type redactingPolicy struct {
	name    string
	newBody []byte
	deny    bool
}

func (p *redactingPolicy) Name() string { return p.name }
func (p *redactingPolicy) Preprocess(_ context.Context, _ pipeline.ReadOnlyRequest, mut pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	if len(p.newBody) > 0 {
		if err := mut.ReplaceBody(p.newBody); err != nil {
			return pipeline.RequestVerdict{}, err
		}
	}
	v := pipeline.RequestVerdict{Outcome: pipeline.OutcomeAllow, Reason: "would redact"}
	if p.deny {
		v.Outcome = pipeline.OutcomeDeny
	}
	return v, nil
}

func TestObserveModePreservesUpstreamBytesVsPolicyAbsent(t *testing.T) {
	t.Parallel()
	original := []byte(strings.ReplaceAll(thinkingBodyTemplate, "%s", "hello"))
	redacted := []byte(strings.ReplaceAll(thinkingBodyTemplate, "%s", "REDACTED"))

	// Control: with the triggering policy ABSENT, the body goes upstream
	// unchanged.
	absent, err := pipeline.RunPre(context.Background(), &fidelityRequest{body: original}, nil)
	if err != nil {
		t.Fatalf("RunPre (absent): %v", err)
	}

	// Observe: the SAME request with the enforcing redact policy present
	// must yield byte-identical upstream bytes to the absent case (the
	// redaction is recorded, not applied).
	observed, err := pipeline.RunPre(
		pipeline.WithObserveMode(context.Background()),
		&fidelityRequest{body: original},
		[]pipeline.RequestPolicy{&redactingPolicy{name: "org_content_policy", newBody: redacted}},
	)
	if err != nil {
		t.Fatalf("RunPre (observe): %v", err)
	}
	if string(observed.FinalBody) != string(absent.FinalBody) {
		t.Fatalf("observe mode altered upstream bytes:\n absent:  %s\n observe: %s", absent.FinalBody, observed.FinalBody)
	}
	assertThinkingBlockPreserved(t, observed.FinalBody)
	if len(observed.Observed) != 1 {
		t.Fatalf("expected the downgraded rewrite to be recorded once; got %d", len(observed.Observed))
	}

	// Enforce (control): the same policy under enforce DOES apply the
	// rewrite — proving the fixture actually mutates bytes when not
	// downgraded.
	enforced, err := pipeline.RunPre(
		context.Background(),
		&fidelityRequest{body: original},
		[]pipeline.RequestPolicy{&redactingPolicy{name: "org_content_policy", newBody: redacted}},
	)
	if err != nil {
		t.Fatalf("RunPre (enforce): %v", err)
	}
	if string(enforced.FinalBody) != string(redacted) {
		t.Fatal("enforce control should apply the rewrite — fixture is not actually mutating bytes")
	}
}

// TestObserveModeDowngradesDenyKeepsBody: a non-mechanical deny under
// observe is recorded but not enforced (no DenyReason), and the body is
// untouched.
func TestObserveModeDowngradesDenyKeepsBody(t *testing.T) {
	t.Parallel()
	original := []byte(strings.ReplaceAll(thinkingBodyTemplate, "%s", "hi"))
	res, err := pipeline.RunPre(
		pipeline.WithObserveMode(context.Background()),
		&fidelityRequest{body: original},
		[]pipeline.RequestPolicy{&redactingPolicy{name: "org_model_policy", deny: true}},
	)
	if err != nil {
		t.Fatalf("RunPre: %v", err)
	}
	if res.DenyReason != "" {
		t.Fatalf("observe must not deny; got DenyReason=%q", res.DenyReason)
	}
	if string(res.FinalBody) != string(original) {
		t.Fatal("observe deny downgrade must leave the body unchanged")
	}
	if len(res.Observed) != 1 || res.AuditParams["observed"] != true {
		t.Fatalf("deny downgrade must be recorded (Observed + audit); got Observed=%d audit=%v", len(res.Observed), res.AuditParams["observed"])
	}
}

// TestObserveModeExemptPolicyStillEnforces: a mechanical (exempt) policy
// still denies in observe mode — malformed-request rejection and other
// mechanical guards are not governance verdicts.
func TestObserveModeExemptPolicyStillEnforces(t *testing.T) {
	t.Parallel()
	original := []byte(strings.ReplaceAll(thinkingBodyTemplate, "%s", "hi"))
	res, err := pipeline.RunPre(
		pipeline.WithObserveMode(context.Background()),
		&fidelityRequest{body: original},
		[]pipeline.RequestPolicy{&redactingPolicy{name: "anthropic_sanitize", deny: true}},
	)
	if err != nil {
		t.Fatalf("RunPre: %v", err)
	}
	if res.DenyReason == "" {
		t.Fatal("exempt (mechanical) policy must still deny in observe mode")
	}
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
	assertThinkingBlockPreserved(t, inbound.Body)
}
