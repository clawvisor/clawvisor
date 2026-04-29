package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/elazarl/goproxy"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

type cachedRuntimeRequestContext struct {
	Context   *RuntimeRequestContext
	ExpiresAt time.Time
}

func (s *Server) InstallRequestContextCarrier() {
	registry := conversation.DefaultRegistry()
	s.goproxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		st := EnsureState(ctx)
		if st.Session == nil {
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
		built := buildRuntimeRequestContext(req, parser, body)
		if st.Runtime != nil && st.Runtime.SecretScan != nil {
			built.SecretScan = st.Runtime.SecretScan
		}
		st.Runtime = built
		if st.Session != nil && st.Runtime != nil {
			s.latestRequestCtxBySession.Store(st.Session.ID, cachedRuntimeRequestContext{
				Context:   st.Runtime,
				ExpiresAt: st.Session.ExpiresAt,
			})
			s.pruneLatestRuntimeRequestContexts()
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
	switch cached := value.(type) {
	case cachedRuntimeRequestContext:
		if !cached.ExpiresAt.IsZero() && time.Now().After(cached.ExpiresAt) {
			s.latestRequestCtxBySession.Delete(sessionID)
			return nil
		}
		return cached.Context
	case *RuntimeRequestContext:
		return cached
	default:
		return nil
	}
}

func (s *Server) pruneLatestRuntimeRequestContexts() {
	if s == nil {
		return
	}
	if atomic.AddUint64(&s.latestRequestCtxPruneTick, 1)%64 != 0 {
		return
	}
	now := time.Now()
	s.latestRequestCtxBySession.Range(func(key, value any) bool {
		cached, ok := value.(cachedRuntimeRequestContext)
		if ok && !cached.ExpiresAt.IsZero() && now.After(cached.ExpiresAt) {
			s.latestRequestCtxBySession.Delete(key)
		}
		return true
	})
}
