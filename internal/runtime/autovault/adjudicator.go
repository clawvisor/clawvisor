package autovault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/pkg/config"
)

const DefaultSecretAdjudicationTimeout = 10 * time.Second

var ErrSecretAdjudicatorDisabled = errors.New("secret adjudicator disabled")

type SecretAdjudicationVerdict struct {
	Credential bool    `json:"credential"`
	Service    string  `json:"service"`
	Confidence float64 `json:"confidence"`
}

type SecretAdjudicationRequest struct {
	Host      string
	FieldName string
	Content   string
	Candidate Candidate
}

type SecretAdjudicationResult struct {
	Verdict  SecretAdjudicationVerdict
	Raw      string
	Duration time.Duration
}

type SecretAdjudicator interface {
	AdjudicateSecret(ctx context.Context, req SecretAdjudicationRequest) (SecretAdjudicationResult, error)
}

type LLMSecretAdjudicator struct {
	ConfigFn  func() config.VerificationConfig
	Logger    *slog.Logger
	MaxTokens int
}

func NewLLMSecretAdjudicator(configFn func() config.VerificationConfig, logger *slog.Logger) *LLMSecretAdjudicator {
	return &LLMSecretAdjudicator{ConfigFn: configFn, Logger: logger, MaxTokens: 250}
}

func (a *LLMSecretAdjudicator) AdjudicateSecret(ctx context.Context, req SecretAdjudicationRequest) (SecretAdjudicationResult, error) {
	if a == nil || a.ConfigFn == nil {
		return SecretAdjudicationResult{}, ErrSecretAdjudicatorDisabled
	}
	cfg := a.ConfigFn()
	if !SecretAdjudicatorConfigured(cfg) {
		return SecretAdjudicationResult{}, ErrSecretAdjudicatorDisabled
	}
	timeout := SecretAdjudicationTimeout(cfg)
	adjudicationCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	maxTokens := a.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 250
	}
	client := llm.NewClient(cfg.LLMProviderConfig).WithMaxTokens(maxTokens)
	startedAt := time.Now()
	raw, err := client.Complete(adjudicationCtx, []llm.ChatMessage{
		{Role: "system", Content: SecretAdjudicatorSystemPrompt},
		{Role: "user", Content: BuildSecretAdjudicatorPrompt(req.Host, req.FieldName, req.Content, req.Candidate)},
	})
	result := SecretAdjudicationResult{Raw: raw, Duration: time.Since(startedAt)}
	if err != nil {
		return result, err
	}
	verdict, err := ParseSecretAdjudicatorVerdict(raw)
	if err != nil {
		return result, err
	}
	result.Verdict = verdict
	return result, nil
}

func SecretAdjudicatorConfigured(cfg config.VerificationConfig) bool {
	if !cfg.Enabled || cfg.Model == "" {
		return false
	}
	if cfg.Endpoint != "" {
		return true
	}
	return cfg.Provider == "gemini" && cfg.Project != ""
}

func SecretAdjudicationTimeout(cfg config.VerificationConfig) time.Duration {
	if cfg.TimeoutSeconds > 0 {
		return time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	return DefaultSecretAdjudicationTimeout
}

func BuildSecretAdjudicatorPrompt(host, fieldName, content string, candidate Candidate) string {
	return fmt.Sprintf(`Host: %s
Field: %s
Candidate charset: %s
Candidate entropy: %.2f
Redacted context:
%s

Decide whether <TOKEN_CANDIDATE_1> is a real credential that should be captured for later placeholder swap. Return strict JSON:
{"credential":true|false,"service":"service-name-or-empty","confidence":0.0-1.0}`,
		host,
		fieldName,
		candidate.Charset,
		candidate.Entropy,
		RedactedCandidateContext(content, candidate.Value),
	)
}

func ParseSecretAdjudicatorVerdict(raw string) (SecretAdjudicationVerdict, error) {
	body := ExtractFirstJSONObject(raw)
	if body == "" {
		return SecretAdjudicationVerdict{}, fmt.Errorf("no JSON object found in adjudicator response")
	}
	var verdict SecretAdjudicationVerdict
	if err := json.Unmarshal([]byte(body), &verdict); err != nil {
		return SecretAdjudicationVerdict{}, err
	}
	return verdict, nil
}

// ExtractFirstJSONObject returns the substring spanning the first balanced
// {...} block in s, ignoring braces that appear inside strings. Handles
// markdown-fenced replies, trailing prose, and prefix commentary that the
// adjudicator LLM occasionally emits.
func ExtractFirstJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

const SecretAdjudicatorSystemPrompt = `You classify redacted candidate strings inside LLM conversation requests.

Rules:
- The candidate value is always redacted as <TOKEN_CANDIDATE_1>.
- Decide whether it is likely a real credential or secret that should be captured and replaced with a placeholder.
- Prefer false when the context is weak or the value looks like an ordinary identifier.
- Return strict JSON only:
  {"credential":true|false,"service":"service-name-or-empty","confidence":0.0-1.0}`
