package conversation

import "testing"

func TestParserForRoute(t *testing.T) {
	r := DefaultRegistry()
	cases := []struct {
		path string
		want Provider
	}{
		{"/v1/messages", ProviderAnthropic},
		{"/v1/messages/count_tokens", ProviderAnthropic},
		{"/v1/chat/completions", ProviderOpenAI},
		{"/v1/responses", ProviderOpenAI},
	}
	for _, c := range cases {
		got := r.ParserForRoute(c.path)
		if got == nil {
			t.Fatalf("%s: expected parser, got nil", c.path)
		}
		if got.Name() != c.want {
			t.Fatalf("%s: expected %s, got %s", c.path, c.want, got.Name())
		}
	}

	if r.ParserForRoute("/api/something-else") != nil {
		t.Fatalf("unknown route should return nil")
	}
	if r.ParserForProvider(ProviderAnthropic) == nil {
		t.Fatalf("ParserForProvider(anthropic) returned nil")
	}
}
