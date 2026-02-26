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

	// Sessions (refresh tokens)
	CreateSession(ctx context.Context, userID, tokenHash string, expiresAt time.Time) (*Session, error)
	GetSession(ctx context.Context, tokenHash string) (*Session, error)
	DeleteSession(ctx context.Context, tokenHash string) error

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
	ListTasks(ctx context.Context, userID string) ([]*Task, error)
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

	// Notification messages (cross-channel message tracking)
	SaveNotificationMessage(ctx context.Context, targetType, targetID, channel, messageID string) error
	GetNotificationMessage(ctx context.Context, targetType, targetID, channel string) (string, error)

	// Health
	Ping(ctx context.Context) error
	Close() error
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
	Service         string          `json:"service"`
	Action          string          `json:"action"`          // specific action or "*"
	AutoExecute     bool            `json:"auto_execute"`
	ResponseFilters json.RawMessage `json:"response_filters,omitempty"`
	ExpectedUse     string          `json:"expected_use,omitempty"`
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
}

// PendingApproval is a gateway request awaiting human approval.
type PendingApproval struct {
	ID            string          `json:"id"`
	UserID        string          `json:"user_id"`
	RequestID     string          `json:"request_id"`
	AuditID       string          `json:"audit_id"`
	RequestBlob   json.RawMessage `json:"request_blob"`
	CallbackURL   *string         `json:"callback_url,omitempty"`
	ExpiresAt     time.Time       `json:"expires_at"`
	CreatedAt     time.Time       `json:"created_at"`
}

// AuditFilter controls which entries are returned by ListAuditEntries.
// Zero values mean "no filter" for that field.
type AuditFilter struct {
	Service    string // filter by service
	Outcome    string // filter by outcome
	DataOrigin string // filter by data_origin
	TaskID     string // filter by task_id
	Limit      int    // 0 → default (50)
	Offset     int
}

// ActivityBucket is one row of the aggregated audit activity histogram.
type ActivityBucket struct {
	Bucket  time.Time `json:"bucket"`
	Outcome string    `json:"outcome"`
	Count   int       `json:"count"`
}
