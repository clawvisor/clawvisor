package otlp

import (
	"bytes"
	"net/http"
	"testing"

	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func strAttr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{
		Key:   k,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}},
	}
}

func postProto(t *testing.T, url string, m proto.Message) {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(url, "application/x-protobuf", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestReceiverCapturesResourceAttrs proves the leak sentinel
// (AllStringAttrValues) sees OTLP Resource attributes on both traces and
// metrics — the receiver used to drop ResourceSpans/ResourceMetrics Resource,
// so a sentinel leaked into a resource attribute would have gone unnoticed.
func TestReceiverCapturesResourceAttrs(t *testing.T) {
	r := NewReceiver(t)

	const traceSentinel = "trace-resource-sentinel"
	const metricSentinel = "metric-resource-sentinel"

	postProto(t, r.srv.URL+"/v1/traces", &collectortracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				strAttr("service.name", "clawvisor"),
				strAttr("leak.marker", traceSentinel),
			}},
			ScopeSpans: []*tracepb.ScopeSpans{{
				Spans: []*tracepb.Span{{Name: "some.span"}},
			}},
		}},
	})

	postProto(t, r.srv.URL+"/v1/metrics", &collectormetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricpb.ResourceMetrics{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				strAttr("service.version", "v1.2.3"),
				strAttr("leak.marker", metricSentinel),
			}},
			ScopeMetrics: []*metricpb.ScopeMetrics{{
				Metrics: []*metricpb.Metric{{Name: "some.metric"}},
			}},
		}},
	})

	all := r.AllStringAttrValues()
	found := map[string]bool{}
	for _, v := range all {
		found[v] = true
	}
	for _, want := range []string{traceSentinel, metricSentinel, "clawvisor", "v1.2.3"} {
		if !found[want] {
			t.Errorf("AllStringAttrValues missing resource attr value %q; got %v", want, all)
		}
	}
}
