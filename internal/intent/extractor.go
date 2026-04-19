package intent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// maxExtractResultLen is the maximum length of the adapter result passed to the LLM.
const maxExtractResultLen = 4096

// maxExtractedFacts caps the number of facts per extraction.
const maxExtractedFacts = 50

// Extractor extracts structural chain facts from adapter results.
type Extractor interface {
	Extract(ctx context.Context, req ExtractRequest) ([]*store.ChainFact, error)
}

// ExtractRequest contains the data needed for chain context extraction.
type ExtractRequest struct {
	TaskPurpose       string
	AuthorizedActions []store.TaskAction
	Service           string
	Action            string
	Result            string // raw adapter result (full; truncated internally for LLM, regexes run against full)
	TaskID            string
	SessionID         string
	AuditID           string
}

// NoopExtractor returns nil (extraction not configured).
type NoopExtractor struct{}

func (NoopExtractor) Extract(_ context.Context, _ ExtractRequest) ([]*store.ChainFact, error) {
	return nil, nil
}

// LLMExtractor uses an LLM to extract structural facts from adapter results.
type LLMExtractor struct {
	health *llm.Health
	logger *slog.Logger
}

// NewLLMExtractor creates an LLM-backed chain context extractor.
func NewLLMExtractor(health *llm.Health, logger *slog.Logger) *LLMExtractor {
	return &LLMExtractor{health: health, logger: logger}
}

const extractionSystemPrompt = `You extract structural references from API results for a security system.

Extract ONLY: IDs, email addresses, phone numbers, person names, amounts, dates, URLs, domains.
NEVER extract: message bodies, file contents, passwords, tokens, API keys, HTML.

The result may be TRUNCATED. Provide regex patterns so we can extract ALL entities from the full result.

Return a JSON object:
- "facts": up to 20 objects with "fact_type" and "fact_value" for entities you see directly.
- "patterns": objects with "fact_type" and "regex" (Go/RE2, one capture group) matching that type's values in the result structure.

fact_type values: email_address, message_id, file_id, phone_number, person_name, thread_id, event_id, etc.

Example:
{"facts":[{"fact_type":"email_address","fact_value":"alice@co.com"}],"patterns":[{"fact_type":"email_address","regex":"\"email\":\\s*\"([^\"]+@[^\"]+)\""},{"fact_type":"message_id","regex":"\"id\":\\s*\"([a-f0-9]{16})\""}]}

Empty result: {"facts":[],"patterns":[]}`

// maxRegexRuntime caps how long we spend running LLM-provided regexes.
const maxRegexRuntime = 2 * time.Second

// maxRegexMatches caps the number of matches per regex pattern to prevent
// runaway extraction from overly broad patterns.
const maxRegexMatches = 200

func (e *LLMExtractor) Extract(ctx context.Context, req ExtractRequest) ([]*store.ChainFact, error) {
	result := req.Result
	truncated := len(result) > maxExtractResultLen
	if truncated {
		result = result[:maxExtractResultLen]
	}

	userMsg := fmt.Sprintf("Service: %s\nAction: %s\n\nResult:\n%s", req.Service, req.Action, result)

	cfg := e.health.ChainContextConfig()
	client := llm.NewClient(cfg.LLMProviderConfig)
	messages := []llm.ChatMessage{
		{Role: "system", Content: extractionSystemPrompt},
		{Role: "user", Content: userMsg},
	}

	raw, err := client.Complete(ctx, messages)
	llmFailed := err != nil
	if llmFailed {
		if errors.Is(err, llm.ErrSpendCapExhausted) {
			e.health.SetSpendCapExhausted()
		}
		e.logger.Warn("chain context extraction LLM call failed, using builtin patterns", "err", err, "task_id", req.TaskID)
	}

	var directFacts []extractedFact
	var patterns []extractionPattern

	if !llmFailed {
		// Parse response — supports both old format (array) and new format (object with facts+patterns).
		raw = strings.TrimSpace(raw)
		raw = strings.TrimPrefix(raw, "```json")
		raw = strings.TrimPrefix(raw, "```")
		raw = strings.TrimSuffix(raw, "```")
		raw = strings.TrimSpace(raw)

		directFacts, patterns = parseExtractionResponse(raw, e.logger, req.TaskID)
	}

	// When the LLM failed or returned no patterns, fall back to builtin
	// patterns for the service so extraction still works without an LLM.
	if len(directFacts) == 0 && len(patterns) == 0 {
		patterns = builtinPatterns(req.Service, req.Action)
		if len(patterns) > 0 {
			e.logger.Debug("using builtin extraction patterns", "task_id", req.TaskID, "service", req.Service, "patterns", len(patterns))
		}
	}

	facts, dropped := mergeExtractionResults(directFacts, patterns, req, e.logger)

	e.logger.Debug("chain context extraction complete",
		"task_id", req.TaskID,
		"direct_facts", len(directFacts)-dropped,
		"dropped", dropped,
		"patterns", len(patterns),
		"total_facts", len(facts),
		"truncated", truncated,
		"llm_failed", llmFailed,
	)

	return facts, nil
}

// mergeExtractionResults validates direct facts against the full result and
// augments them with regex-pattern matches, deduped by (fact_type, fact_value).
//
// Regex always runs when patterns exist. An earlier version gated regex on
// "truncated || llm_failed || no direct facts", but that caused silent
// misses on small list responses: the LLM's 20-direct-fact soft cap could
// omit an ID the response contained, and regex — the only mechanism that
// would have caught it — was skipped because nothing looked wrong. Facts
// are deduped and capped, so there's no double-counting or overflow cost.
func mergeExtractionResults(
	directFacts []extractedFact,
	patterns []extractionPattern,
	req ExtractRequest,
	logger *slog.Logger,
) (facts []*store.ChainFact, dropped int) {
	seen := make(map[string]bool)

	appendFact := func(factType, factValue string) {
		key := factType + "|" + factValue
		if seen[key] {
			return
		}
		seen[key] = true
		facts = append(facts, &store.ChainFact{
			ID:        uuid.New().String(),
			TaskID:    req.TaskID,
			SessionID: req.SessionID,
			AuditID:   req.AuditID,
			Service:   req.Service,
			Action:    req.Action,
			FactType:  factType,
			FactValue: factValue,
		})
	}

	for _, ef := range directFacts {
		if ef.FactType == "" || ef.FactValue == "" {
			dropped++
			continue
		}
		if !substringMatch(req.Result, ef.FactValue, ef.FactType) {
			dropped++
			continue
		}
		appendFact(ef.FactType, ef.FactValue)
	}

	if len(patterns) > 0 {
		regexFacts := runExtractionPatterns(patterns, req.Result, logger, req.TaskID)
		for _, rf := range regexFacts {
			appendFact(rf.factType, rf.factValue)
		}
	}

	if len(facts) > maxExtractedFacts {
		facts = facts[:maxExtractedFacts]
	}
	return facts, dropped
}

type extractedFact struct {
	FactType  string `json:"fact_type"`
	FactValue string `json:"fact_value"`
}

type extractionPattern struct {
	FactType string `json:"fact_type"`
	Regex    string `json:"regex"`
}

// parseExtractionResponse handles both the new object format
// {"facts": [...], "patterns": [...]} and the legacy array format [...].
func parseExtractionResponse(raw string, logger *slog.Logger, taskID string) ([]extractedFact, []extractionPattern) {
	// Try new format first.
	var obj struct {
		Facts    []extractedFact     `json:"facts"`
		Patterns []extractionPattern `json:"patterns"`
	}
	if err := json.Unmarshal([]byte(raw), &obj); err == nil && (len(obj.Facts) > 0 || len(obj.Patterns) > 0) {
		return obj.Facts, obj.Patterns
	}

	// Fall back to legacy array format.
	var arr []extractedFact
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		logger.Warn("chain context extraction parse failed", "err", err, "task_id", taskID)
		return nil, nil
	}
	return arr, nil
}

type regexMatch struct {
	factType  string
	factValue string
}

// runExtractionPatterns compiles and runs LLM-emitted regex patterns against
// the full result. Each pattern must have exactly one capture group.
// Patterns that fail to compile, take too long, or match too broadly are skipped.
func runExtractionPatterns(patterns []extractionPattern, fullResult string, logger *slog.Logger, taskID string) []regexMatch {
	deadline := time.Now().Add(maxRegexRuntime)
	var matches []regexMatch

	for _, p := range patterns {
		if time.Now().After(deadline) {
			logger.Warn("regex extraction deadline exceeded, skipping remaining patterns",
				"task_id", taskID,
			)
			break
		}
		if p.FactType == "" || p.Regex == "" {
			continue
		}

		re, err := regexp.Compile(p.Regex)
		if err != nil {
			logger.Debug("skipping invalid extraction regex",
				"task_id", taskID,
				"fact_type", p.FactType,
				"regex", p.Regex,
				"err", err,
			)
			continue
		}

		found := re.FindAllStringSubmatch(fullResult, maxRegexMatches)
		for _, m := range found {
			// Expect exactly one capture group.
			if len(m) < 2 {
				continue
			}
			val := strings.TrimSpace(m[1])
			if val == "" {
				continue
			}
			matches = append(matches, regexMatch{factType: p.FactType, factValue: val})
		}
	}

	return matches
}

// builtinPatterns returns hardcoded extraction patterns for known service
// result structures. These serve as a fallback when the LLM is unavailable
// (e.g. requests routed through a proxy that rejects extraction prompts).
func builtinPatterns(service, action string) []extractionPattern {
	// Generic patterns that apply to most JSON API responses.
	generic := []extractionPattern{
		{FactType: "email_address", Regex: `"(?:email|emailAddress|from|to|sender|recipient)":\s*"([^"]+@[^"]+)"`},
	}

	svc := strings.SplitN(service, ":", 2)[0] // strip instance suffix

	switch svc {
	case "google.gmail":
		return append(generic,
			extractionPattern{FactType: "message_id", Regex: `"id":\s*"([a-f0-9]{16})"`},
			extractionPattern{FactType: "thread_id", Regex: `"threadId":\s*"([a-f0-9]{16})"`},
			extractionPattern{FactType: "email_address", Regex: `"email":\s*"([^"]+@[^"]+)"`},
		)
	case "google.drive":
		return append(generic,
			extractionPattern{FactType: "file_id", Regex: `"id":\s*"([^"]+)"`},
			extractionPattern{FactType: "email_address", Regex: `"emailAddress":\s*"([^"]+@[^"]+)"`},
		)
	case "google.calendar":
		return append(generic,
			extractionPattern{FactType: "event_id", Regex: `"id":\s*"([^"]+)"`},
			extractionPattern{FactType: "email_address", Regex: `"email":\s*"([^"]+@[^"]+)"`},
		)
	case "google.contacts":
		return append(generic,
			extractionPattern{FactType: "phone_number", Regex: `"value":\s*"(\+?[\d\s\-()]{7,})"`},
			extractionPattern{FactType: "email_address", Regex: `"value":\s*"([^"]+@[^"]+)"`},
		)
	case "github":
		return append(generic,
			extractionPattern{FactType: "issue_id", Regex: `"number":\s*(\d+)`},
			extractionPattern{FactType: "pr_id", Regex: `"number":\s*(\d+)`},
		)
	case "linear":
		return append(generic,
			extractionPattern{FactType: "issue_id", Regex: `"id":\s*"([a-f0-9-]{36})"`},
			extractionPattern{FactType: "issue_id", Regex: `"identifier":\s*"([A-Z]+-\d+)"`},
		)
	case "slack":
		return append(generic,
			extractionPattern{FactType: "channel_id", Regex: `"id":\s*"(C[A-Z0-9]+)"`},
			extractionPattern{FactType: "message_id", Regex: `"ts":\s*"([\d.]+)"`},
		)
	case "stripe":
		return append(generic,
			extractionPattern{FactType: "charge_id", Regex: `"id":\s*"(ch_[a-zA-Z0-9]+)"`},
			extractionPattern{FactType: "customer_id", Regex: `"(?:id|customer)":\s*"(cus_[a-zA-Z0-9]+)"`},
		)
	case "imessage":
		return append(generic,
			extractionPattern{FactType: "message_id", Regex: `"(?:id|guid)":\s*"([^"]+)"`},
			extractionPattern{FactType: "phone_number", Regex: `"(?:handle|sender|recipient)":\s*"(\+?[\d]{10,})"`},
		)
	default:
		// For unknown services, try generic ID and email patterns.
		return append(generic,
			extractionPattern{FactType: "entity_id", Regex: `"id":\s*"([^"]+)"`},
		)
	}
}

// substringMatch checks that factValue appears in result.
// Case-insensitive for email_address and domain fact types; exact match for all others.
func substringMatch(result, factValue, factType string) bool {
	if factType == "email_address" || factType == "domain" {
		return strings.Contains(strings.ToLower(result), strings.ToLower(factValue))
	}
	return strings.Contains(result, factValue)
}
