package intent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// extractEvalCase is a labeled test case for extraction accuracy.
type extractEvalCase struct {
	Name     string             `json:"name"`
	Category string             `json:"category"`
	Request  extractEvalRequest `json:"request"`
	Expected extractEvalExpect  `json:"expected"`
}

type extractEvalRequest struct {
	TaskPurpose string `json:"task_purpose"`
	Service     string `json:"service"`
	Action      string `json:"action"`
	Result      string `json:"result"` // JSON-serialized adapter result
}

type extractEvalExpect struct {
	MinFacts          int      `json:"min_facts"`
	MaxFacts          int      `json:"max_facts,omitempty"`        // 0 = no upper bound
	MustIncludeTypes  []string `json:"must_include_types"`         // e.g. ["email_address", "message_id"]
	MustIncludeValues []string `json:"must_include_values"`        // exact values that must appear
	MustExcludeValues []string `json:"must_exclude_values"`        // values that must NOT appear (secrets)
}

type extractEvalResult struct {
	name     string
	category string
	passed   bool
	details  string
}

// TestEvalExtraction runs labeled eval cases against a real LLM extractor.
// Skipped unless CLAWVISOR_LLM_VERIFICATION_API_KEY is set.
func TestEvalExtraction(t *testing.T) {
	apiKey := os.Getenv("CLAWVISOR_LLM_VERIFICATION_API_KEY")
	if apiKey == "" {
		t.Skip("CLAWVISOR_LLM_VERIFICATION_API_KEY not set — skipping eval")
	}

	model := os.Getenv("CLAWVISOR_LLM_VERIFICATION_MODEL")
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}
	provider := os.Getenv("CLAWVISOR_LLM_VERIFICATION_PROVIDER")
	if provider == "" {
		provider = "anthropic"
	}
	endpoint := os.Getenv("CLAWVISOR_LLM_VERIFICATION_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://api.anthropic.com/v1"
	}

	// Load eval cases.
	data, err := os.ReadFile("testdata/extract_eval_cases.json")
	if err != nil {
		t.Fatalf("failed to read extract eval cases: %v", err)
	}
	var cases []extractEvalCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("failed to parse extract eval cases: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("no extract eval cases found")
	}

	// Build extractor.
	health := llm.NewHealth(config.LLMConfig{
		Provider:       provider,
		Endpoint:       endpoint,
		APIKey:         apiKey,
		Model:          model,
		TimeoutSeconds: 30,
		ChainContext: config.ChainContextConfig{
			LLMProviderConfig: config.LLMProviderConfig{
				Enabled:        true,
				Provider:       provider,
				Endpoint:       endpoint,
				APIKey:         apiKey,
				Model:          model,
				TimeoutSeconds: 30,
			},
		},
	})
	extractor := NewLLMExtractor(health, slog.Default())

	results := make([]extractEvalResult, 0, len(cases))

	for _, tc := range cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			req := ExtractRequest{
				TaskPurpose: tc.Request.TaskPurpose,
				Service:     tc.Request.Service,
				Action:      tc.Request.Action,
				Result:      tc.Request.Result,
				TaskID:      "eval-" + tc.Name,
				SessionID:   "eval-session",
				AuditID:     "eval-audit",
			}

			facts, err := extractor.Extract(context.Background(), req)
			if err != nil {
				t.Fatalf("extract error: %v", err)
			}

			var mismatches []string

			// Check min facts.
			if len(facts) < tc.Expected.MinFacts {
				mismatches = append(mismatches, fmt.Sprintf("min_facts: got %d, want >= %d", len(facts), tc.Expected.MinFacts))
			}

			// Check max facts.
			if tc.Expected.MaxFacts > 0 && len(facts) > tc.Expected.MaxFacts {
				mismatches = append(mismatches, fmt.Sprintf("max_facts: got %d, want <= %d", len(facts), tc.Expected.MaxFacts))
			}

			// Collect extracted types and values for checking.
			extractedTypes := make(map[string]bool)
			var extractedValues []string
			for _, f := range facts {
				extractedTypes[f.FactType] = true
				extractedValues = append(extractedValues, f.FactValue)
			}

			// Check must_include_types.
			for _, typ := range tc.Expected.MustIncludeTypes {
				if !extractedTypes[typ] {
					mismatches = append(mismatches, fmt.Sprintf("missing type %q", typ))
				}
			}

			// Check must_include_values (case-insensitive for emails/domains).
			for _, val := range tc.Expected.MustIncludeValues {
				found := false
				for _, ev := range extractedValues {
					if strings.EqualFold(ev, val) {
						found = true
						break
					}
				}
				if !found {
					mismatches = append(mismatches, fmt.Sprintf("missing value %q", val))
				}
			}

			// Check must_exclude_values.
			for _, val := range tc.Expected.MustExcludeValues {
				for _, ev := range extractedValues {
					if strings.EqualFold(ev, val) {
						mismatches = append(mismatches, fmt.Sprintf("excluded value present: %q", val))
						break
					}
				}
			}

			passed := len(mismatches) == 0
			detail := fmt.Sprintf("extracted %d facts", len(facts))
			if !passed {
				detail = strings.Join(mismatches, "; ") + fmt.Sprintf(" | extracted %d facts: %v", len(facts), factsDebug(facts))
				t.Errorf("FAIL %s: %s", tc.Name, detail)
			} else {
				t.Logf("PASS %s: %d facts extracted", tc.Name, len(facts))
			}

			results = append(results, extractEvalResult{
				name:     tc.Name,
				category: tc.Category,
				passed:   passed,
				details:  detail,
			})
		})
	}

	// Print summary table after all subtests.
	t.Cleanup(func() {
		printExtractEvalSummary(t, results)
	})
}

func factsDebug(facts []*store.ChainFact) string {
	var parts []string
	for _, f := range facts {
		parts = append(parts, fmt.Sprintf("%s=%s", f.FactType, f.FactValue))
	}
	return strings.Join(parts, ", ")
}

func printExtractEvalSummary(t *testing.T, results []extractEvalResult) {
	t.Helper()

	categoryStats := make(map[string][2]int)
	for _, res := range results {
		s := categoryStats[res.category]
		s[1]++
		if res.passed {
			s[0]++
		}
		categoryStats[res.category] = s
	}

	totalPass, totalCount := 0, 0
	t.Logf("")
	t.Logf("╔══════════════════════════════╦═══════╦═══════╦══════════╗")
	t.Logf("║ Category                     ║ Pass  ║ Total ║ Accuracy ║")
	t.Logf("╠══════════════════════════════╬═══════╬═══════╬══════════╣")

	categories := []string{
		"basic_extraction",
		"multi_entity",
		"empty_result",
		"sensitive_filtering",
		"hallucination_resistance",
		"edge_cases",
	}
	for _, cat := range categories {
		s, ok := categoryStats[cat]
		if !ok {
			continue
		}
		pct := 0.0
		if s[1] > 0 {
			pct = float64(s[0]) / float64(s[1]) * 100
		}
		totalPass += s[0]
		totalCount += s[1]
		t.Logf("║ %-28s ║ %5d ║ %5d ║ %6.1f%%  ║", cat, s[0], s[1], pct)
	}

	t.Logf("╠══════════════════════════════╬═══════╬═══════╬══════════╣")
	overallPct := 0.0
	if totalCount > 0 {
		overallPct = float64(totalPass) / float64(totalCount) * 100
	}
	t.Logf("║ %-28s ║ %5d ║ %5d ║ %6.1f%%  ║", "OVERALL", totalPass, totalCount, overallPct)
	t.Logf("╚══════════════════════════════╩═══════╩═══════╩══════════╝")

	if totalCount > 0 && overallPct < 90.0 {
		t.Errorf("overall accuracy %.1f%% is below 90%% threshold", overallPct)
	}
}
