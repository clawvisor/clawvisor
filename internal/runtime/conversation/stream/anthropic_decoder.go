package stream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// AnthropicDecoder parses Anthropic Messages SSE into the canonical
// Event stream. Each event is line-buffered until a blank-line
// terminator arrives, then emitted with its RawBytes equal to the
// exact upstream bytes (including the `event:`/`data:` lines and the
// terminating blank line).
//
// This preserves the round-trip property: Decode(stream) → Encode(...)
// emits byte-identical output when no events are mutated.
type AnthropicDecoder struct {
	r          *bufio.Scanner
	rawBuf     bytes.Buffer
	curEvent   string
	dataLines  []string
	done       bool
	emittedEOF bool
}

// NewAnthropicDecoder wraps r and returns a Decoder that emits one
// canonical Event per complete SSE event. The scanner sees lines as
// SSE specifies (LF or CRLF terminated); the decoder rebuilds each
// event's exact byte sequence via the rawBuf shadow.
func NewAnthropicDecoder(r io.Reader) *AnthropicDecoder {
	s := bufio.NewScanner(r)
	s.Split(scanSSELines)
	// SSE events can be larger than the default 64 KiB scanner buffer
	// (e.g., a tool_use input with a large JSON object).
	s.Buffer(make([]byte, 0, 4096), maxSSELineSize)
	return &AnthropicDecoder{r: s}
}

// Next returns the next event. Returns io.EOF after the stream is fully
// drained. Errors propagate framing problems from the scanner.
func (d *AnthropicDecoder) Next() (Event, error) {
	if d.emittedEOF {
		return Event{}, io.EOF
	}
	for d.r.Scan() {
		line := d.r.Text()
		// Track exact bytes: scanner strips the trailing \n; we re-add
		// it so rawBuf reflects what was on the wire.
		d.rawBuf.WriteString(line)
		d.rawBuf.WriteByte('\n')
		trimmed := strings.TrimRight(line, "\r")

		// Blank line terminates the current event.
		if trimmed == "" {
			ev, ok := d.flushEvent()
			if ok {
				return ev, nil
			}
			continue
		}

		// Comment line — emit as keepalive immediately (these don't
		// participate in an event block).
		if strings.HasPrefix(trimmed, ":") {
			if d.curEvent != "" || len(d.dataLines) > 0 {
				discardLastRawLine(&d.rawBuf, line)
				continue
			}
			raw := append([]byte(nil), d.rawBuf.Bytes()...)
			d.rawBuf.Reset()
			return Event{
				Kind:     KindKeepalive,
				Shape:    conversation.StreamShapeAnthropicMessages,
				RawBytes: raw,
				Meta:     defaultMeta(),
			}, nil
		}

		if strings.HasPrefix(trimmed, "event:") {
			d.curEvent = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
			continue
		}
		if strings.HasPrefix(trimmed, "data:") {
			d.dataLines = append(d.dataLines, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
			continue
		}
		// Unknown line shape (id:, retry:, ...). Emit immediately as
		// keepalive so we don't lose bytes.
		if d.curEvent != "" || len(d.dataLines) > 0 {
			discardLastRawLine(&d.rawBuf, line)
			continue
		}
		raw := append([]byte(nil), d.rawBuf.Bytes()...)
		d.rawBuf.Reset()
		return Event{
			Kind:     KindKeepalive,
			Shape:    conversation.StreamShapeAnthropicMessages,
			RawBytes: raw,
			Meta:     defaultMeta(),
		}, nil
	}
	if err := d.r.Err(); err != nil {
		return Event{}, fmt.Errorf("anthropic decoder: scan: %w", err)
	}
	// Scanner drained. If we have a partial event buffered (no trailing
	// blank line), flush it. Upstreams sometimes terminate without the
	// final blank — the existing streaming_assistant_prepend writer
	// handles this in Close().
	if ev, ok := d.flushEvent(); ok {
		d.emittedEOF = true
		return ev, nil
	}
	d.emittedEOF = true
	return Event{}, io.EOF
}

// flushEvent emits one complete accumulated event and resets the
// accumulator. Returns false if no event was buffered (just a stray
// blank line or initial state).
func (d *AnthropicDecoder) flushEvent() (Event, bool) {
	if d.curEvent == "" && len(d.dataLines) == 0 {
		// Stray blank line. Just drop the buffered raw bytes — the
		// caller hasn't seen them as part of an event.
		d.rawBuf.Reset()
		return Event{}, false
	}

	raw := append([]byte(nil), d.rawBuf.Bytes()...)
	d.rawBuf.Reset()

	data := strings.Join(d.dataLines, "\n")
	eventName := d.curEvent
	d.curEvent = ""
	d.dataLines = d.dataLines[:0]

	ev := Event{
		Shape:    conversation.StreamShapeAnthropicMessages,
		RawBytes: raw,
		Kind:     classifyAnthropicEventKind(eventName),
		Meta: EventMeta{
			SSEEventName:       eventName,
			AnthropicIndex:     -1,
			OpenAIOutputIndex:  -1,
			OpenAIContentIndex: -1,
		},
	}

	// Pull the index out of content_block_* events so policies that
	// shift indices can target the right field via FieldPatch.
	switch ev.Kind {
	case KindBlockStart, KindBlockDelta, KindBlockEnd:
		var probe struct {
			Index *int `json:"index"`
		}
		if err := json.Unmarshal([]byte(data), &probe); err != nil {
			ev.Kind = KindUnknown
		} else if probe.Index != nil {
			ev.Meta.AnthropicIndex = *probe.Index
		}
	}

	return ev, true
}

func defaultMeta() EventMeta {
	return EventMeta{
		AnthropicIndex:     -1,
		OpenAIOutputIndex:  -1,
		OpenAIContentIndex: -1,
	}
}

// classifyAnthropicEventKind maps Anthropic SSE event names to the
// canonical EventKind vocabulary.
func classifyAnthropicEventKind(name string) EventKind {
	switch name {
	case "message_start":
		return KindResponseStart
	case "content_block_start":
		return KindBlockStart
	case "content_block_delta":
		return KindBlockDelta
	case "content_block_stop":
		return KindBlockEnd
	case "message_delta":
		return KindMessageEnd
	case "message_stop":
		return KindResponseEnd
	default:
		return KindUnknown
	}
}
