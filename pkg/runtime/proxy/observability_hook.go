package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"

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

		decision := runtimeProxyDecision(st)

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
		// Defer span.End() to a body-close wrapper so the span covers the
		// full request lifetime — goproxy copies the (often streaming or
		// large) response body to the client AFTER this OnResponse hook
		// returns, so ending the span here would truncate the duration.
		// Mirrors the newToolUseStreamBody body-wrap pattern in this package.
		// Fall back to ending inline when there's no body to hang the end on.
		if resp != nil && resp.Body != nil {
			resp.Body = newSpanEndBody(resp.Body, span)
		} else {
			span.End()
		}

		return resp
	})
}

// runtimeProxyDecision maps a completed request's RequestState to the coarse
// decision attribute (allowed/denied/observed). A Clawvisor policy block is
// recorded from the explicit PolicyDenied marker set by the policy hook — NOT
// from resp.StatusCode — so a genuine upstream 403 (e.g. a bad API key) is not
// mislabeled as a Clawvisor denial and the security-relevant denied count
// stays accurate.
func runtimeProxyDecision(st *RequestState) string {
	switch {
	case st.PolicyDenied:
		return "denied"
	case st.Session != nil && st.Session.ObservationMode:
		return "observed"
	default:
		return "allowed"
	}
}

// spanEndBody wraps a response body so the runtimeproxy.request span ends when
// the body is closed — i.e. after goproxy has copied the full (possibly
// streaming) response to the client — instead of at OnResponse time before the
// body is written. Mirrors the body-wrap pattern used by newToolUseStreamBody.
type spanEndBody struct {
	io.ReadCloser
	span trace.Span
	once sync.Once
}

func newSpanEndBody(body io.ReadCloser, span trace.Span) *spanEndBody {
	return &spanEndBody{ReadCloser: body, span: span}
}

func (b *spanEndBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() { b.span.End() })
	return err
}

// runtimeHostCategory buckets a destination host into "llm" or "other" so the
// runtime-proxy metric never carries a raw hostname. The LLM set covers the
// provider API endpoints the proxy routes agent traffic through.
func runtimeHostCategory(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	// Bedrock uses regional endpoints, bedrock-runtime.<region>.amazonaws.com
	// (e.g. bedrock-runtime.us-east-1.amazonaws.com). The static suffix list
	// can't express the wildcard region segment, so match the prefix+suffix
	// pair explicitly before the generic loop — otherwise all real Bedrock
	// traffic falls through to "other".
	if strings.HasPrefix(h, "bedrock-runtime.") && strings.HasSuffix(h, ".amazonaws.com") {
		return "llm"
	}
	for _, suffix := range llmHostSuffixes {
		if h == suffix || strings.HasSuffix(h, "."+suffix) {
			return "llm"
		}
	}
	return "other"
}

// llmHostSuffixes are exact-or-subdomain LLM provider hosts. Bedrock regional
// endpoints are handled separately in runtimeHostCategory because their
// <region> segment can't be expressed as a fixed suffix.
var llmHostSuffixes = []string{
	"api.anthropic.com",
	"api.openai.com",
	"generativelanguage.googleapis.com",
	"aiplatform.googleapis.com",
}
