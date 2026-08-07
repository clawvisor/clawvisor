package format

import (
	"strings"
	"testing"
)

func TestSanitizeText_TruncatesOnRuneBoundary(t *testing.T) {
	// 10 emoji (4 bytes each = 40 bytes, 10 runes)
	input := strings.Repeat("😀", 10)
	got := SanitizeText(input, 5)
	want := strings.Repeat("😀", 5) + " [truncated]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSanitizeText_NoTruncationUnderLimit(t *testing.T) {
	input := "Hello, world!"
	got := SanitizeText(input, MaxBodyLen)
	if got != input {
		t.Errorf("got %q, want %q", got, input)
	}
}

func TestSanitizeText_CountsRunesNotBytes(t *testing.T) {
	// Each CJK character is 3 bytes. 100 chars = 300 bytes.
	// With a limit of 100 runes, this should NOT be truncated.
	input := strings.Repeat("漢", 100)
	got := SanitizeText(input, 100)
	if got != input {
		t.Errorf("should not truncate 100 runes at limit 100, got length %d", len(got))
	}
	// With a limit of 50 runes, it should be truncated.
	got = SanitizeText(input, 50)
	want := strings.Repeat("漢", 50) + " [truncated]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSanitizeText_ZeroMaxLenNoTruncation(t *testing.T) {
	input := strings.Repeat("a", 10000)
	got := SanitizeText(input, 0)
	if got != input {
		t.Errorf("maxLen=0 should skip truncation")
	}
}

func TestSanitizeText_StripsHTML(t *testing.T) {
	got := SanitizeText("<b>bold</b> text", 100)
	if got != "bold text" {
		t.Errorf("got %q, want %q", got, "bold text")
	}
}

func TestResolveMaxBytes(t *testing.T) {
	if got, err := ResolveMaxBytes(nil); err != nil || got != DefaultDownloadBytes {
		t.Errorf("absent = %d, %v; want %d", got, err, DefaultDownloadBytes)
	}
	for _, v := range []any{float64(2048), int(2048), int64(2048)} {
		got, err := ResolveMaxBytes(v)
		if err != nil || got != 2048 {
			t.Errorf("ResolveMaxBytes(%T) = %d, %v", v, got, err)
		}
	}
	// Fractional values must be rejected, not truncated: 1.9 would silently
	// become 1 and 0.5 would become 0 and fail as "must be positive".
	for _, v := range []float64{1.9, 0.5, 1024.5} {
		if _, err := ResolveMaxBytes(v); err == nil {
			t.Errorf("ResolveMaxBytes(%v) should reject a fractional value", v)
		}
	}
	for _, v := range []any{float64(0), float64(-1), "big", true} {
		if _, err := ResolveMaxBytes(v); err == nil {
			t.Errorf("ResolveMaxBytes(%v) should error", v)
		}
	}
	if _, err := ResolveMaxBytes(float64(MaxDownloadBytes + 1)); err == nil {
		t.Error("above the ceiling should error")
	}
	if got, err := ResolveMaxBytes(float64(MaxDownloadBytes)); err != nil || got != MaxDownloadBytes {
		t.Errorf("exactly the ceiling should be allowed, got %d, %v", got, err)
	}
}

func TestReadBounded(t *testing.T) {
	// Exactly at the limit is not overflow.
	body, overflow, err := ReadBounded(strings.NewReader("12345"), 5)
	if err != nil || overflow || string(body) != "12345" {
		t.Errorf("at limit: body=%q overflow=%v err=%v", body, overflow, err)
	}
	// One byte over is.
	body, overflow, err = ReadBounded(strings.NewReader("123456"), 5)
	if err != nil || !overflow {
		t.Errorf("over limit: overflow=%v err=%v", overflow, err)
	}
	if len(body) != 5 {
		t.Errorf("body should be capped at the limit, got %d bytes", len(body))
	}
	// Under the limit.
	body, overflow, err = ReadBounded(strings.NewReader("ab"), 5)
	if err != nil || overflow || string(body) != "ab" {
		t.Errorf("under limit: body=%q overflow=%v err=%v", body, overflow, err)
	}
}

func TestOverflowMessage(t *testing.T) {
	// Below the ceiling: point at max_bytes, there is headroom.
	if got := OverflowMessage(1024); !strings.Contains(got, "raise max_bytes") {
		t.Errorf("below ceiling should suggest max_bytes, got %q", got)
	}
	// At the ceiling: suggesting max_bytes would name the value already in use.
	got := OverflowMessage(MaxDownloadBytes)
	if strings.Contains(got, "raise max_bytes") {
		t.Errorf("at ceiling should not suggest an impossible raise, got %q", got)
	}
	if !strings.Contains(got, "maximum supported size") {
		t.Errorf("at ceiling should name the hard limit, got %q", got)
	}
}

func TestResolveMaxBytesRejectsOutOfRangeFloat(t *testing.T) {
	// Converting an out-of-range float to int64 is implementation-defined, so
	// the magnitude must be checked while still in float space.
	for _, v := range []float64{1e19, -1e19, 1e30} {
		if _, err := ResolveMaxBytes(v); err == nil {
			t.Errorf("ResolveMaxBytes(%v) should be rejected", v)
		}
	}
}
