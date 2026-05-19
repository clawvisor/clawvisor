package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/google/uuid"

	"github.com/clawvisor/clawvisor/internal/llm"
	runtimeautovault "github.com/clawvisor/clawvisor/internal/runtime/autovault"
	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy"
	runtimetiming "github.com/clawvisor/clawvisor/internal/runtime/timing"
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

type adjudicationVerdict = runtimeautovault.SecretAdjudicationVerdict

// adjudicationDebugRecord is a single observation suitable for emitting to a
// debug log when CLAWVISOR_RUNTIME_PROXY_ADJUDICATION_DEBUG_DIR is set. It
// captures both cache-hit and cache-miss paths so we can verify cache shape
// behavior in production.
type adjudicationDebugRecord struct {
	Host       string
	Field      string
	Candidate  string
	Charset    string
	Entropy    float64
	CacheHit   bool
	Concurrent bool
	Raw        string
	Verdict    *adjudicationVerdict
	Duration   time.Duration
	Err        error
	ParseErr   error
}

type runtimeSecretScanner struct {
	server           *Server
	hooks            InboundSecretHooks
	session          *store.RuntimeSession
	host             string
	replacements     int
	observed         int
	sourceSet        map[string]struct{}
	serviceLabels    map[string]struct{}
	metrics          map[string]time.Duration
	stringsSeen      int
	skippedFields    int
	skippedNoise     int
	candidates       int
	passwords        int
	adjudications    int
	cacheHits        int
	prefilteredNoise int

	// adjudMu guards adjudications, cacheHits, and metrics["...adjudicate"]
	// during the parallel prewarm pass. The sequential walk doesn't need it,
	// but using it consistently keeps the locking obvious.
	adjudMu sync.Mutex
}

var runtimeNoiseSubtreeKeys = map[string]bool{
	"system":          true,
	"tools":           true,
	"tool_choice":     true,
	"response_format": true,
	"model":           true,
	"metadata":        true,
}

var runtimeContextNoisePrefixes = []string{
	"As you answer the user's questions, you can use the following context:",
	"# claudeMd",
	"Contents of ",
}

var runtimeProtectedStringFields = map[string]bool{
	"signature": true,
	"thinking":  true,
}

var runtimeHarnessMetadataTags = []string{
	"system-reminder",
	"available-deferred-tools",
	"command-name",
	"command-message",
	"local-command-caveat",
}

var runtimeHarnessMetadataREs = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(runtimeHarnessMetadataTags))
	for _, tag := range runtimeHarnessMetadataTags {
		out = append(out, regexp.MustCompile(`(?s)<`+regexp.QuoteMeta(tag)+`>.*?</`+regexp.QuoteMeta(tag)+`>`))
	}
	return out
}()

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

		readStartedAt := time.Now()
		body, err := io.ReadAll(req.Body)
		s.recordTimingSpan(req, "inbound_secret.read_body", readStartedAt)
		if err != nil {
			return req, nil
		}
		scanStartedAt := time.Now()
		rewritten, summary, observed, err := s.scanAndReplaceRuntimeSecrets(req.Context(), hooks, st.Session, requestHost(req), body)
		s.recordTimingSpan(req, "inbound_secret.scan", scanStartedAt)
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
	unmarshalStartedAt := time.Now()
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil, 0, nil
	}
	runtimetiming.RecordSpan(ctx, "inbound_secret.scan.unmarshal", time.Since(unmarshalStartedAt))
	scanner := &runtimeSecretScanner{
		server:        s,
		hooks:         hooks,
		session:       session,
		host:          host,
		sourceSet:     map[string]struct{}{},
		serviceLabels: map[string]struct{}{},
		metrics:       map[string]time.Duration{},
	}
	defer scanner.flushMetrics(ctx)
	prewarmStartedAt := time.Now()
	scanner.prewarmVerdicts(ctx, payload)
	runtimetiming.RecordSpan(ctx, "inbound_secret.scan.prewarm", time.Since(prewarmStartedAt))
	walkStartedAt := time.Now()
	rewritten, changed := scanner.walk(ctx, payload, "", true, false)
	runtimetiming.RecordSpan(ctx, "inbound_secret.scan.walk", time.Since(walkStartedAt))
	if !changed && scanner.observed == 0 {
		return body, nil, 0, nil
	}
	marshalStartedAt := time.Now()
	encoded, err := json.Marshal(rewritten)
	runtimetiming.RecordSpan(ctx, "inbound_secret.scan.marshal", time.Since(marshalStartedAt))
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

func (s *runtimeSecretScanner) walk(ctx context.Context, value any, fieldName string, topLevel bool, skipHeuristic bool) (any, bool) {
	switch typed := value.(type) {
	case string:
		if runtimeautovault.ProtectedStringField(fieldName) {
			s.skippedFields++
			return value, false
		}
		rewritten, changed := s.rewriteString(ctx, typed, fieldName, skipHeuristic)
		return rewritten, changed
	case map[string]any:
		if strings.EqualFold(stringValueFromMap(typed, "type"), "thinking") {
			return value, false
		}
		changed := false
		for key, item := range typed {
			childSkipHeuristic := skipHeuristic || (topLevel && runtimeautovault.NoiseSubtreeKey(key))
			next, nextChanged := s.walk(ctx, item, key, false, childSkipHeuristic)
			if nextChanged {
				typed[key] = next
				changed = true
			}
		}
		return typed, changed
	case []any:
		changed := false
		for i, item := range typed {
			next, nextChanged := s.walk(ctx, item, fieldName, false, skipHeuristic)
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

func (s *runtimeSecretScanner) rewriteString(ctx context.Context, value string, fieldName string, skipHeuristic bool) (string, bool) {
	original := value
	replaced := false
	s.stringsSeen++

	knownPrefixStartedAt := time.Now()
	for _, spec := range runtimeautovault.KnownPrefixSpecs() {
		if !strings.Contains(value, spec.Prefix) {
			continue
		}
		re := prefixRegexFor(spec.Prefix)
		value = re.ReplaceAllStringFunc(value, func(match string) string {
			leading, secret := runtimeautovault.SplitPrefixRegexMatch(spec.Prefix, match)
			if runtimeautovault.LooksLikeShadow(secret) || runtimeautovault.LooksLikeIdentifier(secret) {
				return match
			}
			placeholder, err := s.placeholderForValue(ctx, spec.Service, secret)
			if err != nil {
				return match
			}
			replaced = true
			s.replacements++
			s.sourceSet["known_prefix"] = struct{}{}
			s.serviceLabels[spec.Service] = struct{}{}
			return leading + placeholder
		})
	}
	s.addMetric("inbound_secret.scan.known_prefix", time.Since(knownPrefixStartedAt))

	if skipHeuristic {
		return value, replaced && value != original
	}

	protocolNoiseStartedAt := time.Now()
	if runtimeLooksLikeProtocolNoise(fieldName, value) {
		s.addMetric("inbound_secret.scan.protocol_noise_check", time.Since(protocolNoiseStartedAt))
		s.skippedNoise++
		return value, replaced && value != original
	}
	s.addMetric("inbound_secret.scan.protocol_noise_check", time.Since(protocolNoiseStartedAt))
	contextNoiseStartedAt := time.Now()
	if runtimeLooksLikeContextNoise(value) {
		s.addMetric("inbound_secret.scan.context_noise_check", time.Since(contextNoiseStartedAt))
		s.skippedNoise++
		return value, replaced && value != original
	}
	s.addMetric("inbound_secret.scan.context_noise_check", time.Since(contextNoiseStartedAt))

	stripStartedAt := time.Now()
	scannable := stripRuntimeHarnessMetadataTags(value)
	s.addMetric("inbound_secret.scan.strip_tags", time.Since(stripStartedAt))
	detectStartedAt := time.Now()
	candidates := runtimeautovault.DetectCandidates(scannable)
	s.addMetric("inbound_secret.scan.detect_candidates", time.Since(detectStartedAt))
	s.candidates += len(candidates)
	for _, candidate := range candidates {
		if runtimeautovault.LooksLikeShadow(candidate.Value) {
			continue
		}
		if placeholder, ok := s.lookupReusablePlaceholder(ctx, candidate.Value); ok {
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
		if looksObviouslyNonSecret(candidate.Value) {
			s.prefilteredNoise++
			s.observed++
			s.sourceSet["heuristic_observe"] = struct{}{}
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

	passwordStartedAt := time.Now()
	passwordValues := runtimeautovault.FindPasswordRevealCandidates(scannable)
	s.addMetric("inbound_secret.scan.find_passwords", time.Since(passwordStartedAt))
	s.passwords += len(passwordValues)
	for _, passwordValue := range passwordValues {
		if runtimeautovault.LooksLikeShadow(passwordValue) {
			continue
		}
		placeholder, ok := s.lookupReusablePlaceholder(ctx, passwordValue)
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

func (s *runtimeSecretScanner) addMetric(name string, d time.Duration) {
	if s == nil || name == "" || d < 0 {
		return
	}
	if s.metrics == nil {
		s.metrics = map[string]time.Duration{}
	}
	s.metrics[name] += d
}

func (s *runtimeSecretScanner) flushMetrics(ctx context.Context) {
	if s == nil {
		return
	}
	for name, d := range s.metrics {
		runtimetiming.RecordSpan(ctx, name, d)
	}
	runtimetiming.SetAttr(ctx, "inbound_secret.scan.strings_seen", s.stringsSeen)
	runtimetiming.SetAttr(ctx, "inbound_secret.scan.skipped_fields", s.skippedFields)
	runtimetiming.SetAttr(ctx, "inbound_secret.scan.skipped_noise", s.skippedNoise)
	runtimetiming.SetAttr(ctx, "inbound_secret.scan.candidates", s.candidates)
	runtimetiming.SetAttr(ctx, "inbound_secret.scan.passwords", s.passwords)
	runtimetiming.SetAttr(ctx, "inbound_secret.scan.adjudications", s.adjudications)
	runtimetiming.SetAttr(ctx, "inbound_secret.scan.cache_hits", s.cacheHits)
	runtimetiming.SetAttr(ctx, "inbound_secret.scan.prefiltered_noise", s.prefilteredNoise)
}

func runtimeLooksLikeContextNoise(value string) bool {
	return runtimeautovault.LooksLikeContextNoise(value)
}

func runtimeLooksLikeProtocolNoise(fieldName, value string) bool {
	return runtimeautovault.LooksLikeProtocolNoise(fieldName, value)
}

func stripRuntimeHarnessMetadataTags(value string) string {
	return runtimeautovault.StripHarnessMetadataTags(value)
}

func (s *runtimeSecretScanner) placeholderForValue(ctx context.Context, service, raw string) (string, error) {
	placeholder, err := captureRuntimeSecret(ctx, s.server, s.hooks.Store, s.hooks.Vault, s.session, s.host, service, raw)
	if err == nil {
		s.serviceLabels[normalizeSecretService(service)] = struct{}{}
	}
	return placeholder, err
}

func (s *runtimeSecretScanner) lookupReusablePlaceholder(ctx context.Context, raw string) (string, bool) {
	return lookupRuntimeSecretPlaceholder(ctx, s.server, s.hooks.Store, s.session, raw)
}

func (s *runtimeSecretScanner) recordAdjudicationDebug(rec adjudicationDebugRecord) {
	if s == nil || s.server == nil || s.server.adjudicationDebugDir == "" {
		return
	}
	row := map[string]any{
		"timestamp":   time.Now().UTC().Format(time.RFC3339Nano),
		"host":        rec.Host,
		"field":       rec.Field,
		"candidate":   rec.Candidate,
		"charset":     rec.Charset,
		"entropy":     rec.Entropy,
		"cache_hit":   rec.CacheHit,
		"concurrent":  rec.Concurrent,
		"duration_ms": rec.Duration.Milliseconds(),
	}
	if rec.Raw != "" {
		row["raw_response"] = rec.Raw
	}
	if rec.Verdict != nil {
		row["verdict"] = map[string]any{
			"credential": rec.Verdict.Credential,
			"service":    rec.Verdict.Service,
			"confidence": rec.Verdict.Confidence,
		}
	}
	if rec.Err != nil {
		row["err"] = rec.Err.Error()
	}
	if rec.ParseErr != nil {
		row["parse_err"] = rec.ParseErr.Error()
	}
	if s.session != nil {
		row["session_id"] = s.session.ID
		row["agent_id"] = s.session.AgentID
	}
	s.server.writeAdjudicationDebug(row)
}

// adjudicationTask is a single LLM-bound verdict request collected during the
// prewarm pass. We dedupe by cache key so repeated occurrences of the same
// (host, field, charset, content) tuple only issue one LLM call.
type adjudicationTask struct {
	fieldName string
	content   string
	candidate runtimeautovault.Candidate
	cacheKey  string
}

// prewarmVerdicts walks the JSON payload, collects unique adjudication tasks
// that would otherwise run sequentially during the replacement walk, groups
// them by candidate value, and issues at most one LLM call per (host, value)
// in parallel with bounded concurrency. The verdict for that value is then
// applied to every task referencing the same secret regardless of context.
func (s *runtimeSecretScanner) prewarmVerdicts(ctx context.Context, payload any) {
	cfg := verificationConfig(s.hooks.Config)
	if !runtimeautovault.SecretAdjudicatorConfigured(cfg) {
		return
	}
	seen := map[string]struct{}{}
	var tasks []adjudicationTask
	s.collectAdjudicationTasks(payload, "", true, false, seen, &tasks)
	if len(tasks) == 0 {
		return
	}
	byValue := map[string][]adjudicationTask{}
	var values []string
	for _, task := range tasks {
		v := task.candidate.Value
		if _, ok := byValue[v]; !ok {
			values = append(values, v)
		}
		byValue[v] = append(byValue[v], task)
	}
	const concurrency = 8
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	client := llm.NewClient(cfg.LLMProviderConfig).WithMaxTokens(250)
	for _, value := range values {
		group := byValue[value]
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			adjudicationCtx, cancel := context.WithTimeout(ctx, runtimeAdjudicationTimeout(cfg))
			defer cancel()
			s.runAdjudicationGroup(adjudicationCtx, client, group)
		}()
	}
	wg.Wait()
}

// runAdjudicationGroup decides the verdict for a candidate value once and
// applies it to every task referencing that value within the request. It
// short-circuits on the cross-request positive value cache so a secret that
// was identified as a credential in any prior request never gets re-asked.
func (s *runtimeSecretScanner) runAdjudicationGroup(ctx context.Context, client *llm.Client, group []adjudicationTask) {
	if len(group) == 0 {
		return
	}
	for _, task := range group {
		if v, ok := s.server.secretVerdictCache.Load(task.cacheKey); ok {
			verdict := v.(adjudicationVerdict)
			for _, other := range group {
				s.server.secretVerdictCache.Store(other.cacheKey, verdict)
			}
			return
		}
	}
	if verdict, ok := s.server.sharedSecretVerdictGet(group[0].cacheKey); ok {
		for _, task := range group {
			s.server.secretVerdictCache.Store(task.cacheKey, verdict)
		}
		return
	}
	valueKey := secretValueVerdictKey(s.host, group[0].candidate.Value)
	if verdict, ok := s.server.secretValueVerdictCache.Get(valueKey); ok {
		cachedVerdict, _ := verdict.(adjudicationVerdict)
		for _, task := range group {
			s.server.secretVerdictCache.Store(task.cacheKey, cachedVerdict)
		}
		return
	}
	if verdict, ok := s.server.sharedSecretValueVerdictGet(s.host, group[0].candidate.Value); ok {
		for _, task := range group {
			s.server.secretVerdictCache.Store(task.cacheKey, verdict)
		}
		s.server.secretValueVerdictCache.Set(valueKey, verdict)
		return
	}
	rep := group[0]
	startedAt := time.Now()
	raw, err := client.Complete(ctx, []llm.ChatMessage{
		{Role: "system", Content: runtimeautovault.SecretAdjudicatorSystemPrompt},
		{Role: "user", Content: buildSecretAdjudicatorPrompt(s.host, rep.fieldName, rep.content, rep.candidate)},
	})
	duration := time.Since(startedAt)
	debugRec := adjudicationDebugRecord{
		Host:       s.host,
		Field:      rep.fieldName,
		Candidate:  rep.candidate.Value,
		Charset:    rep.candidate.Charset,
		Entropy:    rep.candidate.Entropy,
		CacheHit:   false,
		Concurrent: true,
		Raw:        raw,
		Duration:   duration,
		Err:        err,
	}
	s.adjudMu.Lock()
	s.metrics["inbound_secret.scan.adjudicate"] += duration
	s.adjudications++
	s.adjudMu.Unlock()
	if err != nil {
		s.recordAdjudicationDebug(debugRec)
		if s.hooks.Logger != nil {
			s.hooks.Logger.Warn("runtime secret adjudicator failed", "err", err, "host", s.host, "field", rep.fieldName)
		}
		return
	}
	verdict, perr := parseSecretAdjudicatorVerdict(raw)
	if perr != nil {
		debugRec.ParseErr = perr
		s.recordAdjudicationDebug(debugRec)
		if s.hooks.Logger != nil {
			s.hooks.Logger.Warn("runtime secret adjudicator parse failed",
				"err", perr, "host", s.host, "field", rep.fieldName, "raw_len", len(raw))
		}
		return
	}
	debugRec.Verdict = &verdict
	s.recordAdjudicationDebug(debugRec)
	for _, task := range group {
		s.server.secretVerdictCache.Store(task.cacheKey, verdict)
		s.server.sharedSecretVerdictSet(task.cacheKey, verdict)
	}
	if verdict.Credential {
		s.server.secretValueVerdictCache.Set(valueKey, verdict)
		s.server.sharedSecretValueVerdictSet(s.host, group[0].candidate.Value, verdict)
	}
}

func secretValueVerdictKey(host, value string) string {
	return host + "\x1f" + value
}

// collectAdjudicationTasks mirrors walk()/rewriteString() filtering rules
// without mutating the payload or scanner counters. It enqueues unique
// (cacheKey) adjudication tasks for the prewarm fan-out.
func (s *runtimeSecretScanner) collectAdjudicationTasks(value any, fieldName string, topLevel, skipHeuristic bool, seen map[string]struct{}, out *[]adjudicationTask) {
	switch typed := value.(type) {
	case string:
		if runtimeautovault.ProtectedStringField(fieldName) {
			return
		}
		if skipHeuristic {
			return
		}
		if runtimeLooksLikeProtocolNoise(fieldName, typed) {
			return
		}
		if runtimeLooksLikeContextNoise(typed) {
			return
		}
		scannable := stripRuntimeHarnessMetadataTags(typed)
		for _, candidate := range runtimeautovault.DetectCandidates(scannable) {
			if runtimeautovault.LooksLikeShadow(candidate.Value) {
				continue
			}
			if _, ok := s.lookupReusablePlaceholder(context.Background(), candidate.Value); ok {
				continue
			}
			if highContextSecretField(fieldName) || secretContextHint(typed, candidate.Value) {
				continue
			}
			if looksObviouslyNonSecret(candidate.Value) {
				continue
			}
			key := adjudicationCacheKey(s.host, fieldName, candidate.Charset, redactedCandidateContext(typed, candidate.Value))
			if _, ok := seen[key]; ok {
				continue
			}
			if _, ok := s.server.secretVerdictCache.Load(key); ok {
				continue
			}
			seen[key] = struct{}{}
			*out = append(*out, adjudicationTask{
				fieldName: fieldName,
				content:   typed,
				candidate: candidate,
				cacheKey:  key,
			})
		}
	case map[string]any:
		if strings.EqualFold(stringValueFromMap(typed, "type"), "thinking") {
			return
		}
		for k, v := range typed {
			childSkip := skipHeuristic || (topLevel && runtimeautovault.NoiseSubtreeKey(k))
			s.collectAdjudicationTasks(v, k, false, childSkip, seen, out)
		}
	case []any:
		for _, item := range typed {
			s.collectAdjudicationTasks(item, fieldName, false, skipHeuristic, seen, out)
		}
	}
}

func (s *runtimeSecretScanner) lookupOrAdjudicate(ctx context.Context, fieldName, content string, candidate runtimeautovault.Candidate) (adjudicationVerdict, bool) {
	key := adjudicationCacheKey(s.host, fieldName, candidate.Charset, redactedCandidateContext(content, candidate.Value))
	if cached, ok := s.server.secretVerdictCache.Load(key); ok {
		verdict, _ := cached.(adjudicationVerdict)
		s.cacheHits++
		s.recordAdjudicationDebug(adjudicationDebugRecord{
			Host:      s.host,
			Field:     fieldName,
			Candidate: candidate.Value,
			Charset:   candidate.Charset,
			Entropy:   candidate.Entropy,
			CacheHit:  true,
			Verdict:   &verdict,
		})
		return verdict, true
	}
	if verdict, ok := s.server.sharedSecretVerdictGet(key); ok {
		s.cacheHits++
		s.server.secretVerdictCache.Store(key, verdict)
		s.recordAdjudicationDebug(adjudicationDebugRecord{
			Host:      s.host,
			Field:     fieldName,
			Candidate: candidate.Value,
			Charset:   candidate.Charset,
			Entropy:   candidate.Entropy,
			CacheHit:  true,
			Verdict:   &verdict,
		})
		return verdict, true
	}
	valueKey := secretValueVerdictKey(s.host, candidate.Value)
	if cached, ok := s.server.secretValueVerdictCache.Get(valueKey); ok {
		verdict, _ := cached.(adjudicationVerdict)
		s.cacheHits++
		s.server.secretVerdictCache.Store(key, verdict)
		s.recordAdjudicationDebug(adjudicationDebugRecord{
			Host:      s.host,
			Field:     fieldName,
			Candidate: candidate.Value,
			Charset:   candidate.Charset,
			Entropy:   candidate.Entropy,
			CacheHit:  true,
			Verdict:   &verdict,
		})
		return verdict, true
	}
	if verdict, ok := s.server.sharedSecretValueVerdictGet(s.host, candidate.Value); ok {
		s.cacheHits++
		s.server.secretVerdictCache.Store(key, verdict)
		s.server.secretValueVerdictCache.Set(valueKey, verdict)
		s.recordAdjudicationDebug(adjudicationDebugRecord{
			Host:      s.host,
			Field:     fieldName,
			Candidate: candidate.Value,
			Charset:   candidate.Charset,
			Entropy:   candidate.Entropy,
			CacheHit:  true,
			Verdict:   &verdict,
		})
		return verdict, true
	}
	cfg := verificationConfig(s.hooks.Config)
	if !runtimeautovault.SecretAdjudicatorConfigured(cfg) {
		return adjudicationVerdict{}, false
	}
	client := llm.NewClient(cfg.LLMProviderConfig).WithMaxTokens(250)
	adjudicateStartedAt := time.Now()
	adjudicationCtx, cancel := context.WithTimeout(ctx, runtimeAdjudicationTimeout(cfg))
	defer cancel()
	raw, err := client.Complete(adjudicationCtx, []llm.ChatMessage{
		{Role: "system", Content: runtimeautovault.SecretAdjudicatorSystemPrompt},
		{Role: "user", Content: buildSecretAdjudicatorPrompt(s.host, fieldName, content, candidate)},
	})
	duration := time.Since(adjudicateStartedAt)
	s.adjudMu.Lock()
	s.metrics["inbound_secret.scan.adjudicate"] += duration
	s.adjudications++
	s.adjudMu.Unlock()
	debugRec := adjudicationDebugRecord{
		Host:      s.host,
		Field:     fieldName,
		Candidate: candidate.Value,
		Charset:   candidate.Charset,
		Entropy:   candidate.Entropy,
		CacheHit:  false,
		Raw:       raw,
		Duration:  duration,
		Err:       err,
	}
	if err != nil {
		s.recordAdjudicationDebug(debugRec)
		if s.hooks.Logger != nil {
			s.hooks.Logger.Warn("runtime secret adjudicator failed", "err", err, "host", s.host, "field", fieldName)
		}
		return adjudicationVerdict{}, false
	}
	verdict, perr := parseSecretAdjudicatorVerdict(raw)
	if perr != nil {
		debugRec.ParseErr = perr
		s.recordAdjudicationDebug(debugRec)
		if s.hooks.Logger != nil {
			s.hooks.Logger.Warn("runtime secret adjudicator parse failed",
				"err", perr, "host", s.host, "field", fieldName, "raw_len", len(raw))
		}
		return adjudicationVerdict{}, false
	}
	debugRec.Verdict = &verdict
	s.recordAdjudicationDebug(debugRec)
	s.server.secretVerdictCache.Store(key, verdict)
	s.server.sharedSecretVerdictSet(key, verdict)
	if verdict.Credential {
		s.server.secretValueVerdictCache.Set(valueKey, verdict)
		s.server.sharedSecretValueVerdictSet(s.host, candidate.Value, verdict)
	}
	return verdict, true
}

func secretValueCacheKey(agentID, raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return agentID + ":" + hex.EncodeToString(sum[:])
}

func captureRuntimeSecret(ctx context.Context, srv *Server, st store.Store, v vault.Vault, session *store.RuntimeSession, host, service, raw string) (string, error) {
	if placeholder, ok := lookupRuntimeSecretPlaceholder(ctx, srv, st, session, raw); ok {
		return placeholder, nil
	}
	cacheKey := secretValueCacheKey(session.AgentID, raw)
	if entry, ok := srv.sharedCapturedSecretGet(cacheKey); ok {
		if placeholder, ok := validateCapturedSecretCacheEntry(ctx, srv, st, session, cacheKey, entry); ok {
			return placeholder, nil
		}
	}
	release := func() {}
	if ok, unlock := srv.acquireCapturedSecretLock(cacheKey); ok {
		release = unlock
		defer release()
	} else if srv != nil && srv.redisClient != nil {
		deadline := time.Now().UTC().Add(2 * time.Second)
		for time.Now().UTC().Before(deadline) {
			if entry, ok := srv.sharedCapturedSecretGet(cacheKey); ok {
				if placeholder, ok := validateCapturedSecretCacheEntry(ctx, srv, st, session, cacheKey, entry); ok {
					return placeholder, nil
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
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
	if err := v.Set(ctx, session.UserID, serviceID, []byte(raw)); err != nil {
		return "", err
	}
	grantID := uuid.NewString()
	expiresAt := session.ExpiresAt
	metadata, _ := json.Marshal(map[string]any{"source": "runtime_secret_capture"})
	if err := st.CreateCredentialAuthorization(ctx, &store.CredentialAuthorization{
		ID:            grantID,
		UserID:        session.UserID,
		AgentID:       session.AgentID,
		SessionID:     &session.ID,
		Scope:         "session",
		CredentialRef: serviceID,
		Service:       serviceID,
		Host:          host,
		HeaderName:    "authorization",
		Scheme:        "bearer",
		Status:        "active",
		MetadataJSON:  metadata,
		ExpiresAt:     &expiresAt,
	}); err != nil {
		return "", err
	}
	if err := st.CreateRuntimePlaceholder(ctx, &store.RuntimePlaceholder{
		Placeholder:       placeholder,
		UserID:            session.UserID,
		AgentID:           session.AgentID,
		ServiceID:         serviceID,
		VaultItemID:       serviceID,
		CredentialGrantID: grantID,
		ExpiresAt:         &expiresAt,
	}); err != nil {
		return "", err
	}
	entry := capturedSecretEntry{
		Placeholder: placeholder,
		ServiceID:   serviceID,
	}
	srv.secretValueCache.Store(cacheKey, entry)
	srv.sharedCapturedSecretSet(cacheKey, entry)
	return placeholder, nil
}

func lookupRuntimeSecretPlaceholder(ctx context.Context, srv *Server, st store.Store, session *store.RuntimeSession, raw string) (string, bool) {
	if srv == nil || session == nil {
		return "", false
	}
	cacheKey := secretValueCacheKey(session.AgentID, raw)
	value, ok := srv.secretValueCache.Load(cacheKey)
	if !ok {
		if entry, ok := srv.sharedCapturedSecretGet(cacheKey); ok {
			return validateCapturedSecretCacheEntry(ctx, srv, st, session, cacheKey, entry)
		}
		return "", false
	}
	entry, _ := value.(capturedSecretEntry)
	return validateCapturedSecretCacheEntry(ctx, srv, st, session, cacheKey, entry)
}

func validateCapturedSecretCacheEntry(ctx context.Context, srv *Server, st store.Store, session *store.RuntimeSession, cacheKey string, entry capturedSecretEntry) (string, bool) {
	if entry.Placeholder == "" || st == nil || session == nil {
		return "", false
	}
	rec, err := st.GetRuntimePlaceholder(ctx, entry.Placeholder)
	if err != nil {
		if srv != nil {
			srv.secretValueCache.Delete(cacheKey)
		}
		return "", false
	}
	if entry.ServiceID != "" && rec.ServiceID != entry.ServiceID {
		if srv != nil {
			srv.secretValueCache.Delete(cacheKey)
		}
		return "", false
	}
	if _, ok := llmproxy.ValidateRuntimePlaceholderAccess(ctx, st, rec, session.UserID, session.AgentID, time.Now().UTC()); !ok {
		if srv != nil {
			srv.secretValueCache.Delete(cacheKey)
		}
		return "", false
	}
	if srv != nil {
		srv.secretValueCache.Store(cacheKey, entry)
	}
	return entry.Placeholder, true
}

var knownProtocolNoisePrefixes = []string{
	"toolu_",          // Anthropic tool-use IDs
	"msg_",            // Anthropic message IDs
	"req_",            // Anthropic request IDs
	"chatcmpl_",       // OpenAI chat completion IDs
	"asst_",           // OpenAI assistant IDs
	"thread_",         // OpenAI thread IDs
	"run_",            // OpenAI run IDs
	"step_",           // OpenAI step IDs
	"call_",           // OpenAI tool call IDs
	"clear_thinking_", // Anthropic thinking IDs
}

var (
	uuidCandidateRe        = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	jsIdentifierRe         = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	allCapsConstantRe      = regexp.MustCompile(`^[A-Z][A-Z0-9_]+$`)
	bundlerChunkSuffixRe   = regexp.MustCompile(`-[A-Za-z0-9_-]{8}$`)
	fileExtensionInValueRe = regexp.MustCompile(`\.(?:js|ts|tsx|jsx|json|py|go|rs|md|html?|css|ya?ml|toml|lock|map|svg|png|jpg|jpeg|gif|webp|woff2?|ttf|otf|sql|sh|env|txt)$`)
)

// looksObviouslyNonSecret returns true for candidate strings that the
// detector flags as high-entropy but are clearly not credentials. Skipping
// these saves an LLM round-trip per candidate.
//
// The filter is conservative: if the field name implies a credential (handled
// upstream by highContextSecretField / secretContextHint), we never reach this
// path. Within "content"/"command"/"text" prose, however, we see lots of
// UUIDs in file paths, vite-style chunked filenames, JS identifiers, and
// all-caps env-var names. None of those are secrets.
func looksObviouslyNonSecret(candidate string) bool {
	return runtimeautovault.LooksObviouslyNonSecret(candidate)
}

func isKebabIdentifier(s string) bool {
	return runtimeautovault.IsKebabIdentifier(s)
}

func hasMixedCase(s string) bool {
	return runtimeautovault.HasMixedCase(s)
}

func hasDigit(s string) bool {
	return runtimeautovault.HasDigit(s)
}

func adjudicationCacheKey(host, fieldName, charset, contextWindow string) string {
	return runtimeautovault.AdjudicationCacheKey(host, fieldName, charset, contextWindow)
}

func prefixRegexFor(prefix string) *regexp.Regexp {
	return runtimeautovault.PrefixRegexFor(prefix)
}

func highContextSecretField(fieldName string) bool {
	return runtimeautovault.HighContextSecretField(fieldName)
}

func secretContextHint(content, candidate string) bool {
	return runtimeautovault.SecretContextHint(content, candidate)
}

func guessService(fieldName, content string) string {
	return runtimeautovault.GuessService(fieldName, content)
}

func normalizeSecretService(value string) string {
	return runtimeautovault.NormalizeSecretService(value)
}

func redactedCandidateContext(content, candidate string) string {
	return runtimeautovault.RedactedCandidateContext(content, candidate)
}

func buildSecretAdjudicatorPrompt(host, fieldName, content string, candidate runtimeautovault.Candidate) string {
	return runtimeautovault.BuildSecretAdjudicatorPrompt(host, fieldName, content, candidate)
}

func parseSecretAdjudicatorVerdict(raw string) (adjudicationVerdict, error) {
	return runtimeautovault.ParseSecretAdjudicatorVerdict(raw)
}

func verificationConfig(cfg *config.Config) config.VerificationConfig {
	if cfg == nil {
		return config.VerificationConfig{}
	}
	return cfg.LLM.Verification
}

func runtimeAdjudicationTimeout(cfg config.VerificationConfig) time.Duration {
	return runtimeautovault.SecretAdjudicationTimeout(cfg)
}

func stringValueFromMap(m map[string]any, key string) string {
	raw, ok := m[key]
	if !ok {
		return ""
	}
	value, _ := raw.(string)
	return value
}

func runtimeConversationHost(req *http.Request) bool {
	switch requestHost(req) {
	case "api.anthropic.com", "api.openai.com", "chatgpt.com":
		return true
	default:
		return false
	}
}
