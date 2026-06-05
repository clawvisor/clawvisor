package stream

import "io"

// Decoder reads an upstream response body and emits a sequence of
// canonical Events through the Next method. Implementations are
// per-StreamShape (one for Anthropic Messages SSE, one for OpenAI
// Chat Completions SSE, one for OpenAI Responses SSE, plus buffered
// counterparts).
//
// The decoder's job is to:
//  1. Frame the upstream bytes into events.
//  2. Tag each event with its EventKind and Meta.
//  3. Set RawBytes on every event so untouched events can round-trip
//     verbatim.
//
// The decoder MUST NOT populate Parsed unless the EventKind canonically
// requires it (e.g., never populates Parsed for thinking blocks).
// Population of Parsed is a policy-side action (only when about to
// rewrite the event); the decoder's contract is "always RawBytes,
// never Parsed."
type Decoder interface {
	// Next reads the next event from the stream. Returns io.EOF when
	// the stream ends cleanly. Other errors indicate framing problems.
	Next() (Event, error)
}

// DecoderFor returns the decoder appropriate for the given stream shape.
// Streaming responses pass r as the upstream body Reader; buffered
// responses synthesize a Reader from the parsed body and use the same
// API. Returns nil if the shape has no implementation yet — callers
// should fall back to legacy path in that case.
func DecoderFor(_ /*shape*/ any, _ io.Reader) Decoder {
	// Implementation per-shape lands as each shape's decoder is written.
	// Phase 2 starts with Anthropic SSE; OpenAI shapes follow.
	return nil
}
