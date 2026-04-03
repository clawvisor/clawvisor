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
