package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("store: record not found")

// ErrConflict is returned when a uniqueness constraint is violated.
var ErrConflict = errors.New("store: record already exists")

// Store is the primary data access interface. All database operations go
// through this interface; no direct queries are made outside the store package.
type Store interface {
	// Users
	CreateUser(ctx context.Context, email, passwordHash string) (*User, error)
	GetUserByEmail(ctx context.Context, email string) (*User, error)
	GetUserByID(ctx context.Context, id string) (*User, error)
	UpdateUserPassword(ctx context.Context, userID, newPasswordHash string) error
	DeleteUser(ctx context.Context, userID string) error

	// Restrictions
	CreateRestriction(ctx context.Context, r *Restriction) (*Restriction, error)
	DeleteRestriction(ctx context.Context, id, userID string) error
	ListRestrictions(ctx context.Context, userID string) ([]*Restriction, error)
	MatchRestriction(ctx context.Context, userID, service, action string) (*Restriction, error)

	// Agents
	CreateAgent(ctx context.Context, userID, name, tokenHash string) (*Agent, error)
	GetAgentByToken(ctx context.Context, tokenHash string) (*Agent, error)
	ListAgents(ctx context.Context, userID string) ([]*Agent, error)
	DeleteAgent(ctx context.Context, id, userID string) error
	SetAgentCallbackSecret(ctx context.Context, agentID, secret string) error
	GetAgentCallbackSecret(ctx context.Context, agentID string) (string, error)

	// Sessions (refresh tokens)
	CreateSession(ctx context.Context, userID, tokenHash string, expiresAt time.Time) (*Session, error)
	GetSession(ctx context.Context, tokenHash string) (*Session, error)
	DeleteSession(ctx context.Context, tokenHash string) error
	DeleteUserSessions(ctx context.Context, userID string) error

	// Service credentials metadata (vault stores the actual bytes)
	UpsertServiceMeta(ctx context.Context, userID, serviceID, alias string, activatedAt time.Time) error
	GetServiceMeta(ctx context.Context, userID, serviceID, alias string) (*ServiceMeta, error)
	ListServiceMetas(ctx context.Context, userID string) ([]*ServiceMeta, error)
	DeleteServiceMeta(ctx context.Context, userID, serviceID, alias string) error
	CountServiceMetasByType(ctx context.Context, userID, serviceID string) (int, error)

	// Notification configs
	UpsertNotificationConfig(ctx context.Context, userID, channel string, config json.RawMessage) error
	GetNotificationConfig(ctx context.Context, userID, channel string) (*NotificationConfig, error)
	DeleteNotificationConfig(ctx context.Context, userID, channel string) error

	// Audit log
	LogAudit(ctx context.Context, entry *AuditEntry) error
	UpdateAuditOutcome(ctx context.Context, id, outcome, errMsg string, durationMS int) error
	GetAuditEntry(ctx context.Context, id, userID string) (*AuditEntry, error)
	GetAuditEntryByRequestID(ctx context.Context, requestID, userID string) (*AuditEntry, error)
	ListAuditEntries(ctx context.Context, userID string, filter AuditFilter) ([]*AuditEntry, int, error)
	AuditActivityBuckets(ctx context.Context, userID string, since time.Time, bucketMinutes int) ([]ActivityBucket, error)

	// Tasks
	CreateTask(ctx context.Context, task *Task) error
	GetTask(ctx context.Context, id string) (*Task, error)
	ListTasks(ctx context.Context, userID string, filter TaskFilter) ([]*Task, int, error)
	UpdateTaskStatus(ctx context.Context, id, status string) error
	UpdateTaskApproved(ctx context.Context, id string, expiresAt time.Time) error
	UpdateTaskActions(ctx context.Context, id string, actions []TaskAction, expiresAt time.Time) error
	IncrementTaskRequestCount(ctx context.Context, id string) error
	SetTaskPendingExpansion(ctx context.Context, id string, action *TaskAction, reason string) error
	ListExpiredTasks(ctx context.Context) ([]*Task, error)
	RevokeTask(ctx context.Context, id, userID string) error

	// Pending approvals
	SavePendingApproval(ctx context.Context, pa *PendingApproval) error
	GetPendingApproval(ctx context.Context, requestID string) (*PendingApproval, error)
	ListPendingApprovals(ctx context.Context, userID string) ([]*PendingApproval, error)
	DeletePendingApproval(ctx context.Context, requestID string) error
	ListExpiredPendingApprovals(ctx context.Context) ([]*PendingApproval, error)
	UpdatePendingApprovalStatus(ctx context.Context, requestID, status string) error
	// ClaimPendingApprovalForExecution atomically transitions a pending approval
	// from "approved" to "executing". Returns true if the caller won the claim,
	// false if another caller already claimed it (or the row is not "approved").
	ClaimPendingApprovalForExecution(ctx context.Context, requestID string) (bool, error)
	ListPendingApprovalsByBatch(ctx context.Context, userID, batchID string) ([]*PendingApproval, error)
	UpdatePendingApprovalBlob(ctx context.Context, requestID string, blob json.RawMessage) error

	// Notification messages (cross-channel message tracking)
	SaveNotificationMessage(ctx context.Context, targetType, targetID, channel, messageID string) error
	GetNotificationMessage(ctx context.Context, targetType, targetID, channel string) (string, error)

	// Chain facts (intent verification context chaining)
	SaveChainFacts(ctx context.Context, facts []*ChainFact) error
	ListChainFacts(ctx context.Context, taskID, sessionID string, limit int) ([]*ChainFact, error)
	DeleteChainFactsByTask(ctx context.Context, taskID string) error

	// Paired devices (mobile push notifications)
	CreatePairedDevice(ctx context.Context, d *PairedDevice) error
	GetPairedDevice(ctx context.Context, id string) (*PairedDevice, error)
	ListPairedDevices(ctx context.Context, userID string) ([]*PairedDevice, error)
	DeletePairedDevice(ctx context.Context, id string) error
	ListPairedDevicesByDeviceToken(ctx context.Context, deviceToken string) ([]*PairedDevice, error)
	UpdatePairedDeviceLastSeen(ctx context.Context, id string) error
	UpdatePairedDevicePushToStartToken(ctx context.Context, id, token string) error

	// Connection requests (daemon agent onboarding)
	CreateConnectionRequest(ctx context.Context, req *ConnectionRequest) error
	GetConnectionRequest(ctx context.Context, id string) (*ConnectionRequest, error)
	ListPendingConnectionRequests(ctx context.Context, userID string) ([]*ConnectionRequest, error)
	UpdateConnectionRequestStatus(ctx context.Context, id, status, agentID string) error
	DeleteExpiredConnectionRequests(ctx context.Context) error
	CountPendingConnectionRequests(ctx context.Context) (int, error)

	// MCP sessions (persist across restarts)
	CreateMCPSession(ctx context.Context, id string, expiresAt time.Time) error
	MCPSessionValid(ctx context.Context, id string) (bool, error)
	CleanupMCPSessions(ctx context.Context) error

	// OAuth (MCP client registration + authorization codes)
	CreateOAuthClient(ctx context.Context, client *OAuthClient) error
	GetOAuthClient(ctx context.Context, clientID string) (*OAuthClient, error)
	SaveAuthorizationCode(ctx context.Context, code *OAuthAuthorizationCode) error
	// ConsumeAuthorizationCode atomically retrieves and deletes an authorization
	// code. Returns ErrNotFound if the code does not exist (or was already consumed).
	ConsumeAuthorizationCode(ctx context.Context, codeHash string) (*OAuthAuthorizationCode, error)

	// Aggregate counts (telemetry)
	TelemetryCounts(ctx context.Context) (*TelemetryCounts, error)

	// Health
	Ping(ctx context.Context) error
	Close() error
}

// TelemetryCounts holds aggregate, anonymous usage data for telemetry.
type TelemetryCounts struct {
	Agents          int            // total registered agents
	RequestsByService map[string]int // gateway requests per service (e.g. "gmail": 120)
}

// User represents a registered Clawvisor account.
type User struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Session holds a hashed refresh token.
type Session struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	TokenHash string    `json:"-"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

// Agent is an AI agent that authenticates via a long-lived bearer token.
type Agent struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Name      string    `json:"name"`
	TokenHash string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

// ServiceMeta records that a user has activated a given service.
// The actual credential bytes live in the vault.
type ServiceMeta struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	ServiceID   string    `json:"service_id"`
	Alias       string    `json:"alias"`
	ActivatedAt time.Time `json:"activated_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Restriction is a hard block on a service/action that no task can override.
type Restriction struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Service   string    `json:"service"`
	Action    string    `json:"action"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

// NotificationConfig stores per-user, per-channel notification settings.
type NotificationConfig struct {
	ID        string          `json:"id"`
	UserID    string          `json:"user_id"`
	Channel   string          `json:"channel"`
	Config    json.RawMessage `json:"config"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// AuditEntry is one row in the audit_log table.
type AuditEntry struct {
	ID             string          `json:"id"`
	UserID         string          `json:"user_id"`
	AgentID        *string         `json:"agent_id,omitempty"`
	RequestID      string          `json:"request_id"`
	TaskID         *string         `json:"task_id,omitempty"`
	Timestamp      time.Time       `json:"timestamp"`
	Service        string          `json:"service"`
	Action         string          `json:"action"`
	ParamsSafe     json.RawMessage `json:"params_safe"`
	Decision       string          `json:"decision"`
	Outcome        string          `json:"outcome"`
	PolicyID       *string         `json:"policy_id,omitempty"`
	RuleID         *string         `json:"rule_id,omitempty"`
	SafetyFlagged  bool            `json:"safety_flagged"`
	SafetyReason   *string         `json:"safety_reason,omitempty"`
	Reason         *string         `json:"reason,omitempty"`
	DataOrigin     *string         `json:"data_origin,omitempty"`
	ContextSrc     *string         `json:"context_src,omitempty"`
	DurationMS     int             `json:"duration_ms"`
	FiltersApplied json.RawMessage `json:"filters_applied,omitempty"`
	Verification   json.RawMessage `json:"verification,omitempty"`
	ErrorMsg       *string         `json:"error_msg,omitempty"`
}

// TaskAction represents a single authorized action within a task scope.
type TaskAction struct {
	Service            string          `json:"service"`
	Action             string          `json:"action"`          // specific action or "*"
	AutoExecute        bool            `json:"auto_execute"`
	ResponseFilters    json.RawMessage `json:"response_filters,omitempty"`
	ExpectedUse        string          `json:"expected_use,omitempty"`
	ExpansionRationale string          `json:"expansion_rationale,omitempty"` // set from PendingReason when scope expansion is approved
}

// Task represents a task-scoped authorization.
type Task struct {
	ID                string       `json:"id"`
	UserID            string       `json:"user_id"`
	AgentID           string       `json:"agent_id"`
	Purpose           string       `json:"purpose"`
	Status            string       `json:"status"` // pending_approval | active | completed | expired | denied | cancelled | pending_scope_expansion | revoked
	Lifetime          string       `json:"lifetime"` // session | standing
	AuthorizedActions []TaskAction `json:"authorized_actions"`
	CallbackURL       *string      `json:"callback_url,omitempty"`
	CreatedAt         time.Time    `json:"created_at"`
	ApprovedAt        *time.Time   `json:"approved_at,omitempty"`
	ExpiresAt         *time.Time   `json:"expires_at,omitempty"`
	ExpiresInSeconds  int          `json:"expires_in_seconds,omitempty"`
	RequestCount      int          `json:"request_count"`
	// PendingAction holds the action awaiting scope expansion approval.
	PendingAction *TaskAction `json:"pending_action,omitempty"`
	PendingReason string      `json:"pending_reason,omitempty"`
	// RiskLevel is the LLM-assessed risk level ("low", "medium", "high", "critical", "unknown", or "").
	RiskLevel   string          `json:"risk_level,omitempty"`
	RiskDetails json.RawMessage `json:"risk_details,omitempty"`
}

// PendingApproval is a gateway request awaiting human approval.
type PendingApproval struct {
	ID             string          `json:"id"`
	UserID         string          `json:"user_id"`
	RequestID      string          `json:"request_id"`
	AuditID        string          `json:"audit_id"`
	RequestBlob    json.RawMessage `json:"request_blob"`
	CallbackURL    *string         `json:"callback_url,omitempty"`
	BatchID        *string         `json:"batch_id,omitempty"`
	RenderedFields json.RawMessage `json:"rendered_fields,omitempty"`
	Status         string          `json:"status"` // "pending" or "approved"
	ExpiresAt      time.Time       `json:"expires_at"`
	CreatedAt      time.Time       `json:"created_at"`
}

// TaskFilter controls which tasks are returned by ListTasks.
// Zero values mean "no filter" (backwards compatible).
type TaskFilter struct {
	ActiveOnly bool   // status IN ('active','pending_approval','pending_scope_expansion')
	Status     string // exact status match (e.g. "active", "pending_approval", "denied"); empty = no filter
	Limit      int    // 0 -> no limit
	Offset     int
}

// AuditFilter controls which entries are returned by ListAuditEntries.
// Zero values mean "no filter" for that field.
type AuditFilter struct {
	Service    string // filter by service
	Outcome    string // filter by outcome
	DataOrigin string // filter by data_origin
	TaskID     string // filter by task_id
	Limit      int    // 0 -> default (50)
	Offset     int
}

// ActivityBucket is one row of the aggregated audit activity histogram.
type ActivityBucket struct {
	Bucket  time.Time `json:"bucket"`
	Outcome string    `json:"outcome"`
	Count   int       `json:"count"`
}

// ChainFact is a structural reference extracted from an adapter result for
// chain context verification in multi-step tasks.
type ChainFact struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"task_id"`
	SessionID string    `json:"session_id"`
	AuditID   string    `json:"audit_id"`
	Service   string    `json:"service"`
	Action    string    `json:"action"`
	FactType  string    `json:"fact_type"`
	FactValue string    `json:"fact_value"`
	CreatedAt time.Time `json:"created_at"`
}

// PairedDevice represents a mobile device paired for push notifications.
type PairedDevice struct {
	ID               string    `json:"id"`
	UserID           string    `json:"user_id"`
	DeviceName       string    `json:"device_name"`
	DeviceToken      string    `json:"-"`
	DeviceHMACKey    string    `json:"-"`
	PushToStartToken string    `json:"-"` // APNs push-to-start token for Live Activities
	PairedAt         time.Time `json:"paired_at"`
	LastSeenAt       time.Time `json:"last_seen_at"`
}

// ConnectionRequest represents an agent's request to connect to this daemon.
type ConnectionRequest struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CallbackURL string    `json:"callback_url,omitempty"`
	Status      string    `json:"status"`              // pending | approved | denied | expired
	AgentID     string    `json:"agent_id,omitempty"`   // set on approval
	Token       string    `json:"token,omitempty"`      // raw token, set on approval (never persisted)
	IPAddress   string    `json:"ip_address"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// OAuthClient is a dynamically registered OAuth 2.1 client (RFC 7591).
type OAuthClient struct {
	ID           string    `json:"client_id"`
	ClientName   string    `json:"client_name"`
	RedirectURIs []string  `json:"redirect_uris"`
	CreatedAt    time.Time `json:"created_at"`
}

// OAuthAuthorizationCode is a one-time-use authorization code for the OAuth 2.1 flow.
type OAuthAuthorizationCode struct {
	CodeHash      string    `json:"-"`
	ClientID      string    `json:"client_id"`
	UserID        string    `json:"user_id"`
	DaemonID      string    `json:"daemon_id,omitempty"` // set when authorized via relay
	RedirectURI   string    `json:"redirect_uri"`
	CodeChallenge string    `json:"code_challenge"`
	Scope         string    `json:"scope"`
	ExpiresAt     time.Time `json:"expires_at"`
	CreatedAt     time.Time `json:"created_at"`
}
