package proxy

import (
	"context"
	"net/http"
	"strings"

	"github.com/elazarl/goproxy"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/clawvisor/clawvisor/internal/observability"
)

// InstallObservability registers an OnResponse hook that emits the
// clawvisor.runtimeproxy.requests metric and a clawvisor.runtimeproxy.request
// span per proxied request. It is a no-op when inst is nil (observability
// disabled). The hook runs on both real upstream responses and synthetic
// responses (policy denials), so every proxied request is counted exactly
// once.
//
// Cardinality/content rules: attributes carry only a coarse host_category
// (llm/other), the decision, and the session id — never raw destination
// hostnames, URL paths, headers, or body content.
func (s *Server) InstallObservability(inst *observability.Instruments) {
	if s == nil || inst == nil {
		return
	}
	s.goproxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		st := StateOf(ctx)
		// Requests without session state never entered the enforced path
		// (e.g. CONNECT bookkeeping); skip them to keep the metric aligned
		// with real proxied requests.
		if st == nil || st.Session == nil {
			return resp
		}

		host := ""
		if ctx != nil && ctx.Req != nil {
			host = requestHost(ctx.Req)
		}
		hostCategory := runtimeHostCategory(host)

		decision := "allowed"
		switch {
		case resp != nil && resp.StatusCode == http.StatusForbidden:
			decision = "denied"
		case st.Session.ObservationMode:
			decision = "observed"
		}

		inst.RecordRuntimeProxyRequest(context.Background(), decision, hostCategory)

		// Emit a span spanning the request lifetime (StartedAt → now).
		reqCtx := context.Background()
		if ctx != nil && ctx.Req != nil {
			reqCtx = ctx.Req.Context()
		}
		_, span := observability.Tracer().Start(reqCtx, observability.SpanRuntimeProxyReq,
			trace.WithTimestamp(st.StartedAt))
		span.SetAttributes(
			attribute.String(observability.SpanAttrHostCategory, hostCategory),
			attribute.String(observability.SpanAttrDecision, decision),
			attribute.String(observability.SpanAttrSessionID, st.Session.ID),
		)
		span.End()

		return resp
	})
}

// runtimeHostCategory buckets a destination host into "llm" or "other" so the
// runtime-proxy metric never carries a raw hostname. The LLM set covers the
// provider API endpoints the proxy routes agent traffic through.
func runtimeHostCategory(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	for _, suffix := range llmHostSuffixes {
		if h == suffix || strings.HasSuffix(h, "."+suffix) {
			return "llm"
		}
	}
	return "other"
}

var llmHostSuffixes = []string{
	"api.anthropic.com",
	"api.openai.com",
	"generativelanguage.googleapis.com",
	"aiplatform.googleapis.com",
	"bedrock-runtime.amazonaws.com",
}
