package llmproxy

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// ScriptSessionCache mints + accounts short-lived, multi-use, scoped
// caller-auth tokens that authorize an agent script to call the
// resolver multiple times within a tightly defined capability. Unlike
// CallerNonceCache (one-shot, bound to an exact path), a script
// session is bound to:
//
//   - agent_id
//   - placeholder (single autovault_… per session)
//   - target host (exactly one)
//   - allowed methods
//   - allowed path prefixes
//   - max uses
//   - TTL
//   - per-request and aggregate response byte caps
//
// The resolver looks up the session by token, validates the inbound
// request against the bound capability, atomically increments the
// use count, and records bytes streamed against the aggregate cap.
//
// Tokens carry the ScriptSessionPrefix so the resolver middleware can
// branch on prefix without parsing the payload.
type ScriptSessionCache interface {
	// Mint allocates a fresh token bound to the given session and
	// stores it with the session's expiry. Returns the opaque token
	// string the agent embeds in X-Clawvisor-Caller.
	Mint(ctx context.Context, sess ScriptSession) (token string, err error)

	// Authorize atomically validates the inbound request against the
	// session bound to `token` and, when allowed, increments the
	// session's use count. Returns the session snapshot so the
	// resolver can read the bound placeholder, agent, byte caps, and
	// audit fields without a second lookup. The returned UsedCount is
	// the post-increment value (1-based).
	//
	// Errors:
	//   - ErrScriptSessionNotFound: token unknown / revoked
	//   - ErrScriptSessionExpired: token exists but TTL elapsed
	//   - ErrScriptSessionExhausted: max_uses already reached
	//   - ErrScriptSessionScopeMismatch: host/method/path/placeholder
	//     mismatch against the bound session
	Authorize(ctx context.Context, token string, req ScriptSessionRequest) (ScriptSession, error)

	// RecordBytes adds `bytes` to the session's aggregate response-
	// bytes counter. Returns the post-update session snapshot.
	// Returns ErrScriptSessionBytesExceeded when the cap was already
	// breached (the bytes are still added so the audit row shows the
	// actual overage). Resolver callers should stop streaming when
	// this returns the bytes-exceeded sentinel.
	//
	// Non-existent tokens silently no-op so a session expiring
	// mid-stream doesn't introduce a separate error path.
	RecordBytes(ctx context.Context, token string, bytes int64) (ScriptSession, error)

	// ReleaseAuthorize fully undoes a prior Authorize call:
	// releases the optimistic byte reservation AND decrements
	// UsedCount. Use when a request was Authorize'd but couldn't be
	// forwarded for reasons unrelated to the session — e.g. the
	// agent record was deleted between Authorize and the post-auth
	// agent-token check, the resolver's ScriptSessionCache wiring is
	// broken, or any other internal failure that prevents the
	// request from reaching upstream. Normal happy-path requests
	// (and per-request fast-fail denies that did reach the resolver)
	// should call RecordBytes instead, which only releases the byte
	// reservation — the use was consumed.
	//
	// No-op when the token is unknown.
	ReleaseAuthorize(ctx context.Context, token string) error

	// Revoke marks the session unusable. Future Authorize calls
	// return ErrScriptSessionNotFound. No-op when the token is
	// unknown.
	Revoke(ctx context.Context, token string) error
}

// ScriptSession is the immutable record stored alongside a token. The
// mutable counters (use count, bytes consumed) are kept on the cache
// implementation; callers receive a snapshot copy on Account.
type ScriptSession struct {
	ID              string
	UserID          string
	AgentID         string
	TaskID          string
	Placeholder     string
	ServiceID       string
	TargetHost      string
	Methods         []string
	PathPrefixes    []string
	MaxUses         int
	UsedCount       int
	MaxRequestBytes int64
	MaxTotalBytes   int64
	TotalBytesUsed  int64
	Why             string
	ExpiresAt       time.Time
}

// ScriptSessionRequest is the inbound shape evaluated by Authorize.
type ScriptSessionRequest struct {
	Host        string
	Method      string
	Path        string
	Placeholder string
}

var (
	// ErrScriptSessionNotFound — token unknown or revoked.
	ErrScriptSessionNotFound = errors.New("llmproxy: script session not found")

	// ErrScriptSessionExpired — TTL elapsed.
	ErrScriptSessionExpired = errors.New("llmproxy: script session expired")

	// ErrScriptSessionExhausted — max_uses already reached.
	ErrScriptSessionExhausted = errors.New("llmproxy: script session exhausted")

	// ErrScriptSessionScopeMismatch — host/method/path/placeholder is
	// outside the session's bound capability. Always wrapped in a
	// ScopeMismatchDetail so callers can extract the exact offending
	// field via errors.As; errors.Is(err, ErrScriptSessionScopeMismatch)
	// still works via ScopeMismatchDetail.Is.
	ErrScriptSessionScopeMismatch = errors.New("llmproxy: script session scope mismatch")

	// ErrScriptSessionBytesExceeded — aggregate response byte cap reached.
	ErrScriptSessionBytesExceeded = errors.New("llmproxy: script session bytes exceeded")
)

// ScopeMismatchDetail names the exact field that didn't match between
// an inbound request and a bound script session, and exposes BOTH the
// inbound value and the session's bound value for that field. Callers
// (the middleware that surfaces errors to the agent) use this to emit
// a continuation pointing at the precise gap — generic "scope
// mismatch" messages drove agents into multi-turn debugging loops
// because they couldn't tell whether the host, the method, the path,
// or the placeholder was wrong.
//
// Field is one of: "host" | "method" | "path" | "placeholder".
//
// Got is what the inbound request carried. Expected describes the
// session's bound value(s): a single host for "host", the methods list
// for "method", the path-prefix list for "path", or the bound
// placeholder for "placeholder". For "placeholder", Got may be empty
// when no autovault placeholder was found in the request at all.
type ScopeMismatchDetail struct {
	Field    string
	Got      string
	Expected []string
}

// Error implements error.
func (e *ScopeMismatchDetail) Error() string {
	if e == nil {
		return "llmproxy: script session scope mismatch"
	}
	switch e.Field {
	case "host":
		expected := ""
		if len(e.Expected) > 0 {
			expected = e.Expected[0]
		}
		return fmt.Sprintf("llmproxy: script session scope mismatch: target host %q is not the session's bound host %q", e.Got, expected)
	case "method":
		return fmt.Sprintf("llmproxy: script session scope mismatch: method %q is not in session's allowed methods %v", e.Got, e.Expected)
	case "path":
		return fmt.Sprintf("llmproxy: script session scope mismatch: path %q is not under any of the session's path_prefixes %v", e.Got, e.Expected)
	case "placeholder":
		expected := ""
		if len(e.Expected) > 0 {
			expected = e.Expected[0]
		}
		if e.Got == "" {
			return fmt.Sprintf("llmproxy: script session scope mismatch: no autovault placeholder found in the request; session is bound to %q", expected)
		}
		return fmt.Sprintf("llmproxy: script session scope mismatch: placeholder %q is not the session's bound placeholder %q", e.Got, expected)
	default:
		return "llmproxy: script session scope mismatch"
	}
}

// Is implements errors.Is so callers using
// errors.Is(err, ErrScriptSessionScopeMismatch) continue to match.
func (e *ScopeMismatchDetail) Is(target error) bool {
	return target == ErrScriptSessionScopeMismatch
}

// AgentGuidance formats the detail as a one-sentence continuation
// message the agent can act on. The message names the offending
// field, the value the request carried, and the session's bound
// value(s) — enough for the agent to either correct the call shape
// or re-mint with a wider scope. Generic "scope mismatch" messages
// caused multi-turn debugging loops where agents retried with the
// wrong scope axis.
//
// Lives on the type (rather than in middleware) so the per-field
// formatting is canonical: Error() is the operator-facing form,
// AgentGuidance() is the agent-facing form, and both stay in lockstep
// when a new field is added.
func (e *ScopeMismatchDetail) AgentGuidance() string {
	if e == nil {
		return "request host/method/path/placeholder is outside the session's approved scope"
	}
	switch e.Field {
	case "host":
		expected := ""
		if len(e.Expected) > 0 {
			expected = e.Expected[0]
		}
		return fmt.Sprintf("target host mismatch: your X-Clawvisor-Target-Host (%q) doesn't match the session's bound host (%q). Either send the request with X-Clawvisor-Target-Host: %s, or mint a new session whose target_host covers the host you actually want to call.", e.Got, expected, expected)
	case "method":
		return fmt.Sprintf("method mismatch: request used %s, session allows [%s]. Either reshape this call to one of the allowed methods, or mint a new session whose `methods` include %s.", e.Got, quotedJoin(e.Expected), e.Got)
	case "path":
		return fmt.Sprintf("path mismatch: request path %q is not under any of the session's path_prefixes [%s]. Mint a new session whose path_prefixes covers %q (use a broader prefix like the parent directory when the fan-out will hit multiple sub-paths).", e.Got, quotedJoin(e.Expected), e.Got)
	case "placeholder":
		expected := ""
		if len(e.Expected) > 0 {
			expected = e.Expected[0]
		}
		if e.Got == "" {
			return fmt.Sprintf("placeholder missing: no autovault placeholder found in the request's Authorization (or X-Api-Key) header. The session is bound to %s — include it as `Authorization: Bearer %s`.", expected, expected)
		}
		return fmt.Sprintf("placeholder mismatch: request carried %s, session is bound to %s. Use the placeholder returned at mint time, not a different one.", e.Got, expected)
	default:
		return "request host/method/path/placeholder is outside the session's approved scope"
	}
}

// quotedJoin renders a string slice as `"a", "b", "c"` for inclusion
// in agent-facing messages. Reads more naturally than Go's default
// `[a b c]` and matches the rest of AgentGuidance's `%q`-style
// quoting.
func quotedJoin(s []string) string {
	if len(s) == 0 {
		return ""
	}
	var b strings.Builder
	for i, v := range s {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", v)
	}
	return b.String()
}

// generateScriptSessionToken returns a fresh token. 16 bytes of randomness
// encoded with unpadded base32 (lowercase) — 26 chars after the prefix.
func generateScriptSessionToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])
	return ScriptSessionPrefix + strings.ToLower(enc), nil
}

// normalizeScriptSessionRequest applies the same canonicalization as
// nonceTarget so port-bearing target hosts compare equal to the bare
// host stored on the session.
func normalizeScriptSessionRequest(req ScriptSessionRequest) ScriptSessionRequest {
	host := strings.ToLower(strings.TrimSpace(req.Host))
	if hostOnly, _, err := net.SplitHostPort(host); err == nil {
		host = hostOnly
	}
	req.Host = host
	req.Method = strings.ToUpper(strings.TrimSpace(req.Method))
	p := strings.TrimSpace(req.Path)
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	if i := strings.IndexByte(p, '#'); i >= 0 {
		p = p[:i]
	}
	if p == "" {
		p = "/"
	}
	req.Path = p
	req.Placeholder = strings.TrimSpace(req.Placeholder)
	return req
}

// normalizeScriptSession canonicalizes the host (port stripped, lowercase)
// and methods (uppercase) so Account's comparisons are deterministic.
// PathPrefixes are assumed already canonicalized by the mint endpoint
// via NormalizeScriptSessionPathPrefix; we don't re-clean here so the
// Mint helper can reject malformed input before persistence.
func normalizeScriptSession(sess ScriptSession) ScriptSession {
	sess.TargetHost = strings.ToLower(strings.TrimSpace(sess.TargetHost))
	if hostOnly, _, err := net.SplitHostPort(sess.TargetHost); err == nil {
		sess.TargetHost = hostOnly
	}
	methods := make([]string, 0, len(sess.Methods))
	seen := map[string]struct{}{}
	for _, m := range sess.Methods {
		m = strings.ToUpper(strings.TrimSpace(m))
		if m == "" {
			continue
		}
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		methods = append(methods, m)
	}
	sess.Methods = methods
	sess.Placeholder = strings.TrimSpace(sess.Placeholder)
	return sess
}

// methodAllowed reports whether m is in the session's allowed methods.
func (s ScriptSession) methodAllowed(m string) bool {
	for _, allowed := range s.Methods {
		if allowed == m {
			return true
		}
	}
	return false
}

// pathAllowed reports whether path matches any of the session's
// approved prefixes per ScriptSessionPathPrefixMatch.
func (s ScriptSession) pathAllowed(path string) bool {
	for _, prefix := range s.PathPrefixes {
		if ScriptSessionPathPrefixMatch(prefix, path) {
			return true
		}
	}
	return false
}

// ── In-memory implementation ─────────────────────────────────────────────────

// MemoryScriptSessionCache is the default implementation when no Redis
// is configured. Single-process; for multi-instance deployments use the
// Redis impl so a mint on instance A can be accounted on instance B.
type MemoryScriptSessionCache struct {
	mu      sync.Mutex
	entries map[string]*memoryScriptSessionEntry
	now     func() time.Time
}

type memoryScriptSessionEntry struct {
	sess ScriptSession
}

// NewMemoryScriptSessionCache returns an empty in-memory script-session cache.
func NewMemoryScriptSessionCache() *MemoryScriptSessionCache {
	return &MemoryScriptSessionCache{
		entries: make(map[string]*memoryScriptSessionEntry),
		now:     time.Now,
	}
}

// Mint implements ScriptSessionCache.
func (c *MemoryScriptSessionCache) Mint(_ context.Context, sess ScriptSession) (string, error) {
	token, err := generateScriptSessionToken()
	if err != nil {
		return "", err
	}
	sess = normalizeScriptSession(sess)
	c.mu.Lock()
	c.entries[token] = &memoryScriptSessionEntry{sess: sess}
	c.sweepExpiredLocked()
	c.mu.Unlock()
	return token, nil
}

func (c *MemoryScriptSessionCache) sweepExpiredLocked() {
	now := c.now()
	for k, e := range c.entries {
		if !e.sess.ExpiresAt.IsZero() && now.After(e.sess.ExpiresAt) {
			delete(c.entries, k)
		}
	}
}

// Authorize implements ScriptSessionCache.
func (c *MemoryScriptSessionCache) Authorize(_ context.Context, token string, req ScriptSessionRequest) (ScriptSession, error) {
	req = normalizeScriptSessionRequest(req)
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[token]
	if !ok {
		return ScriptSession{}, ErrScriptSessionNotFound
	}
	if !entry.sess.ExpiresAt.IsZero() && c.now().After(entry.sess.ExpiresAt) {
		delete(c.entries, token)
		return ScriptSession{}, ErrScriptSessionExpired
	}
	sess := entry.sess
	if sess.TargetHost != req.Host {
		return ScriptSession{}, &ScopeMismatchDetail{
			Field: "host", Got: req.Host, Expected: []string{sess.TargetHost},
		}
	}
	if !sess.methodAllowed(req.Method) {
		return ScriptSession{}, &ScopeMismatchDetail{
			Field: "method", Got: req.Method, Expected: append([]string{}, sess.Methods...),
		}
	}
	if !sess.pathAllowed(req.Path) {
		return ScriptSession{}, &ScopeMismatchDetail{
			Field: "path", Got: req.Path, Expected: append([]string{}, sess.PathPrefixes...),
		}
	}
	// Placeholder binding is strict: req.Placeholder must be present
	// AND exactly equal to the session's bound placeholder. An empty
	// req.Placeholder used to skip this check, so a script that put
	// the autovault_… token in Basic auth or any header the middleware
	// doesn't scan reached the resolver unbound — and the resolver's
	// generic header swap would happily replace a sibling placeholder
	// on the same host. Closing the gap here means an off-header
	// placeholder fails fast with SCOPE_MISMATCH at auth time.
	if req.Placeholder == "" || req.Placeholder != sess.Placeholder {
		return ScriptSession{}, &ScopeMismatchDetail{
			Field: "placeholder", Got: req.Placeholder, Expected: []string{sess.Placeholder},
		}
	}
	if sess.MaxUses > 0 && entry.sess.UsedCount >= sess.MaxUses {
		return ScriptSession{}, ErrScriptSessionExhausted
	}
	// Aggregate byte cap is enforced ALSO on the next Authorize, not
	// only post-response in RecordBytes. Without this gate a 10 MiB-
	// exceeded session could keep accepting requests until MaxUses
	// burned down — the per-response RecordBytes signal would only
	// truncate the in-flight body, not prevent the next call.
	if sess.MaxTotalBytes > 0 && entry.sess.TotalBytesUsed >= sess.MaxTotalBytes {
		return ScriptSession{}, ErrScriptSessionBytesExceeded
	}
	// Concurrent inflight reservation: when both per-request and
	// aggregate caps are configured, optimistically reserve the
	// per-request worst case at Authorize time so N concurrent
	// requests don't all read the same TotalBytesUsed snapshot, all
	// pass the gate, and collectively stream up to N × MaxRequestBytes
	// past the aggregate cap before any of them call RecordBytes.
	// RecordBytes trues up the difference between the reservation and
	// the actual bytes streamed; if the reservation would itself push
	// past the cap, the request is denied here. Sessions without a
	// per-request cap fall through to the legacy snapshot semantics
	// (no reservation possible without an upper bound).
	if sess.MaxRequestBytes > 0 && sess.MaxTotalBytes > 0 {
		if entry.sess.TotalBytesUsed+sess.MaxRequestBytes > sess.MaxTotalBytes {
			return ScriptSession{}, ErrScriptSessionBytesExceeded
		}
		entry.sess.TotalBytesUsed += sess.MaxRequestBytes
	}
	entry.sess.UsedCount++
	return entry.sess, nil
}

// RecordBytes implements ScriptSessionCache.
//
// MUST always be called once per Authorize call — including on early-
// exit paths that never streamed anything (bytes == 0). The reservation
// model in Authorize charges MaxRequestBytes against TotalBytesUsed
// upfront; without a corresponding RecordBytes release, every fast-
// failure (placeholder mismatch, upstream timeout, body-too-large,
// middleware-rejected post-Authorize, etc.) leaks the full reservation
// and after ~10 attempts the session's aggregate budget is exhausted
// even though zero bytes were streamed. The bytes == 0 case is the
// release-only signal.
func (c *MemoryScriptSessionCache) RecordBytes(_ context.Context, token string, bytes int64) (ScriptSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[token]
	if !ok {
		return ScriptSession{}, nil
	}
	switch {
	case entry.sess.MaxRequestBytes > 0 && entry.sess.MaxTotalBytes > 0:
		// Reservation path: Authorize charged MaxRequestBytes against
		// TotalBytesUsed. True up by removing the over-reservation
		// (MaxRequestBytes - actualBytes). When bytes == 0 we release
		// the full reservation; when bytes == MaxRequestBytes we
		// release nothing (full usage); the common middle case is
		// somewhere in between.
		//
		// overReservation < 0 means the upstream returned more bytes
		// than the per-request cap allowed — shouldn't happen given
		// the resolver's streamCap, but if it does, we add the
		// overage so the counter still reflects reality.
		overReservation := entry.sess.MaxRequestBytes - bytes
		entry.sess.TotalBytesUsed -= overReservation
		if entry.sess.TotalBytesUsed < 0 {
			entry.sess.TotalBytesUsed = 0
		}
	case bytes > 0:
		// Legacy snapshot path (no per-request cap, so no reservation
		// happened): just add the actual bytes. TotalBytesUsed is
		// monotonically non-decreasing here; subsequent Authorize
		// calls reject with ErrScriptSessionBytesExceeded once the
		// cap is crossed.
		entry.sess.TotalBytesUsed += bytes
	default:
		// Legacy path with bytes == 0: no-op. There's nothing to
		// release (no reservation) and nothing to add.
	}
	if entry.sess.MaxTotalBytes > 0 && entry.sess.TotalBytesUsed > entry.sess.MaxTotalBytes {
		return entry.sess, ErrScriptSessionBytesExceeded
	}
	return entry.sess, nil
}

// ReleaseAuthorize implements ScriptSessionCache.
func (c *MemoryScriptSessionCache) ReleaseAuthorize(_ context.Context, token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[token]
	if !ok {
		return nil
	}
	if entry.sess.MaxRequestBytes > 0 && entry.sess.MaxTotalBytes > 0 {
		entry.sess.TotalBytesUsed -= entry.sess.MaxRequestBytes
		if entry.sess.TotalBytesUsed < 0 {
			entry.sess.TotalBytesUsed = 0
		}
	}
	if entry.sess.UsedCount > 0 {
		entry.sess.UsedCount--
	}
	return nil
}

// Revoke implements ScriptSessionCache.
//
// Drops the entry immediately rather than just flipping `revoked = true`
// and waiting for the next sweep — there's no reason to keep a revoked
// session in the map until its TTL elapses (could be 120s). Authorize
// already short-circuits on entries that aren't present, so deletion is
// indistinguishable from the revoked-flag behavior to callers.
func (c *MemoryScriptSessionCache) Revoke(_ context.Context, token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, token)
	return nil
}

// Len returns the current entry count. Tests / observability.
func (c *MemoryScriptSessionCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

var _ ScriptSessionCache = (*MemoryScriptSessionCache)(nil)
