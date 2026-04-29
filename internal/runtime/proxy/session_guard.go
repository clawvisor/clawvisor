package proxy

import (
	"errors"
	"net/http"

	"github.com/elazarl/goproxy"
)

func (s *Server) InstallSessionGuard(auth *Authenticator) {
	s.goproxy.OnRequest().HandleConnect(goproxy.FuncHttpsHandler(func(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
		sess, err := auth.Authenticate(ctx.Req.Context(), ctx.Req.Header)
		if err != nil {
			ctx.Resp = authRequiredResponse(ctx.Req, err)
			return goproxy.RejectConnect, host
		}
		st := EnsureState(ctx)
		st.Session = sess
		return goproxy.MitmConnect, host
	}))
	s.goproxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		st := StateOf(ctx)
		if st != nil && st.Session != nil && st.Session.ID != "" {
			return req, nil
		}
		sess, err := auth.Authenticate(req.Context(), req.Header)
		if err != nil {
			return req, authRequiredResponse(req, err)
		}
		st = EnsureState(ctx)
		st.Session = sess
		return req, nil
	})
}

func authRequiredResponse(req *http.Request, err error) *http.Response {
	status := http.StatusProxyAuthRequired
	body := "Proxy-Authorization required: provide a bearer token minted by Clawvisor.\n"
	if errors.Is(err, ErrProxyAuthorizationRejected) {
		body = "Proxy-Authorization rejected: token is missing, malformed, or expired.\n"
	}
	return goproxy.NewResponse(req, "text/plain", status, body)
}
