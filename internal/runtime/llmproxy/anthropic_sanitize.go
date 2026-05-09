package llmproxy

import (
	"encoding/json"
	"strings"
)

// SanitizeAnthropicRequest removes empty text content blocks that Anthropic
// rejects on the request path. Some harnesses preserve zero-length streamed
// text blocks in conversation history after a tool-use response.
func SanitizeAnthropicRequest(body []byte) ([]byte, bool, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, false, err
	}
	changed := false

	if sys, ok := raw["system"]; ok {
		sanitized, fieldChanged, empty, err := sanitizeAnthropicContent(sys)
		if err != nil {
			return nil, false, err
		}
		if fieldChanged {
			changed = true
			if empty {
				delete(raw, "system")
			} else {
				raw["system"] = sanitized
			}
		}
	}

	if msgsRaw, ok := raw["messages"]; ok {
		var messages []map[string]json.RawMessage
		if err := json.Unmarshal(msgsRaw, &messages); err != nil {
			return nil, false, err
		}
		out := make([]map[string]json.RawMessage, 0, len(messages))
		for _, msg := range messages {
			content, ok := msg["content"]
			if !ok {
				out = append(out, msg)
				continue
			}
			sanitized, fieldChanged, empty, err := sanitizeAnthropicContent(content)
			if err != nil {
				return nil, false, err
			}
			if fieldChanged {
				changed = true
			}
			if empty {
				continue
			}
			if fieldChanged {
				msg["content"] = sanitized
			}
			out = append(out, msg)
		}
		if len(out) != len(messages) {
			changed = true
		}
		if changed {
			encoded, err := json.Marshal(out)
			if err != nil {
				return nil, false, err
			}
			raw["messages"] = encoded
		}
	}

	if !changed {
		return body, false, nil
	}
	out, err := json.Marshal(raw)
	return out, err == nil, err
}

func sanitizeAnthropicContent(raw json.RawMessage) (json.RawMessage, bool, bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return raw, false, false, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if strings.TrimSpace(text) == "" {
			return nil, true, true, nil
		}
		return raw, false, false, nil
	}

	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return raw, false, false, nil
	}
	changed := false
	out := make([]map[string]json.RawMessage, 0, len(blocks))
	for _, block := range blocks {
		var typ string
		_ = json.Unmarshal(block["type"], &typ)
		if typ == "text" {
			var blockText string
			_ = json.Unmarshal(block["text"], &blockText)
			if strings.TrimSpace(blockText) == "" {
				changed = true
				continue
			}
		}
		if nested, ok := block["content"]; ok {
			sanitized, nestedChanged, empty, err := sanitizeAnthropicContent(nested)
			if err != nil {
				return nil, false, false, err
			}
			if nestedChanged {
				changed = true
				if empty {
					delete(block, "content")
				} else {
					block["content"] = sanitized
				}
			}
		}
		out = append(out, block)
	}
	if !changed {
		return raw, false, false, nil
	}
	if len(out) == 0 {
		return nil, true, true, nil
	}
	encoded, err := json.Marshal(out)
	return encoded, true, false, err
}
