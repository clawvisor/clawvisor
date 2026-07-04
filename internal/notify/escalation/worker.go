package escalation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/pkg/config"
	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store"
)

const (
	statusRetryDelay = 15 * time.Second
	defaultPollEvery = 15 * time.Second
	defaultDueLimit  = 50
)

// Store is the persistence surface the escalation worker needs.
type Store interface {
	CreateApprovalEscalation(ctx context.Context, e *store.ApprovalEscalation) error
	ListDueApprovalEscalations(ctx context.Context, now time.Time, limit int) ([]*store.ApprovalEscalation, error)
	ClaimApprovalEscalationStep(ctx context.Context, id string, currentStep int, now time.Time) (bool, error)
	CompleteApprovalEscalationStep(ctx context.Context, id string, claimedStep, nextStep int, nextAt time.Time) (bool, error)
	ReleaseApprovalEscalationStep(ctx context.Context, id string, claimedStep int, retryAt time.Time) error
	ResolveApprovalEscalation(ctx context.Context, targetType, targetID string) error
	TimeoutApprovalEscalation(ctx context.Context, id string, claimedStep int) error
	SaveNotificationMessage(ctx context.Context, targetType, targetID, channel, messageID string) error
	ListNotificationMessages(ctx context.Context, targetType, targetID string) ([]*store.NotificationMessage, error)
}

// Manager starts, advances, resolves, and cleans up request-approval
// notification escalation sessions.
type Manager struct {
	store        Store
	notifier     notify.Notifier
	chain        []Step
	pollInterval time.Duration
	now          func() time.Time
	logger       *slog.Logger
}

// Step describes one channel in the configured fallback chain.
type Step struct {
	Channel      string `json:"channel"`
	DelaySeconds int    `json:"delay_seconds"`
}

// New creates an escalation manager from validated runtime config. It returns
// nil when escalation is disabled or no notifier is configured.
func New(st Store, notifier notify.Notifier, cfg config.NotificationEscalationConfig, logger *slog.Logger) *Manager {
	if !cfg.Enabled || st == nil || notifier == nil {
		return nil
	}
	chain := make([]Step, 0, len(cfg.DefaultChain))
	for _, step := range cfg.DefaultChain {
		chain = append(chain, Step{
			Channel:      strings.ToLower(strings.TrimSpace(step.Channel)),
			DelaySeconds: step.DelaySeconds,
		})
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		store:        st,
		notifier:     notifier,
		chain:        chain,
		pollInterval: defaultPollEvery,
		now:          time.Now,
		logger:       logger,
	}
}

// StartApproval persists a new approval escalation and dispatches any step due
// immediately, such as a first step configured with delay_seconds: 0.
func (m *Manager) StartApproval(ctx context.Context, approvalRecordID, targetType, targetID string, req notify.ApprovalRequest) error {
	if m == nil || len(m.chain) == 0 {
		return nil
	}
	now := m.now().UTC()
	reqJSON, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal approval request: %w", err)
	}
	chainJSON, err := json.Marshal(m.chain)
	if err != nil {
		return fmt.Errorf("marshal escalation chain: %w", err)
	}
	firstAt := now.Add(time.Duration(m.chain[0].DelaySeconds) * time.Second)
	if err := m.store.CreateApprovalEscalation(ctx, &store.ApprovalEscalation{
		ApprovalRecordID: approvalRecordID,
		UserID:           req.UserID,
		TargetType:       targetType,
		TargetID:         targetID,
		ApprovalRequest:  reqJSON,
		EscalationChain:  chainJSON,
		CurrentStep:      0,
		NextEscalateAt:   firstAt,
		Status:           "active",
	}); err != nil {
		return fmt.Errorf("create approval escalation: %w", err)
	}
	if !firstAt.After(now) {
		if err := m.ProcessDueOnce(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Run polls for due escalation steps until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	if m == nil {
		return
	}
	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.ProcessDueOnce(ctx); err != nil {
				m.logger.Warn("notification escalation poll failed", "err", err)
			}
		}
	}
}

// ProcessDueOnce processes currently due escalation rows. It is exported for
// focused tests and for immediate first-step dispatch.
func (m *Manager) ProcessDueOnce(ctx context.Context) error {
	if m == nil {
		return nil
	}
	now := m.now().UTC()
	due, err := m.store.ListDueApprovalEscalations(ctx, now, defaultDueLimit)
	if err != nil {
		return err
	}
	for _, esc := range due {
		if err := m.process(ctx, esc, now); err != nil {
			m.logger.Warn("notification escalation step failed", "id", esc.ID, "target_type", esc.TargetType, "target_id", esc.TargetID, "err", err)
		}
	}
	return nil
}

// Resolve marks the escalation complete and best-effort updates every channel
// that received a message for this approval.
func (m *Manager) Resolve(ctx context.Context, targetType, targetID, userID, text string) {
	if m == nil {
		return
	}
	if err := m.store.ResolveApprovalEscalation(ctx, targetType, targetID); err != nil {
		m.logger.Warn("notification escalation resolve failed", "target_type", targetType, "target_id", targetID, "err", err)
	}
	messages, err := m.store.ListNotificationMessages(ctx, targetType, targetID)
	if err != nil {
		m.logger.Warn("notification escalation list messages failed", "target_type", targetType, "target_id", targetID, "err", err)
		return
	}
	for _, msg := range messages {
		if msg.MessageID == "" {
			continue
		}
		if err := m.updateChannelMessage(ctx, msg.Channel, userID, msg.MessageID, text); err != nil {
			m.logger.Warn("notification escalation message cleanup failed", "channel", msg.Channel, "target_type", targetType, "target_id", targetID, "err", err)
		}
	}
}

func (m *Manager) process(ctx context.Context, esc *store.ApprovalEscalation, now time.Time) error {
	chain, err := decodeChain(esc.EscalationChain)
	if err != nil {
		return err
	}
	claimed, err := m.store.ClaimApprovalEscalationStep(ctx, esc.ID, esc.CurrentStep, now)
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	if esc.CurrentStep >= len(chain) {
		return m.store.TimeoutApprovalEscalation(ctx, esc.ID, esc.CurrentStep)
	}

	var req notify.ApprovalRequest
	if err := json.Unmarshal(esc.ApprovalRequest, &req); err != nil {
		_ = m.store.ReleaseApprovalEscalationStep(ctx, esc.ID, esc.CurrentStep, now.Add(statusRetryDelay))
		return fmt.Errorf("decode approval request: %w", err)
	}

	step := chain[esc.CurrentStep]
	channel := strings.ToLower(strings.TrimSpace(step.Channel))
	msgID, sendErr := m.sendToChannel(ctx, channel, req)
	if sendErr != nil {
		m.logger.Warn("notification escalation dispatch failed", "channel", channel, "target_type", esc.TargetType, "target_id", esc.TargetID, "err", sendErr)
	} else if msgID != "" {
		if err := m.store.SaveNotificationMessage(ctx, esc.TargetType, esc.TargetID, channel, msgID); err != nil {
			m.logger.Warn("notification escalation message coordinate save failed", "channel", channel, "target_type", esc.TargetType, "target_id", esc.TargetID, "err", err)
		}
	}

	nextStep := esc.CurrentStep + 1
	if nextStep >= len(chain) {
		return m.store.TimeoutApprovalEscalation(ctx, esc.ID, esc.CurrentStep)
	}
	nextAt := nextStepTime(esc.CreatedAt, now, chain[nextStep])
	ok, err := m.store.CompleteApprovalEscalationStep(ctx, esc.ID, esc.CurrentStep, nextStep, nextAt)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return sendErr
}

func (m *Manager) sendToChannel(ctx context.Context, channel string, req notify.ApprovalRequest) (string, error) {
	if sender, ok := m.notifier.(notify.ChannelApprovalSender); ok {
		return sender.SendApprovalRequestToChannel(ctx, channel, req)
	}
	if provider, ok := m.notifier.(notify.ChannelProvider); ok && strings.EqualFold(provider.NotificationChannel(), channel) {
		return m.notifier.SendApprovalRequest(ctx, req)
	}
	return "", fmt.Errorf("notification channel unavailable: %s", channel)
}

func (m *Manager) updateChannelMessage(ctx context.Context, channel, userID, messageID, text string) error {
	if updater, ok := m.notifier.(notify.ChannelMessageUpdater); ok {
		return updater.UpdateMessageForChannel(ctx, channel, userID, messageID, text)
	}
	if provider, ok := m.notifier.(notify.ChannelProvider); ok && strings.EqualFold(provider.NotificationChannel(), channel) {
		return m.notifier.UpdateMessage(ctx, userID, messageID, text)
	}
	return fmt.Errorf("notification channel unavailable: %s", channel)
}

func decodeChain(raw json.RawMessage) ([]Step, error) {
	var chain []Step
	if err := json.Unmarshal(raw, &chain); err != nil {
		return nil, fmt.Errorf("decode escalation chain: %w", err)
	}
	if len(chain) == 0 {
		return nil, fmt.Errorf("escalation chain is empty")
	}
	return chain, nil
}

func nextStepTime(createdAt, now time.Time, step Step) time.Time {
	base := createdAt
	if base.IsZero() {
		base = now
	}
	nextAt := base.UTC().Add(time.Duration(step.DelaySeconds) * time.Second)
	if nextAt.Before(now) {
		return now
	}
	return nextAt
}
