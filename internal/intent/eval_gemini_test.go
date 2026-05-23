package intent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/clawvisor/clawvisor/internal/adapters/apple/imessage"
	"github.com/clawvisor/clawvisor/internal/adapters/definitions"
	"github.com/clawvisor/clawvisor/internal/adapters/google/calendar"
	"github.com/clawvisor/clawvisor/internal/adapters/google/contacts"
	"github.com/clawvisor/clawvisor/internal/adapters/google/drive"
	"github.com/clawvisor/clawvisor/internal/adapters/google/gmail"
	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/adapters/yamlloader"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// TestEvalIntentVerification_Gemini runs the intent-verification eval suite
// against a Gemini model on Vertex AI, using the same prompt and eval cases
// as TestEvalIntentVerification. Skipped unless CLAWVISOR_GEMINI_PROJECT is
// set. Used to compare accuracy/latency vs. Haiku before committing to a
// model switch.
//
// Required env:
//
//	CLAWVISOR_GEMINI_PROJECT — GCP project ID
//
// Optional overrides (defaults shown):
//
//	CLAWVISOR_GEMINI_REGION         = global  (where generateContent runs;
//	                                  preview models like 3.1-flash-lite are
//	                                  global-only)
//	CLAWVISOR_GEMINI_CACHE_REGION   = (defaults to CLAWVISOR_GEMINI_REGION)
//	                                  Where the cachedContents resource is
//	                                  created. Override only when the
//	                                  inference region and cache region must
//	                                  differ (rare).
//	CLAWVISOR_GEMINI_MODEL          = gemini-3.1-flash-lite-preview
//	CLAWVISOR_GEMINI_THINKING_LEVEL = MINIMAL  (LOW | MEDIUM | HIGH)
//	CLAWVISOR_GEMINI_EXPLICIT_CACHE = 1  (set to 0 to disable explicit caching)
//
// Run with:
//
//	CLAWVISOR_GEMINI_PROJECT=my-project go test -count=1 \
//	  -run TestEvalIntentVerification_Gemini -v -timeout=20m ./internal/intent/
func TestEvalIntentVerification_Gemini(t *testing.T) {
	project := os.Getenv("CLAWVISOR_GEMINI_PROJECT")
	if project == "" {
		t.Skip("CLAWVISOR_GEMINI_PROJECT not set; skipping Gemini eval")
	}
	// generateContent runs in CLAWVISOR_GEMINI_REGION. Preview models like
	// gemini-3.1-flash-lite-preview are only available on the global
	// endpoint, so default to "global" for inference. Cache creation has
	// to use a real region (Vertex cachedContents doesn't work on global)
	// — that's controlled separately by CLAWVISOR_GEMINI_CACHE_REGION
	// below. The generateContent call can reference a cross-region cache
	// via its full resource path.
	region := os.Getenv("CLAWVISOR_GEMINI_REGION")
	if region == "" {
		region = "global"
	}
	// Cache region defaults to the inference region. Override only if you
	// need cross-region (e.g., preview model on global with cache regional
	// after the preview gets regional availability).
	cacheRegion := os.Getenv("CLAWVISOR_GEMINI_CACHE_REGION")
	if cacheRegion == "" {
		cacheRegion = region
	}
	model := os.Getenv("CLAWVISOR_GEMINI_MODEL")
	if model == "" {
		model = "gemini-3.1-flash-lite-preview"
	}
	thinkingLevel := "MINIMAL"
	if v := os.Getenv("CLAWVISOR_GEMINI_THINKING_LEVEL"); v != "" {
		thinkingLevel = v
	}

	// Auth via Application Default Credentials — same as production Vertex.
	ctx := context.Background()
	ts, err := google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		t.Fatalf("default token source (run `gcloud auth application-default login`?): %v", err)
	}

	// Load eval cases — same file the Anthropic/Haiku eval uses.
	data, err := os.ReadFile("testdata/eval_cases.json")
	if err != nil {
		t.Fatalf("read eval cases: %v", err)
	}
	var cases []evalCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("parse eval cases: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("no eval cases")
	}

	// Resolve adapter hints exactly like the production verifier flow does.
	loader := yamlloader.New(definitions.FS, nil, nil, nil)
	if err := loader.LoadAll(); err != nil {
		t.Fatalf("loading YAML adapter definitions: %v", err)
	}
	var allAdapters []adapters.Adapter
	for _, ya := range loader.Adapters() {
		allAdapters = append(allAdapters, ya)
	}
	allAdapters = append(allAdapters,
		imessage.New(),
		gmail.New(adapters.NoopOAuthProvider{}),
		calendar.New(adapters.NoopOAuthProvider{}),
		drive.New(adapters.NoopOAuthProvider{}),
		contacts.New(adapters.NoopOAuthProvider{}),
	)
	hintsByService := make(map[string]string)
	for _, ada := range allAdapters {
		if hinter, ok := ada.(adapters.VerificationHinter); ok {
			hintsByService[ada.ServiceID()] = hinter.VerificationHints()
		}
	}

	// generateContent endpoint — uses inference region (may be "global"
	// for preview models). "global" uses unprefixed host; otherwise prefix.
	infHost := region + "-aiplatform.googleapis.com"
	if region == "global" {
		infHost = "aiplatform.googleapis.com"
	}
	endpoint := fmt.Sprintf(
		"https://%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent",
		infHost, project, region, model,
	)
	// cachedContents endpoint — supports both global and regional locations.
	// We reference the cache by its full resource path in generateContent
	// so cross-location reference is fine. Use unprefixed host for global.
	cacheHost := cacheRegion + "-aiplatform.googleapis.com"
	if cacheRegion == "global" {
		cacheHost = "aiplatform.googleapis.com"
	}
	cachedContentsBase := fmt.Sprintf(
		"https://%s/v1/projects/%s/locations/%s/cachedContents",
		cacheHost, project, cacheRegion,
	)
	httpClient := &http.Client{Timeout: 30 * time.Second}

	// Create an explicit context cache for the system prompt. The cache
	// resource lives in CLAWVISOR_GEMINI_CACHE_REGION (regional, since
	// Vertex cachedContents doesn't work on global). generateContent
	// references the cache by its full resource path, which works even
	// when inference runs in a different region (e.g. global preview
	// endpoint pointing at a us-central1 cache). Disable with
	// CLAWVISOR_GEMINI_EXPLICIT_CACHE=0.
	cacheName := ""
	if os.Getenv("CLAWVISOR_GEMINI_EXPLICIT_CACHE") != "0" {
		// The model path in the cache must use the cache's region, not the
		// inference region — that's where the resource actually lives.
		modelPath := fmt.Sprintf("projects/%s/locations/%s/publishers/google/models/%s", project, cacheRegion, model)
		name, err := createGeminiCache(ctx, httpClient, ts, cachedContentsBase, modelPath, verificationSystemPrompt)
		if err != nil {
			t.Logf("explicit cache creation failed; falling back to no-cache mode: %v", err)
		} else {
			cacheName = name
			t.Logf("created Gemini cached content in %s: %s", cacheRegion, cacheName)
			t.Cleanup(func() {
				if err := deleteGeminiCache(context.Background(), httpClient, ts, cacheHost, cacheName); err != nil {
					t.Logf("cached content cleanup failed (will auto-expire by TTL): %v", err)
				}
			})
		}
	}

	results := make([]evalResult, 0, len(cases))
	latenciesMS := make([]int, 0, len(cases))
	var usages []geminiUsage

	t.Logf("Running %d eval cases against Gemini model %q (inference=%s/%s, cache=%s, thinkingLevel=%s, explicit_cache=%v)",
		len(cases), model, region, project, cacheRegion, thinkingLevel, cacheName != "")

	for _, tc := range cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			svcBase := tc.Request.Service
			if idx := strings.IndexByte(svcBase, ':'); idx != -1 {
				svcBase = svcBase[:idx]
			}

			req := VerifyRequest{
				TaskPurpose:         tc.Request.TaskPurpose,
				ExpectedUse:         tc.Request.ExpectedUse,
				ExpansionRationale:  tc.Request.ExpansionRationale,
				Service:             tc.Request.Service,
				Action:              tc.Request.Action,
				Params:              expandDatePlaceholders(tc.Request.Params),
				Reason:              tc.Request.Reason,
				ServiceHints:        hintsByService[svcBase],
				TaskID:              "eval-gemini-" + tc.Name,
				ChainContextOptOut:  tc.Request.ChainContextOptOut,
				ChainContextEnabled: len(tc.Request.ChainFacts) > 0 || tc.Request.ChainContextOptOut,
			}
			for _, f := range tc.Request.ChainFacts {
				req.ChainFacts = append(req.ChainFacts, store.ChainFact{
					Service:   f.Service,
					Action:    f.Action,
					FactType:  f.FactType,
					FactValue: f.FactValue,
				})
			}

			// Use the SAME system prompt and user message as the production
			// verifier so any accuracy delta is purely the model.
			userMsg := buildVerificationUserMessage(req)

			start := time.Now()
			raw, usage, err := callGeminiVerifier(ctx, httpClient, ts, endpoint, verificationSystemPrompt, userMsg, thinkingLevel, cacheName)
			latencyMS := int(time.Since(start).Milliseconds())
			latenciesMS = append(latenciesMS, latencyMS)
			if usage != nil {
				usages = append(usages, *usage)
			}
			if err != nil {
				t.Fatalf("gemini call: %v", err)
			}

			t.Logf("usage: input=%d output=%d cached=%d total=%d",
				usage.PromptTokenCount, usage.CandidatesTokenCount,
				usage.CachedContentTokenCount, usage.TotalTokenCount)

			verdict, err := parseVerificationResponse(raw)
			if err != nil {
				t.Fatalf("parse gemini response: %v\nraw: %s", err, raw)
			}

			var mismatches []string
			if verdict.Allow != tc.Expected.Allow {
				mismatches = append(mismatches, fmt.Sprintf("allow: got %v, want %v", verdict.Allow, tc.Expected.Allow))
			}
			if verdict.ParamScope != tc.Expected.ParamScope {
				mismatches = append(mismatches, fmt.Sprintf("param_scope: got %q, want %q", verdict.ParamScope, tc.Expected.ParamScope))
			}
			if !reasonCoherenceMatches(verdict.ReasonCoherence, tc.Expected.ReasonCoherence) {
				mismatches = append(mismatches, fmt.Sprintf("reason_coherence: got %q, want %q", verdict.ReasonCoherence, tc.Expected.ReasonCoherence))
			}
			if len(tc.Expected.MissingChainValues) > 0 {
				gotSet := make(map[string]bool, len(verdict.MissingChainValues))
				for _, v := range verdict.MissingChainValues {
					gotSet[v] = true
				}
				for _, want := range tc.Expected.MissingChainValues {
					if !gotSet[want] {
						mismatches = append(mismatches, fmt.Sprintf("missing_chain_values: %q not in %v", want, verdict.MissingChainValues))
					}
				}
			}

			passed := len(mismatches) == 0
			detail := verdict.Explanation
			if !passed {
				detail = strings.Join(mismatches, "; ") + " | explanation: " + verdict.Explanation
				t.Errorf("FAIL %s [%dms]: %s", tc.Name, latencyMS, detail)
			} else {
				t.Logf("PASS %s [%dms]: %s", tc.Name, latencyMS, verdict.Explanation)
			}

			results = append(results, evalResult{
				name:     tc.Name,
				category: tc.Category,
				passed:   passed,
				details:  detail,
			})
		})
	}

	t.Cleanup(func() {
		printEvalSummary(t, results)
		printLatencySummary(t, latenciesMS, model)
		printGeminiUsageSummary(t, usages, model)
	})
}

// printGeminiUsageSummary aggregates per-call usage to surface whether
// implicit caching fired. CachedContentTokenCount > 0 indicates the prompt
// prefix was served from cache for that call. A high cache-hit rate after
// the first call suggests implicit caching is working; if it stays at 0
// throughout, implicit caching isn't supported (or the prefix changed) and
// explicit context caching may be needed for a fair latency comparison.
func printGeminiUsageSummary(t *testing.T, usages []geminiUsage, model string) {
	t.Helper()
	if len(usages) == 0 {
		return
	}
	var sumPrompt, sumOutput, sumCached, sumTotal int
	cachedCalls := 0
	for _, u := range usages {
		sumPrompt += u.PromptTokenCount
		sumOutput += u.CandidatesTokenCount
		sumCached += u.CachedContentTokenCount
		sumTotal += u.TotalTokenCount
		if u.CachedContentTokenCount > 0 {
			cachedCalls++
		}
	}
	hitRate := 0.0
	if len(usages) > 0 {
		hitRate = float64(cachedCalls) / float64(len(usages)) * 100
	}
	avgCached := 0
	if cachedCalls > 0 {
		avgCached = sumCached / cachedCalls
	}
	t.Logf("")
	t.Logf("Token usage for %s (n=%d):", model, len(usages))
	t.Logf("  totals:   input=%d output=%d cached=%d total=%d",
		sumPrompt, sumOutput, sumCached, sumTotal)
	t.Logf("  averages: input=%d output=%d total=%d",
		sumPrompt/len(usages), sumOutput/len(usages), sumTotal/len(usages))
	t.Logf("  cache:    %d/%d calls had cached_tokens > 0 (%.1f%%); avg cached_tokens on hits = %d",
		cachedCalls, len(usages), hitRate, avgCached)
	if cachedCalls == 0 {
		t.Logf("  → implicit caching did not fire on any call. The first call always misses; if subsequent calls also stay at 0, the model likely needs explicit context caching for the same prefix-reuse benefit Anthropic gets from cache_control.")
	} else if cachedCalls == 1 {
		t.Logf("  → only one cache hit. Implicit caching may be region-scoped or have a long warmup. Consider explicit caching for stable comparison.")
	} else {
		t.Logf("  → implicit caching is firing on %.0f%% of calls; comparison is fair against Anthropic cache_control.", hitRate)
	}
}

// geminiUsage captures the per-request token accounting Vertex returns in
// usageMetadata. CachedContentTokenCount is the key signal for whether
// implicit prompt caching fired — when nonzero, that many tokens of the
// prompt prefix were served from cache instead of re-processed.
type geminiUsage struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
}

// callGeminiVerifier sends a generateContent request to Vertex Gemini and
// returns the model's text response plus the usage metadata. When cacheName
// is non-empty it references that cached content (containing the system
// prompt) instead of inlining systemInstruction; this is the explicit-
// caching path that gives us reliable hits, unlike Vertex's best-effort
// implicit caching.
func callGeminiVerifier(
	ctx context.Context,
	httpClient *http.Client,
	ts oauth2.TokenSource,
	endpoint, systemPrompt, userMsg string,
	thinkingLevel string,
	cacheName string,
) (string, *geminiUsage, error) {
	body := map[string]any{
		"contents": []map[string]any{
			{
				"role":  "user",
				"parts": []map[string]any{{"text": userMsg}},
			},
		},
		"generationConfig": map[string]any{
			"temperature":     0,
			"maxOutputTokens": 1024,
			"thinkingConfig": map[string]any{
				"thinkingLevel": thinkingLevel,
			},
		},
	}
	if cacheName != "" {
		// systemInstruction lives in the cached content; cannot also be
		// inlined here or the API rejects the request.
		body["cachedContent"] = cacheName
	} else {
		body["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": systemPrompt}},
		}
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return "", nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", nil, err
	}
	tok, err := ts.Token()
	if err != nil {
		return "", nil, fmt.Errorf("token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("vertex gemini status %d: %s", resp.StatusCode, string(respBody))
	}

	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata  geminiUsage `json:"usageMetadata"`
		PromptFeedback any         `json:"promptFeedback"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", nil, fmt.Errorf("decode response: %w (body=%s)", err, truncateForLog(string(respBody), 400))
	}
	if len(out.Candidates) == 0 {
		return "", &out.UsageMetadata, fmt.Errorf("no candidates in response (body=%s)", truncateForLog(string(respBody), 400))
	}
	for _, p := range out.Candidates[0].Content.Parts {
		if p.Text != "" {
			return p.Text, &out.UsageMetadata, nil
		}
	}
	return "", &out.UsageMetadata, fmt.Errorf("no text part (finish=%s, body=%s)", out.Candidates[0].FinishReason, truncateForLog(string(respBody), 400))
}

// truncateForLog clips s to n characters with an ellipsis suffix, used to
// keep error messages bounded when surfacing raw Vertex responses.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// printLatencySummary emits a small p50/p90/p95/p99 table for latencies
// captured during the eval run.
func printLatencySummary(t *testing.T, ms []int, model string) {
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
	t.Logf("Latency for %s (n=%d):", model, len(sorted))
	t.Logf("  mean=%dms  p50=%dms  p75=%dms  p90=%dms  p95=%dms  p99=%dms  max=%dms",
		mean, pct(0.50), pct(0.75), pct(0.90), pct(0.95), pct(0.99), sorted[len(sorted)-1])
}

// createGeminiCache POSTs to cachedContents to register the verification
// system prompt as an explicit context cache. Returns the resource name
// (e.g. "projects/.../cachedContents/12345...") to reference in subsequent
// generateContent calls. modelPath is the fully-qualified model resource
// (projects/.../publishers/google/models/MODEL).
func createGeminiCache(
	ctx context.Context,
	httpClient *http.Client,
	ts oauth2.TokenSource,
	cachedContentsURL, modelPath, systemPrompt string,
) (string, error) {
	body := map[string]any{
		"model": modelPath,
		"systemInstruction": map[string]any{
			"parts": []map[string]any{{"text": systemPrompt}},
		},
		// TTL must be long enough to outlive a full eval run; 30 min is
		// generous for ~215 cases at <2s each. Cache auto-expires after.
		"ttl": "1800s",
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cachedContentsURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	tok, err := ts.Token()
	if err != nil {
		return "", fmt.Errorf("token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("create cachedContents status %d: %s", resp.StatusCode, truncateForLog(string(respBody), 600))
	}
	var out struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("decode cache response: %w (body=%s)", err, truncateForLog(string(respBody), 400))
	}
	if out.Name == "" {
		return "", fmt.Errorf("create cachedContents returned empty name (body=%s)", truncateForLog(string(respBody), 400))
	}
	return out.Name, nil
}

// deleteGeminiCache best-effort deletes the cached content. Failure is
// non-fatal; the cache will auto-expire by its TTL anyway.
func deleteGeminiCache(
	ctx context.Context,
	httpClient *http.Client,
	ts oauth2.TokenSource,
	host, cacheName string,
) error {
	url := fmt.Sprintf("https://%s/v1/%s", host, cacheName)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	tok, err := ts.Token()
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("delete cachedContents status %d: %s", resp.StatusCode, truncateForLog(string(body), 400))
	}
	return nil
}
