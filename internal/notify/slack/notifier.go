// Package slack implements notify.Notifier using the Slack Web API.
package slack

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/clawvisor/clawvisor/internal/display"
	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store"
	"github.com/clawvisor/clawvisor/pkg/vault"
)

const (
	defaultAPIBase        = "https://slack.com/api"
	vaultBotTokenKey     = "notify.slack.bot_token"
	vaultSigningSecretKey = "notify.slack.signing_secret"
	maxInteractionBytes   = 128 << 10
	maxSlackResponseBytes = 1 << 20
)

type Notifier struct {
	store      store.Store
	vault      vault.Vault
	client     *http.Client
	apiBase    string
	cbTokens   CallbackTokenStore
	decisionCh chan notify.CallbackDecision
	logger     *slog.Logger
}

func New(st store.Store, logger *slog.Logger) *Notifier {
	apiBase := strings.TrimRight(os.Getenv("SLACK_API_BASE_URL"), "/")
	if apiBase == "" {
		apiBase = defaultAPIBase
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Notifier{
		store:      st,
		client:     &http.Client{Timeout: 10 * time.Second},
		apiBase:    apiBase,
		cbTokens:   newCallbackTokenStore(),
		decisionCh: make(chan notify.CallbackDecision, 32),
		logger:     logger,
	}
}

func (n *Notifier) NotificationChannel() string { return "slack" }

func (n *Notifier) SetVault(v vault.Vault) {
	n.vault = v
}

func (n *Notifier) SetCallbackTokenStore(store CallbackTokenStore) {
	if store != nil {
		n.cbTokens = store
	}
}

func (n *Notifier) DecisionChannel() <-chan notify.CallbackDecision {
	return n.decisionCh
}

func (n *Notifier) RunCleanup(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n.cbTokens.Cleanup()
		}
	}
}

func (n *Notifier) SendApprovalRequest(ctx context.Context, req notify.ApprovalRequest) (string, error) {
	cfg, err := n.userConfig(ctx, req.UserID)
	if err != nil {
		return "", err
	}
	text := formatApprovalMessage(req)
	blocks := approvalBlocks("Clawvisor Approval Request", text, cfg, req.UserID, "approval", req.RequestID, req.TaskID, req.ApproveURL, req.DenyURL)
	blocks = n.withInteractiveTokens(blocks, cfg, "approval", req.RequestID, req.UserID, req.TaskID, req.ApproveURL, req.DenyURL)
	return n.postMessage(ctx, cfg.BotToken, cfg.ChannelID, text, blocks)
}

func (n *Notifier) SendActivationRequest(ctx context.Context, req notify.ActivationRequest) error {
	cfg, err := n.userConfig(ctx, req.UserID)
	if err != nil {
		return err
	}
	svcName := display.ServiceName(req.Service)
	text := fmt.Sprintf("*Clawvisor - Service Activation Required*\n\n*Agent:* %s\n*Service:* %s", mrkdwn(req.AgentName), mrkdwn(svcName))
	blocks := []any{
		section(text),
		actions(urlButton("Activate "+svcName, req.ActivateURL), urlButton("Deny", req.DenyURL)),
	}
	_, err = n.postMessage(ctx, cfg.BotToken, cfg.ChannelID, text, blocks)
	return err
}

func (n *Notifier) SendTaskApprovalRequest(ctx context.Context, req notify.TaskApprovalRequest) (string, error) {
	cfg, err := n.userConfig(ctx, req.UserID)
	if err != nil {
		return "", err
	}
	text := formatTaskApprovalMessage(req)
	blocks := approvalBlocks("Clawvisor Task Approval Request", text, cfg, req.UserID, "task", req.TaskID, "", req.ApproveURL, req.DenyURL)
	blocks = n.withInteractiveTokens(blocks, cfg, "task", req.TaskID, req.UserID, "", req.ApproveURL, req.DenyURL)
	return n.postMessage(ctx, cfg.BotToken, cfg.ChannelID, text, blocks)
}

func (n *Notifier) SendScopeExpansionRequest(ctx context.Context, req notify.ScopeExpansionRequest) (string, error) {
	cfg, err := n.userConfig(ctx, req.UserID)
	if err != nil {
		return "", err
	}
	text := formatScopeExpansionMessage(req)
	blocks := approvalBlocks("Clawvisor Scope Expansion Request", text, cfg, req.UserID, "scope_expansion", req.TaskID, "", req.ApproveURL, req.DenyURL)
	blocks = n.withInteractiveTokens(blocks, cfg, "scope_expansion", req.TaskID, req.UserID, "", req.ApproveURL, req.DenyURL)
	return n.postMessage(ctx, cfg.BotToken, cfg.ChannelID, text, blocks)
}

func (n *Notifier) SendConnectionRequest(ctx context.Context, req notify.ConnectionRequest) (string, error) {
	cfg, err := n.userConfig(ctx, req.UserID)
	if err != nil {
		return "", err
	}
	text := fmt.Sprintf("*Clawvisor Connection Request*\n\n*Agent:* %s\n*IP address:* `%s`", mrkdwn(req.AgentName), mrkdwn(req.IPAddress))
	blocks := approvalBlocks("Clawvisor Connection Request", text, cfg, req.UserID, "connection", req.ConnectionID, "", req.ApproveURL, req.DenyURL)
	blocks = n.withInteractiveTokens(blocks, cfg, "connection", req.ConnectionID, req.UserID, "", req.ApproveURL, req.DenyURL)
	return n.postMessage(ctx, cfg.BotToken, cfg.ChannelID, text, blocks)
}

func (n *Notifier) UpdateMessage(ctx context.Context, userID, messageID, text string) error {
	cfg, err := n.userConfig(ctx, userID)
	if err != nil {
		return err
	}
	channelID, ts, ok := strings.Cut(messageID, ":")
	if !ok || channelID == "" || ts == "" {
		return fmt.Errorf("slack: invalid message id %q", messageID)
	}
	blocks := []any{section(mrkdwn(text))}
	return n.chatUpdate(ctx, cfg.BotToken, channelID, ts, text, blocks)
}

func (n *Notifier) SendTestMessage(ctx context.Context, userID string) error {
	return n.SendSlackTestMessage(ctx, userID)
}

func (n *Notifier) SendSlackTestMessage(ctx context.Context, userID string) error {
	cfg, err := n.userConfig(ctx, userID)
	if err != nil {
		return err
	}
	_, err = n.postMessage(ctx, cfg.BotToken, cfg.ChannelID, "Clawvisor test message: Slack notifications are working.", nil)
	return err
}

func (n *Notifier) SendAlert(ctx context.Context, userID, text string) error {
	cfg, err := n.userConfig(ctx, userID)
	if err != nil {
		return err
	}
	_, err = n.postMessage(ctx, cfg.BotToken, cfg.ChannelID, text, nil)
	return err
}

func (n *Notifier) HandleInteraction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxInteractionBytes+1))
	if err != nil || len(body) > maxInteractionBytes {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	var payload interactionPayload
	if err := json.Unmarshal([]byte(form.Get("payload")), &payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if len(payload.Actions) == 0 {
		http.Error(w, "missing action", http.StatusBadRequest)
		return
	}
	action := payload.Actions[0]
	var value callbackValue
	if err := json.Unmarshal([]byte(action.Value), &value); err != nil || value.Token == "" || value.UserID == "" {
		http.Error(w, "invalid action value", http.StatusBadRequest)
		return
	}
	cfg, err := n.userConfig(r.Context(), value.UserID)
	if err != nil || cfg.SigningSecret == "" {
		http.Error(w, "slack signing secret not configured", http.StatusUnauthorized)
		return
	}
	if !verifySlackSignature(cfg.SigningSecret, r.Header.Get("X-Slack-Request-Timestamp"), r.Header.Get("X-Slack-Signature"), body) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	entry, err := n.cbTokens.Consume(value.Token)
	if err != nil {
		http.Error(w, "expired or used action", http.StatusConflict)
		return
	}
	if entry.UserID != value.UserID {
		http.Error(w, "invalid action target", http.StatusForbidden)
		return
	}
	decisionAction := "deny"
	if strings.HasPrefix(action.ActionID, "approve") {
		decisionAction = "approve"
	}
	select {
	case n.decisionCh <- notify.CallbackDecision{
		Type: entry.Type, Action: decisionAction, TargetID: entry.TargetID,
		TaskID: entry.TaskID, UserID: entry.UserID,
	}:
	default:
		n.logger.Warn("slack: decision channel full, dropping", "type", entry.Type, "target_id", entry.TargetID)
	}
	writeSlackJSON(w, map[string]string{"text": "Decision recorded."})
}

func (n *Notifier) postMessage(ctx context.Context, token, channelID, text string, blocks []any) (string, error) {
	var resp struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}
	payload := map[string]any{
		"channel": channelID,
		"text":    text,
	}
	if blocks != nil {
		payload["blocks"] = blocks
	}
	if err := n.slackPost(ctx, token, "/chat.postMessage", payload, &resp); err != nil {
		return "", err
	}
	if !resp.OK {
		return "", fmt.Errorf("slack chat.postMessage: %s", resp.Error)
	}
	if resp.Channel == "" {
		resp.Channel = channelID
	}
	if resp.TS == "" {
		return "", errors.New("slack chat.postMessage: missing ts")
	}
	return resp.Channel + ":" + resp.TS, nil
}

func (n *Notifier) chatUpdate(ctx context.Context, token, channelID, ts, text string, blocks []any) error {
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := n.slackPost(ctx, token, "/chat.update", map[string]any{
		"channel": channelID,
		"ts":      ts,
		"text":    text,
		"blocks":  blocks,
	}, &resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("slack chat.update: %s", resp.Error)
	}
	return nil
}

func (n *Notifier) slackPost(ctx context.Context, token, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.apiBase+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("slack %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("slack %s: status %d", path, resp.StatusCode)
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxSlackResponseBytes))
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("slack %s: parse response: %w", path, err)
		}
	}
	return nil
}

func approvalBlocks(title, text string, cfg notify.SlackConfig, userID, entryType, targetID, taskID, approveURL, denyURL string) []any {
	headerText := title
	if strings.EqualFold(cfg.Mode, "openclaw_agent") {
		headerText = title + " via Slack Agent"
	}
	buttons := []any{urlButton("Approve", approveURL), urlButton("Deny", denyURL)}
	if cfg.SigningSecret != "" && !strings.EqualFold(cfg.Mode, "openclaw_agent") {
		buttons = interactiveButtons(userID, entryType, targetID, taskID)
	}
	return []any{
		header(headerText),
		section(text),
		actions(buttons...),
	}
}

func interactiveButtons(userID, entryType, targetID, taskID string) []any {
	// Filled by withInteractiveTokens immediately before sending.
	return []any{
		map[string]any{"type": "button", "style": "primary", "text": plainText("Approve"), "action_id": "approve", "value": mustJSON(callbackValue{UserID: userID, Type: entryType, TargetID: targetID, TaskID: taskID})},
		map[string]any{"type": "button", "style": "danger", "text": plainText("Deny"), "action_id": "deny", "value": mustJSON(callbackValue{UserID: userID, Type: entryType, TargetID: targetID, TaskID: taskID})},
	}
}

func (n *Notifier) withInteractiveTokens(blocks []any, cfg notify.SlackConfig, entryType, targetID, userID, taskID, approveURL, denyURL string) []any {
	if cfg.SigningSecret == "" || strings.EqualFold(cfg.Mode, "openclaw_agent") {
		return blocks
	}
	approveID, denyID, err := n.cbTokens.Generate(entryType, targetID, userID, taskID, cfg.ChannelID, 6*time.Minute)
	if err != nil {
		replaceActionElements(blocks, urlButton("Approve", approveURL), urlButton("Deny", denyURL))
		return blocks
	}
	replaceActionElements(blocks,
		map[string]any{"type": "button", "style": "primary", "text": plainText("Approve"), "action_id": "approve", "value": mustJSON(callbackValue{Token: approveID, UserID: userID})},
		map[string]any{"type": "button", "style": "danger", "text": plainText("Deny"), "action_id": "deny", "value": mustJSON(callbackValue{Token: denyID, UserID: userID})},
	)
	return blocks
}

func replaceActionElements(blocks []any, approve, deny any) {
	for _, block := range blocks {
		b, ok := block.(map[string]any)
		if !ok || b["type"] != "actions" {
			continue
		}
		b["elements"] = []any{approve, deny}
		return
	}
}

func (n *Notifier) userConfig(ctx context.Context, userID string) (notify.SlackConfig, error) {
	nc, err := n.store.GetNotificationConfig(ctx, userID, "slack")
	if errors.Is(err, store.ErrNotFound) {
		return notify.SlackConfig{}, fmt.Errorf("slack: user %s has no Slack notification configured", userID)
	}
	if err != nil {
		return notify.SlackConfig{}, fmt.Errorf("slack: fetching config for user %s: %w", userID, err)
	}
	var cfg notify.SlackConfig
	if err := json.Unmarshal(nc.Config, &cfg); err != nil {
		return notify.SlackConfig{}, fmt.Errorf("slack: invalid config for user %s: %w", userID, err)
	}
	if cfg.BotToken == "" && n.vault != nil {
		if data, vErr := n.vault.Get(ctx, userID, vaultBotTokenKey); vErr == nil {
			cfg.BotToken = string(data)
		}
	}
	if cfg.SigningSecret == "" && n.vault != nil {
		if data, vErr := n.vault.Get(ctx, userID, vaultSigningSecretKey); vErr == nil {
			cfg.SigningSecret = string(data)
		}
	}
	if cfg.Mode == "" {
		cfg.Mode = "direct"
	}
	if cfg.BotToken == "" {
		return notify.SlackConfig{}, fmt.Errorf("slack: user %s config missing bot_token", userID)
	}
	if cfg.ChannelID == "" {
		return notify.SlackConfig{}, fmt.Errorf("slack: user %s config missing channel_id", userID)
	}
	return cfg, nil
}

func (n *Notifier) SaveSlackConfig(ctx context.Context, userID string, cfg notify.SlackConfig) error {
	cfg.ChannelID = strings.TrimSpace(cfg.ChannelID)
	cfg.BotToken = strings.TrimSpace(cfg.BotToken)
	cfg.SigningSecret = strings.TrimSpace(cfg.SigningSecret)
	cfg.Mode = strings.ToLower(strings.TrimSpace(cfg.Mode))
	if cfg.Mode == "" {
		cfg.Mode = "direct"
	}
	if cfg.Mode != "direct" && cfg.Mode != "openclaw_agent" {
		return fmt.Errorf("slack: mode must be direct or openclaw_agent")
	}
	if userID == "" || cfg.BotToken == "" || cfg.ChannelID == "" {
		return fmt.Errorf("slack: user_id, bot_token, and channel_id are required")
	}
	jsonToken := cfg.BotToken
	jsonSecret := cfg.SigningSecret
	if n.vault != nil {
		if err := n.vault.Set(ctx, userID, vaultBotTokenKey, []byte(cfg.BotToken)); err != nil {
			return fmt.Errorf("slack: persist bot_token: %w", err)
		}
		jsonToken = ""
		if cfg.SigningSecret != "" {
			if err := n.vault.Set(ctx, userID, vaultSigningSecretKey, []byte(cfg.SigningSecret)); err != nil {
				_ = n.vault.Delete(ctx, userID, vaultBotTokenKey)
				return fmt.Errorf("slack: persist signing_secret: %w", err)
			}
		} else {
			_ = n.vault.Delete(ctx, userID, vaultSigningSecretKey)
		}
		jsonSecret = ""
	}
	cfgBytes, err := json.Marshal(notify.SlackConfig{
		BotToken: jsonToken, ChannelID: cfg.ChannelID, SigningSecret: jsonSecret, Mode: cfg.Mode,
	})
	if err != nil {
		return fmt.Errorf("slack: marshal config: %w", err)
	}
	if err := n.store.UpsertNotificationConfig(ctx, userID, "slack", cfgBytes); err != nil {
		if n.vault != nil {
			_ = n.vault.Delete(ctx, userID, vaultBotTokenKey)
			_ = n.vault.Delete(ctx, userID, vaultSigningSecretKey)
		}
		return fmt.Errorf("slack: save notification config: %w", err)
	}
	return nil
}

func (n *Notifier) SlackConfig(ctx context.Context, userID string) (notify.SlackConfig, error) {
	return n.userConfig(ctx, userID)
}

func (n *Notifier) DeleteSlackConfig(ctx context.Context, userID string) error {
	if n.vault != nil {
		_ = n.vault.Delete(ctx, userID, vaultBotTokenKey)
		_ = n.vault.Delete(ctx, userID, vaultSigningSecretKey)
	}
	return n.store.DeleteNotificationConfig(ctx, userID, "slack")
}

func formatApprovalMessage(req notify.ApprovalRequest) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("*Agent:* %s\n*Service:* %s\n*Action:* %s", mrkdwn(req.AgentName), mrkdwn(display.ServiceName(req.Service)), mrkdwn(display.ActionName(req.Action))))
	if len(req.Params) > 0 {
		sb.WriteString("\n\n*Parameters:*\n")
		keys := make([]string, 0, len(req.Params))
		for k := range req.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sb.WriteString(fmt.Sprintf("- `%s`: %s\n", mrkdwn(k), mrkdwn(paramValue(req.Params[k]))))
		}
	}
	if req.Reason != "" {
		sb.WriteString(fmt.Sprintf("\n*Agent's stated reason:* %s", mrkdwn(req.Reason)))
	}
	if req.PolicyReason != "" {
		sb.WriteString(fmt.Sprintf("\n*Policy:* %s", mrkdwn(req.PolicyReason)))
	}
	if req.ExpiresIn != "" {
		sb.WriteString(fmt.Sprintf("\n_Expires in %s_", mrkdwn(req.ExpiresIn)))
	}
	return sb.String()
}

func formatTaskApprovalMessage(req notify.TaskApprovalRequest) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("*Agent:* %s\n*Purpose:* %s", mrkdwn(req.AgentName), mrkdwn(req.Purpose)))
	if req.RiskLevel != "" && req.RiskLevel != "unknown" {
		sb.WriteString(fmt.Sprintf("\n*Risk:* %s", mrkdwn(req.RiskLevel)))
	}
	if len(req.ScopeSummary) > 0 {
		sb.WriteString("\n\n*Requested scope:*\n")
		for _, line := range req.ScopeSummary {
			sb.WriteString("- " + mrkdwn(line) + "\n")
		}
	}
	if req.ExpiresIn != "" {
		sb.WriteString(fmt.Sprintf("\n_Expires in %s_", mrkdwn(req.ExpiresIn)))
	}
	return sb.String()
}

func formatScopeExpansionMessage(req notify.ScopeExpansionRequest) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("*Agent:* %s\n*Task:* %s\n*Reason:* %s", mrkdwn(req.AgentName), mrkdwn(req.Purpose), mrkdwn(req.Reason)))
	if req.RiskLevel != "" {
		sb.WriteString(fmt.Sprintf("\n*Risk:* %s", mrkdwn(req.RiskLevel)))
	}
	if summary := notify.RenderExpansionSummary(req); summary != "" {
		sb.WriteString("\n\n*Scope changes:*\n" + mrkdwn(summary))
	}
	if req.Lifetime != "" {
		sb.WriteString(fmt.Sprintf("\n*Lifetime:* %s", mrkdwn(req.Lifetime)))
	}
	return sb.String()
}

func section(text string) map[string]any {
	return map[string]any{"type": "section", "text": map[string]string{"type": "mrkdwn", "text": text}}
}

func header(text string) map[string]any {
	return map[string]any{"type": "header", "text": plainText(text)}
}

func actions(elements ...any) map[string]any {
	return map[string]any{"type": "actions", "elements": elements}
}

func urlButton(text, link string) map[string]any {
	return map[string]any{"type": "button", "text": plainText(text), "url": link}
}

func plainText(text string) map[string]string {
	return map[string]string{"type": "plain_text", "text": text}
}

func mrkdwn(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	if len(s) > 600 {
		s = s[:597] + "..."
	}
	return s
}

func paramValue(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	s := string(b)
	if len(s) > 400 {
		return s[:397] + "..."
	}
	return s
}

type callbackValue struct {
	Token    string `json:"token,omitempty"`
	UserID   string `json:"user_id"`
	Type     string `json:"type,omitempty"`
	TargetID string `json:"target_id,omitempty"`
	TaskID   string `json:"task_id,omitempty"`
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

type interactionPayload struct {
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
}

func verifySlackSignature(secret, ts, sig string, body []byte) bool {
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || unix <= 0 {
		return false
	}
	if d := time.Since(time.Unix(unix, 0)); d > 5*time.Minute || d < -5*time.Minute {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + ts + ":"))
	mac.Write(body)
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(sig))
}

func writeSlackJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

var (
	_ notify.Notifier         = (*Notifier)(nil)
	_ notify.SlackConfigStore = (*Notifier)(nil)
)
