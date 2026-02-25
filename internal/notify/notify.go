package notify

import (
	"context"
	"time"

	"github.com/ericlevine/clawvisor/internal/adapters"
	"github.com/ericlevine/clawvisor/internal/gateway"
	"github.com/ericlevine/clawvisor/internal/store"
)

// Notifier sends approval and activation requests to the user.
type Notifier interface {
	SendApprovalRequest(ctx context.Context, req ApprovalRequest) (messageID string, err error)
	SendActivationRequest(ctx context.Context, req ActivationRequest) error
	SendTaskApprovalRequest(ctx context.Context, req TaskApprovalRequest) (messageID string, err error)
	SendScopeExpansionRequest(ctx context.Context, req ScopeExpansionRequest) (messageID string, err error)
	UpdateMessage(ctx context.Context, userID, messageID, text string) error
	SendTestMessage(ctx context.Context, userID string) error
}

// ApprovalRequest carries the data needed to ask the user to approve or deny a gateway request.
type ApprovalRequest struct {
	PendingID   string
	RequestID   string
	UserID      string
	AgentName   string
	Service     string
	Action      string
	Params      map[string]any
	Reason      string          // agent's stated reason
	PolicyReason string         // policy rule reason
	ExpiresIn   string          // human-readable (e.g. "5 minutes")
	ApproveURL  string          // deep-link for approve action
	DenyURL     string          // deep-link for deny action (or callback data)
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
	TaskID     string
	UserID     string
	AgentName  string
	Purpose    string
	Actions    []store.TaskAction
	ApproveURL string
	DenyURL    string
	ExpiresIn  string
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

// Ensure we import gateway without unused import errors in callers.
var _ = gateway.Request{}
