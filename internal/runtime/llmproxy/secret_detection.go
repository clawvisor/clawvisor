package llmproxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
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
	Findings     []InboundSecretFinding `json:"findings"`
	RedactedBody []byte                 `json:"-"`
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

type SecretDecisionAction string

const (
	SecretDecisionNone      SecretDecisionAction = ""
	SecretDecisionAllowOnce SecretDecisionAction = "allow_once"
	SecretDecisionDiscard   SecretDecisionAction = "discard"
	SecretDecisionNotSecret SecretDecisionAction = "not_secret"
	SecretDecisionVault     SecretDecisionAction = "vault"
)

const (
	SecretDecisionPromptMarker = "Clawvisor detected a possible raw secret"
	SecretDecisionIDMarker     = "[clawvisor:secret="
)

type SecretDecisionReply struct {
	Action    SecretDecisionAction
	VaultName string
}

type PendingSecretDecisionCache interface {
	HoldSecret(ctx context.Context, pending PendingSecretDecision) (PendingSecretDecision, error)
	PeekSecret(ctx context.Context, userID, agentID string, provider conversation.Provider) (*PendingSecretDecision, error)
	ResolveSecret(ctx context.Context, userID, agentID string, provider conversation.Provider) (*PendingSecretDecision, error)
}

type MemoryPendingSecretDecisionCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	pending map[pendingSecretKey]PendingSecretDecision
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
		pending: map[pendingSecretKey]PendingSecretDecision{},
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
		c.pending = map[pendingSecretKey]PendingSecretDecision{}
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
	c.pending[pending.key()] = pending
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
	pending, ok := c.pending[pendingSecretKey{userID: userID, agentID: agentID, provider: provider}]
	if !ok {
		return nil, nil
	}
	cp := pending
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
	pending, ok := c.pending[key]
	if !ok {
		return nil, nil
	}
	delete(c.pending, key)
	return &pending, nil
}

func (c *MemoryPendingSecretDecisionCache) pruneLocked(now time.Time) {
	for key, pending := range c.pending {
		if !pending.ExpiresAt.IsZero() && !pending.ExpiresAt.After(now) {
			delete(c.pending, key)
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
	rewritten, changed := scanInboundSecretValue(ctx, payload, "", true, false, opts, findings)
	if len(findings) == 0 {
		return InboundSecretScanResult{}, false, nil
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
	return InboundSecretScanResult{Findings: list, RedactedBody: encoded}, true, nil
}

func scanInboundSecretValue(ctx context.Context, value any, fieldName string, topLevel bool, skipHeuristic bool, opts InboundSecretScanOptions, findings map[string]InboundSecretFinding) (any, bool) {
	switch typed := value.(type) {
	case string:
		return scanInboundSecretString(ctx, typed, fieldName, skipHeuristic, opts, findings)
	case map[string]any:
		if isToolResultSecretScanSubtree(typed) {
			return value, false
		}
		if strings.EqualFold(stringFromMap(typed, "type"), "thinking") {
			return value, false
		}
		changed := false
		for key, item := range typed {
			childSkipHeuristic := skipHeuristic || (topLevel && runtimeautovault.NoiseSubtreeKey(key))
			next, nextChanged := scanInboundSecretValue(ctx, item, key, false, childSkipHeuristic, opts, findings)
			if nextChanged {
				typed[key] = next
				changed = true
			}
		}
		return typed, changed
	case []any:
		changed := false
		for i, item := range typed {
			next, nextChanged := scanInboundSecretValue(ctx, item, fieldName, false, skipHeuristic, opts, findings)
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

func scanInboundSecretString(ctx context.Context, value, fieldName string, skipHeuristic bool, opts InboundSecretScanOptions, findings map[string]InboundSecretFinding) (string, bool) {
	if strings.TrimSpace(value) == "" || runtimeautovault.ProtectedStringField(fieldName) {
		return value, false
	}
	if isClawvisorGeneratedSecretScanBlock(value) {
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
	scannable := stripSecretRedactionMarkers(runtimeautovault.StripHarnessMetadataTags(value))
	for _, password := range runtimeautovault.FindPasswordRevealCandidates(scannable) {
		value = strings.ReplaceAll(value, password, redactFoundSecret(password, runtimeautovault.GuessService(fieldName, value), "password_reveal", 0, opts.Suppressed, findings))
	}
	for _, candidate := range runtimeautovault.DetectCandidates(scannable) {
		if runtimeautovault.LooksLikeShadow(candidate.Value) {
			continue
		}
		if runtimeautovault.LooksObviouslyNonSecret(candidate.Value) {
			continue
		}
		switch {
		case runtimeautovault.HighContextSecretField(fieldName), runtimeautovault.SecretContextHint(value, candidate.Value):
			value = strings.ReplaceAll(value, candidate.Value, redactFoundSecret(candidate.Value, runtimeautovault.GuessService(fieldName, value), "heuristic_swap", candidate.Entropy, opts.Suppressed, findings))
		default:
			verdict, ok := adjudicateInboundSecret(ctx, opts, fieldName, value, candidate)
			if ok {
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
			value = strings.ReplaceAll(value, candidate.Value, redactFoundSecret(candidate.Value, runtimeautovault.GuessService(fieldName, value), "heuristic_observe", candidate.Entropy, opts.Suppressed, findings))
		}
	}
	return value, value != original
}

func isClawvisorGeneratedSecretScanBlock(value string) bool {
	if value == "" {
		return false
	}
	markers := []string{
		InlineApprovalIDMarker,
		InlineApprovalSubstitutedPromptMarker,
		InlineApprovalAugmentationMarker,
		InlineTaskDenyMarker,
		InlineTaskCreatorErrorMarker,
		SecretDecisionIDMarker,
		SecretDecisionPromptMarker,
		ClawvisorManagedMarker,
		ControlNoticeSentinel,
	}
	for _, marker := range markers {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

var secretRedactionMarkerRE = regexp.MustCompile(`\[redacted secret:[^\]]+\]`)

func stripSecretRedactionMarkers(value string) string {
	if value == "" || !strings.Contains(value, "[redacted secret:") {
		return value
	}
	return secretRedactionMarkerRE.ReplaceAllString(value, "")
}

func adjudicateInboundSecret(ctx context.Context, opts InboundSecretScanOptions, fieldName, content string, candidate runtimeautovault.Candidate) (runtimeautovault.SecretAdjudicationVerdict, bool) {
	if opts.Adjudicator == nil {
		return runtimeautovault.SecretAdjudicationVerdict{}, false
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
		return runtimeautovault.SecretAdjudicationVerdict{}, false
	}
	return result.Verdict, true
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
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func SecretDecisionReplyFromBody(reqProvider conversation.Provider, body []byte) SecretDecisionReply {
	text := LatestUserText(reqProvider, body)
	return ParseSecretDecisionReply(text)
}

func ParseSecretDecisionReply(text string) SecretDecisionReply {
	normalized := strings.ToLower(strings.TrimSpace(text))
	normalized = strings.Trim(normalized, "`\"' ")
	switch {
	case normalized == "allow once" || normalized == "allow":
		return SecretDecisionReply{Action: SecretDecisionAllowOnce}
	case normalized == "discard" || normalized == "redact" || normalized == "discard secret" || normalized == "redact secret":
		return SecretDecisionReply{Action: SecretDecisionDiscard}
	case normalized == "not secret" || normalized == "not a secret" || normalized == "this is not a secret":
		return SecretDecisionReply{Action: SecretDecisionNotSecret}
	case strings.HasPrefix(normalized, "vault "):
		name := strings.TrimSpace(text[len("vault "):])
		name = strings.TrimPrefix(strings.TrimSpace(name), "as ")
		return SecretDecisionReply{Action: SecretDecisionVault, VaultName: sanitizeVaultName(name)}
	default:
		return SecretDecisionReply{}
	}
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
			for i := len(parsed.Messages) - 1; i >= 0; i-- {
				role, _ := parsed.Messages[i]["role"].(string)
				if role != "user" {
					continue
				}
				raw, _ := json.Marshal(parsed.Messages[i]["content"])
				return strings.TrimSpace(flattenOpenAITaskReplyContent(raw))
			}
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
		}
	}
	return ""
}

func llmSecretNoiseSubtree(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "system", "tools", "tool_choice", "response_format", "model", "metadata":
		return true
	default:
		return false
	}
}

func protectedSecretField(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "signature", "thinking":
		return true
	default:
		return false
	}
}

func highSecretContext(fieldName, value string) bool {
	field := strings.ToLower(strings.TrimSpace(fieldName))
	text := strings.ToLower(value)
	for _, needle := range []string{"password", "secret", "api key", "api_key", "token", "bearer", "credential"} {
		if strings.Contains(field, needle) || strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func guessSecretService(fieldName, value string) string {
	text := strings.ToLower(fieldName + " " + value)
	for _, service := range []string{"github", "anthropic", "openai", "slack", "stripe", "resend", "google", "microsoft", "notion", "linear"} {
		if strings.Contains(text, service) {
			return service
		}
	}
	return ""
}

func normalizeSecretLabel(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, " ", "_")
	value = regexp.MustCompile(`[^a-z0-9._:-]+`).ReplaceAllString(value, "_")
	return strings.Trim(value, "._:-")
}

func sanitizeVaultName(value string) string {
	value = normalizeSecretLabel(value)
	if value == "" {
		return "secret"
	}
	if len(value) > 96 {
		value = value[:96]
	}
	return value
}

func stringFromMap(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}
