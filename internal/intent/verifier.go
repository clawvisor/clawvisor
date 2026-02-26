// Package intent provides LLM-powered intent verification for gateway requests.
// It verifies that request parameters are consistent with the approved task scope
// and that the agent's stated reason is coherent with the task purpose.
package intent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/clawvisor/clawvisor/internal/config"
	"github.com/clawvisor/clawvisor/internal/llm"
)

// VerificationVerdict is the result of intent verification.
type VerificationVerdict struct {
	Allow           bool   `json:"allow"`
	ParamScope      string `json:"param_scope"`      // "ok" | "violation" | "n/a"
	ReasonCoherence string `json:"reason_coherence"` // "ok" | "incoherent" | "insufficient"
	Explanation     string `json:"explanation"`
	Model           string `json:"model"`
	LatencyMS       int    `json:"latency_ms"`
	Cached          bool   `json:"cached"`
}

// VerifyRequest contains the data needed for intent verification.
type VerifyRequest struct {
	TaskPurpose string
	ExpectedUse string // from task's authorized_actions; empty → check params against reason only
	Service     string
	Action      string
	Params      map[string]any
	Reason      string
	TaskID      string // cache key component
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

// LLMVerifier performs intent verification via an LLM provider.
type LLMVerifier struct {
	cfg   config.VerificationConfig
	cache *verdictCache
}

// NewLLMVerifier creates an LLM-backed intent verifier.
func NewLLMVerifier(cfg config.VerificationConfig) *LLMVerifier {
	ttl := time.Duration(cfg.CacheTTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &LLMVerifier{
		cfg:   cfg,
		cache: newVerdictCache(ttl),
	}
}

func (v *LLMVerifier) Verify(ctx context.Context, req VerifyRequest) (*VerificationVerdict, error) {
	if !v.cfg.Enabled {
		return nil, nil
	}

	key := buildCacheKey(req)
	if cached, ok := v.cache.Get(key); ok {
		cached.Cached = true
		return cached, nil
	}

	start := time.Now()

	client := llm.NewClient(v.cfg.LLMProviderConfig)
	userMsg := buildVerificationUserMessage(req)
	messages := []llm.ChatMessage{
		{Role: "system", Content: verificationSystemPrompt},
		{Role: "user", Content: userMsg},
	}

	var lastErr error
	for attempt := range 2 {
		raw, err := client.Complete(ctx, messages)
		if err != nil {
			lastErr = err
			if attempt == 0 {
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

		verdict.Model = v.cfg.Model
		verdict.LatencyMS = int(time.Since(start).Milliseconds())
		verdict.Cached = false

		v.cache.Put(key, verdict)
		return verdict, nil
	}

	return &VerificationVerdict{
		Allow:           false,
		ParamScope:      "n/a",
		ReasonCoherence: "n/a",
		Explanation:     "Verification failed after retry: " + lastErr.Error(),
		Model:           v.cfg.Model,
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
