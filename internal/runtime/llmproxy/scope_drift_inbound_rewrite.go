package llmproxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// AnthropicInboundRewriter walks Anthropic /v1/messages bodies and
// substitutes pending scope-drift / recoverable-deny / auto-approve
// placeholders. Implements conversation.InboundRewriter so dispatch
// runs through the InboundRegistry the same way response rewriters
// dispatch through ResponseRegistry.
type AnthropicInboundRewriter struct{}

// Name reports the provider for registry lookup.
func (AnthropicInboundRewriter) Name() conversation.Provider {
	return conversation.ProviderAnthropic
}

// MatchesInbound returns true for Anthropic-bound requests. Host
// matching is intentionally coarse (the user agent or path could
// belong to either the lite proxy or the runtime proxy); the
// downstream provider header carries the real signal in production
// and tests synthesize requests with the right Provider already.
func (AnthropicInboundRewriter) MatchesInbound(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	host := strings.ToLower(req.URL.Host)
	if host == "" {
		host = strings.ToLower(req.Host)
	}
	return strings.Contains(host, "anthropic")
}

// RewriteInbound walks the body's messages[] and applies every
// pending substitution the lookup serves. Two-pass: locate every
// tool_result block whose tool_use_id has a pending substitution,
// then for each apply the assistant-turn restoration BEFORE the
// user-turn substitution so a partial failure can't leave the
// transcript permanently inconsistent (placeholder Bash on top, menu
// text below, with nothing tying them to the model's actual call).
func (AnthropicInboundRewriter) RewriteInbound(ctx context.Context, req conversation.InboundRewriteRequest) (conversation.InboundRewriteResult, error) {
	out := conversation.InboundRewriteResult{Body: req.Body}
	if req.Lookup == nil || req.AgentID == "" {
		return out, nil
	}
	rewritten, applied, err := rewriteAnthropicInbound(ctx, req)
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

// OpenAIInboundRewriter walks OpenAI Chat Completions and Responses
// bodies. Sub-shape disambiguation happens inside RewriteInbound
// where the body bytes (and the request's path) are already in scope.
type OpenAIInboundRewriter struct{}

func (OpenAIInboundRewriter) Name() conversation.Provider {
	return conversation.ProviderOpenAI
}

func (OpenAIInboundRewriter) MatchesInbound(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	host := strings.ToLower(req.URL.Host)
	if host == "" {
		host = strings.ToLower(req.Host)
	}
	return strings.Contains(host, "openai")
}

func (OpenAIInboundRewriter) RewriteInbound(ctx context.Context, req conversation.InboundRewriteRequest) (conversation.InboundRewriteResult, error) {
	out := conversation.InboundRewriteResult{Body: req.Body}
	if req.Lookup == nil || req.AgentID == "" {
		return out, nil
	}
	var (
		rewritten []byte
		applied   []string
		err       error
	)
	if conversation.IsOpenAIChatCompletionsEndpoint(req.HTTPRequest) {
		rewritten, applied, err = rewriteOpenAIChatInbound(ctx, req)
	} else {
		rewritten, applied, err = rewriteOpenAIResponsesInbound(ctx, req)
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

// DefaultInboundRegistry returns the canonical inbound rewriter
// dispatch table. Mirrors conversation.DefaultResponseRegistry on the
// outbound leg so both directions route through one consistent
// abstraction.
func DefaultInboundRegistry() *conversation.InboundRegistry {
	return conversation.NewInboundRegistry(
		AnthropicInboundRewriter{},
		OpenAIInboundRewriter{},
	)
}

// substitutionLookupAdapter satisfies conversation.InboundSubstitutionLookup
// for a SubstitutionRegistry. The conversation package can't depend
// on llmproxy, so the registry's tuple-keyed signature is lifted
// here into the conversation-package lookup shape.
type substitutionLookupAdapter struct {
	Registry SubstitutionRegistry
}

func (a substitutionLookupAdapter) LookupPendingSubstitution(ctx context.Context, agentID, conversationID, toolUseID string) (conversation.InboundPendingSubstitution, bool) {
	if a.Registry == nil {
		return conversation.InboundPendingSubstitution{}, false
	}
	subst, ok := a.Registry.LookupPendingSubstitution(ctx, PendingSubstitutionKey{
		AgentID:        agentID,
		ConversationID: conversationID,
		ToolUseID:      toolUseID,
	})
	if !ok {
		return conversation.InboundPendingSubstitution{}, false
	}
	return conversation.InboundPendingSubstitution{
		DriftID:           subst.DriftID,
		MenuText:          subst.MenuText,
		OriginalToolName:  subst.OriginalToolName,
		OriginalToolInput: subst.OriginalToolInput,
	}, true
}

// NewSubstitutionLookup wraps a SubstitutionRegistry so it can be
// passed to inbound rewriters as conversation.InboundSubstitutionLookup.
// Returns nil when the registry is nil so callers can plug the result
// straight into InboundRewriteRequest.Lookup without nil-guards.
func NewSubstitutionLookup(reg SubstitutionRegistry) conversation.InboundSubstitutionLookup {
	if reg == nil {
		return nil
	}
	return substitutionLookupAdapter{Registry: reg}
}

// rewriteAnthropicInbound performs the messages[] walk for Anthropic
// /v1/messages bodies. Two-pass: collect every tool_result block whose
// tool_use_id has a pending substitution (PASS 1), then apply each
// substitution with assistant-turn restoration first / user-turn
// content substitution second (PASS 2).
func rewriteAnthropicInbound(ctx context.Context, req conversation.InboundRewriteRequest) ([]byte, []string, error) {
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
	type pendingHit struct {
		ToolUseID    string
		Substitution conversation.InboundPendingSubstitution
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
			subst, found := req.Lookup.LookupPendingSubstitution(ctx, req.AgentID, req.ConversationID, blkProbe.ToolUseID)
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

	logger := req.Logger
	if logger == nil {
		logger = slog.Default()
	}
	appliedDriftIDs := make([]string, 0, len(hits))
	for _, hit := range hits {
		restoredIdx := -1
		var restoredMessage json.RawMessage
		for ai := hit.UserMsgIdx - 1; ai >= 0; ai-- {
			candidate, ok := restoreAnthropicAssistantToolUse(messages[ai], hit.ToolUseID, hit.Substitution.OriginalToolName, hit.Substitution.OriginalToolInput)
			if !ok {
				continue
			}
			restoredIdx = ai
			restoredMessage = candidate
			break
		}
		if restoredIdx < 0 {
			logger.WarnContext(ctx, "scope-drift inbound rewrite: could not locate placeholder assistant turn for restoration; skipping substitution",
				"tool_use_id", hit.ToolUseID,
				"drift_id", hit.Substitution.DriftID,
			)
			continue
		}
		newUserMessage, ok := substituteAnthropicToolResultContent(messages[hit.UserMsgIdx], hit.ToolUseID, hit.Substitution.MenuText)
		if !ok {
			logger.WarnContext(ctx, "scope-drift inbound rewrite: failed to substitute tool_result content; skipping restoration",
				"tool_use_id", hit.ToolUseID,
				"drift_id", hit.Substitution.DriftID,
			)
			continue
		}
		messages[restoredIdx] = restoredMessage
		messages[hit.UserMsgIdx] = newUserMessage
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

// rewriteOpenAIResponsesInbound walks the OpenAI Responses `input`
// array. In full-input mode it restores the placeholder function_call
// and substitutes the function_call_output. In chained mode
// (previous_response_id set) only the output is substituted —
// OpenAI's server-stored history already holds the model-original
// function_call.
func rewriteOpenAIResponsesInbound(ctx context.Context, req conversation.InboundRewriteRequest) ([]byte, []string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(req.Body, &raw); err != nil {
		return nil, nil, err
	}
	inputRaw, ok := raw["input"]
	if !ok {
		return nil, nil, nil
	}
	var asString string
	if err := json.Unmarshal(inputRaw, &asString); err == nil {
		return nil, nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(inputRaw, &items); err != nil {
		return nil, nil, err
	}
	chained := false
	if prev, ok := raw["previous_response_id"]; ok {
		var s string
		if err := json.Unmarshal(prev, &s); err == nil && strings.TrimSpace(s) != "" {
			chained = true
		}
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
		subst, found := req.Lookup.LookupPendingSubstitution(ctx, req.AgentID, req.ConversationID, probe.CallID)
		if !found {
			continue
		}
		if chained {
			newItem, ok := substituteOpenAIResponsesFunctionCallOutput(item, subst.MenuText)
			if !ok {
				logger.WarnContext(ctx, "scope-drift inbound rewrite: failed to substitute function_call_output (chained mode)",
					"call_id", probe.CallID,
					"drift_id", subst.DriftID,
				)
				continue
			}
			items[i] = newItem
			appliedDriftIDs = append(appliedDriftIDs, subst.DriftID)
			rewrittenAny = true
			continue
		}
		restoredIdx := -1
		var restoredItem json.RawMessage
		for ai := i - 1; ai >= 0; ai-- {
			candidate, ok := restoreOpenAIResponsesFunctionCall(items[ai], probe.CallID, subst.OriginalToolName, subst.OriginalToolInput)
			if !ok {
				continue
			}
			restoredIdx = ai
			restoredItem = candidate
			break
		}
		if restoredIdx < 0 {
			logger.WarnContext(ctx, "scope-drift inbound rewrite: could not locate placeholder function_call for restoration; skipping substitution",
				"call_id", probe.CallID,
				"drift_id", subst.DriftID,
			)
			continue
		}
		newItem, ok := substituteOpenAIResponsesFunctionCallOutput(item, subst.MenuText)
		if !ok {
			logger.WarnContext(ctx, "scope-drift inbound rewrite: failed to substitute function_call_output; skipping restoration",
				"call_id", probe.CallID,
				"drift_id", subst.DriftID,
			)
			continue
		}
		items[restoredIdx] = restoredItem
		items[i] = newItem
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

// rewriteOpenAIChatInbound walks the OpenAI Chat Completions
// `messages` array. Restores assistant tool_calls and substitutes the
// matching role:"tool" message's content.
func rewriteOpenAIChatInbound(ctx context.Context, req conversation.InboundRewriteRequest) ([]byte, []string, error) {
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
		subst, found := req.Lookup.LookupPendingSubstitution(ctx, req.AgentID, req.ConversationID, probe.ToolCallID)
		if !found {
			continue
		}
		restoredIdx := -1
		var restoredMsg json.RawMessage
		for ai := i - 1; ai >= 0; ai-- {
			candidate, ok := restoreOpenAIChatAssistantToolCall(messages[ai], probe.ToolCallID, subst.OriginalToolName, subst.OriginalToolInput)
			if !ok {
				continue
			}
			restoredIdx = ai
			restoredMsg = candidate
			break
		}
		if restoredIdx < 0 {
			logger.WarnContext(ctx, "scope-drift inbound rewrite: could not locate placeholder assistant tool_call for restoration; skipping substitution",
				"tool_call_id", probe.ToolCallID,
				"drift_id", subst.DriftID,
			)
			continue
		}
		newMsg, ok := substituteOpenAIChatToolMessage(msg, subst.MenuText)
		if !ok {
			logger.WarnContext(ctx, "scope-drift inbound rewrite: failed to substitute chat tool message; skipping restoration",
				"tool_call_id", probe.ToolCallID,
				"drift_id", subst.DriftID,
			)
			continue
		}
		messages[restoredIdx] = restoredMsg
		messages[i] = newMsg
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
