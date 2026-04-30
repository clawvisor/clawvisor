package proxy

import (
	"net/http"
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
	s.goproxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		st := StateOf(ctx)
		if st == nil || st.Timings == nil {
			return resp
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
			if err := s.traceSink.Write(entry); err != nil && s.logger != nil {
				s.logger.Warn("runtime timing trace write failed", "err", err)
			}
		})
		return resp
	})
}
