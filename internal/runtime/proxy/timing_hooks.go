package proxy

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/elazarl/goproxy"

	runtimetiming "github.com/clawvisor/clawvisor/internal/runtime/timing"
)

func (s *Server) attachTimingRecorder(req *http.Request, st *RequestState) *http.Request {
	if s == nil || !s.cfg.LogTimings || req == nil || st == nil {
		return req
	}
	if st.Timings == nil {
		st.Timings = &runtimetiming.Recorder{}
	}
	req = req.WithContext(runtimetiming.WithRecorder(req.Context(), st.Timings))
	return req
}

func (s *Server) recordTimingSpan(req *http.Request, name string, start time.Time) {
	if s == nil || !s.cfg.LogTimings || req == nil {
		return
	}
	runtimetiming.Stop(req.Context(), name, start)
}

func (s *Server) InstallTimingTrace() {
	if s == nil || !s.cfg.LogTimings {
		return
	}
	s.goproxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		st := EnsureState(ctx)
		req = s.attachTimingRecorder(req, st)
		if ctx != nil && ctx.RoundTripper == nil {
			ctx.RoundTripper = newTimingRoundTripper(s)
		}
		if ctx != nil && ctx.Req != nil {
			ctx.Req = req
		}
		return req, nil
	})
	s.goproxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		st := StateOf(ctx)
		if st == nil || st.Timings == nil {
			return resp
		}
		if resp == nil || resp.Body == nil {
			s.logTimingTrace(ctx, st, resp)
			return resp
		}
		resp.Body = &timingReadCloser{
			rc:            resp.Body,
			ctx:           ctx.Req.Context(),
			readSpanName:  "response.client_body_read",
			closeSpanName: "response.client_body_close",
			bytesAttrName: "response.client_body_bytes",
			closeHook: func() {
				s.logTimingTrace(ctx, st, resp)
			},
		}
		return resp
	})
}

func (s *Server) logTimingTrace(ctx *goproxy.ProxyCtx, st *RequestState, resp *http.Response) {
	if s == nil || st == nil || st.Timings == nil {
		return
	}
	st.TimingLogged.Do(func() {
		entry := runtimetiming.TraceEntry{
			Timestamp: time.Now().UTC(),
			RequestID: st.RequestID,
			TotalMS:   time.Since(st.StartedAt).Milliseconds(),
			Summary:   st.Timings.Summary(),
		}
		if st.Session != nil {
			entry.SessionID = st.Session.ID
			entry.AgentID = st.Session.AgentID
			entry.ObservationMode = st.Session.ObservationMode
		}
		if ctx != nil && ctx.Req != nil {
			entry.Method = ctx.Req.Method
			entry.Host = requestHost(ctx.Req)
			if ctx.Req.URL != nil {
				entry.Path = ctx.Req.URL.Path
			}
		}
		if st.Runtime != nil {
			entry.Provider = st.Runtime.Provider
		}
		if resp != nil {
			entry.StatusCode = resp.StatusCode
		}
		spans := st.Timings.Spans()
		if len(spans) > 0 {
			entry.Spans = make([]runtimetiming.TraceSpan, 0, len(spans))
			for _, span := range spans {
				entry.Spans = append(entry.Spans, runtimetiming.TraceSpan{
					Name:       span.Name,
					DurationMS: span.Dur.Milliseconds(),
				})
			}
		}
		if attrs := st.Timings.Attrs(); len(attrs) > 0 {
			entry.Attrs = attrs
		}
		if err := s.traceSink.Write(entry); err != nil && s.logger != nil {
			s.logger.Warn("runtime timing trace write failed", "err", err)
		}
	})
}

type timingReadCloser struct {
	rc            io.ReadCloser
	ctx           context.Context
	readSpanName  string
	closeSpanName string
	bytesAttrName string
	closeHook     func()
	readDur       time.Duration
	bytesRead     int64
	finalizeOnce  sync.Once
}

func (t *timingReadCloser) Read(p []byte) (int, error) {
	if t == nil || t.rc == nil {
		return 0, io.EOF
	}
	start := time.Now()
	n, err := t.rc.Read(p)
	t.readDur += time.Since(start)
	t.bytesRead += int64(n)
	return n, err
}

func (t *timingReadCloser) Close() error {
	if t == nil || t.rc == nil {
		return nil
	}
	start := time.Now()
	err := t.rc.Close()
	t.finalizeOnce.Do(func() {
		runtimetiming.RecordSpan(t.ctx, t.readSpanName, t.readDur)
		if t.bytesAttrName != "" {
			runtimetiming.SetAttr(t.ctx, t.bytesAttrName, t.bytesRead)
		}
		runtimetiming.RecordSpan(t.ctx, t.closeSpanName, time.Since(start))
		if err != nil {
			runtimetiming.SetAttr(t.ctx, t.closeSpanName+".error", err.Error())
		}
		if t.closeHook != nil {
			t.closeHook()
		}
	})
	return err
}
