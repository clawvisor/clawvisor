package auth

import "testing"

// TestGenerateAPIToken_FormatAndPrefix asserts the canonical shape the
// server validates and the Terraform module (spec 03) generates to.
func TestGenerateAPIToken_FormatAndPrefix(t *testing.T) {
	for i := 0; i < 100; i++ {
		tok, prefix, err := GenerateAPIToken()
		if err != nil {
			t.Fatalf("GenerateAPIToken: %v", err)
		}
		if !ValidAPITokenFormat(tok) {
			t.Fatalf("generated token %q fails ValidAPITokenFormat", tok)
		}
		if len(tok) != len(APITokenPrefix)+43 {
			t.Fatalf("token len = %d, want %d", len(tok), len(APITokenPrefix)+43)
		}
		if len(prefix) != APITokenPrefixLen {
			t.Fatalf("prefix len = %d, want %d", len(prefix), APITokenPrefixLen)
		}
		if tok[:APITokenPrefixLen] != prefix {
			t.Fatalf("prefix %q is not the token head %q", prefix, tok[:APITokenPrefixLen])
		}
	}
}

// TestValidAPITokenFormat_Table is the format contract spec 03's module
// must satisfy: cvat_ + exactly 43 base64url chars, no padding.
func TestValidAPITokenFormat_Table(t *testing.T) {
	valid := "cvat_" + "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789-_AbCdE" // 43 chars
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"canonical", valid, true},
		{"empty", "", false},
		{"wrong_prefix_hex", "cvis_0123456789abcdef0123456789abcdef0123456789abcdef", false},
		{"too_short", "cvat_short", false},
		{"has_padding", "cvat_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789-_AbCd=", false},
		{"bad_char", "cvat_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789-_AbC!E", false},
		{"no_prefix", "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789-_AbCdE", false},
		{"44_chars", "cvat_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789-_AbCdEF", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidAPITokenFormat(tc.in); got != tc.want {
				t.Fatalf("ValidAPITokenFormat(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
