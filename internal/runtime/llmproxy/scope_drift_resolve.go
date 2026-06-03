package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// scopeDriftDecisionPattern matches a single
// `<clawvisor:decision drift="..." option="...">BODY</clawvisor:decision>`
// block in the body bytes returned to the harness. The opening tag's
// attributes may appear in any order; values may be wrapped in either
// raw `"` or JSON-escaped `\"` because the markup is emitted by the
// model inside a JSON string field, and the bytes we see here are the
// JSON-encoded body (where `"` becomes `\"` but `<` and `>` are not
// escaped by any common provider).
//
// The pattern is non-greedy on the body so consecutive markup blocks in
// one response are treated as separate decisions.
var scopeDriftDecisionPattern = regexp.MustCompile(
	`(?s)<clawvisor:decision\b([^>]*)>(.*?)</clawvisor:decision>`,
)

// scopeDriftAttrPattern matches `name="value"` and `name=\"value\"` (JSON-
// escaped) attributes inside the opening tag. Whitespace around `=` is
// tolerated so a sloppy emit doesn't silently disappear.
var scopeDriftAttrPattern = regexp.MustCompile(
	`(\w+)\s*=\s*\\?"([^"\\]*)\\?"`,
)

// scopeDriftDecision is one parsed markup block, recorded with byte
// offsets into the source body so the resolver can substitute it
// in-place.
type scopeDriftDecision struct {
	DriftID string
	Option  string
	Body    string
	// Start and End bound the matched markup in the source bytes
	// (inclusive of the opening `<clawvisor:decision...>` and the
	// closing `</clawvisor:decision>`). The resolver replaces this
	// range with a JSON-safe status string.
	Start int
	End   int
}

// parseScopeDriftDecisions returns every well-formed markup block in
// body. Malformed blocks (missing drift= or option=, blank body) are
// dropped so a partial emit doesn't accidentally claim an option.
// Markup that appears inside a markdown code fence (`` ``` `` or
// `` ` ``) is skipped — when the model echoes or quotes the menu
// inside an explanation, the markup is decorative, not an action.
func parseScopeDriftDecisions(body []byte) []scopeDriftDecision {
	matches := scopeDriftDecisionPattern.FindAllSubmatchIndex(body, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]scopeDriftDecision, 0, len(matches))
	for _, m := range matches {
		// m[0],m[1] full match
		// m[2],m[3] attribute group
		// m[4],m[5] body group
		if isInsideCodeFence(body, m[0]) {
			continue
		}
		attrs := string(body[m[2]:m[3]])
		raw := strings.TrimSpace(string(body[m[4]:m[5]]))
		d := scopeDriftDecision{
			Body:  raw,
			Start: m[0],
			End:   m[1],
		}
		for _, am := range scopeDriftAttrPattern.FindAllStringSubmatch(attrs, -1) {
			switch am[1] {
			case "drift":
				d.DriftID = am[2]
			case "option":
				d.Option = strings.ToLower(am[2])
			}
		}
		if d.DriftID == "" || d.Option == "" {
			continue
		}
		out = append(out, d)
	}
	return out
}

// isInsideCodeFence reports whether the byte at offset is wrapped in a
// markdown code construct that should suppress markup parsing. Two
// constructs are recognized, scoped narrowly so an unrelated earlier
// backtick can't cause valid later markup to be silently dropped:
//
//   - Inline code span (single backtick): checked ONLY within the
//     same line as the markup. An odd count of unmatched backticks
//     between the start of the line and the match offset means the
//     match is inside a code span. Backticks earlier in the body but
//     on other lines do not poison the parity (inline code spans
//     don't cross newlines).
//   - Triple-fence block (`` ``` ``): a fence opens with `` ``` `` at
//     the start of a line and closes with another fence at the start
//     of a later line. We walk forward from the start of body
//     tracking only line-leading fences so prose-embedded triples
//     can't open a faux block.
//
// False positives (suppressing a real decision because the model
// happened to type the markup inside what looks like a code block)
// are preferable to false negatives (claiming a drift because the
// model echoed the menu inside a quoted example).
func isInsideCodeFence(body []byte, offset int) bool {
	if offset <= 0 || offset > len(body) {
		return false
	}
	if insideTripleFenceBlock(body, offset) {
		return true
	}
	return insideInlineCodeSpan(body, offset)
}

// insideTripleFenceBlock walks the body up to offset tracking only
// line-leading `` ``` `` fences. Each line-leading fence toggles the
// "inside a block" state; fences in the middle of a line are ignored.
func insideTripleFenceBlock(body []byte, offset int) bool {
	inside := false
	lineStart := 0
	for i := 0; i < offset; i++ {
		if body[i] != '\n' && i != 0 {
			continue
		}
		if i != 0 {
			lineStart = i + 1
		}
		// Peek at the first non-space chars on the line.
		j := lineStart
		for j < offset && (body[j] == ' ' || body[j] == '\t') {
			j++
		}
		if j+3 <= offset && body[j] == '`' && body[j+1] == '`' && body[j+2] == '`' {
			inside = !inside
		}
	}
	return inside
}

// insideInlineCodeSpan checks single-backtick parity only on the same
// line as the offset. Inline spans don't cross newlines so a stray
// backtick on a different line is irrelevant.
func insideInlineCodeSpan(body []byte, offset int) bool {
	// Locate the start of the current line.
	lineStart := offset
	for lineStart > 0 && body[lineStart-1] != '\n' {
		lineStart--
	}
	count := 0
	i := lineStart
	for i < offset {
		if body[i] == '`' {
			// Treat a run of three or more backticks as a fence
			// marker (handled by insideTripleFenceBlock) and skip
			// the whole run so it doesn't pollute parity for the
			// inline check.
			if i+2 < offset && body[i+1] == '`' && body[i+2] == '`' {
				for i < offset && body[i] == '`' {
					i++
				}
				continue
			}
			count++
		}
		i++
	}
	return count%2 == 1
}

// applyScopeDriftDecisions scans body for <clawvisor:decision> markup
// blocks and, for each one tied to a known pending drift for this
// agent, claims the option and acts on it server-side. The matched
// markup ranges are substituted with a JSON-safe status string so the
// model's next-turn context contains the proxy's decision, not the raw
// markup. Returns the (possibly mutated) body bytes.
//
// Substitution preserves JSON validity by emitting plain-ASCII status
// strings with no unescaped quotes, backslashes, or control characters.
// The status strings are embedded directly into the JSON string field
// where the markup originally appeared; downstream parsers see normal
// assistant text.
//
// Errors are logged via the audit emitter (when configured) but never
// propagated: a malformed drift, an expired drift, or a verifier outage
// degrades to a status message in the body rather than failing the
// whole turn. Pre-clears for the original tool call are only inserted
// on a successful resolution.
func applyScopeDriftDecisions(ctx context.Context, cfg PostprocessConfig, provider conversation.Provider, body []byte) ([]byte, bool) {
	if cfg.ScopeDrifts == nil || len(body) == 0 {
		return body, false
	}
	decisions := parseScopeDriftDecisions(body)
	if len(decisions) == 0 {
		return body, false
	}
	// Walk right-to-left so earlier substitutions don't invalidate
	// later byte offsets.
	for i := len(decisions) - 1; i >= 0; i-- {
		d := decisions[i]
		status := resolveScopeDriftDecision(ctx, cfg, provider, d)
		// The markup byte range we replace sits INSIDE a JSON string
		// field (the assistant text content of the response body). Raw
		// status bytes containing `"`, `\`, or control characters
		// would break that enclosing string and corrupt the whole
		// response. The option-(c) one-off prompt embeds the agent's
		// note verbatim and can carry both. JSON-escape the substitute
		// before splicing so the body stays well-formed.
		body = spliceBytes(body, d.Start, d.End, jsonEscapeForStringBody(status))
	}
	return body, true
}

// jsonEscapeForStringBody returns the JSON-string-encoded form of s
// WITHOUT the surrounding double quotes — suitable for splicing
// inside an existing JSON string field. json.Marshal handles all the
// edge cases (control chars, surrogate pairs, the standard set of
// special chars), so we lean on the stdlib rather than re-implementing
// the escape table. On Marshal error (shouldn't happen for a string
// input), fall back to a conservative strip via sanitizeStatusValue
// so we still produce valid-JSON bytes.
func jsonEscapeForStringBody(s string) []byte {
	encoded, err := json.Marshal(s)
	if err != nil {
		return []byte(sanitizeStatusValue(s))
	}
	if len(encoded) < 2 {
		return encoded
	}
	return encoded[1 : len(encoded)-1]
}

// resolveScopeDriftDecision claims and acts on one decision, returning
// the JSON-safe status string to substitute for the markup. Every code
// path returns a string — even on errors — so the body substitution
// always produces valid JSON content.
func resolveScopeDriftDecision(ctx context.Context, cfg PostprocessConfig, provider conversation.Provider, d scopeDriftDecision) string {
	drift, err := cfg.ScopeDrifts.Get(ctx, d.DriftID)
	if err != nil {
		return scopeDriftStatus("Clawvisor: drift " + d.DriftID + " not found (it may have expired). Re-emit the original tool call to get a fresh menu.")
	}
	if drift.AgentID != cfg.AgentID {
		// Cross-agent drift claim — refuse without claiming. Could
		// happen with a stale conversation history copied across
		// sessions.
		return scopeDriftStatus("Clawvisor: drift " + d.DriftID + " was minted for a different agent and cannot be resolved here.")
	}

	switch d.Option {
	case "justify":
		return resolveJustify(ctx, cfg, drift, d.Body)
	case "one-off", "one_off", "oneoff":
		return resolveOneOff(ctx, cfg, provider, drift, d.Body)
	default:
		return scopeDriftStatus("Clawvisor: unknown decision option \"" + sanitizeStatusValue(d.Option) + "\". Valid values: justify, one-off.")
	}
}

// resolveJustify re-runs the verifier with the agent's justification
// threaded into AgentJustification. On accept, the drift's pre-clear is
// inserted so the agent's retry of the original tool call passes scope
// + intent verification once. On reject, the path documents the rejection
// and tells the agent to switch to (a)/(b) — the automatic fallback to
// (c) lives on top of the one-off user-approval channel, which is
// covered by a follow-up PR.
func resolveJustify(ctx context.Context, cfg PostprocessConfig, drift ScopeDrift, justification string) string {
	if drift.Source != ScopeDriftSourceIntentVerification {
		return scopeDriftStatus("Clawvisor: option (d) only applies when the block source is intent_verification. This drift was caused by task_scope — use (a) expand or (b) new task instead.")
	}
	if cfg.IntentVerifier == nil {
		return scopeDriftStatus("Clawvisor: intent verifier is not configured on this daemon. The drift remains unclaimed — pick option (a) expand or (b) new task instead.")
	}

	claimed, err := cfg.ScopeDrifts.ClaimOption(ctx, drift.ID, ScopeDriftOptionJustify, "", justification)
	if errors.Is(err, ErrDriftAlreadyResolved) {
		return scopeDriftStatus("Clawvisor: drift " + drift.ID + " was already resolved with option " + sanitizeStatusValue(string(claimed.ChosenOption)) + ". The one-shot cap forbids re-claiming.")
	}
	if err != nil {
		return scopeDriftStatus("Clawvisor: could not claim drift " + drift.ID + ": " + sanitizeStatusValue(err.Error()))
	}

	// Re-verify with the ORIGINAL tool_use's parameters. Without
	// these, the second pass sees only service/action + the agent's
	// justification, so a verifier that initially rejected based on
	// the actual request shape (target repo, path, body, etc.) would
	// be re-asked a strictly weaker question and could pre-clear an
	// exact call whose arguments were never rechecked. Parse-failure
	// is non-fatal: we still want the verifier to weigh the
	// justification (rejecting if the link is hollow), just with the
	// params field empty — same as the initial pass when the tool's
	// input parser couldn't extract structured params.
	var params map[string]any
	if len(claimed.ToolUse.Input) > 0 {
		_ = json.Unmarshal(claimed.ToolUse.Input, &params)
	}
	verdict, verifyErr := cfg.IntentVerifier.Verify(ctx, IntentVerifyRequest{
		TaskPurpose:        claimed.TaskPurpose,
		ExpectedUse:        claimed.ExpectedUse,
		Service:            claimed.Service,
		Action:             claimed.Action,
		Params:             params,
		Reason:             "lite-proxy tool_use " + claimed.ToolUse.Name + " - second-pass verification via scope-drift justify",
		TaskID:             claimed.TaskID,
		AgentJustification: justification,
	})
	if verifyErr != nil {
		// Verifier outage on the second pass: close the drift so the
		// one-shot cap stays honest, and tell the agent to retry the
		// original tool call (which mints a fresh drift_id they can
		// resolve with (a) or (b)).
		_ = cfg.ScopeDrifts.SetOutcome(ctx, drift.ID, ScopeDriftOutcomeDenied)
		return scopeDriftStatus("Clawvisor: intent verifier was unreachable on the second pass: " + sanitizeStatusValue(verifyErr.Error()) + ". This drift is now closed. Re-emit the original tool call to start over with a fresh drift_id, then pick option (a) expand or (b) new task.")
	}
	if verdict != nil && verdict.Allow {
		if err := cfg.ScopeDrifts.SetOutcome(ctx, drift.ID, ScopeDriftOutcomeSucceeded); err != nil {
			return scopeDriftStatus("Clawvisor: verifier accepted but pre-clear write failed: " + sanitizeStatusValue(err.Error()) + ". Re-emit the original tool call to start over with a fresh drift_id, then pick option (a) or (b).")
		}
		explanation := ""
		if verdict.Explanation != "" {
			explanation = " (" + sanitizeStatusValue(verdict.Explanation) + ")"
		}
		return scopeDriftStatus("Clawvisor: verifier accepted your justification" + explanation + ". Re-emit the original tool call unchanged.")
	}

	// Verifier rejected. The drift is now resolved (negatively).
	// Honest contract: this drift is closed, the agent retries the
	// original tool_use to mint a fresh drift_id and can pick (a)/(b)
	// on the next round.
	_ = cfg.ScopeDrifts.SetOutcome(ctx, drift.ID, ScopeDriftOutcomeDenied)
	explanation := ""
	if verdict != nil && verdict.Explanation != "" {
		explanation = " (" + sanitizeStatusValue(verdict.Explanation) + ")"
	}
	return scopeDriftStatus("Clawvisor: verifier did not accept your justification" + explanation + ". This drift is now closed. Re-emit the original tool call to start over with a fresh drift_id, then pick option (a) expand or (b) new task.")
}

// resolveOneOff handles option (c). The agent's <clawvisor:decision
// option="one-off"> markup is replaced with an inline approval prompt
// the user sees in place of the agent's text. Under the hood:
//
//  1. ClaimOption sets ChosenOption=one_off (one-shot cap enforced).
//  2. We open a PendingLiteApproval at StageAwaitingScopeDriftOneOff
//     carrying the drift_id and the agent's note, so the user's
//     "yes"/"no" reply gets routed to RewriteScopeDriftOneOffApprovalReply.
//  3. The substitution returned here is the user-facing approval prompt
//     — when applyScopeDriftDecisions splices it in, the user sees the
//     proxy's question instead of the raw markup.
//
// Failure paths degrade to a status string so the body substitution
// always produces valid content; the pre-clear is only inserted when
// the user actually approves the one-off (via the reply rewriter).
func resolveOneOff(ctx context.Context, cfg PostprocessConfig, provider conversation.Provider, drift ScopeDrift, agentNote string) string {
	if cfg.PendingApprovals == nil {
		return scopeDriftStatus("Clawvisor: pending-approval cache is not configured on this daemon. Option (c) cannot complete — pick option (a) expand, (b) new task, or (d) justify instead.")
	}
	claimed, err := cfg.ScopeDrifts.ClaimOption(ctx, drift.ID, ScopeDriftOptionOneOff, agentNote, "")
	if errors.Is(err, ErrDriftAlreadyResolved) {
		return scopeDriftStatus("Clawvisor: drift " + drift.ID + " was already resolved with option " + sanitizeStatusValue(string(claimed.ChosenOption)) + ". The one-shot cap forbids re-claiming.")
	}
	if err != nil {
		return scopeDriftStatus("Clawvisor: could not claim drift " + drift.ID + ": " + sanitizeStatusValue(err.Error()))
	}

	hold, holdErr := cfg.PendingApprovals.Hold(ctx, PendingLiteApproval{
		UserID:              cfg.AgentUserID,
		AgentID:             cfg.AgentID,
		Provider:            provider,
		ConversationID:      cfg.ConversationID,
		Stage:               StageAwaitingScopeDriftOneOff,
		ToolUse:             claimed.ToolUse,
		Reason:              "scope-drift one-off: " + claimed.Service + "." + claimed.Action,
		ScopeDriftID:        claimed.ID,
		ScopeDriftAgentNote: agentNote,
	})
	if holdErr != nil {
		// Cache insert failed. Mark the drift denied so the one-shot
		// cap stays consistent (we already claimed it) and tell the
		// agent to retry.
		_ = cfg.ScopeDrifts.SetOutcome(ctx, claimed.ID, ScopeDriftOutcomeDenied)
		return scopeDriftStatus("Clawvisor: could not queue the one-off approval (" + sanitizeStatusValue(holdErr.Error()) + "). Re-emit the original tool call to start over.")
	}
	return renderScopeDriftOneOffPrompt(claimed, hold.Pending.ID)
}

// scopeDriftStatus wraps a status message in a recognisable bracketed
// prefix so the model can pick it out of its conversation history when
// it re-enters this drift's context. The prefix is the same one used
// for other inline proxy notices so the model is already trained to
// treat it as authoritative system text.
func scopeDriftStatus(msg string) string {
	return "[Clawvisor scope-drift] " + msg
}

// sanitizeStatusValue strips characters that would break the JSON
// string the status will be embedded into. We drop quotes,
// backslashes, newlines, and control characters. This is more
// aggressive than necessary for valid JSON but keeps the substitution
// rule simple: the status text is always ASCII-safe.
func sanitizeStatusValue(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '"', '\\':
			continue
		case '\n', '\r', '\t':
			b.WriteByte(' ')
		default:
			if r < 0x20 || r == 0x7F {
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// spliceBytes returns a new slice with body[start:end] replaced by
// replacement. The original slice is not modified.
func spliceBytes(body []byte, start, end int, replacement []byte) []byte {
	if start < 0 || end > len(body) || start > end {
		return body
	}
	// Let append size the backing array. A manual capacity hint here
	// (len(body)-(end-start)+len(replacement)) was flagged by CodeQL
	// as a potential overflow if replacement is pathologically large;
	// our SSE bodies are small enough that the extra grow cost is
	// negligible, and skipping the arithmetic sidesteps the warning.
	var out []byte
	out = append(out, body[:start]...)
	out = append(out, replacement...)
	out = append(out, body[end:]...)
	return out
}
