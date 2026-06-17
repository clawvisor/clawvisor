package llmproxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// ScopeDriftInboundRewriteRequest is the input to
// RewriteScopeDriftPlaceholders. The caller supplies the inbound
// /v1/messages body and identifying context; the rewriter consults the
// ScopeDriftRegistry for pending substitutions and returns the rewritten
// body when one or more applied.
type ScopeDriftInboundRewriteRequest struct {
	HTTPRequest    *http.Request
	Provider       conversation.Provider
	Body           []byte
	Agent          *store.Agent
	ConversationID string
	ScopeDrifts    ScopeDriftRegistry
	Logger         *slog.Logger
}

// ScopeDriftInboundRewriteResult reports what the rewriter did.
type ScopeDriftInboundRewriteResult struct {
	Body            []byte
	Rewritten       bool
	AppliedDriftIDs []string
}

// RewriteScopeDriftPlaceholders walks the inbound /v1/messages body
// looking for tool_result blocks whose tool_use_id matches a pending
// scope-drift substitution. For each match:
//
//  1. The matching tool_result's content is replaced with the drift's
//     menu text.
//  2. The corresponding tool_use block in the prior assistant turn —
//     a harness-side Bash placeholder we substituted on the response
//     leg — is restored to the model's original tool_use (name +
//     input bytes) byte-for-byte.
//
// The substitution is one-shot: LookupPendingSubstitution consumes the
// registry entry. If the rewrite fails (malformed body, missing
// assistant turn) the entry is NOT re-registered — the agent's next
// retry mints a fresh drift on the normal path.
//
// Returns (body, Rewritten=false, nil) when no substitutions apply.
// Errors only on body shape failures the caller should treat as fail-
// closed — typical "no match" paths return Rewritten=false.
func RewriteScopeDriftPlaceholders(ctx context.Context, req ScopeDriftInboundRewriteRequest) (ScopeDriftInboundRewriteResult, error) {
	out := ScopeDriftInboundRewriteResult{Body: req.Body}
	if req.ScopeDrifts == nil || req.Agent == nil {
		return out, nil
	}
	var (
		rewritten []byte
		applied   []string
		err       error
	)
	switch req.Provider {
	case conversation.ProviderAnthropic:
		rewritten, applied, err = rewriteAnthropicScopeDriftPlaceholders(ctx, req)
	case conversation.ProviderOpenAI:
		if conversation.IsOpenAIChatCompletionsEndpoint(req.HTTPRequest) {
			rewritten, applied, err = rewriteOpenAIChatScopeDriftPlaceholders(ctx, req)
		} else {
			rewritten, applied, err = rewriteOpenAIResponsesScopeDriftPlaceholders(ctx, req)
		}
	default:
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if rewritten == nil {
		return out, nil
	}
	out.Body = rewritten
	out.Rewritten = true
	out.AppliedDriftIDs = applied
	return out, nil
}

func rewriteAnthropicScopeDriftPlaceholders(ctx context.Context, req ScopeDriftInboundRewriteRequest) ([]byte, []string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(req.Body, &raw); err != nil {
		return nil, nil, err
	}
	msgsRaw, ok := raw["messages"]
	if !ok {
		return nil, nil, nil
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(msgsRaw, &messages); err != nil {
		return nil, nil, err
	}
	// Two-pass walk: identify every tool_use_id that has a pending
	// substitution (by probing user-turn tool_result blocks), then
	// apply the substitutions in a single body edit so partial failures
	// don't leave half-applied state.
	type pendingHit struct {
		ToolUseID    string
		Substitution PendingSubstitution
		UserMsgIdx   int
		BlockIdx     int
	}
	var hits []pendingHit
	for i, msg := range messages {
		var probe struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(msg, &probe); err != nil {
			continue
		}
		if probe.Role != "user" || len(probe.Content) == 0 {
			continue
		}
		var blocks []json.RawMessage
		if err := json.Unmarshal(probe.Content, &blocks); err != nil {
			// String-form user content carries no tool_results.
			continue
		}
		for bi, blk := range blocks {
			var blkProbe struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
			}
			if err := json.Unmarshal(blk, &blkProbe); err != nil {
				continue
			}
			if blkProbe.Type != "tool_result" || blkProbe.ToolUseID == "" {
				continue
			}
			subst, found := req.ScopeDrifts.LookupPendingSubstitution(ctx, PendingSubstitutionKey{
				AgentID:        req.Agent.ID,
				ConversationID: req.ConversationID,
				ToolUseID:      blkProbe.ToolUseID,
			})
			if !found {
				continue
			}
			hits = append(hits, pendingHit{
				ToolUseID:    blkProbe.ToolUseID,
				Substitution: subst,
				UserMsgIdx:   i,
				BlockIdx:     bi,
			})
		}
	}
	if len(hits) == 0 {
		return nil, nil, nil
	}

	// Apply substitutions. For each hit:
	//   - rewrite the matching user-turn tool_result block's content
	//   - walk back through prior assistant turns to find the tool_use
	//     block carrying the placeholder for this id, and restore the
	//     original name + input.
	logger := req.Logger
	if logger == nil {
		logger = slog.Default()
	}
	appliedDriftIDs := make([]string, 0, len(hits))
	for _, hit := range hits {
		newMessage, ok := substituteAnthropicToolResultContent(messages[hit.UserMsgIdx], hit.ToolUseID, hit.Substitution.MenuText)
		if !ok {
			logger.WarnContext(ctx, "scope-drift inbound rewrite: failed to substitute tool_result content",
				"tool_use_id", hit.ToolUseID,
				"drift_id", hit.Substitution.DriftID,
			)
			continue
		}
		messages[hit.UserMsgIdx] = newMessage

		restored := false
		for ai := hit.UserMsgIdx - 1; ai >= 0; ai-- {
			candidate, ok := restoreAnthropicAssistantToolUse(messages[ai], hit.ToolUseID, hit.Substitution.OriginalToolName, hit.Substitution.OriginalToolInput)
			if !ok {
				continue
			}
			messages[ai] = candidate
			restored = true
			break
		}
		if !restored {
			logger.WarnContext(ctx, "scope-drift inbound rewrite: could not locate placeholder assistant turn for restoration",
				"tool_use_id", hit.ToolUseID,
				"drift_id", hit.Substitution.DriftID,
			)
		}
		appliedDriftIDs = append(appliedDriftIDs, hit.Substitution.DriftID)
	}

	newMsgs, err := json.Marshal(messages)
	if err != nil {
		return nil, nil, err
	}
	raw["messages"] = newMsgs
	out, err := json.Marshal(raw)
	if err != nil {
		return nil, nil, err
	}
	return out, appliedDriftIDs, nil
}

// substituteAnthropicToolResultContent replaces the content field of
// the tool_result block whose tool_use_id matches targetToolUseID. The
// replacement is rendered as a single text block to match Anthropic's
// preferred multi-block tool_result shape — string-form content works
// too but blocks survive concatenation with other model-generated
// content cleanly.
func substituteAnthropicToolResultContent(message json.RawMessage, targetToolUseID, menuText string) (json.RawMessage, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(message, &raw); err != nil {
		return nil, false
	}
	contentRaw, ok := raw["content"]
	if !ok {
		return nil, false
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(contentRaw, &blocks); err != nil {
		return nil, false
	}
	rewritten := false
	for i, blk := range blocks {
		var probe struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
		}
		if err := json.Unmarshal(blk, &probe); err != nil {
			continue
		}
		if probe.Type != "tool_result" || probe.ToolUseID != targetToolUseID {
			continue
		}
		newBlock, err := json.Marshal(map[string]any{
			"type":        "tool_result",
			"tool_use_id": targetToolUseID,
			"content": []map[string]any{{
				"type": "text",
				"text": menuText,
			}},
		})
		if err != nil {
			continue
		}
		blocks[i] = newBlock
		rewritten = true
	}
	if !rewritten {
		return nil, false
	}
	newContent, err := json.Marshal(blocks)
	if err != nil {
		return nil, false
	}
	raw["content"] = newContent
	out, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	return out, true
}

// rewriteOpenAIResponsesScopeDriftPlaceholders walks the OpenAI
// Responses `input` array, finds function_call_output items whose
// call_id matches a pending substitution, replaces the `output` with
// the menu text, and restores the preceding function_call item's
// (name, arguments) to the model's original.
func rewriteOpenAIResponsesScopeDriftPlaceholders(ctx context.Context, req ScopeDriftInboundRewriteRequest) ([]byte, []string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(req.Body, &raw); err != nil {
		return nil, nil, err
	}
	inputRaw, ok := raw["input"]
	if !ok {
		return nil, nil, nil
	}
	// `input` can be a plain string (single-turn). Plain-string inputs
	// have no tool_call history, so there's nothing to substitute.
	var asString string
	if err := json.Unmarshal(inputRaw, &asString); err == nil {
		return nil, nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(inputRaw, &items); err != nil {
		return nil, nil, err
	}
	logger := req.Logger
	if logger == nil {
		logger = slog.Default()
	}
	var appliedDriftIDs []string
	rewrittenAny := false
	for i, item := range items {
		var probe struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		}
		if err := json.Unmarshal(item, &probe); err != nil {
			continue
		}
		if probe.Type != "function_call_output" || probe.CallID == "" {
			continue
		}
		subst, found := req.ScopeDrifts.LookupPendingSubstitution(ctx, PendingSubstitutionKey{
			AgentID:        req.Agent.ID,
			ConversationID: req.ConversationID,
			ToolUseID:      probe.CallID,
		})
		if !found {
			continue
		}
		newItem, ok := substituteOpenAIResponsesFunctionCallOutput(item, subst.MenuText)
		if !ok {
			logger.WarnContext(ctx, "scope-drift inbound rewrite: failed to substitute function_call_output",
				"call_id", probe.CallID,
				"drift_id", subst.DriftID,
			)
			continue
		}
		items[i] = newItem
		restored := false
		for ai := i - 1; ai >= 0; ai-- {
			candidate, ok := restoreOpenAIResponsesFunctionCall(items[ai], probe.CallID, subst.OriginalToolName, subst.OriginalToolInput)
			if !ok {
				continue
			}
			items[ai] = candidate
			restored = true
			break
		}
		if !restored {
			logger.WarnContext(ctx, "scope-drift inbound rewrite: could not locate placeholder function_call for restoration",
				"call_id", probe.CallID,
				"drift_id", subst.DriftID,
			)
		}
		appliedDriftIDs = append(appliedDriftIDs, subst.DriftID)
		rewrittenAny = true
	}
	if !rewrittenAny {
		return nil, nil, nil
	}
	newInput, err := json.Marshal(items)
	if err != nil {
		return nil, nil, err
	}
	raw["input"] = newInput
	out, err := json.Marshal(raw)
	if err != nil {
		return nil, nil, err
	}
	return out, appliedDriftIDs, nil
}

func substituteOpenAIResponsesFunctionCallOutput(item json.RawMessage, menuText string) (json.RawMessage, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(item, &raw); err != nil {
		return nil, false
	}
	encoded, err := json.Marshal(menuText)
	if err != nil {
		return nil, false
	}
	raw["output"] = encoded
	out, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	return out, true
}

func restoreOpenAIResponsesFunctionCall(item json.RawMessage, targetCallID, originalName string, originalInput []byte) (json.RawMessage, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(item, &raw); err != nil {
		return nil, false
	}
	var typ string
	if err := json.Unmarshal(raw["type"], &typ); err != nil || typ != "function_call" {
		return nil, false
	}
	var callID string
	_ = json.Unmarshal(raw["call_id"], &callID)
	if callID != targetCallID {
		return nil, false
	}
	nameRaw, err := json.Marshal(originalName)
	if err != nil {
		return nil, false
	}
	raw["name"] = nameRaw
	// OpenAI `arguments` is a JSON-encoded string of the input. The
	// inspector and rewriter use raw input bytes (parsed JSON
	// document); convert to the string form the wire format expects.
	if len(originalInput) == 0 {
		argsRaw, _ := json.Marshal("{}")
		raw["arguments"] = argsRaw
	} else {
		argsRaw, err := json.Marshal(string(originalInput))
		if err != nil {
			return nil, false
		}
		raw["arguments"] = argsRaw
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	return out, true
}

// rewriteOpenAIChatScopeDriftPlaceholders walks the OpenAI Chat
// Completions `messages` array, finds tool-role messages whose
// tool_call_id matches a pending substitution, replaces their
// content with the menu text, and restores the corresponding assistant
// message's tool_call (name, arguments) to the model's original.
func rewriteOpenAIChatScopeDriftPlaceholders(ctx context.Context, req ScopeDriftInboundRewriteRequest) ([]byte, []string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(req.Body, &raw); err != nil {
		return nil, nil, err
	}
	msgsRaw, ok := raw["messages"]
	if !ok {
		return nil, nil, nil
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(msgsRaw, &messages); err != nil {
		return nil, nil, err
	}
	logger := req.Logger
	if logger == nil {
		logger = slog.Default()
	}
	var appliedDriftIDs []string
	rewrittenAny := false
	for i, msg := range messages {
		var probe struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
		}
		if err := json.Unmarshal(msg, &probe); err != nil {
			continue
		}
		if probe.Role != "tool" || probe.ToolCallID == "" {
			continue
		}
		subst, found := req.ScopeDrifts.LookupPendingSubstitution(ctx, PendingSubstitutionKey{
			AgentID:        req.Agent.ID,
			ConversationID: req.ConversationID,
			ToolUseID:      probe.ToolCallID,
		})
		if !found {
			continue
		}
		newMsg, ok := substituteOpenAIChatToolMessage(msg, subst.MenuText)
		if !ok {
			logger.WarnContext(ctx, "scope-drift inbound rewrite: failed to substitute chat tool message",
				"tool_call_id", probe.ToolCallID,
				"drift_id", subst.DriftID,
			)
			continue
		}
		messages[i] = newMsg
		restored := false
		for ai := i - 1; ai >= 0; ai-- {
			candidate, ok := restoreOpenAIChatAssistantToolCall(messages[ai], probe.ToolCallID, subst.OriginalToolName, subst.OriginalToolInput)
			if !ok {
				continue
			}
			messages[ai] = candidate
			restored = true
			break
		}
		if !restored {
			logger.WarnContext(ctx, "scope-drift inbound rewrite: could not locate placeholder assistant tool_call for restoration",
				"tool_call_id", probe.ToolCallID,
				"drift_id", subst.DriftID,
			)
		}
		appliedDriftIDs = append(appliedDriftIDs, subst.DriftID)
		rewrittenAny = true
	}
	if !rewrittenAny {
		return nil, nil, nil
	}
	newMsgs, err := json.Marshal(messages)
	if err != nil {
		return nil, nil, err
	}
	raw["messages"] = newMsgs
	out, err := json.Marshal(raw)
	if err != nil {
		return nil, nil, err
	}
	return out, appliedDriftIDs, nil
}

func substituteOpenAIChatToolMessage(message json.RawMessage, menuText string) (json.RawMessage, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(message, &raw); err != nil {
		return nil, false
	}
	encoded, err := json.Marshal(menuText)
	if err != nil {
		return nil, false
	}
	raw["content"] = encoded
	out, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	return out, true
}

func restoreOpenAIChatAssistantToolCall(message json.RawMessage, targetToolCallID, originalName string, originalInput []byte) (json.RawMessage, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(message, &raw); err != nil {
		return nil, false
	}
	var role string
	if err := json.Unmarshal(raw["role"], &role); err != nil || role != "assistant" {
		return nil, false
	}
	toolCallsRaw, ok := raw["tool_calls"]
	if !ok {
		return nil, false
	}
	var toolCalls []json.RawMessage
	if err := json.Unmarshal(toolCallsRaw, &toolCalls); err != nil {
		return nil, false
	}
	rewritten := false
	for i, tc := range toolCalls {
		var tcRaw map[string]json.RawMessage
		if err := json.Unmarshal(tc, &tcRaw); err != nil {
			continue
		}
		var id string
		_ = json.Unmarshal(tcRaw["id"], &id)
		if id != targetToolCallID {
			continue
		}
		fnRaw, ok := tcRaw["function"]
		if !ok {
			continue
		}
		var fn map[string]json.RawMessage
		if err := json.Unmarshal(fnRaw, &fn); err != nil {
			continue
		}
		nameRaw, err := json.Marshal(originalName)
		if err != nil {
			continue
		}
		fn["name"] = nameRaw
		// Arguments is a JSON-encoded string of the input.
		argsStr := "{}"
		if len(originalInput) > 0 {
			argsStr = string(originalInput)
		}
		argsRaw, err := json.Marshal(argsStr)
		if err != nil {
			continue
		}
		fn["arguments"] = argsRaw
		newFn, err := json.Marshal(fn)
		if err != nil {
			continue
		}
		tcRaw["function"] = newFn
		newTC, err := json.Marshal(tcRaw)
		if err != nil {
			continue
		}
		toolCalls[i] = newTC
		rewritten = true
		break
	}
	if !rewritten {
		return nil, false
	}
	newToolCalls, err := json.Marshal(toolCalls)
	if err != nil {
		return nil, false
	}
	raw["tool_calls"] = newToolCalls
	out, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	return out, true
}

// restoreAnthropicAssistantToolUse rewrites a tool_use block whose id
// matches targetToolUseID — replacing the harness-side Bash placeholder
// the response rewriter substituted in — back to the model's original
// (name, input) so the upstream model sees its own call.
func restoreAnthropicAssistantToolUse(message json.RawMessage, targetToolUseID, originalName string, originalInput []byte) (json.RawMessage, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(message, &raw); err != nil {
		return nil, false
	}
	roleRaw, ok := raw["role"]
	if !ok {
		return nil, false
	}
	var role string
	if err := json.Unmarshal(roleRaw, &role); err != nil || role != "assistant" {
		return nil, false
	}
	contentRaw, ok := raw["content"]
	if !ok {
		return nil, false
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(contentRaw, &blocks); err != nil {
		return nil, false
	}
	rewritten := false
	for i, blk := range blocks {
		var probe struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(blk, &probe); err != nil {
			continue
		}
		if probe.Type != "tool_use" || probe.ID != targetToolUseID {
			continue
		}
		// Preserve all top-level keys on the block so the restored
		// shape matches what the model emitted (cache_control,
		// metadata, etc.). Only swap name + input.
		var blockMap map[string]json.RawMessage
		if err := json.Unmarshal(blk, &blockMap); err != nil {
			continue
		}
		nameRaw, err := json.Marshal(originalName)
		if err != nil {
			continue
		}
		blockMap["name"] = nameRaw
		if len(originalInput) == 0 {
			blockMap["input"] = json.RawMessage("{}")
		} else {
			blockMap["input"] = json.RawMessage(originalInput)
		}
		newBlock, err := json.Marshal(blockMap)
		if err != nil {
			continue
		}
		blocks[i] = newBlock
		rewritten = true
		break
	}
	if !rewritten {
		return nil, false
	}
	newContent, err := json.Marshal(blocks)
	if err != nil {
		return nil, false
	}
	raw["content"] = newContent
	out, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	return out, true
}
