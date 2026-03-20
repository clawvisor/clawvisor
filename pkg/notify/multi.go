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
	pairer    TelegramPairer
	decrement PollingDecrementer
}

// NewMultiNotifier creates a MultiNotifier that delegates to the given notifiers.
// It inspects each notifier for optional interfaces (TelegramPairer, PollingDecrementer,
// DecisionChannel) and wires them through.
func NewMultiNotifier(logger *slog.Logger, notifiers ...Notifier) *MultiNotifier {
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
				for d := range ch {
					m.decisionCh <- d
				}
			}()
		}
	}

	// Close the merged channel when all inner channels close.
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
