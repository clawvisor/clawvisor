// Package safety provides a secondary LLM-based check on formatted adapter results.
// It runs after policy evaluation and before the result is returned to the agent.
// Disabled by default; enabled via config.llm.safety.enabled.
package safety

import (
	"context"
	"strings"

	"github.com/clawvisor/clawvisor/internal/adapters"
	"github.com/clawvisor/clawvisor/internal/config"
	"github.com/clawvisor/clawvisor/internal/gateway"
	"github.com/clawvisor/clawvisor/internal/llm"
)

// SafetyResult is returned by a SafetyChecker.
type SafetyResult struct {
	Safe   bool
	Reason string
}

// SafetyChecker evaluates a formatted adapter result for anomalies.
// It receives the Result after all formatting — never raw API content.
type SafetyChecker interface {
	// Check evaluates the result. Returns Safe=true to allow execution,
	// Safe=false to route to approval. On error, fails safe (treats as unsafe).
	Check(ctx context.Context, userID string, req *gateway.Request, result *adapters.Result) (SafetyResult, error)
}

// NoopChecker always returns safe. Used when safety is disabled.
type NoopChecker struct{}

func (NoopChecker) Check(_ context.Context, _ string, _ *gateway.Request, _ *adapters.Result) (SafetyResult, error) {
	return SafetyResult{Safe: true}, nil
}

// LLMSafetyChecker performs safety checks via an LLM provider.
type LLMSafetyChecker struct {
	cfg config.LLMConfig
}

// NewLLMSafetyChecker creates an LLM-backed safety checker.
func NewLLMSafetyChecker(cfg config.LLMConfig) *LLMSafetyChecker {
	return &LLMSafetyChecker{cfg: cfg}
}

func (c *LLMSafetyChecker) Check(ctx context.Context, _ string, req *gateway.Request, result *adapters.Result) (SafetyResult, error) {
	current := c.cfg.Safety
	if !current.Enabled {
		return SafetyResult{Safe: true}, nil
	}
	if current.SkipReadonly && isReadonlyAction(req.Action) {
		return SafetyResult{Safe: true}, nil
	}

	client := llm.NewClient(current)
	userMsg := BuildSafetyUserMessage(req, result)
	messages := []llm.ChatMessage{
		{Role: "system", Content: SafetySystemPrompt},
		{Role: "user", Content: userMsg},
	}

	raw, err := client.Complete(ctx, messages)
	if err != nil {
		// Fail safe: timeout or provider error → route to approval
		return SafetyResult{Safe: false, Reason: "safety check unavailable: " + err.Error()}, nil
	}
	return ParseSafetyResponse(raw)
}

// isReadonlyAction returns true for read-only action names (get_*, list_*, read_*).
func isReadonlyAction(action string) bool {
	for _, prefix := range []string{"get_", "list_", "read_", "search_", "fetch_"} {
		if strings.HasPrefix(action, prefix) {
			return true
		}
	}
	return false
}
