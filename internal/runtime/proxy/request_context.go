package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"

	"github.com/elazarl/goproxy"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

func (s *Server) InstallRequestContextCarrier() {
	registry := conversation.DefaultRegistry()
	s.goproxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		st := EnsureState(ctx)
		if st.Session == nil || st.Runtime != nil {
			return req, nil
		}
		parser := registry.Match(req)
		if parser == nil || req.Body == nil {
			return req, nil
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return req, nil
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		st.Runtime = buildRuntimeRequestContext(req, parser, body)
		if st.Session != nil && st.Runtime != nil {
			s.latestRequestCtxBySession.Store(st.Session.ID, st.Runtime)
		}
		return req, nil
	})
}

func buildRuntimeRequestContext(req *http.Request, parser conversation.Parser, body []byte) *RuntimeRequestContext {
	if req == nil || parser == nil {
		return nil
	}
	ctx := &RuntimeRequestContext{
		Provider:       string(parser.Name()),
		RequestPath:    req.URL.Path,
		RequestBodySHA: sha256Hex(body),
	}
	turns, err := parser.ParseRequest(body)
	if err != nil {
		ctx.ParseErr = err
	} else {
		ctx.ParsedTurns = turns
	}
	ctx.ToolResultsSeen = toolResultIDsForRequest(req, body)
	return ctx
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func (s *Server) latestRuntimeRequestContext(sessionID string) *RuntimeRequestContext {
	if s == nil || sessionID == "" {
		return nil
	}
	value, ok := s.latestRequestCtxBySession.Load(sessionID)
	if !ok {
		return nil
	}
	ctx, _ := value.(*RuntimeRequestContext)
	return ctx
}
