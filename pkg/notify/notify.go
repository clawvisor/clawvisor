package notify

import (
	"context"
	"time"

	"github.com/clawvisor/clawvisor/pkg/adapters"
	"github.com/clawvisor/clawvisor/pkg/store"
)

// ExpansionTool / ExpansionEgress / ExpansionCredential are notify-local
// DTOs that mirror the runtime envelope's per-item shape without
// importing from internal/. Per AGENTS.md the pkg/ surface is the
// public/shared interface boundary, so notify cannot reach into
// internal/runtime/tasks. The handler translates from
// internal/runtime/tasks types into these at the boundary.
type ExpansionTool struct {
	ToolName string
	Why      string
	// GatewayAction reports whether tool_name parses as service:action
	// and would materialize as an AuthorizedAction on approve. When
	// true, AutoExecute reflects the effective disposition the user
	// would grant. Local-harness tools (Bash, Edit, …) set
	// GatewayAction=false and the AutoExecute value is meaningless.
	GatewayAction bool
	AutoExecute   bool
}

type ExpansionEgress struct {
	Host string
	Why  string
}

type ExpansionCredential struct {
	VaultItemID     string
	VaultItemHandle string
	Why             string
}

// ReplacedExpansionTool / Egress / Credential carry both the prior and
// new entries for a replace-by-name dedup so renderers can show a
// was/now diff (the reviewer needs to see what's changing, not just
// the new value).
type ReplacedExpansionTool struct {
	Prior ExpansionTool
	New   ExpansionTool
}

type ReplacedExpansionEgress struct {
	Prior ExpansionEgress
	New   ExpansionEgress
}

type ReplacedExpansionCredential struct {
	Prior ExpansionCredential
	New   ExpansionCredential
}

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
	PendingID string
	RequestID string
	// TaskID disambiguates sibling pending approvals that share a request_id
	// under symmetric dedup. Empty for pre-task approvals. Notifiers MUST
	// propagate this onto the CallbackDecision they emit so the resolver
	// hits the right pending row.
	TaskID       string
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
	RequestID string           `json:"request_id"`
	Status    string           `json:"status"` // "executed" | "denied" | "timeout"
	Result    *adapters.Result `json:"result,omitempty"`
	AuditID   string           `json:"audit_id"`
}

// TaskApprovalRequest carries the data needed to ask the user to approve a task scope.
type TaskApprovalRequest struct {
	TaskID       string
	UserID       string
	AgentName    string
	Purpose      string
	Actions      []store.TaskAction
	PlannedCalls []store.PlannedCall
	ScopeSummary []string
	RiskLevel    string // "low", "medium", "high", "critical"
	ApproveURL   string
	DenyURL      string
	ExpiresIn    string
}

// ScopeExpansionRequest is sent when an agent needs to expand a task's scope.
// ScopeExpansionRequest carries the data needed to render a scope-expansion
// approval prompt out-of-band (Telegram, push). The envelope is split into
// genuinely-new entries vs. entries whose `why` is being replaced so the
// user sees the full delta — see internal/runtime/tasks.EnvelopeMergeResult.
// Replaced entries carry both Prior and New so renderers can show the
// was/now diff; an "Updated" entry showing only the prior why hides the
// actual scope change.
type ScopeExpansionRequest struct {
	TaskID              string
	UserID              string
	AgentName           string
	Purpose             string
	AddedTools          []ExpansionTool
	ReplacedTools       []ReplacedExpansionTool
	AddedEgress         []ExpansionEgress
	ReplacedEgress      []ReplacedExpansionEgress
	AddedCredentials    []ExpansionCredential
	ReplacedCredentials []ReplacedExpansionCredential
	Reason              string
	// RiskLevel is the merged-envelope risk assessment as of the
	// approval prompt — fresh from the reassessment ExpandApprove runs
	// (parent risk + new scope can compose into a higher level than
	// either alone). Empty when no assessor is configured.
	RiskLevel  string
	ApproveURL string
	DenyURL    string
}

// ConnectionRequest carries the data for an agent connection request notification.
type ConnectionRequest struct {
	ConnectionID string
	UserID       string
	AgentName    string
	IPAddress    string
	ApproveURL   string
	DenyURL      string
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

// TelegramConfigStore persists and retrieves a user's Telegram bot
// configuration. Implementations are expected to encrypt the bot token at
// rest (e.g. via the credential vault) — the token never appears in any
// API response and should never be written into a plaintext database
// column.
type TelegramConfigStore interface {
	SaveTelegramConfig(ctx context.Context, userID, botToken, chatID string) error
	TelegramConfig(ctx context.Context, userID string) (botToken, chatID string, err error)
	DeleteTelegramConfig(ctx context.Context, userID string) error
}

// CallbackDecision is sent by the Telegram notifier when a user taps an
// inline Approve/Deny button. The server routes this to the appropriate handler.
type CallbackDecision struct {
	Type     string // "approval", "task", "scope_expansion", "connection"
	Action   string // "approve" or "deny"
	TargetID string
	// TaskID disambiguates a Type=="approval" decision when two pending
	// approvals share request_id under symmetric dedup. Empty for the
	// pre-task scope and for non-"approval" decision types.
	TaskID string
	UserID string
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
	StopGroupObservation(userID, groupChatID string)
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

// GroupInfo contains validated information about a Telegram group.
type GroupInfo struct {
	ChatID string `json:"chat_id"`
	Title  string `json:"title"`
	Type   string `json:"type"` // "group" or "supergroup"
}

// GroupMembershipValidator validates that the bot is a member of a Telegram group.
type GroupMembershipValidator interface {
	ValidateGroupMembership(ctx context.Context, userID, groupChatID string) (*GroupInfo, error)
}

// AgentGroupPairer manages agent-to-group-chat pairing for scoped approval checks.
type AgentGroupPairer interface {
	StartGroupPairing(ctx context.Context, userID, groupChatID, baseURL string) (string, error)
	CompleteGroupPairing(ctx context.Context, sessionID, agentID, agentUserID string) error
	AgentGroupChatID(ctx context.Context, agentID string) (string, error)
	PairedAgentIDs(ctx context.Context, groupChatID string) ([]string, error)
	UnpairAgentsForGroup(ctx context.Context, groupChatID string) error
}
