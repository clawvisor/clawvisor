package stream

import (
	"bufio"
	"bytes"
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
	const maxLineSize = 1 << 20
	s.Buffer(make([]byte, 0, 4096), maxLineSize)
	return &OpenAIChatDecoder{r: s}
}

func (d *OpenAIChatDecoder) Next() (Event, error) {
	if d.emittedEOF {
		return Event{}, io.EOF
	}
	for d.r.Scan() {
		line := d.r.Text()
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

		// SSE comment — emit immediately as keepalive.
		if strings.HasPrefix(trimmed, ":") {
			if len(d.dataLines) > 0 {
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
			// OpenAI Chat sometimes elides the blank-line terminator
			// between chunks. Each `data:` line therefore implicitly
			// closes the previous event.
			if len(d.dataLines) > 0 {
				ev, _ := d.flushDataLinesPreserveCurrent(line)
				return ev, nil
			}
			d.dataLines = append(d.dataLines, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
			continue
		}

		// Unknown line shape — emit raw as keepalive to avoid byte loss.
		if len(d.dataLines) > 0 {
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
func (d *OpenAIChatDecoder) flushDataLinesPreserveCurrent(currentLine string) (Event, bool) {
	// Identify how many bytes the current line contributed to rawBuf
	// (currentLine + '\n'). Everything before that belongs to the
	// completed event; the current line starts the next event's rawBuf.
	rawAll := d.rawBuf.String()
	currentBytes := currentLine + "\n"
	completed := strings.TrimSuffix(rawAll, currentBytes)

	d.rawBuf.Reset()
	d.rawBuf.WriteString(currentBytes)

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
