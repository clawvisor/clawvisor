package notify

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
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
	slackCfg       SlackConfigStore
	slackInstaller SlackInstaller
	slackRecv      SlackInteractionReceiver
	telegramTester TelegramTester
	slackTester    SlackTester
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
		if sc, ok := n.(SlackConfigStore); ok && m.slackCfg == nil {
			m.slackCfg = sc
		}
		if si, ok := n.(SlackInstaller); ok && m.slackInstaller == nil {
			m.slackInstaller = si
		}
		if sr, ok := n.(SlackInteractionReceiver); ok && m.slackRecv == nil {
			m.slackRecv = sr
		}
		if tt, ok := n.(TelegramTester); ok && m.telegramTester == nil {
			m.telegramTester = tt
		}
		if st, ok := n.(SlackTester); ok && m.slackTester == nil {
			m.slackTester = st
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
	_ Notifier                 = (*MultiNotifier)(nil)
	_ TelegramPairer           = (*MultiNotifier)(nil)
	_ PollingDecrementer       = (*MultiNotifier)(nil)
	_ GroupObserver            = (*MultiNotifier)(nil)
	_ GroupDetector            = (*MultiNotifier)(nil)
	_ AgentGroupPairer         = (*MultiNotifier)(nil)
	_ GroupMembershipValidator = (*MultiNotifier)(nil)
	_ TargetMessageUpdater     = (*MultiNotifier)(nil)
)

// SlackConfigStore / SlackInstaller delegates. Each returns an error rather
// than panicking when no inner notifier provides Slack, so a deployment with
// Slack disabled degrades to a clear API error instead of a crash.

var errNoSlack = errors.New("slack notifications are not enabled on this deployment")

// SlackEnabled reports whether any inner notifier actually provides Slack.
//
// MultiNotifier satisfies SlackConfigStore and SlackInstaller unconditionally
// so it can return a clear error rather than panicking, which means a type
// assertion against it always succeeds and cannot be used to detect whether
// Slack is configured. Callers wiring Slack-specific routes must ask this
// instead, or a deployment with no Slack app advertises the feature and then
// fails every call.
func (m *MultiNotifier) SlackEnabled() bool {
	return m.slackCfg != nil && m.slackInstaller != nil
}

func (m *MultiNotifier) SaveSlackConfig(ctx context.Context, userID string, cfg SlackConfig) error {
	if m.slackCfg == nil {
		return errNoSlack
	}
	return m.slackCfg.SaveSlackConfig(ctx, userID, cfg)
}

func (m *MultiNotifier) SlackConfig(ctx context.Context, userID string) (SlackConfig, error) {
	if m.slackCfg == nil {
		return SlackConfig{}, errNoSlack
	}
	return m.slackCfg.SlackConfig(ctx, userID)
}

func (m *MultiNotifier) DeleteSlackConfig(ctx context.Context, userID string) error {
	if m.slackCfg == nil {
		return errNoSlack
	}
	return m.slackCfg.DeleteSlackConfig(ctx, userID)
}

// SendTelegramTestMessage sends only via Telegram, so an unconfigured Slack
// (or push) cannot make a delivered Telegram message report as failed.
func (m *MultiNotifier) SendTelegramTestMessage(ctx context.Context, userID string) error {
	if m.telegramTester == nil {
		return errors.New("telegram notifications are not enabled on this deployment")
	}
	return m.telegramTester.SendTelegramTestMessage(ctx, userID)
}

// SendSlackTestMessage sends only via Slack, for the same reason.
func (m *MultiNotifier) SendSlackTestMessage(ctx context.Context, userID string) error {
	if m.slackTester == nil {
		return errNoSlack
	}
	return m.slackTester.SendSlackTestMessage(ctx, userID)
}

func (m *MultiNotifier) SlackInstallURL(state string) (string, error) {
	if m.slackInstaller == nil {
		return "", errNoSlack
	}
	return m.slackInstaller.SlackInstallURL(state)
}

func (m *MultiNotifier) CompleteSlackInstall(ctx context.Context, code string) (SlackInstall, error) {
	if m.slackInstaller == nil {
		return SlackInstall{}, errNoSlack
	}
	return m.slackInstaller.CompleteSlackInstall(ctx, code)
}

func (m *MultiNotifier) ListSlackChannels(ctx context.Context, userID string) ([]SlackChannel, error) {
	if m.slackInstaller == nil {
		return nil, errNoSlack
	}
	return m.slackInstaller.ListSlackChannels(ctx, userID)
}

// HandleInteraction delegates to the Slack notifier. With Slack disabled it
// answers 501 rather than 404 so an operator sees a configuration problem
// rather than a routing one.
func (m *MultiNotifier) HandleInteraction(w http.ResponseWriter, r *http.Request) {
	if m.slackRecv == nil {
		http.Error(w, "Slack notifications are not enabled", http.StatusNotImplemented)
		return
	}
	m.slackRecv.HandleInteraction(w, r)
}

func (m *MultiNotifier) LookupSlackUser(ctx context.Context, userID, slackUserID string) (string, error) {
	if m.slackInstaller == nil {
		return "", errNoSlack
	}
	return m.slackInstaller.LookupSlackUser(ctx, userID, slackUserID)
}

// ── Notifier interface ────────────────────────────────────────────────────────

// fanOutSend runs one send across every notifier and reduces the results the
// way callers actually need.
//
// Two things were wrong with returning "first non-empty ID" plus every error.
//
// The error made a partial success look like a total one. A user with only
// Slack configured gets an error from the unconfigured Telegram notifier on
// every send, so callers logged "failed to send" for a prompt that was
// delivered. An error is only meaningful here if NO channel reached the user.
//
// The ID leaked across channels. Callers persist the returned ID against a
// hardcoded "telegram" channel, so when Telegram failed and Slack succeeded,
// Slack's "channel:ts" reference would be written into Telegram's row. Only
// notifiers that do NOT record their own message reference can supply it —
// a TargetMessageUpdater addresses its messages by target and has already
// stored what it needs.
func (m *MultiNotifier) fanOutSend(ctx context.Context, op string, send func(Notifier) (string, error)) (string, error) {
	var messageID string
	var errs []error
	delivered := 0

	for _, n := range m.notifiers {
		id, err := send(n)
		if err != nil {
			m.logger.WarnContext(ctx, "notifier: send failed", "op", op, "err", err)
			errs = append(errs, err)
			continue
		}
		// A nil error is not proof anyone was reached. Push returns
		// ("", nil) when the user has no paired devices, so counting it
		// would let a genuine Telegram failure report success and nobody
		// would learn the prompt never arrived. Treat delivery as proven
		// only by a message reference, or by a channel that records its
		// own (Slack), which returns one it has already stored.
		_, selfRecording := n.(TargetMessageUpdater)
		if id == "" && !selfRecording {
			m.logger.WarnContext(ctx, "notifier: send reported success but delivered nothing", "op", op)
			continue
		}
		delivered++

		// Only Telegram's reference may be returned. Callers persist it
		// against a hardcoded "telegram" channel, so handing back push's
		// "push:<daemonID>" would point later Telegram edits at a message
		// that never existed. Self-recording channels store their own.
		if _, isTelegram := n.(TelegramTester); isTelegram && messageID == "" {
			messageID = id
		}
	}

	if delivered > 0 {
		return messageID, nil
	}
	return "", errors.Join(errs...)
}

func (m *MultiNotifier) SendApprovalRequest(ctx context.Context, req ApprovalRequest) (string, error) {
	return m.fanOutSend(ctx, "SendApprovalRequest", func(n Notifier) (string, error) {
		return n.SendApprovalRequest(ctx, req)
	})
}

func (m *MultiNotifier) SendActivationRequest(ctx context.Context, req ActivationRequest) error {
	var errs []error
	for _, n := range m.notifiers {
		if err := n.SendActivationRequest(ctx, req); err != nil {
			m.logger.WarnContext(ctx, "notifier: SendActivationRequest failed", "err", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *MultiNotifier) SendTaskApprovalRequest(ctx context.Context, req TaskApprovalRequest) (string, error) {
	return m.fanOutSend(ctx, "SendTaskApprovalRequest", func(n Notifier) (string, error) {
		return n.SendTaskApprovalRequest(ctx, req)
	})
}

func (m *MultiNotifier) SendScopeExpansionRequest(ctx context.Context, req ScopeExpansionRequest) (string, error) {
	return m.fanOutSend(ctx, "SendScopeExpansionRequest", func(n Notifier) (string, error) {
		return n.SendScopeExpansionRequest(ctx, req)
	})
}

func (m *MultiNotifier) SendConnectionRequest(ctx context.Context, req ConnectionRequest) (string, error) {
	return m.fanOutSend(ctx, "SendConnectionRequest", func(n Notifier) (string, error) {
		return n.SendConnectionRequest(ctx, req)
	})
}

func (m *MultiNotifier) UpdateMessage(ctx context.Context, userID, messageID, text string) error {
	var errs []error
	for _, n := range m.notifiers {
		if err := n.UpdateMessage(ctx, userID, messageID, text); err != nil {
			m.logger.WarnContext(ctx, "notifier: UpdateMessage failed", "err", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// UpdateMessageForTarget delegates to every inner notifier that resolves
// messages by approval target rather than by an opaque message ID. Notifiers
// that do not implement TargetMessageUpdater are skipped — they are served by
// the messageID-based UpdateMessage above.
func (m *MultiNotifier) UpdateMessageForTarget(ctx context.Context, userID, targetType, targetID, text string) error {
	var errs []error
	for _, n := range m.notifiers {
		tu, ok := n.(TargetMessageUpdater)
		if !ok {
			continue
		}
		if err := tu.UpdateMessageForTarget(ctx, userID, targetType, targetID, text); err != nil {
			m.logger.WarnContext(ctx, "notifier: UpdateMessageForTarget failed", "err", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *MultiNotifier) SendTestMessage(ctx context.Context, userID string) error {
	var errs []error
	for _, n := range m.notifiers {
		if err := n.SendTestMessage(ctx, userID); err != nil {
			m.logger.WarnContext(ctx, "notifier: SendTestMessage failed", "err", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *MultiNotifier) SendAlert(ctx context.Context, userID, text string) error {
	var errs []error
	for _, n := range m.notifiers {
		if err := n.SendAlert(ctx, userID, text); err != nil {
			m.logger.WarnContext(ctx, "notifier: SendAlert failed", "err", err)
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
