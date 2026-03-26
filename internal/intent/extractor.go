package intent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

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
	Result            string // raw adapter result, truncated to maxExtractResultLen
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

const extractionSystemPrompt = `You are a data extraction assistant for a security system. You will be given the result of an API action (e.g., listing emails, fetching contacts). Your job is to extract structural references from the result.

Extract ONLY:
- IDs (message IDs, file IDs, thread IDs, event IDs, etc.)
- Email addresses
- Phone numbers
- Names (person names, file names, channel names)
- Amounts (monetary values, counts)
- Dates and timestamps
- URLs
- Domains

NEVER extract:
- Message bodies or file contents
- Passwords, tokens, or API keys
- Full paragraphs of text
- HTML or markup content

Return a JSON array of objects, each with "fact_type" and "fact_value" fields.
fact_type should be a lowercase identifier like: "email_address", "message_id", "file_id", "phone_number", "person_name", "amount", "date", "url", "domain", "thread_id", "event_id", "channel_name", etc.

Cap at 50 facts maximum. If there are more, keep the most important ones (IDs and addresses over names and dates).

Example output:
[{"fact_type": "email_address", "fact_value": "alice@company.com"}, {"fact_type": "message_id", "fact_value": "msg_abc123"}]

If there are no extractable facts, return an empty array: []`

func (e *LLMExtractor) Extract(ctx context.Context, req ExtractRequest) ([]*store.ChainFact, error) {
	result := req.Result
	if len(result) > maxExtractResultLen {
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
	if err != nil {
		if errors.Is(err, llm.ErrSpendCapExhausted) {
			e.health.SetSpendCapExhausted()
		}
		e.logger.Warn("chain context extraction LLM call failed", "err", err, "task_id", req.TaskID)
		return nil, nil // fail-safe
	}

	// Parse response
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var extracted []struct {
		FactType  string `json:"fact_type"`
		FactValue string `json:"fact_value"`
	}
	if err := json.Unmarshal([]byte(raw), &extracted); err != nil {
		e.logger.Warn("chain context extraction parse failed", "err", err, "task_id", req.TaskID)
		return nil, nil // fail-safe
	}

	if len(extracted) > maxExtractedFacts {
		extracted = extracted[:maxExtractedFacts]
	}

	// Validate and convert
	var facts []*store.ChainFact
	var dropped int
	for _, ef := range extracted {
		if ef.FactType == "" || ef.FactValue == "" {
			dropped++
			continue
		}

		// Substring validation: check that fact_value appears in the raw result.
		if !substringMatch(req.Result, ef.FactValue, ef.FactType) {
			dropped++
			continue
		}

		facts = append(facts, &store.ChainFact{
			ID:        uuid.New().String(),
			TaskID:    req.TaskID,
			SessionID: req.SessionID,
			AuditID:   req.AuditID,
			Service:   req.Service,
			Action:    req.Action,
			FactType:  ef.FactType,
			FactValue: ef.FactValue,
		})
	}

	e.logger.Debug("chain context extraction complete",
		"task_id", req.TaskID,
		"extracted", len(facts),
		"dropped", dropped,
	)

	return facts, nil
}

// substringMatch checks that factValue appears in result.
// Case-insensitive for email_address and domain fact types; exact match for all others.
func substringMatch(result, factValue, factType string) bool {
	if factType == "email_address" || factType == "domain" {
		return strings.Contains(strings.ToLower(result), strings.ToLower(factValue))
	}
	return strings.Contains(result, factValue)
}
