package llmproxy

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"strings"
	"sync"
	"time"
)

// CallerNonceCache mints + consumes short-lived per-rewrite nonces that
// stand in for the agent's bearer token in the rewritten tool_use. The
// nonce is sent to the resolver via X-Clawvisor-Caller; the resolver
// consumes it (one-shot) to recover the agent context.
//
// Each nonce is bound to a single (agent_id, host, method, path) tuple.
// On Consume the cache validates that the requested (host, method, path)
// match what was minted, and returns the bound agentID. Replaying a
// nonce against a different host/method/path returns
// ErrNonceTargetMismatch. The bound-target check makes a leaked nonce
// significantly less useful than a leaked bearer token: instead of
// granting blanket "act as agent X" authority for the TTL window, it
// only authorizes the specific call the proxy already approved.
type CallerNonceCache interface {
	// Mint generates a new opaque nonce bound to the calling agent for
	// the specific (host, method, path) target. The returned string is
	// the literal value the resolver caller embeds as X-Clawvisor-Caller;
	// it reveals nothing about the agent's actual token.
	Mint(ctx context.Context, agentID string, target NonceTarget) (nonce string, err error)

	// Consume atomically validates and deletes the nonce. Returns the
	// agentID it was bound to, after verifying the target tuple
	// matches. One-shot: a successful Consume invalidates the nonce.
	//
	// Returns ErrNonceNotFound when no such nonce exists (expired,
	// already consumed, or never minted).
	// Returns ErrNonceTargetMismatch when the nonce exists but was
	// minted for a different (host, method, path) tuple.
	Consume(ctx context.Context, nonce string, target NonceTarget) (agentID string, err error)
}

// NonceTarget binds a nonce to the wire-level call shape. The agentID
// is bound separately via Mint's argument and returned via Consume's
// return — keeping them out of NonceTarget avoids the bug where the
// resolver (which doesn't yet know agentID) has to populate it before
// consuming.
type NonceTarget struct {
	Host   string
	Method string
	Path   string
}

var (
	// ErrNonceNotFound is returned when the nonce is unknown — expired,
	// already consumed, or never minted. The resolver returns 401.
	ErrNonceNotFound = errors.New("llmproxy: caller nonce not found")

	// ErrNonceTargetMismatch is returned when the nonce exists but was
	// minted for a different (host, method, path) tuple than the inbound
	// resolver request. This is a misuse signal: a legitimate caller
	// hitting the URL the proxy generated never produces this. Log at
	// WARN level with both target tuples for forensics.
	ErrNonceTargetMismatch = errors.New("llmproxy: caller nonce target mismatch")
)

// NoncePrefix is the leading byte sequence of every nonce produced by
// Mint. Exposed so callers (e.g. the resolver middleware) can recognize
// nonces vs. malformed input quickly and fail with the right error
// shape. The prefix is stable across implementations.
const NoncePrefix = "cv-nonce-"

// normalizeNonceTarget applies the canonicalization rules used on both
// Mint and Consume. Documented in CallerNonceCache so callers know what
// invariants the cache enforces.
func normalizeNonceTarget(t NonceTarget) NonceTarget {
	t.Host = strings.ToLower(strings.TrimSpace(t.Host))
	t.Method = strings.ToUpper(strings.TrimSpace(t.Method))
	// Path: strip query, strip trailing slash (unless root). Keep
	// URL-encoded form verbatim — we don't canonicalize encoding here.
	p := strings.TrimSpace(t.Path)
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	if i := strings.IndexByte(p, '#'); i >= 0 {
		p = p[:i]
	}
	if len(p) > 1 && strings.HasSuffix(p, "/") {
		p = strings.TrimRight(p, "/")
	}
	if p == "" {
		p = "/"
	}
	t.Path = p
	return t
}

// generateNonce returns a fresh nonce string with the NoncePrefix. The
// random portion is 16 bytes (128 bits) encoded with unpadded base32 —
// 26 alphanumeric chars, URL-safe, case-insensitive matchable.
func generateNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])
	return NoncePrefix + strings.ToLower(enc), nil
}

// ── In-memory implementation ─────────────────────────────────────────────────

// MemoryCallerNonceCache is the default implementation when no Redis
// is configured. Suitable for self-host installs that run a single
// daemon process; multi-instance deployments should use the Redis impl
// so a nonce minted on instance A can be consumed on instance B.
type MemoryCallerNonceCache struct {
	mu      sync.Mutex
	entries map[string]memoryNonceEntry
	ttl     time.Duration
	now     func() time.Time
}

type memoryNonceEntry struct {
	agentID   string
	target    NonceTarget
	expiresAt time.Time
}

// NewMemoryCallerNonceCache returns an in-memory cache with the given
// TTL. ttl <= 0 is replaced with 5 minutes.
func NewMemoryCallerNonceCache(ttl time.Duration) *MemoryCallerNonceCache {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &MemoryCallerNonceCache{
		entries: make(map[string]memoryNonceEntry),
		ttl:     ttl,
		now:     time.Now,
	}
}

// Mint implements CallerNonceCache.
func (c *MemoryCallerNonceCache) Mint(ctx context.Context, agentID string, target NonceTarget) (string, error) {
	nonce, err := generateNonce()
	if err != nil {
		return "", err
	}
	target = normalizeNonceTarget(target)
	agentID = strings.TrimSpace(agentID)
	c.mu.Lock()
	c.entries[nonce] = memoryNonceEntry{
		agentID:   agentID,
		target:    target,
		expiresAt: c.now().Add(c.ttl),
	}
	c.mu.Unlock()
	return nonce, nil
}

// Consume implements CallerNonceCache.
func (c *MemoryCallerNonceCache) Consume(ctx context.Context, nonce string, target NonceTarget) (string, error) {
	target = normalizeNonceTarget(target)
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[nonce]
	if !ok {
		return "", ErrNonceNotFound
	}
	// Expired entries are equivalent to not found. Delete so the map
	// doesn't grow indefinitely with expired junk.
	if c.now().After(entry.expiresAt) {
		delete(c.entries, nonce)
		return "", ErrNonceNotFound
	}
	// One-shot: delete the entry before validating target, so an
	// attempted mismatch can't be retried with the same nonce.
	delete(c.entries, nonce)
	if entry.target != target {
		return "", ErrNonceTargetMismatch
	}
	return entry.agentID, nil
}

// Len returns the current entry count. For tests + observability.
func (c *MemoryCallerNonceCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

var _ CallerNonceCache = (*MemoryCallerNonceCache)(nil)
