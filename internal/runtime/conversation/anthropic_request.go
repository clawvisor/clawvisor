package conversation

import "encoding/json"

func AnthropicToolResultIDsFromRequest(body []byte) []string {
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	var ids []string
	for _, message := range req.Messages {
		messageIDs, ok := anthropicToolResultIDs(message.Content)
		if !ok {
			continue
		}
		ids = append(ids, messageIDs...)
	}
	return ids
}
