package pricing

import "testing"

func TestCompute_KnownModel(t *testing.T) {
	// Sonnet 4.7: 1000 input + 500 output should be
	// 1000*3 + 500*15 = 10,500 micros (0.0105 USD).
	got := Compute("claude-sonnet-4-7", Usage{InputTokens: 1000, OutputTokens: 500})
	if !got.Known {
		t.Fatalf("expected known model")
	}
	if got.CostMicros != 10500 {
		t.Fatalf("CostMicros = %d, want 10500", got.CostMicros)
	}
}

func TestCompute_WithCacheTokens(t *testing.T) {
	// Sonnet 4.7 cache_read at 0.30/M: 10_000 read tokens = 3_000 micros.
	got := Compute("claude-sonnet-4-7", Usage{CacheReadTokens: 10000})
	if got.CostMicros != 3000 {
		t.Fatalf("CostMicros = %d, want 3000", got.CostMicros)
	}
}

func TestCompute_UnknownModel(t *testing.T) {
	got := Compute("not-a-real-model", Usage{InputTokens: 1000})
	if got.Known {
		t.Fatalf("unknown model should report Known=false")
	}
	if got.CostMicros != 0 {
		t.Fatalf("unknown model CostMicros = %d, want 0", got.CostMicros)
	}
}

func TestCompute_NormalizesVendorPrefixAndSuffix(t *testing.T) {
	cases := []string{
		"claude-opus-4-7",
		"anthropic/claude-opus-4-7",
		"Claude-Opus-4-7",
		"claude-opus-4-7[1m]",
		"claude-opus-4-7-1m",
		"claude-opus-4-7-20260120", // dated snapshot
	}
	want := Compute("claude-opus-4-7", Usage{InputTokens: 1_000_000}).CostMicros
	for _, c := range cases {
		got := Compute(c, Usage{InputTokens: 1_000_000})
		if !got.Known {
			t.Fatalf("model %q: expected known", c)
		}
		if got.CostMicros != want {
			t.Fatalf("model %q: CostMicros = %d, want %d", c, got.CostMicros, want)
		}
	}
}

func TestNormalize_DatedAndAliasedCollapse(t *testing.T) {
	cases := map[string]string{
		// Anthropic dated variants → table key.
		"claude-3-7-sonnet-20250219": "claude-3-7-sonnet",
		"claude-3-7-sonnet-latest":   "claude-3-7-sonnet",
		"claude-3-5-haiku-20241022":  "claude-3-5-haiku",
		// OpenAI YYYY-MM-DD snapshot folds to the same shape.
		"gpt-4o-2024-08-06": "gpt-4o",
		"gpt-4o-mini-2024-07-18": "gpt-4o-mini",
		// Vendor prefix + casing.
		"Anthropic/Claude-Opus-4-7": "claude-opus-4-7",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCompute_DatedAnthropicMatches(t *testing.T) {
	// The actual upstream wire model — exactly the shape Anthropic
	// returns. Pre-fix this would have landed as Known=false.
	got := Compute("claude-3-7-sonnet-20250219", Usage{InputTokens: 1000, OutputTokens: 500})
	if !got.Known {
		t.Fatalf("expected dated sonnet to price as known")
	}
	// 1000*3 + 500*15 = 10,500 micros
	if got.CostMicros != 10500 {
		t.Errorf("CostMicros = %d, want 10500", got.CostMicros)
	}
}
