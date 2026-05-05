package llm

import "log/slog"

// LogUsage emits a structured log line describing token usage for a single
// completion call. When usage is nil (e.g., the provider didn't return a usage
// block) it logs nothing. The cache_read_input_tokens field is the key signal
// for whether prompt caching is firing — non-zero means a cache hit.
func LogUsage(logger *slog.Logger, op, model string, u *Usage) {
	if logger == nil || u == nil {
		return
	}
	logger.Info("llm usage",
		"op", op,
		"model", model,
		"input_tokens", u.InputTokens,
		"output_tokens", u.OutputTokens,
		"cache_creation_input_tokens", u.CacheCreationInputTokens,
		"cache_read_input_tokens", u.CacheReadInputTokens,
	)
}
