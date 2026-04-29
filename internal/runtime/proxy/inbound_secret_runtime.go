package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/elazarl/goproxy"

	"github.com/clawvisor/clawvisor/internal/llm"
	runtimeautovault "github.com/clawvisor/clawvisor/internal/runtime/autovault"
	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

type InboundSecretHooks struct {
	Store  store.Store
	Vault  vault.Vault
	Config *config.Config
	Logger *slog.Logger
}

type capturedSecretEntry struct {
	Placeholder string
	ServiceID   string
}

type adjudicationVerdict struct {
	Credential bool
	Service    string
	Confidence float64
}

type runtimeSecretScanner struct {
	server        *Server
	hooks         InboundSecretHooks
	session       *store.RuntimeSession
	host          string
	replacements  int
	observed      int
	sourceSet     map[string]struct{}
	serviceLabels map[string]struct{}
}

type knownPrefixSpec struct {
	Service string
	Prefix  string
}

var knownPrefixSpecs = []knownPrefixSpec{
	{Service: "anthropic", Prefix: "sk-ant-"},
	{Service: "github", Prefix: "ghp_"},
	{Service: "github", Prefix: "github_pat_"},
	{Service: "openai", Prefix: "sk-"},
	{Service: "resend", Prefix: "re_"},
	{Service: "slack", Prefix: "xoxb-"},
	{Service: "slack", Prefix: "xoxp-"},
	{Service: "stripe", Prefix: "sk_live_"},
	{Service: "stripe", Prefix: "sk_test_"},
}

func (s *Server) InstallInboundSecretCapture(hooks InboundSecretHooks) {
	if hooks.Store == nil || hooks.Vault == nil {
		return
	}
	registry := conversation.DefaultRegistry()
	s.goproxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		if req.Header.Get(internalBypassHeader) != "" {
			return req, nil
		}
		st := EnsureState(ctx)
		if st.Session == nil || req.Body == nil {
			return req, nil
		}
		parser := registry.Match(req)
		if parser == nil {
			if runtimeConversationHost(req) {
				emitRuntimeEvent(req.Context(), hooks.Store, st.Session, st, runtimeEventOptions{
					EventType: "runtime.provider.unsupported_surface",
					Reason:    stringPtr("runtime provider surface is outside the supported v1 request matrix"),
					Metadata:  map[string]any{"host": requestHost(req), "path": req.URL.Path},
				})
			}
			return req, nil
		}

		body, err := io.ReadAll(req.Body)
		if err != nil {
			return req, nil
		}
		rewritten, summary, observed, err := s.scanAndReplaceRuntimeSecrets(req.Context(), hooks, st.Session, requestHost(req), body)
		if err == nil && summary != nil {
			if st.Runtime == nil {
				st.Runtime = &RuntimeRequestContext{}
			}
			st.Runtime.SecretScan = summary
			if summary.ReplacementCount > 0 {
				emitRuntimeEvent(req.Context(), hooks.Store, st.Session, st, runtimeEventOptions{
					EventType:  "runtime.autovault.captured",
					ActionKind: "conversation",
					Decision:   stringPtr("capture"),
					Outcome:    stringPtr("rewritten"),
					Reason:     stringPtr("runtime inbound secret capture replaced pasted secrets with placeholders"),
					Metadata: map[string]any{
						"replacement_count": summary.ReplacementCount,
						"sources":           summary.Sources,
					},
				})
			}
			if observed > 0 {
				emitRuntimeEvent(req.Context(), hooks.Store, st.Session, st, runtimeEventOptions{
					EventType:  "runtime.autovault.observed",
					ActionKind: "conversation",
					Decision:   stringPtr("observe"),
					Outcome:    stringPtr("detected"),
					Reason:     stringPtr("runtime inbound secret scan observed candidate secrets without replacement"),
					Metadata: map[string]any{
						"observed_count": observed,
						"sources":        summary.Sources,
					},
				})
			}
			body = rewritten
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		return req, nil
	})
}

func (s *Server) scanAndReplaceRuntimeSecrets(ctx context.Context, hooks InboundSecretHooks, session *store.RuntimeSession, host string, body []byte) ([]byte, *SecretScanSummary, int, error) {
	if session == nil || len(body) == 0 || !json.Valid(body) {
		return body, nil, 0, nil
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil, 0, nil
	}
	scanner := &runtimeSecretScanner{
		server:        s,
		hooks:         hooks,
		session:       session,
		host:          host,
		sourceSet:     map[string]struct{}{},
		serviceLabels: map[string]struct{}{},
	}
	rewritten, changed := scanner.walk(ctx, payload, "")
	if !changed && scanner.observed == 0 {
		return body, nil, 0, nil
	}
	encoded, err := json.Marshal(rewritten)
	if err != nil {
		return body, nil, 0, nil
	}
	sources := make([]string, 0, len(scanner.sourceSet))
	for source := range scanner.sourceSet {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	return encoded, &SecretScanSummary{
		ReplacementCount: scanner.replacements,
		Sources:          sources,
	}, scanner.observed, nil
}

func (s *runtimeSecretScanner) walk(ctx context.Context, value any, fieldName string) (any, bool) {
	switch typed := value.(type) {
	case string:
		rewritten, changed := s.rewriteString(ctx, typed, fieldName)
		return rewritten, changed
	case map[string]any:
		changed := false
		for key, item := range typed {
			next, nextChanged := s.walk(ctx, item, key)
			if nextChanged {
				typed[key] = next
				changed = true
			}
		}
		return typed, changed
	case []any:
		changed := false
		for i, item := range typed {
			next, nextChanged := s.walk(ctx, item, fieldName)
			if nextChanged {
				typed[i] = next
				changed = true
			}
		}
		return typed, changed
	default:
		return value, false
	}
}

func (s *runtimeSecretScanner) rewriteString(ctx context.Context, value string, fieldName string) (string, bool) {
	original := value
	replaced := false

	for _, spec := range knownPrefixSpecs {
		if !strings.Contains(value, spec.Prefix) {
			continue
		}
		re := prefixRegexFor(spec.Prefix)
		value = re.ReplaceAllStringFunc(value, func(match string) string {
			if runtimeautovault.LooksLikeShadow(match) {
				return match
			}
			placeholder, err := s.placeholderForValue(ctx, spec.Service, match)
			if err != nil {
				return match
			}
			replaced = true
			s.replacements++
			s.sourceSet["known_prefix"] = struct{}{}
			s.serviceLabels[spec.Service] = struct{}{}
			return placeholder
		})
	}

	for _, candidate := range runtimeautovault.DetectCandidates(value) {
		if runtimeautovault.LooksLikeShadow(candidate.Value) {
			continue
		}
		if placeholder, ok := s.lookupReusablePlaceholder(candidate.Value); ok {
			value = strings.ReplaceAll(value, candidate.Value, placeholder)
			replaced = true
			s.replacements++
			s.sourceSet["value_reuse"] = struct{}{}
			continue
		}
		if highContextSecretField(fieldName) || secretContextHint(value, candidate.Value) {
			placeholder, err := s.placeholderForValue(ctx, guessService(fieldName, value), candidate.Value)
			if err != nil {
				continue
			}
			value = strings.ReplaceAll(value, candidate.Value, placeholder)
			replaced = true
			s.replacements++
			s.sourceSet["heuristic_swap"] = struct{}{}
			continue
		}
		verdict, ok := s.lookupOrAdjudicate(ctx, fieldName, value, candidate)
		if ok && verdict.Credential && verdict.Confidence >= 0.6 {
			placeholder, err := s.placeholderForValue(ctx, firstNonEmpty(normalizeSecretService(verdict.Service), guessService(fieldName, value)), candidate.Value)
			if err != nil {
				continue
			}
			value = strings.ReplaceAll(value, candidate.Value, placeholder)
			replaced = true
			s.replacements++
			s.sourceSet["heuristic_adjudicated"] = struct{}{}
			continue
		}
		s.observed++
		s.sourceSet["heuristic_observe"] = struct{}{}
	}

	for _, passwordValue := range runtimeautovault.FindPasswordRevealCandidates(value) {
		if runtimeautovault.LooksLikeShadow(passwordValue) {
			continue
		}
		placeholder, ok := s.lookupReusablePlaceholder(passwordValue)
		if !ok {
			var err error
			placeholder, err = s.placeholderForValue(ctx, guessService(fieldName, value), passwordValue)
			if err != nil {
				continue
			}
		}
		value = strings.ReplaceAll(value, passwordValue, placeholder)
		replaced = true
		s.replacements++
		s.sourceSet["password_reveal"] = struct{}{}
	}

	return value, replaced && value != original
}

func (s *runtimeSecretScanner) placeholderForValue(ctx context.Context, service, raw string) (string, error) {
	if placeholder, ok := s.lookupReusablePlaceholder(raw); ok {
		return placeholder, nil
	}
	service = normalizeSecretService(service)
	if service == "" {
		service = "captured"
	}
	placeholder, err := runtimeautovault.GeneratePlaceholder(runtimeautovault.PlaceholderPrefix(service))
	if err != nil {
		return "", err
	}
	serviceID := "runtime.captured." + service + "." + placeholder
	if err := s.hooks.Vault.Set(ctx, s.session.UserID, serviceID, []byte(raw)); err != nil {
		return "", err
	}
	if err := s.hooks.Store.CreateRuntimePlaceholder(ctx, &store.RuntimePlaceholder{
		Placeholder: placeholder,
		UserID:      s.session.UserID,
		AgentID:     s.session.AgentID,
		ServiceID:   serviceID,
	}); err != nil {
		return "", err
	}
	s.server.secretValueCache.Store(secretValueCacheKey(s.session.AgentID, raw), capturedSecretEntry{
		Placeholder: placeholder,
		ServiceID:   serviceID,
	})
	s.serviceLabels[service] = struct{}{}
	return placeholder, nil
}

func (s *runtimeSecretScanner) lookupReusablePlaceholder(raw string) (string, bool) {
	value, ok := s.server.secretValueCache.Load(secretValueCacheKey(s.session.AgentID, raw))
	if !ok {
		return "", false
	}
	entry, _ := value.(capturedSecretEntry)
	return entry.Placeholder, entry.Placeholder != ""
}

func (s *runtimeSecretScanner) lookupOrAdjudicate(ctx context.Context, fieldName, content string, candidate runtimeautovault.Candidate) (adjudicationVerdict, bool) {
	key := adjudicationCacheKey(s.host, fieldName, candidate.Charset, redactedCandidateContext(content, candidate.Value))
	if cached, ok := s.server.secretVerdictCache.Load(key); ok {
		verdict, _ := cached.(adjudicationVerdict)
		return verdict, true
	}
	cfg := verificationConfig(s.hooks.Config)
	if !cfg.Enabled || cfg.Endpoint == "" || cfg.Model == "" {
		return adjudicationVerdict{}, false
	}
	client := llm.NewClient(cfg.LLMProviderConfig).WithMaxTokens(250)
	raw, err := client.Complete(ctx, []llm.ChatMessage{
		{Role: "system", Content: runtimeSecretAdjudicatorSystemPrompt},
		{Role: "user", Content: buildSecretAdjudicatorPrompt(s.host, fieldName, content, candidate)},
	})
	if err != nil {
		if s.hooks.Logger != nil {
			s.hooks.Logger.Warn("runtime secret adjudicator failed", "err", err, "host", s.host, "field", fieldName)
		}
		return adjudicationVerdict{}, false
	}
	verdict, err := parseSecretAdjudicatorVerdict(raw)
	if err != nil {
		return adjudicationVerdict{}, false
	}
	s.server.secretVerdictCache.Store(key, verdict)
	return verdict, true
}

func secretValueCacheKey(agentID, raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return agentID + ":" + hex.EncodeToString(sum[:])
}

func adjudicationCacheKey(host, fieldName, charset, contextWindow string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(host) + "\n" + strings.ToLower(fieldName) + "\n" + charset + "\n" + contextWindow))
	return hex.EncodeToString(sum[:])
}

func prefixRegexFor(prefix string) *regexp.Regexp {
	return regexp.MustCompile(regexp.QuoteMeta(prefix) + `[A-Za-z0-9_-]{4,}`)
}

func highContextSecretField(fieldName string) bool {
	field := strings.ToLower(strings.TrimSpace(fieldName))
	for _, token := range []string{"api_key", "apikey", "access_token", "token", "authorization", "auth", "secret", "password", "passcode"} {
		if field == token || strings.Contains(field, token) {
			return true
		}
	}
	return false
}

func secretContextHint(content, candidate string) bool {
	lower := strings.ToLower(content)
	lower = strings.ReplaceAll(lower, candidate, "<candidate>")
	for _, hint := range []string{"api key", "access token", "authorization", "bearer", "password", "secret", "token"} {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

func guessService(fieldName, content string) string {
	lower := strings.ToLower(fieldName + " " + content)
	for _, spec := range knownPrefixSpecs {
		if strings.Contains(lower, spec.Service) {
			return spec.Service
		}
	}
	return "captured"
}

func normalizeSecretService(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, " ", "_")
	value = strings.ReplaceAll(value, "/", "_")
	value = strings.ReplaceAll(value, ".", "_")
	if value == "" {
		return ""
	}
	return value
}

func redactedCandidateContext(content, candidate string) string {
	return strings.ReplaceAll(content, candidate, "<TOKEN_CANDIDATE_1>")
}

func buildSecretAdjudicatorPrompt(host, fieldName, content string, candidate runtimeautovault.Candidate) string {
	return fmt.Sprintf(`Host: %s
Field: %s
Candidate charset: %s
Candidate entropy: %.2f
Redacted context:
%s

Decide whether <TOKEN_CANDIDATE_1> is a real credential that should be captured for later placeholder swap. Return strict JSON:
{"credential":true|false,"service":"service-name-or-empty","confidence":0.0-1.0}`,
		host,
		fieldName,
		candidate.Charset,
		candidate.Entropy,
		redactedCandidateContext(content, candidate.Value),
	)
}

func parseSecretAdjudicatorVerdict(raw string) (adjudicationVerdict, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)
	var verdict adjudicationVerdict
	if err := json.Unmarshal([]byte(raw), &verdict); err != nil {
		return adjudicationVerdict{}, err
	}
	return verdict, nil
}

func verificationConfig(cfg *config.Config) config.VerificationConfig {
	if cfg == nil {
		return config.VerificationConfig{}
	}
	return cfg.LLM.Verification
}

func runtimeConversationHost(req *http.Request) bool {
	switch requestHost(req) {
	case "api.anthropic.com", "api.openai.com", "chatgpt.com":
		return true
	default:
		return false
	}
}

const runtimeSecretAdjudicatorSystemPrompt = `You classify redacted candidate strings inside LLM conversation requests.

Rules:
- The candidate value is always redacted as <TOKEN_CANDIDATE_1>.
- Decide whether it is likely a real credential or secret that should be captured and replaced with a placeholder.
- Prefer false when the context is weak or the value looks like an ordinary identifier.
- Return strict JSON only:
  {"credential":true|false,"service":"service-name-or-empty","confidence":0.0-1.0}`
