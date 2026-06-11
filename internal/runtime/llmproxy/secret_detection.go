package llmproxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	runtimeautovault "github.com/clawvisor/clawvisor/internal/runtime/autovault"
	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
)

type InboundSecretFinding struct {
	Value               string  `json:"-"`
	Fingerprint         string  `json:"fingerprint"`
	Service             string  `json:"service,omitempty"`
	SuggestedName       string  `json:"suggested_name"`
	Source              string  `json:"source"`
	Entropy             float64 `json:"entropy,omitempty"`
	ExistingVaultItemID string  `json:"existing_vault_item_id,omitempty"`
}

type InboundSecretScanResult struct {
	Findings      []InboundSecretFinding      `json:"findings"`
	Adjudications []InboundSecretAdjudication `json:"adjudications,omitempty"`
	RedactedBody  []byte                      `json:"-"`
}

type InboundSecretAdjudication struct {
	Fingerprint  string  `json:"fingerprint"`
	FieldName    string  `json:"field_name,omitempty"`
	Charset      string  `json:"charset,omitempty"`
	Entropy      float64 `json:"entropy,omitempty"`
	Outcome      string  `json:"outcome"`
	Credential   bool    `json:"credential,omitempty"`
	Service      string  `json:"service,omitempty"`
	Confidence   float64 `json:"confidence,omitempty"`
	ErrorKind    string  `json:"error_kind,omitempty"`
	ErrorMessage string  `json:"error_message,omitempty"`
	DurationMS   int64   `json:"duration_ms,omitempty"`
}

type InboundSecretScanOptions struct {
	Provider    conversation.Provider
	Host        string
	Body        []byte
	Suppressed  map[string]struct{}
	Adjudicator runtimeautovault.SecretAdjudicator
}

type PendingSecretDecision struct {
	ID           string
	UserID       string
	AgentID      string
	Provider     conversation.Provider
	OriginalBody []byte
	RedactedBody []byte
	Findings     []InboundSecretFinding
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

const (
	SecretDecisionPromptMarker = "Clawvisor detected a possible raw secret"
)

type PendingSecretDecisionCache interface {
	HoldSecret(ctx context.Context, pending PendingSecretDecision) (PendingSecretDecision, error)
	PeekSecret(ctx context.Context, userID, agentID string, provider conversation.Provider) (*PendingSecretDecision, error)
	ResolveSecret(ctx context.Context, userID, agentID string, provider conversation.Provider) (*PendingSecretDecision, error)
	ResolveSecretID(ctx context.Context, userID, agentID string, provider conversation.Provider, id string) (*PendingSecretDecision, error)
}

type MemoryPendingSecretDecisionCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	pending map[pendingSecretKey][]PendingSecretDecision
	now     func() time.Time
}

type pendingSecretKey struct {
	userID   string
	agentID  string
	provider conversation.Provider
}

var secretDecisionRandRead = rand.Read

func NewMemoryPendingSecretDecisionCache(ttl time.Duration) *MemoryPendingSecretDecisionCache {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &MemoryPendingSecretDecisionCache{
		ttl:     ttl,
		pending: map[pendingSecretKey][]PendingSecretDecision{},
		now:     time.Now,
	}
}

func (c *MemoryPendingSecretDecisionCache) HoldSecret(_ context.Context, pending PendingSecretDecision) (PendingSecretDecision, error) {
	if c == nil {
		return pending, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending == nil {
		c.pending = map[pendingSecretKey][]PendingSecretDecision{}
	}
	now := c.now().UTC()
	if pending.ID == "" {
		id, err := newSecretDecisionID()
		if err != nil {
			return PendingSecretDecision{}, err
		}
		pending.ID = id
	}
	if pending.CreatedAt.IsZero() {
		pending.CreatedAt = now
	}
	if pending.ExpiresAt.IsZero() {
		pending.ExpiresAt = now.Add(c.ttl)
	}
	c.pruneLocked(now)
	key := pending.key()
	c.pending[key] = append(c.pending[key], pending)
	return pending, nil
}

func (c *MemoryPendingSecretDecisionCache) PeekSecret(_ context.Context, userID, agentID string, provider conversation.Provider) (*PendingSecretDecision, error) {
	if c == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now().UTC()
	c.pruneLocked(now)
	entries := c.pending[pendingSecretKey{userID: userID, agentID: agentID, provider: provider}]
	if len(entries) == 0 {
		return nil, nil
	}
	cp := entries[len(entries)-1]
	return &cp, nil
}

func (c *MemoryPendingSecretDecisionCache) ResolveSecret(_ context.Context, userID, agentID string, provider conversation.Provider) (*PendingSecretDecision, error) {
	if c == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now().UTC()
	c.pruneLocked(now)
	key := pendingSecretKey{userID: userID, agentID: agentID, provider: provider}
	entries := c.pending[key]
	if len(entries) == 0 {
		return nil, nil
	}
	pending := entries[len(entries)-1]
	entries = entries[:len(entries)-1]
	if len(entries) == 0 {
		delete(c.pending, key)
	} else {
		c.pending[key] = entries
	}
	return &pending, nil
}

func (c *MemoryPendingSecretDecisionCache) ResolveSecretID(_ context.Context, userID, agentID string, provider conversation.Provider, id string) (*PendingSecretDecision, error) {
	if c == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now().UTC()
	c.pruneLocked(now)
	key := pendingSecretKey{userID: userID, agentID: agentID, provider: provider}
	entries := c.pending[key]
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].ID != id {
			continue
		}
		pending := entries[i]
		entries = append(entries[:i], entries[i+1:]...)
		if len(entries) == 0 {
			delete(c.pending, key)
		} else {
			c.pending[key] = entries
		}
		return &pending, nil
	}
	return nil, nil
}

func (c *MemoryPendingSecretDecisionCache) pruneLocked(now time.Time) {
	for key, entries := range c.pending {
		kept := entries[:0]
		for _, pending := range entries {
			if !pending.ExpiresAt.IsZero() && !pending.ExpiresAt.After(now) {
				continue
			}
			kept = append(kept, pending)
		}
		if len(kept) == 0 {
			delete(c.pending, key)
		} else {
			c.pending[key] = kept
		}
	}
}

func (p PendingSecretDecision) key() pendingSecretKey {
	return pendingSecretKey{userID: p.UserID, agentID: p.AgentID, provider: p.Provider}
}

func newSecretDecisionID() (string, error) {
	var b [16]byte
	if _, err := secretDecisionRandRead(b[:]); err != nil {
		return "", err
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])
	return "cv-secret-" + strings.ToLower(enc), nil
}

func ScanInboundSecrets(provider conversation.Provider, body []byte, suppressed map[string]struct{}) (InboundSecretScanResult, bool, error) {
	return ScanInboundSecretsWithOptions(context.Background(), InboundSecretScanOptions{
		Provider:   provider,
		Body:       body,
		Suppressed: suppressed,
	})
}

func ScanInboundSecretsWithOptions(ctx context.Context, opts InboundSecretScanOptions) (InboundSecretScanResult, bool, error) {
	body := opts.Body
	if len(body) == 0 || !json.Valid(body) {
		return InboundSecretScanResult{}, false, nil
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return InboundSecretScanResult{}, false, err
	}
	findings := map[string]InboundSecretFinding{}
	var adjudications []InboundSecretAdjudication
	rewritten, changed := scanInboundSecretValue(ctx, payload, "", true, false, false, opts, findings, &adjudications)
	if len(findings) == 0 {
		return InboundSecretScanResult{Adjudications: adjudications}, false, nil
	}
	encoded := body
	if changed {
		out, err := json.Marshal(rewritten)
		if err != nil {
			return InboundSecretScanResult{}, false, err
		}
		encoded = out
	}
	list := make([]InboundSecretFinding, 0, len(findings))
	for _, finding := range findings {
		list = append(list, finding)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Fingerprint < list[j].Fingerprint })
	return InboundSecretScanResult{Findings: list, Adjudications: adjudications, RedactedBody: encoded}, true, nil
}

func scanInboundSecretValue(ctx context.Context, value any, fieldName string, topLevel bool, skipHeuristic bool, inToolResult bool, opts InboundSecretScanOptions, findings map[string]InboundSecretFinding, adjudications *[]InboundSecretAdjudication) (any, bool) {
	switch typed := value.(type) {
	case string:
		return scanInboundSecretString(ctx, typed, fieldName, skipHeuristic, inToolResult, opts, findings, adjudications)
	case map[string]any:
		if strings.EqualFold(stringFromMap(typed, "type"), "thinking") {
			return value, false
		}
		childInToolResult := inToolResult || isToolResultSecretScanSubtree(typed)
		changed := false
		for key, item := range typed {
			childSkipHeuristic := skipHeuristic || (topLevel && runtimeautovault.NoiseSubtreeKey(key))
			next, nextChanged := scanInboundSecretValue(ctx, item, key, false, childSkipHeuristic, childInToolResult, opts, findings, adjudications)
			if nextChanged {
				typed[key] = next
				changed = true
			}
		}
		return typed, changed
	case []any:
		changed := false
		for i, item := range typed {
			next, nextChanged := scanInboundSecretValue(ctx, item, fieldName, false, skipHeuristic, inToolResult, opts, findings, adjudications)
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

func isToolResultSecretScanSubtree(value map[string]any) bool {
	switch strings.ToLower(strings.TrimSpace(stringFromMap(value, "type"))) {
	case "function_call_output", "tool_result":
		return true
	}
	return strings.EqualFold(stringFromMap(value, "role"), "tool")
}

func scanInboundSecretString(ctx context.Context, value, fieldName string, skipHeuristic bool, inToolResult bool, opts InboundSecretScanOptions, findings map[string]InboundSecretFinding, adjudications *[]InboundSecretAdjudication) (string, bool) {
	if strings.TrimSpace(value) == "" || runtimeautovault.ProtectedStringField(fieldName) {
		return value, false
	}
	original := value
	if skipHeuristic {
		return value, false
	}
	suppressedKnownPrefix := false
	for _, spec := range runtimeautovault.KnownPrefixSpecs() {
		if !strings.Contains(value, spec.Prefix) {
			continue
		}
		re := runtimeautovault.PrefixRegexFor(spec.Prefix)
		value = re.ReplaceAllStringFunc(value, func(match string) string {
			leading, secret := runtimeautovault.SplitPrefixRegexMatch(spec.Prefix, match)
			if runtimeautovault.LooksLikeIdentifier(secret) {
				return match
			}
			if _, ok := opts.Suppressed[SecretFingerprint(secret)]; ok {
				suppressedKnownPrefix = true
				return match
			}
			return leading + redactFoundSecret(secret, spec.Service, "known_prefix", 0, opts.Suppressed, findings)
		})
	}
	if suppressedKnownPrefix && value == original {
		return value, false
	}
	if runtimeautovault.LooksLikeProtocolNoise(fieldName, value) || runtimeautovault.LooksLikeContextNoise(value) {
		return value, value != original
	}
	scannable := stripClawvisorGeneratedMarkers(stripSecretRedactionMarkers(runtimeautovault.StripHarnessMetadataTags(value)))
	for _, password := range runtimeautovault.FindPasswordRevealCandidates(scannable) {
		value = strings.ReplaceAll(value, password, redactFoundSecret(password, runtimeautovault.GuessService(fieldName, value), "password_reveal", 0, opts.Suppressed, findings))
	}
	for _, assignment := range highContextSecretAssignments(scannable) {
		value = strings.ReplaceAll(value, assignment.Value, redactFoundSecret(assignment.Value, runtimeautovault.GuessService(assignment.Name, value), "heuristic_swap", assignment.Entropy, opts.Suppressed, findings))
	}
	for _, candidate := range runtimeautovault.DetectCandidates(scannable) {
		if runtimeautovault.LooksLikeShadow(candidate.Value) {
			continue
		}
		if runtimeautovault.LooksObviouslyNonSecret(candidate.Value) {
			continue
		}
		switch {
		case runtimeautovault.HighContextSecretField(fieldName), !inToolResult && runtimeautovault.SecretContextHint(value, candidate.Value):
			value = strings.ReplaceAll(value, candidate.Value, redactFoundSecret(candidate.Value, runtimeautovault.GuessService(fieldName, value), "heuristic_swap", candidate.Entropy, opts.Suppressed, findings))
		default:
			result, ok, adjudicatorErr := adjudicateInboundSecret(ctx, opts, fieldName, value, candidate)
			recordInboundSecretAdjudication(adjudications, fieldName, candidate, result, ok, adjudicatorErr)
			if ok {
				verdict := result.Verdict
				if !verdict.Credential || verdict.Confidence < 0.6 {
					continue
				}
				service := runtimeautovault.NormalizeSecretService(verdict.Service)
				if service == "" {
					service = runtimeautovault.GuessService(fieldName, value)
				}
				value = strings.ReplaceAll(value, candidate.Value, redactFoundSecret(candidate.Value, service, "heuristic_adjudicated", candidate.Entropy, opts.Suppressed, findings))
				continue
			}
			if adjudicatorErr == nil {
				continue
			}
			value = strings.ReplaceAll(value, candidate.Value, redactFoundSecret(candidate.Value, runtimeautovault.GuessService(fieldName, value), "heuristic_observe", candidate.Entropy, opts.Suppressed, findings))
		}
	}
	return value, value != original
}

var clawvisorFooterMarkerRE = regexp.MustCompile(`\[clawvisor:(secret|approval)=[^\]]+\]`)
var highContextAssignmentRE = regexp.MustCompile(`(?i)\b([A-Z0-9][A-Z0-9_-]*(?:API[_-]?KEY|ACCESS[_-]?TOKEN|AUTH[_-]?TOKEN|REFRESH[_-]?TOKEN|SECRET|PASSWORD|PASSCODE)[A-Z0-9_-]*)\b\s*[:=]\s*["']?([A-Za-z0-9_./+=:-]{8,})["']?`)

type highContextAssignment struct {
	Name    string
	Value   string
	Entropy float64
}

func highContextSecretAssignments(value string) []highContextAssignment {
	matches := highContextAssignmentRE.FindAllStringSubmatch(value, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]highContextAssignment, 0, len(matches))
	for _, match := range matches {
		if len(match) < 3 {
			continue
		}
		name := strings.TrimSpace(match[1])
		secret := strings.TrimSpace(match[2])
		if _, ok := seen[secret]; ok || !secretAssignmentValueLooksSensitive(secret) {
			continue
		}
		seen[secret] = struct{}{}
		out = append(out, highContextAssignment{
			Name:    name,
			Value:   secret,
			Entropy: secretAssignmentEntropy(secret),
		})
	}
	return out
}

func secretAssignmentValueLooksSensitive(value string) bool {
	if len(value) < 12 || runtimeautovault.LooksLikeShadow(value) || runtimeautovault.LooksObviouslyNonSecret(value) {
		return false
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{"example", "dummy", "fake", "placeholder", "replace", "changeme", "change_me", "your_", "test_key", "tooloutput"} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	if secretAssignmentEntropy(value) < 3.2 {
		return false
	}
	return true
}

func secretAssignmentEntropy(value string) float64 {
	if value == "" {
		return 0
	}
	counts := map[rune]int{}
	for _, r := range value {
		counts[r]++
	}
	var entropy float64
	n := float64(len([]rune(value)))
	for _, count := range counts {
		p := float64(count) / n
		entropy -= p * math.Log2(p)
	}
	return entropy
}

func stripClawvisorGeneratedMarkers(value string) string {
	value = clawvisorFooterMarkerRE.ReplaceAllString(value, " ")
	// noticeOpenTagRE matches the full opening element of any
	// <clawvisor-notice ...> regardless of kind, so a future kind
	// added via Render is scrubbed without revisiting this list. The
	// matching closing-tag literal is stripped separately.
	value = noticeOpenTagRE.ReplaceAllString(value, " ")
	value = strings.ReplaceAll(value, noticeTagClose, " ")
	markers := []string{
		InlineApprovalIDMarker,
		InlineApprovalSubstitutedPromptMarker,
		InlineExpansionApprovalSubstitutedPromptMarker,
		SecretDecisionIDMarker,
		SecretDecisionPromptMarker,
		ClawvisorManagedMarker,
		ControlNoticeSentinel,
	}
	for _, marker := range markers {
		if marker != "" {
			value = strings.ReplaceAll(value, marker, " ")
		}
	}
	return value
}

// noticeOpenTagRE matches any <clawvisor-notice ...> opening element
// so the secret scrubber can strip the envelope cleanly regardless of
// the kind attribute. The body is captured non-greedily but stays
// inside the attribute list — the closing `>` ends the match before
// the body, which is left in place for downstream English/entropy
// analysis (the envelope itself carries no secret-bearing tokens).
var noticeOpenTagRE = regexp.MustCompile(`<clawvisor-notice[^>]*>`)

var secretRedactionMarkerRE = regexp.MustCompile(`\[redacted secret:[^\]]+\]`)

func stripSecretRedactionMarkers(value string) string {
	if value == "" || !strings.Contains(value, "[redacted secret:") {
		return value
	}
	return secretRedactionMarkerRE.ReplaceAllString(value, "")
}

func adjudicateInboundSecret(ctx context.Context, opts InboundSecretScanOptions, fieldName, content string, candidate runtimeautovault.Candidate) (runtimeautovault.SecretAdjudicationResult, bool, error) {
	if opts.Adjudicator == nil {
		return runtimeautovault.SecretAdjudicationResult{}, false, nil
	}
	host := opts.Host
	if host == "" {
		host = string(opts.Provider)
	}
	result, err := opts.Adjudicator.AdjudicateSecret(ctx, runtimeautovault.SecretAdjudicationRequest{
		Host:      host,
		FieldName: fieldName,
		Content:   content,
		Candidate: candidate,
	})
	if err != nil {
		return result, false, err
	}
	return result, true, nil
}

func recordInboundSecretAdjudication(adjudications *[]InboundSecretAdjudication, fieldName string, candidate runtimeautovault.Candidate, result runtimeautovault.SecretAdjudicationResult, ok bool, err error) {
	if adjudications == nil {
		return
	}
	trace := InboundSecretAdjudication{
		Fingerprint: SecretFingerprint(candidate.Value),
		FieldName:   fieldName,
		Charset:     candidate.Charset,
		Entropy:     candidate.Entropy,
	}
	if result.Duration > 0 {
		trace.DurationMS = result.Duration.Milliseconds()
	}
	switch {
	case ok:
		trace.Outcome = "verdict"
		trace.Credential = result.Verdict.Credential
		trace.Service = runtimeautovault.NormalizeSecretService(result.Verdict.Service)
		trace.Confidence = result.Verdict.Confidence
	case err != nil:
		trace.Outcome = "error"
		trace.ErrorKind = inboundSecretAdjudicationErrorKind(err)
		trace.ErrorMessage = err.Error()
	default:
		trace.Outcome = "not_configured"
	}
	*adjudications = append(*adjudications, trace)
}

func inboundSecretAdjudicationErrorKind(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, runtimeautovault.ErrSecretAdjudicatorDisabled):
		return "disabled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded"):
		return "timeout"
	case strings.Contains(msg, "gemini auth") || strings.Contains(msg, "oauth2") || strings.Contains(msg, "credentials"):
		return "auth"
	case strings.Contains(msg, "status "):
		return "upstream_status"
	case strings.Contains(msg, "no json object") || strings.Contains(msg, "invalid character") || strings.Contains(msg, "unmarshal"):
		return "parse"
	case strings.Contains(msg, "decode gemini response") || strings.Contains(msg, "no candidates") || strings.Contains(msg, "no text part"):
		return "response_decode"
	default:
		return "error"
	}
}

func redactFoundSecret(raw, service, source string, entropy float64, suppressed map[string]struct{}, findings map[string]InboundSecretFinding) string {
	if runtimeautovault.LooksLikeShadow(raw) {
		return raw
	}
	fp := SecretFingerprint(raw)
	if _, ok := suppressed[fp]; ok {
		return raw
	}
	if existing, ok := findings[fp]; ok {
		name := existing.SuggestedName
		if name == "" {
			name = "secret"
		}
		return "[redacted secret:" + name + "]"
	}
	service = normalizeSecretLabel(service)
	name := service
	if name == "" {
		name = "secret"
	}
	findings[fp] = InboundSecretFinding{
		Value:         raw,
		Fingerprint:   fp,
		Service:       service,
		SuggestedName: name,
		Source:        source,
		Entropy:       entropy,
	}
	return "[redacted secret:" + name + "]"
}

func SecretFingerprint(raw string) string {
	// codeql[go/weak-sensitive-data-hashing] Internal secret deduplication fingerprint; not used for password storage or authentication.
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func SecretDecisionReplyFromBody(reqProvider conversation.Provider, body []byte) SecretDecisionReply {
	text := LatestUserText(reqProvider, body)
	return ParseSecretDecisionReply(text)
}

// secretDecisionIDRE captures the pending-decision ID emitted into the
// assistant prompt that asks the user to allow/discard/vault a detected
// secret. The marker is `[clawvisor:secret=<id>]` and the ID is
// guaranteed to be on a single line of opaque text — we constrain the
// match to a non-`]` character class so an outer literal in surrounding
// text can't tail-match.
var secretDecisionIDRE = regexp.MustCompile(`\[clawvisor:secret=([^\]\s]+)\]`)

// LatestAssistantSecretDecisionID returns the pending-decision ID embedded
// in the most recent assistant message that carries the
// SecretDecisionIDMarker. Returns empty string when no such message is
// present. Callers thread the returned ID through ResolveSecretID so the
// user's reply releases the specific pending decision they were shown,
// not whatever happened to be at the tail of the queue when a concurrent
// request enqueued a second pending.
func LatestAssistantSecretDecisionID(provider conversation.Provider, body []byte) string {
	collect := func(text string) string {
		if !strings.Contains(text, SecretDecisionIDMarker) {
			return ""
		}
		m := secretDecisionIDRE.FindStringSubmatch(text)
		if len(m) < 2 {
			return ""
		}
		return m[1]
	}
	switch provider {
	case conversation.ProviderAnthropic:
		var parsed struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &parsed); err == nil {
			for i := len(parsed.Messages) - 1; i >= 0; i-- {
				if parsed.Messages[i].Role != "assistant" {
					continue
				}
				if id := collect(flattenAnthropicTaskReplyText(parsed.Messages[i].Content)); id != "" {
					return id
				}
			}
		}
	case conversation.ProviderOpenAI:
		var parsed struct {
			Messages []map[string]any `json:"messages"`
			Input    json.RawMessage  `json:"input"`
		}
		if err := json.Unmarshal(body, &parsed); err == nil {
			var items []map[string]any
			if len(parsed.Input) > 0 && json.Unmarshal(parsed.Input, &items) == nil {
				for i := len(items) - 1; i >= 0; i-- {
					role, _ := items[i]["role"].(string)
					if role != "assistant" {
						continue
					}
					raw, _ := json.Marshal(items[i]["content"])
					if id := collect(flattenOpenAITaskReplyContent(raw)); id != "" {
						return id
					}
				}
			}
			for i := len(parsed.Messages) - 1; i >= 0; i-- {
				role, _ := parsed.Messages[i]["role"].(string)
				if role != "assistant" {
					continue
				}
				raw, _ := json.Marshal(parsed.Messages[i]["content"])
				if id := collect(flattenOpenAITaskReplyContent(raw)); id != "" {
					return id
				}
			}
		}
	}
	return ""
}

func LatestUserText(provider conversation.Provider, body []byte) string {
	switch provider {
	case conversation.ProviderAnthropic:
		var parsed struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &parsed); err == nil {
			for i := len(parsed.Messages) - 1; i >= 0; i-- {
				if parsed.Messages[i].Role == "user" {
					return strings.TrimSpace(flattenAnthropicTaskReplyText(parsed.Messages[i].Content))
				}
			}
		}
	case conversation.ProviderOpenAI:
		var parsed struct {
			Messages []map[string]any `json:"messages"`
			Input    json.RawMessage  `json:"input"`
		}
		if err := json.Unmarshal(body, &parsed); err == nil {
			var input string
			if len(parsed.Input) > 0 && json.Unmarshal(parsed.Input, &input) == nil {
				return strings.TrimSpace(input)
			}
			var items []map[string]any
			if len(parsed.Input) > 0 && json.Unmarshal(parsed.Input, &items) == nil {
				for i := len(items) - 1; i >= 0; i-- {
					role, _ := items[i]["role"].(string)
					if role != "user" {
						continue
					}
					raw, _ := json.Marshal(items[i]["content"])
					return strings.TrimSpace(flattenOpenAITaskReplyContent(raw))
				}
			}
			for i := len(parsed.Messages) - 1; i >= 0; i-- {
				role, _ := parsed.Messages[i]["role"].(string)
				if role != "user" {
					continue
				}
				raw, _ := json.Marshal(parsed.Messages[i]["content"])
				return strings.TrimSpace(flattenOpenAITaskReplyContent(raw))
			}
		}
	}
	return ""
}

func stringFromMap(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}
