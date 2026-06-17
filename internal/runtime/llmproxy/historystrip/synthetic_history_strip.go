package historystrip

import (
	"encoding/json"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/jsonsurgery"
)

type SyntheticApprovalHistoryStripRequest struct {
	Provider conversation.Provider
	Body     []byte
	// ReconstructionLookup, when non-nil, lets the strip path
	// REPLACE the substituted-prompt assistant turn with a
	// synthetic [tool_use(original)] + paired tool_result rather
	// than dropping it. Without this, reconstruction is one-shot
	// (only the post-approval body editor sees the reconstructed
	// pair) and subsequent turns lose evidence of the original
	// call.
	//
	// The lookup is keyed by the approval_id parsed from the
	// substituted-prompt marker. Returns nil when no
	// reconstruction is available (older approvals predating the
	// lifecycle audit, or store outage) — strip falls back to
	// drop-the-turn behavior.
	//
	// Callers wire this with whatever store they have. The
	// historystrip package itself stays storage-agnostic.
	ReconstructionLookup ReconstructionLookup
}

// ReconstructionLookup queries the lifecycle audit (or any
// equivalent source) for the data needed to reconstruct the model's
// missing assistant turn after a substituted-prompt approval.
//
// Returns nil when reconstruction is not possible for this
// approval_id; the caller falls through to the drop-the-turn path.
type ReconstructionLookup func(approvalID string) *ReconstructedPair

// ReconstructedPair carries everything the strip path needs to
// inject a synthetic [tool_use, tool_result] pair where the
// substituted-prompt turn used to live.
//
// ToolUseID / ToolName / Input are the agent's verbatim original
// tool_use (captured at hold time, stored in task_lifecycle_events).
// ResultText is the notice the synthetic tool_result content
// carries — typically the same body the augmenter emits ("scope was
// expanded…" or "task was created…"), composed by the caller from
// the event Kind.
type ReconstructedPair struct {
	ToolUseID  string
	ToolName   string
	Input      json.RawMessage
	ResultText string
}

type SyntheticApprovalHistoryStripResult struct {
	Body     []byte
	Modified bool
}

const ToolApprovalSubstitutedPromptMarker = "Clawvisor paused this tool call for approval."

// StripSyntheticApprovalHistory removes Clawvisor-generated approval UI from
// conversation history before it is sent back to the upstream model. The live
// pending-approval cache is the source of truth; historical assistant text that
// looks like an approval prompt is untrusted model context and can be copied or
// hallucinated by the model on later turns.
func StripSyntheticApprovalHistory(req SyntheticApprovalHistoryStripRequest) (SyntheticApprovalHistoryStripResult, error) {
	if len(req.Body) == 0 {
		return SyntheticApprovalHistoryStripResult{Body: req.Body}, nil
	}
	body := req.Body
	modified := false
	if req.Provider == conversation.ProviderAnthropic {
		res, err := stripAnthropicSyntheticApprovalHistory(body, req.ReconstructionLookup)
		if err != nil {
			return SyntheticApprovalHistoryStripResult{Body: body}, err
		}
		if res.Modified {
			body = res.Body
			modified = true
		}
		return SyntheticApprovalHistoryStripResult{Body: body, Modified: modified}, nil
	}
	return SyntheticApprovalHistoryStripResult{Body: body}, nil
}

func stripAnthropicSyntheticApprovalHistory(body []byte, lookup ReconstructionLookup) (SyntheticApprovalHistoryStripResult, error) {
	if !strings.Contains(string(body), "Clawvisor") {
		return SyntheticApprovalHistoryStripResult{Body: body}, nil
	}
	// Byte fidelity invariant: surviving messages pass through verbatim.
	// Top-level body keys keep their order. Only messages we merge get
	// re-marshalled — and those are user messages, never assistants
	// carrying thinking blocks, so signature verification is unaffected.
	msgsStart, msgsEnd, ok := jsonsurgery.FindFieldValue(body, "messages")
	if !ok {
		return SyntheticApprovalHistoryStripResult{Body: body}, nil
	}
	messages, ok := jsonsurgery.FlattenArray(body[msgsStart:msgsEnd])
	if !ok {
		return SyntheticApprovalHistoryStripResult{Body: body}, nil
	}
	survivors := make([]json.RawMessage, 0, len(messages))
	modified := false
	skipNextBareApprovalReply := false
	// orphanedToolUseIDs are the tool_use_ids from a just-stripped
	// assistant turn (e.g. AskUserQuestion). Any tool_result on the
	// next user turn referencing one of these has no parent tool_use
	// in history anymore — Anthropic rejects orphan tool_results with
	// a 400, so the strip must clean both ends of the pair together.
	var orphanedToolUseIDs map[string]struct{}
	// pendingReconstruction, when set, signals that the next user
	// turn must be paired with the reconstruction's ToolUseID. Two
	// shapes feed in here:
	//   - AskUserQuestion path: the substituted assistant turn carried
	//     a synthetic picker tool_use; orphanedToolUseIDs holds its id
	//     and the user's tool_result for it gets SWAPPED to point at
	//     the reconstructed tool_use_id.
	//   - Text-only path: no synthetic picker tool_use existed, so
	//     orphanedToolUseIDs is empty. The user's content (a plain
	//     notice text the body editor or augmenter spliced in) gets
	//     WRAPPED into a fresh tool_result paired to the reconstructed
	//     tool_use_id. Without the wrap the next request goes upstream
	//     with a reconstructed [tool_use] but a plain-text user turn,
	//     and Anthropic rejects with "tool_use ids were found without
	//     tool_result blocks immediately after" (the Telegram-bot /
	//     Codex agents failure mode).
	var pendingReconstruction *ReconstructedPair
	for _, msg := range messages {
		role := extractMessageRole(msg)
		content := extractMessageContent(msg)
		contentText := flattenAnthropicTaskReplyText(content)
		if skipNextBareApprovalReply {
			skipNextBareApprovalReply = false
			if role == "user" && isBareSyntheticApprovalReply(contentText) {
				modified = true
				continue
			}
		}
		if role == "user" && pendingReconstruction != nil {
			if len(orphanedToolUseIDs) > 0 {
				// AskUserQuestion path: replace the orphan
				// tool_result block with one paired to the
				// reconstructed tool_use_id.
				swapped, swapChanged, swapErr := replaceToolResultsForReconstruction(content, orphanedToolUseIDs, pendingReconstruction)
				if swapErr == nil && swapChanged {
					modified = true
					newMsg, err := jsonsurgery.SetField(msg, "content", swapped)
					if err == nil {
						msg = newMsg
					}
				}
			} else {
				// Text-only path: the original substituted turn
				// carried no synthetic picker tool_use, so the
				// user content has no orphan tool_result to swap.
				// Wrap the user's notice text as a fresh
				// tool_result paired to the reconstructed
				// tool_use_id so Anthropic's
				// tool_use→tool_result adjacency holds.
				wrapped, wrapChanged, wrapErr := wrapUserContentAsToolResult(content, pendingReconstruction)
				if wrapErr == nil && wrapChanged {
					modified = true
					newMsg, err := jsonsurgery.SetField(msg, "content", wrapped)
					if err == nil {
						msg = newMsg
					}
				}
			}
			orphanedToolUseIDs = nil
			pendingReconstruction = nil
		} else if role == "user" && len(orphanedToolUseIDs) > 0 {
			cleaned, dropped, changed, err := stripToolResultsByID(content, orphanedToolUseIDs)
			orphanedToolUseIDs = nil
			if err == nil && changed {
				modified = true
				if dropped {
					// User message had only the orphan tool_result
					// (and maybe blank text). Drop the whole turn.
					continue
				}
				newMsg, err := jsonsurgery.SetField(msg, "content", cleaned)
				if err == nil {
					msg = newMsg
				}
			}
		}
		if role == "assistant" && isSyntheticApprovalPromptText(contentText) {
			// Try reconstruction first: if the lookup callback
			// returns a faithful tool_use snapshot for this
			// approval, REPLACE this turn with a synthetic
			// [tool_use(original)] instead of dropping it. The
			// model sees its own call on every subsequent turn
			// (not just the post-approval one).
			ids := extractClawvisorSyntheticToolUseIDs(content)
			var reconstructed *ReconstructedPair
			if lookup != nil {
				if approvalID := findApprovalIDInPrompt(contentText); approvalID != "" {
					reconstructed = lookup(approvalID)
				}
			}
			if reconstructed != nil && reconstructed.ToolUseID != "" && reconstructed.ToolName != "" && len(reconstructed.Input) > 0 {
				replacement, ok := buildReconstructedAssistantBlock(reconstructed)
				if ok {
					newMsg, err := jsonsurgery.SetField(msg, "content", replacement)
					if err == nil {
						msg = newMsg
						modified = true
						survivors = append(survivors, msg)
						// Signal the next user turn to pair
						// up with the reconstructed tool_use:
						// SWAP an existing tool_result (when
						// the substituted turn carried a
						// synthetic picker call) or WRAP the
						// user's text content as a fresh
						// tool_result (text-only path). The
						// pendingReconstruction flag carries
						// the data; orphanedToolUseIDs
						// selects between the two paths.
						pendingReconstruction = reconstructed
						if len(ids) > 0 {
							orphanedToolUseIDs = make(map[string]struct{}, len(ids))
							for _, id := range ids {
								orphanedToolUseIDs[id] = struct{}{}
							}
						}
						skipNextBareApprovalReply = true
						continue
					}
				}
			}
			// Fall through to drop-the-turn behavior when
			// reconstruction is unavailable or block-building
			// failed.
			modified = true
			skipNextBareApprovalReply = true
			if len(ids) > 0 {
				if orphanedToolUseIDs == nil {
					orphanedToolUseIDs = make(map[string]struct{}, len(ids))
				}
				for _, id := range ids {
					orphanedToolUseIDs[id] = struct{}{}
				}
			}
			continue
		}
		survivors = append(survivors, msg)
	}
	if !modified || len(survivors) == 0 {
		return SyntheticApprovalHistoryStripResult{Body: body}, nil
	}

	// Merge consecutive user messages that became adjacent after the strip.
	var merged []json.RawMessage
	for _, msg := range survivors {
		if len(merged) == 0 {
			merged = append(merged, msg)
			continue
		}
		prev := merged[len(merged)-1]
		prevRole := extractMessageRole(prev)
		currRole := extractMessageRole(msg)
		if prevRole == currRole && currRole == "user" {
			prevContent := extractMessageContent(prev)
			currContent := extractMessageContent(msg)
			if canMergeAnthropicContent(prevContent, currContent) {
				mergedContent, err := mergeAnthropicContent(prevContent, currContent)
				if err != nil {
					return SyntheticApprovalHistoryStripResult{Body: body}, err
				}
				newPrev, err := jsonsurgery.SetField(prev, "content", mergedContent)
				if err != nil {
					return SyntheticApprovalHistoryStripResult{Body: body}, err
				}
				merged[len(merged)-1] = newPrev
				continue
			}
		}
		merged = append(merged, msg)
	}

	newMsgsBytes, err := jsonsurgery.MarshalNoEscape(merged)
	if err != nil {
		return SyntheticApprovalHistoryStripResult{Body: body}, err
	}
	next, err := jsonsurgery.SetField(body, "messages", newMsgsBytes)
	if err != nil {
		return SyntheticApprovalHistoryStripResult{Body: body}, err
	}
	return SyntheticApprovalHistoryStripResult{Body: next, Modified: true}, nil
}

func extractMessageRole(msg json.RawMessage) string {
	start, end, ok := jsonsurgery.FindFieldValue(msg, "role")
	if !ok {
		return ""
	}
	var s string
	_ = json.Unmarshal(msg[start:end], &s)
	return s
}

func extractMessageContent(msg json.RawMessage) json.RawMessage {
	start, end, ok := jsonsurgery.FindFieldValue(msg, "content")
	if !ok {
		return nil
	}
	return msg[start:end]
}

func canMergeAnthropicContent(c1, c2 json.RawMessage) bool {
	var s1, s2 string
	err1 := json.Unmarshal(c1, &s1)
	err2 := json.Unmarshal(c2, &s2)
	if err1 == nil || err2 == nil {
		return err1 == nil && err2 == nil
	}
	var blocks1, blocks2 []json.RawMessage
	return json.Unmarshal(c1, &blocks1) == nil && json.Unmarshal(c2, &blocks2) == nil
}

func mergeAnthropicContent(c1, c2 json.RawMessage) (json.RawMessage, error) {
	if len(c1) == 0 {
		return c2, nil
	}
	if len(c2) == 0 {
		return c1, nil
	}

	var s1, s2 string
	err1 := json.Unmarshal(c1, &s1)
	err2 := json.Unmarshal(c2, &s2)

	if err1 == nil && err2 == nil {
		merged := s1 + "\n\n" + s2
		return jsonsurgery.MarshalNoEscape(merged)
	}
	if err1 != nil && err2 == nil {
		var blocks1 []json.RawMessage
		if err := json.Unmarshal(c1, &blocks1); err != nil {
			return nil, err
		}
		blocks1 = append(blocks1, anthropicTextBlockRaw(s2))
		return jsonsurgery.MarshalNoEscape(blocks1)
	}
	if err1 == nil && err2 != nil {
		var blocks2 []json.RawMessage
		if err := json.Unmarshal(c2, &blocks2); err != nil {
			return nil, err
		}
		// Use append-from-literal to sidestep CodeQL's
		// allocation-size-overflow warning on `len(blocks2)+1`.
		out := append([]json.RawMessage{anthropicTextBlockRaw(s1)}, blocks2...)
		return jsonsurgery.MarshalNoEscape(out)
	}

	var blocks1 []json.RawMessage
	if err := json.Unmarshal(c1, &blocks1); err != nil {
		return nil, err
	}

	var blocks2 []json.RawMessage
	if err := json.Unmarshal(c2, &blocks2); err != nil {
		return nil, err
	}

	mergedBlocks := append(blocks1, blocks2...)
	return jsonsurgery.MarshalNoEscape(mergedBlocks)
}

func anthropicTextBlockRaw(text string) json.RawMessage {
	block, _ := jsonsurgery.MarshalNoEscape(map[string]string{
		"type": "text",
		"text": text,
	})
	return json.RawMessage(block)
}

func rawMessageString(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func isSyntheticApprovalPromptText(text string) bool {
	return strings.Contains(text, InlineApprovalSubstitutedPromptMarker) ||
		strings.Contains(text, InlineExpansionApprovalSubstitutedPromptMarker) ||
		strings.Contains(text, ToolApprovalSubstitutedPromptMarker)
}

// buildReconstructedAssistantBlock renders the synthetic
// [tool_use(original)] content that replaces the substituted-prompt
// assistant turn. The block is wrapped in a single-element JSON
// array (Anthropic content shape) so the caller can drop it
// straight into a message's content field.
func buildReconstructedAssistantBlock(rec *ReconstructedPair) (json.RawMessage, bool) {
	if rec == nil || rec.ToolUseID == "" || rec.ToolName == "" || len(rec.Input) == 0 {
		return nil, false
	}
	block := map[string]any{
		"type":  "tool_use",
		"id":    rec.ToolUseID,
		"name":  rec.ToolName,
		"input": rec.Input,
	}
	raw, err := jsonsurgery.MarshalNoEscape([]any{block})
	if err != nil {
		return nil, false
	}
	return raw, true
}

// wrapUserContentAsToolResult is the text-only-path counterpart to
// replaceToolResultsForReconstruction. When the substituted-prompt
// assistant turn carried no synthetic picker tool_use (the
// AskUserQuestion-less path used by Codex / Telegram-bot agents), the
// user turn has no orphan tool_result to swap — just a notice text
// block (or plain string content) the body editor / augmenter
// produced. Wrap that notice into a fresh tool_result block paired to
// the reconstruction's ToolUseID so the next request's
// [reconstructed tool_use] → [user tool_result] adjacency is valid.
//
// Skips when:
//   - content already contains a tool_result block (already wrapped,
//     or paired against an unrelated exchange — don't double-wrap).
//   - content blocks contain non-text shapes (tool_use, image, …) —
//     unsafe to re-shape into a single tool_result content.
//   - content is a multi-text-block array with no notice marker —
//     we can't tell which block is the approval reply, so refuse to
//     guess.
//
// On wrap, the tool_result becomes the first content block and any
// non-notice text blocks (e.g. system reminders the harness appended)
// pass through unchanged alongside it.
func wrapUserContentAsToolResult(raw json.RawMessage, rec *ReconstructedPair) (json.RawMessage, bool, error) {
	if len(raw) == 0 || rec == nil || rec.ToolUseID == "" {
		return raw, false, nil
	}
	// Plain-string content: the body editor's text-shape rewrite
	// (replaceAnthropicApprovalReply) lands here. Wrap the whole
	// string as the tool_result's content.
	var simple string
	if err := json.Unmarshal(raw, &simple); err == nil {
		block, err := jsonsurgery.MarshalNoEscape(map[string]any{
			"type":        "tool_result",
			"tool_use_id": rec.ToolUseID,
			"content":     simple,
		})
		if err != nil {
			return raw, false, err
		}
		out, err := jsonsurgery.MarshalNoEscape([]json.RawMessage{block})
		if err != nil {
			return raw, false, err
		}
		return out, true, nil
	}
	// Block-array content: the persistent augmenter
	// (augmentAnthropicApprovedInlineTasks) lands here after splicing
	// the notice into a text block.
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return raw, false, nil
	}
	noticeIdx := -1
	var noticeText string
	var noticeCacheControl json.RawMessage
	for i, blk := range blocks {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(blk, &fields); err != nil {
			return raw, false, nil
		}
		var typeStr, textStr string
		_ = json.Unmarshal(fields["type"], &typeStr)
		_ = json.Unmarshal(fields["text"], &textStr)
		switch typeStr {
		case "tool_result":
			// Existing tool_result block — leave the message
			// alone rather than risk a double-wrap.
			return raw, false, nil
		case "text":
			if noticeIdx < 0 && ContainsInlineApprovalAugmentationMarker(textStr) {
				noticeIdx = i
				noticeText = textStr
				// Preserve cache_control if the harness pinned
				// its breakpoint on the notice block (rare here
				// but matches the tool_result path's invariant).
				if cc, ok := fields["cache_control"]; ok && len(cc) > 0 {
					noticeCacheControl = cc
				}
			}
		default:
			// Non-text, non-tool_result block (tool_use, image,
			// document, …). Refuse to re-shape; the wrap path
			// isn't equipped to preserve the semantics.
			return raw, false, nil
		}
	}
	if noticeIdx < 0 {
		// No notice marker in any text block — can't tell which
		// block belongs in the tool_result. Skip rather than guess.
		return raw, false, nil
	}
	toolResultFields := map[string]any{
		"type":        "tool_result",
		"tool_use_id": rec.ToolUseID,
		"content":     noticeText,
	}
	if len(noticeCacheControl) > 0 {
		toolResultFields["cache_control"] = noticeCacheControl
	}
	toolResultBlock, err := jsonsurgery.MarshalNoEscape(toolResultFields)
	if err != nil {
		return raw, false, err
	}
	// Prepend the tool_result; drop the original notice text block;
	// preserve the remaining blocks (system reminders, etc.) in order.
	newBlocks := make([]json.RawMessage, 0, len(blocks))
	newBlocks = append(newBlocks, toolResultBlock)
	for i, blk := range blocks {
		if i == noticeIdx {
			continue
		}
		newBlocks = append(newBlocks, blk)
	}
	out, err := jsonsurgery.MarshalNoEscape(newBlocks)
	if err != nil {
		return raw, false, err
	}
	return out, true, nil
}

// replaceToolResultsForReconstruction walks the user-turn content,
// replacing tool_result blocks whose tool_use_id matches any of the
// stripped synthetic ids with a tool_result paired to the
// reconstructed tool_use_id (carrying the reconstruction's
// ResultText as content). Other blocks pass through unchanged.
//
// Any non-(type|tool_use_id|content) fields on the original block —
// notably cache_control — survive the swap. The harness's deepest
// prompt-cache breakpoint lands on this tool_result; dropping it
// forces Anthropic to fall back to a system-level cache that has
// proven not to hit in practice, busting ~15k cached tokens on every
// approval turn.
func replaceToolResultsForReconstruction(raw json.RawMessage, orphans map[string]struct{}, rec *ReconstructedPair) (json.RawMessage, bool, error) {
	if len(raw) == 0 || rec == nil || rec.ToolUseID == "" {
		return raw, false, nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return raw, false, nil
	}
	changed := false
	for i, blk := range blocks {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(blk, &fields); err != nil {
			continue
		}
		var typeStr, toolUseID string
		_ = json.Unmarshal(fields["type"], &typeStr)
		_ = json.Unmarshal(fields["tool_use_id"], &toolUseID)
		if typeStr != "tool_result" {
			continue
		}
		if _, ok := orphans[toolUseID]; !ok {
			continue
		}
		newToolUseID, err := jsonsurgery.MarshalNoEscape(rec.ToolUseID)
		if err != nil {
			continue
		}
		newContent, err := jsonsurgery.MarshalNoEscape(rec.ResultText)
		if err != nil {
			continue
		}
		fields["tool_use_id"] = newToolUseID
		fields["content"] = newContent
		newBlock, err := jsonsurgery.MarshalNoEscape(fields)
		if err != nil {
			continue
		}
		blocks[i] = newBlock
		changed = true
	}
	if !changed {
		return raw, false, nil
	}
	out, err := jsonsurgery.MarshalNoEscape(blocks)
	if err != nil {
		return raw, false, err
	}
	return out, true, nil
}

// findApprovalIDInPrompt extracts the cv-<id> marker from the
// substituted-prompt text so the lookup callback can query the
// lifecycle audit. Mirrors conversation.FindLatestApprovalIDMarker's
// regex without taking a cross-package dep (the historystrip
// package's only external dep is conversation.Provider).
func findApprovalIDInPrompt(text string) string {
	const marker = "[clawvisor:approval="
	idx := strings.LastIndex(text, marker)
	if idx < 0 {
		return ""
	}
	rest := text[idx+len(marker):]
	end := strings.Index(rest, "]")
	if end < 0 {
		return ""
	}
	id := strings.TrimSpace(rest[:end])
	if !strings.HasPrefix(id, "cv-") {
		return ""
	}
	return strings.ToLower(id)
}

// extractClawvisorSyntheticToolUseIDs walks an assistant message's
// content blocks and returns the IDs of every tool_use whose ID
// carries the SyntheticToolUseIDPrefix namespace — i.e. the picker
// calls Clawvisor synthesized for an inline approval substitution.
// The strip path uses these to delete the matching tool_result
// blocks from the next user turn so Anthropic doesn't see an
// orphan tool_result and 400 the request.
//
// Filtering by prefix (not by tool name) keeps this package
// harness-agnostic: the synthesizer is the only producer of the
// prefix, so the strip doesn't need to know whether the substituted
// tool is AskUserQuestion (Claude Code), some other native picker,
// or a future variant.
func extractClawvisorSyntheticToolUseIDs(content json.RawMessage) []string {
	if len(content) == 0 {
		return nil
	}
	var blocks []struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil
	}
	var ids []string
	for _, b := range blocks {
		if b.Type == "tool_use" && strings.HasPrefix(b.ID, SyntheticToolUseIDPrefix) {
			ids = append(ids, b.ID)
		}
	}
	return ids
}

// stripToolResultsByID removes tool_result blocks whose tool_use_id
// is in orphans from content. Returns:
//   - cleaned: the content with matching tool_results removed (only
//     populated when changed is true)
//   - dropped: true when the message has no remaining meaningful
//     blocks after the strip (caller should drop the message entirely)
//   - changed: true when any tool_result was actually removed
func stripToolResultsByID(content json.RawMessage, orphans map[string]struct{}) (cleaned json.RawMessage, dropped, changed bool, err error) {
	if len(content) == 0 || len(orphans) == 0 {
		return nil, false, false, nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, false, false, nil
	}
	kept := blocks[:0]
	for _, blk := range blocks {
		var probe struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
			Text      string `json:"text,omitempty"`
		}
		if err := json.Unmarshal(blk, &probe); err == nil {
			if probe.Type == "tool_result" {
				if _, isOrphan := orphans[probe.ToolUseID]; isOrphan {
					changed = true
					continue
				}
			}
		}
		kept = append(kept, blk)
	}
	if !changed {
		return nil, false, false, nil
	}
	// "Dropped" if nothing meaningful remains — only blank text or
	// truly empty content. The harness sometimes pads with
	// system-reminder text blocks alongside the tool_result; those
	// stay and the message survives with just the reminders.
	dropped = !hasMeaningfulBlocks(kept)
	if dropped {
		return nil, true, true, nil
	}
	out, err := jsonsurgery.MarshalNoEscape(kept)
	if err != nil {
		return nil, false, false, err
	}
	return out, false, true, nil
}

func hasMeaningfulBlocks(blocks []json.RawMessage) bool {
	for _, blk := range blocks {
		var probe struct {
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
		}
		if err := json.Unmarshal(blk, &probe); err != nil {
			return true
		}
		if probe.Type == "text" {
			if strings.TrimSpace(probe.Text) != "" {
				return true
			}
			continue
		}
		// Any non-text block (tool_use, tool_result, image, etc.) is
		// meaningful by default.
		return true
	}
	return false
}

func isBareSyntheticApprovalReply(text string) bool {
	// ContainsInlineApprovalAugmentationMarker recognizes every
	// proxy-substituted inline-task notice (approved / denied / error)
	// via the shared `<clawvisor-notice kind="task-` substring. A turn
	// carrying that substring is the proxy's own rewrite, not a bare
	// approval verb from the user.
	if ContainsInlineApprovalAugmentationMarker(text) {
		return false
	}
	verb, _ := conversation.ParseApprovalReplyText(text)
	return verb != ""
}
