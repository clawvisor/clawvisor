package notify

import (
	"context"
	"errors"
	"log/slog"
	"sync"
)

// MultiNotifier fans out notifications to multiple Notifier implementations.
// Errors are logged but do not short-circuit delivery to remaining notifiers.
type MultiNotifier struct {
	notifiers  []Notifier
	logger     *slog.Logger
	decisionCh chan CallbackDecision

	// Delegated interfaces discovered on construction.
	pairer         TelegramPairer
	decrement      PollingDecrementer
	groupObs       GroupObserver
	groupDetector  GroupDetector
	agentPairer    AgentGroupPairer
	groupValidator GroupMembershipValidator
}

// NewMultiNotifier creates a MultiNotifier that delegates to the given notifiers.
// It inspects each notifier for optional interfaces (TelegramPairer, PollingDecrementer,
// DecisionChannel) and wires them through.
func NewMultiNotifier(ctx context.Context, logger *slog.Logger, notifiers ...Notifier) *MultiNotifier {
	m := &MultiNotifier{
		notifiers:  notifiers,
		logger:     logger,
		decisionCh: make(chan CallbackDecision, 64),
	}

	for _, n := range notifiers {
		if p, ok := n.(TelegramPairer); ok && m.pairer == nil {
			m.pairer = p
		}
		if d, ok := n.(PollingDecrementer); ok && m.decrement == nil {
			m.decrement = d
		}
		if g, ok := n.(GroupObserver); ok && m.groupObs == nil {
			m.groupObs = g
		}
		if gd, ok := n.(GroupDetector); ok && m.groupDetector == nil {
			m.groupDetector = gd
		}
		if ap, ok := n.(AgentGroupPairer); ok && m.agentPairer == nil {
			m.agentPairer = ap
		}
		if gv, ok := n.(GroupMembershipValidator); ok && m.groupValidator == nil {
			m.groupValidator = gv
		}
	}

	// Fan-in all decision channels into the merged channel.
	var wg sync.WaitGroup
	for _, n := range notifiers {
		type decisionSource interface {
			DecisionChannel() <-chan CallbackDecision
		}
		if ds, ok := n.(decisionSource); ok {
			ch := ds.DecisionChannel()
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case d, ok := <-ch:
						if !ok {
							return
						}
						m.decisionCh <- d
					case <-ctx.Done():
						return
					}
				}
			}()
		}
	}

	// Close the merged channel when all inner channels close or ctx cancels.
	go func() {
		wg.Wait()
		close(m.decisionCh)
	}()

	return m
}

// Compile-time interface checks.
var (
	_ Notifier           = (*MultiNotifier)(nil)
	_ TelegramPairer     = (*MultiNotifier)(nil)
	_ PollingDecrementer = (*MultiNotifier)(nil)
	_ GroupObserver      = (*MultiNotifier)(nil)
	_ GroupDetector      = (*MultiNotifier)(nil)
	_ AgentGroupPairer        = (*MultiNotifier)(nil)
	_ GroupMembershipValidator = (*MultiNotifier)(nil)
)

// ── Notifier interface ────────────────────────────────────────────────────────

func (m *MultiNotifier) SendApprovalRequest(ctx context.Context, req ApprovalRequest) (string, error) {
	var messageID string
	var errs []error
	for _, n := range m.notifiers {
		id, err := n.SendApprovalRequest(ctx, req)
		if err != nil {
			m.logger.Warn("notifier: SendApprovalRequest failed", "err", err)
			errs = append(errs, err)
		} else if messageID == "" && id != "" {
			messageID = id
		}
	}
	return messageID, errors.Join(errs...)
}

func (m *MultiNotifier) SendActivationRequest(ctx context.Context, req ActivationRequest) error {
	var errs []error
	for _, n := range m.notifiers {
		if err := n.SendActivationRequest(ctx, req); err != nil {
			m.logger.Warn("notifier: SendActivationRequest failed", "err", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *MultiNotifier) SendTaskApprovalRequest(ctx context.Context, req TaskApprovalRequest) (string, error) {
	var messageID string
	var errs []error
	for _, n := range m.notifiers {
		id, err := n.SendTaskApprovalRequest(ctx, req)
		if err != nil {
			m.logger.Warn("notifier: SendTaskApprovalRequest failed", "err", err)
			errs = append(errs, err)
		} else if messageID == "" && id != "" {
			messageID = id
		}
	}
	return messageID, errors.Join(errs...)
}

func (m *MultiNotifier) SendScopeExpansionRequest(ctx context.Context, req ScopeExpansionRequest) (string, error) {
	var messageID string
	var errs []error
	for _, n := range m.notifiers {
		id, err := n.SendScopeExpansionRequest(ctx, req)
		if err != nil {
			m.logger.Warn("notifier: SendScopeExpansionRequest failed", "err", err)
			errs = append(errs, err)
		} else if messageID == "" && id != "" {
			messageID = id
		}
	}
	return messageID, errors.Join(errs...)
}

func (m *MultiNotifier) SendConnectionRequest(ctx context.Context, req ConnectionRequest) (string, error) {
	var messageID string
	var errs []error
	for _, n := range m.notifiers {
		id, err := n.SendConnectionRequest(ctx, req)
		if err != nil {
			m.logger.Warn("notifier: SendConnectionRequest failed", "err", err)
			errs = append(errs, err)
		} else if messageID == "" && id != "" {
			messageID = id
		}
	}
	return messageID, errors.Join(errs...)
}

func (m *MultiNotifier) UpdateMessage(ctx context.Context, userID, messageID, text string) error {
	var errs []error
	for _, n := range m.notifiers {
		if err := n.UpdateMessage(ctx, userID, messageID, text); err != nil {
			m.logger.Warn("notifier: UpdateMessage failed", "err", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *MultiNotifier) SendTestMessage(ctx context.Context, userID string) error {
	var errs []error
	for _, n := range m.notifiers {
		if err := n.SendTestMessage(ctx, userID); err != nil {
			m.logger.Warn("notifier: SendTestMessage failed", "err", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *MultiNotifier) SendAlert(ctx context.Context, userID, text string) error {
	var errs []error
	for _, n := range m.notifiers {
		if err := n.SendAlert(ctx, userID, text); err != nil {
			m.logger.Warn("notifier: SendAlert failed", "err", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ── DecisionChannel + RunCleanup ──────────────────────────────────────────────

// DecisionChannel returns a merged channel that receives decisions from all
// inner notifiers that support inline callbacks.
func (m *MultiNotifier) DecisionChannel() <-chan CallbackDecision {
	return m.decisionCh
}

// RunCleanup delegates to each inner notifier that supports it.
func (m *MultiNotifier) RunCleanup(ctx context.Context) {
	type cleaner interface {
		RunCleanup(context.Context)
	}
	var wg sync.WaitGroup
	for _, n := range m.notifiers {
		if c, ok := n.(cleaner); ok {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.RunCleanup(ctx)
			}()
		}
	}
	wg.Wait()
}

// ── TelegramPairer delegation ─────────────────────────────────────────────────

func (m *MultiNotifier) StartPairing(ctx context.Context, userID, botToken string) (*PairingSession, error) {
	if m.pairer == nil {
		return nil, errors.New("telegram pairing not available")
	}
	return m.pairer.StartPairing(ctx, userID, botToken)
}

func (m *MultiNotifier) PairingStatus(pairingID string) (*PairingSession, error) {
	if m.pairer == nil {
		return nil, errors.New("telegram pairing not available")
	}
	return m.pairer.PairingStatus(pairingID)
}

func (m *MultiNotifier) ConfirmPairing(ctx context.Context, pairingID, code string) error {
	if m.pairer == nil {
		return errors.New("telegram pairing not available")
	}
	return m.pairer.ConfirmPairing(ctx, pairingID, code)
}

func (m *MultiNotifier) CancelPairing(pairingID string) {
	if m.pairer != nil {
		m.pairer.CancelPairing(pairingID)
	}
}

// ── PollingDecrementer delegation ─────────────────────────────────────────────

func (m *MultiNotifier) DecrementPolling(userID string) {
	if m.decrement != nil {
		m.decrement.DecrementPolling(userID)
	}
}

// ── GroupObserver delegation ──────────────────────────────────────────────────

func (m *MultiNotifier) EnsureGroupObservation(userID, botToken, chatID, groupChatID string) {
	if m.groupObs != nil {
		m.groupObs.EnsureGroupObservation(userID, botToken, chatID, groupChatID)
	}
}

func (m *MultiNotifier) StopGroupObservation(userID, groupChatID string) {
	if m.groupObs != nil {
		m.groupObs.StopGroupObservation(userID, groupChatID)
	}
}

// ── GroupDetector delegation ──────────────────────────────────────────────────

func (m *MultiNotifier) DetectGroups(ctx context.Context, userID string) ([]PendingGroup, error) {
	if m.groupDetector == nil {
		return nil, errors.New("group detection not available")
	}
	return m.groupDetector.DetectGroups(ctx, userID)
}

func (m *MultiNotifier) PendingGroups(userID string) []PendingGroup {
	if m.groupDetector == nil {
		return nil
	}
	return m.groupDetector.PendingGroups(userID)
}

func (m *MultiNotifier) RemovePendingGroup(userID, chatID string) {
	if m.groupDetector != nil {
		m.groupDetector.RemovePendingGroup(userID, chatID)
	}
}

// ── AgentGroupPairer delegation ───────────────────────────────────────────────

func (m *MultiNotifier) StartGroupPairing(ctx context.Context, userID, groupChatID, baseURL string) (string, error) {
	if m.agentPairer == nil {
		return "", errors.New("agent-group pairing not available")
	}
	return m.agentPairer.StartGroupPairing(ctx, userID, groupChatID, baseURL)
}

func (m *MultiNotifier) CompleteGroupPairing(ctx context.Context, sessionID, agentID, agentUserID string) error {
	if m.agentPairer == nil {
		return errors.New("agent-group pairing not available")
	}
	return m.agentPairer.CompleteGroupPairing(ctx, sessionID, agentID, agentUserID)
}

func (m *MultiNotifier) AgentGroupChatID(ctx context.Context, agentID string) (string, error) {
	if m.agentPairer == nil {
		return "", nil
	}
	return m.agentPairer.AgentGroupChatID(ctx, agentID)
}

func (m *MultiNotifier) PairedAgentIDs(ctx context.Context, groupChatID string) ([]string, error) {
	if m.agentPairer == nil {
		return nil, nil
	}
	return m.agentPairer.PairedAgentIDs(ctx, groupChatID)
}

func (m *MultiNotifier) UnpairAgentsForGroup(ctx context.Context, groupChatID string) error {
	if m.agentPairer == nil {
		return nil
	}
	return m.agentPairer.UnpairAgentsForGroup(ctx, groupChatID)
}

// ── GroupMembershipValidator delegation ────────────────────────────────────────

func (m *MultiNotifier) ValidateGroupMembership(ctx context.Context, userID, groupChatID string) (*GroupInfo, error) {
	if m.groupValidator == nil {
		return nil, errors.New("group membership validation not available")
	}
	return m.groupValidator.ValidateGroupMembership(ctx, userID, groupChatID)
}

// BootstrapGroupObservation delegates to the underlying notifier that supports it.
func (m *MultiNotifier) BootstrapGroupObservation(ctx context.Context) {
	type bootstrapper interface {
		BootstrapGroupObservation(context.Context)
	}
	for _, n := range m.notifiers {
		if b, ok := n.(bootstrapper); ok {
			b.BootstrapGroupObservation(ctx)
		}
	}
}
