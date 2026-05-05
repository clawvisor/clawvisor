package proxy

import (
	"testing"
	"time"
)

func TestLatestRuntimeRequestContextPrunesExpiredEntries(t *testing.T) {
	srv := &Server{}
	srv.latestRequestCtxBySession.Store("expired", cachedRuntimeRequestContext{
		Context:   &RuntimeRequestContext{Provider: "expired"},
		ExpiresAt: time.Now().Add(-time.Minute),
	})
	srv.latestRequestCtxBySession.Store("active", cachedRuntimeRequestContext{
		Context:   &RuntimeRequestContext{Provider: "active"},
		ExpiresAt: time.Now().Add(time.Minute),
	})
	srv.latestRequestCtxPruneTick = 63
	srv.pruneLatestRuntimeRequestContexts()

	if got := srv.latestRuntimeRequestContext("expired"); got != nil {
		t.Fatalf("expected expired context to be pruned, got %+v", got)
	}
	if got := srv.latestRuntimeRequestContext("active"); got == nil || got.Provider != "active" {
		t.Fatalf("expected active context to remain, got %+v", got)
	}
}
