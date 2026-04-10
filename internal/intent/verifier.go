// Package intent provides LLM-powered intent verification for gateway requests.
// It verifies that request parameters are consistent with the approved task scope
// and that the agent's stated reason is coherent with the task purpose.
package intent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// VerificationVerdict is the result of intent verification.
type VerificationVerdict struct {
	Allow             bool   `json:"allow"`
	ParamScope        string `json:"param_scope"`        // "ok" | "violation" | "n/a"
	ReasonCoherence   string `json:"reason_coherence"`   // "ok" | "incoherent" | "insufficient"
	ExtractContext    bool   `json:"extract_context"`
	MissingChainValues []string `json:"missing_chain_values"` // entities the LLM flagged as absent from chain context
	Explanation       string `json:"explanation"`
	Model             string `json:"model"`
	LatencyMS         int    `json:"latency_ms"`
	Cached            bool   `json:"cached"`
}

// VerifyRequest contains the data needed for intent verification.
type VerifyRequest struct {
	TaskPurpose        string
	ExpectedUse        string // from task's authorized_actions; empty → check params against reason only
	ExpansionRationale string // from approved scope expansion; empty if action was in original task
	Service            string
	Action             string
	Params             map[string]any
	Reason             string
	TaskID             string // cache key component
	ServiceHints       string // adapter-provided verification guidance; empty for most adapters
	ChainFacts           []store.ChainFact
	ChainContextOptOut   bool // standing task without session_id — agent bypassed chain context
	ChainContextEnabled  bool // chain context tracking is enabled in config
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
	health *llm.Health
	logger *slog.Logger
	cache  VerdictCacher
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

func (v *LLMVerifier) Verify(ctx context.Context, req VerifyRequest) (*VerificationVerdict, error) {
	cfg := v.health.VerificationConfig()
	if !cfg.Enabled {
		return nil, nil
	}

	key := buildCacheKey(req)
	if cached, ok := v.cache.Get(key); ok {
		cached.Cached = true
		return cached, nil
	}

	start := time.Now()

	client := llm.NewClient(cfg.LLMProviderConfig)
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
			if errors.Is(err, llm.ErrSpendCapExhausted) {
				v.health.SetSpendCapExhausted()
				break // no point retrying a spend cap error
			}
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

		verdict.Model = cfg.Model
		verdict.LatencyMS = int(time.Since(start).Milliseconds())
		verdict.Cached = false

		v.cache.Put(key, verdict)
		return verdict, nil
	}

	v.logger.Warn("intent verification failed after retry",
		"error", lastErr,
		"service", req.Service,
		"action", req.Action,
		"task_id", req.TaskID,
	)
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
