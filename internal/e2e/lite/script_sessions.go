package lite

import (
	"context"
	"errors"

	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
)

// Counter series exposed for scenarios that exercise the autovault
// script-session path. Hard expectations key on these to assert
// proxy mechanics without depending on agent behavior text.
//
// Series:
//   - SeriesScriptSessionMint:          POST /api/control/autovault/script-session minted a session
//   - SeriesScriptSessionUse:           resolver Authorize() admitted a request under a session token
//   - SeriesScriptSessionScopeMismatch: resolver Authorize() rejected for host/method/path/placeholder
//   - SeriesScriptSessionExhausted:     resolver Authorize() rejected for max_uses reached
//
// IMPORTANT: the harness's lite resolver always calls
// ScriptSessionCache.RecordBytes(token, 0) — release-only, never
// records actual upstream bytes. That keeps the optimistic byte
// reservation released (so MaxRequestBytes doesn't leak), but means
// SeriesScriptSessionUse + the session's TotalBytesUsed counter do
// NOT reflect real payload sizes. A scenario that asserts on
// MaxTotalBytes utilization (e.g. "cap fires after N MiB streamed")
// would pass spuriously in this harness. If such a scenario is added,
// wrap io.Copy in newLiteResolver to count bytes and pass that count
// to RecordBytes — see proxy_resolver.go's defer for the production
// shape.
const (
	SeriesScriptSessionMint          = "script_session.mint"
	SeriesScriptSessionUse           = "script_session.use"
	SeriesScriptSessionScopeMismatch = "script_session.scope_mismatch"
	SeriesScriptSessionExhausted     = "script_session.exhausted"
)

// countingScriptSessionCache wraps a real ScriptSessionCache and
// increments harness counters on each operation. The mint endpoint
// and the resolver middleware both call into this cache, so a single
// wrapper instruments both ends of the script-session lifecycle.
//
// Errors are NOT translated — we forward whatever the inner cache
// returns. The wrapper only observes; it never changes outcomes.
type countingScriptSessionCache struct {
	inner    llmproxy.ScriptSessionCache
	counters *Counters
}

var _ llmproxy.ScriptSessionCache = (*countingScriptSessionCache)(nil)

func (c *countingScriptSessionCache) Mint(ctx context.Context, sess llmproxy.ScriptSession) (string, error) {
	token, err := c.inner.Mint(ctx, sess)
	if err == nil {
		c.counters.Inc(SeriesScriptSessionMint)
	}
	return token, err
}

func (c *countingScriptSessionCache) Authorize(ctx context.Context, token string, req llmproxy.ScriptSessionRequest) (llmproxy.ScriptSession, error) {
	sess, err := c.inner.Authorize(ctx, token, req)
	switch {
	case err == nil:
		c.counters.Inc(SeriesScriptSessionUse)
	case errors.Is(err, llmproxy.ErrScriptSessionScopeMismatch):
		c.counters.Inc(SeriesScriptSessionScopeMismatch)
	case errors.Is(err, llmproxy.ErrScriptSessionExhausted):
		c.counters.Inc(SeriesScriptSessionExhausted)
	}
	return sess, err
}

func (c *countingScriptSessionCache) RecordBytes(ctx context.Context, token string, bytes int64) (llmproxy.ScriptSession, error) {
	return c.inner.RecordBytes(ctx, token, bytes)
}

func (c *countingScriptSessionCache) ReleaseAuthorize(ctx context.Context, token string) error {
	return c.inner.ReleaseAuthorize(ctx, token)
}

func (c *countingScriptSessionCache) Revoke(ctx context.Context, token string) error {
	return c.inner.Revoke(ctx, token)
}
