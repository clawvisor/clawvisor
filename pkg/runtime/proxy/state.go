package proxy

import (
	"sync"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	runtimetiming "github.com/clawvisor/clawvisor/internal/runtime/timing"
	"github.com/clawvisor/clawvisor/pkg/store"
)

type SecretScanSummary struct {
	ReplacementCount int      `json:"replacement_count"`
	Sources          []string `json:"sources,omitempty"`
}

type RuntimeRequestContext struct {
	Provider        string
	RequestPath     string
	ParsedTurns     []conversation.Turn
	ParseErr        error
	RequestBodySHA  string
	ToolResultsSeen []string
	SecretScan      *SecretScanSummary
}

type RequestState struct {
	RequestID              string
	StartedAt              time.Time
	Session                *store.RuntimeSession
	Runtime                *RuntimeRequestContext
	AuditID                string
	SkipAuditOutcomeUpdate bool
	// PolicyDenied marks that Clawvisor short-circuited this request with a
	// synthetic 403 (a runtime policy deny or an approval/review hold) rather
	// than forwarding it upstream. The observability hook reads this instead
	// of resp.StatusCode so a genuine upstream 403 (e.g. a bad API key) is not
	// mislabeled as a Clawvisor denial in the runtimeproxy.requests metric.
	PolicyDenied bool
	Timings      *runtimetiming.Recorder

	StatusLogged sync.Once
	TimingLogged sync.Once
}

func StateOf(ctx *goproxy.ProxyCtx) *RequestState {
	if ctx == nil || ctx.UserData == nil {
		return nil
	}
	s, _ := ctx.UserData.(*RequestState)
	return s
}

func EnsureState(ctx *goproxy.ProxyCtx) *RequestState {
	if s := StateOf(ctx); s != nil {
		return s
	}
	s := &RequestState{
		RequestID: uuid.NewString(),
		StartedAt: time.Now().UTC(),
	}
	ctx.UserData = s
	return s
}
