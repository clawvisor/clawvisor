package intent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/pkg/config"
)

// TestEvalExtraction_Gemini runs the chain-context extraction eval suite
// against a Gemini model on Vertex AI, using the same cases as
// TestEvalExtraction. Skipped unless CLAWVISOR_GEMINI_PROJECT is set.
//
// The test wires the production-style code path: builds an llm.Client with
// Provider="gemini", creates a GeminiCacheManager holding the
// extractionSystemPrompt, attaches the cache function to the client, then
// for each case calls CompleteWithUsage and runs the same parse +
// merge-with-builtin-patterns logic the production extractor uses. Any
// accuracy delta vs the Anthropic eval reflects the model itself, not
// extraction wiring.
//
// Required env:
//
//	CLAWVISOR_GEMINI_PROJECT — GCP project ID
//
// Optional overrides (defaults shown):
//
//	CLAWVISOR_GEMINI_REGION         = global  (where generateContent runs)
//	CLAWVISOR_GEMINI_CACHE_REGION   = (defaults to CLAWVISOR_GEMINI_REGION)
//	CLAWVISOR_GEMINI_MODEL          = gemini-3.1-flash-lite-preview
//	CLAWVISOR_GEMINI_THINKING_LEVEL = MINIMAL  (LOW | MEDIUM | HIGH)
//	CLAWVISOR_GEMINI_EXPLICIT_CACHE = 1  (set to 0 to disable explicit caching)
//
// Run with:
//
//	CLAWVISOR_GEMINI_PROJECT=my-project go test -count=1 \
//	  -run TestEvalExtraction_Gemini -v -timeout=20m ./internal/intent/
func TestEvalExtraction_Gemini(t *testing.T) {
	project := os.Getenv("CLAWVISOR_GEMINI_PROJECT")
	if project == "" {
		t.Skip("CLAWVISOR_GEMINI_PROJECT not set; skipping Gemini extraction eval")
	}
	region := os.Getenv("CLAWVISOR_GEMINI_REGION")
	if region == "" {
		region = "global"
	}
	cacheRegion := os.Getenv("CLAWVISOR_GEMINI_CACHE_REGION")
	if cacheRegion == "" {
		cacheRegion = region
	}
	model := os.Getenv("CLAWVISOR_GEMINI_MODEL")
	if model == "" {
		model = "gemini-3.1-flash-lite-preview"
	}
	thinkingLevel := os.Getenv("CLAWVISOR_GEMINI_THINKING_LEVEL")
	if thinkingLevel == "" {
		thinkingLevel = "MINIMAL"
	}

	// Load eval cases — same file the Anthropic extraction eval uses.
	data, err := os.ReadFile("testdata/extract_eval_cases.json")
	if err != nil {
		t.Fatalf("read extract eval cases: %v", err)
	}
	var cases []extractEvalCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("parse extract eval cases: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("no extract eval cases")
	}

	// Build the Gemini-backed LLM client. Endpoint is constructed by
	// NewClient from project/region/model since Endpoint is left empty.
	client := llm.NewClient(config.LLMProviderConfig{
		Provider:            "gemini",
		Project:             project,
		Region:              region,
		Model:               model,
		TimeoutSeconds:      30,
		GeminiThinkingLevel: thinkingLevel,
	})

	// Optional explicit caching — same lifecycle as production wiring.
	cacheName := ""
	if os.Getenv("CLAWVISOR_GEMINI_EXPLICIT_CACHE") != "0" {
		mgr, err := llm.NewGeminiCacheManager(llm.GeminiCacheManagerConfig{
			Project:      project,
			Region:       cacheRegion,
			Model:        model,
			SystemPrompt: extractionSystemPrompt,
			TTL:          30 * time.Minute,
			Logger:       slog.Default(),
		})
		if err != nil {
			t.Logf("explicit cache manager init failed; falling back to uncached: %v", err)
		} else {
			startCtx, startCancel := context.WithTimeout(context.Background(), 30*time.Second)
			startErr := mgr.Start(startCtx)
			startCancel()
			if startErr != nil {
				t.Logf("explicit cache start failed; falling back to uncached: %v", startErr)
			} else {
				client.AttachGeminiCacheNameFn(mgr.CacheName)
				cacheName = mgr.CacheName()
				t.Logf("created Gemini extraction cache in %s: %s", cacheRegion, cacheName)
				t.Cleanup(func() {
					stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer stopCancel()
					mgr.Stop(stopCtx)
				})
			}
		}
	}

	results := make([]extractEvalResult, 0, len(cases))
	latenciesMS := make([]int, 0, len(cases))
	cachedHits := 0

	t.Logf("Running %d extraction cases against Gemini model %q (inference=%s/%s, cache=%s, thinkingLevel=%s, explicit_cache=%v)",
		len(cases), model, region, project, cacheRegion, thinkingLevel, cacheName != "")

	for _, tc := range cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			req := ExtractRequest{
				TaskPurpose: tc.Request.TaskPurpose,
				Service:     tc.Request.Service,
				Action:      tc.Request.Action,
				Result:      tc.Request.Result,
				TaskID:      "eval-gemini-" + tc.Name,
				SessionID:   "eval-session",
				AuditID:     "eval-audit",
			}

			// Production user message shape — matches LLMExtractor.Extract.
			result := req.Result
			if len(result) > maxExtractResultLen {
				result = result[:maxExtractResultLen]
			}
			userMsg := fmt.Sprintf("Service: %s\nAction: %s\n\nResult:\n%s", req.Service, req.Action, result)

			start := time.Now()
			raw, usage, err := client.CompleteWithUsage(context.Background(), []llm.ChatMessage{
				{Role: "system", Content: extractionSystemPrompt, CacheControl: true},
				{Role: "user", Content: userMsg},
			})
			latencyMS := int(time.Since(start).Milliseconds())
			latenciesMS = append(latenciesMS, latencyMS)
			if err != nil {
				t.Fatalf("gemini extract: %v", err)
			}
			if usage != nil && usage.CacheReadInputTokens > 0 {
				cachedHits++
			}

			// Parse + merge with builtin patterns — same flow the production
			// extractor uses. extractFirstJSONValue tolerates fences/prose.
			extracted := extractFirstJSONValue(raw)
			directFacts, llmPatterns := parseExtractionResponse(extracted, slog.Default(), req.TaskID)
			patterns := append(llmPatterns, builtinPatterns(req.Service, req.Action)...)
			facts, _ := mergeExtractionResults(directFacts, patterns, req, slog.Default())

			// Same comparison logic as TestEvalExtraction.
			var mismatches []string
			if len(facts) < tc.Expected.MinFacts {
				mismatches = append(mismatches, fmt.Sprintf("min_facts: got %d, want >= %d", len(facts), tc.Expected.MinFacts))
			}
			if tc.Expected.MaxFacts > 0 && len(facts) > tc.Expected.MaxFacts {
				mismatches = append(mismatches, fmt.Sprintf("max_facts: got %d, want <= %d", len(facts), tc.Expected.MaxFacts))
			}
			extractedTypes := make(map[string]bool)
			var extractedValues []string
			for _, f := range facts {
				extractedTypes[f.FactType] = true
				extractedValues = append(extractedValues, f.FactValue)
			}
			for _, typ := range tc.Expected.MustIncludeTypes {
				if !extractedTypes[typ] {
					mismatches = append(mismatches, fmt.Sprintf("missing type %q", typ))
				}
			}
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
				t.Errorf("FAIL %s [%dms]: %s", tc.Name, latencyMS, detail)
			} else {
				t.Logf("PASS %s [%dms]: %d facts extracted", tc.Name, latencyMS, len(facts))
			}

			results = append(results, extractEvalResult{
				name:     tc.Name,
				category: tc.Category,
				passed:   passed,
				details:  detail,
			})
		})
	}

	t.Cleanup(func() {
		printExtractEvalSummary(t, results)
		printExtractLatencySummary(t, latenciesMS, model)
		if cacheName != "" || cachedHits > 0 {
			t.Logf("")
			t.Logf("Gemini cache hits: %d/%d (%.1f%%)",
				cachedHits, len(latenciesMS), 100*float64(cachedHits)/float64(max1(len(latenciesMS))))
		}
	})
}

// printExtractLatencySummary mirrors printLatencySummary in eval_gemini_test.go
// but doesn't import or share it (test files in the same package can
// otherwise collide on names). Kept small and self-contained.
func printExtractLatencySummary(t *testing.T, ms []int, model string) {
	t.Helper()
	if len(ms) == 0 {
		return
	}
	sorted := make([]int, len(ms))
	copy(sorted, ms)
	sort.Ints(sorted)
	pct := func(p float64) int {
		if len(sorted) == 0 {
			return 0
		}
		idx := int(float64(len(sorted)-1) * p)
		return sorted[idx]
	}
	mean := 0
	for _, v := range sorted {
		mean += v
	}
	mean /= len(sorted)
	t.Logf("")
	t.Logf("Extraction latency for %s (n=%d):", model, len(sorted))
	t.Logf("  mean=%dms  p50=%dms  p75=%dms  p90=%dms  p95=%dms  p99=%dms  max=%dms",
		mean, pct(0.50), pct(0.75), pct(0.90), pct(0.95), pct(0.99), sorted[len(sorted)-1])
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}
