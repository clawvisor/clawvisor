package llmproxy

import (
	"io"
	"log/slog"
	"sync"
	"time"
)

// ProgressReader wraps an io.Reader and emits slog events at first
// byte, every progressInterval / progressBytes, and on final
// EOF/error. Used to diagnose stalls in upstream SSE streams — when
// a request goes from "headers received" to "no bytes for 30s" the
// log will say so, and we can correlate with the client-side
// timeout that abandoned the stream.
//
// Strictly observational. Reads pass through unchanged.
type ProgressReader struct {
	mu sync.Mutex

	r         io.Reader
	logger    *slog.Logger
	requestID string
	rawLog    *RawIOLogger
	// Threshold settings. Defaults are populated by NewProgressReader
	// if zero. Whichever fires first emits a progress event.
	tickInterval time.Duration
	tickBytes    int64

	start     time.Time
	firstByte time.Time
	lastTick  time.Time
	lastBytes int64
	total     int64
	done      bool
}

// NewProgressReader wires r with periodic progress reporting. logger
// may be nil. rawLog may be nil. requestID is the audit correlation
// id propagated into every emit. Sensible defaults: emit every 2s
// or every 64 KiB of body, whichever first.
func NewProgressReader(r io.Reader, logger *slog.Logger, rawLog *RawIOLogger, requestID string) *ProgressReader {
	now := time.Now()
	return &ProgressReader{
		r:            r,
		logger:       logger,
		requestID:    requestID,
		rawLog:       rawLog,
		tickInterval: 2 * time.Second,
		tickBytes:    64 << 10,
		start:        now,
		lastTick:     now,
	}
}

// Read forwards the read and emits diagnostics. Always returns n, err
// faithfully — instrumentation can never break the data path.
func (p *ProgressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	if n > 0 {
		if p.firstByte.IsZero() {
			p.firstByte = now
			p.emit("first_byte", n, nil, now)
		}
		p.total += int64(n)
		if now.Sub(p.lastTick) >= p.tickInterval ||
			p.total-p.lastBytes >= p.tickBytes {
			p.emit("progress", n, nil, now)
			p.lastTick = now
			p.lastBytes = p.total
		}
	}
	if err != nil && !p.done {
		p.done = true
		event := "eof"
		if err != io.EOF {
			event = "error"
		}
		p.emit(event, n, err, now)
	}
	return n, err
}

func (p *ProgressReader) emit(event string, n int, err error, now time.Time) {
	elapsed := now.Sub(p.start)
	var ttfb time.Duration
	if !p.firstByte.IsZero() {
		ttfb = p.firstByte.Sub(p.start)
	}
	kv := []any{
		"event", "lite_proxy.upstream_progress.body_" + event,
		"request_id", p.requestID,
		"bytes_total", p.total,
		"chunk_bytes", n,
		"elapsed_ms", elapsed.Milliseconds(),
		"ttfb_ms", ttfb.Milliseconds(),
	}
	if err != nil {
		kv = append(kv, "err", err.Error())
	}
	if p.logger != nil {
		p.logger.Info("lite-proxy upstream body "+event, kv...)
	}
	if p.rawLog != nil {
		fields := map[string]any{
			"phase":       "upstream_progress",
			"request_id":  p.requestID,
			"marker":      event,
			"bytes_total": p.total,
			"chunk_bytes": n,
			"elapsed_ms":  elapsed.Milliseconds(),
			"ttfb_ms":     ttfb.Milliseconds(),
		}
		if err != nil {
			fields["err"] = err.Error()
		}
		p.rawLog.EmitRaw(fields)
	}
}

// Stats returns a snapshot of the progress counters. Useful at
// handler-exit time to log a single summary line without re-emitting
// all the progress events.
func (p *ProgressReader) Stats() (bytesTotal int64, elapsed, ttfb time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	bytesTotal = p.total
	elapsed = time.Since(p.start)
	if !p.firstByte.IsZero() {
		ttfb = p.firstByte.Sub(p.start)
	}
	return
}
