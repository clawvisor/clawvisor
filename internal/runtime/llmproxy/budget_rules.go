package llmproxy

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/clawvisor/clawvisor/internal/runtime/conversation"
	"github.com/clawvisor/clawvisor/internal/runtime/llmproxy/pricing"
	"github.com/clawvisor/clawvisor/pkg/store"
)

const StageBudgetWarning PendingApprovalStage = "budget_warning"

type BudgetOverrideCache struct {
	mu        sync.Mutex
	overrides map[string]time.Time
}

func NewBudgetOverrideCache() *BudgetOverrideCache {
	return &BudgetOverrideCache{
		overrides: make(map[string]time.Time),
	}
}

func (c *BudgetOverrideCache) Add(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.overrides[id] = time.Now().Add(10 * time.Minute)
}

func (c *BudgetOverrideCache) Has(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.overrides[id]
	if !ok {
		return false
	}
	if time.Now().After(t) {
		delete(c.overrides, id)
		return false
	}
	return true
}

type RateLimitState struct {
	RequestsLimit     int64
	RequestsRemaining int64
	RequestsResetSecs float64
	TokensLimit       int64
	TokensRemaining   int64
	TokensResetSecs   float64
	LastUpdated       time.Time
}

type RateLimitTracker struct {
	mu     sync.Mutex
	states map[string]RateLimitState
}

func NewRateLimitTracker() *RateLimitTracker {
	return &RateLimitTracker{
		states: make(map[string]RateLimitState),
	}
}

func (t *RateLimitTracker) Update(agentID, provider, model string, header http.Header) {
	t.mu.Lock()
	defer t.mu.Unlock()

	reqLimit, _ := strconv.ParseInt(header.Get("anthropic-ratelimit-requests-limit"), 10, 64)
	reqRem, _ := strconv.ParseInt(header.Get("anthropic-ratelimit-requests-remaining"), 10, 64)
	reqReset, _ := strconv.ParseFloat(header.Get("anthropic-ratelimit-requests-reset"), 64)

	tokLimit, _ := strconv.ParseInt(header.Get("anthropic-ratelimit-tokens-limit"), 10, 64)
	tokRem, _ := strconv.ParseInt(header.Get("anthropic-ratelimit-tokens-remaining"), 10, 64)
	tokReset, _ := strconv.ParseFloat(header.Get("anthropic-ratelimit-tokens-reset"), 64)

	if reqLimit == 0 {
		reqLimit, _ = strconv.ParseInt(header.Get("x-ratelimit-limit-requests"), 10, 64)
		reqRem, _ = strconv.ParseInt(header.Get("x-ratelimit-remaining-requests"), 10, 64)
		reqReset, _ = strconv.ParseFloat(strings.TrimSuffix(header.Get("x-ratelimit-reset-requests"), "s"), 64)
	}
	if tokLimit == 0 {
		tokLimit, _ = strconv.ParseInt(header.Get("x-ratelimit-limit-tokens"), 10, 64)
		tokRem, _ = strconv.ParseInt(header.Get("x-ratelimit-remaining-tokens"), 10, 64)
		tokReset, _ = strconv.ParseFloat(strings.TrimSuffix(header.Get("x-ratelimit-reset-tokens"), "s"), 64)
	}

	if reqLimit > 0 || tokLimit > 0 {
		key := agentID + ":" + provider + ":" + pricing.Normalize(model)
		t.states[key] = RateLimitState{
			RequestsLimit:     reqLimit,
			RequestsRemaining: reqRem,
			RequestsResetSecs: reqReset,
			TokensLimit:       tokLimit,
			TokensRemaining:   tokRem,
			TokensResetSecs:   tokReset,
			LastUpdated:       time.Now(),
		}
	}
}

func (t *RateLimitTracker) ThrottleDelay(agentID, provider, model string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()

	key := agentID + ":" + provider + ":" + pricing.Normalize(model)
	state, ok := t.states[key]
	if !ok || time.Since(state.LastUpdated) > 10*time.Minute {
		return 0
	}

	var delay time.Duration
	if state.TokensLimit > 0 && state.TokensRemaining < state.TokensLimit/10 && state.TokensResetSecs > 0 {
		d := time.Duration(float64(time.Second) * state.TokensResetSecs * (1.0 - float64(state.TokensRemaining)/float64(state.TokensLimit)))
		if d > 5*time.Second {
			d = 5 * time.Second
		}
		if d > delay {
			delay = d
		}
	}
	if state.RequestsLimit > 0 && state.RequestsRemaining < state.RequestsLimit/10 && state.RequestsResetSecs > 0 {
		d := time.Duration(float64(time.Second) * state.RequestsResetSecs * (1.0 - float64(state.RequestsRemaining)/float64(state.RequestsLimit)))
		if d > 5*time.Second {
			d = 5 * time.Second
		}
		if d > delay {
			delay = d
		}
	}
	return delay
}

type EvaluateBudgetResult struct {
	Blocked    bool
	Refused    bool
	Warning    bool
	Message    string
	ApprovalID string
}

func EvaluateBudget(ctx context.Context, db store.Store, approvals PendingApprovalCache, overrides *BudgetOverrideCache, agent *store.Agent, taskID string, provider conversation.Provider, conversationID string) (EvaluateBudgetResult, error) {
	var limitCost, limitTokens *int64
	var currentCost, currentTokens int64
	enforcingTaskBudget := false

	if taskID != "" {
		task, err := db.GetTask(ctx, taskID)
		if err != nil && err != store.ErrNotFound {
			return EvaluateBudgetResult{}, err
		}
		if err == nil && task != nil {
			limitCost = task.MaxCostMicros
			limitTokens = task.MaxTokens
			if limitCost != nil || limitTokens != nil {
				enforcingTaskBudget = true
				summary, err := db.GetTaskCost(ctx, agent.UserID, taskID)
				if err != nil {
					return EvaluateBudgetResult{}, err
				}
				if summary != nil {
					currentCost = summary.CostMicros
					currentTokens = summary.InputTokens + summary.OutputTokens
				}
			}
		}
	}

	if !enforcingTaskBudget {
		settings, err := db.GetAgentRuntimeSettings(ctx, agent.ID)
		if err != nil && err != store.ErrNotFound {
			return EvaluateBudgetResult{}, err
		}
		if err == nil && settings != nil {
			limitCost = settings.MaxCostMicros
			limitTokens = settings.MaxTokens
			summary, err := db.GetAgentCost(ctx, agent.UserID, agent.ID)
			if err != nil {
				return EvaluateBudgetResult{}, err
			}
			if summary != nil {
				currentCost = summary.CostMicros
				currentTokens = summary.InputTokens + summary.OutputTokens
			}
		}
	}

	if limitCost == nil && limitTokens == nil {
		return EvaluateBudgetResult{}, nil
	}

	var overrideID string
	if enforcingTaskBudget {
		overrideID = fmt.Sprintf("task:%s", taskID)
	} else {
		overrideID = fmt.Sprintf("agent:%s", agent.ID)
	}

	if overrides.Has(overrideID) {
		return EvaluateBudgetResult{}, nil
	}

	holdSearchKey := ResolveRequest{
		UserID:         agent.UserID,
		AgentID:        agent.ID,
		Provider:       provider,
		ConversationID: conversationID,
		Stage:          StageBudgetWarning,
	}

	if (limitCost != nil && currentCost >= *limitCost) || (limitTokens != nil && currentTokens >= *limitTokens) {
		var reason string
		if limitCost != nil && currentCost >= *limitCost {
			reason = fmt.Sprintf("Cost budget exceeded: spent $%0.4f of limit $%0.4f", float64(currentCost)/1e6, float64(*limitCost)/1e6)
		} else {
			reason = fmt.Sprintf("Token budget exceeded: consumed %d of limit %d tokens", currentTokens, *limitTokens)
		}
		return EvaluateBudgetResult{
			Blocked: true,
			Refused: true,
			Message: fmt.Sprintf("Clawvisor: %s. Run `/allow budget` to raise it.", reason),
		}, nil
	}

	isCostWarning := limitCost != nil && currentCost >= (*limitCost)*9/10
	isTokenWarning := limitTokens != nil && currentTokens >= (*limitTokens)*9/10

	if isCostWarning || isTokenWarning {
		var warningDetail string
		if isCostWarning {
			warningDetail = fmt.Sprintf("cost spent $%0.4f of limit $%0.4f", float64(currentCost)/1e6, float64(*limitCost)/1e6)
		} else {
			warningDetail = fmt.Sprintf("tokens consumed %d of limit %d", currentTokens, *limitTokens)
		}

		if peeked, err := approvals.Peek(ctx, holdSearchKey); err == nil && peeked != nil {
			return EvaluateBudgetResult{
				Blocked:    true,
				Warning:    true,
				ApprovalID: peeked.ID,
				Message:    fmt.Sprintf("Clawvisor: Task budget is at 90%% (%s). Reply y or approve to continue. (Hold ID: %s)", warningDetail, peeked.ID),
			}, nil
		}

		approvalID, err := newLiteApprovalID()
		if err != nil {
			return EvaluateBudgetResult{}, err
		}
		hold := PendingLiteApproval{
			ID:             approvalID,
			UserID:         agent.UserID,
			AgentID:        agent.ID,
			Provider:       provider,
			ConversationID: conversationID,
			Reason:         fmt.Sprintf("Budget warning: %s", warningDetail),
			Stage:          StageBudgetWarning,
			CreatedAt:      time.Now(),
			ExpiresAt:      time.Now().Add(10 * time.Minute),
			PendingTaskID:  taskID,
		}
		if _, err := approvals.Hold(ctx, hold); err != nil {
			return EvaluateBudgetResult{}, err
		}

		return EvaluateBudgetResult{
			Blocked:    true,
			Warning:    true,
			ApprovalID: approvalID,
			Message:    fmt.Sprintf("Clawvisor: Task budget is at 90%% (%s). Reply y or approve to continue. (Hold ID: %s)", warningDetail, approvalID),
		}, nil
	}

	return EvaluateBudgetResult{}, nil
}

func RewriteBudgetApprovalReply(ctx context.Context, req TaskReplyRewriteRequest, overrides *BudgetOverrideCache) (TaskReplyRewriteResult, error) {
	editor, ok := newApprovalBodyEditor(req.HTTPRequest, req.Provider, req.Body)
	if !ok {
		return TaskReplyRewriteResult{Body: req.Body}, nil
	}
	verb, approvalID, ok := editor.LatestApprovalReply()
	if !ok || req.PendingApproval == nil || req.Agent == nil {
		return TaskReplyRewriteResult{Body: req.Body}, nil
	}
	if verb != "approve" {
		return TaskReplyRewriteResult{Body: req.Body}, nil
	}

	hold, err := req.PendingApproval.Peek(ctx, ResolveRequest{
		UserID:         req.Agent.UserID,
		AgentID:        req.Agent.ID,
		Provider:       req.Provider,
		ConversationID: req.ConversationID,
		ApprovalID:     approvalID,
		Stage:          StageBudgetWarning,
	})
	if err != nil || hold == nil {
		return TaskReplyRewriteResult{Body: req.Body}, err
	}

	_, err = req.PendingApproval.Resolve(ctx, ResolveRequest{
		UserID:         req.Agent.UserID,
		AgentID:        req.Agent.ID,
		Provider:       req.Provider,
		ConversationID: req.ConversationID,
		ApprovalID:     hold.ID,
		Stage:          StageBudgetWarning,
	})
	if err != nil {
		return TaskReplyRewriteResult{Body: req.Body}, err
	}

	overrideID := fmt.Sprintf("agent:%s", req.Agent.ID)
	overrides.Add(overrideID)
	if hold.PendingTaskID != "" {
		overrides.Add(fmt.Sprintf("task:%s", hold.PendingTaskID))
	}

	return TaskReplyRewriteResult{Body: req.Body, Rewritten: true}, nil
}
