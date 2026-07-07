package pipeline_test

import (
	"context"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/clawvisor/clawvisor/internal/observability"
	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pipeline"
)

// TestRunPreEmitsVerdictMetricViaDecisionFromOutcome proves that RunPre emits
// clawvisor.pipeline.verdicts with the outcome attribute mapped through
// DecisionFromOutcome for every outcome the pre-chain can produce
// (allow/deny/skip/short_circuit). Hold and Rewrite are not valid pre
// outcomes (RunPre rejects them), so their DecisionFromOutcome mapping is
// asserted directly below.
func TestRunPreEmitsVerdictMetricViaDecisionFromOutcome(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	inst, err := observability.NewInstruments(mp.Meter("test"))
	if err != nil {
		t.Fatalf("NewInstruments: %v", err)
	}
	ctx := observability.ContextWithInstruments(context.Background(), inst)

	req := &orchTestRequest{provider: conversation.ProviderAnthropic, body: []byte("{}")}
	// allow → allow ; skip → block ; deny → block ; short_circuit → block.
	// Run each in isolation because deny/short_circuit halt the chain.
	if _, err := pipeline.RunPre(ctx, req, []pipeline.RequestPolicy{&allowingPolicy{name: "allow_pol", field: "k", value: "v"}}); err != nil {
		t.Fatalf("RunPre allow: %v", err)
	}
	if _, err := pipeline.RunPre(ctx, req, []pipeline.RequestPolicy{&skippingPolicy{name: "skip_pol"}}); err != nil {
		t.Fatalf("RunPre skip: %v", err)
	}
	if _, err := pipeline.RunPre(ctx, req, []pipeline.RequestPolicy{&denyingPolicy{name: "deny_pol"}}); err != nil {
		t.Fatalf("RunPre deny: %v", err)
	}
	if _, err := pipeline.RunPre(ctx, req, []pipeline.RequestPolicy{&shortCircuitingPolicy{name: "sc_pol", body: []byte("{}")}}); err != nil {
		t.Fatalf("RunPre short_circuit: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	// Sum the verdict counter grouped by (policy, outcome).
	got := map[[2]string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != observability.MetricPipelineVerdicts {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("pipeline.verdicts not an int64 sum: %T", m.Data)
			}
			for _, dp := range sum.DataPoints {
				policy, _ := dp.Attributes.Value("policy")
				outcome, _ := dp.Attributes.Value("outcome")
				phase, _ := dp.Attributes.Value("phase")
				if phase.AsString() != "pre" {
					t.Errorf("verdict phase=%q, want pre", phase.AsString())
				}
				got[[2]string{policy.AsString(), outcome.AsString()}] += dp.Value
			}
		}
	}

	want := map[[2]string]int64{
		{"allow_pol", "allow"}: 1,
		{"skip_pol", "block"}:  1, // OutcomeSkip → DecisionFromOutcome → block
		{"deny_pol", "block"}:  1, // OutcomeDeny → block
		{"sc_pol", "block"}:    1, // OutcomeShortCircuit → block
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("verdict{policy=%s,outcome=%s}=%d, want %d (all: %v)", k[0], k[1], got[k], v, got)
		}
	}
}

// skippingPolicy returns OutcomeSkip (chain continues; maps to block).
type skippingPolicy struct{ name string }

func (p *skippingPolicy) Name() string { return p.name }
func (p *skippingPolicy) Preprocess(_ context.Context, _ pipeline.ReadOnlyRequest, _ pipeline.RequestMutator) (pipeline.RequestVerdict, error) {
	return pipeline.RequestVerdict{Outcome: pipeline.OutcomeSkip}, nil
}

// TestDecisionFromOutcomeCoversAllSixOutcomes asserts the outcome-attribute
// mapping for all six Outcome values, including Hold and Rewrite which are
// tool_use/post outcomes rather than pre outcomes.
func TestDecisionFromOutcomeCoversAllSixOutcomes(t *testing.T) {
	cases := map[conversation.Outcome]string{
		pipeline.OutcomeAllow:        "allow",
		pipeline.OutcomeRewrite:      "rewrite",
		pipeline.OutcomeDeny:         "block",
		pipeline.OutcomeHold:         "block",
		pipeline.OutcomeShortCircuit: "block",
		pipeline.OutcomeSkip:         "block",
	}
	for outcome, want := range cases {
		if got := string(pipeline.DecisionFromOutcome(outcome)); got != want {
			t.Errorf("DecisionFromOutcome(%s)=%q, want %q", outcome, got, want)
		}
	}
}
