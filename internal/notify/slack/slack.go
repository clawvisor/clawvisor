// Package slack implements notify.Notifier on top of the Slack Web API.
//
// Unlike the Telegram notifier, where each user brings their own bot token
// and the server long-polls getUpdates, Slack has no polling equivalent for
// button clicks. A single Clawvisor-owned Slack app is installed into the
// user's workspace via OAuth, and Slack POSTs interactions back to a public
// endpoint (see interactivity.go). That makes this notifier cloud-only: it
// needs a publicly reachable, signature-verified callback URL.
//
// Per-user state lives in notification_configs (channel = "slack"); the bot
// token is held in the credential vault, never in the JSON column.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/display"
	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

// vaultBotTokenKey is the key under which a workspace bot token is encrypted
// in the credential vault.
const vaultBotTokenKey = "notify.slack.bot_token"

// notifyChannel is the notification_configs.channel discriminator.
const notifyChannel = "slack"

// PendingChannel is the ChannelID written for a workspace that has completed
// OAuth but has not yet had a channel picked. SaveSlackConfig requires a
// non-empty channel, so the install is carried forward under this sentinel
// until SlackSetChannel replaces it.
//
// It is exported because the API layer stores it and the send path has to
// refuse it; two independent spellings of the same magic string is exactly
// how a sentinel becomes a real destination.
const PendingChannel = "__pending__"

// ErrNoChannelConfigured is returned by every send path when the user's
// workspace is installed but no channel has been chosen yet.
var ErrNoChannelConfigured = errors.New("slack: no approval channel selected")

// defaultAPIBase is the Slack Web API root.
const defaultAPIBase = "https://slack.com/api/"

// callbackTTL bounds how long a posted button stays live.
//
// This is deliberately long rather than matching a gateway approval's own
// expiry. The server is the authority on whether a request is still
// actionable — ApproveByRequestID re-checks ExpiresAt, and the expiry sweeper
// rewrites the message — so a short token TTL adds no safety and actively
// misleads: a task in pending_approval has no ExpiresAt at all and stays
// approvable indefinitely, so a 6-minute token made a still-valid prompt
// report itself expired. The token is an unguessable single-use capability,
// not an expiry mechanism.
const callbackTTL = messageContextTTL

// messageContextTTL outlives callbackTTL: a request can be resolved from the
// dashboard long after its Slack buttons expire, and the resolved message
// should still carry its summary and detail thread.
const messageContextTTL = 24 * time.Hour

// Notifier posts approval prompts to a user's Slack channel and turns button
// clicks back into notify.CallbackDecision values.
type Notifier struct {
	store  store.Store
	vault  vault.Vault // optional; when set, bot tokens are encrypted at rest
	client *http.Client
	// responseClient posts to the interaction response_url. It refuses
	// redirects: see newResponseClient.
	responseClient *http.Client
	logger         *slog.Logger

	signingSecret string
	creds         AppCredentials
	// apiBase is the Slack Web API root. Overridden in tests; there is no
	// production reason to change it.
	apiBase    string
	cbTokens   CallbackTokenStorer
	msgCtx     MessageContextStorer
	decisionCh chan notify.CallbackDecision
	replay     ReplayGuard
}

// New creates a Slack notifier. signingSecret verifies that interaction
// payloads really came from Slack; creds identify the Clawvisor Slack app
// during the OAuth install.
func New(st store.Store, signingSecret string, creds AppCredentials, logger *slog.Logger) *Notifier {
	return &Notifier{
		store:          st,
		client:         &http.Client{Timeout: 10 * time.Second},
		responseClient: newResponseClient(),
		logger:         logger,
		signingSecret:  signingSecret,
		creds:          creds,
		apiBase:        defaultAPIBase,
		cbTokens:       newCallbackTokenStore(),
		msgCtx:         newMessageContextStore(),
		decisionCh:     make(chan notify.CallbackDecision, 32),
		replay:         newMemoryReplayGuard(),
	}
}

// SetVault wires the credential vault in. Call before serving traffic.
func (n *Notifier) SetVault(v vault.Vault) { n.vault = v }

// SetRedisStores configures cross-instance stores. A Slack interaction can
// land on any replica, so both the callback tokens and the replay guard must
// be shared state in multi-instance deployments — the in-memory defaults
// would let a click on replica B fail to find a token minted on replica A.
func (n *Notifier) SetRedisStores(cbTokens CallbackTokenStorer, replay ReplayGuard) {
	if cbTokens != nil {
		n.cbTokens = cbTokens
	}
	if replay != nil {
		n.replay = replay
	}
}

// DecisionChannel exposes resolved button clicks to the server's decision
// consumer. Discovered by MultiNotifier via type assertion.
func (n *Notifier) DecisionChannel() <-chan notify.CallbackDecision { return n.decisionCh }

// RunCleanup evicts expired callback tokens until ctx is cancelled.
func (n *Notifier) RunCleanup(ctx context.Context) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.cbTokens.Cleanup()
			n.msgCtx.Cleanup()
		}
	}
}

// ── notify.Notifier implementation ───────────────────────────────────────────

func (n *Notifier) SendApprovalRequest(ctx context.Context, req notify.ApprovalRequest) (string, error) {
	cfg, err := n.userConfig(ctx, req.UserID)
	if err != nil {
		return "", err
	}

	detail := approvalBlocks(req)
	// The callback token carries the bare RequestID, not the composite
	// notification key: the decision consumer passes TargetID straight to
	// ApproveByRequestID, which looks up by request_id. TaskID travels
	// separately and is what disambiguates sibling approvals under
	// symmetric dedup. The composite below is only a notification_messages
	// key — conflating the two makes every task-scoped approval a no-op.
	blocks := append(append([]block{}, detail...), n.actionsFor(
		"approval", req.RequestID,
		req.UserID, req.TaskID, cfg.ChannelID, "Deny", req.ApproveURL, req.DenyURL,
	))

	ref, err := n.post(ctx, cfg, fallbackText("Approval request", req.AgentName), blocks)
	if err != nil {
		return "", fmt.Errorf("slack: send approval request: %w", err)
	}
	summary := summarise(req.AgentName, display.FormatServiceAction(req.Service, req.Action))
	n.recordMessage(ctx, "approval", approvalNotifyTargetID(req.RequestID, req.TaskID), ref, summary, detail)
	return ref, nil
}

func (n *Notifier) SendTaskApprovalRequest(ctx context.Context, req notify.TaskApprovalRequest) (string, error) {
	cfg, err := n.userConfig(ctx, req.UserID)
	if err != nil {
		return "", err
	}

	detail := taskApprovalBlocks(req)
	blocks := append(append([]block{}, detail...), n.actionsFor(
		"task", req.TaskID, req.UserID, "", cfg.ChannelID, "Deny", req.ApproveURL, req.DenyURL,
	))

	ref, err := n.post(ctx, cfg, fallbackText("Task approval request", req.AgentName), blocks)
	if err != nil {
		return "", fmt.Errorf("slack: send task approval request: %w", err)
	}
	n.recordMessage(ctx, "task", req.TaskID, ref, summarise(req.AgentName, req.Purpose), detail)
	return ref, nil
}

func (n *Notifier) SendScopeExpansionRequest(ctx context.Context, req notify.ScopeExpansionRequest) (string, error) {
	cfg, err := n.userConfig(ctx, req.UserID)
	if err != nil {
		return "", err
	}

	detail := scopeExpansionBlocks(req)
	blocks := append(append([]block{}, detail...), n.actionsFor(
		"scope_expansion", req.TaskID, req.UserID, "", cfg.ChannelID,
		"Deny expansion", req.ApproveURL, req.DenyURL,
	))

	ref, err := n.post(ctx, cfg, fallbackText("Scope expansion request", req.AgentName), blocks)
	if err != nil {
		return "", fmt.Errorf("slack: send scope expansion request: %w", err)
	}
	// Scope expansion resolves against the parent task, so it shares the
	// "task" target namespace with the original approval prompt.
	n.recordMessage(ctx, "task", req.TaskID, ref, summarise(req.AgentName, "scope expansion", req.Purpose), detail)
	return ref, nil
}

func (n *Notifier) SendConnectionRequest(ctx context.Context, req notify.ConnectionRequest) (string, error) {
	cfg, err := n.userConfig(ctx, req.UserID)
	if err != nil {
		return "", err
	}

	detail := connectionBlocks(req)
	blocks := append(append([]block{}, detail...), n.actionsFor(
		"connection", req.ConnectionID, req.UserID, "", cfg.ChannelID, "Deny", req.ApproveURL, req.DenyURL,
	))

	ref, err := n.post(ctx, cfg, fallbackText("Agent connection request", req.AgentName), blocks)
	if err != nil {
		return "", fmt.Errorf("slack: send connection request: %w", err)
	}
	n.recordMessage(ctx, "connection", req.ConnectionID, ref, summarise(req.AgentName, req.IPAddress), detail)
	return ref, nil
}

// SendActivationRequest posts a service-activation prompt. Activation is
// completed in the dashboard (it needs an OAuth round trip), so this carries
// link buttons rather than callback tokens.
func (n *Notifier) SendActivationRequest(ctx context.Context, req notify.ActivationRequest) error {
	cfg, err := n.userConfig(ctx, req.UserID)
	if err != nil {
		return err
	}
	svc := display.ServiceName(req.Service)
	blocks := []block{
		header("🔔 Service Activation Required"),
		section(fmt.Sprintf("*%s* wants to use *%s*, which isn't connected yet.", esc(req.AgentName), esc(svc))),
		linkActions(req.ActivateURL, req.DenyURL),
	}
	if _, err := n.post(ctx, cfg, fallbackText("Service activation required", req.AgentName), blocks); err != nil {
		return fmt.Errorf("slack: send activation request: %w", err)
	}
	return nil
}

func (n *Notifier) SendAlert(ctx context.Context, userID, text string) error {
	cfg, err := n.userConfig(ctx, userID)
	if err != nil {
		return err
	}
	_, err = n.post(ctx, cfg, text, []block{section(":bell: " + esc(text))})
	return err
}

func (n *Notifier) SendTestMessage(ctx context.Context, userID string) error {
	cfg, err := n.userConfig(ctx, userID)
	if err != nil {
		return err
	}
	_, err = n.post(ctx, cfg, "Clawvisor test message", []block{
		section(":white_check_mark: *Clawvisor is connected.* Approval requests will appear in this channel."),
	})
	return err
}

// SendSlackTestMessage sends a test message via Slack only.
// Implements notify.SlackTester.
func (n *Notifier) SendSlackTestMessage(ctx context.Context, userID string) error {
	return n.SendTestMessage(ctx, userID)
}

// UpdateMessage satisfies notify.Notifier but is intentionally inert.
//
// Call sites read the message ID from notification_messages with a hardcoded
// channel of "telegram" and MultiNotifier fans that one ID out to every
// notifier — so the ID arriving here addresses a Telegram message, not a
// Slack one. Editing Slack messages goes through UpdateMessageForTarget,
// which resolves this notifier's own message reference.
func (n *Notifier) UpdateMessage(_ context.Context, _, _, _ string) error { return nil }

// UpdateMessageForTarget rewrites the prompt for a resolved target, dropping
// its buttons so a settled request cannot be clicked again. Implements
// notify.TargetMessageUpdater.
func (n *Notifier) UpdateMessageForTarget(ctx context.Context, userID, targetType, targetID, text string) error {
	// Every early return below is a no-op by design — a resolve must not fail
	// because a chat message could not be edited — but they must not be
	// silent. A resolved request whose Slack prompt still shows live buttons
	// is exactly the symptom someone reports, and without these lines the
	// logs cannot distinguish "no message recorded" from "config unreadable".
	ref, err := n.store.GetNotificationMessage(ctx, targetType, targetID, notifyChannel)
	switch {
	case errors.Is(err, store.ErrNotFound):
		n.logger.WarnContext(ctx, "slack: no message recorded for target, cannot update prompt",
			"target_type", targetType, "target_id", targetID)
		return nil
	case err != nil:
		n.logger.WarnContext(ctx, "slack: message lookup failed, cannot update prompt",
			"err", err, "target_type", targetType, "target_id", targetID)
		return nil
	case ref == "":
		n.logger.WarnContext(ctx, "slack: empty message reference recorded for target",
			"target_type", targetType, "target_id", targetID)
		return nil
	}

	cfg, err := n.userConfig(ctx, userID)
	if err != nil {
		n.logger.WarnContext(ctx, "slack: config unavailable, cannot update prompt",
			"err", err, "user_id", userID, "target_type", targetType, "target_id", targetID)
		return nil
	}

	channelID, ts, ok := splitMessageRef(ref)
	if !ok {
		return fmt.Errorf("slack: malformed message reference %q", ref)
	}

	// Absent context (a replica that did not post the prompt, or an expired
	// entry) degrades to the outcome line alone rather than failing.
	mc, haveCtx := n.msgCtx.TakeForResolve(contextKey(targetType, targetID))
	if !haveCtx {
		// In-memory store: a decision consumed on a different replica than
		// the one that posted the prompt misses here. The outcome still gets
		// written, but without attribution or the detail thread — worth
		// seeing, because it looks like a partial failure to the user.
		n.logger.InfoContext(ctx, "slack: no message context for target, updating without attribution",
			"target_type", targetType, "target_id", targetID)
	}

	return n.update(ctx, cfg, channelID, ts, text, mc)
}

// ── Message plumbing ──────────────────────────────────────────────────────────

// actionsFor mints callback tokens and returns the button row, falling back
// to dashboard deep links when token generation fails so the prompt is still
// actionable.
func (n *Notifier) actionsFor(entryType, targetID, userID, taskID, channelID, denyLabel, approveURL, denyURL string) block {
	approveTok, denyTok, err := n.cbTokens.Generate(entryType, targetID, userID, taskID, channelID, callbackTTL)
	if err != nil {
		n.logger.Warn("slack: callback token generation failed, falling back to link buttons", "err", err)
		return linkActions(approveURL, denyURL)
	}
	return approveDenyActions(approveTok, denyTok, denyLabel)
}

// recordMessage stores the Slack message reference so the resolve path can
// edit it later. Failures are logged, not returned: the prompt is already
// delivered and losing the ability to edit it must not fail the send.
func (n *Notifier) recordMessage(ctx context.Context, targetType, targetID, ref string, summary string, detail []block) {
	if err := n.store.SaveNotificationMessage(ctx, targetType, targetID, notifyChannel, ref); err != nil {
		n.logger.WarnContext(ctx, "slack: could not record message reference",
			"err", err, "target_type", targetType, "target_id", targetID)
	}
	// Kept so the resolved message can still say what was approved and move
	// the detail into a thread instead of deleting it.
	n.msgCtx.Put(contextKey(targetType, targetID), messageContext{
		Summary: summary,
		Detail:  detail,
	}, messageContextTTL)
}

// messageRef packs the channel and timestamp Slack needs to address a
// message. chat.update requires both, but notification_messages stores a
// single opaque string per channel.
func messageRef(channelID, ts string) string { return channelID + ":" + ts }

func splitMessageRef(ref string) (channelID, ts string, ok bool) {
	i := strings.LastIndex(ref, ":")
	if i <= 0 || i == len(ref)-1 {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
}

// fallbackText is the notification/preview string shown where blocks cannot
// render (mobile push, notification centre, screen readers).
func fallbackText(kind, agent string) string {
	return fmt.Sprintf("%s from %s", kind, agent)
}

func (n *Notifier) post(ctx context.Context, cfg notify.SlackConfig, text string, blocks []block) (string, error) {
	// PendingChannel is not a channel — posting to it fails at Slack with
	// channel_not_found. The guard lives here rather than in userConfig
	// because the pending state is exactly when the channel picker, the
	// settings view and SlackSetChannel all need to read the config; making
	// userConfig reject it would break the flow that resolves it. Every
	// outbound message funnels through post, so no send path can miss this.
	if cfg.ChannelID == "" || cfg.ChannelID == PendingChannel {
		return "", ErrNoChannelConfigured
	}
	payload := map[string]any{
		"channel": cfg.ChannelID,
		"text":    text,
		"blocks":  clamp(blocks),
	}
	var out struct {
		TS      string `json:"ts"`
		Channel string `json:"channel"`
	}
	if err := n.call(ctx, cfg.BotToken, "chat.postMessage", payload, &out); err != nil {
		return "", err
	}
	ch := out.Channel
	if ch == "" {
		ch = cfg.ChannelID
	}
	return messageRef(ch, out.TS), nil
}

// update collapses a resolved prompt to its outcome. Blocks are replaced
// wholesale, which is also what clears the buttons; the detail has already
// been moved to a thread reply by the caller.
func (n *Notifier) update(ctx context.Context, cfg notify.SlackConfig, channelID, ts, text string, mc messageContext) error {
	// The incoming text is Telegram-flavoured HTML, so it needs translating
	// rather than escaping — escaping renders "<b>Approved</b>" literally.
	blocks := []block{section(telegramHTMLToMrkdwn(text))}

	// Attribution and summary go in a context line rather than the section,
	// so the outcome stays the visually dominant part of the message.
	if line := resolutionContext(mc); line != "" {
		blocks = append(blocks, contextBlock(line))
	}

	// The original request detail stays in this same message rather than
	// moving to a thread reply. A reply is a new message, so it notifies the
	// channel a second time — immediately after someone has just acted, which
	// is the moment they least want pinging. Editing in place notifies nobody,
	// and Slack collapses the result behind "Show more" on its own, which is
	// the collapsed-but-present behaviour the thread was reaching for.
	if len(mc.Detail) > 0 {
		blocks = append(blocks, divider())
		blocks = append(blocks, mc.Detail...)
	}

	payload := map[string]any{
		"channel": channelID,
		"ts":      ts,
		"text":    plainText(text),
		"blocks":  clamp(blocks),
	}
	return n.call(ctx, cfg.BotToken, "chat.update", payload, nil)
}

// resolutionContext renders the "by whom / of what" line under the outcome.
//
// The approver is plain text, deliberately not a `<@U…>` mention. A mention
// notifies that person, and the only time this renders is the instant after
// they pressed the button — so the mention's entire effect is to ping someone
// about something they just did themselves. It is also user-controlled text
// once it is a display name, so it must be escaped.
func resolutionContext(mc messageContext) string {
	var parts []string
	if mc.Approver != "" {
		parts = append(parts, "Resolved by "+escapeMrkdwn(mc.Approver))
	}
	if mc.Summary != "" {
		parts = append(parts, escapeMrkdwn(mc.Summary))
	}
	return strings.Join(parts, " · ")
}

// doJSON executes a prepared request and decodes the JSON body into out.
// Shared by the OAuth and lookup paths, which build their own requests.
func (n *Notifier) doJSON(req *http.Request, out any) error {
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("slack: malformed response: %w", err)
	}
	return nil
}

// call performs a Slack Web API request. Slack signals failure with HTTP 200
// and {"ok":false,"error":"..."}, so the status code alone proves nothing.
func (n *Notifier) call(ctx context.Context, botToken, method string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		n.apiBase+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+botToken)

	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var env struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("slack: %s: malformed response: %w", method, err)
	}
	if !env.OK {
		return fmt.Errorf("slack: %s: %s", method, env.Error)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("slack: %s: decode result: %w", method, err)
		}
	}
	return nil
}

// ── Config storage ────────────────────────────────────────────────────────────

// slackCfgJSON is the shape persisted in notification_configs.config. The bot
// token is deliberately absent — it lives in the vault.
type slackCfgJSON struct {
	BotToken             string                 `json:"bot_token,omitempty"` // legacy rows only
	TeamID               string                 `json:"team_id"`
	TeamName             string                 `json:"team_name"`
	ChannelID            string                 `json:"channel_id"`
	ChannelName          string                 `json:"channel_name"`
	InstallerSlackUserID string                 `json:"installer_slack_user_id"`
	Approvers            []notify.SlackApprover `json:"approvers"`
}

// SaveSlackConfig persists a workspace connection. Implements
// notify.SlackConfigStore.
func (n *Notifier) SaveSlackConfig(ctx context.Context, userID string, cfg notify.SlackConfig) error {
	if userID == "" {
		return errors.New("slack: user_id is required")
	}
	if cfg.BotToken == "" {
		return errors.New("slack: bot_token is required")
	}
	if cfg.ChannelID == "" {
		return errors.New("slack: channel_id is required")
	}

	jsonToken := cfg.BotToken
	// prior is the token this write is about to overwrite, captured before
	// the overwrite so a failed config upsert can put it back. A missing or
	// unreadable entry leaves it nil, which the rollback reads as "there was
	// nothing here" and deletes instead of restoring.
	var prior []byte
	if n.vault != nil {
		b, gerr := n.vault.Get(ctx, userID, vaultBotTokenKey)
		switch {
		case gerr == nil:
			prior = b
		case errors.Is(gerr, vault.ErrNotFound):
			// Genuinely absent: a rollback should delete, not restore.
		default:
			// Transient or decryption failure. We cannot tell an absent
			// token from an unreadable one, and guessing "absent" is
			// unsafe: a later upsert failure would roll back by DELETING a
			// token the surviving config still references, bricking a
			// working connection. Refuse before overwriting anything —
			// nothing has changed yet, so the caller can simply retry.
			return fmt.Errorf("slack: cannot read existing bot_token, refusing to overwrite it: %w", gerr)
		}
		if err := n.vault.Set(ctx, userID, vaultBotTokenKey, []byte(cfg.BotToken)); err != nil {
			return fmt.Errorf("slack: persist bot_token: %w", err)
		}
		jsonToken = "" // never duplicate the secret into the JSON column
	}

	cfgBytes, err := json.Marshal(slackCfgJSON{
		BotToken:             jsonToken,
		TeamID:               cfg.TeamID,
		TeamName:             cfg.TeamName,
		ChannelID:            cfg.ChannelID,
		ChannelName:          cfg.ChannelName,
		InstallerSlackUserID: cfg.InstallerSlackUserID,
		Approvers:            cfg.Approvers,
	})
	if err != nil {
		return fmt.Errorf("slack: marshal config: %w", err)
	}

	if err := n.store.UpsertNotificationConfig(ctx, userID, notifyChannel, cfgBytes); err != nil {
		n.rollbackBotToken(ctx, userID, prior)
		return fmt.Errorf("slack: save notification config: %w", err)
	}
	return nil
}

// rollbackBotToken undoes the vault write of a SaveSlackConfig whose config
// upsert then failed.
//
// Deleting unconditionally — the old behaviour — was only correct for a first
// install. On an update (a re-install, a channel change, an approver edit) the
// previous config row survives the failed upsert and still references the
// vault key, so deleting it left that row permanently unusable: userConfig
// finds no token and every send fails with "no bot token configured". A
// non-empty prior value is therefore restored; only a genuinely absent one is
// deleted, so a first install still leaves no unreferenced secret behind.
func (n *Notifier) rollbackBotToken(ctx context.Context, userID string, prior []byte) {
	if n.vault == nil {
		return
	}
	var err error
	if len(prior) > 0 {
		err = n.vault.Set(ctx, userID, vaultBotTokenKey, prior)
	} else {
		err = n.vault.Delete(ctx, userID, vaultBotTokenKey)
	}
	if err != nil {
		// Nothing left to try: the config write already failed, and failing
		// louder cannot repair the vault. Log so the inconsistency is
		// recoverable by hand.
		n.logger.ErrorContext(ctx, "slack: could not roll back bot token after failed config save",
			"err", err, "user_id", userID, "vault_key", vaultBotTokenKey, "restored", len(prior) > 0)
	}
}

// SlackConfig returns the user's workspace configuration, bot token included.
// Implements notify.SlackConfigStore.
func (n *Notifier) SlackConfig(ctx context.Context, userID string) (notify.SlackConfig, error) {
	return n.userConfig(ctx, userID)
}

// DeleteSlackConfig removes the workspace connection and its vaulted token.
// Implements notify.SlackConfigStore.
func (n *Notifier) DeleteSlackConfig(ctx context.Context, userID string) error {
	// The config row goes first, deliberately. It is the only reference that
	// makes the token reachable, so dropping it is the half of a disconnect
	// that actually revokes access. The old vault-first order inverted the
	// failure mode: a store error after a successful vault delete left a
	// config row pointing at nothing, so the user still looked connected but
	// every send failed and reconnecting was the only repair.
	if err := n.store.DeleteNotificationConfig(ctx, userID, notifyChannel); err != nil {
		return err
	}
	if n.vault == nil {
		return nil
	}
	if err := n.vault.Delete(ctx, userID, vaultBotTokenKey); err != nil {
		// Reported as success on purpose. The disconnect has already taken
		// effect — nothing can load this token again — so returning an error
		// would tell the user they are still connected when they are not, and
		// a retry could never clear it because the row it keys off is gone.
		// Completing quietly is what the old `_ =` did; the difference is
		// that this is now the only record that an encrypted blob is left in
		// the vault, so it is logged at error level with the exact
		// (user, key) pair an operator needs to sweep it.
		n.logger.ErrorContext(ctx, "slack: orphaned bot token left in vault after disconnect",
			"err", err, "user_id", userID, "vault_key", vaultBotTokenKey)
	}
	return nil
}

func (n *Notifier) userConfig(ctx context.Context, userID string) (notify.SlackConfig, error) {
	rec, err := n.store.GetNotificationConfig(ctx, userID, notifyChannel)
	if err != nil {
		return notify.SlackConfig{}, err
	}
	var raw slackCfgJSON
	if err := json.Unmarshal(rec.Config, &raw); err != nil {
		return notify.SlackConfig{}, fmt.Errorf("slack: parse config: %w", err)
	}

	token := raw.BotToken // legacy plaintext rows
	if n.vault != nil {
		if b, verr := n.vault.Get(ctx, userID, vaultBotTokenKey); verr == nil && len(b) > 0 {
			token = string(b)
		}
	}
	if token == "" {
		return notify.SlackConfig{}, errors.New("slack: no bot token configured")
	}

	return notify.SlackConfig{
		BotToken:             token,
		TeamID:               raw.TeamID,
		TeamName:             raw.TeamName,
		ChannelID:            raw.ChannelID,
		ChannelName:          raw.ChannelName,
		InstallerSlackUserID: raw.InstallerSlackUserID,
		Approvers:            raw.Approvers,
	}, nil
}

// approvalNotifyTargetID mirrors the gateway handler's composition so both
// channels address the same notification_messages row.
func approvalNotifyTargetID(requestID, taskID string) string {
	if taskID == "" {
		return requestID
	}
	return requestID + "|" + taskID
}

// Compile-time interface checks.
var (
	_ notify.Notifier             = (*Notifier)(nil)
	_ notify.SlackConfigStore     = (*Notifier)(nil)
	_ notify.TargetMessageUpdater = (*Notifier)(nil)
)
