package llmproxy

import (
	"encoding/json"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// ExpiredTaskNoticeSentinel is the substring callers can search for in
// an inbound body to decide whether the proxy has already announced a
// specific task's expiry to the model. Idempotency is keyed per
// taskID — once a notice carrying `ExpiredTaskNoticeSentinel + taskID`
// is in conversation history, subsequent passes skip injection for
// that task. A later-checked-out task that also expires gets its own
// fresh notice.
const ExpiredTaskNoticeSentinel = "expired_task_id="

// RenderExpiredTaskNotice builds the user-role <clawvisor-notice
// kind="task-expired"> the proxy prepends to the first genuine user
// turn after the agent's checked-out task lapses. The body embeds the
// sentinel (`expired_task_id={taskID}`) and a sanitized purpose so the
// model can identify the lapsed task without a control-plane lookup,
// then routes it to the standard recovery moves (POST a new task or
// POST /control/task/checkout).
//
// taskID is rendered verbatim (it's an internal UUID, not model- or
// agent-supplied free text). purpose is sanitized — agents can write
// arbitrary text into Task.Purpose, so it's untrusted.
func RenderExpiredTaskNotice(taskID, purpose string) string {
	taskID = strings.TrimSpace(taskID)
	purpose = sanitizeExpiredTaskPurpose(purpose)
	var b strings.Builder
	b.WriteString("The previously active task has expired (")
	b.WriteString(ExpiredTaskNoticeSentinel)
	b.WriteString(taskID)
	if purpose != "" {
		b.WriteString(", purpose=\"")
		b.WriteString(purpose)
		b.WriteString("\"")
	}
	b.WriteString("). The next tool_use that needs task scope will be denied. To recover: (A) POST https://clawvisor.local/control/tasks?surface=inline to create a fresh task for the body of work you're continuing, or (B) POST https://clawvisor.local/control/task/checkout with the task_id of a different active task you already own. Acknowledge the expiry briefly to the user; do not retry the prior tool_use until task scope is re-established.")
	return Render(NoticeKindTaskExpired, b.String())
}

// sanitizeExpiredTaskPurpose collapses whitespace, strips control
// characters and the structural delimiters our notice format uses
// (double quotes, backticks, the middle-dot the snapshot uses as its
// bullet field separator), then length-caps the result. Render
// XML-escapes the body, so this sanitizer is purely about layout /
// size — the goal is keeping the surrounding notice readable when a
// purpose is long or contains hostile characters, not preventing
// envelope escape (Render handles that).
func sanitizeExpiredTaskPurpose(raw string) string {
	const maxLen = 120
	var b strings.Builder
	b.Grow(len(raw))
	lastSpace := true
	for _, r := range raw {
		switch {
		case r == ' ':
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
		case r == '\n', r == '\r', r == '\t', r == '\v', r == '\f':
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
		case r < 0x20, r == 0x7f:
			continue
		case r == '`', r == '"', r == '·':
			continue
		default:
			b.WriteRune(r)
			lastSpace = false
		}
	}
	out := strings.TrimSpace(b.String())
	if len(out) > maxLen {
		runes := []rune(out)
		if len(runes) > maxLen-1 {
			out = string(runes[:maxLen-1]) + "…"
		}
	}
	return out
}

// PrependExpiredTaskNoticeToLastUserMessage walks the inbound request
// body, finds the LAST genuine user-role message, and prepends a new
// text content block carrying the rendered task-expired notice. The
// "genuine" filter mirrors ExtractRecentHumanTurns: bare approval verbs
// (the user typing "yes" / "no" to drive an approval flow) and prior
// <clawvisor-notice> turns are skipped, so we keep walking backward
// past those. A user-role message whose flattened text is empty (e.g.
// a tool_result-only turn) is also skipped — the notice would land on
// top of harness output where the model is unlikely to act on it
// before continuing the tool loop.
//
// Idempotency: callers MUST check for the sentinel substring
// (ExpiredTaskNoticeSentinel + taskID) before invoking; this function
// does NOT re-check, so it would happily inject twice if called twice
// in a row. The sentinel check lives in the policy so the rendering
// helper here stays pure.
//
// Provider gating: Anthropic only for now. OpenAI returns (body, false,
// nil) — same as the existing inline-task augmentation, which is also
// Anthropic-first.
//
// Returns (body, true, nil) when a target was found and modified;
// (body, false, nil) when no qualifying user-role message exists (e.g.
// the inbound body's tail is all tool_result turns or all internal
// notices); (nil, false, err) on JSON malformation. Errors are
// expected to be non-fatal at the call site — a malformed body is
// already a downstream concern, not something the notice injector
// should turn into a deny.
func PrependExpiredTaskNoticeToLastUserMessage(body []byte, provider conversation.Provider, taskID, purpose string) ([]byte, bool, error) {
	if provider != conversation.ProviderAnthropic {
		return body, false, nil
	}
	notice := RenderExpiredTaskNotice(taskID, purpose)
	return prependAnthropicLastUserNotice(body, notice)
}

func prependAnthropicLastUserNotice(body []byte, notice string) ([]byte, bool, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, false, err
	}
	msgsRaw, ok := raw["messages"]
	if !ok {
		return body, false, nil
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(msgsRaw, &messages); err != nil {
		return nil, false, err
	}
	for i := len(messages) - 1; i >= 0; i-- {
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(messages[i], &msg); err != nil {
			continue
		}
		var role string
		_ = json.Unmarshal(msg["role"], &role)
		if role != "user" {
			continue
		}
		content := msg["content"]
		text := strings.TrimSpace(extractAnthropicUserText(content))
		if text == "" || isClawvisorInternalUserText(text) {
			continue
		}
		// Found the target: prepend the notice as a text content block.
		newContent, err := prependTextBlockToAnthropicUserContent(content, notice)
		if err != nil {
			return nil, false, err
		}
		msg["content"] = newContent
		encodedMsg, err := json.Marshal(msg)
		if err != nil {
			return nil, false, err
		}
		messages[i] = encodedMsg
		newMessages, err := json.Marshal(messages)
		if err != nil {
			return nil, false, err
		}
		raw["messages"] = newMessages
		out, err := json.Marshal(raw)
		if err != nil {
			return nil, false, err
		}
		return out, true, nil
	}
	return body, false, nil
}

// prependTextBlockToAnthropicUserContent normalizes the user-role
// `content` field (which Anthropic accepts as either a string or a
// block array) and prepends a fresh {"type":"text","text":notice}
// block. A string content turns into a 2-element array:
//   [notice_block, {"type":"text","text":<original_string>}]
//
// An array content gets the notice block prepended ahead of the
// existing blocks; relative order of the existing blocks is preserved
// so downstream consumers (Anthropic, historystrip, etc.) see the same
// shape they already do, just with one extra leading text block.
func prependTextBlockToAnthropicUserContent(content json.RawMessage, notice string) (json.RawMessage, error) {
	noticeBlock, err := json.Marshal(map[string]string{"type": "text", "text": notice})
	if err != nil {
		return nil, err
	}
	if len(content) == 0 || string(content) == "null" {
		blocks := []json.RawMessage{noticeBlock}
		return json.Marshal(blocks)
	}
	// String form? Wrap original into a text block, then prepend notice.
	var asString string
	if err := json.Unmarshal(content, &asString); err == nil {
		origBlock, err := json.Marshal(map[string]string{"type": "text", "text": asString})
		if err != nil {
			return nil, err
		}
		blocks := []json.RawMessage{noticeBlock, origBlock}
		return json.Marshal(blocks)
	}
	// Block-array form: prepend.
	var blocks []json.RawMessage
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(blocks)+1)
	out = append(out, noticeBlock)
	out = append(out, blocks...)
	return json.Marshal(out)
}
