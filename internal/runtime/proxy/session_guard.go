package proxy

import (
	"errors"
	"net/http"
	"time"

	"github.com/elazarl/goproxy"
)

func (s *Server) InstallSessionGuard(auth *Authenticator) {
	s.goproxy.OnRequest().HandleConnect(goproxy.FuncHttpsHandler(func(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
		t0 := time.Now()
		sess, err := auth.Authenticate(ctx.Req.Context(), ctx.Req.Header)
		if err != nil {
			ctx.Resp = authRequiredResponse(ctx.Req, err)
			return goproxy.RejectConnect, host
		}
		ctx.Req.Header.Del(internalBypassHeader)
		st := EnsureState(ctx)
		st.Session = sess
		ctx.Req = s.attachTimingRecorder(ctx.Req, st)
		s.recordTimingSpan(ctx.Req, "session_guard.auth", t0)
		return goproxy.MitmConnect, host
	}))
	s.goproxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		st := StateOf(ctx)
		if st != nil && st.Session != nil && st.Session.ID != "" {
			req.Header.Del(internalBypassHeader)
			req = s.attachTimingRecorder(req, st)
			if ctx != nil && ctx.Req != nil {
				ctx.Req = req
			}
			return req, nil
		}
		t0 := time.Now()
		sess, err := auth.Authenticate(req.Context(), req.Header)
		if err != nil {
			return req, authRequiredResponse(req, err)
		}
		req.Header.Del(internalBypassHeader)
		if ctx.Req != nil {
			ctx.Req.Header.Del(internalBypassHeader)
		}
		st = EnsureState(ctx)
		st.Session = sess
		req = s.attachTimingRecorder(req, st)
		s.recordTimingSpan(req, "session_guard.auth", t0)
		if ctx != nil && ctx.Req != nil {
			ctx.Req = req
		}
		return req, nil
	})
}

func authRequiredResponse(req *http.Request, err error) *http.Response {
	status := http.StatusProxyAuthRequired
	body := "Proxy-Authorization required: provide a Clawvisor runtime token or use an authenticated proxy URL.\n"
	if errors.Is(err, ErrProxyAuthorizationRejected) {
		body = "Proxy-Authorization rejected: credentials are missing, malformed, or expired.\n"
	}
	return goproxy.NewResponse(req, "text/plain", status, body)
}
