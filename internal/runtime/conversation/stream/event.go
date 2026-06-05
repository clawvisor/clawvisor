// Package stream models LLM responses as a unified event sequence.
//
// Both streaming SSE responses and buffered JSON responses decode into the
// same Event channel; both encode back to wire format the same way. This
// is the abstraction Phase 2 of the LLM proxy refactor introduces — see
// .context/llmproxy-refactor-plan.md.
//
// THE THREE-STATE EVENT CONTRACT
//
// Each Event lives in exactly one of three states. The encoder asserts
// the invariant and unit tests pin it.
//
//  1. PASS-THROUGH: RawBytes set, FieldPatches empty, Parsed nil.
//     Encoder emits RawBytes verbatim. Used for events untouched by any
//     policy. Preserves thinking-block signatures byte-for-byte.
//
//  2. PATCHED: RawBytes set, FieldPatches non-empty, Parsed nil.
//     Encoder applies each FieldPatch to RawBytes via json_surgery
//     (typically a single SetJSONField for an index shift) and emits.
//     Used for events that are byte-identical to upstream EXCEPT for
//     a small set of field edits — the common case when a leading
//     block has been injected and sibling block indices need to shift.
//
//  3. REPLACED: RawBytes empty, FieldPatches empty, Parsed set.
//     Encoder re-serializes from Parsed. Used for events whose content
//     has genuinely changed (new text, rewritten tool_use input,
//     fully-synthesized notice events).
//
// Misuse of the contract — e.g., setting both RawBytes and Parsed —
// would corrupt thinking signatures silently. The Event.Validate method
// catches the mismatch; encoders call it before emit.
package stream

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// EventKind names a position in the provider-agnostic response structure.
// Concrete provider events map onto these (Anthropic's `content_block_start`
// is a BlockStart; OpenAI Responses' `response.output_item.added` for an
// assistant message is a MessageStart; etc.).
type EventKind int

const (
	// KindUnknown is the zero value. Indicates the decoder didn't
	// recognize the event. Encoders emit RawBytes verbatim.
	KindUnknown EventKind = iota
	// KindResponseStart is the top-level envelope opening (Anthropic's
	// `message_start`, OpenAI Responses' `response.created`).
	KindResponseStart
	// KindMessageStart marks the start of an assistant message item.
	// For Anthropic this is implicit in `message_start`; for OpenAI
	// Responses it's `response.output_item.added` of an assistant item.
	KindMessageStart
	// KindBlockStart opens a content block: text, tool_use, thinking.
	KindBlockStart
	// KindBlockDelta is a streaming delta within a block.
	KindBlockDelta
	// KindBlockEnd closes a content block.
	KindBlockEnd
	// KindMessageEnd closes the assistant message.
	KindMessageEnd
	// KindResponseEnd is the top-level envelope close (Anthropic's
	// `message_stop`, OpenAI's `response.completed` or `data: [DONE]`).
	KindResponseEnd
	// KindKeepalive is an SSE comment line, vendor ping, or other
	// non-content noise. Encoders pass through verbatim.
	KindKeepalive
)

// Event is one unit of the unified response stream.
type Event struct {
	Kind  EventKind
	Shape conversation.StreamShape

	// State 1: PASS-THROUGH (RawBytes set, others empty).
	// State 2: PATCHED (RawBytes set, FieldPatches non-empty, Parsed nil).
	// State 3: REPLACED (RawBytes nil, Parsed non-nil).
	RawBytes     []byte
	FieldPatches []FieldPatch
	Parsed       EventPayload

	// Meta carries provider-specific fields the canonical model can't
	// generalize: Anthropic block index, OpenAI item_id, content_index,
	// SSE event name. Encoders consult Meta when re-serializing.
	Meta EventMeta
}

// FieldPatch is a minimal JSON field edit to a pass-through event's
// RawBytes. JSONPath is dotted (e.g., "index", "delta.text", "item.id");
// the encoder uses json_surgery to apply it while preserving every
// other byte.
type FieldPatch struct {
	JSONPath string
	NewValue json.RawMessage
}

// EventMeta carries provider-specific event metadata. Fields are zero
// when not applicable.
type EventMeta struct {
	// SSEEventName is the `event:` line value for SSE streams
	// (e.g., "message_start", "content_block_delta", "response.created").
	// Empty for buffered events.
	SSEEventName string

	// AnthropicIndex is the `index` field on Anthropic content_block_*
	// events. -1 indicates "not set."
	AnthropicIndex int

	// OpenAIOutputIndex is the `output_index` field on OpenAI Responses
	// events. -1 indicates "not set."
	OpenAIOutputIndex int

	// OpenAIContentIndex is the `content_index` field on OpenAI Responses
	// events. -1 indicates "not set."
	OpenAIContentIndex int

	// OpenAIItemID is the `item_id` field on OpenAI Responses events.
	OpenAIItemID string
}

// EventPayload is the typed REPLACED-state payload. Implementers are
// the concrete block types below; encoders type-switch on this when
// re-serializing.
type EventPayload interface{ isEventPayload() }

// TextBlock carries text content for a BlockStart/BlockDelta/BlockEnd.
type TextBlock struct {
	Text string
}

func (TextBlock) isEventPayload() {}

// ToolUseBlock carries an assistant-emitted tool_use block.
type ToolUseBlock struct {
	ID    string
	Name  string
	Input json.RawMessage
}

func (ToolUseBlock) isEventPayload() {}

// ThinkingBlock is the opaque thinking-block payload. The decoder never
// populates Parsed for thinking blocks — they're always state 1 or 2,
// never re-serialized — but the type exists so policies can detect and
// skip thinking blocks when walking content.
type ThinkingBlock struct{}

func (ThinkingBlock) isEventPayload() {}

// RawPayload is the fallback for events the canonical vocabulary doesn't
// model. Carries the original bytes; encoders emit verbatim. Used for
// provider-specific event types we don't need to introspect.
type RawPayload struct{}

func (RawPayload) isEventPayload() {}

// Validate enforces the three-state invariant. Encoders call this before
// emit; misuse panics in tests (via t.Helper plus Validate) rather than
// corrupting bytes in production.
func (e Event) Validate() error {
	hasRaw := len(e.RawBytes) > 0
	hasPatches := len(e.FieldPatches) > 0
	hasParsed := e.Parsed != nil

	switch {
	case hasRaw && hasParsed:
		return errors.New("stream.Event: both RawBytes and Parsed set (must be one or the other)")
	case hasPatches && !hasRaw:
		return errors.New("stream.Event: FieldPatches set without RawBytes (patches apply to raw bytes)")
	case hasPatches && hasParsed:
		return errors.New("stream.Event: FieldPatches set with Parsed (patches apply to raw bytes, not parsed)")
	case !hasRaw && !hasParsed && e.Kind != KindKeepalive:
		return fmt.Errorf("stream.Event: kind=%v has neither RawBytes nor Parsed", e.Kind)
	}
	return nil
}

// State returns which of the three contract states this event is in.
// Used by encoders to dispatch and by tests to assert.
type State int

const (
	StatePassThrough State = iota // RawBytes only
	StatePatched                  // RawBytes + FieldPatches
	StateReplaced                 // Parsed only
)

func (e Event) State() State {
	if e.Parsed != nil {
		return StateReplaced
	}
	if len(e.FieldPatches) > 0 {
		return StatePatched
	}
	return StatePassThrough
}
