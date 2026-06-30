// Package intent provides LLM-powered intent verification for gateway requests.
// It verifies that request parameters are consistent with the approved task scope
// and that the agent's stated reason is coherent with the task purpose.
package intent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// VerificationVerdict is the result of intent verification.
type VerificationVerdict struct {
	Allow              bool     `json:"allow"`
	ParamScope         string   `json:"param_scope"`      // "ok" | "violation" | "n/a"
	ReasonCoherence    string   `json:"reason_coherence"` // "ok" | "incoherent" | "insufficient"
	ExtractContext     bool     `json:"extract_context"`
	MissingChainValues []string `json:"missing_chain_values"` // entities the LLM flagged as absent from chain context
	Explanation        string   `json:"explanation"`
	Model              string   `json:"model"`
	LatencyMS          int      `json:"latency_ms"`
	Cached             bool     `json:"cached"`
}

// VerifyRequest contains the data needed for intent verification.
type VerifyRequest struct {
	TaskPurpose         string
	ExpectedUse         string // from task's authorized_actions; empty → check params against reason only
	ExpansionRationale  string // from approved scope expansion; empty if action was in original task
	Service             string
	Action              string
	Params              map[string]any
	Reason              string
	TaskID              string // cache key component
	ServiceHints        string // adapter-provided verification guidance; empty for most adapters
	ChainFacts          []store.ChainFact
	ChainContextOptOut  bool // standing task without session_id — agent bypassed chain context
	ChainContextEnabled bool // chain context tracking is enabled in config
	Lenient             bool // use lenient verification prompt (give agent benefit of the doubt)
	ProxyLite           bool // include proxy-lite-specific verifier guidance

	// OrgID identifies the org context this verification runs under.
	// Used to scope cached verdicts so per-org prompt overrides and
	// task guidance can't cross-contaminate caches. Empty for non-
	// org-scoped requests (the open-source build, admin sessions).
	OrgID string

	// PromptOverride, when non-empty, replaces the default system
	// verification prompt for this request. Sourced from the cloud
	// repo's org_prompt_override table by the caller (the cloud
	// gateway resolves this via LoadPolicySnapshot before constructing
	// the request). The cache key includes a hash of this string so
	// orgs with different overrides do not share cached verdicts.
	PromptOverride string

	// TaskGuidance, when non-empty, is appended to the SYSTEM prompt
	// (NOT the user message — mixing org policy with agent-controlled
	// content opens prompt-injection). Sourced from org_task_policy
	// in the cloud repo. Cache-keyed alongside PromptOverride.
	TaskGuidance string
}

// Verifier checks whether a gateway request is consistent with the approved task.
type Verifier interface {
	Verify(ctx context.Context, req VerifyRequest) (*VerificationVerdict, error)
}

// NoopVerifier returns nil (verification not configured). The gateway treats
// nil verdict as "no verification performed — proceed".
type NoopVerifier struct{}

func (NoopVerifier) Verify(_ context.Context, _ VerifyRequest) (*VerificationVerdict, error) {
	return nil, nil
}

// PromptResolverFn returns the per-org system-prompt override (or "")
// and task guidance (or "") for an org. Wired in by the cloud package
// (cmd/cloud) so the verifier can resolve governance overrides without
// the gateway handlers needing to thread the cloud store through.
// When unset or returning both empty strings, the verifier uses the
// system default prompt with no addendum (existing behavior).
type PromptResolverFn func(ctx context.Context, orgID string) (override, guidance string)

// LLMVerifier performs intent verification via an LLM provider.
type LLMVerifier struct {
	health   *llm.Health
	logger   *slog.Logger
	cache    VerdictCacher
	resolver PromptResolverFn

	geminiMu sync.RWMutex
	// geminiCaches holds one binding per prompt variant — the strict base
	// prompt and the proxy-lite variant (base + addendum) are cached as
	// separate Vertex cachedContents resources because Gemini drops
	// systemInstruction when cachedContent is set, so an addendum cannot
	// be appended to a cached base prompt at request time. Verify() picks
	// the binding that matches the request's variant; missing entries
	// fall through to the uncached path.
	geminiCaches    map[promptVariant]geminiCacheBinding
	geminiCacheMgrs []*llm.GeminiCacheManager
}

// geminiCacheBinding pairs a cache-name accessor with its invalidator for
// one prompt variant.
type geminiCacheBinding struct {
	nameFn      func() string
	invalidator func(string)
}

// NewLLMVerifier creates an LLM-backed intent verifier.
// It reads its config from health on each call, so runtime config updates
// take effect immediately.
func NewLLMVerifier(health *llm.Health, logger *slog.Logger) *LLMVerifier {
	cfg := health.VerificationConfig()
	ttl := time.Duration(cfg.CacheTTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &LLMVerifier{
		health: health,
		logger: logger,
		cache:  newVerdictCache(ttl),
	}
}

// SetVerdictCache overrides the default in-memory verdict cache.
func (v *LLMVerifier) SetVerdictCache(c VerdictCacher) {
	v.cache = c
}

// SetPromptResolver wires the per-org governance resolver. When set
// and a VerifyRequest carries a non-empty OrgID, the verifier consults
// the resolver for an override prompt and task guidance before
// building the cache key and calling the LLM. Callbacks returning
// (empty, empty) preserve the default behavior.
func (v *LLMVerifier) SetPromptResolver(r PromptResolverFn) {
	v.resolver = r
}

// RunCleanup periodically calls the verdict cache's Cleanup hook so expired
// entries don't accumulate forever in long-running processes. Without this
// the cache only evicts on Get of an expired key, so cold keys leak memory.
func (v *LLMVerifier) RunCleanup(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if v.cache != nil {
				v.cache.Cleanup()
			}
		}
	}
}

// StartGeminiCache initializes a Gemini explicit context cache for each
// verifier prompt variant (strict + proxy-lite) and registers them so
// per-request clients reference the right one automatically based on
// the request's ProxyLite flag. cfg.SystemPrompt is filled in by the
// verifier and should be left empty by callers.
//
// Each variant is started independently: a failure to create one cache
// is logged and that variant degrades to inline system prompts, but the
// other variant still benefits from caching. The first hard error
// encountered (if any) is returned for visibility, but the verifier is
// always functional regardless.
func (v *LLMVerifier) StartGeminiCache(ctx context.Context, cfg llm.GeminiCacheManagerConfig) error {
	logger := v.logger
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Logger == nil {
		cfg.Logger = logger
	}
	newCaches := make(map[promptVariant]geminiCacheBinding, 2)
	newMgrs := make([]*llm.GeminiCacheManager, 0, 2)
	variants := []struct {
		variant promptVariant
		prompt  string
	}{
		{variantStrict, verificationSystemPromptFor(false)},
		{variantProxyLite, verificationSystemPromptFor(true)},
	}
	var firstErr error
	for _, vr := range variants {
		mgr, nameFn, invalidator, err := llm.StartCachedSystemPrompt(ctx, cfg, vr.prompt)
		if err != nil {
			logger.WarnContext(ctx, "verifier gemini cache start failed for variant; running uncached for this variant",
				"variant", vr.variant.String(), "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("verifier gemini cache (%s): %w", vr.variant.String(), err)
			}
			continue
		}
		newMgrs = append(newMgrs, mgr)
		newCaches[vr.variant] = geminiCacheBinding{nameFn: nameFn, invalidator: invalidator}
	}

	v.geminiMu.Lock()
	oldMgrs := v.geminiCacheMgrs
	v.geminiCaches = newCaches
	v.geminiCacheMgrs = newMgrs
	v.geminiMu.Unlock()

	stopGeminiCacheManagers(oldMgrs)
	return firstErr
}

// StopGeminiCache stops any verifier-owned Gemini cache managers and clears
// their bindings. It is safe to call multiple times.
func (v *LLMVerifier) StopGeminiCache(ctx context.Context) {
	v.geminiMu.Lock()
	mgrs := v.geminiCacheMgrs
	v.geminiCacheMgrs = nil
	v.geminiCaches = nil
	v.geminiMu.Unlock()

	stopGeminiCacheManagersWithContext(ctx, mgrs)
}

func stopGeminiCacheManagers(mgrs []*llm.GeminiCacheManager) {
	if len(mgrs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stopGeminiCacheManagersWithContext(ctx, mgrs)
}

func stopGeminiCacheManagersWithContext(ctx context.Context, mgrs []*llm.GeminiCacheManager) {
	for _, mgr := range mgrs {
		if mgr != nil {
			mgr.Stop(ctx)
		}
	}
}

func (v *LLMVerifier) geminiCacheBinding(req VerifyRequest) (geminiCacheBinding, bool) {
	v.geminiMu.RLock()
	defer v.geminiMu.RUnlock()
	binding, ok := v.geminiCaches[variantForRequest(req)]
	return binding, ok
}

func (v *LLMVerifier) Verify(ctx context.Context, req VerifyRequest) (*VerificationVerdict, error) {
	cfg := v.health.VerificationConfig()
	if !cfg.Enabled {
		return nil, nil
	}

	// Resolve per-org governance overrides before keying the cache.
	// Callers may also pre-populate these fields; the resolver only
	// fills empty fields, so a caller-provided override wins.
	if v.resolver != nil && req.OrgID != "" {
		override, guidance := v.resolver(ctx, req.OrgID)
		if req.PromptOverride == "" {
			req.PromptOverride = override
		}
		if req.TaskGuidance == "" {
			req.TaskGuidance = guidance
		}
	}

	key := buildCacheKey(req)
	if cached, ok := v.cache.Get(key); ok {
		cached.Cached = true
		return cached, nil
	}

	start := time.Now()

	client := llm.NewClient(cfg.LLMProviderConfig)
	// Lenient mode appends a per-call addendum that isn't pre-cached, so it
	// always inlines the system prompt. Proxy-lite has its own cached
	// variant; pick the binding that matches the request.
	//
	// Per-org PromptOverride also bypasses Gemini cachedContents because
	// the cache binding was prepared at startup for the strict + proxy-
	// lite variants — an overridden prompt is a different binding the
	// service hasn't registered. The verdict is still cached locally
	// (the cache key includes the override hash), so the upstream LLM
	// cost regression is bounded to orgs that opt into overrides.
	//
	// TaskGuidance ALSO bypasses cachedContents: Gemini drops
	// systemInstruction when cachedContent is set, so an appended
	// guidance addendum on the system prompt would be silently dropped
	// at request time — the per-org policy would never reach the
	// model. Bypass the cache path so the inline system prompt (with
	// guidance) is what Gemini actually sees.
	usingOverride := req.PromptOverride != ""
	usingGuidance := req.TaskGuidance != ""
	if !req.Lenient && !usingOverride && !usingGuidance {
		if binding, ok := v.geminiCacheBinding(req); ok && binding.nameFn != nil {
			client.AttachGeminiCacheNameFn(binding.nameFn)
			if binding.invalidator != nil {
				client.AttachGeminiCacheInvalidator(binding.invalidator)
			}
		}
	}
	if v.logger != nil {
		client = client.WithLogger(v.logger)
	}
	userMsg := buildVerificationUserMessage(req)
	var systemPrompt string
	if usingOverride {
		systemPrompt = req.PromptOverride
	} else {
		systemPrompt = verificationSystemPromptFor(req.ProxyLite)
	}
	if req.Lenient {
		systemPrompt += lenientAddendum
	}
	// Org-specific natural-language task guidance is appended to the
	// SYSTEM prompt (NOT the user message — see security note on
	// VerifyRequest.TaskGuidance: the user role contains agent-supplied
	// content and mixing org policy there enables prompt-injection).
	if req.TaskGuidance != "" {
		systemPrompt += "\n\n## Org-specific guidance\n" + req.TaskGuidance
	}
	messages := []llm.ChatMessage{
		{Role: "system", Content: systemPrompt, CacheControl: true},
		{Role: "user", Content: userMsg},
	}

	var lastErr error
	for attempt := range 2 {
		raw, usage, err := client.CompleteWithUsage(ctx, messages)
		if err != nil {
			lastErr = err
			if errors.Is(err, llm.ErrSpendCapExhausted) {
				v.health.SetSpendCapExhausted()
				break // no point retrying a spend cap error
			}
			if attempt == 0 {
				// Jittered backoff: 2s ± 1s (uniform [1s, 3s]).
				jitter := time.Duration(1000+rand.IntN(2001)) * time.Millisecond
				t := time.NewTimer(jitter)
				select {
				case <-t.C:
				case <-ctx.Done():
					t.Stop()
					lastErr = ctx.Err()
					break // breaks select; next Complete call will fail fast on cancelled ctx
				}
				continue
			}
			break
		}

		verdict, parseErr := parseVerificationResponse(raw)
		if parseErr != nil {
			lastErr = parseErr
			if attempt == 0 {
				continue
			}
			break
		}

		verdict.Model = cfg.Model
		verdict.LatencyMS = int(time.Since(start).Milliseconds())
		verdict.Cached = false

		llm.LogUsage(v.logger, "intent_verification", cfg.Model, usage)

		v.cache.Put(key, verdict)
		return verdict, nil
	}

	v.logger.WarnContext(ctx, "intent verification failed after retry",
		"error", lastErr,
		"service", req.Service,
		"action", req.Action,
		"task_id", req.TaskID,
		"fail_closed", cfg.FailClosed,
	)
	if !cfg.FailClosed {
		// Fail open: degrade to "no verification performed" so the request is
		// not blocked on LLM availability. The gateway treats nil verdict the
		// same as the NoopVerifier ‒ proceed without a verification check.
		return nil, nil
	}
	return &VerificationVerdict{
		Allow:           false,
		ParamScope:      "n/a",
		ReasonCoherence: "n/a",
		Explanation:     "Verification failed after retry: " + lastErr.Error(),
		Model:           cfg.Model,
		LatencyMS:       int(time.Since(start).Milliseconds()),
	}, nil
}

// MarshalVerdict marshals a verdict to JSON for storage in the audit log.
func MarshalVerdict(v *VerificationVerdict) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
