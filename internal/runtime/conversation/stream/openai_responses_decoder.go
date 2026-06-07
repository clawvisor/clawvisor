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

// OpenAIResponsesDecoder parses OpenAI Responses API SSE into canonical
// Events. The format uses named events (`response.created`,
// `response.output_item.added`, `response.output_text.delta`,
// `response.content_part.done`, `response.output_item.done`,
// `response.completed`, etc.) framed identically to Anthropic's
// `event:`/`data:`/blank-line shape.
//
// Each event carries `output_index` and/or `content_index` fields that
// shift when a leading item is injected; the decoder surfaces these
// on EventMeta so policies can patch them via FieldPatch without
// re-parsing.
type OpenAIResponsesDecoder struct {
	r          *bufio.Scanner
	rawBuf     bytes.Buffer
	curEvent   string
	dataLines  []string
	emittedEOF bool
}

func NewOpenAIResponsesDecoder(r io.Reader) *OpenAIResponsesDecoder {
	s := bufio.NewScanner(r)
	s.Split(scanSSELines)
	s.Buffer(make([]byte, 0, 4096), maxSSELineSize)
	return &OpenAIResponsesDecoder{r: s}
}

func (d *OpenAIResponsesDecoder) Next() (Event, error) {
	if d.emittedEOF {
		return Event{}, io.EOF
	}
	for d.r.Scan() {
		line := d.r.Text()
		d.rawBuf.WriteString(line)
		d.rawBuf.WriteByte('\n')

		trimmed := strings.TrimRight(line, "\r")

		if trimmed == "" {
			ev, ok := d.flushEvent()
			if ok {
				return ev, nil
			}
			continue
		}

		if strings.HasPrefix(trimmed, ":") {
			if d.curEvent != "" || len(d.dataLines) > 0 {
				discardLastRawLine(&d.rawBuf, line)
				continue
			}
			raw := append([]byte(nil), d.rawBuf.Bytes()...)
			d.rawBuf.Reset()
			return Event{
				Kind:     KindKeepalive,
				Shape:    conversation.StreamShapeOpenAIResponses,
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

		if d.curEvent != "" || len(d.dataLines) > 0 {
			discardLastRawLine(&d.rawBuf, line)
			continue
		}
		raw := append([]byte(nil), d.rawBuf.Bytes()...)
		d.rawBuf.Reset()
		return Event{
			Kind:     KindKeepalive,
			Shape:    conversation.StreamShapeOpenAIResponses,
			RawBytes: raw,
			Meta:     defaultMeta(),
		}, nil
	}
	if err := d.r.Err(); err != nil {
		return Event{}, fmt.Errorf("openai responses decoder: scan: %w", err)
	}
	if ev, ok := d.flushEvent(); ok {
		d.emittedEOF = true
		return ev, nil
	}
	d.emittedEOF = true
	return Event{}, io.EOF
}

func (d *OpenAIResponsesDecoder) flushEvent() (Event, bool) {
	if d.curEvent == "" && len(d.dataLines) == 0 {
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
		Kind:     classifyOpenAIResponsesEventKind(eventName),
		Shape:    conversation.StreamShapeOpenAIResponses,
		RawBytes: raw,
		Meta: EventMeta{
			SSEEventName:       eventName,
			AnthropicIndex:     -1,
			OpenAIOutputIndex:  -1,
			OpenAIContentIndex: -1,
		},
	}

	// Probe for output_index / content_index / item_id so policies
	// can shift them via FieldPatch.
	var probe struct {
		OutputIndex  *int    `json:"output_index"`
		ContentIndex *int    `json:"content_index"`
		ItemID       *string `json:"item_id"`
	}
	if err := json.Unmarshal([]byte(data), &probe); err != nil {
		ev.Kind = KindUnknown
	} else {
		if probe.OutputIndex != nil {
			ev.Meta.OpenAIOutputIndex = *probe.OutputIndex
		}
		if probe.ContentIndex != nil {
			ev.Meta.OpenAIContentIndex = *probe.ContentIndex
		}
		if probe.ItemID != nil {
			ev.Meta.OpenAIItemID = *probe.ItemID
		}
	}

	return ev, true
}

// classifyOpenAIResponsesEventKind maps the OpenAI Responses event
// name vocabulary onto the canonical EventKind.
func classifyOpenAIResponsesEventKind(name string) EventKind {
	switch name {
	case "response.created":
		return KindResponseStart
	case "response.output_item.added":
		return KindMessageStart
	case "response.content_part.added":
		return KindBlockStart
	case "response.output_text.delta":
		return KindBlockDelta
	case "response.output_text.done", "response.content_part.done":
		return KindBlockEnd
	case "response.output_item.done":
		return KindMessageEnd
	case "response.completed":
		return KindResponseEnd
	default:
		return KindUnknown
	}
}
