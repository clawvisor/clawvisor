package observability

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Metric attribute keys (contractual — see spec 01 §"Metric names").
const (
	AttrProvider     = "provider"
	AttrModel        = "model"
	AttrStreaming    = "streaming"
	AttrOutcome      = "outcome"
	AttrAuthMode     = "auth_mode"
	AttrDirection    = "direction"
	AttrPolicy       = "policy"
	AttrPhase        = "phase"
	AttrResolution   = "resolution"
	AttrService      = "service"
	AttrStatus       = "status"
	AttrDecision     = "decision"
	AttrHostCategory = "host_category"
)

// Span attribute keys (contractual — see spec 01 §"Span model"). Content-
// bearing keys are deliberately absent: attributes never carry prompt or
// completion text, tool arguments, header values, or secret material.
const (
	SpanAttrProvider       = "provider"
	SpanAttrModel          = "model"
	SpanAttrStreaming      = "streaming"
	SpanAttrAgentID        = "clawvisor.agent_id"
	SpanAttrConversationID = "clawvisor.conversation_id"
	SpanAttrSessionID      = "clawvisor.session_id"
	SpanAttrAuthMode       = "clawvisor.auth_mode"
	SpanAttrOutcome        = "clawvisor.outcome"
	SpanAttrHTTPStatus     = "http.status_code"
	SpanAttrUpstreamHost   = "upstream.host"
	SpanAttrHostCategory   = "host_category"
	SpanAttrDecision       = "decision"
	SpanAttrToolName       = "tool_name"
	SpanAttrVerdict        = "verdict"
	SpanAttrHeldKind       = "held_kind"
	SpanAttrReason         = "reason"
)

// Span names (contractual).
const (
	SpanProxyLiteRequest = "clawvisor.proxy_lite.request"
	SpanPipelinePre      = "clawvisor.pipeline.pre"
	SpanUpstreamForward  = "clawvisor.upstream.forward"
	SpanPipelinePost     = "clawvisor.pipeline.post"
	SpanGatewayRequest   = "clawvisor.gateway.request"
	SpanRuntimeProxyReq  = "clawvisor.runtimeproxy.request"
	EventPolicyVerdict   = "policy.verdict"
	EventToolUseVerdict  = "tooluse.verdict"
)

// Tracer returns the package tracer from the global provider. When
// observability is disabled the global provider is a no-op, so spans opened
// through this tracer cost nothing.
func Tracer() trace.Tracer {
	return otel.Tracer(TracerName)
}

// RecordLLMRequest increments clawvisor.llm.requests and records the
// clawvisor.llm.request.duration histogram. Nil-safe.
func (i *Instruments) RecordLLMRequest(ctx context.Context, provider, model string, streaming bool, outcome, authMode string, durationMs float64) {
	if i == nil {
		return
	}
	if i.LLMRequests != nil {
		i.LLMRequests.Add(ctx, 1, metric.WithAttributes(
			attribute.String(AttrProvider, provider),
			attribute.String(AttrModel, model),
			attribute.Bool(AttrStreaming, streaming),
			attribute.String(AttrOutcome, outcome),
			attribute.String(AttrAuthMode, authMode),
		))
	}
	if i.LLMRequestDur != nil {
		i.LLMRequestDur.Record(ctx, durationMs, metric.WithAttributes(
			attribute.String(AttrProvider, provider),
			attribute.String(AttrModel, model),
			attribute.String(AttrOutcome, outcome),
		))
	}
}

// RecordTokens increments clawvisor.llm.tokens once per non-zero direction.
// Nil-safe.
func (i *Instruments) RecordTokens(ctx context.Context, provider, model string, input, output, cacheRead, cacheWrite int64) {
	if i == nil || i.LLMTokens == nil {
		return
	}
	add := func(dir string, n int64) {
		if n <= 0 {
			return
		}
		i.LLMTokens.Add(ctx, n, metric.WithAttributes(
			attribute.String(AttrProvider, provider),
			attribute.String(AttrModel, model),
			attribute.String(AttrDirection, dir),
		))
	}
	add("input", input)
	add("output", output)
	add("cache_read", cacheRead)
	add("cache_write", cacheWrite)
}

// RecordCost increments clawvisor.llm.cost.usd_micros. Nil-safe; skips
// non-positive amounts.
func (i *Instruments) RecordCost(ctx context.Context, provider, model string, micros int64) {
	if i == nil || i.LLMCostUSDMicros == nil || micros <= 0 {
		return
	}
	i.LLMCostUSDMicros.Add(ctx, micros, metric.WithAttributes(
		attribute.String(AttrProvider, provider),
		attribute.String(AttrModel, model),
	))
}

// RecordVerdict increments clawvisor.pipeline.verdicts. outcome must already
// be mapped through DecisionFromOutcome by the caller. Nil-safe.
func (i *Instruments) RecordVerdict(ctx context.Context, policy, outcome, phase string) {
	if i == nil || i.PipelineVerdicts == nil {
		return
	}
	i.PipelineVerdicts.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrPolicy, policy),
		attribute.String(AttrOutcome, outcome),
		attribute.String(AttrPhase, phase),
	))
}

// RecordHold increments clawvisor.approvals.holds. resolution ∈
// {approved, denied, timeout, pending}. Nil-safe.
func (i *Instruments) RecordHold(ctx context.Context, resolution string) {
	if i == nil || i.ApprovalsHolds == nil {
		return
	}
	i.ApprovalsHolds.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrResolution, resolution),
	))
}

// RecordGatewayRequest increments clawvisor.gateway.requests. service must be
// alias-stripped; status is a 2xx/4xx/5xx bucket. Nil-safe.
func (i *Instruments) RecordGatewayRequest(ctx context.Context, service, statusBucket string) {
	if i == nil || i.GatewayRequests == nil {
		return
	}
	i.GatewayRequests.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrService, service),
		attribute.String(AttrStatus, statusBucket),
	))
}

// RecordRuntimeProxyRequest increments clawvisor.runtimeproxy.requests.
// hostCategory ∈ {llm, other} — never a raw hostname. Nil-safe.
func (i *Instruments) RecordRuntimeProxyRequest(ctx context.Context, decision, hostCategory string) {
	if i == nil || i.RuntimeProxyReqs == nil {
		return
	}
	i.RuntimeProxyReqs.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrDecision, decision),
		attribute.String(AttrHostCategory, hostCategory),
	))
}

// StatusBucket maps an HTTP status code to a "2xx"/"4xx"/"5xx" bucket for
// low-cardinality metric attributes.
func StatusBucket(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500:
		return "5xx"
	default:
		return "other"
	}
}

// instrumentsCtxKey carries the per-request Instruments through context so
// code deep in the pipeline (which has no handler reference) can emit metrics
// without threading the instrument set through every signature.
type instrumentsCtxKey struct{}

// ContextWithInstruments attaches inst to ctx.
func ContextWithInstruments(ctx context.Context, inst *Instruments) context.Context {
	if inst == nil {
		return ctx
	}
	return context.WithValue(ctx, instrumentsCtxKey{}, inst)
}

// InstrumentsFromContext returns the Instruments attached to ctx, or nil.
func InstrumentsFromContext(ctx context.Context) *Instruments {
	inst, _ := ctx.Value(instrumentsCtxKey{}).(*Instruments)
	return inst
}

// RecordPolicyVerdict emits both the clawvisor.pipeline.verdicts metric (via
// the Instruments in ctx) and a policy.verdict span event on the active span
// in ctx. reason is attached only when outcome is not "allow" (it is a
// policy-generated string, never model content). Safe to call when neither
// instruments nor a recording span are present.
func RecordPolicyVerdict(ctx context.Context, policy, outcome, phase, reason string) {
	InstrumentsFromContext(ctx).RecordVerdict(ctx, policy, outcome, phase)
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		attrs := []attribute.KeyValue{
			attribute.String(AttrPolicy, policy),
			attribute.String(AttrOutcome, outcome),
			attribute.String(AttrPhase, phase),
		}
		if outcome != "allow" && reason != "" {
			attrs = append(attrs, attribute.String(SpanAttrReason, reason))
		}
		span.AddEvent(EventPolicyVerdict, trace.WithAttributes(attrs...))
	}
}
