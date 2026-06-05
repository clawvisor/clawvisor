package conversation

import (
	"net/http"
	"strings"
	"testing"
)

// TestGoogleParser_Matches pins the URL gate.
func TestGoogleParser_Matches(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want bool
	}{
		{
			name: "Gemini generateContent",
			url:  "https://generativelanguage.googleapis.com/v1beta/models/gemini-pro:generateContent",
			want: true,
		},
		{
			name: "Gemini streamGenerateContent",
			url:  "https://generativelanguage.googleapis.com/v1/models/gemini-pro:streamGenerateContent",
			want: true,
		},
		{
			name: "Wrong host (anthropic)",
			url:  "https://api.anthropic.com/v1/messages",
			want: false,
		},
		{
			name: "Gemini OAuth endpoint (not generateContent)",
			url:  "https://generativelanguage.googleapis.com/v1/auth",
			want: false,
		},
	}
	p := GoogleParser{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, c.url, nil)
			if got := p.Matches(req); got != c.want {
				t.Errorf("Matches(%s) = %v, want %v", c.url, got, c.want)
			}
		})
	}
}

// TestGoogleParser_ParseRequest_BasicTurns verifies role mapping.
func TestGoogleParser_ParseRequest_BasicTurns(t *testing.T) {
	body := []byte(`{
		"systemInstruction": {"role":"user","parts":[{"text":"You are a helpful assistant."}]},
		"contents": [
			{"role":"user","parts":[{"text":"hello"}]},
			{"role":"model","parts":[{"text":"hi there"}]}
		]
	}`)
	turns, err := GoogleParser{}.ParseRequest(body)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if len(turns) != 3 {
		t.Fatalf("expected 3 turns, got %d: %+v", len(turns), turns)
	}
	if turns[0].Role != RoleSystem || !strings.Contains(turns[0].Content, "helpful assistant") {
		t.Errorf("turn[0] = %+v, want system/helpful", turns[0])
	}
	if turns[1].Role != RoleUser || turns[1].Content != "hello" {
		t.Errorf("turn[1] = %+v, want user/hello", turns[1])
	}
	if turns[2].Role != RoleAssistant || turns[2].Content != "hi there" {
		t.Errorf("turn[2] = %+v, want assistant/hi there", turns[2])
	}
}

// TestGoogleParser_RegisteredInDefaultRegistry verifies DefaultRegistry
// returns the GoogleParser for Gemini URLs.
func TestGoogleParser_RegisteredInDefaultRegistry(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://generativelanguage.googleapis.com/v1beta/models/gemini-pro:generateContent", nil)
	parser := DefaultRegistry().Match(req)
	if parser == nil {
		t.Fatalf("DefaultRegistry returned no parser for Gemini URL")
	}
	if parser.Name() != ProviderGoogle {
		t.Errorf("parser.Name() = %v, want %v", parser.Name(), ProviderGoogle)
	}
}
