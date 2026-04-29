package proxy

import (
	"net/http"
	"time"

	"github.com/elazarl/goproxy"

	runtimeautovault "github.com/clawvisor/clawvisor/internal/runtime/autovault"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

type PlaceholderHooks struct {
	Store store.Store
	Vault vault.Vault
}

func (s *Server) InstallPlaceholderSwap(hooks PlaceholderHooks) {
	if hooks.Store == nil || hooks.Vault == nil {
		return
	}
	s.goproxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		if req.Header.Get(internalBypassHeader) != "" {
			return req, nil
		}
		st := StateOf(ctx)
		if st == nil || st.Session == nil {
			return req, nil
		}

		for headerName, values := range req.Header {
			if len(values) == 0 {
				continue
			}
			replacedValues := make([]string, len(values))
			for i, value := range values {
				replaced, placeholders, err := runtimeautovault.ReplaceHeaderValue(value, func(placeholder string) (string, error) {
					meta, err := hooks.Store.GetRuntimePlaceholder(req.Context(), placeholder)
					if err != nil {
						return "", err
					}
					if meta.AgentID != st.Session.AgentID || meta.UserID != st.Session.UserID {
						return "", store.ErrNotFound
					}
					credBytes, err := hooks.Vault.Get(req.Context(), meta.UserID, meta.ServiceID)
					if err != nil {
						return "", err
					}
					return runtimeautovault.ExtractCredentialValue(credBytes)
				})
				if err != nil {
					return req, goproxy.NewResponse(req, "application/json", http.StatusForbidden, `{"error":"runtime placeholder rejected","code":"PLACEHOLDER_REJECTED"}`)
				}
				replacedValues[i] = replaced
				for _, placeholder := range placeholders {
					_ = hooks.Store.TouchRuntimePlaceholder(req.Context(), placeholder, time.Now().UTC())
				}
			}
			req.Header[headerName] = replacedValues
		}
		return req, nil
	})
}
