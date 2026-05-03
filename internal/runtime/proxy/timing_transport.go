package proxy

import (
	"bytes"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptrace"
	"time"

	"github.com/elazarl/goproxy"

	runtimetiming "github.com/clawvisor/clawvisor/internal/runtime/timing"
)

type timingRoundTripper struct {
	server *Server
}

func newTimingRoundTripper(server *Server) goproxy.RoundTripper {
	return goproxy.RoundTripperFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Response, error) {
		return (&timingRoundTripper{server: server}).RoundTrip(req, ctx)
	})
}

func (t *timingRoundTripper) RoundTrip(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Response, error) {
	if req == nil || ctx == nil || ctx.Proxy == nil || ctx.Proxy.Tr == nil {
		return nil, nil
	}
	if t == nil || t.server == nil {
		return ctx.Proxy.Tr.RoundTrip(req)
	}
	st := StateOf(ctx)
	if t.server.cfg.BodyTraces && st != nil && req.Body != nil {
		requestBody, err := io.ReadAll(req.Body)
		if err != nil {
			runtimetiming.SetAttr(req.Context(), "request.capture_error", err.Error())
		} else {
			req.Body = io.NopCloser(bytes.NewReader(requestBody))
			req.ContentLength = int64(len(requestBody))
			t.server.captureBodyArtifact(req.Context(), st, "request", requestBody)
		}
	}

	overallStartedAt := time.Now()
	var getConnStartedAt time.Time
	var dnsStartedAt time.Time
	var connectStartedAt time.Time
	var tlsStartedAt time.Time
	var wroteRequestAt time.Time

	trace := &httptrace.ClientTrace{
		GetConn: func(_ string) {
			getConnStartedAt = time.Now()
		},
		GotConn: func(info httptrace.GotConnInfo) {
			if !getConnStartedAt.IsZero() {
				runtimetiming.RecordSpan(req.Context(), "upstream.get_conn_wait", time.Since(getConnStartedAt))
			}
			runtimetiming.SetAttr(req.Context(), "upstream.conn_reused", info.Reused)
			runtimetiming.SetAttr(req.Context(), "upstream.conn_was_idle", info.WasIdle)
			runtimetiming.SetAttr(req.Context(), "upstream.conn_idle_ms", info.IdleTime.Milliseconds())
			if info.Conn != nil {
				runtimetiming.SetAttr(req.Context(), "upstream.remote_addr", info.Conn.RemoteAddr().String())
				runtimetiming.SetAttr(req.Context(), "upstream.local_addr", info.Conn.LocalAddr().String())
			}
		},
		DNSStart: func(httptrace.DNSStartInfo) {
			dnsStartedAt = time.Now()
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			if !dnsStartedAt.IsZero() {
				runtimetiming.RecordSpan(req.Context(), "upstream.dns", time.Since(dnsStartedAt))
			}
			runtimetiming.SetAttr(req.Context(), "upstream.dns_addrs", len(info.Addrs))
			if info.Err != nil {
				runtimetiming.SetAttr(req.Context(), "upstream.dns_error", info.Err.Error())
			}
		},
		ConnectStart: func(_, _ string) {
			connectStartedAt = time.Now()
		},
		ConnectDone: func(_, _ string, err error) {
			if !connectStartedAt.IsZero() {
				runtimetiming.RecordSpan(req.Context(), "upstream.connect", time.Since(connectStartedAt))
			}
			if err != nil {
				runtimetiming.SetAttr(req.Context(), "upstream.connect_error", err.Error())
			}
		},
		TLSHandshakeStart: func() {
			tlsStartedAt = time.Now()
		},
		TLSHandshakeDone: func(cs tls.ConnectionState, err error) {
			if !tlsStartedAt.IsZero() {
				runtimetiming.RecordSpan(req.Context(), "upstream.tls_handshake", time.Since(tlsStartedAt))
			}
			runtimetiming.SetAttr(req.Context(), "upstream.tls_version", tlsVersionName(cs.Version))
			runtimetiming.SetAttr(req.Context(), "upstream.tls_cipher", tls.CipherSuiteName(cs.CipherSuite))
			if err != nil {
				runtimetiming.SetAttr(req.Context(), "upstream.tls_error", err.Error())
			}
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			runtimetiming.RecordSpan(req.Context(), "upstream.write_request", time.Since(overallStartedAt))
			wroteRequestAt = time.Now()
			if info.Err != nil {
				runtimetiming.SetAttr(req.Context(), "upstream.write_error", info.Err.Error())
			}
		},
		GotFirstResponseByte: func() {
			if !wroteRequestAt.IsZero() {
				runtimetiming.RecordSpan(req.Context(), "upstream.wait_first_byte", time.Since(wroteRequestAt))
			}
		},
	}

	resp, err := ctx.Proxy.Tr.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	runtimetiming.RecordSpan(req.Context(), "upstream.roundtrip_headers", time.Since(overallStartedAt))
	if err != nil {
		runtimetiming.SetAttr(req.Context(), "upstream.roundtrip_error", err.Error())
		return nil, err
	}
	if resp != nil {
		runtimetiming.SetAttr(req.Context(), "upstream.proto", resp.Proto)
		runtimetiming.SetAttr(req.Context(), "upstream.status_code", resp.StatusCode)
		runtimetiming.SetAttr(req.Context(), "upstream.content_length", resp.ContentLength)
		if resp.Body != nil {
			resp.Body = &timingReadCloser{
				rc:            resp.Body,
				ctx:           req.Context(),
				readSpanName:  "upstream.response_body_read",
				closeSpanName: "upstream.response_body_close",
				bytesAttrName: "upstream.response_body_bytes",
				captureBody:   t.server.cfg.BodyTraces,
				bodyHook: func(body []byte) {
					t.server.captureBodyArtifact(req.Context(), st, "upstream_response", body)
				},
			}
		}
	}
	return resp, nil
}

func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS10:
		return "TLS1.0"
	case tls.VersionTLS11:
		return "TLS1.1"
	case tls.VersionTLS12:
		return "TLS1.2"
	case tls.VersionTLS13:
		return "TLS1.3"
	default:
		return ""
	}
}
