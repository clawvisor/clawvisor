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
// JSON-encoded body.
//
// The pattern is non-greedy on the body so consecutive markup blocks in
// one response are treated as separate decisions.
var scopeDriftDecisionPattern = regexp.MustCompile(
	`(?s)<clawvisor:decision\b([^>]*)>(.*?)</clawvisor:decision>`,
)

// scopeDriftAttrPattern matches `name="value"` and `name=\"value\"` (JSON-
// escaped) attributes inside the opening tag.
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
	Start   int
	End     int
}

// parseScopeDriftDecisions returns every well-formed markup block in
// body. Malformed blocks (missing drift= or option=, blank body) are
// dropped so a partial emit doesn't accidentally claim an option.
// Markup that appears inside a markdown code fence (``` or `) is
// skipped — when the model echoes or quotes the menu inside an
// explanation, the markup is decorative, not an action.
func parseScopeDriftDecisions(body []byte) []scopeDriftDecision {
	matches := scopeDriftDecisionPattern.FindAllSubmatchIndex(body, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]scopeDriftDecision, 0, len(matches))
	for _, m := range matches {
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
// markdown code construct that should suppress markup parsing.
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
// line-leading ``` fences. Each line-leading fence toggles the "inside
// a block" state; fences in the middle of a line are ignored.
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
// line as the offset.
func insideInlineCodeSpan(body []byte, offset int) bool {
	lineStart := offset
	for lineStart > 0 && body[lineStart-1] != '\n' {
		lineStart--
	}
	count := 0
	i := lineStart
	for i < offset {
		if body[i] == '`' {
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

// ScopeDriftResolveContext is the narrow dependency surface
// applyScopeDriftDecisions needs. Built by the postproc orchestrator
// from its sub-contexts (AgentContext, AuditContext.ConversationID,
// AuthorizationContext.ScopeDrifts, ApprovalContext.PendingApprovals)
// so this layer doesn't depend on the full PostprocessConfig.
type ScopeDriftResolveContext struct {
	AgentContext
	ConversationID   string
	Registry         ScopeDriftRegistry
	PendingApprovals PendingApprovalCache
}

// ApplyScopeDriftDecisions scans body for <clawvisor:decision> markup
// blocks and, for each one tied to a known pending drift for this
// agent + conversation, claims the option and acts on it server-side.
// The matched markup ranges are substituted with a JSON-safe status
// string so the model's next-turn context contains the proxy's
// decision, not the raw markup. Returns the (possibly mutated) body
// bytes and whether anything changed.
//
// Errors degrade to a status message in the body rather than failing
// the whole turn. Pre-clears for the original tool call are only
// inserted on a successful user-approved one-off (handled later by the
// reply rewriter).
//
// Exported for use by the postproc package, which builds the narrow
// ScopeDriftResolveContext from its sub-contexts.
func ApplyScopeDriftDecisions(ctx context.Context, rc ScopeDriftResolveContext, provider conversation.Provider, body []byte) ([]byte, bool) {
	if rc.Registry == nil || len(body) == 0 {
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
		status := resolveScopeDriftDecision(ctx, rc, provider, d)
		body = spliceBytes(body, d.Start, d.End, jsonEscapeForStringBody(status))
	}
	return body, true
}

// jsonEscapeForStringBody returns the JSON-string-encoded form of s
// WITHOUT the surrounding double quotes — suitable for splicing inside
// an existing JSON string field.
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
// path returns a string.
func resolveScopeDriftDecision(ctx context.Context, rc ScopeDriftResolveContext, provider conversation.Provider, d scopeDriftDecision) string {
	drift, err := rc.Registry.Get(ctx, d.DriftID)
	if err != nil {
		return scopeDriftStatus("Clawvisor: drift " + d.DriftID + " not found (it may have expired). Re-emit the original tool call to get a fresh menu.")
	}
	if drift.AgentID != rc.AgentID {
		return scopeDriftStatus("Clawvisor: drift " + d.DriftID + " was minted for a different agent and cannot be resolved here.")
	}
	if drift.ConversationID != rc.ConversationID {
		return scopeDriftStatus("Clawvisor: drift " + d.DriftID + " belongs to a different conversation and cannot be resolved here.")
	}

	switch d.Option {
	case "one-off", "one_off", "oneoff":
		return resolveOneOff(ctx, rc, provider, drift, d.Body)
	default:
		return scopeDriftStatus("Clawvisor: unknown decision option \"" + sanitizeStatusValue(d.Option) + "\". Valid value: one-off. Use POST /control/tasks{,/<id>/expand} for the other options.")
	}
}

// resolveOneOff handles option (c). The agent's <clawvisor:decision
// option="one-off"> markup is replaced with an inline approval prompt
// the user sees in place of the agent's text. Under the hood:
//
//  1. ClaimOption sets ChosenOption=one_off (one-shot cap enforced).
//  2. Open a PendingLiteApproval at StageAwaitingScopeDriftOneOff
//     carrying the drift_id and the agent's note so the user's
//     yes/no reply routes to RewriteScopeDriftOneOffApprovalReply.
//  3. Return the user-facing approval prompt — when
//     applyScopeDriftDecisions splices it in, the user sees the
//     proxy's question instead of the raw markup.
//
// Failure paths degrade to a status string so the body substitution
// always produces valid content; the pre-clear is only inserted when
// the user actually approves the one-off (via the reply rewriter).
func resolveOneOff(ctx context.Context, rc ScopeDriftResolveContext, provider conversation.Provider, drift ScopeDrift, agentNote string) string {
	if rc.PendingApprovals == nil {
		return scopeDriftStatus("Clawvisor: pending-approval cache is not configured on this daemon. Option (c) cannot complete — pick option (a) expand or (b) new task instead.")
	}
	claimed, err := rc.Registry.ClaimOption(ctx, drift.ID, ScopeDriftOptionOneOff, agentNote)
	if errors.Is(err, ErrDriftAlreadyResolved) {
		return scopeDriftStatus("Clawvisor: drift " + drift.ID + " was already resolved with option " + sanitizeStatusValue(string(claimed.ChosenOption)) + ". The one-shot cap forbids re-claiming.")
	}
	if err != nil {
		return scopeDriftStatus("Clawvisor: could not claim drift " + drift.ID + ": " + sanitizeStatusValue(err.Error()))
	}

	hold, holdErr := rc.PendingApprovals.Hold(ctx, PendingLiteApproval{
		UserID:              rc.AgentUserID,
		AgentID:             rc.AgentID,
		Provider:            provider,
		ConversationID:      rc.ConversationID,
		Stage:               StageAwaitingScopeDriftOneOff,
		ToolUse:             claimed.ToolUse,
		Reason:              "scope-drift one-off: " + claimed.Service + "." + claimed.Action,
		ScopeDriftID:        claimed.ID,
		ScopeDriftAgentNote: agentNote,
	})
	if holdErr != nil {
		_ = rc.Registry.SetOutcome(ctx, claimed.ID, ScopeDriftOutcomeDenied)
		return scopeDriftStatus("Clawvisor: could not queue the one-off approval (" + sanitizeStatusValue(holdErr.Error()) + "). Re-emit the original tool call to start over.")
	}
	return renderScopeDriftOneOffPrompt(claimed, hold.Pending.ID)
}

// scopeDriftStatus wraps a status message in a recognisable bracketed
// prefix so the model can pick it out of its conversation history.
func scopeDriftStatus(msg string) string {
	return "[Clawvisor scope-drift] " + msg
}

// sanitizeStatusValue strips characters that would break the JSON
// string the status will be embedded into.
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
	var out []byte
	out = append(out, body[:start]...)
	out = append(out, replacement...)
	out = append(out, body[end:]...)
	return out
}
