// Package postproc owns the response-side coordination that
// llmproxy's tool_use evaluator chain runs inside: response parsing,
// rewriter invocation, hold buffering, and streaming SSE injection.
// The handler calls postproc.Postprocess (buffered) or
// postproc.PostprocessStream (SSE) once per upstream response.
//
// Response-level finalization (coalesce-vs-replay decision, hold
// replay, audit flushing, coalesced prompt rendering) lives in
// pipeline.Finalizer plus the llmproxy finalizer adapter. Postproc
// keeps the per-call buffering wrappers (capturedHoldSink +
// pendingAuditEventBuffer) that feed the finalizer.
package postproc

import (
	"context"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
)

// capturedHoldSink buffers PendingApprovalCache.Hold calls the first
// eval pass makes. The wrapper does NOT touch the underlying cache
// during pass 1 — it generates a stable ID, stores the Pending in the
// buffer, and returns. After eval the pipeline.Finalizer reads the
// buffer and decides between coalesce and per-tool replay.
type capturedHoldSink struct {
	holds []capturedHold
}

type capturedHold struct {
	Pending llmproxy.PendingLiteApproval
}

// holdCapturingApprovalCache wraps a PendingApprovalCache so pass-1
// Hold calls are buffered, not committed. Peek/Resolve/Drop fall
// through to the inner cache.
type holdCapturingApprovalCache struct {
	inner llmproxy.PendingApprovalCache
	sink  *capturedHoldSink
}

func newHoldCapturingApprovalCache(inner llmproxy.PendingApprovalCache, sink *capturedHoldSink) *holdCapturingApprovalCache {
	return &holdCapturingApprovalCache{
		inner: inner,
		sink:  sink,
	}
}

func (c *holdCapturingApprovalCache) Hold(_ context.Context, pending llmproxy.PendingLiteApproval) (llmproxy.HoldResult, error) {
	if pending.ID == "" {
		id, err := llmproxy.NewLiteApprovalID()
		if err != nil {
			return llmproxy.HoldResult{}, err
		}
		pending.ID = id
	}
	if c.sink != nil {
		c.sink.holds = append(c.sink.holds, capturedHold{Pending: pending})
	}
	return llmproxy.HoldResult{Pending: pending}, nil
}

func (c *holdCapturingApprovalCache) Peek(ctx context.Context, req llmproxy.ResolveRequest) (*llmproxy.PendingLiteApproval, error) {
	return c.inner.Peek(ctx, req)
}

func (c *holdCapturingApprovalCache) Resolve(ctx context.Context, req llmproxy.ResolveRequest) (*llmproxy.PendingLiteApproval, error) {
	return c.inner.Resolve(ctx, req)
}

func (c *holdCapturingApprovalCache) Drop(ctx context.Context, req llmproxy.ResolveRequest) error {
	return c.inner.Drop(ctx, req)
}

// pendingAuditEventBuffer buffers audit rows from pass 1. The
// pipeline.Finalizer reads this and either flushes verbatim on the
// per-tool replay path or discards the contents in favor of per-tool
// coalesced rows.
type pendingAuditEventBuffer struct {
	entries []conversation.AuditEvent
}
