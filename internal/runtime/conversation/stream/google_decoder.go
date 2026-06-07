package stream

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

// GoogleDecoder handles Gemini's :streamGenerateContent SSE framing,
// where each event is a JSON object on a single `data:` line, similar
// to OpenAI Chat Completions but with a different envelope shape
// (candidates[].content.parts[] instead of choices[].delta).
//
// This partial codec uses the same SSE framing as OpenAIChatDecoder;
// the difference is in what the decoded events mean and how they're
// classified into EventKind. Until full Gemini codec work lands, all
// Gemini events classify as KindBlockDelta.
type GoogleDecoder struct {
	r          *bufio.Scanner
	rawBuf     bytes.Buffer
	dataLines  []string
	emittedEOF bool
}

func NewGoogleDecoder(r io.Reader) *GoogleDecoder {
	s := bufio.NewScanner(r)
	const maxLineSize = 1 << 20
	s.Buffer(make([]byte, 0, 4096), maxLineSize)
	return &GoogleDecoder{r: s}
}

func (d *GoogleDecoder) Next() (Event, error) {
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
			if len(d.dataLines) > 0 {
				continue
			}
			raw := append([]byte(nil), d.rawBuf.Bytes()...)
			d.rawBuf.Reset()
			return Event{
				Kind:     KindKeepalive,
				Shape:    conversation.StreamShapeGoogleGemini,
				RawBytes: raw,
				Meta:     defaultMeta(),
			}, nil
		}

		if strings.HasPrefix(trimmed, "data:") {
			d.dataLines = append(d.dataLines, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
			continue
		}

		raw := append([]byte(nil), d.rawBuf.Bytes()...)
		d.rawBuf.Reset()
		return Event{
			Kind:     KindKeepalive,
			Shape:    conversation.StreamShapeGoogleGemini,
			RawBytes: raw,
			Meta:     defaultMeta(),
		}, nil
	}
	if err := d.r.Err(); err != nil {
		return Event{}, fmt.Errorf("google decoder: scan: %w", err)
	}
	if ev, ok := d.flushEvent(); ok {
		d.emittedEOF = true
		return ev, nil
	}
	d.emittedEOF = true
	return Event{}, io.EOF
}

func (d *GoogleDecoder) flushEvent() (Event, bool) {
	if len(d.dataLines) == 0 {
		d.rawBuf.Reset()
		return Event{}, false
	}
	raw := append([]byte(nil), d.rawBuf.Bytes()...)
	d.rawBuf.Reset()
	d.dataLines = d.dataLines[:0]
	// All Gemini data events classify as BlockDelta in this stub —
	// Gemini doesn't have explicit start/stop event markers within
	// the stream (each chunk is a self-contained candidate update).
	return Event{
		Kind:     KindBlockDelta,
		Shape:    conversation.StreamShapeGoogleGemini,
		RawBytes: raw,
		Meta:     defaultMeta(),
	}, true
}
