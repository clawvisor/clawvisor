package proxy

import (
	"sync"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
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

	StatusLogged sync.Once
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
