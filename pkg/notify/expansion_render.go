package notify

import (
	"strings"
)

// scopeExpansionMaxCompactLen caps the joined "+x, ~y" summary so a
// large expansion can't push the push-notification payload over the
// APNs/FCM ~4KB limit (the daemon serializes the whole pushPayload
// JSON — title, body, data map, live-activity attributes — and each
// edge has its own ceiling). 256 bytes leaves headroom for body +
// purpose + target_id + daemon_url within ~1KB total, which both
// platforms accept without truncation.
const scopeExpansionMaxCompactLen = 256

// scopeExpansionMaxBodyLen caps the human-readable body string that
// goes into AlertBody / notification body. Long agent reasons get
// elided with an ellipsis so a runaway model can't smuggle multi-KB
// text into a single push.
const scopeExpansionMaxBodyLen = 512

// RenderExpansionSummary builds a short, identifier-only summary of an
// expansion envelope: new entries with "+" markers and replaced
// entries with "~" markers, joined by ", ". Used by surfaces that
// can't fit the full diff (push notification action_summary,
// live-activity status). Identifiers only — no `why` — since the
// notification body already carries the reason.
//
// The result is capped at ~scopeExpansionMaxCompactLen bytes;
// remaining entries are summarized as "(+N more)". Empty envelopes
// return the sentinel "scope_expansion" so consumers don't render
// an empty action_summary.
//
// Shared with the Telegram renderer (which uses the structured
// diff for prose) so both surfaces agree on the +/~ vocabulary.
func RenderExpansionSummary(req ScopeExpansionRequest) string {
	var parts []string
	for _, t := range req.AddedTools {
		parts = append(parts, "+"+t.ToolName)
	}
	for _, t := range req.ReplacedTools {
		parts = append(parts, "~"+t.New.ToolName)
	}
	for _, e := range req.AddedEgress {
		parts = append(parts, "+"+e.Host)
	}
	for _, e := range req.ReplacedEgress {
		parts = append(parts, "~"+e.New.Host)
	}
	for _, c := range req.AddedCredentials {
		parts = append(parts, "+"+credentialID(c))
	}
	for _, c := range req.ReplacedCredentials {
		parts = append(parts, "~"+credentialID(c.New))
	}
	if len(parts) == 0 {
		return "scope_expansion"
	}
	return joinWithCap(parts, ", ", scopeExpansionMaxCompactLen)
}

// CapExpansionBody bounds the body / AlertBody string a notifier
// sends. The agent's reason is the dominant variable input here; an
// uncapped body lets a runaway model push the surrounding push
// payload over the APNs/FCM size limit. Mirrors the paramValue
// truncation pattern from internal/notify/telegram.
func CapExpansionBody(body string) string {
	if len(body) <= scopeExpansionMaxBodyLen {
		return body
	}
	return body[:scopeExpansionMaxBodyLen-3] + "..."
}

// credentialID picks the populated identifier off an ExpansionCredential.
// Both fields can't be set after validation, but the helper stays
// total so renderers don't need to defend against a malformed entry.
func credentialID(c ExpansionCredential) string {
	if c.VaultItemID != "" {
		return c.VaultItemID
	}
	return c.VaultItemHandle
}

// joinWithCap joins parts with sep and truncates with a "(+N more)"
// suffix once adding the next part would exceed maxBytes. A single
// oversized first item is itself ellipsized so a runaway model
// can't smuggle a long identifier past the cap by making it the
// only entry. The "(+N more)" suffix length is reserved against
// maxBytes proactively so the final output stays at or below the
// cap — the previous version appended the suffix unconditionally
// and could overshoot by ~14 bytes.
//
// Returns "" if parts is empty.
func joinWithCap(parts []string, sep string, maxBytes int) string {
	if len(parts) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, p := range parts {
		next := p
		if i > 0 {
			next = sep + p
		}
		remaining := len(parts) - i
		// Compute the worst-case suffix length for THIS index ("+ N more)")
		// so the bail threshold reserves room for it; the suffix is
		// appended only when i > 0, but reserving for it earlier is
		// harmless (the only effect is we may bail one entry sooner).
		suffix := " (+" + itoa(remaining) + " more)"
		// Bail when adding `next` would push the running total OR the
		// running total + suffix over maxBytes. Without the second
		// check, the suffix could land past the cap.
		if sb.Len()+len(next) > maxBytes || (i > 0 && sb.Len()+len(next)+len(suffix) > maxBytes) {
			if i == 0 {
				// Single oversized entry: truncate it in place
				// rather than emitting an empty string. Reserve
				// 3 bytes for the ellipsis.
				if maxBytes > 3 && len(p) > maxBytes-3 {
					sb.WriteString(p[:maxBytes-3])
					sb.WriteString("...")
					return sb.String()
				}
				sb.WriteString(p)
				return sb.String()
			}
			sb.WriteString(suffix)
			return sb.String()
		}
		sb.WriteString(next)
	}
	return sb.String()
}

// itoa is a tiny stdlib-free int→ascii helper so this file doesn't
// pull strconv just for a "+N more" suffix.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
