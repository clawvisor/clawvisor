// Package pricing turns raw LLM token counts into a cost in micro-USD.
//
// Money is stored as int64 micro-USD (1e-6 USD) everywhere downstream
// of this package. That avoids float drift in SUM() aggregates and
// matches how Stripe / GCP / most billing systems represent cents-fractions.
//
// The table is hand-maintained; bump pricedAt on each entry when you
// touch it so a future "is this stale?" check is grep-able. Unknown
// models return Cost{Known: false} — callers should still log token
// counts and skip the cost field rather than fail the request.
package pricing

import (
	"regexp"
	"strings"
	"time"
)

// ModelPricing is the per-1M-token list price (USD) for one model.
// CacheWritePerM is the price for a 5-minute prompt-cache write
// (typically 1.25x InputPerM). CacheWrite1hPerM is the longer-TTL
// variant Anthropic introduced for repeated-system-prompt workloads,
// typically ~2x InputPerM; falls back to CacheWritePerM when zero so
// older table entries keep working. CacheReadPerM is the discounted
// re-read price, typically 0.1x InputPerM.
type ModelPricing struct {
	InputPerM        float64
	OutputPerM       float64
	CacheWritePerM   float64
	CacheWrite1hPerM float64
	CacheReadPerM    float64
	PricedAt         time.Time
}

// Usage is the per-request token breakdown extracted from the upstream
// response. Field names match the Anthropic wire shape; OpenAI's
// equivalents (prompt_tokens, completion_tokens, cached_tokens) are
// normalised into the same struct at the call site.
//
// CacheWriteTokens is the 5-minute-TTL bucket; CacheWrite1hTokens is
// the 1-hour-TTL bucket. Anthropic reports them separately under
// `usage.cache_creation.{ephemeral_5m_input_tokens,
// ephemeral_1h_input_tokens}`. Older clients that only emit the
// top-level `cache_creation_input_tokens` land entirely in
// CacheWriteTokens (5m bucket) — that matches the historical default
// TTL.
type Usage struct {
	InputTokens        int
	OutputTokens       int
	CacheWriteTokens   int
	CacheWrite1hTokens int
	CacheReadTokens    int
}

// Cost is the computed result. CostMicros is the only field downstream
// storage cares about; the rest are surfaced for debugging.
type Cost struct {
	Known      bool
	Model      string
	CostMicros int64
}

// lastBulkPricedAt is the timestamp the entire table was last
// reconciled against upstream pricing in one pass. Single-row updates
// between bulk reconciliations should inline a distinct time.Date(...)
// literal on the row instead — using this shared stamp on a row that
// was edited later makes the PricedAt claim a lie for that row.
var lastBulkPricedAt = time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC)

var table = map[string]ModelPricing{
	// Anthropic — current generation (Claude 4.x). CacheWrite1hPerM
	// is 2x InputPerM (Anthropic's 1-hour cache premium); 5-minute
	// writes are at 1.25x.
	"claude-opus-4-8":             {InputPerM: 15.00, OutputPerM: 75.00, CacheWritePerM: 18.75, CacheWrite1hPerM: 30.00, CacheReadPerM: 1.50, PricedAt: time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)},
	"claude-opus-4-7":             {InputPerM: 15.00, OutputPerM: 75.00, CacheWritePerM: 18.75, CacheWrite1hPerM: 30.00, CacheReadPerM: 1.50, PricedAt: lastBulkPricedAt},
	"claude-opus-4-6":             {InputPerM: 15.00, OutputPerM: 75.00, CacheWritePerM: 18.75, CacheWrite1hPerM: 30.00, CacheReadPerM: 1.50, PricedAt: lastBulkPricedAt},
	"claude-opus-4-5":             {InputPerM: 15.00, OutputPerM: 75.00, CacheWritePerM: 18.75, CacheWrite1hPerM: 30.00, CacheReadPerM: 1.50, PricedAt: lastBulkPricedAt},
	"claude-sonnet-4-7":           {InputPerM: 3.00, OutputPerM: 15.00, CacheWritePerM: 3.75, CacheWrite1hPerM: 6.00, CacheReadPerM: 0.30, PricedAt: lastBulkPricedAt},
	"claude-sonnet-4-6":           {InputPerM: 3.00, OutputPerM: 15.00, CacheWritePerM: 3.75, CacheWrite1hPerM: 6.00, CacheReadPerM: 0.30, PricedAt: lastBulkPricedAt},
	"claude-sonnet-4-5":           {InputPerM: 3.00, OutputPerM: 15.00, CacheWritePerM: 3.75, CacheWrite1hPerM: 6.00, CacheReadPerM: 0.30, PricedAt: lastBulkPricedAt},
	"claude-haiku-4-5":            {InputPerM: 1.00, OutputPerM: 5.00, CacheWritePerM: 1.25, CacheWrite1hPerM: 2.00, CacheReadPerM: 0.10, PricedAt: lastBulkPricedAt},
	// Keys are the normalized form: vendor prefix, `-latest` suffix,
	// and `-YYYYMMDD` date suffix stripped (see Normalize). So the
	// concrete `claude-3-7-sonnet-20250219` and the alias
	// `claude-3-7-sonnet-latest` both resolve to this row.
	"claude-3-7-sonnet":           {InputPerM: 3.00, OutputPerM: 15.00, CacheWritePerM: 3.75, CacheWrite1hPerM: 6.00, CacheReadPerM: 0.30, PricedAt: lastBulkPricedAt},
	"claude-3-5-haiku":            {InputPerM: 0.80, OutputPerM: 4.00, CacheWritePerM: 1.00, CacheWrite1hPerM: 1.60, CacheReadPerM: 0.08, PricedAt: lastBulkPricedAt},

	// OpenAI. CacheWritePerM is omitted because OpenAI caches
	// automatically with no per-write premium; their prompt-caching
	// only discounts the cached-read portion. Cache-write fields
	// stay zero for OpenAI rows.
	"gpt-5.5":         {InputPerM: 5.00, OutputPerM: 30.00, CacheReadPerM: 0.50, PricedAt: lastBulkPricedAt},
	"gpt-5.5-pro":     {InputPerM: 30.00, OutputPerM: 180.00, CacheReadPerM: 3.00, PricedAt: lastBulkPricedAt},
	"gpt-5.2-codex":   {InputPerM: 1.75, OutputPerM: 14.00, CacheReadPerM: 0.175, PricedAt: lastBulkPricedAt},
	"gpt-4o":          {InputPerM: 2.50, OutputPerM: 10.00, CacheReadPerM: 1.25, PricedAt: lastBulkPricedAt},
	"gpt-4o-mini":     {InputPerM: 0.15, OutputPerM: 0.60, CacheReadPerM: 0.075, PricedAt: lastBulkPricedAt},
	"gpt-4-turbo":     {InputPerM: 10.00, OutputPerM: 30.00, PricedAt: lastBulkPricedAt},
	"o1":              {InputPerM: 15.00, OutputPerM: 60.00, CacheReadPerM: 7.50, PricedAt: lastBulkPricedAt},
	"o1-mini":         {InputPerM: 3.00, OutputPerM: 12.00, CacheReadPerM: 1.50, PricedAt: lastBulkPricedAt},
	"o3-mini":         {InputPerM: 1.10, OutputPerM: 4.40, CacheReadPerM: 0.55, PricedAt: lastBulkPricedAt},
}

// Compute returns the dollar cost of one request, in micro-USD.
// model is matched against the table after a Normalize pass. Unknown
// models return Cost{Known: false, CostMicros: 0}.
func Compute(model string, u Usage) Cost {
	key := Normalize(model)
	p, ok := table[key]
	if !ok {
		return Cost{Known: false, Model: model}
	}
	// (tokens / 1_000_000) * pricePerM USD * 1_000_000 micros/USD
	// = tokens * pricePerM micros — the 1M factors cancel, so we
	// don't lose precision on small token counts.
	cacheWrite1hRate := p.CacheWrite1hPerM
	if cacheWrite1hRate == 0 {
		// Table entry doesn't distinguish TTLs (older row, or a
		// provider with only one cache write tier). Fall back to the
		// 5m rate so we under-bill less catastrophically than zero.
		cacheWrite1hRate = p.CacheWritePerM
	}
	total := float64(u.InputTokens)*p.InputPerM +
		float64(u.OutputTokens)*p.OutputPerM +
		float64(u.CacheWriteTokens)*p.CacheWritePerM +
		float64(u.CacheWrite1hTokens)*cacheWrite1hRate +
		float64(u.CacheReadTokens)*p.CacheReadPerM
	return Cost{
		Known:      true,
		Model:      key,
		CostMicros: int64(total + 0.5),
	}
}

// dateSuffix matches an `-YYYYMMDD` trailing component on a model
// id. Anthropic's stable wire IDs include the release date
// (`claude-3-7-sonnet-20250219`); OpenAI does the same for some
// snapshots (`gpt-4o-2024-08-06`, which after the dash-only pass
// reads as `gpt-4o-20240806`). Strip it so dated and aliased ids
// collapse to the same table key.
var dateSuffix = regexp.MustCompile(`-\d{8}$`)

// Normalize collapses the many shapes a model identifier can arrive
// in down to one table key. Strips, in order: vendor prefixes
// (`anthropic/`, `openai/`); the `[1m]` / `:1m` / `-1m` context-
// window markers (hosted 1M-context Anthropic variants price the
// same as the 200k base today); `-latest` and `-YYYYMMDD` so
// aliased and concrete-dated ids resolve to the same row. Also
// folds OpenAI's `YYYY-MM-DD` snapshot suffix to the same shape.
//
// Exported so callers can normalize the model name they persist on
// llm_request_cost rows — GROUP BY would otherwise fragment a
// task's spend across spelling variants.
func Normalize(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	// Strip vendor prefixes. Slash-form (`anthropic/...`) is the
	// common one for first-party SDKs and most gateways. Dot-form
	// (`anthropic.claude-opus-4-7`) shows up on Bedrock-style
	// routing, which the lite-proxy doesn't target today but might
	// see indirectly via an upstream that does.
	for _, p := range []string{"anthropic/", "openai/", "anthropic.", "openai."} {
		m = strings.TrimPrefix(m, p)
	}
	m = strings.TrimSuffix(m, "[1m]")
	m = strings.TrimSuffix(m, ":1m")
	m = strings.TrimSuffix(m, "-1m")
	m = strings.TrimSuffix(m, "-latest")
	// Fold OpenAI's YYYY-MM-DD snapshot ids (e.g. `gpt-4o-2024-08-06`)
	// into the same shape Anthropic uses so one strip handles both.
	if i := len(m) - 11; i > 0 && m[i] == '-' &&
		isDigits(m[i+1:i+5]) && m[i+5] == '-' && isDigits(m[i+6:i+8]) && m[i+8] == '-' && isDigits(m[i+9:]) {
		m = m[:i] + "-" + m[i+1:i+5] + m[i+6:i+8] + m[i+9:]
	}
	m = dateSuffix.ReplaceAllString(m, "")
	return m
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}
