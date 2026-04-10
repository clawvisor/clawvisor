// Package telegram implements notify.Notifier using the Telegram Bot API.
// Each user stores their own bot_token and chat_id in notification_configs
// (channel = "telegram", config = {"bot_token":"...", "chat_id":"..."}).
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/clawvisor/clawvisor/internal/display"
	"github.com/clawvisor/clawvisor/internal/groupchat"
	"github.com/clawvisor/clawvisor/pkg/notify"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// Notifier sends Telegram messages for approval and service-activation requests.
type Notifier struct {
	store         store.Store
	client        *http.Client
	pairingClient *http.Client // longer timeout for long-poll getUpdates
	pairings      sync.Map     // pairing ID → *pairingSession

	cbTokens      CallbackTokenStorer
	pollers       sync.Map // userID → *pollingSession
	decisionCh    chan notify.CallbackDecision
	serverCtx     context.Context
	msgBuffer     groupchat.Buffer // may be nil; set via SetMessageBuffer
	pendingGroups sync.Map                // userID → *pendingGroupList (in-memory fallback)
	groupPairings sync.Map                // sessionID → *groupPairingSession (in-memory fallback)

	// Redis-backed stores for multi-instance mode. When nil, in-memory fallbacks are used.
	pendingGroupStore PendingGroupStore
	groupPairingStore GroupPairingStore
	pollingLock       PollingLock
}

// New creates a Telegram Notifier that reads per-user bot tokens from the store.
// serverCtx is the server's top-level context; polling goroutines respect its cancellation.
func New(st store.Store, serverCtx context.Context) *Notifier {
	n := &Notifier{
		store:         st,
		client:        &http.Client{Timeout: 10 * time.Second},
		pairingClient: &http.Client{Timeout: 30 * time.Second},
		cbTokens:      newCallbackTokenStore(),
		decisionCh:    make(chan notify.CallbackDecision, 32),
		serverCtx:     serverCtx,
		pollingLock:   noopPollingLock{},
	}
	return n
}

// SetMessageBuffer sets the message buffer used for group chat observation.
func (n *Notifier) SetMessageBuffer(buf groupchat.Buffer) {
	n.msgBuffer = buf
}

// SetRedisStores configures Redis-backed stores for multi-instance deployments.
// Call before the server starts handling requests.
func (n *Notifier) SetRedisStores(cbTokens CallbackTokenStorer, pgStore PendingGroupStore, gpStore GroupPairingStore, pollLock PollingLock) {
	if cbTokens != nil {
		n.cbTokens = cbTokens
	}
	if pgStore != nil {
		n.pendingGroupStore = pgStore
	}
	if gpStore != nil {
		n.groupPairingStore = gpStore
	}
	if pollLock != nil {
		n.pollingLock = pollLock
	}
}

// ── Agent-to-group pairing ────────────────────────────────────────────────────

type groupPairingSession struct {
	ID          string
	UserID      string
	GroupChatID string
	CreatedAt   time.Time
}

// StartGroupPairing creates a pairing session and returns the session ID.
// The caller is responsible for surfacing the pairing URL to the user (e.g.
// in the dashboard) so they can share it with their agent.
func (n *Notifier) StartGroupPairing(_ context.Context, userID, groupChatID, _ string) (string, error) {
	sessionID := generatePairingID()
	session := &groupPairingSession{
		ID:          sessionID,
		UserID:      userID,
		GroupChatID: groupChatID,
		CreatedAt:   time.Now(),
	}
	if n.groupPairingStore != nil {
		n.groupPairingStore.Store(sessionID, session)
	} else {
		n.groupPairings.Store(sessionID, session)
		go func() {
			time.Sleep(5 * time.Minute)
			n.groupPairings.Delete(sessionID)
		}()
	}

	return sessionID, nil
}

// CompleteGroupPairing validates a pairing session and persists the agent-group mapping.
func (n *Notifier) CompleteGroupPairing(ctx context.Context, sessionID, agentID, agentUserID string) error {
	var session *groupPairingSession
	if n.groupPairingStore != nil {
		s, ok := n.groupPairingStore.Load(sessionID)
		if !ok {
			return fmt.Errorf("pairing session not found or expired")
		}
		session = s
	} else {
		val, ok := n.groupPairings.Load(sessionID)
		if !ok {
			return fmt.Errorf("pairing session not found or expired")
		}
		session = val.(*groupPairingSession)
	}

	if time.Since(session.CreatedAt) > 5*time.Minute {
		if n.groupPairingStore != nil {
			n.groupPairingStore.Delete(sessionID)
		} else {
			n.groupPairings.Delete(sessionID)
		}
		return fmt.Errorf("pairing session expired")
	}
	if agentUserID != session.UserID {
		return fmt.Errorf("agent does not belong to the user who created this pairing")
	}

	if err := n.store.CreateAgentGroupPairing(ctx, session.UserID, agentID, session.GroupChatID); err != nil {
		return fmt.Errorf("persisting agent-group pairing: %w", err)
	}
	if n.groupPairingStore != nil {
		n.groupPairingStore.Delete(sessionID)
	} else {
		n.groupPairings.Delete(sessionID)
	}
	return nil
}

// AgentGroupChatID returns the group chat ID paired with the given agent.
func (n *Notifier) AgentGroupChatID(ctx context.Context, agentID string) (string, error) {
	return n.store.GetAgentGroupChatID(ctx, agentID)
}

// PairedAgentIDs returns the IDs of all agents paired to the given group chat.
func (n *Notifier) PairedAgentIDs(ctx context.Context, groupChatID string) ([]string, error) {
	return n.store.ListAgentIDsByGroup(ctx, groupChatID)
}

// UnpairAgentsForGroup removes all agent pairings for the given group chat ID.
func (n *Notifier) UnpairAgentsForGroup(ctx context.Context, groupChatID string) error {
	return n.store.DeleteAgentGroupPairingsByGroup(ctx, groupChatID)
}

// pendingGroupList is a mutex-protected list of pending groups for a user.
type pendingGroupList struct {
	mu     sync.Mutex
	groups []notify.PendingGroup
}

// AddPendingGroup stores a detected group for the user, deduplicating by ChatID.
func (n *Notifier) AddPendingGroup(userID string, pg notify.PendingGroup) {
	if n.pendingGroupStore != nil {
		n.pendingGroupStore.Add(userID, pg)
		return
	}
	n.addPendingGroupMemory(userID, pg)
}

func (n *Notifier) addPendingGroupMemory(userID string, pg notify.PendingGroup) {
	val, _ := n.pendingGroups.LoadOrStore(userID, &pendingGroupList{})
	pgl := val.(*pendingGroupList)
	pgl.mu.Lock()
	defer pgl.mu.Unlock()
	for _, g := range pgl.groups {
		if g.ChatID == pg.ChatID {
			return // already known
		}
	}
	pgl.groups = append(pgl.groups, pg)
}

// PendingGroups returns the list of groups the bot has been added to but
// observation has not been enabled for.
func (n *Notifier) PendingGroups(userID string) []notify.PendingGroup {
	if n.pendingGroupStore != nil {
		return n.pendingGroupStore.List(userID)
	}
	return n.pendingGroupsMemory(userID)
}

func (n *Notifier) pendingGroupsMemory(userID string) []notify.PendingGroup {
	val, ok := n.pendingGroups.Load(userID)
	if !ok {
		return nil
	}
	pgl := val.(*pendingGroupList)
	pgl.mu.Lock()
	defer pgl.mu.Unlock()
	result := make([]notify.PendingGroup, len(pgl.groups))
	copy(result, pgl.groups)
	return result
}

// RemovePendingGroup removes a group from the pending list (after approval or dismissal).
func (n *Notifier) RemovePendingGroup(userID, chatID string) {
	if n.pendingGroupStore != nil {
		n.pendingGroupStore.Remove(userID, chatID)
		return
	}
	n.removePendingGroupMemory(userID, chatID)
}

func (n *Notifier) removePendingGroupMemory(userID, chatID string) {
	val, ok := n.pendingGroups.Load(userID)
	if !ok {
		return
	}
	pgl := val.(*pendingGroupList)
	pgl.mu.Lock()
	defer pgl.mu.Unlock()
	filtered := pgl.groups[:0]
	for _, g := range pgl.groups {
		if g.ChatID != chatID {
			filtered = append(filtered, g)
		}
	}
	pgl.groups = filtered
}

// DetectGroups does a one-shot scan for my_chat_member updates to find groups
// the bot has been added to. If a polling session is already active for the
// user, it returns the already-captured pending groups instead (to avoid
// conflicting getUpdates consumers).
func (n *Notifier) DetectGroups(ctx context.Context, userID string) ([]notify.PendingGroup, error) {
	// If polling is active, we can't call getUpdates — just return what we have.
	if _, ok := n.pollers.Load(userID); ok {
		return n.PendingGroups(userID), nil
	}

	botToken, _, err := n.userConfig(ctx, userID)
	if err != nil {
		return nil, err
	}

	// One-shot drain of all pending updates.
	updates, err := n.getUpdates(ctx, botToken, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("telegram: detect groups: %w", err)
	}

	now := time.Now()
	for _, u := range updates {
		if u.MyChatMember == nil {
			continue
		}
		cm := u.MyChatMember
		if cm.NewMember.Status != "member" && cm.NewMember.Status != "administrator" {
			continue
		}
		if cm.Chat.Type != "group" && cm.Chat.Type != "supergroup" {
			continue
		}
		n.AddPendingGroup(userID, notify.PendingGroup{
			ChatID:     fmt.Sprintf("%d", cm.Chat.ID),
			Title:      cm.Chat.Title,
			Type:       cm.Chat.Type,
			DetectedAt: now,
		})
	}

	return n.PendingGroups(userID), nil
}

// DecisionChannel returns a read-only channel that emits callback decisions
// from inline button taps.
func (n *Notifier) DecisionChannel() <-chan notify.CallbackDecision {
	return n.decisionCh
}

// RunCleanup periodically removes expired/used callback tokens.
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

// ── notify.Notifier implementation ───────────────────────────────────────────

func (n *Notifier) SendApprovalRequest(ctx context.Context, req notify.ApprovalRequest) (string, error) {
	botToken, chatID, err := n.userConfig(ctx, req.UserID)
	if err != nil {
		return "", err
	}

	text := formatApprovalMessage(req)

	// Generate callback tokens for inline buttons.
	approveID, denyID, tokenErr := n.cbTokens.Generate("approval", req.RequestID, req.UserID, chatID, 6*time.Minute)
	var keyboard any
	if tokenErr == nil {
		keyboard = inlineKeyboard([][]inlineButton{{
			{Text: "✅ Approve", CallbackData: "a:" + approveID},
			{Text: "❌ Deny", CallbackData: "d:" + denyID},
		}})
	} else {
		// Fallback to URL buttons if token generation fails.
		keyboard = inlineKeyboard([][]inlineButton{{
			{Text: "✅ Approve", URL: req.ApproveURL},
			{Text: "❌ Deny", URL: req.DenyURL},
		}})
	}

	msgID, err := n.sendMessage(ctx, botToken, chatID, text, keyboard)
	if err != nil {
		return "", fmt.Errorf("telegram: send approval request: %w", err)
	}

	if tokenErr == nil {
		n.ensurePolling(req.UserID, botToken, chatID)
	}

	return msgID, nil
}

func (n *Notifier) SendActivationRequest(ctx context.Context, req notify.ActivationRequest) error {
	botToken, chatID, err := n.userConfig(ctx, req.UserID)
	if err != nil {
		return err
	}

	svcName := display.ServiceName(req.Service)
	text := fmt.Sprintf(
		"🔔 <b>Clawvisor — Service Activation Required</b>\n\n"+
			"<b>Agent:</b> %s wants to use: <b>%s</b>\n"+
			"<b>Requested action:</b> %s",
		html.EscapeString(req.AgentName),
		html.EscapeString(svcName),
		html.EscapeString(svcName),
	)
	keyboard := inlineKeyboard([][]inlineButton{{
		{Text: "🔗 Activate " + svcName, URL: req.ActivateURL},
		{Text: "❌ Deny", URL: req.DenyURL},
	}})

	if _, err := n.sendMessage(ctx, botToken, chatID, text, keyboard); err != nil {
		return fmt.Errorf("telegram: send activation request: %w", err)
	}
	return nil
}

func (n *Notifier) SendTaskApprovalRequest(ctx context.Context, req notify.TaskApprovalRequest) (string, error) {
	botToken, chatID, err := n.userConfig(ctx, req.UserID)
	if err != nil {
		return "", err
	}

	text := formatTaskApprovalMessage(req)

	approveID, denyID, tokenErr := n.cbTokens.Generate("task", req.TaskID, req.UserID, chatID, 6*time.Minute)
	var keyboard any
	if tokenErr == nil {
		keyboard = inlineKeyboard([][]inlineButton{{
			{Text: "✅ Approve Task", CallbackData: "a:" + approveID},
			{Text: "❌ Deny Task", CallbackData: "d:" + denyID},
		}})
	} else {
		keyboard = inlineKeyboard([][]inlineButton{{
			{Text: "✅ Approve Task", URL: req.ApproveURL},
			{Text: "❌ Deny Task", URL: req.DenyURL},
		}})
	}

	msgID, err := n.sendMessage(ctx, botToken, chatID, text, keyboard)
	if err != nil {
		return "", fmt.Errorf("telegram: send task approval request: %w", err)
	}

	if tokenErr == nil {
		n.ensurePolling(req.UserID, botToken, chatID)
	}

	return msgID, nil
}

func (n *Notifier) SendScopeExpansionRequest(ctx context.Context, req notify.ScopeExpansionRequest) (string, error) {
	botToken, chatID, err := n.userConfig(ctx, req.UserID)
	if err != nil {
		return "", err
	}

	text := formatScopeExpansionMessage(req)

	approveID, denyID, tokenErr := n.cbTokens.Generate("scope_expansion", req.TaskID, req.UserID, chatID, 6*time.Minute)
	var keyboard any
	if tokenErr == nil {
		keyboard = inlineKeyboard([][]inlineButton{{
			{Text: "✅ Approve Expansion", CallbackData: "a:" + approveID},
			{Text: "❌ Deny Expansion", CallbackData: "d:" + denyID},
		}})
	} else {
		keyboard = inlineKeyboard([][]inlineButton{{
			{Text: "✅ Approve Expansion", URL: req.ApproveURL},
			{Text: "❌ Deny Expansion", URL: req.DenyURL},
		}})
	}

	msgID, err := n.sendMessage(ctx, botToken, chatID, text, keyboard)
	if err != nil {
		return "", fmt.Errorf("telegram: send scope expansion request: %w", err)
	}

	if tokenErr == nil {
		n.ensurePolling(req.UserID, botToken, chatID)
	}

	return msgID, nil
}

func (n *Notifier) UpdateMessage(ctx context.Context, userID, messageID, text string) error {
	botToken, chatID, err := n.userConfig(ctx, userID)
	if err != nil {
		return err
	}
	return n.editMessage(ctx, botToken, chatID, messageID, text)
}

func (n *Notifier) SendConnectionRequest(ctx context.Context, req notify.ConnectionRequest) (string, error) {
	botToken, chatID, err := n.userConfig(ctx, req.UserID)
	if err != nil {
		return "", err
	}
	text := fmt.Sprintf("🔗 <b>Agent Connection Request</b>\n\nAgent <b>%s</b> is requesting to connect from %s.",
		req.AgentName, req.IPAddress)
	msgID, err := n.sendMessage(ctx, botToken, chatID, text, nil)
	if err != nil {
		return "", fmt.Errorf("telegram: send connection request: %w", err)
	}
	return msgID, nil
}

func (n *Notifier) SendAlert(ctx context.Context, userID, text string) error {
	botToken, chatID, err := n.userConfig(ctx, userID)
	if err != nil {
		return err
	}
	if _, err := n.sendMessage(ctx, botToken, chatID, text, nil); err != nil {
		return fmt.Errorf("telegram: send alert: %w", err)
	}
	return nil
}

func (n *Notifier) SendTestMessage(ctx context.Context, userID string) error {
	botToken, chatID, err := n.userConfig(ctx, userID)
	if err != nil {
		return err
	}
	text := "✅ <b>Clawvisor — Test Message</b>\n\nYour Telegram notifications are working!"
	if _, err := n.sendMessage(ctx, botToken, chatID, text, nil); err != nil {
		return fmt.Errorf("telegram: send test message: %w", err)
	}
	return nil
}

// ── Message formatting ────────────────────────────────────────────────────────

func formatApprovalMessage(req notify.ApprovalRequest) string {
	var sb strings.Builder
	sb.WriteString("🔔 <b>Clawvisor Approval Request</b>\n\n")
	sb.WriteString(fmt.Sprintf("<b>Agent:</b> %s\n", html.EscapeString(req.AgentName)))
	sb.WriteString(fmt.Sprintf("<b>Service:</b> %s\n", html.EscapeString(display.ServiceName(req.Service))))
	sb.WriteString(fmt.Sprintf("<b>Action:</b> %s\n", html.EscapeString(display.ActionName(req.Action))))
	sb.WriteString(fmt.Sprintf("<b>Time:</b> %s\n", time.Now().UTC().Format("Mon Jan 2 2006, 3:04 PM MST")))

	if len(req.Params) > 0 {
		sb.WriteString("\n<b>Parameters:</b>\n")
		// Sort keys for deterministic output.
		keys := make([]string, 0, len(req.Params))
		for k := range req.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sb.WriteString(fmt.Sprintf("  %s: %s\n",
				html.EscapeString(k),
				html.EscapeString(paramValue(req.Params[k])),
			))
		}
	}

	if req.Reason != "" {
		sb.WriteString(fmt.Sprintf("\n<b>Agent's stated reason:</b>\n  %q\n",
			html.EscapeString(req.Reason)))
	}

	// Advisory verification warning
	if hasVerificationWarning(req) {
		sb.WriteString("\n🔍 <b>Verification Warning</b>\n")
		if req.VerifyParamScope == "violation" {
			sb.WriteString("  ❌ <b>param_scope:</b> violation\n")
		} else if req.VerifyParamScope == "ok" {
			sb.WriteString("  ✅ param_scope: ok\n")
		}
		if req.VerifyReasonCoherence == "incoherent" {
			sb.WriteString("  ❌ <b>reason:</b> incoherent\n")
		} else if req.VerifyReasonCoherence == "insufficient" {
			sb.WriteString("  ⚠️ <b>reason:</b> insufficient\n")
		} else if req.VerifyReasonCoherence == "ok" {
			sb.WriteString("  ✅ reason: ok\n")
		}
		if req.VerifyExplanation != "" {
			sb.WriteString(fmt.Sprintf("  💬 %s\n", html.EscapeString(req.VerifyExplanation)))
		}
	}

	if req.PolicyReason != "" {
		sb.WriteString(fmt.Sprintf("\n⚠️ %s\n", html.EscapeString(req.PolicyReason)))
	} else {
		sb.WriteString("\n⚠️ No policy covers this action.\n")
	}

	sb.WriteString(fmt.Sprintf("\n<i>Expires in %s</i>", html.EscapeString(req.ExpiresIn)))
	return sb.String()
}

func formatTaskApprovalMessage(req notify.TaskApprovalRequest) string {
	var sb strings.Builder
	sb.WriteString("📋 <b>Clawvisor — Task Approval Request</b>\n\n")
	sb.WriteString(fmt.Sprintf("<b>Agent:</b> %s\n", html.EscapeString(req.AgentName)))
	sb.WriteString(fmt.Sprintf("<b>Purpose:</b> %s\n", html.EscapeString(req.Purpose)))
	if req.RiskLevel != "" && req.RiskLevel != "unknown" {
		emoji := riskEmoji(req.RiskLevel)
		sb.WriteString(fmt.Sprintf("<b>Risk:</b> %s %s\n", emoji, html.EscapeString(req.RiskLevel)))
	}
	sb.WriteString(fmt.Sprintf("<b>Time:</b> %s\n", time.Now().UTC().Format("Mon Jan 2 2006, 3:04 PM MST")))

	sb.WriteString("\n<b>Requested actions:</b>\n")
	for _, a := range req.Actions {
		mode := "auto-execute"
		if !a.AutoExecute {
			mode = "requires per-request approval"
		}
		sb.WriteString(fmt.Sprintf("  • %s (%s)\n",
			html.EscapeString(display.FormatServiceAction(a.Service, a.Action)),
			mode))
	}

	if len(req.PlannedCalls) > 0 {
		sb.WriteString("\n<b>Planned calls:</b>\n")
		for _, pc := range req.PlannedCalls {
			sb.WriteString(fmt.Sprintf("  • %s — %s\n",
				html.EscapeString(display.FormatServiceAction(pc.Service, pc.Action)),
				html.EscapeString(pc.Reason)))
		}
	}

	sb.WriteString(fmt.Sprintf("\n<i>Expires in %s</i>", html.EscapeString(req.ExpiresIn)))
	return sb.String()
}

func formatScopeExpansionMessage(req notify.ScopeExpansionRequest) string {
	var sb strings.Builder
	sb.WriteString("🔄 <b>Clawvisor — Scope Expansion Request</b>\n\n")
	sb.WriteString(fmt.Sprintf("<b>Agent:</b> %s\n", html.EscapeString(req.AgentName)))
	sb.WriteString(fmt.Sprintf("<b>Task:</b> %s\n", html.EscapeString(req.Purpose)))

	mode := "auto-execute"
	if !req.NewAction.AutoExecute {
		mode = "requires per-request approval"
	}
	sb.WriteString(fmt.Sprintf("\n<b>New action:</b> %s (%s)\n",
		html.EscapeString(display.FormatServiceAction(req.NewAction.Service, req.NewAction.Action)),
		mode))

	if req.Reason != "" {
		sb.WriteString(fmt.Sprintf("\n<b>Reason:</b> %s\n", html.EscapeString(req.Reason)))
	}

	return sb.String()
}

// hasVerificationWarning returns true if the approval request contains a non-clean verification result.
func hasVerificationWarning(req notify.ApprovalRequest) bool {
	if req.VerifyParamScope == "" && req.VerifyReasonCoherence == "" {
		return false // verification not run
	}
	return req.VerifyParamScope != "ok" || req.VerifyReasonCoherence != "ok"
}

// riskEmoji returns an emoji for the given risk level.
func riskEmoji(level string) string {
	switch level {
	case "low":
		return "🟢"
	case "medium":
		return "🟡"
	case "high":
		return "🟠"
	case "critical":
		return "🔴"
	default:
		return ""
	}
}

// paramValue converts a param value to a display string, truncated at 100 chars.
func paramValue(v any) string {
	var s string
	switch val := v.(type) {
	case string:
		s = val
	case nil:
		s = ""
	default:
		b, _ := json.Marshal(val)
		s = string(b)
	}
	if len(s) > 100 {
		return s[:97] + "..."
	}
	return s
}

// ── Telegram API calls ────────────────────────────────────────────────────────

type inlineButton struct {
	Text         string `json:"text"`
	URL          string `json:"url,omitempty"`
	CallbackData string `json:"callback_data,omitempty"`
}

func inlineKeyboard(rows [][]inlineButton) any {
	type replyMarkup struct {
		InlineKeyboard [][]inlineButton `json:"inline_keyboard"`
	}
	return replyMarkup{InlineKeyboard: rows}
}

// sendMessage calls the Telegram sendMessage API and returns the message ID as a string.
// If the API rejects the request when a reply_markup is included (e.g. inline keyboard
// buttons with localhost URLs), it retries without the markup so the notification text
// is still delivered.
func (n *Notifier) sendMessage(ctx context.Context, botToken, chatID, text string, replyMarkup any) (string, error) {
	msgID, err := n.doSendMessage(ctx, botToken, chatID, text, replyMarkup)
	if err != nil && replyMarkup != nil {
		// Retry without the inline keyboard — the markup (e.g. button URLs)
		// is the most common reason for Telegram rejecting the request.
		msgID, retryErr := n.doSendMessage(ctx, botToken, chatID, text, nil)
		if retryErr == nil {
			return msgID, nil
		}
		// Both attempts failed; return the original error.
	}
	return msgID, err
}

func (n *Notifier) doSendMessage(ctx context.Context, botToken, chatID, text string, replyMarkup any) (string, error) {
	payload := map[string]any{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "HTML",
	}
	if replyMarkup != nil {
		payload["reply_markup"] = replyMarkup
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var tResp struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(respBody, &tResp); err != nil {
		return "", fmt.Errorf("telegram API response: %w", err)
	}
	if !tResp.OK {
		return "", fmt.Errorf("telegram API error: %s", tResp.Description)
	}
	return fmt.Sprintf("%d", tResp.Result.MessageID), nil
}

// editMessage calls the Telegram editMessageText API to update a sent message
// and remove its inline keyboard.
func (n *Notifier) editMessage(ctx context.Context, botToken, chatID, messageID, text string) error {
	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
		"parse_mode": "HTML",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/editMessageText", botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var tResp struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(respBody, &tResp); err != nil {
		return fmt.Errorf("telegram editMessageText response: %w", err)
	}
	if !tResp.OK {
		return fmt.Errorf("telegram editMessageText error: %s", tResp.Description)
	}
	return nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// telegramCfg is the JSON structure stored in notification_configs for channel="telegram".
type telegramCfg struct {
	BotToken    string `json:"bot_token"`
	ChatID      string `json:"chat_id"`
	GroupChatID string `json:"group_chat_id,omitempty"`
}

// userConfig fetches the per-user bot_token and chat_id from the store.
func (n *Notifier) userConfig(ctx context.Context, userID string) (botToken, chatID string, err error) {
	nc, err := n.store.GetNotificationConfig(ctx, userID, "telegram")
	if errors.Is(err, store.ErrNotFound) {
		return "", "", fmt.Errorf("telegram: user %s has no Telegram notification configured", userID)
	}
	if err != nil {
		return "", "", fmt.Errorf("telegram: fetching config for user %s: %w", userID, err)
	}
	var cfg telegramCfg
	if err := json.Unmarshal(nc.Config, &cfg); err != nil {
		return "", "", fmt.Errorf("telegram: invalid config for user %s: %w", userID, err)
	}
	if cfg.BotToken == "" {
		return "", "", fmt.Errorf("telegram: user %s config missing bot_token", userID)
	}
	if cfg.ChatID == "" {
		return "", "", fmt.Errorf("telegram: user %s config missing chat_id", userID)
	}
	return cfg.BotToken, cfg.ChatID, nil
}
