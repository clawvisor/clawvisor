package notify

import (
	"context"
	"time"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// Notifier sends approval and activation requests to the user.
type Notifier interface {
	SendApprovalRequest(ctx context.Context, req ApprovalRequest) (messageID string, err error)
	SendActivationRequest(ctx context.Context, req ActivationRequest) error
	SendTaskApprovalRequest(ctx context.Context, req TaskApprovalRequest) (messageID string, err error)
	SendScopeExpansionRequest(ctx context.Context, req ScopeExpansionRequest) (messageID string, err error)
	UpdateMessage(ctx context.Context, userID, messageID, text string) error
	SendTestMessage(ctx context.Context, userID string) error
	SendConnectionRequest(ctx context.Context, req ConnectionRequest) (messageID string, err error)
	SendAlert(ctx context.Context, userID, text string) error
}

// ApprovalRequest carries the data needed to ask the user to approve or deny a gateway request.
type ApprovalRequest struct {
	PendingID    string
	RequestID    string
	UserID       string
	AgentName    string
	Service      string
	Action       string
	Params       map[string]any
	Reason       string // agent's stated reason
	PolicyReason string // policy rule reason
	ExpiresIn    string // human-readable (e.g. "5 minutes")
	ApproveURL   string // deep-link for approve action
	DenyURL      string // deep-link for deny action (or callback data)

	// Advisory intent verification results (flat to avoid internal/intent dependency).
	VerifyParamScope      string // "ok" | "violation" | "n/a" | "" (not run)
	VerifyReasonCoherence string // "ok" | "incoherent" | "insufficient" | ""
	VerifyExplanation     string
}

// ActivationRequest is sent when a service is not yet configured.
type ActivationRequest struct {
	UserID      string
	AgentName   string
	Service     string
	ActivateURL string
	DenyURL     string
}

// CallbackPayload is posted to the agent's callback URL when a pending request resolves.
type CallbackPayload struct {
	RequestID string          `json:"request_id"`
	Status    string          `json:"status"` // "executed" | "denied" | "timeout"
	Result    *adapters.Result `json:"result,omitempty"`
	AuditID   string          `json:"audit_id"`
}

// TaskApprovalRequest carries the data needed to ask the user to approve a task scope.
type TaskApprovalRequest struct {
	TaskID       string
	UserID       string
	AgentName    string
	Purpose      string
	Actions      []store.TaskAction
	PlannedCalls []store.PlannedCall
	RiskLevel    string // "low", "medium", "high", "critical"
	ApproveURL   string
	DenyURL      string
	ExpiresIn    string
}

// ScopeExpansionRequest is sent when an agent needs to expand a task's scope.
type ScopeExpansionRequest struct {
	TaskID     string
	UserID     string
	AgentName  string
	Purpose    string
	NewAction  store.TaskAction
	Reason     string
	ApproveURL string
	DenyURL    string
}

// ConnectionRequest carries the data for an agent connection request notification.
type ConnectionRequest struct {
	ConnectionID string
	UserID       string
	AgentName    string
	IPAddress    string
}

// PairingSession represents an in-progress Telegram bot pairing.
type PairingSession struct {
	ID          string    `json:"pairing_id"`
	UserID      string    `json:"-"`
	BotUsername string    `json:"bot_username"`
	Status      string    `json:"status"` // polling | ready | confirmed | expired
	ExpiresAt   time.Time `json:"expires_at"`
}

// TelegramPairer manages the Telegram bot pairing flow.
type TelegramPairer interface {
	StartPairing(ctx context.Context, userID, botToken string) (*PairingSession, error)
	PairingStatus(pairingID string) (*PairingSession, error)
	ConfirmPairing(ctx context.Context, pairingID, code string) error
	CancelPairing(pairingID string)
}

// CallbackDecision is sent by the Telegram notifier when a user taps an
// inline Approve/Deny button. The server routes this to the appropriate handler.
type CallbackDecision struct {
	Type     string // "approval", "task", "scope_expansion"
	Action   string // "approve" or "deny"
	TargetID string
	UserID   string
}

// PollingDecrementer is implemented by notifiers that run callback polling
// goroutines (e.g. Telegram). Call DecrementPolling when a pending approval
// or task is resolved outside the inline button flow (deny via web UI, expiry).
type PollingDecrementer interface {
	DecrementPolling(userID string)
}

// GroupObserver is implemented by notifiers that support observing messages
// in a Telegram group chat for pre-approval signals.
type GroupObserver interface {
	EnsureGroupObservation(userID, botToken, chatID, groupChatID string)
	StopGroupObservation(userID string)
}

// PendingGroup represents a Telegram group that the bot has been added to
// but group observation has not yet been enabled for.
type PendingGroup struct {
	ChatID     string    `json:"chat_id"`
	Title      string    `json:"title"`
	Type       string    `json:"type"` // "group" or "supergroup"
	DetectedAt time.Time `json:"detected_at"`
}

// GroupDetector is implemented by notifiers that can detect when the bot
// has been added to Telegram groups, for the group observation setup flow.
type GroupDetector interface {
	DetectGroups(ctx context.Context, userID string) ([]PendingGroup, error)
	PendingGroups(userID string) []PendingGroup
	RemovePendingGroup(userID, chatID string)
}

// AgentGroupPairer manages agent-to-group-chat pairing for scoped approval checks.
type AgentGroupPairer interface {
	StartGroupPairing(ctx context.Context, userID, groupChatID, baseURL string) (string, error)
	CompleteGroupPairing(ctx context.Context, sessionID, agentID, agentUserID string) error
	AgentGroupChatID(ctx context.Context, agentID string) (string, error)
	PairedAgentIDs(ctx context.Context, groupChatID string) ([]string, error)
	UnpairAgentsForGroup(ctx context.Context, groupChatID string) error
}
