package bodytransform

import (
	"encoding/json"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/jsonsurgery"
)

// SanitizeAnthropicRequest removes empty text content blocks that Anthropic
// rejects on the request path. Some harnesses preserve zero-length streamed
// text blocks in conversation history after a tool-use response.
//
// Byte fidelity invariant: any content block we are not actively removing
// passes through with its original bytes. Top-level body keys, message
// keys, and block keys keep their incoming order. This matters because
// Anthropic verifies thinking-block signatures across turns; an
// `unmarshal → map → marshal` round-trip alphabetizes keys and trips
// "thinking blocks cannot be modified" 400s on subsequent requests.
func SanitizeAnthropicRequest(body []byte) ([]byte, bool, error) {
	// Eagerly validate the body is parseable JSON. The previous
	// implementation did the same via `json.Unmarshal(body, &raw)` at
	// the top, and callers depend on this to surface a 400 for
	// malformed bodies before they reach the upstream provider.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, false, err
	}
	out := body
	changed := false

	// system field.
	if sysStart, sysEnd, ok := jsonsurgery.FindFieldValue(out, "system"); ok {
		sys := out[sysStart:sysEnd]
		sanitized, fieldChanged, empty, err := sanitizeAnthropicContent(sys)
		if err != nil {
			return nil, false, err
		}
		if fieldChanged {
			changed = true
			if empty {
				if deleted, ok := jsonsurgery.DeleteField(out, "system"); ok {
					out = deleted
				}
			} else {
				replaced, err := jsonsurgery.SetField(out, "system", sanitized)
				if err != nil {
					return nil, false, err
				}
				out = replaced
			}
		}
	}

	// messages field.
	if msgsStart, msgsEnd, ok := jsonsurgery.FindFieldValue(out, "messages"); ok {
		msgsBytes := out[msgsStart:msgsEnd]
		messages, ok := jsonsurgery.FlattenArray(msgsBytes)
		if !ok {
			// `messages` present but not an array — malformed input.
			// Surface the parse error so the caller can return a 400.
			if err := json.Unmarshal(msgsBytes, &messages); err != nil {
				return nil, false, err
			}
			if !changed {
				return body, false, nil
			}
			return out, true, nil
		}
		newMessages := make([]json.RawMessage, 0, len(messages))
		msgsChanged := false
		for _, msg := range messages {
			newMsg, msgChanged, drop, err := sanitizeAnthropicMessage(msg)
			if err != nil {
				return nil, false, err
			}
			if msgChanged {
				msgsChanged = true
			}
			if drop {
				continue
			}
			newMessages = append(newMessages, newMsg)
		}
		if msgsChanged || len(newMessages) != len(messages) {
			changed = true
			newMsgsBytes, err := json.Marshal(newMessages)
			if err != nil {
				return nil, false, err
			}
			replaced, err := jsonsurgery.SetField(out, "messages", newMsgsBytes)
			if err != nil {
				return nil, false, err
			}
			out = replaced
		}
	}

	if !changed {
		return body, false, nil
	}
	return out, true, nil
}

// sanitizeAnthropicMessage applies sanitization to a single message's
// content. Returns (newBytes, contentChanged, dropMessage, err).
// When dropMessage is true, the caller should omit the message from
// the parent array.
func sanitizeAnthropicMessage(msg json.RawMessage) (json.RawMessage, bool, bool, error) {
	contentStart, contentEnd, ok := jsonsurgery.FindFieldValue(msg, "content")
	if !ok {
		return msg, false, false, nil
	}
	content := msg[contentStart:contentEnd]
	sanitized, contentChanged, empty, err := sanitizeAnthropicContent(content)
	if err != nil {
		return nil, false, false, err
	}
	if !contentChanged {
		return msg, false, false, nil
	}
	if empty {
		return nil, true, true, nil
	}
	newMsg, err := jsonsurgery.SetField(msg, "content", sanitized)
	if err != nil {
		return nil, false, false, err
	}
	return newMsg, true, false, nil
}

// sanitizeAnthropicContent returns the sanitized content payload. The
// shape mirrors the original API: (newBytes, changed, empty, err).
// When empty is true the caller should remove the surrounding field
// (or drop the parent message).
func sanitizeAnthropicContent(raw json.RawMessage) (json.RawMessage, bool, bool, error) {
	if len(raw) == 0 {
		return raw, false, false, nil
	}
	trimmed := jsonsurgery.TrimWS(raw)
	if string(trimmed) == "null" {
		return raw, false, false, nil
	}
	// String content: empty/whitespace → drop. Otherwise preserve.
	if jsonsurgery.LooksLikeString(raw) {
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			if strings.TrimSpace(text) == "" {
				return nil, true, true, nil
			}
		}
		return raw, false, false, nil
	}
	// Array content: walk blocks, preserve unchanged ones byte-for-byte.
	blocks, ok := jsonsurgery.FlattenArray(raw)
	if !ok {
		return raw, false, false, nil
	}
	newBlocks := make([]json.RawMessage, 0, len(blocks))
	changed := false
	for _, block := range blocks {
		// Inspect type to decide whether to keep/modify.
		blockType := extractBlockType(block)
		if blockType == "text" {
			if isEmptyTextBlock(block) {
				changed = true
				continue
			}
		}
		// Recurse into nested content (e.g. tool_result blocks whose
		// `content` field is itself a list of content blocks).
		if nestedStart, nestedEnd, ok := jsonsurgery.FindFieldValue(block, "content"); ok {
			nested := block[nestedStart:nestedEnd]
			sanitized, nestedChanged, empty, err := sanitizeAnthropicContent(nested)
			if err != nil {
				return nil, false, false, err
			}
			if nestedChanged {
				changed = true
				if empty {
					deleted, ok := jsonsurgery.DeleteField(block, "content")
					if ok {
						block = deleted
					}
				} else {
					replaced, err := jsonsurgery.SetField(block, "content", sanitized)
					if err != nil {
						return nil, false, false, err
					}
					block = replaced
				}
			}
		}
		newBlocks = append(newBlocks, block)
	}
	if !changed {
		return raw, false, false, nil
	}
	if len(newBlocks) == 0 {
		return nil, true, true, nil
	}
	encoded, err := json.Marshal(newBlocks)
	return encoded, true, false, err
}

func extractBlockType(block json.RawMessage) string {
	start, end, ok := jsonsurgery.FindFieldValue(block, "type")
	if !ok {
		return ""
	}
	var typ string
	if err := json.Unmarshal(block[start:end], &typ); err != nil {
		return ""
	}
	return typ
}

func isEmptyTextBlock(block json.RawMessage) bool {
	start, end, ok := jsonsurgery.FindFieldValue(block, "text")
	if !ok {
		return false
	}
	var text string
	if err := json.Unmarshal(block[start:end], &text); err != nil {
		// Malformed `text` (number, object) — leave alone; dropping
		// silently risks losing real content the model emitted.
		return false
	}
	return strings.TrimSpace(text) == ""
}
