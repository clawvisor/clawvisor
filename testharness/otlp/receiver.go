// Package otlp provides a tiny in-process OTLP/HTTP receiver for the
// deterministic e2e lane. The clawvisor-server subprocess exports its OTel
// traces and metrics here (protocol: http, insecure) so tests can assert on
// the exported span tree and metric counters without a real collector.
//
// It is intentionally minimal: it accepts protobuf ExportTraceServiceRequest
// and ExportMetricsServiceRequest bodies on /v1/traces and /v1/metrics,
// flattens them, and exposes query helpers. No gRPC, no persistence.
package otlp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// Receiver is an in-process OTLP/HTTP collector for tests.
type Receiver struct {
	srv *httptest.Server

	mu      sync.Mutex
	spans   []*tracepb.Span
	metrics []*metricpb.Metric
	// resourceAttrs accumulates the Resource attributes off every received
	// ResourceSpans/ResourceMetrics envelope. The receiver otherwise flattens
	// straight to spans/metrics and would drop these, leaving the leak
	// sentinel (AllStringAttrValues) blind to anything carried on resource
	// attributes (service.name, service.version, and any future additions).
	resourceAttrs []*commonpb.KeyValue
}

// NewReceiver starts the receiver and registers cleanup on t.
func NewReceiver(t *testing.T) *Receiver {
	t.Helper()
	r := &Receiver{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/traces", r.handleTraces)
	mux.HandleFunc("/v1/metrics", r.handleMetrics)
	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)
	return r
}

// Endpoint returns the host:port (no scheme, no path) to pass to the OTLP
// exporter's endpoint config with protocol=http and insecure=true.
func (r *Receiver) Endpoint() string {
	return strings.TrimPrefix(r.srv.URL, "http://")
}

func (r *Receiver) handleTraces(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var msg collectortracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(body, &msg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	for _, rs := range msg.GetResourceSpans() {
		r.resourceAttrs = append(r.resourceAttrs, rs.GetResource().GetAttributes()...)
		for _, ss := range rs.GetScopeSpans() {
			r.spans = append(r.spans, ss.GetSpans()...)
		}
	}
	r.mu.Unlock()
	writeProto(w, &collectortracepb.ExportTraceServiceResponse{})
}

func (r *Receiver) handleMetrics(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var msg collectormetricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(body, &msg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	for _, rm := range msg.GetResourceMetrics() {
		r.resourceAttrs = append(r.resourceAttrs, rm.GetResource().GetAttributes()...)
		for _, sm := range rm.GetScopeMetrics() {
			r.metrics = append(r.metrics, sm.GetMetrics()...)
		}
	}
	r.mu.Unlock()
	writeProto(w, &collectormetricspb.ExportMetricsServiceResponse{})
}

func writeProto(w http.ResponseWriter, m proto.Message) {
	b, err := proto.Marshal(m)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(b)
}

// Spans returns a snapshot of all received spans.
func (r *Receiver) Spans() []*tracepb.Span {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*tracepb.Span, len(r.spans))
	copy(out, r.spans)
	return out
}

// Metrics returns a snapshot of all received metrics.
func (r *Receiver) Metrics() []*metricpb.Metric {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*metricpb.Metric, len(r.metrics))
	copy(out, r.metrics)
	return out
}

// ResourceAttrs returns a snapshot of the Resource attributes seen across all
// received ResourceSpans/ResourceMetrics envelopes.
func (r *Receiver) ResourceAttrs() []*commonpb.KeyValue {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*commonpb.KeyValue, len(r.resourceAttrs))
	copy(out, r.resourceAttrs)
	return out
}

// SpanByName returns the first received span with the given name, or nil.
func (r *Receiver) SpanByName(name string) *tracepb.Span {
	for _, s := range r.Spans() {
		if s.GetName() == name {
			return s
		}
	}
	return nil
}

// SpansByName returns all received spans with the given name.
func (r *Receiver) SpansByName(name string) []*tracepb.Span {
	var out []*tracepb.Span
	for _, s := range r.Spans() {
		if s.GetName() == name {
			out = append(out, s)
		}
	}
	return out
}

// MetricByName returns the metric with the given name, or nil.
func (r *Receiver) MetricByName(name string) *metricpb.Metric {
	for _, m := range r.Metrics() {
		if m.GetName() == name {
			return m
		}
	}
	return nil
}

// WaitForMetric polls until a metric with the given name is present or the
// deadline elapses. Returns the metric or nil on timeout.
func (r *Receiver) WaitForMetric(name string, timeout time.Duration) *metricpb.Metric {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if m := r.MetricByName(name); m != nil {
			return m
		}
		time.Sleep(50 * time.Millisecond)
	}
	return r.MetricByName(name)
}

// WaitForSpan polls until a span with the given name is present or the
// deadline elapses. Returns the span or nil on timeout.
func (r *Receiver) WaitForSpan(name string, timeout time.Duration) *tracepb.Span {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s := r.SpanByName(name); s != nil {
			return s
		}
		time.Sleep(50 * time.Millisecond)
	}
	return r.SpanByName(name)
}

// AttrString returns the string value of a span attribute, or "".
func AttrString(attrs []*commonpb.KeyValue, key string) string {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

// HasAttr reports whether the attribute key is present.
func HasAttr(attrs []*commonpb.KeyValue, key string) bool {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return true
		}
	}
	return false
}

// SumForAttrs returns the summed counter value across the metric's Sum data
// points whose attributes match every key=value in want. Works for the
// integer OTLP Sum counters this project emits.
func SumForAttrs(m *metricpb.Metric, want map[string]string) int64 {
	if m == nil || m.GetSum() == nil {
		return 0
	}
	var total int64
	for _, dp := range m.GetSum().GetDataPoints() {
		if !attrsMatch(dp.GetAttributes(), want) {
			continue
		}
		total += dp.GetAsInt()
	}
	return total
}

// CounterAttrValues returns the distinct values seen for a given attribute key
// across a Sum metric's data points.
func CounterAttrValues(m *metricpb.Metric, key string) map[string]bool {
	out := map[string]bool{}
	if m == nil || m.GetSum() == nil {
		return out
	}
	for _, dp := range m.GetSum().GetDataPoints() {
		out[AttrString(dp.GetAttributes(), key)] = true
	}
	return out
}

func attrsMatch(attrs []*commonpb.KeyValue, want map[string]string) bool {
	for k, v := range want {
		if AttrString(attrs, k) != v {
			return false
		}
	}
	return true
}

// AllStringAttrValues returns every string attribute value across all received
// spans (including span-event attributes) and all metric data points. Used by
// the content-safety test to prove no prompt sentinel leaked into telemetry.
func (r *Receiver) AllStringAttrValues() []string {
	var out []string
	for _, kv := range r.ResourceAttrs() {
		out = append(out, kv.GetValue().GetStringValue())
	}
	for _, s := range r.Spans() {
		out = append(out, s.GetName())
		for _, kv := range s.GetAttributes() {
			out = append(out, kv.GetValue().GetStringValue())
		}
		for _, ev := range s.GetEvents() {
			out = append(out, ev.GetName())
			for _, kv := range ev.GetAttributes() {
				out = append(out, kv.GetValue().GetStringValue())
			}
		}
	}
	for _, m := range r.Metrics() {
		if m.GetSum() != nil {
			for _, dp := range m.GetSum().GetDataPoints() {
				for _, kv := range dp.GetAttributes() {
					out = append(out, kv.GetValue().GetStringValue())
				}
			}
		}
		if m.GetHistogram() != nil {
			for _, dp := range m.GetHistogram().GetDataPoints() {
				for _, kv := range dp.GetAttributes() {
					out = append(out, kv.GetValue().GetStringValue())
				}
			}
		}
	}
	return out
}
