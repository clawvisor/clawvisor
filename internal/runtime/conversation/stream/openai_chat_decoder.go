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

// OpenAIChatDecoder parses OpenAI Chat Completions SSE into the
// canonical Event stream. The wire format is simpler than Anthropic:
// each event is a single `data: <json>` line with no `event:` prefix,
// terminated by either a blank line or the next `data:` line. The
// terminal sentinel is the literal `data: [DONE]`.
//
// Each event's RawBytes equals the exact upstream bytes (the `data:`
// line plus its terminator). The round-trip property: decoding and
// re-encoding without mutation produces byte-identical output.
type OpenAIChatDecoder struct {
	r          *bufio.Scanner
	rawBuf     bytes.Buffer
	dataLines  []string
	emittedEOF bool
}

// NewOpenAIChatDecoder wraps r.
func NewOpenAIChatDecoder(r io.Reader) *OpenAIChatDecoder {
	s := bufio.NewScanner(r)
	s.Split(scanSSELines)
	s.Buffer(make([]byte, 0, 4096), maxSSELineSize)
	return &OpenAIChatDecoder{r: s}
}

func (d *OpenAIChatDecoder) Next() (Event, error) {
	if d.emittedEOF {
		return Event{}, io.EOF
	}
	for d.r.Scan() {
		rawLine := d.r.Text()
		line := strings.TrimSuffix(rawLine, "\n")
		d.rawBuf.WriteString(rawLine)

		trimmed := strings.TrimRight(line, "\r")

		// Blank line terminates the current event.
		if trimmed == "" {
			ev, ok := d.flushEvent()
			if ok {
				return ev, nil
			}
			continue
		}

		// SSE comment — emit immediately as keepalive.
		if strings.HasPrefix(trimmed, ":") {
			if len(d.dataLines) > 0 {
				discardLastRawLine(&d.rawBuf, line)
				continue
			}
			raw := append([]byte(nil), d.rawBuf.Bytes()...)
			d.rawBuf.Reset()
			return Event{
				Kind:     KindKeepalive,
				Shape:    conversation.StreamShapeOpenAIChat,
				RawBytes: raw,
				Meta:     defaultMeta(),
			}, nil
		}

		if strings.HasPrefix(trimmed, "data:") {
			dataValue := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			// OpenAI Chat sometimes elides the blank-line terminator
			// between chunks. Each `data:` line therefore implicitly
			// closes the previous event when it looks like a new chat
			// chunk. Otherwise, keep standard SSE multi-line data
			// semantics and join the data lines with "\n".
			if len(d.dataLines) > 0 && openAIChatDataComplete(strings.Join(d.dataLines, "\n")) && startsOpenAIChatEvent(dataValue) {
				ev, _ := d.flushDataLinesPreserveCurrent(rawLine, line)
				return ev, nil
			}
			d.dataLines = append(d.dataLines, dataValue)
			continue
		}

		// Unknown line shape — emit raw as keepalive to avoid byte loss.
		if len(d.dataLines) > 0 {
			discardLastRawLine(&d.rawBuf, line)
			continue
		}
		raw := append([]byte(nil), d.rawBuf.Bytes()...)
		d.rawBuf.Reset()
		return Event{
			Kind:     KindKeepalive,
			Shape:    conversation.StreamShapeOpenAIChat,
			RawBytes: raw,
			Meta:     defaultMeta(),
		}, nil
	}
	if err := d.r.Err(); err != nil {
		return Event{}, fmt.Errorf("openai chat decoder: scan: %w", err)
	}
	// Stream drained: flush any buffered event.
	if ev, ok := d.flushEvent(); ok {
		d.emittedEOF = true
		return ev, nil
	}
	d.emittedEOF = true
	return Event{}, io.EOF
}

// flushEvent emits one complete buffered event.
func (d *OpenAIChatDecoder) flushEvent() (Event, bool) {
	if len(d.dataLines) == 0 {
		d.rawBuf.Reset()
		return Event{}, false
	}
	raw := append([]byte(nil), d.rawBuf.Bytes()...)
	d.rawBuf.Reset()
	data := strings.Join(d.dataLines, "\n")
	d.dataLines = d.dataLines[:0]

	kind := classifyOpenAIChatEventKind(data)
	return Event{
		Kind:     kind,
		Shape:    conversation.StreamShapeOpenAIChat,
		RawBytes: raw,
		Meta:     EventMeta{AnthropicIndex: -1, OpenAIOutputIndex: -1, OpenAIContentIndex: -1},
	}, true
}

// flushDataLinesPreserveCurrent emits the events buffered so far,
// retaining the current line as the start of the *next* event in the
// rawBuf. This handles the OpenAI Chat case where `data:` lines aren't
// separated by blank lines.
func (d *OpenAIChatDecoder) flushDataLinesPreserveCurrent(currentRawLine, currentLine string) (Event, bool) {
	// Identify how many bytes the current line contributed to rawBuf
	// (including its newline, if present). Everything before that belongs to the
	// completed event; the current line starts the next event's rawBuf.
	rawAll := d.rawBuf.String()
	completed := strings.TrimSuffix(rawAll, currentRawLine)

	d.rawBuf.Reset()
	d.rawBuf.WriteString(currentRawLine)

	data := strings.Join(d.dataLines, "\n")
	d.dataLines = d.dataLines[:0]
	d.dataLines = append(d.dataLines, strings.TrimSpace(strings.TrimPrefix(strings.TrimRight(currentLine, "\r"), "data:")))

	return Event{
		Kind:     classifyOpenAIChatEventKind(data),
		Shape:    conversation.StreamShapeOpenAIChat,
		RawBytes: []byte(completed),
		Meta:     EventMeta{AnthropicIndex: -1, OpenAIOutputIndex: -1, OpenAIContentIndex: -1},
	}, true
}

func startsOpenAIChatEvent(data string) bool {
	return data == "[DONE]" || strings.HasPrefix(data, "{")
}

func openAIChatDataComplete(data string) bool {
	if data == "[DONE]" {
		return true
	}
	return json.Valid([]byte(data))
}

func ensureSSETerminator(raw []byte) []byte {
	if len(raw) == 0 || bytes.HasSuffix(raw, []byte("\n\n")) || bytes.HasSuffix(raw, []byte("\r\n\r\n")) {
		return raw
	}
	out := append([]byte(nil), raw...)
	switch {
	case bytes.HasSuffix(out, []byte("\r\n")):
		return append(out, '\r', '\n')
	case bytes.HasSuffix(out, []byte("\n")):
		return append(out, '\n')
	default:
		return append(out, '\n', '\n')
	}
}

// classifyOpenAIChatEventKind inspects the data payload to determine
// what kind of event this is. OpenAI Chat events don't have explicit
// kinds; we infer from the payload shape.
func classifyOpenAIChatEventKind(data string) EventKind {
	if data == "[DONE]" {
		return KindResponseEnd
	}
	// Most chunks are BlockDelta-equivalent (carry a choices[].delta).
	// Without parsing JSON we can't precisely distinguish start/delta/end;
	// for round-trip purposes the chunks are interchangeable: encoder
	// emits RawBytes verbatim regardless.
	return KindBlockDelta
}
