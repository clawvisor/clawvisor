package conversation

// ParserForProvider returns the registered parser for a given provider name.
// The runtime proxy uses host-based Match(req); the lite-proxy LLM endpoint
// dispatches by route (the request host is clawvisor.example, not the
// upstream's host), so it needs an explicit lookup.
//
// Returns nil for unknown providers.
func (r *Registry) ParserForProvider(p Provider) Parser {
	if r == nil {
		return nil
	}
	for _, parser := range r.parsers {
		if parser.Name() == p {
			return parser
		}
	}
	return nil
}

// ParserForRoute returns the parser keyed off the lite-proxy route path. The
// lite-proxy exposes /v1/messages (Anthropic) and /v1/chat/completions +
// /v1/responses (OpenAI). When future providers ship their own routes, extend
// here.
//
// Returns nil for unrecognized paths.
func (r *Registry) ParserForRoute(path string) Parser {
	switch {
	case path == "/v1/messages" || path == "/v1/messages/count_tokens":
		return r.ParserForProvider(ProviderAnthropic)
	case path == "/v1/chat/completions" || path == "/v1/responses":
		return r.ParserForProvider(ProviderOpenAI)
	}
	return nil
}
