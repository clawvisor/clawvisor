package llm

import (
	"strings"
	"sync"

	"github.com/clawvisor/clawvisor/pkg/config"
)

// Health tracks the operational status of the LLM subsystem and provides
// a mutable config store that the verifier, assessor, and extractor read from.
type Health struct {
	mu                sync.RWMutex
	spendCapExhausted bool
	llmCfg            config.LLMConfig
}

// NewHealth creates a Health tracker from the initial LLM config.
func NewHealth(cfg config.LLMConfig) *Health {
	return &Health{llmCfg: cfg}
}

// IsHaikuProxy returns true when the current shared API key is a haiku proxy key.
func (h *Health) IsHaikuProxy() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return strings.HasPrefix(h.llmCfg.APIKey, "hkp_")
}

// SpendCapExhausted returns true when the haiku proxy key has hit its spend cap.
func (h *Health) SpendCapExhausted() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.spendCapExhausted
}

// SetSpendCapExhausted marks the key as exhausted.
func (h *Health) SetSpendCapExhausted() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.spendCapExhausted = true
}

// VerificationConfig returns a snapshot of the current verification config.
func (h *Health) VerificationConfig() config.VerificationConfig {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.llmCfg.Verification
}

// TaskRiskConfig returns a snapshot of the current task risk config.
func (h *Health) TaskRiskConfig() config.TaskRiskConfig {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.llmCfg.TaskRisk
}

// ChainContextConfig returns a snapshot of the current chain context config.
func (h *Health) ChainContextConfig() config.ChainContextConfig {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.llmCfg.ChainContext
}

// FeedbackReviewConfig returns a snapshot of the current feedback review config.
func (h *Health) FeedbackReviewConfig() config.FeedbackReviewConfig {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.llmCfg.FeedbackReview
}

// UpdateConfig atomically replaces the LLM config and clears the
// spend-cap-exhausted flag (the new key may have budget remaining).
func (h *Health) UpdateConfig(cfg config.LLMConfig) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.llmCfg = cfg
	h.spendCapExhausted = false
}

// LLMConfig returns a snapshot of the full LLM config.
func (h *Health) LLMConfig() config.LLMConfig {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.llmCfg
}
