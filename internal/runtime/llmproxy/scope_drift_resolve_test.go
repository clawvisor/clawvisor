package llmproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

func TestParseScopeDriftDecisions_RawAndJSONEscapedQuotes(t *testing.T) {
	// Raw quotes — the markup as it would appear in plain text.
	raw := []byte(`prefix <clawvisor:decision drift="drift-1" option="justify">because X</clawvisor:decision> suffix`)
	got := parseScopeDriftDecisions(raw)
	if len(got) != 1 {
		t.Fatalf("raw: expected 1 decision, got %d", len(got))
	}
	if got[0].DriftID != "drift-1" || got[0].Option != "justify" || got[0].Body != "because X" {
		t.Errorf("raw: unexpected parse: %+v", got[0])
	}

	// JSON-escaped quotes — the markup as the model would emit it
	// inside a JSON string field. The body bytes the proxy sees
	// contain `\"` because the outer string is JSON-encoded.
	escaped := []byte(`{"text":"<clawvisor:decision drift=\"drift-2\" option=\"one-off\">throwaway probe</clawvisor:decision>"}`)
	got = parseScopeDriftDecisions(escaped)
	if len(got) != 1 {
		t.Fatalf("escaped: expected 1 decision, got %d", len(got))
	}
	if got[0].DriftID != "drift-2" || got[0].Option != "one-off" || got[0].Body != "throwaway probe" {
		t.Errorf("escaped: unexpected parse: %+v", got[0])
	}
}

func TestParseScopeDriftDecisions_MultipleAndAttrOrder(t *testing.T) {
	body := []byte(`first <clawvisor:decision option="justify" drift="d1">a</clawvisor:decision> and <clawvisor:decision drift="d2" option="one-off">b</clawvisor:decision> done`)
	got := parseScopeDriftDecisions(body)
	if len(got) != 2 {
		t.Fatalf("expected 2 decisions, got %d", len(got))
	}
	if got[0].DriftID != "d1" || got[0].Option != "justify" {
		t.Errorf("first decision wrong: %+v", got[0])
	}
	if got[1].DriftID != "d2" || got[1].Option != "one-off" {
		t.Errorf("second decision wrong: %+v", got[1])
	}
}

func TestParseScopeDriftDecisions_SkipsCodeFencedMarkup(t *testing.T) {
	// Triple-fenced block at line start: realistic echo case — the
	// model wraps the menu inside ``` to explain its plan. Should
	// NOT be parsed as an action.
	tripleFenced := []byte("```\n<clawvisor:decision drift=\"d1\" option=\"justify\">x</clawvisor:decision>\n```")
	if got := parseScopeDriftDecisions(tripleFenced); len(got) != 0 {
		t.Errorf("triple-fenced markup should be skipped, got %+v", got)
	}
	// Inline backtick (same line): similarly skipped.
	inlineFenced := []byte("the markup is `<clawvisor:decision drift=\"d2\" option=\"one-off\">x</clawvisor:decision>` here")
	if got := parseScopeDriftDecisions(inlineFenced); len(got) != 0 {
		t.Errorf("inline-fenced markup should be skipped, got %+v", got)
	}
	// Bare markup outside any fence — DOES parse.
	bare := []byte("decision: <clawvisor:decision drift=\"d3\" option=\"justify\">x</clawvisor:decision>")
	if got := parseScopeDriftDecisions(bare); len(got) != 1 {
		t.Errorf("bare markup should parse, got %d decisions", len(got))
	}
}

func TestParseScopeDriftDecisions_StrayBacktickEarlierDoesNotPoisonLaterMarkup(t *testing.T) {
	// Realistic prose: model talks about `something` on an early line
	// (inline backtick that closes on the same line), then later
	// emits the real decision on a different line. The earlier
	// backtick MUST NOT cause the later markup to be suppressed.
	body := []byte("First I considered `git status`.\n\nDecision: <clawvisor:decision drift=\"d1\" option=\"justify\">the call is in scope</clawvisor:decision>")
	got := parseScopeDriftDecisions(body)
	if len(got) != 1 {
		t.Fatalf("expected 1 decision (earlier backtick should not poison later line), got %d", len(got))
	}
	if got[0].DriftID != "d1" {
		t.Errorf("wrong drift parsed: %+v", got[0])
	}
}

func TestParseScopeDriftDecisions_UnclosedInlineBacktickStillScopedToLine(t *testing.T) {
	// An unbalanced single backtick on an earlier line (odd count)
	// would in a naïve parity counter mark every later byte as
	// "inside code." Scoping to same-line means the later markup
	// on its own line is unaffected.
	body := []byte("Here is an example with a stray ` backtick.\nNow the decision: <clawvisor:decision drift=\"d1\" option=\"justify\">in scope</clawvisor:decision>")
	got := parseScopeDriftDecisions(body)
	if len(got) != 1 {
		t.Fatalf("stray same-line backtick should not poison later lines; got %d decisions", len(got))
	}
}

func TestParseScopeDriftDecisions_MalformedDropped(t *testing.T) {
	// Missing drift and missing option attributes — both should be
	// silently dropped so a partial emit can't claim anything.
	body := []byte(`<clawvisor:decision option="justify">no drift attr</clawvisor:decision>` +
		`<clawvisor:decision drift="d3">no option attr</clawvisor:decision>`)
	if got := parseScopeDriftDecisions(body); len(got) != 0 {
		t.Fatalf("expected 0 decisions, got %d (%+v)", len(got), got)
	}
}

// stubVerifier captures the request and returns a predetermined verdict.
type stubVerifier struct {
	verdict *IntentVerdict
	err     error
	lastReq IntentVerifyRequest
}

func (s *stubVerifier) Verify(_ context.Context, req IntentVerifyRequest) (*IntentVerdict, error) {
	s.lastReq = req
	return s.verdict, s.err
}

func TestApplyScopeDriftDecisions_JustifyAccepted_InsertsPreClear(t *testing.T) {
	reg := NewMemoryScopeDriftRegistry(0)
	ctx := context.Background()

	drift, _ := reg.Register(ctx, ScopeDrift{
		UserID:  "user-1",
		AgentID: "agent-1",
		ToolUse: conversation.ToolUse{Name: "Bash"},
		Service: "github",
		Action:  "create_issue",
		Host:    "api.github.com",
		Method:  "POST",
		Path:    "/repos/x/y/issues",
		Source:  ScopeDriftSourceIntentVerification,
	})

	verifier := &stubVerifier{
		verdict: &IntentVerdict{Allow: true, Explanation: "fits the active task purpose"},
	}
	cfg := PostprocessConfig{
		AgentID:        "agent-1",
		AgentUserID:    "user-1",
		ScopeDrifts:    reg,
		IntentVerifier: verifier,
	}

	body := []byte(`{"text":"sure: <clawvisor:decision drift=\"` + drift.ID +
		`\" option=\"justify\">the call extends the existing audit purpose by reading a related issue</clawvisor:decision>"}`)
	out, _ := applyScopeDriftDecisions(ctx, cfg, conversation.ProviderAnthropic, body)

	// Markup must be substituted.
	if strings.Contains(string(out), "<clawvisor:decision") {
		t.Errorf("markup not substituted:\n%s", out)
	}
	if !strings.Contains(string(out), "verifier accepted your justification") {
		t.Errorf("status message missing acceptance phrase:\n%s", out)
	}
	// Resulting JSON must still parse.
	var probe map[string]any
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Errorf("substitution broke JSON: %v\n%s", err, out)
	}
	// Verifier received the justification.
	if !strings.Contains(verifier.lastReq.AgentJustification, "audit purpose") {
		t.Errorf("verifier did not receive justification: %q", verifier.lastReq.AgentJustification)
	}
	// Pre-clear is now consumable exactly once.
	if _, ok := reg.LookupPreClear(ctx, "agent-1", drift.Fingerprint()); !ok {
		t.Errorf("expected pre-clear after accepted justification")
	}
}

func TestApplyScopeDriftDecisions_JustifyRejected_NoPreClearMarksFallback(t *testing.T) {
	reg := NewMemoryScopeDriftRegistry(0)
	ctx := context.Background()
	drift, _ := reg.Register(ctx, ScopeDrift{
		AgentID: "agent-1",
		UserID:  "user-1",
		Service: "github",
		Action:  "create_issue",
		Source:  ScopeDriftSourceIntentVerification,
	})

	verifier := &stubVerifier{
		verdict: &IntentVerdict{Allow: false, Explanation: "params target unrelated entity"},
	}
	cfg := PostprocessConfig{
		AgentID:        "agent-1",
		AgentUserID:    "user-1",
		ScopeDrifts:    reg,
		IntentVerifier: verifier,
	}

	body := []byte(`<clawvisor:decision drift="` + drift.ID + `" option="justify">it fits because</clawvisor:decision>`)
	out, _ := applyScopeDriftDecisions(ctx, cfg, conversation.ProviderAnthropic, body)
	if strings.Contains(string(out), "<clawvisor:decision") {
		t.Errorf("markup not substituted:\n%s", out)
	}
	if !strings.Contains(string(out), "did not accept your justification") {
		t.Errorf("status message missing rejection phrase:\n%s", out)
	}
	// No pre-clear inserted on rejection.
	if _, ok := reg.LookupPreClear(ctx, "agent-1", drift.Fingerprint()); ok {
		t.Errorf("pre-clear inserted on rejected justification")
	}
	// Drift recorded as denied so a follow-up doesn't replay it.
	updated, _ := reg.Get(ctx, drift.ID)
	if updated.Outcome != ScopeDriftOutcomeDenied {
		t.Errorf("expected outcome denied, got %q", updated.Outcome)
	}
}

func TestApplyScopeDriftDecisions_JustifyOnTaskScopeDriftRefusesWithoutClaim(t *testing.T) {
	reg := NewMemoryScopeDriftRegistry(0)
	ctx := context.Background()
	drift, _ := reg.Register(ctx, ScopeDrift{
		AgentID: "agent-1",
		Source:  ScopeDriftSourceTaskScope, // not eligible for justify
	})
	cfg := PostprocessConfig{
		AgentID:     "agent-1",
		ScopeDrifts: reg,
		// IntentVerifier deliberately nil to assert the source check
		// fires before any verifier dependency is looked at.
	}

	body := []byte(`<clawvisor:decision drift="` + drift.ID + `" option="justify">i swear</clawvisor:decision>`)
	out, _ := applyScopeDriftDecisions(ctx, cfg, conversation.ProviderAnthropic, body)
	if !strings.Contains(string(out), "only applies when the block source is intent_verification") {
		t.Errorf("status message did not explain the source mismatch:\n%s", out)
	}
	// Drift NOT claimed — agent can still use (a)/(b).
	updated, _ := reg.Get(ctx, drift.ID)
	if updated.ChosenOption != "" {
		t.Errorf("source-mismatch path burned the one-shot cap; ChosenOption=%q", updated.ChosenOption)
	}
}

func TestApplyScopeDriftDecisions_JustifyVerifierErrorMarksFallbackNoPreClear(t *testing.T) {
	reg := NewMemoryScopeDriftRegistry(0)
	ctx := context.Background()
	drift, _ := reg.Register(ctx, ScopeDrift{
		AgentID: "agent-1",
		Service: "github",
		Action:  "create_issue",
		Source:  ScopeDriftSourceIntentVerification,
	})

	verifier := &stubVerifier{err: errors.New("upstream LLM down")}
	cfg := PostprocessConfig{
		AgentID:        "agent-1",
		ScopeDrifts:    reg,
		IntentVerifier: verifier,
	}

	body := []byte(`<clawvisor:decision drift="` + drift.ID + `" option="justify">argument</clawvisor:decision>`)
	out, _ := applyScopeDriftDecisions(ctx, cfg, conversation.ProviderAnthropic, body)
	if !strings.Contains(string(out), "verifier was unreachable") {
		t.Errorf("status message did not surface verifier error:\n%s", out)
	}
	updated, _ := reg.Get(ctx, drift.ID)
	if updated.Outcome != ScopeDriftOutcomeDenied {
		t.Errorf("expected outcome denied on verifier error, got %q", updated.Outcome)
	}
	if _, ok := reg.LookupPreClear(ctx, "agent-1", drift.Fingerprint()); ok {
		t.Errorf("pre-clear inserted on verifier error path")
	}
}

func TestApplyScopeDriftDecisions_OneOffCreatesUserApprovalHold(t *testing.T) {
	reg := NewMemoryScopeDriftRegistry(0)
	pending := NewMemoryPendingApprovalCache(0)
	ctx := context.Background()
	drift, _ := reg.Register(ctx, ScopeDrift{
		AgentID: "agent-1",
		UserID:  "user-1",
		Service: "github",
		Action:  "create_issue",
		Source:  ScopeDriftSourceIntentVerification,
	})
	cfg := PostprocessConfig{
		AgentID:          "agent-1",
		AgentUserID:      "user-1",
		ScopeDrifts:      reg,
		PendingApprovals: pending,
	}

	body := []byte(`<clawvisor:decision drift="` + drift.ID + `" option="one-off">quick diagnostic check, not repeated</clawvisor:decision>`)
	out, _ := applyScopeDriftDecisions(ctx, cfg, conversation.ProviderAnthropic, body)
	// The user-facing approval prompt is substituted into the body
	// in place of the markup.
	if !strings.Contains(string(out), "Clawvisor: the agent requested a one-off execution") {
		t.Errorf("approval prompt header missing from substitution:\n%s", out)
	}
	if !strings.Contains(string(out), "quick diagnostic check") {
		t.Errorf("agent rationale missing from substitution:\n%s", out)
	}
	if !strings.Contains(string(out), "Reply `yes` or `y`") {
		t.Errorf("yes/no instructions missing:\n%s", out)
	}
	// The drift is claimed (one-shot cap honored) but outcome stays
	// pending until the user replies.
	updated, _ := reg.Get(ctx, drift.ID)
	if updated.ChosenOption != ScopeDriftOptionOneOff {
		t.Errorf("expected ChosenOption=one_off, got %q", updated.ChosenOption)
	}
	if updated.Outcome != ScopeDriftOutcomePending {
		t.Errorf("expected outcome=pending while user is deciding, got %q", updated.Outcome)
	}
	// A pending approval hold should have been created so the user's
	// "yes"/"no" reply can route to the scope-drift reply rewriter.
	if got := snapshotPendingApprovals(pending, "user-1", "agent-1", conversation.ProviderAnthropic); len(got) != 1 {
		t.Fatalf("expected exactly 1 pending hold, got %d", len(got))
	} else if got[0].Stage != StageAwaitingScopeDriftOneOff {
		t.Errorf("hold has wrong stage: %q", got[0].Stage)
	} else if got[0].ScopeDriftID != drift.ID {
		t.Errorf("hold's ScopeDriftID doesn't match: %q", got[0].ScopeDriftID)
	} else if got[0].Provider != conversation.ProviderAnthropic {
		t.Errorf("hold has wrong provider: %q (the reply rewriter peeks by provider, so a mismatch here means the user's yes/no reply can't find the hold)", got[0].Provider)
	}
}

// TestApplyScopeDriftDecisions_AgentNoteSpecialCharsKeepBodyValidJSON
// proves the substitution is JSON-escaped when spliced back into the
// response body. Without escaping, an agent note containing `"` or
// `\` would close the surrounding JSON string and corrupt the body
// the client deserializes — leaving the user with a parse error
// instead of the proxy's prompt.
func TestApplyScopeDriftDecisions_AgentNoteSpecialCharsKeepBodyValidJSON(t *testing.T) {
	reg := NewMemoryScopeDriftRegistry(0)
	pending := NewMemoryPendingApprovalCache(0)
	ctx := context.Background()
	drift, _ := reg.Register(ctx, ScopeDrift{
		AgentID: "agent-1",
		UserID:  "user-1",
		Service: "github",
		Action:  "create_issue",
		Source:  ScopeDriftSourceIntentVerification,
	})
	cfg := PostprocessConfig{
		AgentID:          "agent-1",
		AgentUserID:      "user-1",
		ScopeDrifts:      reg,
		PendingApprovals: pending,
	}
	// Agent note carries every char class likely to bite a naive
	// splicer: bare quotes, backslash, newline, tab. Encode the JSON
	// string field manually with an encoder that leaves `<` / `>`
	// alone — Anthropic's API does not HTML-escape, and using the
	// stdlib json.Marshal here would produce `<` for `<`, hiding
	// the markup from the parser pattern (which is a separate
	// concern from the splice safety this test guards).
	note := `quote " then \backslash and ` + "\n" + ` newline ` + "\t" + ` tab`
	innerText := `<clawvisor:decision drift="` + drift.ID + `" option="one-off">` + note + `</clawvisor:decision>`
	var jsonBuf bytes.Buffer
	enc := json.NewEncoder(&jsonBuf)
	enc.SetEscapeHTML(false)
	if mErr := enc.Encode(innerText); mErr != nil {
		t.Fatalf("encode inner text: %v", mErr)
	}
	// json.Encoder.Encode appends a newline; trim it so the bytes are
	// suitable for substitution into a larger document.
	encoded := bytes.TrimRight(jsonBuf.Bytes(), "\n")
	body := []byte(`{"content":[{"type":"text","text":` + string(encoded) + `}]}`)
	out, _ := applyScopeDriftDecisions(ctx, cfg, conversation.ProviderAnthropic, body)
	var probe struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("substituted body is not valid JSON: %v\nbody:\n%s", err, out)
	}
	if len(probe.Content) == 0 || !strings.Contains(probe.Content[0].Text, "the agent requested a one-off") {
		t.Errorf("substitution header missing from decoded text: %#v", probe)
	}
}

func TestApplyScopeDriftDecisions_OneOffWithoutCacheDegradesGracefully(t *testing.T) {
	reg := NewMemoryScopeDriftRegistry(0)
	ctx := context.Background()
	drift, _ := reg.Register(ctx, ScopeDrift{
		AgentID: "agent-1",
		Source:  ScopeDriftSourceIntentVerification,
	})
	cfg := PostprocessConfig{
		AgentID:     "agent-1",
		ScopeDrifts: reg,
		// PendingApprovals deliberately nil
	}
	body := []byte(`<clawvisor:decision drift="` + drift.ID + `" option="one-off">x</clawvisor:decision>`)
	out, _ := applyScopeDriftDecisions(ctx, cfg, conversation.ProviderAnthropic, body)
	if !strings.Contains(string(out), "pending-approval cache is not configured") {
		t.Errorf("expected misconfiguration status, got:\n%s", out)
	}
	// Drift not claimed when cache is missing — agent can still try (a)/(b)/(d).
	updated, _ := reg.Get(ctx, drift.ID)
	if updated.ChosenOption != "" {
		t.Errorf("cache-missing path burned the one-shot cap; ChosenOption=%q", updated.ChosenOption)
	}
}

// TestStreamingDriftSubstitution_RoundTrips reproduces the live e2e
// streaming path's expectations: an AssistantTurn carrying the
// markup, a wired registry + pending-approval cache, and a provider.
// Asserts that streamingDriftSubstitution returns the user-facing
// approval prompt (so the streaming write path can append it as a
// follow-up text block) AND that the pending hold lands with the
// right Provider so the reply rewriter on the next turn can find it.
func TestStreamingDriftSubstitution_RoundTrips(t *testing.T) {
	reg := NewMemoryScopeDriftRegistry(0)
	pending := NewMemoryPendingApprovalCache(0)
	ctx := context.Background()
	drift, _ := reg.Register(ctx, ScopeDrift{
		AgentID: "agent-1",
		UserID:  "user-1",
		Service: "github",
		Action:  "add_issue_comment",
		Source:  ScopeDriftSourceIntentVerification,
	})
	cfg := PostprocessConfig{
		AgentID:          "agent-1",
		AgentUserID:      "user-1",
		ScopeDrifts:      reg,
		PendingApprovals: pending,
	}

	// Mirrors the agent's assembled assistant-text content as it
	// would appear in streamResult.AssistantTurn.Content after the
	// stream parser concatenates fragments.
	turn := &conversation.Turn{
		Role:    conversation.RoleAssistant,
		Content: `You asked for a one-off, so I'll go with **(c)**:` + "\n\n" + `<clawvisor:decision drift="` + drift.ID + `" option="one-off">Single courtesy comment on one issue.</clawvisor:decision>`,
	}

	out := streamingDriftSubstitution(ctx, cfg, conversation.ProviderAnthropic, turn)
	if out == "" {
		t.Fatal("expected non-empty substitution")
	}
	if !strings.Contains(out, "Clawvisor: the agent requested a one-off execution") {
		t.Errorf("substitution missing one-off prompt header:\n%s", out)
	}

	// The pending hold MUST carry Provider matching the streaming
	// path's value so the reply rewriter's bare-yes/no lookup
	// resolves to this hold rather than miss.
	holds := snapshotPendingApprovals(pending, "user-1", "agent-1", conversation.ProviderAnthropic)
	if len(holds) != 1 {
		t.Fatalf("expected 1 hold under ProviderAnthropic, got %d", len(holds))
	}
	if holds[0].Stage != StageAwaitingScopeDriftOneOff {
		t.Errorf("wrong stage: %q", holds[0].Stage)
	}
}

// snapshotPendingApprovals exposes the cache's holds for assertions in
// tests. The production code path doesn't need this — release/peek
// are the supported lookup surfaces.
func snapshotPendingApprovals(cache PendingApprovalCache, userID, agentID string, provider conversation.Provider) []PendingLiteApproval {
	mem, ok := cache.(*MemoryPendingApprovalCache)
	if !ok {
		return nil
	}
	return mem.snapshotHoldsForTest(userID, agentID, provider)
}

func TestApplyScopeDriftDecisions_CrossAgentDriftRefused(t *testing.T) {
	reg := NewMemoryScopeDriftRegistry(0)
	ctx := context.Background()
	drift, _ := reg.Register(ctx, ScopeDrift{
		AgentID: "agent-other",
		Source:  ScopeDriftSourceIntentVerification,
	})
	cfg := PostprocessConfig{
		AgentID:     "agent-1",
		ScopeDrifts: reg,
	}

	body := []byte(`<clawvisor:decision drift="` + drift.ID + `" option="justify">x</clawvisor:decision>`)
	out, _ := applyScopeDriftDecisions(ctx, cfg, conversation.ProviderAnthropic, body)
	if !strings.Contains(string(out), "minted for a different agent") {
		t.Errorf("status message did not flag cross-agent claim:\n%s", out)
	}
	updated, _ := reg.Get(ctx, drift.ID)
	if updated.ChosenOption != "" {
		t.Errorf("cross-agent path burned the one-shot cap")
	}
}

// TestApplyScopeDriftDecisions_CrossConversationDriftRefused pins the
// conversation-scope check: a drift minted for the same agent in
// conversation A must not be claimable from conversation B (e.g. an
// agent that finds the drift_id in stale assistant history copied
// across sessions). Without the check, the claim would consume the
// one-shot cap and burn the drift on the wrong session.
func TestApplyScopeDriftDecisions_CrossConversationDriftRefused(t *testing.T) {
	reg := NewMemoryScopeDriftRegistry(0)
	ctx := context.Background()
	drift, _ := reg.Register(ctx, ScopeDrift{
		AgentID:        "agent-1",
		ConversationID: "conv-A",
		Source:         ScopeDriftSourceIntentVerification,
	})
	cfg := PostprocessConfig{
		AgentID:        "agent-1",
		ConversationID: "conv-B",
		ScopeDrifts:    reg,
	}
	body := []byte(`<clawvisor:decision drift="` + drift.ID + `" option="justify">x</clawvisor:decision>`)
	out, _ := applyScopeDriftDecisions(ctx, cfg, conversation.ProviderAnthropic, body)
	if !strings.Contains(string(out), "different conversation") {
		t.Errorf("status message did not flag cross-conversation claim:\n%s", out)
	}
	updated, _ := reg.Get(ctx, drift.ID)
	if updated.ChosenOption != "" {
		t.Errorf("cross-conversation path burned the one-shot cap (ChosenOption=%q)", updated.ChosenOption)
	}
}

func TestApplyScopeDriftDecisions_NoMarkupReturnsUnchanged(t *testing.T) {
	reg := NewMemoryScopeDriftRegistry(0)
	cfg := PostprocessConfig{AgentID: "agent-1", ScopeDrifts: reg}
	in := []byte(`{"content":[{"type":"text","text":"plain assistant text with no markup"}]}`)
	out, _ := applyScopeDriftDecisions(context.Background(), cfg, conversation.ProviderAnthropic, in)
	if string(out) != string(in) {
		t.Errorf("body mutated unexpectedly:\nin : %s\nout: %s", in, out)
	}
}

func TestApplyScopeDriftDecisions_NoRegistryIsNoOp(t *testing.T) {
	cfg := PostprocessConfig{AgentID: "agent-1"} // ScopeDrifts nil
	in := []byte(`<clawvisor:decision drift="x" option="justify">y</clawvisor:decision>`)
	out, _ := applyScopeDriftDecisions(context.Background(), cfg, conversation.ProviderAnthropic, in)
	if string(out) != string(in) {
		t.Errorf("nil registry should be a no-op; got mutation")
	}
}
