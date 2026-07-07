package observability

import (
	"errors"

	"go.opentelemetry.io/otel/metric"
)

// Instrument names (exact, contractual). All prefixed "clawvisor.".
const (
	MetricLLMRequests      = "clawvisor.llm.requests"
	MetricLLMRequestDur    = "clawvisor.llm.request.duration"
	MetricLLMTokens        = "clawvisor.llm.tokens"
	MetricLLMCostUSDMicros = "clawvisor.llm.cost.usd_micros"
	MetricPipelineVerdicts = "clawvisor.pipeline.verdicts"
	MetricApprovalsHolds   = "clawvisor.approvals.holds"
	MetricGatewayRequests  = "clawvisor.gateway.requests"
	MetricRuntimeProxyReqs = "clawvisor.runtimeproxy.requests"
)

// TracerName is the instrumentation scope name for spans this package's
// callers open.
const TracerName = "github.com/clawvisor/clawvisor/internal/observability"

// Instruments holds every metric instrument, constructed once via
// NewInstruments and threaded into handlers through their option structs.
//
// A nil *Instruments is safe: all Record* helpers no-op on nil, so callers can
// hold a nil pointer when observability is disabled without nil-checking at
// each call site.
type Instruments struct {
	LLMRequests      metric.Int64Counter
	LLMRequestDur    metric.Float64Histogram
	LLMTokens        metric.Int64Counter
	LLMCostUSDMicros metric.Int64Counter
	PipelineVerdicts metric.Int64Counter
	ApprovalsHolds   metric.Int64Counter
	GatewayRequests  metric.Int64Counter
	RuntimeProxyReqs metric.Int64Counter
}

// NewInstruments builds the instrument set from meter. The meter is typically
// otel.GetMeterProvider().Meter(...) — when observability is disabled that is
// a no-op meter, so instrument creation never fails in practice, but any
// construction error is returned for the caller to surface.
func NewInstruments(meter metric.Meter) (*Instruments, error) {
	var errs []error
	counter := func(name, desc string) metric.Int64Counter {
		c, err := meter.Int64Counter(name, metric.WithDescription(desc))
		if err != nil {
			errs = append(errs, err)
		}
		return c
	}

	inst := &Instruments{
		LLMRequests:      counter(MetricLLMRequests, "Count of LLM requests through the proxy-lite pipeline."),
		LLMTokens:        counter(MetricLLMTokens, "Token counts by direction."),
		LLMCostUSDMicros: counter(MetricLLMCostUSDMicros, "LLM cost in USD micro-dollars."),
		PipelineVerdicts: counter(MetricPipelineVerdicts, "Per-policy verdict counts."),
		ApprovalsHolds:   counter(MetricApprovalsHolds, "Approval hold create/resolve counts."),
		GatewayRequests:  counter(MetricGatewayRequests, "Skill-gateway request counts."),
		RuntimeProxyReqs: counter(MetricRuntimeProxyReqs, "Runtime (network) proxy request counts."),
	}

	dur, err := meter.Float64Histogram(MetricLLMRequestDur,
		metric.WithDescription("LLM request duration."),
		metric.WithUnit("ms"))
	if err != nil {
		errs = append(errs, err)
	}
	inst.LLMRequestDur = dur

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return inst, nil
}
