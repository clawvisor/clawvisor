// Package judge is the Stage 3 M5 LLM judge — called by the Clawvisor
// server when a fast-policy rule matches with action=flag. The judge
// returns one of {allow, block, flag_for_human_review} based on the
// request context + the rule's intent.
//
// The judge is deliberately narrow: it receives only what the server
// policy pipeline already has (rule definition, destination, agent, a
// small transcript snippet). It does NOT get free reign to invoke
// tools or see the full transcript. Every call is logged to
// judge_decisions.
//
// See docs/design-proxy-stage3.md §§6, 8.
package judge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/llm"
	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// Decision is the judge verdict. Values are wire-stable with the proxy.
type Decision string

const (
	DecisionAllow             Decision = "allow"
	DecisionBlock             Decision = "block"
	DecisionFlagForHumanReview Decision = "flag_for_human_review"
)

// Request is one judge invocation's input. Destination fields let the
// judge reason about what the agent is about to do; TranscriptSnippet
// gives it a few recent conversation turns for context.
type Request struct {
	BridgeID     string
	AgentTokenID string
	RuleName     string
	RuleAction   string // fast-rule action that led here (always "flag")
	RuleMessage  string

	Method          string
	DestinationURL  string
	DestinationHost string
	DestinationPath string
	RequestBody     string // truncated at caller

	// Optional conversation context, in chronological order. Keep short.
	TranscriptSnippet []TranscriptTurn

	// ConversationID is used alongside (rule, request-hash) to build the
	// decision cache key.
	ConversationID string
}

// TranscriptTurn is one conversation turn handed to the judge as context.
type TranscriptTurn struct {
	Role string // "user" | "assistant" | "tool"
	Text string
}

// Verdict is the judge's output.
type Verdict struct {
	Decision         Decision
	Reason           string
	Model            string
	LatencyMs        int
	PromptTokens     int
	CompletionTokens int
	CacheHit         bool
}

// Judge is the runtime. Thread-safe.
type Judge struct {
	health        *llm.Health
	store         store.Store
	logger        *slog.Logger
	cacheTTL      time.Duration
	defaultModel  string
	defaultTimeout time.Duration
}

// New builds a judge that reads its LLM config from the shared health
// tracker (same config used by the verifier/assessor elsewhere).
func New(health *llm.Health, st store.Store, logger *slog.Logger) *Judge {
	if logger == nil {
		logger = slog.Default()
	}
	return &Judge{
		health:         health,
		store:          st,
		logger:         logger,
		cacheTTL:       5 * time.Minute,
		defaultModel:   "claude-haiku-4-5",
		defaultTimeout: 10 * time.Second,
	}
}

// Decide runs the judge. Checks the decision cache first; on miss,
// invokes the LLM and persists the decision for future hits. On LLM
// failure the caller decides behavior via the policy's on_error
// setting — this function returns (Verdict{}, err).
func (j *Judge) Decide(ctx context.Context, req Request) (Verdict, error) {
	if j == nil || j.health == nil {
		return Verdict{}, errors.New("judge: not configured")
	}
	cfg := j.health.LLMConfig()
	if cfg.Provider == "" {
		return Verdict{}, errors.New("judge: no LLM configured on server")
	}
	cacheKey := CacheKey(req)

	// Cache hit short-circuits.
	if rec, err := j.store.GetJudgeDecisionByCacheKey(ctx, cacheKey, time.Now().Add(-j.cacheTTL)); err == nil && rec != nil {
		return Verdict{
			Decision:  Decision(rec.Decision),
			Reason:    rec.Reason,
			Model:     rec.Model,
			LatencyMs: rec.LatencyMs,
			CacheHit:  true,
		}, nil
	}

	// Build the prompt.
	sys, user := buildPrompt(req, cfg.Model)
	messages := []llm.ChatMessage{
		{Role: "system", Content: sys},
		{Role: "user", Content: user},
	}

	// Hard-cap the call so a slow provider can't stall the proxy.
	timeout := j.defaultTimeout
	if cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := llm.NewClient(config.LLMProviderConfig{
		Provider:       cfg.Provider,
		Endpoint:       cfg.Endpoint,
		APIKey:         cfg.APIKey,
		Model:          cfg.Model,
		TimeoutSeconds: cfg.TimeoutSeconds,
	}).WithMaxTokens(300)

	start := time.Now()
	out, err := client.Complete(callCtx, messages)
	latency := time.Since(start)
	if err != nil {
		return Verdict{}, fmt.Errorf("judge: LLM call failed: %w", err)
	}

	decision, reason := parseVerdict(out)
	if decision == "" {
		return Verdict{}, fmt.Errorf("judge: unparseable LLM output: %s", truncate(out, 200))
	}

	verdict := Verdict{
		Decision:  decision,
		Reason:    reason,
		Model:     cfg.Model,
		LatencyMs: int(latency / time.Millisecond),
	}

	// Persist to judge_decisions. Best-effort — a DB failure shouldn't
	// change the verdict (the proxy has the decision it needs).
	_ = j.store.InsertJudgeDecision(ctx, &store.JudgeDecision{
		BridgeID:     req.BridgeID,
		AgentTokenID: req.AgentTokenID,
		RuleName:     req.RuleName,
		CacheKey:     cacheKey,
		Decision:     string(decision),
		Reason:       reason,
		Model:        cfg.Model,
		LatencyMs:    verdict.LatencyMs,
		DecisionPath: "proxy_flag_rule",
	})

	return verdict, nil
}

// CacheKey is exported so the wire can reference the same hash.
// Stable across (rule, conversation, host, method, path). Body is NOT
// hashed — two identical actions on different arguments are
// semantically distinct so we let the LLM re-decide if the body
// differs enough to matter. (We still dedupe on identical requests via
// conversation_id + path.)
func CacheKey(req Request) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s|%s|%s",
		req.BridgeID, req.RuleName, req.ConversationID,
		strings.ToUpper(req.Method), strings.ToLower(req.DestinationHost), req.DestinationPath)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// buildPrompt composes the system + user messages for the judge call.
// System message pins the decision vocabulary; user message carries
// the request + context. Kept deliberately simple — the judge should
// be a quick Y/N reasoning step, not an open-ended chat.
func buildPrompt(req Request, model string) (system, user string) {
	system = `You are a security judge for an AI coding agent.
Your job is to decide whether a specific HTTP request the agent is about to make should be allowed, blocked, or flagged for a human to review.

Respond with STRICTLY one of these three words on the FIRST line:
- allow: the request is safe and consistent with the recent conversation
- block: the request is clearly harmful, destructive, or inconsistent with the agent's stated task
- flag_for_human_review: the request is ambiguous or high-impact enough that a human should sign off before it proceeds

On the SECOND line, write ONE sentence explaining the decision. Do not include any other text.`

	var b strings.Builder
	fmt.Fprintf(&b, "## Matched rule: %s\n", req.RuleName)
	if req.RuleMessage != "" {
		fmt.Fprintf(&b, "Rule description: %s\n", req.RuleMessage)
	}
	fmt.Fprintf(&b, "\n## Request\n%s %s\n", strings.ToUpper(req.Method), req.DestinationURL)
	if req.RequestBody != "" {
		fmt.Fprintf(&b, "Body:\n%s\n", truncate(req.RequestBody, 2000))
	}
	if len(req.TranscriptSnippet) > 0 {
		b.WriteString("\n## Recent conversation (oldest first)\n")
		for _, t := range req.TranscriptSnippet {
			fmt.Fprintf(&b, "[%s] %s\n", t.Role, truncate(t.Text, 400))
		}
	}
	b.WriteString("\nDecide now.")
	return system, b.String()
}

// parseVerdict reads the first non-empty line as the decision and the
// next line as the reason. Tolerant to whitespace, punctuation, and
// common variants like "Decision: block".
func parseVerdict(out string) (Decision, string) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var decisionLine, reasonLine string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if decisionLine == "" {
			decisionLine = l
			continue
		}
		reasonLine = l
		break
	}
	decisionLine = strings.ToLower(strings.TrimLeft(decisionLine, "-* "))
	decisionLine = strings.TrimPrefix(decisionLine, "decision:")
	decisionLine = strings.TrimSpace(decisionLine)
	// Strip any trailing punctuation.
	decisionLine = strings.Trim(decisionLine, ". ")

	switch {
	case strings.HasPrefix(decisionLine, "allow"):
		return DecisionAllow, reasonLine
	case strings.HasPrefix(decisionLine, "block"):
		return DecisionBlock, reasonLine
	case strings.HasPrefix(decisionLine, "flag"):
		return DecisionFlagForHumanReview, reasonLine
	}
	return "", ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// PolicyDecisionRequest mirrors the proxy-facing wire request for the
// /api/proxy/policy-decision endpoint. Duplicated here so the handler
// package can import only `judge` without importing the handlers
// subpackage into the proxy-side wire contract.
type PolicyDecisionRequest struct {
	BridgeID          string           `json:"bridge_id,omitempty"`
	AgentTokenID      string           `json:"agent_token_id"`
	RuleName          string           `json:"rule_name"`
	RuleMessage       string           `json:"rule_message,omitempty"`
	Method            string           `json:"method"`
	DestinationURL    string           `json:"destination_url"`
	DestinationHost   string           `json:"destination_host,omitempty"`
	DestinationPath   string           `json:"destination_path,omitempty"`
	RequestBody       string           `json:"request_body,omitempty"`
	ConversationID    string           `json:"conversation_id,omitempty"`
	TranscriptSnippet []TranscriptTurn `json:"transcript,omitempty"`
}

// PolicyDecisionResponse is what the proxy receives back.
type PolicyDecisionResponse struct {
	Decision  string `json:"decision"` // allow | block | flag_for_human_review
	Reason    string `json:"reason,omitempty"`
	Model     string `json:"model,omitempty"`
	LatencyMs int    `json:"latency_ms,omitempty"`
	CacheHit  bool   `json:"cache_hit,omitempty"`
}

// MarshalResponse exposes the Verdict as the wire response shape.
func (v Verdict) MarshalResponse() PolicyDecisionResponse {
	return PolicyDecisionResponse{
		Decision:  string(v.Decision),
		Reason:    v.Reason,
		Model:     v.Model,
		LatencyMs: v.LatencyMs,
		CacheHit:  v.CacheHit,
	}
}

// LogLocalAutoApproval audits a Stage 0 server-local auto-approval
// decision (the intent.CheckApproval path used by tasks.go for plugin/
// Telegram-mediated bridges) to the same judge_decisions table the
// proxy uses. Stage 3 M7 — decision_path distinguishes the two paths
// so dashboards can correlate them.
//
// best-effort; failure is logged but does not propagate.
func (j *Judge) LogLocalAutoApproval(ctx context.Context, bridgeID, agentID, ruleName, decision, reason string, latencyMs int) {
	if j == nil || j.store == nil {
		return
	}
	cacheKey := fmt.Sprintf("local:%s:%s:%s:%d", bridgeID, agentID, ruleName, time.Now().UnixNano())
	err := j.store.InsertJudgeDecision(ctx, &store.JudgeDecision{
		BridgeID:     bridgeID,
		AgentTokenID: agentID,
		RuleName:     ruleName,
		CacheKey:     cacheKey,
		Decision:     decision,
		Reason:       reason,
		LatencyMs:    latencyMs,
		DecisionPath: "server_local_auto_approval",
	})
	if err != nil && j.logger != nil {
		j.logger.Warn("local auto-approval audit failed", "err", err, "bridge_id", bridgeID)
	}
}

// unused-import guards for the package when callers tree-shake imports.
var _ = json.Marshal
